"""Unit tests for bridge-claude OTLP telemetry (issue #90, #92, #109).

opentelemetry は mock して、span 属性・skip guard・init ロジック・
parent_span_id / span_id 注入 (issue #92)・tool_use child span (issue #109) を確認する。
"""

from __future__ import annotations

import os
from unittest.mock import MagicMock, patch

import pytest

from agent_hub_bridges.claude import telemetry


def _make_result(
    *,
    is_error: bool = False,
    input_tokens: int = 100,
    output_tokens: int = 50,
    cache_read: int = 20,
    usage_none: bool = False,
    total_cost_usd: float | None = None,
) -> MagicMock:
    result = MagicMock()
    result.is_error = is_error
    result.total_cost_usd = total_cost_usd
    if usage_none:
        result.usage = None
    else:
        result.usage = {
            "input_tokens": input_tokens,
            "output_tokens": output_tokens,
            "cache_read_input_tokens": cache_read,
        }
    return result


# ---------- UUID 変換ユーティリティ (issue #92) ----------


class TestUUIDConversion:
    """_uuid_to_span_id_int / _uuid_to_trace_id_int の変換ロジックを確認する。"""

    def test_span_id_uses_first_16_hex_chars(self) -> None:
        """先頭 16 hex 文字 (高位 64bit) が span_id として使われる。"""
        uuid = "550e8400-e29b-41d4-a716-446655440000"
        result = telemetry._uuid_to_span_id_int(uuid)
        assert result == int("550e8400e29b41d4", 16)

    def test_span_id_no_dashes(self) -> None:
        """ハイフンなし UUID も正しく変換される。"""
        result = telemetry._uuid_to_span_id_int("550e8400e29b41d4a716446655440000")
        assert result == int("550e8400e29b41d4", 16)

    def test_trace_id_uses_full_128bit(self) -> None:
        """全 32 hex 文字 (128bit) が trace_id として使われる。"""
        uuid = "550e8400-e29b-41d4-a716-446655440000"
        result = telemetry._uuid_to_trace_id_int(uuid)
        assert result == int("550e8400e29b41d4a716446655440000", 16)

    def test_span_id_invalid_raises_value_error(self) -> None:
        with pytest.raises(ValueError, match="Invalid UUID"):
            telemetry._uuid_to_span_id_int("not-a-uuid")

    def test_trace_id_invalid_raises_value_error(self) -> None:
        with pytest.raises(ValueError, match="Invalid UUID"):
            telemetry._uuid_to_trace_id_int("short")

    def test_span_id_returns_nonzero_for_typical_uuid(self) -> None:
        """典型的な UUID で 0 以外の整数が返る。"""
        result = telemetry._uuid_to_span_id_int(
            "cafebabe-dead-beef-1234-567890abcdef"
        )
        assert result != 0
        assert isinstance(result, int)

    def test_trace_id_returns_nonzero_for_typical_uuid(self) -> None:
        result = telemetry._uuid_to_trace_id_int(
            "cafebabe-dead-beef-1234-567890abcdef"
        )
        assert result != 0
        assert isinstance(result, int)


# ---------- _FixedNextSpanIdGenerator (issue #92) ----------


class TestFixedNextSpanIdGenerator:
    """_FixedNextSpanIdGenerator の one-shot 挙動を確認する。"""

    def test_returns_fixed_span_id_once(self) -> None:
        gen = telemetry._FixedNextSpanIdGenerator()
        gen.set_next_span_id(0xCAFEBABECAFEBABE)
        first = gen.generate_span_id()
        assert first == 0xCAFEBABECAFEBABE

    def test_falls_back_to_random_after_one_shot(self) -> None:
        gen = telemetry._FixedNextSpanIdGenerator()
        gen.set_next_span_id(0xCAFEBABECAFEBABE)
        gen.generate_span_id()  # consumes the fixed value
        second = gen.generate_span_id()
        # random; practically never equals the fixed value
        assert second != 0xCAFEBABECAFEBABE

    def test_random_when_not_set(self) -> None:
        gen = telemetry._FixedNextSpanIdGenerator()
        sid = gen.generate_span_id()
        assert isinstance(sid, int)
        assert sid != 0

    def test_generate_trace_id_returns_nonzero_int(self) -> None:
        gen = telemetry._FixedNextSpanIdGenerator()
        tid = gen.generate_trace_id()
        assert isinstance(tid, int)
        assert tid != 0

    def test_set_next_span_id_is_one_shot(self) -> None:
        """set_next_span_id → generate → generate は 2 回目にランダムになる。"""
        gen = telemetry._FixedNextSpanIdGenerator()
        fixed = 0x0000000000000001
        gen.set_next_span_id(fixed)
        assert gen.generate_span_id() == fixed
        # _next_span_id は None にリセットされている
        assert gen._next_span_id is None


# ---------- サイレント skip (URL 未設定) ----------


