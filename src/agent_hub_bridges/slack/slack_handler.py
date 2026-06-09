"""Slack 側 event handler.

実装段階:
- M0: bot mention で "hi" を返すだけの hello-world。
- M1: mention 構文 (`@agent-hub <peer> <body>`) を parse → agent-hub へ relay
- M2: agent-hub inbox push → Slack default channel に post
- M3: thread context map (routing.ThreadContext) — thread 内 follow-up を 同じ
      peer に維持しつつ、hub→Slack の reply を元 thread に戻す
- M4: 異常ケースを Slack thread に可視化 (ParticipantNotFoundError / HubTransientError
      → 人間が読める warning、Slack rate limit → Retry-After 尊重で 1 回 retry)
      ← 現状
"""

from __future__ import annotations

import logging
import re
from typing import TYPE_CHECKING

import anyio
from agent_hub_sdk import HubTransientError, PeerNotFoundError
from slack_bolt.async_app import AsyncApp

from agent_hub_bridges.slack.routing import (
    ThreadContext,
    format_hub_to_slack_text,
    format_participants_listing,
    format_relay_body,
    parse_bot_command,
    parse_mention,
    parse_slack_retry_after,
)

if TYPE_CHECKING:
    from agent_hub_sdk import HubSession, IncomingMessage

    from agent_hub_bridges.slack.config import Config

logger = logging.getLogger(__name__)

# Slack の mention は `<@U01234567>` 形式。bot mention 部分の除去用 regex。
_MENTION_RE = re.compile(r"<@[A-Z0-9]+>")

# 簡易な display name キャッシュ (M1 の最小実装、再起動で消える)。
_USER_NAME_CACHE: dict[str, str] = {}


def _strip_bot_mention(text: str) -> str:
    """先頭の bot mention `<@U...>` を取り除いて trim する.

    M0 から残してるユーティリティ。M1 では使わないが、debug log / 互換性のため温存。
    """
    return _MENTION_RE.sub("", text or "", count=1).strip()


def format_send_failure_message(peer: str, error: BaseException) -> str:
    """`send_message` 失敗時に Slack thread に出す text を組み立てる (M4).

    DESIGN.md M4 acceptance「異常ケースが Slack 内で観測可能」の主要 path。
    例外の型ごとに 人間が次のアクションを取れる文面に差別化する:

      - `ParticipantNotFoundError` → participant の handle 違い / オフライン を示唆。
      - `HubTransientError` → 一時的、後ほど再試行を促す。
      - その他 → generic な warning (= 想定外、debug 用に str(e) を そのまま付ける)。

    呼出元 (`handle_app_mention` / `_relay_thread_follow_up`) が `say(...)` に
    渡す前提の pure 関数として 切り出してある (= unit test 可能)。
    """
    if isinstance(error, PeerNotFoundError):
        peer_name = peer.lstrip("@")
        return (
            f":bust_in_silhouette: `{peer}` は agent-hub に居ません "
            f"(未登録、または オフライン)。handle を確認するか、相手側 worker の "
            f"起動状態を確認してください。"
            f"\n_参考: `get_participants` で 参加者一覧を確認できます。_"
            f"\n_(検出: {peer_name})_"
        )
    if isinstance(error, HubTransientError):
        return (
            f":hourglass: agent-hub が 一時的に応答していません ({error})。"
            f"少し時間を置いて 再度 `@bot {peer.lstrip('@')} ...` を試してください。"
        )
    return f":warning: `{peer}` への relay に失敗しました: `{error}`"


async def _resolve_user_display(client, user_id: str) -> str:
    """Slack user id から display name を解決する.

    `users:read` scope が必要。失敗時は user_id を fallback で返す。
    in-process キャッシュあり (= 同じ user の連投で API call を抑制)。
    """
    if user_id in _USER_NAME_CACHE:
        return _USER_NAME_CACHE[user_id]

    try:
        resp = await client.users_info(user=user_id)
        profile = (resp.get("user") or {}).get("profile") or {}
        # display_name は空のことがあるので real_name にも fallback
        name = (
            profile.get("display_name")
            or profile.get("real_name")
            or resp.get("user", {}).get("name")
            or user_id
        )
    except Exception as e:
        logger.warning("users_info failed for %s: %s; using raw id as fallback", user_id, e)
        name = user_id

    _USER_NAME_CACHE[user_id] = name
    return name


