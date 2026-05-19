"""M2 unit test: hub → Slack の relay logic (slack_handler の internal helpers).

`_post_one` / `_drain_inbox_to_slack` の分岐パターンを fake client で網羅する。
実 Slack API / 実 MCP には接続しない (= 単体 test として隔離)。
"""

from __future__ import annotations

import pytest
from agent_hub_sdk import IncomingMessage

from agent_hub_bridges.slack.config import Config
from agent_hub_bridges.slack.routing import ThreadContext
from agent_hub_bridges.slack.slack_handler import _drain_inbox_to_slack, _post_one


def _make_config(*, user: str = "slack-bot", default_channel: str | None = "C0123") -> Config:
    """test 用の Config を組み立てる. dataclass の field を直接渡すだけ.

    monorepo 化で `BaseConfig` を継承しているため、 slack bridge では
    使わない `workdir` field にも明示 None を渡す必要がある。
    """
    return Config(
        slack_bot_token="xoxb-test",
        slack_app_token="xapp-test",
        slack_default_channel=default_channel,
        user=user,
        display_name=None,
        tenant=None,
        agent_hub_url="http://localhost/mcp",
        github_pat="ghp_test",
        workdir=None,
    )


def _make_msg(
    *,
    msg_id: str = "msg-1",
    sender: str = "@gemma",
    to: str = "@slack-bot",
    body: str = "hello",
) -> IncomingMessage:
    return IncomingMessage(
        id=msg_id,
        sender=sender,
        to=to,
        body=body,
        timestamp="2026-05-13T13:00:00Z",
    )


class _FakeSlackClient:
    """slack_sdk.web.async_client.AsyncWebClient の最小 stub.

    chat_postMessage を呼ばれた回数 / 引数を覚えるだけ。`fail_with` をセット
    すると raise する (= Slack API エラー再現)。
    """

    def __init__(self, fail_with: Exception | None = None) -> None:
        self.calls: list[dict] = []
        self.fail_with = fail_with

    async def chat_postMessage(self, *, channel: str, text: str, thread_ts: str | None) -> dict:
        self.calls.append({"channel": channel, "text": text, "thread_ts": thread_ts})
        if self.fail_with is not None:
            raise self.fail_with
        return {"ok": True, "ts": "1700000000.000100"}


class _FakeHub:
    """HubSession の最小 stub. get_unread / ack のみ."""

    def __init__(self, unread: list[IncomingMessage] | None = None) -> None:
        self.unread = list(unread or [])
        self.marked_read: list[str] = []
        self.get_unread_raise: Exception | None = None
        self.mark_raise: Exception | None = None

    async def get_unread(self) -> list[IncomingMessage]:
        if self.get_unread_raise is not None:
            raise self.get_unread_raise
        out = list(self.unread)
        self.unread.clear()
        return out

    async def ack(self, message_id: str) -> None:
        if self.mark_raise is not None:
            raise self.mark_raise
        self.marked_read.append(message_id)


# ----- _post_one ---------------------------------------------------------