class TestNoopWhenUrlNotSet:
    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def test_emit_span_is_noop_without_url(self, monkeypatch) -> None:
        """AGENT_HUB_TELEMETRY_URL 未設定時は何も起きない。"""
        monkeypatch.delenv("AGENT_HUB_TELEMETRY_URL", raising=False)
        result = _make_result()
        # 例外が出なければ OK
        telemetry.emit_span(
            caused_by_id="test-id", model="claude-sonnet-4-6", result=result
        )

    def test_get_tracer_returns_none_without_url(self, monkeypatch) -> None:
        monkeypatch.delenv("AGENT_HUB_TELEMETRY_URL", raising=False)
        assert telemetry._get_tracer() is None

    def test_get_tracer_cached_after_first_call(self, monkeypatch) -> None:
        """_TRACER_INIT フラグで 2 回目以降はキャッシュを返す。"""
        monkeypatch.delenv("AGENT_HUB_TELEMETRY_URL", raising=False)
        first = telemetry._get_tracer()
        second = telemetry._get_tracer()
        assert first is second is None


# ---------- opentelemetry 未インストール時 ----------


class TestNoopOnImportError:
    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def test_emit_span_graceful_on_import_error(self, monkeypatch) -> None:
        """opentelemetry が未インストールでも例外を上げない。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        with patch.dict("sys.modules", {
            "opentelemetry": None,
            "opentelemetry.exporter": None,
            "opentelemetry.exporter.otlp": None,
            "opentelemetry.exporter.otlp.proto": None,
            "opentelemetry.exporter.otlp.proto.http": None,
            "opentelemetry.exporter.otlp.proto.http.trace_exporter": None,
            "opentelemetry.sdk": None,
            "opentelemetry.sdk.trace": None,
            "opentelemetry.sdk.trace.export": None,
        }):
            result = _make_result()
            telemetry.emit_span(
                caused_by_id="test-id", model="claude-sonnet-4-6", result=result
            )
        # 例外なければ OK


# ---------- span 属性の確認 ----------


class TestSpanAttributes:
    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def _inject_mock_tracer(self, monkeypatch) -> MagicMock:
        """mock Tracer を telemetry module に注入して span を返す helper。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")

        mock_span = MagicMock()
        mock_ctx = MagicMock()
        mock_ctx.__enter__ = MagicMock(return_value=mock_span)
        mock_ctx.__exit__ = MagicMock(return_value=False)

        mock_tracer = MagicMock()
        mock_tracer.start_as_current_span.return_value = mock_ctx

        telemetry._tracer = mock_tracer
        telemetry._TRACER_INIT = True
        return mock_span

    def test_span_sets_message_id(self, monkeypatch) -> None:
        """issue #109: 属性名は 'message.id' (旧 'msg_id' から変更)。"""
        span = self._inject_mock_tracer(monkeypatch)
        caused_by = "550e8400-e29b-41d4-a716-446655440000"
        telemetry.emit_span(
            caused_by_id=caused_by,
            model="claude-sonnet-4-6",
            result=_make_result(),
        )
        span.set_attribute.assert_any_call("message.id", caused_by)

    def test_span_sets_model(self, monkeypatch) -> None:
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="o3",
            result=_make_result(),
        )
        span.set_attribute.assert_any_call("gen_ai.request.model", "o3")

    def test_span_sets_input_tokens(self, monkeypatch) -> None:
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(input_tokens=200),
        )
        span.set_attribute.assert_any_call("gen_ai.usage.input_tokens", 200)

    def test_span_sets_output_tokens(self, monkeypatch) -> None:
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(output_tokens=75),
        )
        span.set_attribute.assert_any_call("gen_ai.usage.output_tokens", 75)

    def test_span_sets_cache_read_tokens_dot_separated(self, monkeypatch) -> None:
        """issue #90: cache_read.input_tokens はドット区切り (アンダースコア不可)。"""
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(cache_read=30),
        )
        span.set_attribute.assert_any_call(
            "gen_ai.usage.cache_read.input_tokens", 30
        )

    def test_span_status_ok_on_success(self, monkeypatch) -> None:
        from opentelemetry.trace import StatusCode

        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(is_error=False),
        )
        span.set_status.assert_called_once_with(StatusCode.OK)

    def test_span_status_error_on_is_error(self, monkeypatch) -> None:
        from opentelemetry.trace import StatusCode

        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(is_error=True),
        )
        span.set_status.assert_called_once_with(StatusCode.ERROR)

    def test_span_usage_none_uses_zero(self, monkeypatch) -> None:
        """result.usage = None でも 0 として処理される。"""
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(usage_none=True),
        )
        span.set_attribute.assert_any_call("gen_ai.usage.input_tokens", 0)
        span.set_attribute.assert_any_call("gen_ai.usage.output_tokens", 0)
        span.set_attribute.assert_any_call(
            "gen_ai.usage.cache_read.input_tokens", 0
        )

    def test_span_name_is_bridge_claude_send_message(self, monkeypatch) -> None:
        """span 名が 'bridge_claude.send_message' であること。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        mock_tracer = MagicMock()
        telemetry._tracer = mock_tracer
        telemetry._TRACER_INIT = True

        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(),
        )
        assert (
            mock_tracer.start_as_current_span.call_args.args[0]
            == "bridge_claude.send_message"
        )

    def test_emit_span_swallows_exception_from_span_op(self, monkeypatch) -> None:
        """span 操作で例外が出ても bridge は停止しない。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        mock_tracer = MagicMock()
        mock_tracer.start_as_current_span.side_effect = RuntimeError("span error")
        telemetry._tracer = mock_tracer
        telemetry._TRACER_INIT = True

        # 例外が上がらなければ OK
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(),
        )


