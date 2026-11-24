"""issue #405: prompt に送信時刻 (server の timestamp そのまま) と経過時間を入れる.

Go 版 (bridge-claude2 / bridge-codex2 / bridge-tmux の ``TestFormatSentAtLine``)
と同じ入力・同じ期待値で、丸めと省き方の規則がずれていないことを確かめる。
"""

from __future__ import annotations

from datetime import UTC, datetime

import pytest
from agent_hub_sdk import IncomingMessage

from agent_hub_bridges._common.prompt import format_peer_message_prompt, format_sent_at_line

NOW = datetime(2026, 11, 24, 12, 0, 0, tzinfo=UTC)


@pytest.mark.parametrize(
    ("ts", "want"),
    [
        # empty は行ごと省く
        ("", ""),
        # 30 秒前は 1 分以内
        ("2026-11-24T11:59:30.000Z", "送信時刻: 2026-11-24T11:59:30.000Z (受信処理の 1 分以内)\n"),
        # ちょうど 1 分
        ("2026-11-24T11:59:00Z", "送信時刻: 2026-11-24T11:59:00Z (受信処理の 1 分前)\n"),
        # 59 分 59 秒は切り捨てで 59 分
        ("2026-11-24T11:00:01Z", "送信時刻: 2026-11-24T11:00:01Z (受信処理の 59 分前)\n"),
        # ちょうど 1 時間
        ("2026-11-24T11:00:00Z", "送信時刻: 2026-11-24T11:00:00Z (受信処理の 1 時間前)\n"),
        # 23 時間 59 分は 23 時間
        ("2026-11-23T12:01:00Z", "送信時刻: 2026-11-23T12:01:00Z (受信処理の 23 時間前)\n"),
        # ちょうど 1 日
        ("2026-11-23T12:00:00Z", "送信時刻: 2026-11-23T12:00:00Z (受信処理の 1 日前)\n"),
        # 2 か月前 (agent-hub#542 の @*)
        ("2026-09-19T21:33:18.123Z", "送信時刻: 2026-09-19T21:33:18.123Z (受信処理の 65 日前)\n"),
        # 30 秒先 (時計のずれ) は 1 分以内
        ("2026-11-24T12:00:30Z", "送信時刻: 2026-11-24T12:00:30Z (受信処理の 1 分以内)\n"),
        # 1 時間先は経過時間を付けない
        ("2026-11-24T13:00:00Z", "送信時刻: 2026-11-24T13:00:00Z\n"),
        # タイムゾーンが無ければそのまま載せる
        ("2026-11-24 11:00:00", "送信時刻: 2026-11-24 11:00:00\n"),
        # 日時として読めなければそのまま載せる
        ("not-a-time", "送信時刻: not-a-time\n"),
    ],
)
def test_format_sent_at_line(ts: str, want: str) -> None:
    assert format_sent_at_line(ts, NOW) == want


def _msg(timestamp: str) -> IncomingMessage:
    return IncomingMessage(id="msg-1", sender="@alice", to="@*", body="hello", timestamp=timestamp)


def test_prompt_has_sent_at_between_to_and_body() -> None:
    out = format_peer_message_prompt(_msg("2026-11-21T09:00:00.000Z"), now=NOW)
    want = "宛先: @*\n送信時刻: 2026-11-21T09:00:00.000Z (受信処理の 3 日前)\n本文:\nhello\n"
    assert want in out


def test_prompt_without_timestamp_has_no_sent_at() -> None:
    out = format_peer_message_prompt(_msg(""), now=NOW)
    assert "送信時刻" not in out
    assert "宛先: @*\n本文:\nhello\n" in out


def test_prompt_default_now_is_current_utc() -> None:
    """now を渡さなければ現在の UTC を基準にする (実際の呼び出し元はこの形)."""
    out = format_peer_message_prompt(_msg("2020-01-01T00:00:00Z"))
    assert "送信時刻: 2020-01-01T00:00:00Z (受信処理の " in out
    assert " 日前)\n" in out
