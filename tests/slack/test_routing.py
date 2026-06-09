"""routing.py の unit test.

DESIGN.md §6 M5 で言及されてる「routing.py の parse の TDD」相当。
M1 でも parse_mention は本番 path なので前倒しで test しておく。
"""

from __future__ import annotations

import pytest
from agent_hub_sdk import Participant

# ``classify_hub_error`` lives in agent-hub-sdk now; we keep a re-export from
# ``agent_hub_bridges.slack.routing`` for backward compat and the test
# imports the wrapper path on purpose to keep the re-export covered.
from agent_hub_bridges.slack.routing import (
    ParsedMention,
    ThreadContext,
    classify_hub_error,
    format_hub_to_slack_text,
    format_participants_listing,
    format_relay_body,
    parse_bot_command,
    parse_mention,
    parse_slack_retry_after,
)


class TestParseMention:
    def test_basic_mention_peer_body(self) -> None:
        result = parse_mention("<@U01BOT> gemma 翻訳して")
        assert result == ParsedMention(
            bot_user_id="U01BOT",
            peer="@gemma",
            body="翻訳して",
        )

    def test_peer_with_leading_at_normalized(self) -> None:
        # ユーザが `@gemma` と書いても `gemma` と書いても同じ canonical 形に
        result = parse_mention("<@U01BOT> @gemma 翻訳して")
        assert result is not None
        assert result.peer == "@gemma"
        assert result.body == "翻訳して"

    def test_body_can_have_multiple_words_and_newlines(self) -> None:
        result = parse_mention("<@U01BOT> gemma この PR\nの差分を英訳して")
        assert result is not None
        assert result.peer == "@gemma"
        assert result.body == "この PR\nの差分を英訳して"

    def test_body_can_contain_other_mentions(self) -> None:
        # bot 自身の mention だけが冒頭で剥がされ、本文中の他 mention は残る
        result = parse_mention(
            "<@U01BOT> claude-reviewer <@U89999999> が翻訳した結果、確認お願い"
        )
        assert result is not None
        assert result.peer == "@claude-reviewer"
        assert result.body == "<@U89999999> が翻訳した結果、確認お願い"

    def test_mention_with_display_alias(self) -> None:
        # Slack の <@U01BOT|botname> 形式にも対応
        result = parse_mention("<@U01BOT|slack-bot> gemma hi")
        assert result is not None
        assert result.bot_user_id == "U01BOT"
        assert result.peer == "@gemma"
        assert result.body == "hi"

    def test_leading_whitespace_after_mention_tolerated(self) -> None:
        result = parse_mention("<@U01BOT>     gemma   hi")
        assert result is not None
        assert result.peer == "@gemma"
        assert result.body == "hi"

    def test_no_mention_returns_none(self) -> None:
        # bot mention が無い ＝ relay 対象外
        assert parse_mention("gemma hi") is None
        assert parse_mention("hi @gemma") is None

    def test_mention_only_no_peer_returns_none(self) -> None:
        assert parse_mention("<@U01BOT>") is None
        assert parse_mention("<@U01BOT>   ") is None

    def test_peer_only_no_body_returns_empty_body(self) -> None:
        # `@bot gemma` だけ ＝ peer はあるが body 空。relay は可能と扱う。
        result = parse_mention("<@U01BOT> gemma")
        assert result is not None
        assert result.peer == "@gemma"
        assert result.body == ""

    def test_empty_or_none_returns_none(self) -> None:
        assert parse_mention("") is None
        assert parse_mention(None) is None


class TestFormatRelayBody:
    def test_basic_format(self) -> None:
        out = format_relay_body(
            channel_id="C0123",
            user_display="alice",
            body="この PR の差分を英訳して",
        )
        assert out == "(via slack <#C0123> by @alice):\nこの PR の差分を英訳して"

    def test_multiline_body_preserved(self) -> None:
        out = format_relay_body(
            channel_id="C0123",
            user_display="alice",
            body="line 1\nline 2",
        )
        assert out == "(via slack <#C0123> by @alice):\nline 1\nline 2"

    def test_empty_body_still_prefixed(self) -> None:
        # parse_mention が body="" を返すケースに対応
        out = format_relay_body(channel_id="C0123", user_display="alice", body="")
        assert out == "(via slack <#C0123> by @alice):\n"