def build_slack_app(
    config: Config,
    hub: HubSession,
    thread_ctx: ThreadContext | None = None,
) -> AsyncApp:
    """Slack Bolt の AsyncApp を作って handler を登録して返す.

    現行 (M3) の振る舞い:

      - **`app_mention`** — bot が直接 mention されたとき:
          1. `<@bot> <peer> <body>` を parse
          2. agent-hub `send_message` で relay
          3. 成功時 ``thread_ctx.bind(channel, thread_ts, peer)`` で この thread
             を peer に紐付ける (= 以降の thread 内 reply を同 peer に流す前提)
          4. Slack thread に confirmation を post (silent fail しない)
          5. parse 失敗時は usage hint を thread に出す

      - **`message`** (M3 で実装) — thread 内 follow-up:
          - thread_ts があり、`thread_ctx` で peer が既に bind されている、かつ
            `@bot` mention で始まっていない (= app_mention は別ルート) ならば、
            同 peer に relay する。bot 発話 / 既に処理済 (mention 付き) は弾く。
          - 未 bind の thread / channel root の独立 message は無視 (= 過剰 relay
            しない)。

    Args:
        thread_ctx: M3 で導入された ThreadContext。`None` を渡すと thread
            follow-up は無効化される (= 後方互換、test 用にも便利)。
    """
    app = AsyncApp(token=config.slack_bot_token)

    @app.event("app_mention")
    async def handle_app_mention(event: dict, say, client, logger: logging.Logger) -> None:
        user_id = event.get("user", "?")
        channel = event.get("channel", "?")
        thread_ts = event.get("thread_ts") or event.get("ts")
        raw_text = event.get("text", "")

        logger.info(
            "app_mention received: user=%s channel=%s thread_ts=%s text=%r",
            user_id,
            channel,
            thread_ts,
            raw_text,
        )

        # issue #4: bridge-internal command (`list` / `participants`) は relay 前に判定
        # する。予約語に match したら agent-hub に relay せず、bridge が直接応答する。
        bot_command = parse_bot_command(raw_text)
        if bot_command == "list_participants":
            await _handle_list_participants(
                hub=hub,
                say=say,
                thread_ts=thread_ts,
                logger=logger,
            )
            return

        parsed = parse_mention(raw_text)
        if parsed is None:
            await say(
                text=(
                    "使い方: `@agent-hub <peer> <body>`\n"
                    "例: `@agent-hub gemma この PR の差分を英訳して`\n"
                    "参加者一覧: `@agent-hub list` (or `participants`)"
                ),
                thread_ts=thread_ts,
            )
            return

        user_display = await _resolve_user_display(client, user_id)
        relay_body = format_relay_body(
            channel_id=channel,
            user_display=user_display,
            body=parsed.body,
        )

        try:
            await hub.send_with_retry(to=parsed.peer, message=relay_body)
        except Exception as e:
            # ParticipantNotFoundError / HubTransientError / その他想定外を 1 本で受け、
            # format_send_failure_message が isinstance で分岐する責務分担。
            logger.warning("send_message to %s failed: %s", parsed.peer, e)
            await say(
                text=format_send_failure_message(parsed.peer, e),
                thread_ts=thread_ts,
            )
            return

        # M3: この thread を peer に紐付ける。以降の thread 内 reply が
        # `handle_message_events` 経由で同 peer に転送される。
        if thread_ctx is not None and channel and thread_ts:
            thread_ctx.bind(channel=channel, thread_ts=thread_ts, peer=parsed.peer)

        await say(
            text=(
                f":outbox_tray: {parsed.peer} に relay しました。"
                f"応答が届いたら ここに post します。"
            ),
            thread_ts=thread_ts,
        )

    @app.event("message")
    async def handle_message_events(
        event: dict, say, client, logger: logging.Logger
    ) -> None:
        """M3: thread 内 follow-up を 同 peer に relay する.

        Slack-bolt の event handler は test しづらいので、判定 + relay 本体は
        `_relay_thread_follow_up` に切り出して unit test 可能にしている。
        """
        await _relay_thread_follow_up(
            event=event,
            hub=hub,
            thread_ctx=thread_ctx,
            slack_client=client,
            say=say,
        )

    return app


