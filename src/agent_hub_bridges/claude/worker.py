"""Bridge worker main loop (claude).

実装段階 (= 旧 repo `agent-hub-bridge-claude` の milestone を引き継ぐ):
- M0: agent-hub MCP に attach、 register tool で @<user> を登録 (疎通) ✅
- M1: inbox subscribe → message を Claude に流す → 応答が send_message で hub へ ✅
- M2: PreToolUse hook で permission propagation (将来)
- M3: CLAUDE.md / settings 注入の正式対応、 session_id resume (将来)
- M_sdk: 旧 ``HubClient`` (= 同梱 ``hub.py``) を ``agent-hub-sdk`` に
  置換。 push + poll + heartbeat の 手書き 3-task ループを
  ``async with hub.inbox() as messages: async for msg in messages: …`` に集約。 ✅
- **M_monorepo (= 本 file)**: `agent-hub-bridges` monorepo に移植 + outer
  reconnect / `_summarize_exc` / `_format_prompt` を `_common/` に
  委譲。 挙動は 1:1 同等 (= 旧 repo PR #M_sdk 完了時の状態)。

reconnect は SDK 内部ではなく caller (= 本 file の `run_with_reconnect`) で
担当する。 SDK の M2 PR #11 で deferred、 SDK 側 reconnect は別 milestone。
"""

from __future__ import annotations

import contextlib
import json
import logging
import os
import sys
import tempfile
from collections.abc import Iterator
from pathlib import Path

from agent_hub_sdk import AgentHub, HubSession, IncomingMessage
from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    ResultMessage,
    SystemMessage,
    TextBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

from agent_hub_bridges._common.prompt import format_peer_message_prompt
from agent_hub_bridges._common.reconnect import run_with_reconnect
from agent_hub_bridges.claude.config import Config

logger = logging.getLogger(__name__)


