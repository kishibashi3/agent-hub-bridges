"""OTLP span emit for bridge-claude (issue #90, #92, #109).

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

**tool_use child span (issue #109)**:
  1 メッセージの trace で LLM コスト + 全 tool 実行を一覧できるよう、
  ``tool_uses`` で渡された各 ``ToolUseRecord`` に対して child span を emit する。
  child span の親は root span (``bridge_claude.send_message``)。
  child span 名: ``tool.<tool_name>`` (例: ``tool.Bash``, ``tool.Read``)。
  explicit start/end timestamp を使うため、tool 実行が過去に遡っても正確な duration を記録できる。

span 属性 (GenAI semantic conventions + custom):
  Root span:
  - ``message.id``: 受信した agent-hub message ID (caused_by_id; custom)
  - ``gen_ai.request.model``: model name
  - ``gen_ai.usage.input_tokens``: input tokens
  - ``gen_ai.usage.output_tokens``: output tokens
  - ``gen_ai.usage.cache_read.input_tokens``: cache read tokens (ドット区切り)
  - ``gen_ai.usage.cost_usd``: estimated LLM cost in USD (ResultMessage.total_cost_usd)

  Child span (per tool_use):
  - ``message.id``: 受信した agent-hub message ID (caused_by_id; custom)
  - ``tool.name``: tool 名 (例: ``"Bash"``, ``"Read"``)
  - ``tool.args.<key>``: sanitized tool 引数 (各値を 200 文字に truncate)
  - ``duration_ms``: tool 実行時間 (ms)

送信先: ``${AGENT_HUB_TELEMETRY_URL}/v1/traces``
"""

from __future__ import annotations

import logging
import os
from typing import TYPE_CHECKING, Any, TypedDict

if TYPE_CHECKING:
    from claude_agent_sdk import ResultMessage

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# ToolUseRecord (issue #109)
# ---------------------------------------------------------------------------


class ToolUseRecord(TypedDict):
    """1 回の tool_use 実行の timing record.

    ``worker._handle_one`` で ``AssistantMessage`` / ``UserMessage`` を観測して
    構築し、 ``emit_span`` に渡す。 ``emit_span`` はこれを child span として emit する。

    Attributes:
        name:          tool 名 (例: ``"Bash"``, ``"Read"``, ``"mcp__agent-hub__send_message"``)。
        input:         tool 呼び出し時の引数 dict。``_sanitize_tool_input`` で sanitize
                       してから span 属性に記録する (値は 200 文字に truncate)。
        start_time_ns: tool_use request (``AssistantMessage`` の ``ToolUseBlock``) を
                       受信した時刻 (``time.time_ns()``)。
        end_time_ns:   tool_use result (``UserMessage`` の ``ToolResultBlock``) を
                       受信した時刻 (``time.time_ns()``)。
        is_error:      ``ToolResultBlock.is_error`` が truthy かどうか。
    """

    name: str
    input: dict[str, Any]
    start_time_ns: int
    end_time_ns: int
    is_error: bool


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
# _sanitize_tool_input (issue #109, #112)
# ---------------------------------------------------------------------------

# サブワード合成パターンでマスクするキーワード (issue #112)。
# substring match を適用するため、短すぎて false positive になる単語は含めない。
# 例: "key" は exact match 専用 (_SENSITIVE_KEYS_EXACT) で管理する。
_SENSITIVE_KEYWORDS: tuple[str, ...] = (
    "password",
    "secret",
    "token",
    "credential",
    "authorization",
    "api_key",
    "private_key",
    "access_key",
)

# 完全一致専用 (短すぎて substring match では false positive になるもの)
_SENSITIVE_KEYS_EXACT: frozenset[str] = frozenset({"key", "pat"})

_TOOL_ARG_MAX_LEN: int = 200