async def _handle_list_participants(
    *,
    hub: HubSession,
    say,
    thread_ts: str | None,
    logger: logging.Logger,
) -> None:
    """`@bot list` / `@bot participants` への応答 (issue #4).

    `hub.get_participants()` で agent-hub の register 済 person を取得し、
    `format_participants_listing` で整形して Slack thread に post する。

    エラー方針 (M4 と一貫):
      - `HubTransientError` (5xx / network) は thread に warning を出して終了。
        `send_message_with_retry` のような自動 retry は入れない (= ユーザ起動の
        UI 操作なので、もう一度 `list` を打てば良い。ぼーっと待たせない)。
      - その他 例外も generic warning を thread に post して silent fail を防ぐ
        (DESIGN.md §5.3 方針)。

    Args:
        thread_ts: post 先 thread. `None` なら channel root に post される
            (= `say` の default 動作)。
    """
    try:
        participants = await hub.get_participants()
    except HubTransientError as e:
        logger.warning("get_participants transient: %s", e)
        await say(
            text=(
                f":hourglass: agent-hub が一時的に応答していません ({e})。"
                "少し時間を置いて再度 `@bot list` を試してください。"
            ),
            thread_ts=thread_ts,
        )
        return
    except Exception as e:
        logger.exception("get_participants failed: %s", e)
        await say(
            text=f":warning: 参加者一覧の取得に失敗しました: `{e}`",
            thread_ts=thread_ts,
        )
        return

    text = format_participants_listing(participants)
    logger.info("list_participants: %d 件 → Slack thread post", len(participants))
    await say(text=text, thread_ts=thread_ts)


async def _relay_thread_follow_up(
    *,
    event: dict,
    hub: HubSession,
    thread_ctx: ThreadContext | None,
    slack_client,
    say,
) -> bool:
    """thread 内の bot 宛 follow-up reply を agent-hub に relay する.

    `build_slack_app` 内の `handle_message_events` から呼ばれる。pure な
    async 関数として 切り出してあるので、Bolt context 抜きで unit test できる。

    ガード (= ここで弾いた event は no-op で return):
      - `thread_ctx` 未設定 (= M3 機能 OFF) は何もしない
      - bot 発話 / system message (`bot_id`, `subtype`) は無視
      - `user` 不明 (= bot 経由の自動 post 等) は無視
      - thread reply でない (`thread_ts` なし) は無視
      - 先頭が bot mention (`<@U...>` で parse_mention 成功) → `app_mention`
        ハンドラで既に処理されてるので二重 relay 防止のため skip
      - bind 済みでない thread → 過剰 relay しない (= M3 範囲外)

    relay 失敗時は thread に warning を post する (silent fail しない、
    `app_mention` と同方針)。

    Returns:
        True なら relay を試みた (成功 / 失敗を問わず)。test 用の判定値。
    """
    if thread_ctx is None:
        return False
    if event.get("bot_id"):
        return False
    if event.get("subtype"):
        # message_changed / message_deleted / channel_join 等は無視
        return False
    if not event.get("user"):
        return False

    thread_ts = event.get("thread_ts")
    if not thread_ts:
        return False  # thread reply のみが対象

    channel = event.get("channel")
    if not channel:
        return False

    raw_text = event.get("text", "") or ""
    if parse_mention(raw_text) is not None:
        # `@bot ...` 先頭は app_mention で既に処理される
        return False

    peer = thread_ctx.peer_for_thread(channel=channel, thread_ts=thread_ts)
    if peer is None:
        return False  # 未 bind の thread は 過剰 relay しない

    user_id = event["user"]
    user_display = await _resolve_user_display(slack_client, user_id)
    relay_body = format_relay_body(
        channel_id=channel,
        user_display=user_display,
        body=raw_text,
    )

    logger.info(
        "thread follow-up: user=%s channel=%s thread_ts=%s → %s",
        user_id,
        channel,
        thread_ts,
        peer,
    )

    try:
        await hub.send_with_retry(to=peer, message=relay_body)
    except Exception as e:
        logger.warning("thread follow-up relay to %s failed: %s", peer, e)
        await say(
            text=format_send_failure_message(peer, e),
            thread_ts=thread_ts,
        )
        return True

    # peer の「最新 thread」を refresh (= 直近の会話相手をこの thread に)。
    # 既に bind 済みの thread なので新規 binding ではなく touch() を使う
    # (= 意図を明示する API。bind() でも結果は同じだが副作用が暗黙になる)。
    thread_ctx.touch(channel=channel, thread_ts=thread_ts, peer=peer)
    return True