class TestFormatHubToSlackText:
    def test_basic(self) -> None:
        out = format_hub_to_slack_text(sender="@gemma", body="hello")
        assert out == "*@gemma* via agent-hub:\nhello"

    def test_missing_at_prefix_added(self) -> None:
        # agent-hub の field 名規約変更時の保険 (defensive)
        out = format_hub_to_slack_text(sender="gemma", body="hello")
        assert out == "*@gemma* via agent-hub:\nhello"

    def test_multiline_body_preserved(self) -> None:
        out = format_hub_to_slack_text(sender="@claude", body="line1\nline2")
        assert out == "*@claude* via agent-hub:\nline1\nline2"

    def test_empty_body(self) -> None:
        out = format_hub_to_slack_text(sender="@gemma", body="")
        assert out == "*@gemma* via agent-hub:\n"


class TestThreadContext:
    """M3 の thread ↔ peer map. Slack handler / hub→Slack の両側で参照される."""

    def test_unbound_thread_returns_none(self) -> None:
        ctx = ThreadContext()
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") is None

    def test_unknown_peer_returns_none(self) -> None:
        ctx = ThreadContext()
        assert ctx.thread_for_peer("@gemma") is None

    def test_bind_then_lookup_both_directions(self) -> None:
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") == "@gemma"
        assert ctx.thread_for_peer("@gemma") == ("C1", "t1")

    def test_rebind_same_thread_overwrites_peer(self) -> None:
        # 同 thread を 違う peer に振り替えた (例: ユーザが間違って別 peer mention) 場合
        # 新しい binding が勝つ。古い peer の latest thread は依然 残るが、
        # それは「直近 active な thread」の意味として許容範囲。
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.bind(channel="C1", thread_ts="t1", peer="@claude-reviewer")
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") == "@claude-reviewer"
        assert ctx.thread_for_peer("@claude-reviewer") == ("C1", "t1")

    def test_peer_latest_thread_updates_on_rebind(self) -> None:
        # 同 peer を 別 thread に bind し直したら、最新 thread が更新される
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.bind(channel="C1", thread_ts="t2", peer="@gemma")
        assert ctx.thread_for_peer("@gemma") == ("C1", "t2")
        # 古い thread も bind は残る (= follow-up が来たら同 peer に流せる)
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") == "@gemma"
        assert ctx.peer_for_thread(channel="C1", thread_ts="t2") == "@gemma"

    def test_multiple_peers_same_channel_independent(self) -> None:
        # 同一 channel 内で 異なる peer を bind すると、両方の binding が保持される
        # (= channel 切替が起きない限り 旧 binding は破棄されない)
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.bind(channel="C1", thread_ts="t9", peer="@claude-reviewer")
        assert ctx.thread_for_peer("@gemma") == ("C1", "t1")
        assert ctx.thread_for_peer("@claude-reviewer") == ("C1", "t9")
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") == "@gemma"
        assert ctx.peer_for_thread(channel="C1", thread_ts="t9") == "@claude-reviewer"

    def test_bind_in_different_channel_rebinds_everything(self) -> None:
        # admin@2026-05-14 要望: 別 channel から bind されたら 旧 channel の
        # binding は全部破棄して 新 channel に "全体を bind し直す"。
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.bind(channel="C1", thread_ts="t2", peer="@claude-reviewer")
        # 別 channel C2 で bind → C1 の binding は全消える
        ctx.bind(channel="C2", thread_ts="t9", peer="@oracle")
        # C2 側 (新 active) の binding は残る
        assert ctx.active_channel == "C2"
        assert ctx.thread_for_peer("@oracle") == ("C2", "t9")
        assert ctx.peer_for_thread(channel="C2", thread_ts="t9") == "@oracle"
        # C1 側 (旧) の binding は全消滅
        assert ctx.thread_for_peer("@gemma") is None
        assert ctx.thread_for_peer("@claude-reviewer") is None
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") is None
        assert ctx.peer_for_thread(channel="C1", thread_ts="t2") is None

    def test_active_channel_initially_none(self) -> None:
        # 未 bind 状態では active_channel は None
        ctx = ThreadContext()
        assert ctx.active_channel is None

    def test_active_channel_updates_on_bind(self) -> None:
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        assert ctx.active_channel == "C1"
        ctx.bind(channel="C2", thread_ts="t9", peer="@oracle")
        assert ctx.active_channel == "C2"

    def test_active_channel_persists_after_touch_in_same_channel(self) -> None:
        # 同 channel 内の touch では active_channel は変わらない (= 当然)
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.touch(channel="C1", thread_ts="t2", peer="@gemma")
        assert ctx.active_channel == "C1"

    # ----- active_thread_ts (admin@2026-05-14 第二次要望: sticky 化) ----
    #
    # 「一度 bind された channel/thread に sticky にして。active_channel に bind
    # されていれば、どの peer からの返信もそこに集まるようにして」を表現する
    # property. bind/touch ごとに active_channel と pair で更新される。

    def test_active_thread_ts_initially_none(self) -> None:
        ctx = ThreadContext()
        assert ctx.active_thread_ts is None

    def test_active_thread_ts_updates_on_bind(self) -> None:
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        assert ctx.active_thread_ts == "t1"
        # 別 thread での bind は 新 thread に切替 (= 会話の主導権が新 thread に)
        ctx.bind(channel="C1", thread_ts="t2", peer="@gemma")
        assert ctx.active_thread_ts == "t2"

    def test_active_thread_ts_updates_on_bind_with_different_peer(self) -> None:
        # 別 peer の bind でも active_thread_ts は 新 thread に切替わる
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.bind(channel="C1", thread_ts="t9", peer="@claude-reviewer")
        assert ctx.active_thread_ts == "t9"
        # 古い thread の binding は残る (= 別 thread での follow-up は壊れない)
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") == "@gemma"

    def test_active_thread_ts_updates_on_touch(self) -> None:
        # touch でも 直近の thread が active になる
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.touch(channel="C1", thread_ts="t2", peer="@gemma")
        assert ctx.active_thread_ts == "t2"

    def test_active_thread_ts_resets_on_channel_switch(self) -> None:
        # channel 切替で 旧 binding 全消し → 新 channel/thread が active
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.bind(channel="C2", thread_ts="t9", peer="@oracle")
        assert (ctx.active_channel, ctx.active_thread_ts) == ("C2", "t9")

    def test_different_channels_same_thread_ts_isolated(self) -> None:
        # Slack の thread_ts は channel 横断では unique でないので key には channel も含める
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        assert ctx.peer_for_thread(channel="C2", thread_ts="t1") is None

    def test_empty_args_are_noop(self) -> None:
        # defensive: 不正な arg で map を汚さない
        ctx = ThreadContext()
        ctx.bind(channel="", thread_ts="t1", peer="@gemma")
        ctx.bind(channel="C1", thread_ts="", peer="@gemma")
        ctx.bind(channel="C1", thread_ts="t1", peer="")
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") is None
        assert ctx.thread_for_peer("@gemma") is None

    def test_lookup_with_empty_args_returns_none(self) -> None:
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        assert ctx.peer_for_thread(channel="", thread_ts="t1") is None
        assert ctx.peer_for_thread(channel="C1", thread_ts="") is None
        assert ctx.thread_for_peer("") is None

    def test_touch_updates_latest_thread_only(self) -> None:
        # touch() は peer の最新 thread を持ち上げるだけで、新しい
        # thread → peer binding は入れない (= 既 bind 済みの thread 専用 API)。
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.touch(channel="C1", thread_ts="t2", peer="@gemma")
        # 最新 thread は t2 に更新される
        assert ctx.thread_for_peer("@gemma") == ("C1", "t2")
        # ただし t2 → peer の binding は入っていない (touch は副作用を絞る)
        assert ctx.peer_for_thread(channel="C1", thread_ts="t2") is None
        # 元の bind は残る
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") == "@gemma"

    def test_touch_with_same_thread_is_noop_semantically(self) -> None:
        # 既 bind の thread を そのまま touch しても 状態は変わらない
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.touch(channel="C1", thread_ts="t1", peer="@gemma")
        assert ctx.thread_for_peer("@gemma") == ("C1", "t1")
        assert ctx.peer_for_thread(channel="C1", thread_ts="t1") == "@gemma"

    def test_touch_empty_args_are_noop(self) -> None:
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t1", peer="@gemma")
        ctx.touch(channel="", thread_ts="t2", peer="@gemma")
        ctx.touch(channel="C1", thread_ts="", peer="@gemma")
        ctx.touch(channel="C1", thread_ts="t2", peer="")
        # どれも最新 thread を更新しない
        assert ctx.thread_for_peer("@gemma") == ("C1", "t1")


