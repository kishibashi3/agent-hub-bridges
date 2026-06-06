"""Bridge worker main loop (bridge-tmux).

claude_p bridge との差分:
  - `ClaudePCLIEngine` (1 メッセージ = 1 subprocess) の代わりに
    `SessionManager` (tmux session を keep-alive) を使う
  - `--print` (headless) ではなく interactive tmux セッションを使うため
    Claude Code subscription 課金のまま動作する (6/15 対応 issue #110)
  - セッションが idle なら on-demand spawn、N 分無通信で kill (wake-on-message)

フロー:
  1. AgentHub.inbox() で DM を受信
  2. SessionManager.handle(prompt) で Tier2 (tmux) にメッセージを注入
  3. claude が MCP send_message を呼んで返信 → bridge は wait_for_idle で待機
  4. 完了後 hub.ack(msg.id)

設計: docs/design-bridge-tmux.md
Issue: #110
"""

from __future__ import annotations

import asyncio
import logging
import signal
import sys

import anyio
from agent_hub_sdk import AgentHub, HubSession, IncomingMessage

from agent_hub_bridges._common.prompt import format_peer_message_prompt
from agent_hub_bridges._common.reconnect import run_with_reconnect
from agent_hub_bridges.tmux.config import Config
from agent_hub_bridges.tmux.session import SessionManager

logger = logging.getLogger(__name__)


def _format_prompt(self_handle: str, msg: IncomingMessage) -> str:
    """受信 message を claude インタラクティブセッションへの prompt に整形する.

    claude_p bridge と同一形式: claude が MCP tool (send_message) 経由で
    送信者個人へ DM で返信することを指示する。
    """
    base = format_peer_message_prompt(msg, self_handle=self_handle)
    return (
        f"{base}\n"
        f"宛先 (to) は必ず `{msg.sender}` を指定。team 宛 broadcast は避け、"
        f"送信者個人へ DM で返すこと。"
    )


async def run_worker(config: Config) -> None:
    """Bridge worker メインループ.

    SessionManager は hub 再接続に巻き込まず外側で 1 度だけ生成し、
    再接続時も同じインスタンスを使い回す。shutdown() は finally で 1 回のみ。

    SIGTERM グレースフルシャットダウン: claude_p bridge と同パターン。
    """
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    logger.info(
        "Starting agent-hub-bridge-tmux as @%s (workdir=%s, tenant=%s, idle=%.0fs)",
        config.user,
        config.workdir,
        config.tenant or "default",
        config.idle_timeout_s,
    )

    # SIGTERM → task.cancel() (claude_p 同パターン, issue #58)
    loop = asyncio.get_running_loop()
    main_task = asyncio.current_task()

    def _on_sigterm() -> None:
        logger.info("SIGTERM received — initiating graceful shutdown")
        if main_task is not None:
            main_task.cancel()

    loop.add_signal_handler(signal.SIGTERM, _on_sigterm)

    manager = SessionManager.create(config)
    try:

        async def _one_session() -> None:
            await _run_hub_session(config, manager)

        await run_with_reconnect(_one_session, name="hub session (bridge-tmux)")
    finally:
        await manager.shutdown()
        try:
            loop.remove_signal_handler(signal.SIGTERM)
        except Exception as exc:
            logger.debug("remove_signal_handler(SIGTERM) failed: %s", exc)


async def _run_hub_session(config: Config, manager: SessionManager) -> None:
    """1 回分の hub session を最後まで走らせる."""
    async with AgentHub.connect(
        user=config.user,
        mode="stateful",
        tenant=config.tenant,
        display_name=config.display_name,
        url=config.agent_hub_url,
        pat=config.github_pat,
    ) as hub:
        registered = await hub.register()
        logger.info(
            "Hub session ready (%s), listening on inbox...",
            registered.splitlines()[0] if registered else "(no body)",
        )

        async with hub.inbox() as messages:
            async for msg in messages:
                await _handle_one(hub, manager, msg, config)
                await hub.ack(msg.id)


async def _handle_one(
    hub: HubSession,
    manager: SessionManager,
    msg: IncomingMessage,
    config: Config,
) -> None:
    """message 1 件を tmux セッションに渡す.

    issue #51 パターン: workdir が存在しない場合は early return (ack を保証)。
    自分自身宛の echo は skip (無限ループ防止)。
    """
    self_handle = f"@{config.user}"

    # workdir が存在しない場合は early return
    if not config.workdir.is_dir():
        logger.error(
            "workdir does not exist: %s — skipping %s to prevent crash-ack loop",
            config.workdir, msg.id,
        )
        with anyio.move_on_after(10):
            try:
                await hub.send(
                    to=msg.sender,
                    message=f"(auto) bridge workdir does not exist: {config.workdir}",
                    caused_by=msg.id,
                )
            except Exception:
                logger.exception("workdir-missing fallback DM to %s failed", msg.sender)
        return

    # 自己ループ防止
    if msg.sender == self_handle:
        logger.info("Skipping self-sent message %s (avoid loop)", msg.id)
        return

    logger.info("<- message %s from %s: %s", msg.id, msg.sender, msg.body[:120])

    prompt = _format_prompt(self_handle, msg)
    try:
        await manager.handle(prompt)
    except TimeoutError as exc:
        logger.error("Timeout processing message %s: %s", msg.id, exc)
        with anyio.move_on_after(10):
            try:
                await hub.send(
                    to=msg.sender,
                    message=(
                        f"(auto) bridge-tmux: response timeout — "
                        f"{type(exc).__name__}: {exc}"
                    ),
                    caused_by=msg.id,
                )
            except Exception:
                logger.exception("fallback send_message also failed")
        return
    except Exception as exc:
        logger.exception("Error processing message %s: %s", msg.id, exc)
        with anyio.move_on_after(10):
            try:
                await hub.send(
                    to=msg.sender,
                    message=(
                        f"(auto) bridge-tmux error: "
                        f"{type(exc).__name__}: {exc}"
                    ),
                    caused_by=msg.id,
                )
            except Exception:
                logger.exception("fallback send_message also failed")
        return

    logger.info("✓ processed %s from %s", msg.id, msg.sender)
