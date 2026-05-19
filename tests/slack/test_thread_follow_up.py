"""M3 unit test: slack→hub の thread follow-up relay logic.

`_relay_thread_follow_up` の分岐パターンを fake で網羅する。実 Slack / 実
HubClient には接続しない。

`build_slack_app` 内の Bolt handler は本関数の薄い wrapper なので、ここを
通せば handler 動作も担保される。
"""

from __future__ import annotations

import pytest

from agent_hub_bridges.slack.routing import ThreadContext
from agent_hub_bridges.slack.slack_handler import _relay_thread_follow_up


class _FakeHub:
    """HubClient.send_message_with_retry のみ stub (M3/M4 で使う path)."""

    def __init__(self, fail_with: Exception | None = None) -> None:
        self.calls: list[dict] = []
        self.fail_with = fail_with

    async def send_with_retry(self, to: str, message: str, **kwargs) -> str:
        self.calls.append({"to": to, "message": message})
        if self.fail_with is not None:
            raise self.fail_with
        return "ok"


class _FakeWebClient:
    """slack web client (users_info) stub. display name 解決のみ."""

    def __init__(self, display: str = "alice") -> None:
        self.display = display
        self.users_info_calls: list[str] = []

    async def users_info(self, *, user: str) -> dict:
        self.users_info_calls.append(user)
        return {
            "user": {
                "profile": {"display_name": self.display},
                "name": "fallback",
            }
        }


class _Say:
    """Callable double that records `(text, thread_ts)` per invocation.

    Bound to the surrounding `calls` list at construction time so tests can
    inspect what `say(...)` was called with.
    """

    def __init__(self, calls: list[dict]) -> None:
        self._calls = calls

    async def __call__(self, *, text: str, thread_ts: str | None = None) -> None:
        self._calls.append({"text": text, "thread_ts": thread_ts})


def _make_say() -> tuple[list[dict], _Say]:
    """`say(text=..., thread_ts=...)` の呼出を記録する fake."""
    calls: list[dict] = []
    return calls, _Say(calls)


# ----- 正常系 -------------------------------------------------------------


@pytest.mark.asyncio
async def test_bound_thread_relays_to_peer() -> None:
    """thread が peer に bind されてる + mention 無しの reply → relay 発射."""
    hub = _FakeHub()
    web = _FakeWebClient(display="alice")
    say_calls, say = _make_say()
    ctx = ThreadContext()
    ctx.bind(channel="C1", thread_ts="t-1", peer="@gemma")

    event = {
        "user": "U_alice",
        "channel": "C1",
        "thread_ts": "t-1",
        "ts": "t-2",
        "text": "もう一度説明お願い",
    }

    relayed = await _relay_thread_follow_up(
        event=event, hub=hub, thread_ctx=ctx, slack_client=web, say=say
    )

    assert relayed is True
    assert hub.calls == [
        {"to": "@gemma", "message": "(via slack <#C1> by @alice):\nもう一度説明お願い"}
    ]
    # 成功時は warning say は出ない
    assert say_calls == []
    # peer の最新 thread が refresh されてる
    assert ctx.thread_for_peer("@gemma") == ("C1", "t-1")


@pytest.mark.asyncio
async def test_follow_up_keeps_peer_consistent_across_replies() -> None:
    """完了条件: 同じ thread 内で何度 reply しても peer が変わらない."""
    hub = _FakeHub()
    web = _FakeWebClient()
    _, say = _make_say()
    ctx = ThreadContext()
    ctx.bind(channel="C1", thread_ts="t-1", peer="@gemma")

    for body in ["1 回目", "2 回目", "3 回目"]:
        await _relay_thread_follow_up(
            event={
                "user": "U_alice",
                "channel": "C1",
                "thread_ts": "t-1",
                "ts": f"t-r-{body}",
                "text": body,
            },
            hub=hub, thread_ctx=ctx, slack_client=web, say=say,
        )

    # 3 回とも同じ peer に流れた
    assert [c["to"] for c in hub.calls] == ["@gemma", "@gemma", "@gemma"]
    assert ctx.peer_for_thread(channel="C1", thread_ts="t-1") == "@gemma"


@pytest.mark.asyncio
async def test_send_message_failure_posts_warning() -> None:
    """relay 失敗時は thread に warning を出す (silent fail しない)."""
    hub = _FakeHub(fail_with=RuntimeError("hub 503"))
    web = _FakeWebClient()
    say_calls, say = _make_say()
    ctx = ThreadContext()
    ctx.bind(channel="C1", thread_ts="t-1", peer="@gemma")

    relayed = await _relay_thread_follow_up(
        event={
            "user": "U_alice", "channel": "C1", "thread_ts": "t-1",
            "ts": "t-2", "text": "hi",
        },
        hub=hub, thread_ctx=ctx, slack_client=web, say=say,
    )

    assert relayed is True  # 試みた
    assert len(say_calls) == 1
    assert ":warning:" in say_calls[0]["text"]
    assert "@gemma" in say_calls[0]["text"]
    assert say_calls[0]["thread_ts"] == "t-1"