class TestClassifyHubError:
    """M4: agent-hub の error 文字列を participant_not_found / transient / unknown に分ける."""

    def test_empty_returns_unknown(self) -> None:
        assert classify_hub_error("") == "unknown"
        assert classify_hub_error(None) == "unknown"

    def test_participant_not_found_phrases(self) -> None:
        for text in [
            "peer @gemma not found",
            "Peer not online",
            "no such peer: claude-reviewer",
            "Unknown peer @bot",
            "User does not exist",
            "peer not registered yet",
            "Recipient is offline",
        ]:
            assert classify_hub_error(text) == "participant_not_found", text

    def test_transient_phrases(self) -> None:
        for text in [
            "503 Service Unavailable",
            "502 Bad Gateway",
            "504 Gateway Timeout",
            "connection refused",
            "Network error: ECONNRESET",
            "request timed out",
            "please try again later",
            "service temporarily unavailable",
        ]:
            assert classify_hub_error(text) == "transient", text

    def test_unknown_phrases(self) -> None:
        for text in [
            "invalid arguments",
            "schema validation failed",
            "permission denied",
            "internal logic error xyz",
        ]:
            assert classify_hub_error(text) == "unknown", text

    def test_case_insensitive(self) -> None:
        assert classify_hub_error("PEER NOT FOUND") == "participant_not_found"
        assert classify_hub_error("ServICE Unavailable") == "transient"

    def test_japanese_participant_not_found_phrases(self) -> None:
        # issue #6: agent-hub server は Japanese で error を返す。
        # 実観測 phrase + 想定類義語の全てが participant_not_found に分類されること。
        for text in [
            "宛先 @list は存在しません",  # 実観測 (agent-hub server の生 phrase)
            "@gemma は存在しません",
            "peer が見つかりません",
            "@xyz は登録されていません",
            "宛先がオフラインです",
        ]:
            assert classify_hub_error(text) == "participant_not_found", text

    def test_japanese_transient_phrases(self) -> None:
        # 同様に 5xx / network / timeout 系の Japanese 表現も拾う
        for text in [
            "agent-hub がタイムアウトしました",
            "サーバが一時的に応答できません",
            "agent-hub が応答していません",
            "しばらくしてから再試行してください",
        ]:
            assert classify_hub_error(text) == "transient", text

    def test_japanese_unknown_still_unknown(self) -> None:
        # 日本語でも pattern に match しないなら unknown に倒す (= 安全側)
        for text in [
            "引数が不正です",
            "権限がありません",
            "schema 検証 error",
        ]:
            assert classify_hub_error(text) == "unknown", text


