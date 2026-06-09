"""Client-codex worker main loop (stateless).

bridge-gemini と同一パターン:
  - `AgentHub.inbox()` で DM を受信
  - `CodexCLIEngine.run()` で `codex exec` を起動
  - codex が MCP tool 経由で agent-hub に返信
  - subprocess 完了後に `hub.ack(msg.id)`

差分(gemini との比較):
  - rate-limit fallback DM なし(M1、error pattern 未知のため)
  - workdir missing check あり(issue #51 実績パターン)
  - retry なし(M1)

設計: docs/design-bridge-codex.md §8
"""

from __future__ import annotations

import logging
import sys

import anyio
from agent_hub_sdk import AgentHub, HubSession, IncomingMessage

from agent_hub_bridges._common.prompt import format_peer_message_prompt
from agent_hub_bridges._common.reconnect import run_with_reconnect
from agent_hub_bridges.client_codex.config import Config
from agent_hub_bridges.client_codex.engine import CodexCLIEngine

logger = logging.getLogger(__name__)


def _format_prompt(self_handle: str, msg: IncomingMessage) -> str:
    """受信 message を codex exec への user prompt に整形."""
    base = format_peer_message_prompt(msg, self_handle=self_handle)
    return (
        f"{base}\n"
        f"宛先 (to) は必ず `{msg.sender}` を指定。team 宛 broadcast は避け、"
        f"送信者個人へ DM で返すこと。"
    )


async def run_worker(config: Config) -> None:
    """Bridge worker メインループ.

    `CodexCLIEngine` は hub 再接続に巻き込まず外側で 1 度だけ立ち上げ、
    再接続時も同じインスタンスを使い回す。engine.close() は finally で 1 回のみ。
    """
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    logger.info(
        "Starting agent-hub-client-codex as @%s (workdir=%s, tenant=%s, sandbox=%s)",
        config.user,
        config.workdir,
        config.tenant or "default",
        config.sandbox_mode,
    )

    engine = CodexCLIEngine.create(config)
    try:

        async def _one_session() -> None:
            await _run_hub_session(config, engine)

        await run_with_reconnect(_one_session, name="hub session (codex)")
    finally:
        engine.close()


async def _run_hub_session(config: Config, engine: CodexCLIEngine) -> None:
    """1 回分の hub session を最後まで走らせる."""
    async with AgentHub.connect(
        user=config.user,
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
                await _handle_one(hub, engine, msg, config)
                await hub.ack(msg.id)


async def _handle_one(
    hub: HubSession,
    engine: CodexCLIEngine,
    msg: IncomingMessage,
    config: Config,
) -> None:
    """message 1 件を codex exec に渡し、subprocess 完了で 1 ターン終了とする.

    issue #51 パターン: workdir が存在しない場合は early return で ack を保証する。
    自分自身宛の echo は skip(無限ループ防止)。
    """
    self_handle = f"@{config.user}"

    # issue #51: workdir が存在しない場合は early return(crash-ack ループ防止)。
    if not config.workdir.is_dir():
        logger.error(
            "workdir does not exist or is not a directory: %s — "
            "skipping message %s to prevent crash-ack loop (issue #51)",
            config.workdir,
            msg.id,
        )
        with anyio.move_on_after(10):
            try:
                await hub.send(
                    to=msg.sender,
                    message=(
                        f"(自動応答) bridge の workdir が存在しません: {config.workdir}"
                    ),
                )
            except Exception:
                logger.exception("workdir-missing fallback DM to %s failed", msg.sender)
        return

    if msg.sender == self_handle:
        logger.info("Skipping self-sent message %s (avoid loop)", msg.id)
        return

    logger.info("← message %s from %s: %s", msg.id, msg.sender, msg.body[:120])

    prompt = _format_prompt(self_handle, msg)
    try:
        result = await engine.run(peer=msg.sender, prompt=prompt)
    except Exception as exc:
        logger.exception("codex CLI error for message %s: %s", msg.id, exc)
        with anyio.move_on_after(10):
            try:
                await hub.send(
                    to=msg.sender,
                    message=(
                        f"(自動応答) codex CLI engine でエラー: "
                        f"{type(exc).__name__}: {exc}"
                    ),
                )
            except Exception:
                logger.exception("fallback send_message also failed")
        return

    status_emoji = "✓" if result.returncode == 0 else "✗"
    logger.info(
        "%s processed %s from %s (exit=%d, %.1fs)",
        status_emoji,
        msg.id,
        msg.sender,
        result.returncode,
        result.duration_s,
    )