# ---------- parent_span_id / span_id 注入 (issue #92) ----------


class TestSpanContextInjection:
    """caused_by_id → parent_span_id、sent_msg_id → span_id の注入を確認する。"""

    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def _inject_mock_tracer(self, monkeypatch) -> MagicMock:
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        mock_span = MagicMock()
        mock_ctx = MagicMock()
        mock_ctx.__enter__ = MagicMock(return_value=mock_span)
        mock_ctx.__exit__ = MagicMock(return_value=False)
        mock_tracer = MagicMock()
        mock_tracer.start_as_current_span.return_value = mock_ctx
        telemetry._tracer = mock_tracer
        telemetry._TRACER_INIT = True
        return mock_tracer

    def test_start_as_current_span_called_with_context(self, monkeypatch) -> None:
        """caused_by_id が提供されると start_as_current_span に context= が渡る。"""
        mock_tracer = self._inject_mock_tracer(monkeypatch)

        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
        )

        call_kwargs = mock_tracer.start_as_current_span.call_args.kwargs
        assert "context" in call_kwargs

    def test_parent_context_span_id_matches_caused_by_id(self, monkeypatch) -> None:
        """context 内の parent span_id が caused_by_id の UUID 変換値と一致する。"""
        mock_tracer = self._inject_mock_tracer(monkeypatch)

        caused_by_id = "550e8400-e29b-41d4-a716-446655440000"
        telemetry.emit_span(
            caused_by_id=caused_by_id, model="m", result=_make_result()
        )

        ctx = mock_tracer.start_as_current_span.call_args.kwargs["context"]

        from opentelemetry.trace import get_current_span

        parent_span = get_current_span(ctx)
        parent_sc = parent_span.get_span_context()
        expected_span_id = telemetry._uuid_to_span_id_int(caused_by_id)
        assert parent_sc.span_id == expected_span_id

    def test_parent_context_trace_id_matches_caused_by_id(self, monkeypatch) -> None:
        """context 内の trace_id が caused_by_id の UUID 変換値 (128bit) と一致する。"""
        mock_tracer = self._inject_mock_tracer(monkeypatch)

        caused_by_id = "550e8400-e29b-41d4-a716-446655440000"
        telemetry.emit_span(
            caused_by_id=caused_by_id, model="m", result=_make_result()
        )

        ctx = mock_tracer.start_as_current_span.call_args.kwargs["context"]

        from opentelemetry.trace import get_current_span

        parent_span = get_current_span(ctx)
        parent_sc = parent_span.get_span_context()
        expected_trace_id = telemetry._uuid_to_trace_id_int(caused_by_id)
        assert parent_sc.trace_id == expected_trace_id

    def test_sent_msg_id_calls_set_next_span_id(self, monkeypatch) -> None:
        """sent_msg_id が _id_generator.set_next_span_id に渡される。"""
        self._inject_mock_tracer(monkeypatch)

        mock_id_gen = MagicMock()
        telemetry._id_generator = mock_id_gen

        sent_msg_id = "cafebabe-dead-beef-0000-000000000001"
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            sent_msg_id=sent_msg_id,
            model="m",
            result=_make_result(),
        )

        expected = telemetry._uuid_to_span_id_int(sent_msg_id)
        mock_id_gen.set_next_span_id.assert_called_once_with(expected)

    def test_sent_msg_id_none_does_not_call_set_next_span_id(
        self, monkeypatch
    ) -> None:
        """sent_msg_id=None のとき set_next_span_id は呼ばれない。"""
        self._inject_mock_tracer(monkeypatch)

        mock_id_gen = MagicMock()
        telemetry._id_generator = mock_id_gen

        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            sent_msg_id=None,
            model="m",
            result=_make_result(),
        )

        mock_id_gen.set_next_span_id.assert_not_called()

    def test_id_generator_none_does_not_raise_when_sent_msg_id_set(
        self, monkeypatch
    ) -> None:
        """_id_generator が None でも sent_msg_id が渡されても例外を上げない。"""
        self._inject_mock_tracer(monkeypatch)
        telemetry._id_generator = None  # explicitly None

        # should not raise
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            sent_msg_id="cafebabe-dead-beef-0000-000000000001",
            model="m",
            result=_make_result(),
        )

    def test_full_round_trip_with_real_otel(self, monkeypatch) -> None:
        """実際の OTel TracerProvider で parent_span_id / span_id が正しく設定される。

        mock を使わず実 OTel SDK を呼び出す integration test。
        BatchSpanProcessor への export は行わない (exporter を inject しない)。
        """
        from opentelemetry.sdk.trace import TracerProvider

        id_gen = telemetry._FixedNextSpanIdGenerator()
        provider = TracerProvider(id_generator=id_gen)
        real_tracer = provider.get_tracer("test")

        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        telemetry._tracer = real_tracer
        telemetry._TRACER_INIT = True
        telemetry._id_generator = id_gen

        caused_by_id = "550e8400-e29b-41d4-a716-446655440000"
        sent_msg_id = "cafebabe-dead-beef-1234-567890abcdef"

        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
            InMemorySpanExporter,
        )

        exporter = InMemorySpanExporter()
        provider.add_span_processor(SimpleSpanProcessor(exporter))

        telemetry.emit_span(
            caused_by_id=caused_by_id,
            sent_msg_id=sent_msg_id,
            model="claude-sonnet-4-6",
            result=_make_result(),
        )

        spans = exporter.get_finished_spans()
        assert len(spans) == 1
        span = spans[0]

        expected_span_id = telemetry._uuid_to_span_id_int(sent_msg_id)
        expected_parent_span_id = telemetry._uuid_to_span_id_int(caused_by_id)
        expected_trace_id = telemetry._uuid_to_trace_id_int(caused_by_id)

        assert span.context.span_id == expected_span_id, (
            f"span_id mismatch: got {hex(span.context.span_id)}, "
            f"expected {hex(expected_span_id)}"
        )
        assert span.parent.span_id == expected_parent_span_id, (
            f"parent_span_id mismatch: got {hex(span.parent.span_id)}, "
            f"expected {hex(expected_parent_span_id)}"
        )
        assert span.context.trace_id == expected_trace_id, (
            f"trace_id mismatch: got {hex(span.context.trace_id)}, "
            f"expected {hex(expected_trace_id)}"
        )
        # issue #109: span 属性は "message.id" (旧 "msg_id" から変更)
        assert span.attributes.get("message.id") == caused_by_id