class TestParseSlackRetryAfter:
    """M4: slack_sdk.SlackApiError 風の例外から Retry-After を取り出す."""

    @staticmethod
    def _make(error: str | None, retry_after: str | None = None) -> Exception:
        """SlackApiError の最低限 stub."""

        class _Resp:
            def __init__(self, err, ra):
                self._err = err
                self.headers = {"Retry-After": ra} if ra is not None else {}

            def get(self, key, default=None):
                if key == "error":
                    return self._err
                return default

        class _Exc(Exception):
            pass

        e = _Exc("boom")
        e.response = _Resp(error, retry_after)
        return e

    def test_plain_exception_returns_none(self) -> None:
        assert parse_slack_retry_after(RuntimeError("plain")) is None

    def test_response_without_ratelimited_returns_none(self) -> None:
        e = self._make(error="channel_not_found")
        assert parse_slack_retry_after(e) is None

    def test_ratelimited_with_header_returns_seconds(self) -> None:
        e = self._make(error="ratelimited", retry_after="42")
        assert parse_slack_retry_after(e) == 42

    def test_ratelimited_without_header_defaults_to_one(self) -> None:
        e = self._make(error="ratelimited")
        assert parse_slack_retry_after(e) == 1

    def test_ratelimited_with_garbage_header_defaults_to_one(self) -> None:
        e = self._make(error="ratelimited", retry_after="not-a-number")
        assert parse_slack_retry_after(e) == 1

    def test_negative_or_zero_clamped_to_one(self) -> None:
        e = self._make(error="ratelimited", retry_after="-5")
        assert parse_slack_retry_after(e) == 1
        e = self._make(error="ratelimited", retry_after="0")
        assert parse_slack_retry_after(e) == 1


