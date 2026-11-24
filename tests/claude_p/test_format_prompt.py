"""issue #405: claude_p の prompt にも送信時刻と経過時間の行が入る.

claude_p の ``_format_prompt`` は共通の ``format_peer_message_prompt`` に
宛先の注意書きを足すだけなので、共通側の行がそのまま残ることを確かめる。
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta

from agent_hub_sdk import IncomingMessage

from agent_hub_bridges.claude_p.worker import _format_prompt


def test_format_prompt_has_sent_at_line() -> None:
    sent = (datetime.now(UTC) - timedelta(days=2, hours=1)).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    msg = IncomingMessage(id="msg-1", sender="@alice", to="@*", body="hello", timestamp=sent)
    out = _format_prompt("@bridge-claude-p", msg)
    assert f"宛先: @*\n送信時刻: {sent} (受信処理の 2 日前)\n本文:\nhello\n" in out
    assert out.endswith("送信者個人へ DM で返すこと。")