@contextlib.contextmanager
def _mcp_config_file(config: Config) -> Iterator[Path]:
    """agent-hub の MCP config を一時 file に書き出す (PAT を ps に出さないため).

    本 helper は Claude Agent SDK 側 が ``mcp__agent-hub__*`` tools を呼ぶ
    ために必要 (= bridge 自身の inbox subscribe 用 session とは別接続)。
    Claude 側 が agent-hub を呼ぶ path は file-based config なので本関数は
    引き続き必要 (= claude bridge 専用、 `_common` に抽出しない)。
    """
    headers: dict[str, str] = {
        "Authorization": f"Bearer {config.github_pat}",
        "X-User-Id": config.user,
    }
    if config.tenant:
        headers["X-Tenant-Id"] = config.tenant

    payload = {
        "mcpServers": {
            "agent-hub": {
                "type": "http",
                "url": config.agent_hub_url,
                "headers": headers,
            },
        },
    }

    fd, path_str = tempfile.mkstemp(prefix="agent-hub-bridge-claude-", suffix=".json")
    path = Path(path_str)
    try:
        os.chmod(path, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(payload, f)
        yield path
    finally:
        with contextlib.suppress(FileNotFoundError):
            path.unlink()


def _build_options(config: Config, mcp_config_path: Path) -> ClaudeAgentOptions:
    """Claude SDK の options を組み立てる.

    bridge は「入力経路を agent-hub に差し替えただけの Claude Code」を目指す。
    振る舞いは workdir の CLAUDE.md / project .claude/settings に従う。
    """
    return ClaudeAgentOptions(
        # str (file path) として渡し、 CLI 引数経由の PAT 露出を回避
        mcp_servers=str(mcp_config_path),
        cwd=str(config.workdir),
        # 確認 UI は出さない (CLI なので元々出ないが明示)。 M2 で hook 経由の
        # propagation に置き換える。
        permission_mode="bypassPermissions",
        # user-level の plugin marketplace は読まない (agent-hub-plugin の
        # auto-engage を防ぐ)。 workdir の CLAUDE.md / .claude/settings は読む。
        setting_sources=["project", "local"],
    )


def _format_message(msg: object) -> str:
    """SDK message を 1 行に整形してログ出力用."""
    if isinstance(msg, AssistantMessage):
        parts = []
        for block in msg.content:
            if isinstance(block, TextBlock):
                parts.append(f"[text] {block.text}")
            elif isinstance(block, ToolUseBlock):
                parts.append(f"[tool_use] {block.name}({block.input})")
            else:
                parts.append(f"[{type(block).__name__}]")
        return "ASSISTANT: " + " | ".join(parts)
    if isinstance(msg, UserMessage):
        parts = []
        for block in msg.content if isinstance(msg.content, list) else [msg.content]:
            if isinstance(block, ToolResultBlock):
                parts.append(f"[tool_result] {str(block.content)[:200]}")
            elif isinstance(block, str):
                parts.append(f"[text] {block}")
            else:
                parts.append(f"[{type(block).__name__}]")
        return "USER: " + " | ".join(parts)
    if isinstance(msg, SystemMessage):
        return f"SYSTEM: {msg.subtype}"
    if isinstance(msg, ResultMessage):
        return (
            f"RESULT: turns={msg.num_turns}, "
            f"cost=${msg.total_cost_usd or 0:.4f}, "
            f"duration={msg.duration_ms}ms"
        )
    return f"{type(msg).__name__}: {msg!r}"


async def run_worker(config: Config) -> None:
    """Bridge worker メインループ.

    `_common.reconnect.run_with_reconnect` で outer reconnect loop を回す。
    `ClaudeSDKClient` (= peer ごとの会話履歴を持つ) は hub 再接続に
    巻き込まず 外側で 1 度だけ立ち上げ、 再接続時も同じインスタンスを使い回す。
    """
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    logger.info(
        "Starting agent-hub-bridge-claude as @%s (workdir=%s, tenant=%s)",
        config.user,
        config.workdir,
        config.tenant or "default",
    )

    with _mcp_config_file(config) as mcp_config_path:
        options = _build_options(config, mcp_config_path)

        async with ClaudeSDKClient(options=options) as claude:
            await claude.connect()
            logger.info("Claude session started, awaiting hub session...")

            async def _one_session() -> None:
                await _run_hub_session(config, claude)

            await run_with_reconnect(_one_session, name="hub session (claude)")


async def _run_hub_session(config: Config, claude: ClaudeSDKClient) -> None:
    """1 回分の hub session を最後まで走らせる.

    `AgentHub.connect` → `hub.inbox()` の async iterator を `async for` で
    回すだけ。 push / poll / heartbeat / `/ping` intercept は 全部 SDK 側。
    session が死ぬと iterator 内部 task が例外を上げ、 `hub.inbox()` の
    `async with` 出口で transport が tear down し、 本関数 から例外が伝播
    して 上位 `run_with_reconnect` の retry に乗る。

    SDK M5 (agent-hub-sdk#27, merge ``fc4a4cd``) auto-registers as part
    of ``AgentHub.connect``. The explicit ``registered = await
    hub.register()`` that used to live here is now a harmless duplicate
    (= server-side ``register`` is idempotent), so we drop it. The log
    message previously printed the server's registration confirmation
    text (e.g. ``registered: @claude-bridge``); now that the return
    value is gone, we log the user handle from the already-resolved
    ``config.user`` — same operator-facing signal that the bridge is up.
    """
    async with AgentHub.connect(
        user=config.user,
        mode="stateful",
        tenant=config.tenant,
        display_name=config.display_name,
        url=config.agent_hub_url,
        pat=config.github_pat,
    ) as hub:
        logger.info(
            "Hub session ready (@%s), listening on inbox...",
            config.user,
        )

        async with hub.inbox() as messages:
            async for msg in messages:
                await _handle_one(hub, claude, msg, config)
                await hub.ack(msg.id)


async def _handle_one(
    hub: HubSession, claude: ClaudeSDKClient, msg: IncomingMessage, config: Config
) -> None:
    """message 1 件を Claude に流して応答を待つ.

    `claude.query` の `session_id` を sender にすることで、 peer ごとに
    会話 context が 分離される (= M3 の stateful 化の基礎)。

    NOTE: `hub.ack(msg.id)` は呼出元 (= `_run_hub_session` の `async for`
    body) で 1 行下に書く (= caller が ack)。
    """
    del hub, config  # unused in M1 path; reserved for future hooks
    logger.info("← message %s from %s: %s", msg.id, msg.sender, msg.body[:120])

    prompt = format_peer_message_prompt(msg)
    await claude.query(prompt, session_id=msg.sender)

    async for sdk_msg in claude.receive_response():
        formatted = _format_message(sdk_msg)
        logger.info(formatted)
        if isinstance(sdk_msg, ResultMessage):
            break