class TestPostOne:
    @pytest.mark.asyncio
    async def test_posts_to_default_channel_and_returns_true(self) -> None:
        client = _FakeSlackClient()
        config = _make_config(default_channel="C0123")
        msg = _make_msg(sender="@gemma", body="やほー")

        should_ack = await _post_one(client, config, msg)

        assert should_ack is True
        assert client.calls == [
            {
                "channel": "C0123",
                "text": "*@gemma* via agent-hub:\nやほー",
                "thread_ts": None,
            }
        ]

    @pytest.mark.asyncio
    async def test_self_echo_is_drained_without_post(self) -> None:
        # sender が自分自身ならば永久 loop 防止のため drain (= ack=True) し post もしない
        client = _FakeSlackClient()
        config = _make_config(user="slack-bot")
        msg = _make_msg(sender="@slack-bot", body="echo")

        should_ack = await _post_one(client, config, msg)

        assert should_ack is True
        assert client.calls == []

    @pytest.mark.asyncio
    async def test_missing_default_channel_drains_without_post(self) -> None:
        # SLACK_DEFAULT_CHANNEL 未設定なら 永久 retry を避けるため drain (ack=True)
        client = _FakeSlackClient()
        config = _make_config(default_channel=None)
        msg = _make_msg()

        should_ack = await _post_one(client, config, msg)

        assert should_ack is True
        assert client.calls == []

    @pytest.mark.asyncio
    async def test_slack_api_error_returns_false_for_retry(self) -> None:
        # Slack API が落ちたら ack せず、次の push で retry されるようにする
        client = _FakeSlackClient(fail_with=RuntimeError("rate limited"))
        config = _make_config()
        msg = _make_msg()

        should_ack = await _post_one(client, config, msg)

        assert should_ack is False
        # 呼び出しは試みた
        assert len(client.calls) == 1

    # ----- M3: ThreadContext を渡した場合 ----------------------------------

    @pytest.mark.asyncio
    async def test_thread_bound_post_replies_in_thread(self) -> None:
        # peer の最新 thread が bind されてれば そこに reply する
        client = _FakeSlackClient()
        config = _make_config(default_channel="C0123")
        ctx = ThreadContext()
        ctx.bind(channel="C9", thread_ts="t-original", peer="@gemma")

        msg = _make_msg(sender="@gemma", body="reply")
        should_ack = await _post_one(client, config, msg, ctx)

        assert should_ack is True
        assert client.calls == [
            {
                "channel": "C9",
                "text": "*@gemma* via agent-hub:\nreply",
                "thread_ts": "t-original",
            }
        ]

    @pytest.mark.asyncio
    async def test_thread_bound_takes_precedence_over_default_channel(self) -> None:
        # default channel があっても thread bind が優先
        client = _FakeSlackClient()
        config = _make_config(default_channel="C-default")
        ctx = ThreadContext()
        ctx.bind(channel="C-thread", thread_ts="t-1", peer="@gemma")

        msg = _make_msg(sender="@gemma")
        await _post_one(client, config, msg, ctx)

        assert client.calls[0]["channel"] == "C-thread"
        assert client.calls[0]["thread_ts"] == "t-1"

    @pytest.mark.asyncio
    async def test_unbound_peer_sticks_to_active_thread(self) -> None:
        # admin@2026-05-14 第二次要望 (sticky 化): 別 peer に bind 済みの thread が
        # あれば、未 bind の peer から来た message も **その同じ thread** に集める。
        # 以前は active_channel の root に post していたが、別 agent からの返信が
        # 別の場所に散らばる問題があったため、active thread への sticky 化に変更。
        client = _FakeSlackClient()
        config = _make_config(default_channel="C-default")
        ctx = ThreadContext()
        ctx.bind(channel="C-active", thread_ts="t-1", peer="@claude-reviewer")

        msg = _make_msg(sender="@gemma")
        await _post_one(client, config, msg, ctx)

        # active (channel, thread_ts) に sticky に reply (= 同 thread に集約)
        assert client.calls[0]["channel"] == "C-active"
        assert client.calls[0]["thread_ts"] == "t-1"

    @pytest.mark.asyncio
    async def test_unbound_peer_falls_back_to_default_when_no_active(self) -> None:
        # ThreadContext を渡しても 一度も bind してなければ default channel に流れる
        # (= active_channel が None の場合の従来挙動を保つ)
        client = _FakeSlackClient()
        config = _make_config(default_channel="C-default")
        ctx = ThreadContext()  # 何も bind しない

        msg = _make_msg(sender="@gemma")
        await _post_one(client, config, msg, ctx)

        assert client.calls[0]["channel"] == "C-default"
        assert client.calls[0]["thread_ts"] is None

    @pytest.mark.asyncio
    async def test_no_default_no_thread_drains(self) -> None:
        # default channel 未設定 + thread 未 bind → drain (永久 retry 回避)
        client = _FakeSlackClient()
        config = _make_config(default_channel=None)
        ctx = ThreadContext()

        msg = _make_msg(sender="@gemma")
        should_ack = await _post_one(client, config, msg, ctx)

        assert should_ack is True
        assert client.calls == []

    @pytest.mark.asyncio
    async def test_latest_active_thread_wins_when_multiple_threads(self) -> None:
        # bind を重ねたら 直近 (= active) thread に reply (sticky semantics)。
        # 旧挙動: peer 別最新 thread を引いていた
        # 新挙動: active_thread_ts (= 最後に bind/touch した thread) に sticky
        client = _FakeSlackClient()
        config = _make_config()
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t-old", peer="@gemma")
        ctx.bind(channel="C1", thread_ts="t-new", peer="@gemma")

        msg = _make_msg(sender="@gemma")
        await _post_one(client, config, msg, ctx)

        assert client.calls[0]["thread_ts"] == "t-new"

    @pytest.mark.asyncio
    async def test_any_peer_sticks_to_active_thread(self) -> None:
        # admin@2026-05-14 第二次要望 (sticky 化): bind 済 thread とは別 peer に
        # 後から bind した場合、active が新 thread に切替わる。以降 全 peer の
        # 返信は新 active thread に集まる。
        client = _FakeSlackClient()
        config = _make_config()
        ctx = ThreadContext()
        ctx.bind(channel="C1", thread_ts="t-g", peer="@gemma")
        # 後から別 thread で別 peer を bind → active が t-r に切替
        ctx.bind(channel="C1", thread_ts="t-r", peer="@claude-reviewer")

        # @gemma の返信も新 active (t-r) に sticky
        await _post_one(client, config, _make_msg(sender="@gemma"), ctx)
        assert client.calls[-1]["thread_ts"] == "t-r"

        # @claude-reviewer の返信ももちろん t-r
        await _post_one(client, config, _make_msg(sender="@claude-reviewer"), ctx)
        assert client.calls[-1]["thread_ts"] == "t-r"

        # 全く別 peer の返信も同じ thread に集まる
        await _post_one(client, config, _make_msg(sender="@stranger"), ctx)
        assert client.calls[-1]["thread_ts"] == "t-r"