class TestParseBotCommand:
    """issue #4: `@bot list` / `@bot participants` の予約語判定."""

    def test_list_returns_list_participants(self) -> None:
        assert parse_bot_command("<@U01BOT> list") == "list_participants"

    def test_participants_returns_list_participants(self) -> None:
        assert parse_bot_command("<@U01BOT> participants") == "list_participants"

    def test_at_prefix_tolerated(self) -> None:
        # `@bot @list` のように handle 形式で書かれても拾う
        assert parse_bot_command("<@U01BOT> @list") == "list_participants"
        assert parse_bot_command("<@U01BOT> @participants") == "list_participants"

    def test_extra_args_ignored(self) -> None:
        # 引数があっても command として認識する (= 将来 filter 用に予約)
        assert parse_bot_command("<@U01BOT> list online") == "list_participants"
        assert parse_bot_command("<@U01BOT> participants foo bar") == "list_participants"

    def test_leading_whitespace_tolerated(self) -> None:
        assert parse_bot_command("<@U01BOT>   list") == "list_participants"

    def test_mention_with_display_alias(self) -> None:
        assert parse_bot_command("<@U01BOT|slack-bot> list") == "list_participants"

    def test_peer_handle_returns_none(self) -> None:
        # 通常の peer relay は command として認識しない (= None で fall through)
        assert parse_bot_command("<@U01BOT> gemma hi") is None
        assert parse_bot_command("<@U01BOT> claude-reviewer 確認お願い") is None

    def test_case_insensitive_match(self) -> None:
        # Slack mobile の auto-cap 救済のため case-insensitive で判定する
        # (peer 名は慣習上 lowercase なので衝突 risk は事実上ゼロ)
        assert parse_bot_command("<@U01BOT> LIST") == "list_participants"
        assert parse_bot_command("<@U01BOT> List") == "list_participants"
        assert parse_bot_command("<@U01BOT> Participants") == "list_participants"

    def test_no_mention_returns_none(self) -> None:
        # 行頭が bot mention でない場合 (= channel 内の通常発言) は無視
        assert parse_bot_command("list") is None
        assert parse_bot_command("please list participants") is None

    def test_mention_only_returns_none(self) -> None:
        # `@bot` だけ (= command も peer も無い) は usage hint path に倒す
        assert parse_bot_command("<@U01BOT>") is None
        assert parse_bot_command("<@U01BOT>   ") is None

    def test_empty_or_none_returns_none(self) -> None:
        assert parse_bot_command("") is None
        assert parse_bot_command(None) is None


class TestFormatParticipantsListing:
    """issue #4: get_participants 結果の Slack 用整形."""

    @staticmethod
    def _p(
        name: str,
        *,
        display_name: str | None = None,
        mode: str | None = "stateful",
        is_online: bool = True,
    ) -> Participant:
        return Participant(
            name=name,
            display_name=display_name,
            mode=mode,
            is_online=is_online,
        )

    def test_online_only_listed(self) -> None:
        out = format_participants_listing([
            self._p("@gemma", display_name="gemma", is_online=True),
            self._p("@offline-peer", display_name="offline-peer", is_online=False),
        ])
        assert "@gemma" in out
        assert "@offline-peer" not in out

    def test_alphabetical_order(self) -> None:
        # 並び順は name 昇順 (= 表示安定性)
        out = format_participants_listing([
            self._p("@reviewer", display_name="reviewer"),
            self._p("@planner", display_name="planner"),
            self._p("@gemma", display_name="gemma"),
        ])
        # @gemma → @planner → @reviewer の順で行が並ぶこと
        lines = out.splitlines()
        gemma_line = next(i for i, line in enumerate(lines) if "@gemma" in line)
        planner_line = next(i for i, line in enumerate(lines) if "@planner" in line)
        reviewer_line = next(i for i, line in enumerate(lines) if "@reviewer" in line)
        assert gemma_line < planner_line < reviewer_line

    def test_basic_format_matches_issue_example(self) -> None:
        # issue #4 「期待する動作」例の format に合わせる
        out = format_participants_listing([
            self._p("@planner", display_name="planner"),
            self._p(
                "@researcher",
                display_name="Researcher — queue-based issue investigation",
            ),
        ])
        assert out == (
            "現在の参加者:\n"
            "  @planner (online) — planner\n"
            "  @researcher (online) — Researcher — queue-based issue investigation"
        )

    def test_missing_display_name_uses_name_fallback(self) -> None:
        # display_name が None / 空でも落ちず、handle を fallback で出す
        out = format_participants_listing([
            self._p("@gemma", display_name=None),
            self._p("@oracle", display_name=""),
        ])
        assert "@gemma (online) — gemma" in out
        assert "@oracle (online) — oracle" in out

    def test_empty_input_returns_no_one_message(self) -> None:
        # 1 人も居ない (= agent-hub が完全に空) でも post 可能な文字列を返す
        out = format_participants_listing([])
        assert out == "現在 online な参加者は居ません。"

    def test_all_offline_returns_no_one_message(self) -> None:
        # 全員 offline でも空 string は返さない (Slack で post できなくなるため)
        out = format_participants_listing([
            self._p("@gemma", is_online=False),
            self._p("@oracle", is_online=False),
        ])
        assert out == "現在 online な参加者は居ません。"


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
