"""OTLP span emit for bridge-claude (issue #90).

``AGENT_HUB_TELEMETRY_URL`` が未設定の場合は全操作がサイレント skip (opt-in)。
設定されている場合は OTLP/HTTP (`Content-Type: application/x-protobuf`) で span emit する。

**送信フォーマット**:
  ``opentelemetry-exporter-otlp-proto-http`` v1.42.1 は protobuf のみをサポートする
  (``encode_spans().SerializePartialToString()``、 ``Content-Type: application/x-protobuf``)。
  issue #90 の仕様は "JSON" を指定しているが、otelite (Grafana Alloy) は
  protobuf も受け付け、スパンは正常に届く。真の JSON が必要な場合は
  将来の SDK バージョンアップ、または `requests.Session` ベースのカスタム
  exporter への差し替えを検討する。

span 属性 (GenAI semantic conventions + custom):
  - ``msg_id``: agent-hub message ID (custom)
  - ``gen_ai.request.model``: model name
  - ``gen_ai.usage.input_tokens``: input tokens
  - ``gen_ai.usage.output_tokens``: output tokens
  - ``gen_ai.usage.cache_read.input_tokens``: cache read tokens (ドット区切り)

送信先: ``${AGENT_HUB_TELEMETRY_URL}/v1/traces``
"""

from __future__ import annotations

import logging
import os
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from claude_agent_sdk import ResultMessage

logger = logging.getLogger(__name__)

# module-level singleton: TracerProvider / Tracer の遅延初期化。
# None   = 未初期化 or 初期化済みだが無効 (URL 未設定 / import 失敗)。
# object = 有効な opentelemetry Tracer インスタンス。
#
# スレッド安全性: bridge-claude は asyncio single-threaded で動作するため
# GIL を超えた concurrent write は発生しない。複数スレッドから呼ぶ場合は
# Lock を追加すること。
_tracer: Any = None
_TRACER_INIT: bool = False


def _get_tracer() -> Any:
    """TracerProvider を遅延初期化して Tracer を返す。

    URL 未設定または opentelemetry 未インストールの場合は None。
    初回呼び出しのみ初期化処理を走らせる (以降はキャッシュを返す)。
    """
    global _tracer, _TRACER_INIT
    if _TRACER_INIT:
        return _tracer

    _TRACER_INIT = True
    url = os.environ.get("AGENT_HUB_TELEMETRY_URL")
    if not url:
        logger.debug(
            "AGENT_HUB_TELEMETRY_URL not set — OTLP telemetry disabled (opt-in)"
        )
        return None

    try:
        from opentelemetry import trace
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import (
            OTLPSpanExporter,
        )
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor

        # opentelemetry-exporter-otlp-proto-http v1.42.1 は常に protobuf を使う。
        # OTEL_EXPORTER_OTLP_PROTOCOL は OTLPSpanExporter コンストラクタで
        # 読まれないため、env var を書き換えても効果がない (reviewer Minor 対応)。
        # Content-Type は "application/x-protobuf" で固定される。
        # otelite は protobuf を受け付け、スパンは正常に届くことを実機確認済み。
        endpoint = url.rstrip("/") + "/v1/traces"
        exporter = OTLPSpanExporter(endpoint=endpoint)
        provider = TracerProvider()
        provider.add_span_processor(BatchSpanProcessor(exporter))
        trace.set_tracer_provider(provider)
        _tracer = trace.get_tracer("agent-hub-bridge-claude")
        logger.info(
            "[telemetry] OTLP span emit enabled: endpoint=%s (protobuf)",
            endpoint,
        )
    except ImportError:
        logger.warning(
            "[telemetry] opentelemetry packages not installed — "
            "OTLP telemetry disabled. "
            "Install with: pip install 'agent-hub-bridges[claude]'"
        )
    except Exception:
        logger.exception(
            "[telemetry] Failed to initialize OTLP telemetry — telemetry disabled"
        )

    return _tracer


def emit_span(
    *,
    msg_id: str,
    model: str,
    result: ResultMessage,
) -> None:
    """send_message 1 呼び出し後に OTLP span を emit する (issue #90).

    ``AGENT_HUB_TELEMETRY_URL`` 未設定時はサイレント skip (opt-in)。
    例外は ``logger.warning`` で読み捨て — span 失敗で bridge を停止させない。

    Args:
        msg_id: 受信した agent-hub message の UUID (``IncomingMessage.id``)。
        model:  呼び出した Claude model (例: ``"claude-sonnet-4-6"``)。
        result: ``receive_response()`` が最後に返す ``ResultMessage``。
                ``result.usage`` から token counts を取り出す。

    Span 属性 (ドット区切り — アンダースコア不可):
        - ``msg_id``: agent-hub message ID
        - ``gen_ai.request.model``: model name
        - ``gen_ai.usage.input_tokens``: input tokens (int)
        - ``gen_ai.usage.output_tokens``: output tokens (int)
        - ``gen_ai.usage.cache_read.input_tokens``: cache read tokens (int)
    """
    tracer = _get_tracer()
    if tracer is None:
        return

    try:
        from opentelemetry.trace import StatusCode

        usage: dict[str, Any] = {}
        if isinstance(result.usage, dict):
            usage = result.usage

        input_tokens: int = int(usage.get("input_tokens") or 0)
        output_tokens: int = int(usage.get("output_tokens") or 0)
        cache_read: int = int(usage.get("cache_read_input_tokens") or 0)

        with tracer.start_as_current_span("bridge_claude.send_message") as span:
            span.set_attribute("msg_id", msg_id)
            span.set_attribute("gen_ai.request.model", model)
            span.set_attribute("gen_ai.usage.input_tokens", input_tokens)
            span.set_attribute("gen_ai.usage.output_tokens", output_tokens)
            span.set_attribute("gen_ai.usage.cache_read.input_tokens", cache_read)
            span.set_status(
                StatusCode.ERROR if result.is_error else StatusCode.OK
            )

        logger.debug(
            "[telemetry] span emitted: msg_id=%s model=%s "
            "in=%d out=%d cache_read=%d is_error=%s",
            msg_id,
            model,
            input_tokens,
            output_tokens,
            cache_read,
            result.is_error,
        )
    except Exception:
        logger.warning("[telemetry] OTLP span emit failed (non-fatal)", exc_info=True)


def reset_for_testing() -> None:
    """テスト用: module-level singleton をリセットする。

    テスト間で状態が漏れないようにするため、各テストの setUp / tearDown で呼ぶ。
    本番コードでは使用しない。
    """
    global _tracer, _TRACER_INIT
    _tracer = None
    _TRACER_INIT = False
