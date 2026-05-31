"""Unit tests for bridge-claude OTLP telemetry (issue #90).

opentelemetry は mock して、span 属性・skip guard・init ロジックを確認する。
"""

from __future__ import annotations

import os
from unittest.mock import MagicMock, patch

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


# ---------- サイレント skip (URL 未設定) ----------


class TestNoopWhenUrlNotSet:
    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def test_emit_span_is_noop_without_url(self, monkeypatch) -> None:
        """AGENT_HUB_TELEMETRY_URL 未設定時は何も起きない。"""
        monkeypatch.delenv("AGENT_HUB_TELEMETRY_URL", raising=False)
        result = _make_result()
        # 例外が出なければ OK
        telemetry.emit_span(msg_id="test-id", model="claude-sonnet-4-6", result=result)

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
            telemetry.emit_span(msg_id="test-id", model="claude-sonnet-4-6", result=result)
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
        telemetry.emit_span(
            msg_id="abc-123", model="claude-sonnet-4-6", result=_make_result()
        )
        span.set_attribute.assert_any_call("msg_id", "abc-123")

    def test_span_sets_model(self, monkeypatch) -> None:
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(msg_id="x", model="o3", result=_make_result())
        span.set_attribute.assert_any_call("gen_ai.request.model", "o3")

    def test_span_sets_input_tokens(self, monkeypatch) -> None:
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            msg_id="x", model="m", result=_make_result(input_tokens=200)
        )
        span.set_attribute.assert_any_call("gen_ai.usage.input_tokens", 200)

    def test_span_sets_output_tokens(self, monkeypatch) -> None:
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            msg_id="x", model="m", result=_make_result(output_tokens=75)
        )
        span.set_attribute.assert_any_call("gen_ai.usage.output_tokens", 75)

    def test_span_sets_cache_read_tokens_dot_separated(self, monkeypatch) -> None:
        """issue #90: cache_read.input_tokens はドット区切り (アンダースコア不可)。"""
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            msg_id="x", model="m", result=_make_result(cache_read=30)
        )
        span.set_attribute.assert_any_call("gen_ai.usage.cache_read.input_tokens", 30)

    def test_span_status_ok_on_success(self, monkeypatch) -> None:
        from opentelemetry.trace import StatusCode

        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            msg_id="x", model="m", result=_make_result(is_error=False)
        )
        span.set_status.assert_called_once_with(StatusCode.OK)

    def test_span_status_error_on_is_error(self, monkeypatch) -> None:
        from opentelemetry.trace import StatusCode

        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            msg_id="x", model="m", result=_make_result(is_error=True)
        )
        span.set_status.assert_called_once_with(StatusCode.ERROR)

    def test_span_usage_none_uses_zero(self, monkeypatch) -> None:
        """result.usage = None でも 0 として処理される。"""
        span = self._inject_mock_tracer(monkeypatch)
        telemetry.emit_span(
            msg_id="x", model="m", result=_make_result(usage_none=True)
        )
        span.set_attribute.assert_any_call("gen_ai.usage.input_tokens", 0)
        span.set_attribute.assert_any_call("gen_ai.usage.output_tokens", 0)
        span.set_attribute.assert_any_call("gen_ai.usage.cache_read.input_tokens", 0)

    def test_span_name_is_bridge_claude_send_message(self, monkeypatch) -> None:
        """span 名が 'bridge_claude.send_message' であること。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        mock_tracer = MagicMock()
        telemetry._tracer = mock_tracer
        telemetry._TRACER_INIT = True

        telemetry.emit_span(msg_id="x", model="m", result=_make_result())
        mock_tracer.start_as_current_span.assert_called_once_with(
            "bridge_claude.send_message"
        )

    def test_emit_span_swallows_exception_from_span_op(self, monkeypatch) -> None:
        """span 操作で例外が出ても bridge は停止しない。"""
        monkeypatch.setenv("AGENT_HUB_TELEMETRY_URL", "http://localhost:4318")
        mock_tracer = MagicMock()
        mock_tracer.start_as_current_span.side_effect = RuntimeError("span error")
        telemetry._tracer = mock_tracer
        telemetry._TRACER_INIT = True

        # 例外が上がらなければ OK
        telemetry.emit_span(msg_id="x", model="m", result=_make_result())


# ---------- JSON protocol 設定 ----------


class TestJsonProtocol:
    def setup_method(self) -> None:
        telemetry.reset_for_testing()

    def test_sets_json_protocol_env_when_not_set(self, monkeypatch) -> None:
        """OTEL_EXPORTER_OTLP_PROTOCOL 未設定時は 'http/json' を注入する。"""
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

        assert os.environ.get("OTEL_EXPORTER_OTLP_PROTOCOL") == "http/json"

    def test_preserves_existing_protocol_env(self, monkeypatch) -> None:
        """ユーザーが OTEL_EXPORTER_OTLP_PROTOCOL を設定済みならそれを尊重する。"""
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

        assert os.environ.get("OTEL_EXPORTER_OTLP_PROTOCOL") == "http/protobuf"