# ---------- gen_ai.usage.cost_usd (issue #109) ----------


class TestCostUsd:
    """issue #109: ResultMessage.total_cost_usd が gen_ai.usage.cost_usd 属性に記録される。"""

    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def _inject_mock_tracer(self, monkeypatch) -> MagicMock:
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        mock_span = MagicMock()
        mock_ctx = MagicMock()
        mock_ctx.__enter__ = MagicMock(return_value=mock_span)
        mock_ctx.__exit__ = MagicMock(return_value=False)
        mock_tracer = MagicMock()
        mock_tracer.start_as_current_span.return_value = mock_ctx
        telemetry._tracer = mock_tracer
        telemetry._TRACER_INIT = True
        return mock_span

    def test_cost_usd_set_when_available(self, monkeypatch) -> None:
        """total_cost_usd が設定されているとき gen_ai.usage.cost_usd 属性が記録される。"""
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(total_cost_usd=0.0025),
        )
        span.set_attribute.assert_any_call("gen_ai.usage.cost_usd", 0.0025)

    def test_cost_usd_not_set_when_none(self, monkeypatch) -> None:
        """total_cost_usd が None のとき gen_ai.usage.cost_usd 属性は記録されない。"""
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            caused_by_id="00000000-0000-0000-0000-000000000001",
            model="m",
            result=_make_result(total_cost_usd=None),
        )
        all_calls = [c.args[0] for c in span.set_attribute.call_args_list]
        assert "gen_ai.usage.cost_usd" not in all_calls


# ---------- tool_use child spans (issue #109) ----------


