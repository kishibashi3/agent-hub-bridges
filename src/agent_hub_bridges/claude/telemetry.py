"""OTLP span emit for bridge-claude (issue #90, #92).

``AGENT_HUB_TELEMETRY_URL`` が未設定の場合は全操作がサイレント skip (opt-in)。
設定されている場合は OTLP/HTTP (`Content-Type: application/x-protobuf`) で span emit する。

**送信フォーマット**:
  ``opentelemetry-exporter-otlp-proto-http`` v1.42.1 は protobuf のみをサポートする
  (``encode_spans().SerializePartialToString()``、 ``Content-Type: application/x-protobuf``)。
  issue #90 の仕様は "JSON" を指定しているが、otelite (Grafana Alloy) は
  protobuf も受け付け、スパンは正常に届く。真の JSON が必要な場合は
  将来の SDK バージョンアップ、または `requests.Session` ベースのカスタム
  exporter への差し替えを検討する。

**span 文脈 (issue #92)**:
  ``caused_by_id`` (= 受信した ``IncomingMessage.id``) を
  ``parent_span_id`` (64bit hex) として設定。
  ``sent_msg_id`` (= ``send_message`` 返却の ``id`` フィールド) を
  ``span_id`` (64bit hex) として設定。
  UUID → 64bit hex 変換: UUID の先頭 16 hex 文字 (= 高位 64bit) を使う。

  これにより otelite 上で caused_by の連鎖が trace の親子関係として見えるようになる。
  ``root_message_id`` が bridge に届かないため各ホップは別 trace_id だが、
  ``parent_span_id`` → ``span_id`` の連鎖で辿れる (1 ホップずつ)。

span 属性 (GenAI semantic conventions + custom):
  - ``msg_id``: 受信した agent-hub message ID (caused_by_id; custom)
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

# module-level singletons: TracerProvider / Tracer / IdGenerator の遅延初期化。
# None   = 未初期化 or 初期化済みだが無効 (URL 未設定 / import 失敗)。
# object = 有効な opentelemetry Tracer / _FixedNextSpanIdGenerator インスタンス。
#
# スレッド安全性: bridge-claude は asyncio single-threaded で動作するため
# GIL を超えた concurrent write は発生しない。複数スレッドから呼ぶ場合は
# Lock を追加すること。
_tracer: Any = None
_TRACER_INIT: bool = False
_id_generator: Any = None  # _FixedNextSpanIdGenerator | None (issue #92)
_SERVICE_NAME: str = "bridge-claude"  # issue #96: OTel service.name (set via configure())


# ---------------------------------------------------------------------------
# UUID → OTel ID 変換ユーティリティ (issue #92)
# ---------------------------------------------------------------------------


def _uuid_to_span_id_int(uuid_str: str) -> int:
    """UUID 文字列を 64-bit 整数に変換する (先頭 16 hex 文字 = 高位 64bit を使用).

    OTel の span_id / parent_span_id は 64bit。UUID は 128bit なので
    先頭 16 hex 文字 (= 高位 64bit) を int に変換する。

    Args:
        uuid_str: ハイフン付き or なしの UUID 文字列
                  (例: ``"550e8400-e29b-41d4-a716-446655440000"``)。

    Returns:
        64-bit unsigned integer。

    Raises:
        ValueError: hex 文字列が 32 文字でない (= 有効な UUID ではない)。
    """
    hex_str = uuid_str.replace("-", "")
    if len(hex_str) != 32:
        raise ValueError(
            f"Invalid UUID hex length ({len(hex_str)} chars): {uuid_str!r}"
        )
    return int(hex_str[:16], 16)


def _uuid_to_trace_id_int(uuid_str: str) -> int:
    """UUID 文字列を 128-bit 整数に変換する (OTel trace_id 用).

    Args:
        uuid_str: ハイフン付き or なしの UUID 文字列。

    Returns:
        128-bit unsigned integer。

    Raises:
        ValueError: hex 文字列が 32 文字でない (= 有効な UUID ではない)。
    """
    hex_str = uuid_str.replace("-", "")
    if len(hex_str) != 32:
        raise ValueError(
            f"Invalid UUID hex length ({len(hex_str)} chars): {uuid_str!r}"
        )
    return int(hex_str, 16)


# ---------------------------------------------------------------------------
# _FixedNextSpanIdGenerator (issue #92)
# ---------------------------------------------------------------------------


class _FixedNextSpanIdGenerator:
    """OTel IdGenerator の duck-type 互換実装: 次の span_id に固定値を one-shot 注入できる.

    ``set_next_span_id(span_id)`` で予約した値を次の ``generate_span_id()`` で返す。
    一度返したら ``None`` にリセットして以降はランダム生成に戻る (one-shot)。

    用途 (issue #92): send_message が返した ``id`` を OTel span の span_id として
    設定するために、span 開始直前に ``set_next_span_id`` で予約しておく。

    スレッド安全性: bridge-claude は asyncio single-threaded なので Lock 不要。
    duck-type で ``opentelemetry.sdk.trace.id_generator.IdGenerator`` の
    ``generate_span_id()`` / ``generate_trace_id()`` インタフェースを満たす。
    """

    def __init__(self) -> None:
        self._next_span_id: int | None = None
        self._inner: Any = None  # RandomIdGenerator, lazy init

    def _ensure_inner(self) -> None:
        if self._inner is None:
            from opentelemetry.sdk.trace.id_generator import RandomIdGenerator

            self._inner = RandomIdGenerator()

    def set_next_span_id(self, span_id: int) -> None:
        """次の ``generate_span_id()`` が返す値を予約する (one-shot)."""
        self._next_span_id = span_id

    def generate_span_id(self) -> int:
        if self._next_span_id is not None:
            sid = self._next_span_id
            self._next_span_id = None
            return sid
        self._ensure_inner()
        return self._inner.generate_span_id()

    def generate_trace_id(self) -> int:
        self._ensure_inner()
        return self._inner.generate_trace_id()


# ---------------------------------------------------------------------------
# configure (issue #96)
# ---------------------------------------------------------------------------


def configure(*, service_name: str) -> None:
    """Telemetry の service.name を設定する (issue #96).

    otelite ダッシュボードで ``Service: unknown_service`` になる問題を解消するため、
    ``TracerProvider`` に ``Resource({"service.name": service_name})`` を設定する。

    **呼び出しタイミング**: ``_get_tracer()`` の遅延初期化より前に呼ぶこと。
    初期化後に呼んでも ``TracerProvider`` は再作成されないため反映されない。
    ``run_worker()`` の先頭で ``configure(service_name=f"@{config.user}")``
    を呼ぶのが想定使用例。

    Args:
        service_name: OTel ``service.name`` に設定する文字列
                      (例: ``"@bridge-claude"``, ``"@planner"``)。
    """
    global _SERVICE_NAME
    _SERVICE_NAME = service_name


# ---------------------------------------------------------------------------
# build_traceparent / make_subprocess_telemetry_env (issue #91)
# ---------------------------------------------------------------------------


def build_traceparent(msg_id: str) -> str:
    """受信 msg_id から W3C traceparent 文字列を生成する (issue #91).

    format: ``00-{trace_id_hex_32}-{span_id_hex_16}-01``

    trace_id は ``_uuid_to_trace_id_int(msg_id)`` (UUID 全 128bit)。
    parent_span_id は ``_uuid_to_span_id_int(msg_id)`` (UUID 高位 64bit)。
    flags: ``01`` (sampled)。

    生成した traceparent を ``ClaudeAgentOptions.env["TRACEPARENT"]`` に設定すると、
    Claude CLI subprocess の ``claude_code.llm_request`` span が
    この traceparent の子 span として OTLP に記録される。
    ``subprocess_cli.py`` の ``connect()`` がこの値を process_env に注入する
    (``ClaudeAgentOptions.env`` は auto-inject より優先)。

    Args:
        msg_id: agent-hub の受信 ``IncomingMessage.id`` (UUID 文字列)。

    Returns:
        W3C traceparent 文字列。例::

            "00-550e8400e29b41d4a716446655440000-550e8400e29b41d4-01"

    Raises:
        ValueError: ``msg_id`` が有効な UUID でない場合 (``_uuid_to_trace_id_int``
            / ``_uuid_to_span_id_int`` が raise する)。

    Note:
        nil UUID (``00000000-0000-0000-0000-000000000000``) を渡すと
        trace_id / span_id がすべてゼロになり W3C 仕様上無効な traceparent になる。
        agent-hub の msg_id は UUID v4 前提であるため実害はないが、
        nil UUID を渡さないよう呼び出し側で保証すること。
    """
    trace_id_int = _uuid_to_trace_id_int(msg_id)
    span_id_int = _uuid_to_span_id_int(msg_id)
    return f"00-{trace_id_int:032x}-{span_id_int:016x}-01"


def make_subprocess_telemetry_env(telemetry_url: str) -> dict[str, str]:
    """Claude CLI subprocess に渡す OpenTelemetry 環境変数を返す (issue #91).

    ``AGENT_HUB_TELEMETRY_URL`` が設定されている場合に ``_build_options()`` から
    呼ばれ、Claude CLI の telemetry (``claude_code.llm_request`` span 等) を
    有効化するための環境変数セットを返す。

    Args:
        telemetry_url: OTLP エクスポート先 URL (``AGENT_HUB_TELEMETRY_URL`` の値)。

    Returns:
        env dict::

            {
                "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
                "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
                "OTEL_TRACES_EXPORTER": "otlp",
                "OTEL_EXPORTER_OTLP_ENDPOINT": "<url>",
            }

        trailing ``/`` は URL から除去する。
    """
    return {
        "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
        "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
        "OTEL_TRACES_EXPORTER": "otlp",
        "OTEL_EXPORTER_OTLP_ENDPOINT": telemetry_url.rstrip("/"),
    }


# ---------------------------------------------------------------------------
# Tracer 初期化
# ---------------------------------------------------------------------------


def _get_tracer() -> Any:
    """TracerProvider を遅延初期化して Tracer を返す。

    URL 未設定または opentelemetry 未インストールの場合は None。
    初回呼び出しのみ初期化処理を走らせる (以降はキャッシュを返す)。

    issue #92: ``_FixedNextSpanIdGenerator`` を TracerProvider に注入して
    ``_id_generator`` モジュールグローバルに保持する。
    """
    global _tracer, _TRACER_INIT, _id_generator
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

        # issue #92: span_id を sent_msg_id に固定するためのカスタム IdGenerator。
        # issue #96: service.name を @handle 名に設定する (Resource 経由)。
        from opentelemetry.sdk.resources import Resource

        id_gen = _FixedNextSpanIdGenerator()
        resource = Resource({"service.name": _SERVICE_NAME})
        provider = TracerProvider(id_generator=id_gen, resource=resource)
        provider.add_span_processor(BatchSpanProcessor(exporter))
        trace.set_tracer_provider(provider)
        _tracer = trace.get_tracer("agent-hub-bridge-claude")
        _id_generator = id_gen
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


# ---------------------------------------------------------------------------
# emit_span
# ---------------------------------------------------------------------------


def emit_span(
    *,
    caused_by_id: str,
    sent_msg_id: str | None = None,
    model: str,
    result: ResultMessage,
    session_trace_root_id: str | None = None,
) -> None:
    """send_message 1 呼び出し後に OTLP span を emit する (issue #90, #92, #108).

    ``AGENT_HUB_TELEMETRY_URL`` 未設定時はサイレント skip (opt-in)。
    例外は ``logger.warning`` で読み捨て — span 失敗で bridge を停止させない。

    Args:
        caused_by_id:           受信した agent-hub message の UUID (``IncomingMessage.id``)。
                                ``msg_id`` span 属性として記録される。
        sent_msg_id:            ``send_message`` ツールが返した ``id`` フィールドの UUID。
                                OTel ``span_id`` (64bit hex) として設定される。
                                ``None`` の場合は span_id をランダム生成にフォールバックする。
        model:                  呼び出した Claude model (例: ``"claude-sonnet-4-6"``)。
        result:                 ``receive_response()`` が最後に返す ``ResultMessage``。
                                ``result.usage`` から token counts を取り出す。
        session_trace_root_id:  セッション内の最初のメッセージ ID (issue #108)。
                                指定された場合、この UUID から ``trace_id`` と
                                ``parent_span_id`` を生成する。
                                ``None`` の場合は ``caused_by_id`` を使う (後方互換)。

    OTel span 文脈 (issue #92, #108):
        - ``trace_id``:      ``_uuid_to_trace_id_int(session_trace_root_id or caused_by_id)``
                             session_trace_root_id を指定するとセッション内の全 bridge span が
                             同じ trace_id を持ち、subprocess span と Jaeger 上で接続される。
        - ``parent_span_id``: ``_uuid_to_span_id_int(session_trace_root_id or caused_by_id)``
                             subprocess の TRACEPARENT と同じ値を使うことで
                             bridge span と subprocess span が同じ phantom root 下に並ぶ。
        - ``span_id``:       ``_uuid_to_span_id_int(sent_msg_id)`` (送信 msg.id の高位 64bit)

    Span 属性 (ドット区切り — アンダースコア不可):
        - ``msg_id``: 受信した agent-hub message ID (caused_by_id)
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

        # issue #92: span_id を sent_msg_id に固定 (IdGenerator one-shot 注入)。
        if sent_msg_id is not None and _id_generator is not None:
            _id_generator.set_next_span_id(_uuid_to_span_id_int(sent_msg_id))

        # issue #92: parent_span_id / trace_id を caused_by_id (or session_trace_root_id)
        # から構築して OTel context として注入する。
        #
        # issue #108: trace_id mismatch 修正。
        # subprocess の TRACEPARENT はセッション最初のメッセージ ID で設定される。
        # bridge span も同じ trace_id を使わないと Jaeger 上でトレースが分離する。
        # session_trace_root_id が渡された場合はそちらを trace_id / parent_span_id に使う。
        from opentelemetry.trace import (
            NonRecordingSpan,
            SpanContext,
            TraceFlags,
            set_span_in_context,
        )

        trace_root = session_trace_root_id if session_trace_root_id else caused_by_id
        parent_span_id_int = _uuid_to_span_id_int(trace_root)
        trace_id_int = _uuid_to_trace_id_int(trace_root)
        parent_ctx = SpanContext(
            trace_id=trace_id_int,
            span_id=parent_span_id_int,
            is_remote=True,
            trace_flags=TraceFlags(TraceFlags.SAMPLED),
        )
        ctx = set_span_in_context(NonRecordingSpan(parent_ctx))

        with tracer.start_as_current_span(
            "bridge_claude.send_message", context=ctx
        ) as span:
            span.set_attribute("msg_id", caused_by_id)
            span.set_attribute("gen_ai.request.model", model)
            span.set_attribute("gen_ai.usage.input_tokens", input_tokens)
            span.set_attribute("gen_ai.usage.output_tokens", output_tokens)
            span.set_attribute("gen_ai.usage.cache_read.input_tokens", cache_read)
            span.set_status(
                StatusCode.ERROR if result.is_error else StatusCode.OK
            )

        logger.debug(
            "[telemetry] span emitted: caused_by=%s sent=%s model=%s "
            "in=%d out=%d cache_read=%d is_error=%s",
            caused_by_id,
            sent_msg_id,
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
    global _tracer, _TRACER_INIT, _id_generator, _SERVICE_NAME
    _tracer = None
    _TRACER_INIT = False
    _id_generator = None
    _SERVICE_NAME = "bridge-claude"
