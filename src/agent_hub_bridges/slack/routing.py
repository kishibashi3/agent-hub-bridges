"""Slack message → agent-hub の routing 補助.

責務:
  1. Slack message text を `@bot <peer> <body>` 構文として parse する
  2. agent-hub に渡す body を「誰経由か」付きに整形する
  3. agent-hub から戻ってきた message を Slack 投稿用 text に整形する (M2)
  4. Slack thread ↔ agent-hub peer の対応 map を保持する (M3、`ThreadContext`)
  5. エラー分類: agent-hub error 文字列 / Slack rate limit 例外を pure な
     関数で分類する (M4、`classify_hub_error` / `parse_slack_retry_after`)
  6. bridge-internal な command (`list` / `participants`) を parse / 整形する
     (issue #4、`parse_bot_command` / `format_participants_listing`)
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Literal

if TYPE_CHECKING:
    # Participant lives in agent-hub-sdk as of the SDK migration; the bridge
    # only imports it as a type hint for the participants-listing formatter.
    from agent_hub_sdk import Participant

# Slack の bot mention 形式. `<@U01234567>` または `<@U01234567|display>`。
# 行頭に来る場合のみ「bot 宛て」として扱う。
_LEADING_MENTION_RE = re.compile(r"^<@(?P<uid>[A-Z0-9]+)(?:\|[^>]+)?>\s*")


@dataclass(frozen=True)
class ParsedMention:
    """`@bot <peer> <body>` の parse 結果.

    Attributes:
        bot_user_id: 先頭の mention で指された Slack user id (= U...)。
            app_mention event を経由するなら ほぼ常に bot 自身の id だが、
            将来 mention の verifyに使えるよう保持しておく。
        peer: 宛先 peer の handle。必ず `@` 始まりに正規化される (`gemma` → `@gemma`)。
        body: peer token を除いた残り (= 実際に転送する本文)。空文字もありうる。
    """

    bot_user_id: str
    peer: str
    body: str


def parse_mention(text: str | None) -> ParsedMention | None:
    """Slack message text から `@bot <peer> <body>` を取り出す.

    Returns:
        正しく parse できれば ParsedMention、できなければ None。
        None になるケース:
          - text が空 / None
          - 先頭が `<@U...>` 形式の mention でない
          - mention の後ろに peer token が無い
    """
    if not text:
        return None

    m = _LEADING_MENTION_RE.match(text)
    if not m:
        return None

    rest = text[m.end():].strip()
    if not rest:
        return None

    parts = rest.split(maxsplit=1)
    raw_peer = parts[0]
    body = parts[1].strip() if len(parts) > 1 else ""

    # `@gemma` でも `gemma` でも受け入れる。canonical 形は `@gemma`。
    peer_handle = raw_peer.lstrip("@").strip()
    if not peer_handle:
        return None

    return ParsedMention(
        bot_user_id=m.group("uid"),
        peer=f"@{peer_handle}",
        body=body,
    )


def format_relay_body(
    *,
    channel_id: str,
    user_display: str,
    body: str,
) -> str:
    """agent-hub に渡す本文を「誰経由か」付きに整形する.

    DESIGN.md §5.1 の format:
        (via slack <#channel> by @<slack-user-display-name>):
        <original body>

    `<#C...>` は Slack 内で render される channel link 形式。
    agent-hub 側の AI peer には不透明だが、後で人間が log を読む際に
    Slack の元 thread に飛べる利点がある。
    """
    return f"(via slack <#{channel_id}> by @{user_display}):\n{body}"


def format_hub_to_slack_text(*, sender: str, body: str) -> str:
    """agent-hub から受け取った message を Slack 投稿用 text に整形する.

    DESIGN.md §5.2 の format:
        *@<sender-handle>* via agent-hub:
        <message body>

    `*` は Slack の bold 記法。`sender` は通常 `@gemma` のように `@` 始まりだが、
    将来 agent-hub の field 名規約が変わっても落ちないよう、欠けてたら補う
    (defensive)。
    """
    handle = sender if sender.startswith("@") else f"@{sender}"
    return f"*{handle}* via agent-hub:\n{body}"


# ============================================================================
# M3: thread context map
# ============================================================================


@dataclass
class ThreadContext:
    """Slack thread ↔ agent-hub peer の対応を持つ in-memory map (DESIGN.md M3).

    用途は 2 方向:

      Slack → hub: 既に bind 済みの thread 内で ユーザが `@bot` 無しの reply を
                    した時、どの peer に流すかを `peer_for_thread()` で引く。

      hub → Slack: agent-hub から peer 経由で message が来た時、まず
                    `thread_for_peer()` で 元 thread を引き、なければ
                    `active_channel` に root post する (= 既知 channel に常に
                    届ける、bind されていない peer も drop しない)。

    ### Active channel / active thread 概念 (admin@2026-05-14 の追加要望)

    `@bot` mention (= `bind()` 呼出) は「今 bridge が会話に参加している channel
    と thread」を 1 組だけ持つ、というモデル。直近の bind 先が
    `(active_channel, active_thread_ts)` になり、hub→Slack の **全 peer から
    の返信** はその場所に sticky に流れる。

    別 channel から新しい `@bot` mention が来たら、active を切替え、
    **古い channel に紐付いていた binding は全て破棄する** (= "全体を bind し
    直す")。これによって 1 channel に 1 会話の clean な状態に戻る。

    同一 channel 内で別 thread から `@bot` mention が来たら、active_thread_ts
    だけが更新され (= 会話の主導権が新 thread に移る)、古い thread→peer の
    binding は引き続き残る (= 古い thread での Slack→hub follow-up は機能し
    続ける)。

    永続化はしない (DESIGN.md M3:「bridge 再起動で context lost、ただし 新しい
    thread からは clean に始まる」)。bridge は single-process / single-event-loop
    なので lock は不要 (= async でも 1 つの coroutine が触る間に suspend しない
    操作のみ)。

    Attributes:
        _thread_to_peer: (channel, thread_ts) → peer。bind で書く、follow-up
            で読む。同じ key への再 bind は上書き (= peer 変更を許可)。
            active_channel が切り替わると古い channel の entry は除去される。
        _peer_to_latest_thread: peer → (channel, thread_ts)。bind / touch
            ごとに「直近の thread」へ更新される。同じく active_channel 切替時に
            古い entry は除去される。`thread_for_peer()` で参照されるが、
            hub→Slack の post 先決定では `_active_thread_ts` の方が優先される
            (admin@2026-05-14: sticky 化)。
        _active_channel: 現在 bridge が貼り付いている Slack channel。`None` は
            「まだ一度も bind されていない」状態。hub→Slack の post 先 channel
            として `_resolve_target` で参照される。
        _active_thread_ts: 現在 bridge が貼り付いている Slack thread の ts。
            `None` は「未 bind」状態。`active_channel` と同時に bind / touch
            ごとに更新される。hub→Slack の post 先 thread_ts として
            `_resolve_target` で参照され、**どの peer から来た message も
            同じ thread に集める** sticky model の核 (admin@2026-05-14 要望)。
    """

    _thread_to_peer: dict[tuple[str, str], str] = field(default_factory=dict)
    _peer_to_latest_thread: dict[str, tuple[str, str]] = field(default_factory=dict)
    _active_channel: str | None = None
    _active_thread_ts: str | None = None

    @property
    def active_channel(self) -> str | None:
        """直近 bind / touch された channel. 未 bind なら None.

        hub→Slack の routing で「default channel が無くても active_channel に
        投げる」fallback として参照される。read-only (= 外部からの直接代入は
        bind/touch 経由で行う)。
        """
        return self._active_channel

    @property
    def active_thread_ts(self) -> str | None:
        """直近 bind / touch された thread_ts. 未 bind なら None.

        `active_channel` と pair で使う。hub→Slack の sticky routing で
        「どの peer からの返信も この thread に集める」ための post 先として
        参照される。read-only (= bind/touch 経由のみ更新)。
        """
        return self._active_thread_ts

    def bind(self, *, channel: str, thread_ts: str, peer: str) -> None:
        """Slack thread と peer を 新規に紐付ける. 既存 binding は上書きする.

        新規 `@bot` mention で呼び出すことを想定: thread → peer の方向に新 binding
        を入れ、同時に「peer の最新 thread」も更新する。

        ### Active channel の切替

        既存 `active_channel` と異なる channel での bind は、**古い channel の
        全 binding を破棄して 新 channel に "全体を bind し直す"** 動作になる
        (= admin@2026-05-14 要望)。これにより:

          - 旧 channel での未消化 peer→thread 情報は新会話に持ち越されない
          - 新 channel での hub→Slack post は「未 bind の peer も新 channel に
            集める」挙動になる (= 1 channel = 1 会話 model)

        同一 channel 内での bind 連打 (= thread 切替や peer 切替) は従来通り
        既存 binding を保持しつつ最新を上書き / 追加する。

        既に bind 済みの thread で 同一 peer の follow-up を受けた場合に
        「最新 thread」だけを更新したい用途には `touch()` を使うこと。`bind()` で
        同等処理はできるが、副作用が暗黙的になるので意図を明示する API を分ける。

        全 arg が空文字 / falsy なら no-op (= defensive、誤呼出で map を汚さない)。
        """
        if not channel or not thread_ts or not peer:
            return
        if self._active_channel is not None and self._active_channel != channel:
            # channel が切り替わった: "全体を bind し直す" — 旧 channel の binding を破棄
            self._thread_to_peer.clear()
            self._peer_to_latest_thread.clear()
        self._active_channel = channel
        self._active_thread_ts = thread_ts
        key = (channel, thread_ts)
        self._thread_to_peer[key] = peer
        self._peer_to_latest_thread[peer] = key

    def touch(self, *, channel: str, thread_ts: str, peer: str) -> None:
        """既 bind の thread を peer の「最新 thread」として持ち上げる.

        `bind()` との違い:
          - `bind()` は thread → peer の binding を新規 (or 上書き) する。
          - `touch()` は **既存の thread → peer binding は変更せず**、
            `_peer_to_latest_thread[peer]` だけを更新する (= peer の inbox push を
            この thread に reply させたい、という意図の明示)。

        想定用途: thread 内 follow-up の relay 成功時、その peer の「直近の
        会話 thread」を 当該 thread に更新する。`bind()` を呼んでも結果は
        同じになるが、新規 binding 追加と区別がつかなくなる副作用を避ける。

        `touch()` は通常「既 bind 済みの thread」が対象なので channel 切替は
        起きない想定だが、defensive に `_active_channel` も sync する
        (= follow-up の channel が新たな active になる、bind と一貫)。

        全 arg が空文字 / falsy なら no-op (= defensive)。
        """
        if not channel or not thread_ts or not peer:
            return
        if self._active_channel is not None and self._active_channel != channel:
            # touch でも channel 切替が起きたら 旧 binding を捨てる (bind と同じ semantics)
            self._thread_to_peer.clear()
            self._peer_to_latest_thread.clear()
        self._active_channel = channel
        self._active_thread_ts = thread_ts
        self._peer_to_latest_thread[peer] = (channel, thread_ts)

    def peer_for_thread(self, *, channel: str, thread_ts: str) -> str | None:
        """指定 thread に紐付いている peer を返す. 未 bind なら None.

        thread 内で `@bot` 無しの reply を見たとき、ここを引いて「この thread の
        会話相手」を決める用途。
        """
        if not channel or not thread_ts:
            return None
        return self._thread_to_peer.get((channel, thread_ts))

    def thread_for_peer(self, peer: str) -> tuple[str, str] | None:
        """peer の最新 thread を `(channel, thread_ts)` で返す. 未 bind なら None.

        agent-hub の inbox 経由で peer から message が来た時、どの Slack thread に
        post すべきかを引く用途。同 peer で複数 thread に並行参加した場合、
        最後に bind / touch された方を返す (= 直近の会話に reply する想定)。
        """
        if not peer:
            return None
        return self._peer_to_latest_thread.get(peer)


# ============================================================================
# M4: agent-hub エラー分類 — agent-hub-sdk に移行済 (= 旧 hub.py 廃止と同時期)
# ============================================================================
#
# 旧版では本 module 内に `classify_hub_error` + `_HUB_*_PATTERNS` を持っていた
# が、agent-hub-sdk の `errors` モジュールに同等品が ある (= 英 / 日 pattern
# 共通) ため、こちらは re-export のみ残す。bridge-slack 外の consumer も
# 同じ classifier を使えるよう、依存方向は bridge-slack → SDK の一方向。
#
# 移行前 import で本 module 経由で使っていた箇所は、移行 PR で agent-hub-sdk
# 直接 import に置換済。本 re-export は **後方互換維持用** のみ。

from agent_hub_sdk import HubErrorKind, classify_hub_error  # noqa: E402, F401

__sdk_classifier_exports__ = ("HubErrorKind", "classify_hub_error")


def parse_slack_retry_after(exception: BaseException) -> int | None:
    """Slack の rate-limit 例外から Retry-After 秒数を取り出す.

    対応するのは `slack_sdk.errors.SlackApiError`:
      - `.response.get("error") == "ratelimited"` で rate limit 判定
      - `.response.headers["Retry-After"]` (秒) を 推奨待機時間として使う

    duck typing で書いているので slack_sdk が install されてない test 環境でも
    そのまま動く (= response 属性を持つ偽 exception を投げて assert できる)。

    Returns:
        rate limit と判定できれば 待機秒数 (header 欠落時は 1)、それ以外は None。
    """
    response = getattr(exception, "response", None)
    if response is None:
        return None
    # SlackResponse は `.get(key)` を持つ (= dict-like)
    try:
        if response.get("error") != "ratelimited":  # type: ignore[union-attr]
            return None
    except (AttributeError, TypeError):
        return None
    headers = getattr(response, "headers", None) or {}
    try:
        retry_after = int(headers.get("Retry-After", "1"))
    except (ValueError, TypeError):
        retry_after = 1
    # 防御: 負値や 0 は 1 に丸める (= 即時 retry の暴走防止)
    return max(1, retry_after)


# ============================================================================
# Bridge-internal commands (issue #4)
# ============================================================================
#
# `@bot <peer> <body>` の peer position に bridge 自身の予約語 (`list` /
# `participants`) が来た場合、agent-hub に relay する代わりに bridge が直接
# 応答する。peer 経由ではないので token は `@list` のような偽 handle として
# parse_mention にかからないよう、別途 `parse_bot_command` で先に判定する。
#
# 現状の commands:
#   - `list` / `participants` — agent-hub の register 済 person 一覧を返す
#
# 設計判断:
#   - 予約語の判定は **case-insensitive** (= `list` / `List` / `LIST` 全部 match)。
#     Slack mobile の自動補正で先頭大文字化されるケースを救うため。peer 名は
#     慣習上 全 lowercase なので衝突 risk は事実上ゼロ。
#   - 引数 (body) は無視する (= `@bot list foo bar` でも `list` として扱う)。
#     将来 filter 引数を取りたければここで parse する。

BotCommand = Literal["list_participants"]

# bridge が予約する command 名 set. 同義語を許す (= `list` でも `participants`
# でも同じ命令)。peer 名と被るのを避けるため、user 側で `@bot @list ...` と
# `@` 付きで指定したい場合も拾えるよう、parser 側で `@` を strip する。
_BOT_COMMAND_LIST = frozenset({"list", "participants"})


def parse_bot_command(text: str | None) -> BotCommand | None:
    """`<@bot> <command>` から bridge-internal command を取り出す.

    `parse_mention` と並ぶ 1 段目の判定. `parse_mention` より先に呼んで、
    予約語に match したら peer relay を skip して bridge が直接応答する。

    判定 rule:
      1. 先頭が `<@U...>` で始まる (= bot 宛 mention)
      2. mention 直後の token が `_BOT_COMMAND_LIST` の予約語と
         **case-insensitive で一致** (= `LIST` / `List` / `list` 全部 OK)
      3. その後の token は無視 (= 将来 filter 引数を入れる余地)

    Returns:
        match した command, または None (= 通常の peer relay path に進む)。
    """
    if not text:
        return None
    m = _LEADING_MENTION_RE.match(text)
    if not m:
        return None
    rest = text[m.end():].strip()
    if not rest:
        return None
    first_token = rest.split(maxsplit=1)[0]
    # `@` 付き (`@list`) でも `@` 無し (`list`) でも同じ意味で扱う
    canonical = first_token.lstrip("@").lower()
    if canonical in _BOT_COMMAND_LIST:
        return "list_participants"
    return None


def format_participants_listing(participants: list[Participant]) -> str:
    """`get_participants` 結果を Slack post 用 text に整形する (issue #4).

    出力 format (issue #4 の「期待する動作」より):
        現在の参加者:
          @planner (online) — planner
          @reviewer (online) — reviewer
          ...

    表示 rule:
      - **online な person のみ** を一覧表示する (= 不要な offline noise を
        Slack に流さない、issue #4「is_online な参加者を一覧表示」)。
      - 表示順は `name` の昇順 (= Slack 上の見た目を安定させる)。
      - `display_name` が空 / None の場合は `name.lstrip("@")` を fallback。
      - 1 人も online で居なければ「現在 online な参加者は居ません」を返す
        (= 空 string を返すと Slack 側で post できないので必ず文章を出す)。
      - team entry は呼び出し前に除外されている前提 (`_parse_participants_json`
        が person だけを通す)。

    Returns:
        Slack に post 可能な multi-line text。末尾改行なし。
    """
    online = [p for p in participants if p.is_online]
    if not online:
        return "現在 online な参加者は居ません。"
    online.sort(key=lambda p: p.name)
    lines = ["現在の参加者:"]
    for p in online:
        display = p.display_name or p.name.lstrip("@")
        lines.append(f"  {p.name} (online) — {display}")
    return "\n".join(lines)