class TestToolUseSpans:
    """issue #109: tool_uses が渡されると child span が emit されることを確認する。"""

    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def _setup_real_otel(self, monkeypatch):
        """InMemorySpanExporter を使った実 OTel provider をセットアップする。"""
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import (
            InMemorySpanExporter,
        )

        id_gen = telemetry._FixedNextSpanIdGenerator()
        provider = TracerProvider(id_generator=id_gen)
        exporter = InMemorySpanExporter()
        provider.add_span_processor(SimpleSpanProcessor(exporter))
        real_tracer = provider.get_tracer("test")

        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        telemetry._tracer = real_tracer
        telemetry._TRACER_INIT = True
        telemetry._id_generator = id_gen

        return exporter

    def test_no_tool_uses_emits_one_root_span(self, monkeypatch) -> None:
        """tool_uses=None のとき root span のみ emit される。"""
        exporter = self._setup_real_otel(monkeypatch)
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
            tool_uses=None,
        )
        spans = exporter.get_finished_spans()
        assert len(spans) == 1
        assert spans[0].name == "bridge_claude.send_message"

    def test_tool_uses_emits_child_spans(self, monkeypatch) -> None:
        """tool_uses が 2 件あると root + 2 child = 3 spans が emit される。"""
        exporter = self._setup_real_otel(monkeypatch)
        now = 1_000_000_000
        tool_uses: list[telemetry.ToolUseRecord] = [
            {
                "name": "Bash",
                "input": {"command": "ls"},
                "start_time_ns": now,
                "end_time_ns": now + 500_000_000,
                "is_error": False,
            },
            {
                "name": "Read",
                "input": {"file_path": "/tmp/x"},
                "start_time_ns": now + 600_000_000,
                "end_time_ns": now + 700_000_000,
                "is_error": False,
            },
        ]
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
            tool_uses=tool_uses,
        )
        spans = exporter.get_finished_spans()
        assert len(spans) == 3
        span_names = {s.name for s in spans}
        assert span_names == {"bridge_claude.send_message", "tool.Bash", "tool.Read"}

    def test_child_span_name_prefix(self, monkeypatch) -> None:
        """child span の名前は 'tool.<tool_name>' 形式であること。"""
        exporter = self._setup_real_otel(monkeypatch)
        now = 1_000_000_000
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
            tool_uses=[
                {
                    "name": "Write",
                    "input": {"file_path": "/tmp/f", "content": "hello"},
                    "start_time_ns": now,
                    "end_time_ns": now + 100_000_000,
                    "is_error": False,
                }
            ],
        )
        spans = exporter.get_finished_spans()
        child_spans = [s for s in spans if s.name != "bridge_claude.send_message"]
        assert len(child_spans) == 1
        assert child_spans[0].name == "tool.Write"

    def test_child_span_has_message_id_attribute(self, monkeypatch) -> None:
        """child span に message.id 属性が記録される。"""
        exporter = self._setup_real_otel(monkeypatch)
        caused_by_id = "550e8400-e29b-41d4-a716-446655440000"
        now = 1_000_000_000
        telemetry.emit_span(
            caused_by_id=caused_by_id,
            model="m",
            result=_make_result(),
            tool_uses=[
                {
                    "name": "Bash",
                    "input": {"command": "echo hi"},
                    "start_time_ns": now,
                    "end_time_ns": now + 200_000_000,
                    "is_error": False,
                }
            ],
        )
        spans = exporter.get_finished_spans()
        child = next(s for s in spans if s.name == "tool.Bash")
        assert child.attributes.get("message.id") == caused_by_id

    def test_child_span_has_tool_name_attribute(self, monkeypatch) -> None:
        """child span に tool.name 属性が記録される。"""
        exporter = self._setup_real_otel(monkeypatch)
        now = 1_000_000_000
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
            tool_uses=[
                {
                    "name": "Read",
                    "input": {"file_path": "/x"},
                    "start_time_ns": now,
                    "end_time_ns": now + 50_000_000,
                    "is_error": False,
                }
            ],
        )
        spans = exporter.get_finished_spans()
        child = next(s for s in spans if s.name == "tool.Read")
        assert child.attributes.get("tool.name") == "Read"

    def test_child_span_has_duration_ms_attribute(self, monkeypatch) -> None:
        """child span に duration_ms 属性が記録される。"""
        exporter = self._setup_real_otel(monkeypatch)
        start_ns = 1_000_000_000
        end_ns = start_ns + 250_000_000  # 250ms
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
            tool_uses=[
                {
                    "name": "Bash",
                    "input": {"command": "sleep 0"},
                    "start_time_ns": start_ns,
                    "end_time_ns": end_ns,
                    "is_error": False,
                }
            ],
        )
        spans = exporter.get_finished_spans()
        child = next(s for s in spans if s.name == "tool.Bash")
        assert child.attributes.get("duration_ms") == 250

    def test_child_span_tool_args_sanitized(self, monkeypatch) -> None:
        """child span に tool.args.<key> 属性が記録され、値が truncate される。"""
        exporter = self._setup_real_otel(monkeypatch)
        now = 1_000_000_000
        long_value = "x" * 300
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
            tool_uses=[
                {
                    "name": "Bash",
                    "input": {"command": long_value},
                    "start_time_ns": now,
                    "end_time_ns": now + 100_000_000,
                    "is_error": False,
                }
            ],
        )
        spans = exporter.get_finished_spans()
        child = next(s for s in spans if s.name == "tool.Bash")
        cmd_val = child.attributes.get("tool.args.command")
        assert cmd_val is not None
        assert len(cmd_val) == 200  # truncated to _TOOL_ARG_MAX_LEN

    def test_child_span_error_status_when_is_error(self, monkeypatch) -> None:
        """is_error=True の child span に StatusCode.ERROR が設定される。"""
        from opentelemetry.trace import StatusCode

        exporter = self._setup_real_otel(monkeypatch)
        now = 1_000_000_000
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
            tool_uses=[
                {
                    "name": "Bash",
                    "input": {"command": "false"},
                    "start_time_ns": now,
                    "end_time_ns": now + 10_000_000,
                    "is_error": True,
                }
            ],
        )
        spans = exporter.get_finished_spans()
        child = next(s for s in spans if s.name == "tool.Bash")
        assert child.status.status_code == StatusCode.ERROR

    def test_child_span_is_child_of_root_span(self, monkeypatch) -> None:
        """child span の parent_span_id が root span の span_id と一致する。"""
        exporter = self._setup_real_otel(monkeypatch)
        now = 1_000_000_000
        sent_msg_id = "cafebabe-dead-beef-1234-567890abcdef"
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            sent_msg_id=sent_msg_id,
            model="m",
            result=_make_result(),
            tool_uses=[
                {
                    "name": "Read",
                    "input": {"file_path": "/x"},
                    "start_time_ns": now,
                    "end_time_ns": now + 100_000_000,
                    "is_error": False,
                }
            ],
        )
        spans = exporter.get_finished_spans()
        root = next(s for s in spans if s.name == "bridge_claude.send_message")
        child = next(s for s in spans if s.name == "tool.Read")
        # child の parent は root span
        assert child.parent is not None
        assert child.parent.span_id == root.context.span_id

    def test_child_span_same_trace_id_as_root(self, monkeypatch) -> None:
        """child span と root span は同じ trace_id を持つ。"""
        exporter = self._setup_real_otel(monkeypatch)
        now = 1_000_000_000
        caused_by_id = "550e8400-e29b-41d4-a716-446655440000"
        telemetry.emit_span(
            caused_by_id=caused_by_id,
            model="m",
            result=_make_result(),
            tool_uses=[
                {
                    "name": "Bash",
                    "input": {"command": "pwd"},
                    "start_time_ns": now,
                    "end_time_ns": now + 50_000_000,
                    "is_error": False,
                }
            ],
        )
        spans = exporter.get_finished_spans()
        root = next(s for s in spans if s.name == "bridge_claude.send_message")
        child = next(s for s in spans if s.name == "tool.Bash")
        assert child.context.trace_id == root.context.trace_id
        assert child.context.trace_id == telemetry._uuid_to_trace_id_int(caused_by_id)

    def test_empty_tool_uses_list_emits_only_root(self, monkeypatch) -> None:
        """tool_uses=[] (空リスト) のとき root span のみ emit される。"""
        exporter = self._setup_real_otel(monkeypatch)
        telemetry.emit_span(
            caused_by_id="550e8400-e29b-41d4-a716-446655440000",
            model="m",
            result=_make_result(),
            tool_uses=[],
        )
        spans = exporter.get_finished_spans()
        assert len(spans) == 1