def _sanitize_tool_input(tool_input: dict[str, Any]) -> dict[str, str]:
    """tool_input を span 属性用に sanitize する (issue #109, #112).

    各 value を ``str`` に変換して ``_TOOL_ARG_MAX_LEN`` 文字に truncate する。
    key 名が機密情報と判断される場合は値を ``"***"`` に置換する。

    機密判定ロジック (issue #112):
    - ``_SENSITIVE_KEYS_EXACT`` に完全一致する場合 ("key", "pat")
    - ``_SENSITIVE_KEYWORDS`` のいずれかを substring として含む場合
      (例: "api_token", "access_token", "client_secret" 等のサブワード合成を捕捉)
    - 大文字小文字を区別しない

    Args:
        tool_input: ``ToolUseBlock.input`` の dict。

    Returns:
        sanitize 済みの ``{key: str_value}`` dict。
    """
    result: dict[str, str] = {}
    for k, v in tool_input.items():
        lower = k.lower()
        if lower in _SENSITIVE_KEYS_EXACT or any(kw in lower for kw in _SENSITIVE_KEYWORDS):
            result[k] = "***"
        else:
            result[k] = str(v)[:_TOOL_ARG_MAX_LEN]
    return result


# ---------------------------------------------------------------------------
# emit_span
# ---------------------------------------------------------------------------