# ----- _drain_inbox_to_slack --------------------------------------------


class TestDrainInboxToSlack:
    @pytest.mark.asyncio
    async def test_drains_multiple_and_marks_each(self) -> None:
        hub = _FakeHub(
            unread=[
                _make_msg(msg_id="m1", sender="@gemma", body="a"),
                _make_msg(msg_id="m2", sender="@claude-reviewer", body="b"),
            ]
        )
        client = _FakeSlackClient()
        config = _make_config()

        await _drain_inbox_to_slack(hub, client, config)

        assert [c["channel"] for c in client.calls] == ["C0123", "C0123"]
        assert hub.marked_read == ["m1", "m2"]

    @pytest.mark.asyncio
    async def test_no_unread_is_noop(self) -> None:
        hub = _FakeHub(unread=[])
        client = _FakeSlackClient()
        config = _make_config()

        await _drain_inbox_to_slack(hub, client, config)

        assert client.calls == []
        assert hub.marked_read == []

    @pytest.mark.asyncio
    async def test_slack_error_leaves_unread_unmarked(self) -> None:
        # post 失敗 → mark_as_read を呼ばずに次の push で retry できる状態にする
        hub = _FakeHub(unread=[_make_msg(msg_id="m1")])
        client = _FakeSlackClient(fail_with=RuntimeError("boom"))
        config = _make_config()

        await _drain_inbox_to_slack(hub, client, config)

        assert hub.marked_read == []

    @pytest.mark.asyncio
    async def test_get_unread_failure_is_swallowed(self) -> None:
        # hub 側一時障害は次の push で recover を期待 (loop は止めない)
        hub = _FakeHub()
        hub.get_unread_raise = RuntimeError("hub 502")
        client = _FakeSlackClient()
        config = _make_config()

        await _drain_inbox_to_slack(hub, client, config)  # should not raise

        assert client.calls == []
        assert hub.marked_read == []

    @pytest.mark.asyncio
    async def test_mark_as_read_failure_is_swallowed(self) -> None:
        # 投稿は成功したが mark_as_read が失敗 → loop は継続させたい
        hub = _FakeHub(unread=[_make_msg(msg_id="m1"), _make_msg(msg_id="m2", body="c")])
        hub.mark_raise = RuntimeError("mark down")
        client = _FakeSlackClient()
        config = _make_config()

        await _drain_inbox_to_slack(hub, client, config)  # should not raise

        # post は両方 試みた
        assert len(client.calls) == 2

    @pytest.mark.asyncio
    async def test_mixed_self_echo_and_normal(self) -> None:
        hub = _FakeHub(
            unread=[
                _make_msg(msg_id="m1", sender="@slack-bot", body="self"),
                _make_msg(msg_id="m2", sender="@gemma", body="normal"),
            ]
        )
        client = _FakeSlackClient()
        config = _make_config(user="slack-bot")

        await _drain_inbox_to_slack(hub, client, config)

        # self echo は drain (mark) されるが post されない、normal は両方
        assert [c["text"] for c in client.calls] == ["*@gemma* via agent-hub:\nnormal"]
        assert hub.marked_read == ["m1", "m2"]

    @pytest.mark.asyncio
    async def test_drain_all_peers_stick_to_latest_active_thread(self) -> None:
        # admin@2026-05-14 第二次要望 (sticky 化): 同一 channel 内で 2 peer が
        # 別 thread に bind された場合、active は **最後に bind した方** に倒れる。
        # それ以降 全 peer の返信は active thread に sticky に集約される
        # (= 別 agent からの返信が「別の場所」に散らばらない)。
        hub = _FakeHub(
            unread=[
                _make_msg(msg_id="m1", sender="@gemma", body="a"),
                _make_msg(msg_id="m2", sender="@claude-reviewer", body="b"),
            ]
        )
        client = _FakeSlackClient()
        config = _make_config(default_channel=None)
        ctx = ThreadContext()
        ctx.bind(channel="C-active", thread_ts="t-g", peer="@gemma")
        # 後から bind: active が t-r に切替
        ctx.bind(channel="C-active", thread_ts="t-r", peer="@claude-reviewer")

        await _drain_inbox_to_slack(hub, client, config, ctx)

        assert client.calls == [
            # @gemma の返信も新 active thread に sticky
            {
                "channel": "C-active",
                "text": "*@gemma* via agent-hub:\na",
                "thread_ts": "t-r",
            },
            {
                "channel": "C-active",
                "text": "*@claude-reviewer* via agent-hub:\nb",
                "thread_ts": "t-r",
            },
        ]
        assert hub.marked_read == ["m1", "m2"]

    @pytest.mark.asyncio
    async def test_drain_unbound_peer_sticks_to_active_thread(self) -> None:
        # admin@2026-05-14 第二次要望 (sticky 化): bind されてない peer から
        # message が来ても、active thread に reply する (= drop しない、かつ
        # bind 済 peer の返信と同じ場所に集約)。
        hub = _FakeHub(
            unread=[
                _make_msg(msg_id="m1", sender="@gemma", body="bound"),
                _make_msg(msg_id="m2", sender="@stranger", body="unbound"),
            ]
        )
        client = _FakeSlackClient()
        config = _make_config(default_channel=None)
        ctx = ThreadContext()
        ctx.bind(channel="C-active", thread_ts="t-g", peer="@gemma")

        await _drain_inbox_to_slack(hub, client, config, ctx)

        assert client.calls == [
            # @gemma は active thread へ
            {
                "channel": "C-active",
                "text": "*@gemma* via agent-hub:\nbound",
                "thread_ts": "t-g",
            },
            # @stranger も同じ active thread に集約 (= 別の場所に散らばらない)
            {
                "channel": "C-active",
                "text": "*@stranger* via agent-hub:\nunbound",
                "thread_ts": "t-g",
            },
        ]
        assert hub.marked_read == ["m1", "m2"]


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