# ---------- build_traceparent / make_subprocess_telemetry_env (issue #91) ----------


class TestBuildTraceparent:
    """build_traceparent() の W3C traceparent 文字列生成を確認する (issue #91)."""

    _UUID = "550e8400-e29b-41d4-a716-446655440000"

    def test_build_traceparent_format(self) -> None:
        """format は ``00-{32hex}-{16hex}-01`` であること。"""
        result = telemetry.build_traceparent(self._UUID)
        parts = result.split("-", 3)
        assert parts[0] == "00"
        # trace_id: 32 hex chars (128 bits)
        assert len(parts[1]) == 32
        assert all(c in "0123456789abcdef" for c in parts[1])
        # span_id: 16 hex chars (64 bits)
        assert len(parts[2]) == 16
        assert all(c in "0123456789abcdef" for c in parts[2])
        # flags
        assert parts[3] == "01"

    def test_build_traceparent_trace_id(self) -> None:
        """trace_id 32 hex = UUID の全 128bit。"""
        result = telemetry.build_traceparent(self._UUID)
        trace_id_hex = result.split("-")[1]
        expected = int("550e8400e29b41d4a716446655440000", 16)
        assert int(trace_id_hex, 16) == expected

    def test_build_traceparent_span_id(self) -> None:
        """span_id 16 hex = UUID の高位 64bit。"""
        result = telemetry.build_traceparent(self._UUID)
        span_id_hex = result.split("-")[2]
        expected = int("550e8400e29b41d4", 16)
        assert int(span_id_hex, 16) == expected

    def test_build_traceparent_sampled_flag(self) -> None:
        """flags フィールドは ``01`` (sampled) であること。"""
        result = telemetry.build_traceparent(self._UUID)
        assert result.endswith("-01")

    def test_build_traceparent_invalid_uuid(self) -> None:
        """不正な UUID 文字列は ValueError を raise する。"""
        with pytest.raises(ValueError):
            telemetry.build_traceparent("not-a-valid-uuid")


