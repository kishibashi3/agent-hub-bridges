"""Unit tests for A2A ↔ agent-hub mapping helpers in `worker.py`.

a2a-sdk の Client / A2ACardResolver は外側で 1 度しか open しないので
本 test では mock しない。 代わりに mapping 系の純関数
(`_extract_reply_text` / `_build_send_message_request` /
`_derive_display_name`) を 直接 叩く。

protobuf message は `a2a.types.a2a_pb2` から 直接インスタンス化する
(= 実 SDK と同 path で 動作)。
"""

from __future__ import annotations

from a2a.types.a2a_pb2 import (
    ROLE_USER,
    AgentCard,
    Message,
    Part,
    StreamResponse,
)

from agent_hub_bridges.a2a.worker import (
    _assemble_reply,
    _build_send_message_request,
    _derive_display_name,
    _extract_reply_text,
    _extract_reply_text_from_response,
)

# ---------------------------------------------------------------------------
# _extract_reply_text
# ---------------------------------------------------------------------------


def _stream_with_text(*texts: str) -> list[StreamResponse]:
    """text part だけ持つ StreamResponse list を作る test helper."""
    out = []
    for t in texts:
        msg = Message(parts=[Part(text=t)])
        out.append(StreamResponse(message=msg))
    return out


def test_extract_reply_text_single_chunk() -> None:
    stream = _stream_with_text("hello")
    assert _extract_reply_text(stream) == "hello"


def test_extract_reply_text_multiple_chunks_joined_with_newline() -> None:
    stream = _stream_with_text("foo", "bar", "baz")
    assert _extract_reply_text(stream) == "foo\nbar\nbaz"


def test_extract_reply_text_empty_stream() -> None:
    assert _extract_reply_text([]) == ""


def test_extract_reply_text_no_message_field_skipped() -> None:
    """`StreamResponse` に message が無い event (= task / status_update /
    artifact_update のみ) は スキップされる."""
    stream = [StreamResponse()]  # 全フィールド unset
    assert _extract_reply_text(stream) == ""


def test_extract_reply_text_non_text_parts_get_omitted_with_note() -> None:
    """text を持たない part (raw / url / data) は スキップされ、
    末尾に omitted 件数の note が 付く."""
    msg = Message(
        parts=[
            Part(text="visible"),
            Part(raw=b"binary"),  # text 無し
        ]
    )
    stream = [StreamResponse(message=msg)]
    out = _extract_reply_text(stream)
    assert "visible" in out
    assert "non-text parts omitted: 1" in out


def test_extract_reply_text_all_non_text_only_note() -> None:
    msg = Message(parts=[Part(raw=b"a"), Part(url="b")])
    stream = [StreamResponse(message=msg)]
    out = _extract_reply_text(stream)
    # text が完全に無いケースでも note は残る
    assert "non-text parts omitted: 2" in out


# ---------------------------------------------------------------------------
# _extract_reply_text_from_response (per-chunk, issue #14 item 1)
# ---------------------------------------------------------------------------


def test_extract_from_response_text_part() -> None:
    msg = Message(parts=[Part(text="hello")])
    text, skipped = _extract_reply_text_from_response(StreamResponse(message=msg))
    assert text == "hello"
    assert skipped == 0


def test_extract_from_response_multiple_text_parts_joined() -> None:
    msg = Message(parts=[Part(text="foo"), Part(text="bar")])
    text, skipped = _extract_reply_text_from_response(StreamResponse(message=msg))
    assert text == "foo\nbar"
    assert skipped == 0


def test_extract_from_response_no_message_field() -> None:
    text, skipped = _extract_reply_text_from_response(StreamResponse())
    assert text == ""
    assert skipped == 0


def test_extract_from_response_mixed_parts() -> None:
    msg = Message(parts=[Part(text="visible"), Part(raw=b"binary")])
    text, skipped = _extract_reply_text_from_response(StreamResponse(message=msg))
    assert text == "visible"
    assert skipped == 1


def test_extract_from_response_all_non_text() -> None:
    msg = Message(parts=[Part(raw=b"a"), Part(url="b")])
    text, skipped = _extract_reply_text_from_response(StreamResponse(message=msg))
    assert text == ""
    assert skipped == 2


# ---------------------------------------------------------------------------
# _assemble_reply (issue #14 item 1 & 2)
# ---------------------------------------------------------------------------


def test_assemble_reply_text_only() -> None:
    assert _assemble_reply(["hello"], 0) == "hello"


def test_assemble_reply_multiple_parts_joined() -> None:
    assert _assemble_reply(["foo", "bar", "baz"], 0) == "foo\nbar\nbaz"


def test_assemble_reply_empty() -> None:
    assert _assemble_reply([], 0) == ""


def test_assemble_reply_skipped_with_text() -> None:
    out = _assemble_reply(["visible"], 1)
    assert "visible" in out
    assert "non-text parts omitted: 1" in out


def test_assemble_reply_skipped_only() -> None:
    out = _assemble_reply([], 2)
    assert "non-text parts omitted: 2" in out
    assert not out.startswith("\n")


def test_assemble_reply_interrupted_with_text() -> None:
    out = _assemble_reply(["partial"], 0, interrupted="ConnectionError: timeout")
    assert "partial" in out
    assert "stream interrupted: ConnectionError: timeout" in out


def test_assemble_reply_interrupted_no_text() -> None:
    out = _assemble_reply([], 0, interrupted="ConnectionError: timeout")
    assert "stream interrupted: ConnectionError: timeout" in out
    assert not out.startswith("\n")


def test_assemble_reply_interrupted_with_skipped() -> None:
    out = _assemble_reply(["partial"], 1, interrupted="ConnectionError: timeout")
    assert "partial" in out
    assert "non-text parts omitted: 1" in out
    assert "stream interrupted: ConnectionError: timeout" in out


# ---------------------------------------------------------------------------
# _build_send_message_request
# ---------------------------------------------------------------------------


def test_build_send_message_request_minimal() -> None:
    req = _build_send_message_request("hello")
    assert req.message.role == ROLE_USER
    assert len(req.message.parts) == 1
    assert req.message.parts[0].text == "hello"
    assert req.message.message_id  # 自動生成、 非空


def test_build_send_message_request_distinct_message_ids() -> None:
    """uuid 生成なので 連続呼出で 異なる id が 入る."""
    req1 = _build_send_message_request("a")
    req2 = _build_send_message_request("b")
    assert req1.message.message_id != req2.message.message_id


def test_build_send_message_request_empty_body() -> None:
    """body 空でも protobuf は 受理する (= server 側で reject される想定)."""
    req = _build_send_message_request("")
    assert req.message.parts[0].text == ""


# ---------------------------------------------------------------------------
# _derive_display_name
# ---------------------------------------------------------------------------


def test_derive_display_name_prefers_description() -> None:
    card = AgentCard(name="my-agent", description="My Awesome Agent")
    assert _derive_display_name(card, "fallback") == "My Awesome Agent"


def test_derive_display_name_falls_back_to_name_when_no_description() -> None:
    card = AgentCard(name="my-agent")
    assert _derive_display_name(card, "fallback") == "my-agent"


def test_derive_display_name_falls_back_to_fallback_when_card_empty() -> None:
    card = AgentCard()
    assert _derive_display_name(card, "the-fallback") == "the-fallback"
