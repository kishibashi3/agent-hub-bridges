"""Bridge worker main loop (gemini, SDK-migrated).

実装段階 (= 旧 repo `agent-hub-bridge-gemini` の milestone を引き継ぐ):
- M0: agent-hub MCP に attach、 register tool で @<user> を登録 (疎通) ✅
- M1: inbox subscribe → message を Gemini SDK に流す → 応答を hub へ
  send_message ✅
- M2: SDK 直叩きを `gemini` CLI 呼出に置換、 tool 使用は CLI 側に委譲 ✅
  M2 stretch: rate limit (429) 検出時に engine 内で retry/backoff ✅
- M3 (chat session 永続化、 旧 repo の M3): 未着手
- **M_monorepo (= 本 file)**: monorepo へ移植 + SDK 移行 (= 旧 `hub.py` /
  自前 `HubClient` を削除 → `agent_hub_sdk.AgentHub` + `hub.inbox()` に
  切替)。 push + poll + heartbeat の 手書き 2-task ループを 1 つの
  `async for msg in messages:` に集約。 outer reconnect は
  `_common.reconnect.run_with_reconnect` で 共通化。
  bridge-claude と同じ構造 (= claude/gemini 双方 LLM engine 系 single
  task)。

`gemini` 自身が `mcp__agent-hub__send_message` tool を呼んで返信する path
は そのまま (= worker は subprocess を spawn するだけ、 返信 text を
Python 側で組み立てない)。
"""

from __future__ import annotations

import logging
import sys

import anyio
from agent_hub_sdk import AgentHub, HubSession, IncomingMessage

from agent_hub_bridges._common.prompt import format_peer_message_prompt
from agent_hub_bridges._common.reconnect import run_with_reconnect
from agent_hub_bridges.gemini.config import Config
from agent_hub_bridges.gemini.engine import GeminiCLIEngine

logger = logging.getLogger(__name__)


def _format_prompt(self_handle: str, msg: IncomingMessage) -> str:
    """受信 message を gemini CLI への user prompt に整形.

    `format_peer_message_prompt` (`_common/prompt.py`) で 共通骨格を作り、
    gemini 固有の補足 (= team broadcast 避けて DM で返せ、 自己 echo は
    無視) を 追記する。
    """
    base = format_peer_message_prompt(msg, self_handle=self_handle)
    return (
        f"{base}\n"
        f"宛先 (to) は必ず `{msg.sender}` を指定。 team 宛 broadcast は避け、"
        f"送信者個人へ DM で返すこと。"
    )


async def run_worker(config: Config) -> None:
    """Bridge worker メインループ.

    `_common.reconnect.run_with_reconnect` で outer reconnect loop を回す
    (= claude と同 pattern)。 `GeminiCLIEngine` (= isolated HOME 所有) は
    hub 再接続に 巻き込まず 外側で 1 度だけ立ち上げ、 再接続時も同じ
    インスタンスを使い回す。 engine.close() は finally で 1 回だけ。
    """
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    logger.info(
        "Starting agent-hub-bridge-gemini as @%s (workdir=%s, tenant=%s, model=%s)",
        config.user,
        config.workdir,
        config.tenant or "default",
        config.gemini_model,
    )

    engine = GeminiCLIEngine.create(config)
    try:

        async def _one_session() -> None:
            await _run_hub_session(config, engine)

        await run_with_reconnect(_one_session, name="hub session (gemini)")
    finally:
        engine.close()


async def _run_hub_session(config: Config, engine: GeminiCLIEngine) -> None:
    """1 回分の hub session を最後まで走らせる.

    `AgentHub.connect` → `hub.register` → `hub.inbox()` の async iterator
    を `async for` で 回すだけ。 push / poll / heartbeat は 全部 SDK 側。
    session が死ぬと iterator 内部 task が例外を上げ、 `hub.inbox()` の
    `async with` 出口で transport が tear down し、 本関数 から例外が
    伝播して 上位 `run_with_reconnect` の retry に乗る。
    """
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
                await _handle_one(hub, engine, msg, config)
                await hub.ack(msg.id)


async def _handle_one(
    hub: HubSession, engine: GeminiCLIEngine, msg: IncomingMessage, config: Config
) -> None:
    """message 1 件を gemini CLI に渡し、 subprocess の完了で 1 ターン終了とする.

    bridge-claude と違い、 応答 text を worker 側で hub に送り返したりは
    しない。 `gemini` 自身が isolated settings.json 経由で agent-hub MCP
    に接続し、 `mcp__agent-hub__send_message` tool で sender へ返信する。

    自分自身宛の echo (= team broadcast の自己反射、 agent-hub の実装次第)
    は 処理対象から外す: peer = `@<self>` は 無限ループの種なので skip。
    """
    self_handle = f"@{config.user}"
    if msg.sender == self_handle:
        logger.info("Skipping self-sent message %s (avoid loop)", msg.id)
        return

    logger.info("← message %s from %s: %s", msg.id, msg.sender, msg.body[:120])

    prompt = _format_prompt(self_handle, msg)
    try:
        result = await engine.run(peer=msg.sender, prompt=prompt)
    except Exception as exc:
        logger.exception("gemini CLI error for message %s: %s", msg.id, exc)
        # gemini が起動すらできなかった場合に限り、 worker から最小通知を送る。
        # tool 実行途中で gemini がコケた場合は gemini 側で reply 済みかも
        # 知れないため、 二重送信は避けたい。 返信先 (sender) のみへの
        # fallback。
        with anyio.move_on_after(10):
            try:
                await hub.send(
                    to=msg.sender,
                    message=(
                        f"(自動応答) gemini CLI engine でエラー: "
                        f"{type(exc).__name__}: {exc}"
                    ),
                )
            except Exception:
                logger.exception("fallback send_message also failed")
        return

    # stdout / stderr は engine 側で既に INFO ログに残してあるので、
    # ここでは 1 行サマリを残すだけ。
    #
    # emoji は exit code で分岐: `✓` = 成功 (returncode == 0)、 `✗` =
    # 失敗 (= gemini CLI が non-zero で 終了。 reply 自体は CLI が tool
    # 経由で 送っているかもしれないが、 ログ目視で 異常を見落とさないため)。
    #
    # issue #25: rate-limit retry を経由した場合 (attempts >= 2) に
    # " RETRIED" marker を末尾に付ける。これにより `grep RETRIED` で
    # retry が発生した message だけを抽出できる。
    status_emoji = "✓" if result.returncode == 0 else "✗"
    retry_marker = " RETRIED" if result.attempts >= 2 else ""
    logger.info(
        "%s processed %s from %s (exit=%d, %.1fs, attempts=%d%s)",
        status_emoji,
        msg.id,
        msg.sender,
        result.returncode,
        result.duration_s,
        result.attempts,
        retry_marker,
    )