class TestMakeSubprocessTelemetryEnv:
    """make_subprocess_telemetry_env() の env dict 生成を確認する (issue #91)."""

    def test_make_subprocess_telemetry_env_keys(self) -> None:
        """4 つの必須キーが全て含まれること。"""
        env = telemetry.make_subprocess_telemetry_env("http://localhost:4318")
        assert "CLAUDE_CODE_ENABLE_TELEMETRY" in env
        assert "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA" in env
        assert "OTEL_TRACES_EXPORTER" in env
        assert "OTEL_EXPORTER_OTLP_ENDPOINT" in env

    def test_make_subprocess_telemetry_env_endpoint(self) -> None:
        """指定した URL が ``OTEL_EXPORTER_OTLP_ENDPOINT`` に設定されること。"""
        url = "http://localhost:4318"
        env = telemetry.make_subprocess_telemetry_env(url)
        assert env["OTEL_EXPORTER_OTLP_ENDPOINT"] == url

    def test_make_subprocess_telemetry_env_trailing_slash(self) -> None:
        """末尾の ``/`` が除去されること。"""
        env = telemetry.make_subprocess_telemetry_env("http://localhost:4318/")
        assert env["OTEL_EXPORTER_OTLP_ENDPOINT"] == "http://localhost:4318"


# ---------- env var 非汚染の確認 ----------


class TestNoEnvMutation:
    """reviewer Minor 対応 (#91): _get_tracer が os.environ を書き換えないことを確認する。

    opentelemetry-exporter-otlp-proto-http v1.42.1 は OTLPSpanExporter
    コンストラクタで OTEL_EXPORTER_OTLP_PROTOCOL を読まないため、
    env var を注入しても効果がない。本クラスはその非汚染を回帰テストとして保持する。
    """

    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def test_does_not_set_protocol_env(self, monkeypatch) -> None:
        """_get_tracer が OTEL_EXPORTER_OTLP_PROTOCOL を書き込まない。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        monkeypatch.delenv("OTEL_EXPORTER_OTLP_PROTOCOL", raising=False)

        with (
            patch("opentelemetry.sdk.trace.TracerProvider"),
            patch(
                "opentelemetry.exporter.otlp.proto.http.trace_exporter.OTLPSpanExporter"
            ),
            patch("opentelemetry.sdk.trace.export.BatchSpanProcessor"),
            patch("opentelemetry.trace.set_tracer_provider"),
            patch("opentelemetry.trace.get_tracer", return_value=MagicMock()),
        ):
            telemetry._get_tracer()

        # env var を書き換えていないこと
        assert "OTEL_EXPORTER_OTLP_PROTOCOL" not in os.environ

    def test_does_not_overwrite_user_protocol_env(self, monkeypatch) -> None:
        """ユーザーが OTEL_EXPORTER_OTLP_PROTOCOL を設定済みでも書き換えない。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        monkeypatch.setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

        with (
            patch("opentelemetry.sdk.trace.TracerProvider"),
            patch(
                "opentelemetry.exporter.otlp.proto.http.trace_exporter.OTLPSpanExporter"
            ),
            patch("opentelemetry.sdk.trace.export.BatchSpanProcessor"),
            patch("opentelemetry.trace.set_tracer_provider"),
            patch("opentelemetry.trace.get_tracer", return_value=MagicMock()),
        ):
            telemetry._get_tracer()

        # ユーザーの値が変わっていないこと
        assert os.environ.get("OTEL_EXPORTER_OTLP_PROTOCOL") == "http/protobuf"


# ---------- service.name 設定 (issue #96) ----------