# ----- ガード条件: skip するべきケース ----------------------------------


@pytest.mark.asyncio
async def test_skip_when_thread_ctx_none() -> None:
    hub = _FakeHub()
    _, say = _make_say()
    relayed = await _relay_thread_follow_up(
        event={
            "user": "U", "channel": "C1", "thread_ts": "t-1",
            "ts": "t-2", "text": "hi",
        },
        hub=hub, thread_ctx=None, slack_client=_FakeWebClient(), say=say,
    )
    assert relayed is False
    assert hub.calls == []


@pytest.mark.asyncio
async def test_skip_bot_message() -> None:
    """bot 発話 (= bot_id 付き) は relay しない (= echo loop 防止)."""
    hub = _FakeHub()
    _, say = _make_say()
    ctx = ThreadContext()
    ctx.bind(channel="C1", thread_ts="t-1", peer="@gemma")

    relayed = await _relay_thread_follow_up(
        event={
            "user": "U", "channel": "C1", "thread_ts": "t-1",
            "ts": "t-2", "text": "from bot", "bot_id": "B0123",
        },
        hub=hub, thread_ctx=ctx, slack_client=_FakeWebClient(), say=say,
    )
    assert relayed is False
    assert hub.calls == []


@pytest.mark.asyncio
async def test_skip_subtype_messages() -> None:
    """message_changed / message_deleted / channel_join 等は無視."""
    hub = _FakeHub()
    _, say = _make_say()
    ctx = ThreadContext()
    ctx.bind(channel="C1", thread_ts="t-1", peer="@gemma")

    for subtype in ("message_changed", "message_deleted", "channel_join"):
        relayed = await _relay_thread_follow_up(
            event={
                "user": "U", "channel": "C1", "thread_ts": "t-1",
                "ts": "t-2", "text": "x", "subtype": subtype,
            },
            hub=hub, thread_ctx=ctx, slack_client=_FakeWebClient(), say=say,
        )
        assert relayed is False
    assert hub.calls == []


@pytest.mark.asyncio
async def test_skip_when_not_in_thread() -> None:
    """thread_ts 無し (= 通常 channel post) は relay 対象外."""
    hub = _FakeHub()
    _, say = _make_say()
    ctx = ThreadContext()
    ctx.bind(channel="C1", thread_ts="t-1", peer="@gemma")

    relayed = await _relay_thread_follow_up(
        event={
            "user": "U", "channel": "C1", "ts": "t-99",
            "text": "channel post",
        },
        hub=hub, thread_ctx=ctx, slack_client=_FakeWebClient(), say=say,
    )
    assert relayed is False
    assert hub.calls == []


@pytest.mark.asyncio
async def test_skip_when_bot_mentioned_at_head() -> None:
    """先頭が `<@U...>` (bot mention) → app_mention で処理される → skip."""
    hub = _FakeHub()
    _, say = _make_say()
    ctx = ThreadContext()
    ctx.bind(channel="C1", thread_ts="t-1", peer="@gemma")

    relayed = await _relay_thread_follow_up(
        event={
            "user": "U", "channel": "C1", "thread_ts": "t-1",
            "ts": "t-2", "text": "<@U01BOT> gemma 別件",
        },
        hub=hub, thread_ctx=ctx, slack_client=_FakeWebClient(), say=say,
    )
    assert relayed is False
    assert hub.calls == []


@pytest.mark.asyncio
async def test_skip_when_thread_not_bound() -> None:
    """bind されてない thread の reply は relay しない (= 過剰 relay 防止)."""
    hub = _FakeHub()
    _, say = _make_say()
    ctx = ThreadContext()
    # 別 thread を bind してても、対象 thread が無ければ skip
    ctx.bind(channel="C1", thread_ts="t-OTHER", peer="@gemma")

    relayed = await _relay_thread_follow_up(
        event={
            "user": "U", "channel": "C1", "thread_ts": "t-1",
            "ts": "t-2", "text": "random reply",
        },
        hub=hub, thread_ctx=ctx, slack_client=_FakeWebClient(), say=say,
    )
    assert relayed is False
    assert hub.calls == []


@pytest.mark.asyncio
async def test_skip_when_no_user_field() -> None:
    """user 不明 (= system が生成した message 等) は skip."""
    hub = _FakeHub()
    _, say = _make_say()
    ctx = ThreadContext()
    ctx.bind(channel="C1", thread_ts="t-1", peer="@gemma")

    relayed = await _relay_thread_follow_up(
        event={
            "channel": "C1", "thread_ts": "t-1", "ts": "t-2", "text": "x",
        },
        hub=hub, thread_ctx=ctx, slack_client=_FakeWebClient(), say=say,
    )
    assert relayed is False
    assert hub.calls == []


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
