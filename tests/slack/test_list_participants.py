"""issue #4 unit test: Slack `@bot list` → `get_participants` → thread post.

`_handle_list_participants` の分岐パターンを fake で網羅する。実 Slack / 実
HubClient には接続しない (`test_thread_follow_up.py` と同パターン)。

`build_slack_app` 内の Bolt handler は `_handle_list_participants` の薄い
wrapper なので、ここを通せば handler 動作も担保される。
"""

from __future__ import annotations

import logging

import pytest
from agent_hub_sdk import HubTransientError, Participant

from agent_hub_bridges.slack.slack_handler import _handle_list_participants


class _FakeHub:
    """HubClient.get_participants のみ stub する (issue #4 で使う path)."""

    def __init__(
        self,
        participants: list[Participant] | None = None,
        fail_with: Exception | None = None,
    ) -> None:
        self.calls = 0
        self.participants = participants or []
        self.fail_with = fail_with

    async def get_participants(self) -> list[Participant]:
        self.calls += 1
        if self.fail_with is not None:
            raise self.fail_with
        return self.participants


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


_LOGGER = logging.getLogger("test-list-participants")


# ----- 正常系 -------------------------------------------------------------


@pytest.mark.asyncio
async def test_lists_online_participants_to_thread() -> None:
    """online な person だけが Slack thread に出力される (issue #4 期待動作)."""
    hub = _FakeHub(participants=[
        Participant(
            name="@planner",
            display_name="planner",
            mode="stateful",
            is_online=True,
        ),
        Participant(
            name="@offline-peer",
            display_name="offline-peer",
            mode="stateful",
            is_online=False,
        ),
        Participant(
            name="@reviewer",
            display_name="reviewer",
            mode="stateful",
            is_online=True,
        ),
    ])
    say_calls, say = _make_say()

    await _handle_list_participants(
        hub=hub,  # type: ignore[arg-type]
        say=say,
        thread_ts="t1",
        logger=_LOGGER,
    )

    assert hub.calls == 1
    assert len(say_calls) == 1
    body = say_calls[0]["text"]
    assert say_calls[0]["thread_ts"] == "t1"
    # online な peer は両方含まれる、offline は含まれない
    assert "@planner" in body
    assert "@reviewer" in body
    assert "@offline-peer" not in body
    # header 行も含まれる
    assert body.startswith("現在の参加者:")


@pytest.mark.asyncio
async def test_empty_participants_posts_no_one_message() -> None:
    """agent-hub が空でも post は止めない (空 string は Slack で post 不可)."""
    hub = _FakeHub(participants=[])
    say_calls, say = _make_say()

    await _handle_list_participants(
        hub=hub,  # type: ignore[arg-type]
        say=say,
        thread_ts="t1",
        logger=_LOGGER,
    )

    assert say_calls == [
        {"text": "現在 online な参加者は居ません。", "thread_ts": "t1"}
    ]


@pytest.mark.asyncio
async def test_no_thread_ts_posts_to_channel_root() -> None:
    """thread_ts が None なら channel root に post される (say の default 動作)."""
    hub = _FakeHub(participants=[
        Participant(
            name="@gemma",
            display_name="gemma",
            mode="stateful",
            is_online=True,
        ),
    ])
    say_calls, say = _make_say()

    await _handle_list_participants(
        hub=hub,  # type: ignore[arg-type]
        say=say,
        thread_ts=None,
        logger=_LOGGER,
    )

    assert len(say_calls) == 1
    assert say_calls[0]["thread_ts"] is None


# ----- エラー系 -----------------------------------------------------------


@pytest.mark.asyncio
async def test_transient_error_posts_warning_no_retry() -> None:
    """HubTransientError は thread に warning を出して終了 (= retry 無し)."""
    hub = _FakeHub(fail_with=HubTransientError("503 Service Unavailable"))
    say_calls, say = _make_say()

    await _handle_list_participants(
        hub=hub,  # type: ignore[arg-type]
        say=say,
        thread_ts="t1",
        logger=_LOGGER,
    )

    # 1 回だけ呼ばれて (= 自動 retry しない)、Slack に warning を 1 通 post
    assert hub.calls == 1
    assert len(say_calls) == 1
    assert say_calls[0]["thread_ts"] == "t1"
    text = say_calls[0]["text"]
    assert ":hourglass:" in text
    # ユーザにもう一度 `@bot list` を促す hint がある
    assert "list" in text


@pytest.mark.asyncio
async def test_unknown_error_posts_generic_warning() -> None:
    """想定外 exception でも silent fail しない (DESIGN.md §5.3 方針)."""
    hub = _FakeHub(fail_with=RuntimeError("schema mismatch"))
    say_calls, say = _make_say()

    await _handle_list_participants(
        hub=hub,  # type: ignore[arg-type]
        say=say,
        thread_ts="t1",
        logger=_LOGGER,
    )

    assert len(say_calls) == 1
    text = say_calls[0]["text"]
    assert ":warning:" in text
    # raw error を debug 用に含める
    assert "schema mismatch" in text


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