def emit_span(
    *,
    caused_by_id: str,
    sent_msg_id: str | None = None,
    model: str,
    result: ResultMessage,
    tool_uses: list[ToolUseRecord] | None = None,
) -> None:
    """send_message 1 呼び出し後に OTLP span を emit する (issue #90, #92, #109).

    ``AGENT_HUB_TELEMETRY_URL`` 未設定時はサイレント skip (opt-in)。
    例外は ``logger.warning`` で読み捨て — span 失敗で bridge を停止させない。

    **設計 (issue #109)**:
    各メッセージに対して 1 root span + N child span (tool_use ごと) を emit する。
    セッション全体で trace_id を統一するのではなく、メッセージごとに独立した trace_id を
    使う (``caused_by_id`` から生成)。tool_use child span に ``message.id`` 属性を付けることで
    Jaeger の属性検索で 1 メッセージの全 tool 実行を一覧できる。

    Args:
        caused_by_id:   受信した agent-hub message の UUID (``IncomingMessage.id``)。
                        root span の ``message.id`` 属性・trace_id の生成元。
        sent_msg_id:    ``send_message`` ツールが返した ``id`` フィールドの UUID。
                        OTel root span の ``span_id`` (64bit hex) として設定される。
                        ``None`` の場合は span_id をランダム生成にフォールバックする。
        model:          呼び出した Claude model (例: ``"claude-sonnet-4-6"``)。
        result:         ``receive_response()`` が最後に返す ``ResultMessage``。
                        ``result.usage`` から token counts、``result.total_cost_usd``
                        から推定コストを取り出す。
        tool_uses:      ``_handle_one`` で収集した ``ToolUseRecord`` のリスト。
                        各 record が child span として emit される。
                        ``None`` または空リストの場合は child span を emit しない。

    OTel span 文脈 (issue #92):
        - ``trace_id``:       ``_uuid_to_trace_id_int(caused_by_id)``
                              各メッセージが独立した trace を持つ (per-message trace)。
        - ``parent_span_id``: ``_uuid_to_span_id_int(caused_by_id)``
                              caused_by_id を phantom parent として設定。
        - ``span_id``:        ``_uuid_to_span_id_int(sent_msg_id)`` (送信 msg.id の高位 64bit)

    Root span 属性 (ドット区切り — アンダースコア不可):
        - ``message.id``: 受信した agent-hub message ID (caused_by_id)
        - ``gen_ai.request.model``: model name
        - ``gen_ai.usage.input_tokens``: input tokens (int)
        - ``gen_ai.usage.output_tokens``: output tokens (int)
        - ``gen_ai.usage.cache_read.input_tokens``: cache read tokens (int)
        - ``gen_ai.usage.cost_usd``: estimated cost in USD (float; omitted if None)

    Child span 属性 (per tool_use):
        - ``message.id``: caused_by_id (root span と同じ値)
        - ``tool.name``: tool 名 (例: ``"Bash"``)
        - ``tool.args.<key>``: sanitize 済みの tool 引数値 (200 文字に truncate)
        - ``duration_ms``: tool 実行時間 (ms; int)
    """
    tracer = _get_tracer()
    if tracer is None:
        return

    try:
        from opentelemetry import context as otel_ctx_api
        from opentelemetry.trace import (
            NonRecordingSpan,
            SpanContext,
            StatusCode,
            TraceFlags,
            set_span_in_context,
        )

        usage: dict[str, Any] = {}
        if isinstance(result.usage, dict):
            usage = result.usage

        input_tokens: int = int(usage.get("input_tokens") or 0)
        output_tokens: int = int(usage.get("output_tokens") or 0)
        cache_read: int = int(usage.get("cache_read_input_tokens") or 0)

        # issue #92: span_id を sent_msg_id に固定 (IdGenerator one-shot 注入)。
        if sent_msg_id is not None and _id_generator is not None:
            _id_generator.set_next_span_id(_uuid_to_span_id_int(sent_msg_id))

        # issue #92: parent_span_id / trace_id を caused_by_id から構築して
        # OTel context として注入する。各メッセージが独立した trace_id を持つ
        # (per-message trace design; issue #109 redesign)。
        parent_span_id_int = _uuid_to_span_id_int(caused_by_id)
        trace_id_int = _uuid_to_trace_id_int(caused_by_id)
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
            span.set_attribute("message.id", caused_by_id)
            span.set_attribute("gen_ai.request.model", model)
            span.set_attribute("gen_ai.usage.input_tokens", input_tokens)
            span.set_attribute("gen_ai.usage.output_tokens", output_tokens)
            span.set_attribute("gen_ai.usage.cache_read.input_tokens", cache_read)
            # issue #109: LLM コスト (USD) を span 属性として記録する。
            total_cost = getattr(result, "total_cost_usd", None)
            if total_cost is not None:
                try:
                    span.set_attribute("gen_ai.usage.cost_usd", float(total_cost))
                except (TypeError, ValueError):
                    pass
            span.set_status(
                StatusCode.ERROR if result.is_error else StatusCode.OK
            )

            # issue #109: tool_use ごとに child span を emit する。
            # child span の親は root span (start_as_current_span で設定した current span)。
            # explicit start/end timestamp で実際の tool 実行時間を記録する。
            if tool_uses:
                root_span_ctx = otel_ctx_api.get_current()
                for tu in tool_uses:
                    child = tracer.start_span(
                        f"tool.{tu['name']}",
                        context=root_span_ctx,
                        start_time=tu["start_time_ns"],
                    )
                    child.set_attribute("message.id", caused_by_id)
                    child.set_attribute("tool.name", tu["name"])
                    sanitized = _sanitize_tool_input(tu["input"])
                    for k, v in sanitized.items():
                        child.set_attribute(f"tool.args.{k}", v)
                    duration_ms = max(
                        0,
                        (tu["end_time_ns"] - tu["start_time_ns"]) // 1_000_000,
                    )
                    child.set_attribute("duration_ms", duration_ms)
                    child.set_status(
                        StatusCode.ERROR if tu["is_error"] else StatusCode.OK
                    )
                    child.end(end_time=tu["end_time_ns"])

        logger.debug(
            "[telemetry] span emitted: caused_by=%s sent=%s model=%s "
            "in=%d out=%d cache_read=%d cost_usd=%s is_error=%s tools=%d",
            caused_by_id,
            sent_msg_id,
            model,
            input_tokens,
            output_tokens,
            cache_read,
            total_cost,
            result.is_error,
            len(tool_uses) if tool_uses else 0,
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
