"""Bridge worker main loop.

実装段階:
- M0: Slack Bolt を Socket Mode で起動するだけ (agent-hub 未接続)。
- M1: agent-hub MCP に attach、register、Slack → hub の relay。
- M2: anyio TaskGroup で 2-loop 並行構造へ。hub → Slack の relay loop 追加。
- M3: ThreadContext を 2 loop で共有し、thread 内 follow-up と元 thread reply
      復帰を有効化。
- M4: 3 つ目の task で 周期的 re-subscribe を回す (= MCP 切断の workaround,
      bridge-claude#2 と同じ問題への対処)。
- M5 (= agent-hub-sdk 移行): 旧 `hub.py` (= bridge-slack の HubClient) を削除し
  agent-hub-sdk の `AgentHub.connect` / `HubSession` に切り替え。method 名は
  SDK の規約に従い `register` / `send_with_retry` / `ack` 等にリネーム。
  振る舞いは 1:1 同等。
"""

from __future__ import annotations

import logging
import sys

import anyio
from agent_hub_sdk import AgentHub, HubSession
from slack_bolt.adapter.socket_mode.async_handler import AsyncSocketModeHandler

from agent_hub_bridges.slack.config import Config
from agent_hub_bridges.slack.routing import ThreadContext
from agent_hub_bridges.slack.slack_handler import build_slack_app, run_hub_to_slack_loop

logger = logging.getLogger(__name__)

# M4: re-subscribe を回す間隔 (秒)。デフォルトは 10 分。長すぎると 切断検知が
# 遅れ、短すぎると agent-hub に 不要な負荷をかける。10 分は DESIGN.md §5.3 と
# bridge-claude#2 issue の議論から拝借。env で上書き可能にしてもよいが、まずは
# 固定値で M4 範囲を絞る。
_RESUBSCRIBE_INTERVAL_S = 600.0


async def _resubscribe_once(hub: HubSession) -> None:
    """inbox を 1 回 re-subscribe する (例外は log のみで握り潰す).

    `_periodic_resubscribe` から周期実行される。subscribe は idempotent な操作
    である前提 (= 既に subscribe 済でも害は無い)。失敗は log のみで loop は
    継続する (= DESIGN.md §5.3「silent fail しない、ただし loop は止めない」)。

    test 用にループ無しで切り出してある (= unit test 可能)。
    """
    try:
        await hub.subscribe_inbox()
        logger.debug("periodic resubscribe ok")
    except Exception:
        logger.exception(
            "periodic resubscribe failed (継続、次の interval で再試行)"
        )


async def _periodic_resubscribe(
    hub: HubSession,
    *,
    interval_s: float = _RESUBSCRIBE_INTERVAL_S,
    sleep_fn=None,
) -> None:
    """周期的に `_resubscribe_once` を呼ぶ無限 loop.

    Args:
        interval_s: 1 回 sleep する秒数。
        sleep_fn: test 用 injection。None なら `anyio.sleep`。

    Note:
        loop 内で例外を catch せずに上に伝播すると TaskGroup が他 task を
        cancel するので、ここでは `_resubscribe_once` 側で握り潰している。
    """
    sleep = sleep_fn if sleep_fn is not None else anyio.sleep
    logger.info("periodic resubscribe loop 開始 (interval=%.1fs)", interval_s)
    while True:
        await sleep(interval_s)
        await _resubscribe_once(hub)


async def _run_slack_socket_mode(handler: AsyncSocketModeHandler) -> None:
    """Slack Socket Mode handler を 走らせるだけの薄い wrapper.

    `start_async()` は WebSocket 接続を張って message を待ち続ける。
    切断時は slack-bolt が内部で自動 reconnect する。TaskGroup が cancel した
    時点で raise される CancelledError は anyio が呼出元 (TaskGroup) まで
    伝播してくれるので、ここでは catch しない。
    """
    logger.info("slack→hub: Socket Mode 接続を開始")
    await handler.start_async()


async def run_worker(config: Config) -> None:
    """Bridge worker メインループ (M4: 3 task 並行 + 共有 ThreadContext).

    anyio TaskGroup で 3 task を並行に走らせる:
      - **Slack → agent-hub** の relay loop: slack-bolt の Socket Mode handler
        が `app_mention` / `message` を受けて `slack_handler` 経由で
        `hub.send_with_retry` を呼ぶ (M4)。`@bot` mention で thread が
        bind され、以降の thread reply は同じ peer に流れる (M3)。
      - **agent-hub → Slack** の relay loop: `hub.inbox_pushes()` を購読し、
        push 受信ごとに `get_unread` → `chat_postMessage` (peer の最新 thread
        が bind 済ならそこへ reply、未 bind なら `SLACK_DEFAULT_CHANNEL`、
        rate limit は Retry-After 尊重で inline 1 回 retry, M4) → `mark_as_read`。
      - **周期 re-subscribe**: 10 分ごとに `subscribe_inbox` を 再発行 (M4)。
        MCP の subscribe が長時間で 効果切れになる workaround
        (bridge-claude#2 と同じ問題)。

    `ThreadContext` は 2 loop で共有する: Slack 側で bind され、hub 側で読まれる。
    同一 process / event loop の中なので lock は不要。

    どれか一つが例外を投げたら TaskGroup が他を cancel して全体を終了する。
    再接続レベルの一時障害は各 loop 側で握り潰す責任を持つ。

    (Python 3.10 を最小ターゲットにしたいので asyncio.TaskGroup ではなく
     anyio.create_task_group を使う。hub.py で既に anyio に依存している。)
    """
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    logger.info(
        "Starting agent-hub-bridge-slack as @%s (tenant=%s, agent_hub=%s, default_channel=%s)",
        config.user,
        config.tenant or "default",
        config.agent_hub_url,
        config.slack_default_channel or "(未設定: thread 未 bind の peer はスキップされます)",
    )

    # M3: 2 loop 間で共有する thread map (in-memory、再起動で消える)
    thread_ctx = ThreadContext()

    async with AgentHub.connect(
        user=config.user,
        mode="stateful",
        tenant=config.tenant,
        display_name=config.display_name,
        url=config.agent_hub_url,
        pat=config.github_pat,
        client_type="agent-hub-bridge/slack",  # issue #280: mode auto-detection
    ) as hub:
        # SDK M5 (agent-hub-sdk#27, merge fc4a4cd, included in v0.6.0)
        # auto-registers as part of ``AgentHub.connect``. The explicit
        # ``await hub.register()`` that used to live here is now a harmless
        # duplicate (= server-side ``register`` is idempotent), so we drop
        # it. Catches up legacy `agent-hub-bridge-slack#10` (= `fcd8025`).
        await hub.subscribe_inbox()

        app = build_slack_app(config, hub, thread_ctx)
        handler = AsyncSocketModeHandler(app, config.slack_app_token)

        async with anyio.create_task_group() as tg:
            tg.start_soon(_run_slack_socket_mode, handler)
            tg.start_soon(run_hub_to_slack_loop, app, hub, config, thread_ctx)
            tg.start_soon(_periodic_resubscribe, hub)
