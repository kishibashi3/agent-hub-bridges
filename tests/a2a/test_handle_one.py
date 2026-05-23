"""Behavioral tests for _handle_one (issue #14 stream memory + partial-chunk).

カバーするケース:
  - streaming aggregation: list[StreamResponse] を保持せず chunk ごとに処理
  - 正常系: stream 完走 → hub.send に reply が届く
  - partial-chunk: stream 途中例外 + 部分テキストあり → partial + 中断注記を送信
  - エラーのみ (partial なし): 従来通りエラー通知のみ、hub.send はエラー文面
  - 自己 echo skip
"""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock

import pytest
from a2a.types.a2a_pb2 import Message, Part, StreamResponse

from agent_hub_bridges.a2a.worker import _handle_one

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


def _make_config(user: str = "a2a-bridge") -> MagicMock:
    cfg = MagicMock()
    cfg.user = user
    return cfg


def _make_msg(sender: str = "@alice", body: str = "hello") -> MagicMock:
    msg = MagicMock()
    msg.id = "msg-001"
    msg.sender = sender
    msg.body = body
    return msg


def _text_response(text: str) -> StreamResponse:
    return StreamResponse(message=Message(parts=[Part(text=text)]))


async def _async_gen(*responses: StreamResponse):
    """StreamResponse のリストを AsyncIterator として返す helper."""
    for r in responses:
        yield r


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


class TestHandleOneNormalStream:
    @pytest.mark.asyncio
    async def test_full_stream_sends_reply(self) -> None:
        """正常系: stream 完走 → hub.send にテキストが届く。"""
        hub = MagicMock()
        hub.send = AsyncMock()

        a2a_client = MagicMock()
        a2a_client.send_message = MagicMock(
            return_value=_async_gen(
                _text_response("chunk1"),
                _text_response("chunk2"),
            )
        )

        config = _make_config()
        msg = _make_msg()

        await _handle_one(hub, a2a_client, msg, config)

        hub.send.assert_awaited_once()
        sent_message = hub.send.call_args.kwargs["message"]
        assert "chunk1" in sent_message
        assert "chunk2" in sent_message

    @pytest.mark.asyncio
    async def test_empty_stream_does_not_send(self) -> None:
        """空 stream → hub.send は呼ばない。"""
        hub = MagicMock()
        hub.send = AsyncMock()

        a2a_client = MagicMock()
        a2a_client.send_message = MagicMock(return_value=_async_gen())

        await _handle_one(hub, a2a_client, _make_msg(), _make_config())

        hub.send.assert_not_awaited()

    @pytest.mark.asyncio
    async def test_self_echo_skipped(self) -> None:
        """自分自身からのメッセージは skip する。"""
        hub = MagicMock()
        hub.send = AsyncMock()
        a2a_client = MagicMock()

        config = _make_config(user="a2a-bridge")
        msg = _make_msg(sender="@a2a-bridge")

        await _handle_one(hub, a2a_client, msg, config)

        hub.send.assert_not_awaited()
        a2a_client.send_message.assert_not_called()


class TestHandleOneStreamingAggregation:
    @pytest.mark.asyncio
    async def test_multiple_chunks_joined(self) -> None:
        """複数 chunk が改行で結合されて hub.send される。"""
        hub = MagicMock()
        hub.send = AsyncMock()

        a2a_client = MagicMock()
        a2a_client.send_message = MagicMock(
            return_value=_async_gen(
                _text_response("alpha"),
                _text_response("beta"),
                _text_response("gamma"),
            )
        )

        await _handle_one(hub, a2a_client, _make_msg(), _make_config())

        sent = hub.send.call_args.kwargs["message"]
        assert sent == "alpha\nbeta\ngamma"

    @pytest.mark.asyncio
    async def test_no_list_accumulation_result_matches_aggregated_text(self) -> None:
        """streaming aggregation の結果が全 chunk text の連結と一致する。

        NOTE: 実装が list を使わないことを内部で直接 assert するのは困難なため、
        出力テキストが正しく組み立てられることで間接的に確認する。
        """
        hub = MagicMock()
        hub.send = AsyncMock()

        chunks = [f"chunk{i}" for i in range(10)]
        a2a_client = MagicMock()
        a2a_client.send_message = MagicMock(
            return_value=_async_gen(*[_text_response(c) for c in chunks])
        )

        await _handle_one(hub, a2a_client, _make_msg(), _make_config())

        sent = hub.send.call_args.kwargs["message"]
        assert sent == "\n".join(chunks)


class TestHandleOnePartialChunk:
    @pytest.mark.asyncio
    async def test_stream_interrupted_with_partial_sends_partial_plus_note(
        self,
    ) -> None:
        """stream 途中例外 + partial text あり → partial + 中断注記を hub.send。

        issue #14 item 2: partial を捨てずに送信する。
        """
        hub = MagicMock()
        hub.send = AsyncMock()

        async def _partial_stream():
            yield _text_response("first chunk")
            yield _text_response("second chunk")
            raise ConnectionError("remote closed")

        a2a_client = MagicMock()
        a2a_client.send_message = MagicMock(return_value=_partial_stream())

        await _handle_one(hub, a2a_client, _make_msg(), _make_config())

        hub.send.assert_awaited_once()
        sent = hub.send.call_args.kwargs["message"]
        assert "first chunk" in sent
        assert "second chunk" in sent
        assert "stream interrupted" in sent
        assert "ConnectionError" in sent
        # str(exc) は含まない (URL/認証情報漏出防止 Minor: PR #68 review)
        assert "remote closed" not in sent

    @pytest.mark.asyncio
    async def test_stream_error_no_partial_sends_error_only(self) -> None:
        """stream 即失敗 (partial なし) → エラー通知のみ。

        partial が 0 件の場合は従来通りの挙動を維持する (issue #14 item 2)。
        """
        hub = MagicMock()
        hub.send = AsyncMock()

        async def _immediate_error():
            raise RuntimeError("connection refused")
            yield  # pragma: no cover  — makes this an async generator

        a2a_client = MagicMock()
        a2a_client.send_message = MagicMock(return_value=_immediate_error())

        await _handle_one(hub, a2a_client, _make_msg(), _make_config())

        hub.send.assert_awaited_once()
        sent = hub.send.call_args.kwargs["message"]
        assert "RuntimeError" in sent
        # partial ではないのでエラー通知のみ (stream interrupted 注記は付かない)
        assert "stream interrupted" not in sent
        # str(exc) は含まない (URL/認証情報漏出防止 Minor: PR #68 review)
        assert "connection refused" not in sent

    @pytest.mark.asyncio
    async def test_stream_interrupted_only_non_text_sends_interruption_note(
        self,
    ) -> None:
        """stream 途中例外 + skipped のみ (text なし) → 中断注記を送信。"""
        hub = MagicMock()
        hub.send = AsyncMock()

        async def _non_text_then_error():
            yield StreamResponse(
                message=Message(parts=[Part(raw=b"binary")])
            )
            raise TimeoutError("timed out")

        a2a_client = MagicMock()
        a2a_client.send_message = MagicMock(return_value=_non_text_then_error())

        await _handle_one(hub, a2a_client, _make_msg(), _make_config())

        hub.send.assert_awaited_once()
        sent = hub.send.call_args.kwargs["message"]
        assert "stream interrupted" in sent
        assert "TimeoutError" in sent