class TestServiceNameConfig:
    """issue #96: configure() による service.name 設定と TracerProvider への反映を確認する。

    otelite ダッシュボードで ``Service: unknown_service`` になっていた問題の修正。
    ``configure(service_name=f"@{config.user}")`` を ``run_worker`` 先頭で呼ぶことで
    ``Resource({"service.name": "@<handle>"})`` が ``TracerProvider`` に渡される。
    """

    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def test_default_service_name_is_bridge_claude(self) -> None:
        """configure() 未呼び出し時の _SERVICE_NAME デフォルト値は 'bridge-claude'。"""
        assert telemetry._SERVICE_NAME == "bridge-claude"

    def test_configure_sets_service_name(self) -> None:
        """configure() で _SERVICE_NAME が指定値に更新される。"""
        telemetry.configure(service_name="@planner")
        assert telemetry._SERVICE_NAME == "@planner"

    def test_reset_for_testing_resets_service_name(self) -> None:
        """reset_for_testing() で _SERVICE_NAME がデフォルト値にリセットされる。"""
        telemetry.configure(service_name="@reviewer")
        telemetry.reset_for_testing()
        assert telemetry._SERVICE_NAME == "bridge-claude"

    def test_tracer_provider_receives_resource_with_configured_service_name(
        self, monkeypatch
    ) -> None:
        """configure() 後に _get_tracer() を呼ぶと TracerProvider に service.name が渡る。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        telemetry.configure(service_name="@bridge-test")

        with (
            patch("opentelemetry.sdk.trace.TracerProvider") as mock_provider_cls,
            patch(
                "opentelemetry.exporter.otlp.proto.http.trace_exporter.OTLPSpanExporter"
            ),
            patch("opentelemetry.sdk.trace.export.BatchSpanProcessor"),
            patch("opentelemetry.trace.set_tracer_provider"),
            patch("opentelemetry.trace.get_tracer", return_value=MagicMock()),
        ):
            telemetry._get_tracer()

        call_kwargs = mock_provider_cls.call_args.kwargs
        assert "resource" in call_kwargs
        resource = call_kwargs["resource"]
        assert resource.attributes.get("service.name") == "@bridge-test"

    def test_default_service_name_used_when_not_configured(
        self, monkeypatch
    ) -> None:
        """configure() 未呼び出し時は 'bridge-claude' が service.name として使われる。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        # configure() を呼ばない

        with (
            patch("opentelemetry.sdk.trace.TracerProvider") as mock_provider_cls,
            patch(
                "opentelemetry.exporter.otlp.proto.http.trace_exporter.OTLPSpanExporter"
            ),
            patch("opentelemetry.sdk.trace.export.BatchSpanProcessor"),
            patch("opentelemetry.trace.set_tracer_provider"),
            patch("opentelemetry.trace.get_tracer", return_value=MagicMock()),
        ):
            telemetry._get_tracer()

        call_kwargs = mock_provider_cls.call_args.kwargs
        resource = call_kwargs["resource"]
        assert resource.attributes.get("service.name") == "bridge-claude"


# ---------------------------------------------------------------------------
# TestSanitizeToolInput (issue #111, #112)
# ---------------------------------------------------------------------------


class TestSanitizeToolInput:
    """_sanitize_tool_input の masking ロジックを直接検証する (issue #111, #112)。"""

    def test_password_masked(self) -> None:
        """'password' キーは '***' に置換される。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({"password": "secret123", "cmd": "ls"})
        assert result["password"] == "***"
        assert result["cmd"] == "ls"

    def test_api_key_exact_masked(self) -> None:
        """'api_key' キーは '***' に置換される。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({"api_key": "sk-abc123"})
        assert result["api_key"] == "***"

    def test_key_exact_masked(self) -> None:
        """'key' キー(完全一致)は '***' に置換される。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({"key": "encryption-key-value"})
        assert result["key"] == "***"

    def test_pat_exact_masked(self) -> None:
        """'pat' キー(完全一致)は '***' に置換される。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({"pat": "ghp_xxxx"})
        assert result["pat"] == "***"

    def test_compound_token_masked(self) -> None:
        """'api_token' / 'access_token' 等のサブワード合成キーがマスクされる (issue #112)。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({
            "api_token": "tok-abc",
            "access_token": "tok-xyz",
        })
        assert result["api_token"] == "***"
        assert result["access_token"] == "***"

    def test_compound_secret_masked(self) -> None:
        """'client_secret' 等のサブワード合成キーがマスクされる (issue #112)。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({"client_secret": "cs-123"})
        assert result["client_secret"] == "***"

    def test_private_key_masked(self) -> None:
        """'private_key' はサブワード合成としてマスクされる (issue #112)。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({"private_key": "-----BEGIN RSA..."})
        assert result["private_key"] == "***"

    def test_case_insensitive_masking(self) -> None:
        """大文字・混在キーも大文字小文字を区別せずマスクされる。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({
            "PASSWORD": "secret",
            "Api_Key": "key123",
            "ACCESS_TOKEN": "tok",
        })
        assert result["PASSWORD"] == "***"
        assert result["Api_Key"] == "***"
        assert result["ACCESS_TOKEN"] == "***"

    def test_normal_fields_not_masked(self) -> None:
        """通常フィールドはマスクされず、値がそのまま返る。"""
        from agent_hub_bridges.claude.telemetry import _sanitize_tool_input

        result = _sanitize_tool_input({
            "command": "ls -la",
            "path": "/tmp/foo",
            "count": "42",
        })
        assert result["command"] == "ls -la"
        assert result["path"] == "/tmp/foo"
        assert result["count"] == "42"

    def test_all_sensitive_keywords_in_keys(self) -> None:
        """_SENSITIVE_KEYWORDS の全キーワードを含むキーがマスクされる。"""
        from agent_hub_bridges.claude.telemetry import (
            _SENSITIVE_KEYWORDS,
            _sanitize_tool_input,
        )

        # 各キーワードをキー名に含むエントリを生成して全てマスクされることを確認
        tool_input = {f"my_{kw}_field": "value" for kw in _SENSITIVE_KEYWORDS}
        result = _sanitize_tool_input(tool_input)
        for k in tool_input:
            assert result[k] == "***", f"{k!r} should be masked"
