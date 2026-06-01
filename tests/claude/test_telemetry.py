"""Unit tests for bridge-claude OTLP telemetry (issue #90, #92).

opentelemetry は mock して、span 属性・skip guard・init ロジック・
parent_span_id / span_id 注入 (issue #92) を確認する。
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
) -> MagicMock:
    result = MagicMock()
    result.is_error = is_error
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

    def test_span_sets_msg_id(self, monkeypatch) -> None:
        span = self._inject_mock_tracer(monkeypatch)
        caused_by = "550e8400-e29b-41d4-a716-446655440000"
        telemetry.emit_span(
            caused_by_id=caused_by,
            model="claude-sonnet-4-6",
            result=_make_result(),
        )
        span.set_attribute.assert_any_call("msg_id", caused_by)

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
