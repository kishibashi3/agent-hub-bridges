"""Bridge-codex worker main loop (resident-process, session-persistent).

client_codex worker と同じ骨格。セッション永続化 (issue #79) により
codex が会話状態をネイティブに管理するため、get_user_history への依存を廃止。

設計: docs/design-bridge-codex.md §8
Issue: #77, #79
"""

from __future__ import annotations

import logging
import sys

import anyio
from agent_hub_sdk import AgentHub, HubSession, IncomingMessage

from agent_hub_bridges._common.reconnect import run_with_reconnect
from agent_hub_bridges.client_codex.engine import CodexCLIEngine
from agent_hub_bridges.codex.config import Config

logger = logging.getLogger(__name__)


def _format_prompt(self_handle: str, msg: IncomingMessage) -> str:
    """受信 message を bridge-codex の prompt に整形.

    issue #79: セッション永続化により codex が会話状態をネイティブに保持するため、
    get_user_history への依存を廃止。send_message で直接返信する。
    """
    reply_to = msg.sender
    return (
        f"あなたは agent-hub の peer worker `{self_handle}` として動いています。\n"
        f"agent-hub 経由で {msg.sender} から以下の message が届きました。\n"
        f"宛先: {msg.to}\n"
        f"本文:\n{msg.body}\n\n"
        f"`mcp__agent-hub__send_message` で {reply_to} へ DM を送信してください。\n"
        f"(caused_by='{msg.id}' を設定すること — 因果チェーン追跡 issue #162)\n"
        f"team 宛 broadcast は避け、送信者個人へ DM で返すこと。"
    )


async def run_worker(config: Config) -> None:
    """Bridge worker メインループ.

    `CodexCLIEngine` は client_codex から再利用。Config の sandbox_mode /
    approval_bypass が bridge-codex 専用デフォルト (danger-full-access / True) に
    なっているため、engine は変更なしで動作する。
    """
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    logger.info(
        "Starting agent-hub-bridge-codex as @%s (workdir=%s, tenant=%s, sandbox=%s)",
        config.user,
        config.workdir,
        config.tenant or "default",
        config.sandbox_mode,
    )

    engine = CodexCLIEngine.create(config)  # type: ignore[arg-type]  # codex.Config is structurally compatible with client_codex.Config
    try:

        async def _one_session() -> None:
            await _run_hub_session(config, engine)

        await run_with_reconnect(_one_session, name="hub session (bridge-codex)")
    finally:
        engine.close()


async def _run_hub_session(config: Config, engine: CodexCLIEngine) -> None:
    """1 回分の hub session を最後まで走らせる."""
    async with AgentHub.connect(
        participant=config.user,
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
    自分自身宛の echo は skip (無限ループ防止)。
    """
    self_handle = f"@{config.user}"

    # issue #51: workdir が存在しない場合は early return (crash-ack ループ防止)
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
                    caused_by=msg.id,
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
                    caused_by=msg.id,
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