# ============================================================================
# M2 + M3: agent-hub → Slack の relay
# ============================================================================
#
# 設計メモ:
#   - inbox subscribe は worker.py 側で行う (HubClient.subscribe_inbox)。
#     ここでは push を受けたあと get_unread → Slack post → mark_as_read を回す。
#   - M3 で `ThreadContext` を導入: peer の最新 thread が分かれば そこに reply、
#     未 bind なら `SLACK_DEFAULT_CHANNEL` に fallback。
#   - 「mark_as_read するか否か」の方針:
#       * Slack に post 成功 → mark_as_read
#       * 自己 echo (sender == @<self>) → 既読化して drain (loop 防止)
#       * 配信先 channel 解決不可 (SLACK_DEFAULT_CHANNEL 未設定 + thread 未 bind)
#           → 既読化して drain (永久 retry を避ける、起動時に warn 出す)
#       * Slack API エラー (rate limit / network / channel_not_found 等)
#           → 既読化せず、次の push で再試行
#     1 件 post が失敗しても loop は止めない (silent fail はしない、必ず log)。


def _resolve_target(
    config: Config,
    msg: IncomingMessage,
    thread_ctx: ThreadContext | None = None,
) -> tuple[str | None, str | None]:
    """post する (channel, thread_ts) を決める.

    優先順位 (admin@2026-05-14 第二次要望: sticky 化):
      1. `thread_ctx` に active な (channel, thread_ts) がある (= 過去に一度でも
         `@bot` mention された) → **どの peer からの返信もそこに sticky に集める**
         (= 別 peer / 未 bind の peer も含め 全部 直近 active thread に reply)。
         以前は peer 別に thread を引いていたが、別 agent からの返信が別の場所に
         散らばる問題があったため、active thread への sticky routing に変更
         (admin@2026-05-14: 「一度 bind された channel/thread に sticky に。
         active_channel に bind されていれば、どの peer からの返信もそこに集める」)。
      2. それ以外は `SLACK_DEFAULT_CHANNEL` に root post (= 一度も bind されて
         いない cold-start 状態)
      3. どれも無ければ `(None, None)` (= 配信先不明、caller が drain する)

    注意: peer 別 thread の lookup (`thread_for_peer`) は Slack→hub の方向
    (= follow-up reply で peer を引く) でのみ意味を持ち、hub→Slack の post 先
    決定では使わない。peer 別 binding 情報自体は ThreadContext 内に残るので、
    Slack 上で 別 thread の `@bot` mention に対する follow-up は引き続き機能
    する (= 古い thread での会話継続は壊れない)。

    Returns:
        (channel, thread_ts)。channel が None なら post 不可。
    """
    if thread_ctx is not None and thread_ctx.active_channel is not None:
        # 一度でも bind されていれば、全 peer から来た message を active thread に
        # sticky に集める。active_thread_ts が None になることは現状ない (bind/touch
        # で必ず pair で更新される) が、defensive に None なら root post に倒す。
        return thread_ctx.active_channel, thread_ctx.active_thread_ts
    return config.slack_default_channel, None


async def _post_one(
    web_client: object,
    config: Config,
    msg: IncomingMessage,
    thread_ctx: ThreadContext | None = None,
    *,
    sleep_fn=None,
) -> bool:
    """1 件の hub 受信 message を Slack に投げる. mark_as_read してよければ True.

    `thread_ctx` を渡すと、sender の最新 thread に reply する (= M3)。`None`
    のままなら default channel に root post する (= M2 互換動作、test や
    degraded mode 用)。

    M4 で Slack rate limit 対応を追加: `chat_postMessage` が 429 (ratelimited)
    を返したら、`Retry-After` header の秒数だけ待って 1 回 inline retry する。
    それでも失敗するなら False を返して 次の push に retry を委ねる。

    Args:
        sleep_fn: rate-limit retry 時の sleep 関数 (test 用 injection)。
            None ならば `anyio.sleep`。

    Returns:
        True なら caller が mark_as_read してよい (= 配信成功 or drain 妥当)。
        False なら inbox に残して次の push で再試行する (= Slack 側一時障害)。
    """
    self_handle = f"@{config.user}"
    if msg.sender == self_handle:
        # 自分発の message が回って来ることは通常無いが、defensive に drain。
        logger.debug("skipping self-echo message %s", msg.id)
        return True

    channel, thread_ts = _resolve_target(config, msg, thread_ctx)
    if not channel:
        logger.warning(
            "hub→slack: 配信先 channel 不明のため message %s (from %s) を drop します。"
            "SLACK_DEFAULT_CHANNEL を env に設定する or `@bot %s ...` で thread を "
            "bind してください。",
            msg.id,
            msg.sender,
            msg.sender.lstrip("@"),
        )
        return True  # 永久 retry を避けるため drain 扱い

    text = format_hub_to_slack_text(sender=msg.sender, body=msg.body)
    sleep = sleep_fn if sleep_fn is not None else anyio.sleep

    try:
        await web_client.chat_postMessage(
            channel=channel,
            text=text,
            thread_ts=thread_ts,
        )
    except Exception as e:
        retry_after_s = parse_slack_retry_after(e)
        if retry_after_s is not None:
            # rate limit: Retry-After 秒待って 1 回だけ inline retry
            logger.warning(
                "hub→slack: Slack rate limited (Retry-After=%ds), retrying once "
                "after sleep (message %s from %s)",
                retry_after_s, msg.id, msg.sender,
            )
            await sleep(retry_after_s)
            try:
                await web_client.chat_postMessage(
                    channel=channel,
                    text=text,
                    thread_ts=thread_ts,
                )
            except Exception as e2:
                logger.exception(
                    "hub→slack: rate-limit retry も失敗 (message %s); 次の push で再試行: %s",
                    msg.id, e2,
                )
                return False
            logger.info(
                "hub→slack: rate-limit retry 成功 %s from %s → channel=%s thread=%s",
                msg.id, msg.sender, channel, thread_ts,
            )
            return True

        logger.exception(
            "hub→slack: chat_postMessage 失敗 (message %s from %s); 次の push で再試行: %s",
            msg.id,
            msg.sender,
            e,
        )
        return False

    logger.info(
        "hub→slack: posted %s from %s → channel=%s thread=%s",
        msg.id,
        msg.sender,
        channel,
        thread_ts,
    )
    return True


async def _drain_inbox_to_slack(
    hub: HubSession,
    web_client: object,
    config: Config,
    thread_ctx: ThreadContext | None = None,
) -> None:
    """未読 inbox を 1 サイクル分 Slack に流す.

    例外は全部 log に逃がして loop を止めない (DESIGN.md §5.3「silent fail しない、
    ただし loop は止めない」方針)。

    `thread_ctx` は `_post_one` にそのまま委譲される (M3、thread 復帰)。
    """
    try:
        messages = await hub.get_unread()
    except Exception:
        logger.exception("hub→slack: get_unread に失敗; 次の push で再試行")
        return

    if not messages:
        return

    logger.info("hub→slack: %d 件の未読を処理します", len(messages))
    for msg in messages:
        try:
            should_ack = await _post_one(web_client, config, msg, thread_ctx)
        except Exception:
            logger.exception("hub→slack: 想定外エラー (message %s)", msg.id)
            continue
        if should_ack:
            try:
                await hub.ack(msg.id)
            except Exception:
                logger.exception("hub→slack: mark_as_read 失敗 (%s)", msg.id)


async def run_hub_to_slack_loop(
    app: AsyncApp,
    hub: HubSession,
    config: Config,
    thread_ctx: ThreadContext | None = None,
) -> None:
    """agent-hub inbox push を受けて Slack に relay するメインループ.

    Caller (worker.py) は事前に `hub.subscribe_inbox()` を呼んでいる前提。
    本関数は:
      1. 起動直後に未読を一度 drain (subscribe より前に積まれた message 対策、
         bridge-claude/worker.py と同パターン)
      2. inbox push を受けるたびに drain を回す
    本関数が return することは通常 無く、cancel されて終わる。

    `thread_ctx` は Slack handler 側と共有された ThreadContext を期待 (M3)。
    """
    web_client = app.client

    if not config.slack_default_channel:
        logger.warning(
            "SLACK_DEFAULT_CHANNEL 未設定: hub→Slack の relay は thread bind 済 "
            "peer のみ post できます (未 bind の peer は drain されます)。env に "
            "SLACK_DEFAULT_CHANNEL=C... を設定すると 全 message が cold-start でも "
            "流れます。"
        )

    logger.info("hub→slack: inbox push subscribe loop 開始")
    # 起動時 drain
    await _drain_inbox_to_slack(hub, web_client, config, thread_ctx)
    async for _uri in hub.inbox_pushes():
        await _drain_inbox_to_slack(hub, web_client, config, thread_ctx)
