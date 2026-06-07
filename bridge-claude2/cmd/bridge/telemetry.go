// telemetry.go — OTLP span emit for bridge-claude2 (issue #267)
//
// Python bridge-claude の telemetry.py 相当 (issue #90, #92, #265 の Go 移植)。
//
// AGENT_HUB_TELEMETRY_URL 未設定の場合は全操作がサイレント skip (opt-in)。
// 設定されている場合は OTLP/HTTP protobuf で span emit する。
//
// span 属性 (GenAI semantic conventions):
//   - message.id:                       受信 agent-hub message ID (caused_by_id)
//   - gen_ai.system:                    固定値 "anthropic" (issue #265)
//   - gen_ai.request.model:             Claude model 名
//   - gen_ai.usage.input_tokens:        input tokens
//   - gen_ai.usage.output_tokens:       output tokens
//   - gen_ai.usage.cache_read.input_tokens: cache read tokens
//   - gen_ai.usage.cost_usd:            推定コスト (USD)、nil なら omit
//
// span 文脈 (Python #92 相当):
//   caused_by_id (受信メッセージ UUID) を parent span context として設定する。
//   trace_id = UUID 全 128 bit、parent_span_id = UUID 高位 64 bit。
//   これにより caused_by の連鎖が otelite 上で親子関係として辿れる。
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	globalTracer trace.Tracer
	globalTP     *sdktrace.TracerProvider
	telOnce      sync.Once
)

// initTelemetry は OTLP tracer を初期化する。sync.Once で 1 度だけ実行される。
// AGENT_HUB_TELEMETRY_URL 未設定なら no-op。
// runWorker の前 (main で) 呼ぶこと。
func initTelemetry(serviceName string) {
	telOnce.Do(func() {
		url := os.Getenv("AGENT_HUB_TELEMETRY_URL")
		if url == "" {
			slog.Debug("[telemetry] AGENT_HUB_TELEMETRY_URL not set — OTLP disabled (opt-in)")
			return
		}

		endpoint := strings.TrimRight(url, "/")
		exp, err := otlptracehttp.New(context.Background(),
			otlptracehttp.WithEndpointURL(endpoint+"/v1/traces"),
		)
		if err != nil {
			slog.Warn("[telemetry] failed to create OTLP exporter — telemetry disabled", "err", err)
			return
		}

		res, err := resource.New(context.Background(),
			resource.WithAttributes(semconv.ServiceName(serviceName)),
		)
		if err != nil {
			res = resource.Default()
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
		)

		globalTP = tp
		globalTracer = tp.Tracer("bridge-claude2")
		slog.Info("[telemetry] OTLP span emit enabled", "endpoint", endpoint, "service", serviceName)
	})
}

// shutdownTelemetry は TracerProvider を正常停止し、バッファ内 span を flush する。
// main で defer 呼び出しすること。AGENT_HUB_TELEMETRY_URL 未設定時は no-op。
func shutdownTelemetry() {
	if globalTP == nil {
		return
	}
	if err := globalTP.Shutdown(context.Background()); err != nil {
		slog.Warn("[telemetry] TracerProvider shutdown error", "err", err)
	}
}

// emitSpan は 1 メッセージ処理後に OTLP span を emit する (issue #267)。
// globalTracer が nil (AGENT_HUB_TELEMETRY_URL 未設定) の場合はサイレント skip。
// 例外は slog.Warn で読み捨て — span 失敗で bridge を停止させない。
func emitSpan(causedByID, model string, usage queryUsage) {
	if globalTracer == nil {
		return
	}
	model = orDefault(model, "claude-default")
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("[telemetry] emitSpan panic (non-fatal)", "recover", r)
		}
	}()

	traceID, err := uuidToTraceID(causedByID)
	if err != nil {
		slog.Warn("[telemetry] invalid causedByID for traceID", "err", err)
		return
	}
	parentSpanID, err := uuidToSpanID(causedByID)
	if err != nil {
		slog.Warn("[telemetry] invalid causedByID for spanID", "err", err)
		return
	}

	// caused_by_id を phantom parent として設定 (Python #92 相当)。
	// これにより otelite 上で caused_by チェーンが親子関係として見える。
	parentCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     parentSpanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), parentCtx)

	_, span := globalTracer.Start(ctx, "bridge_claude2.send_message")
	defer span.End()

	attrs := []attribute.KeyValue{
		attribute.String("message.id", causedByID),
		attribute.String("gen_ai.system", "anthropic"),
		attribute.String("gen_ai.request.model", model),
		attribute.Int("gen_ai.usage.input_tokens", usage.InputTokens),
		attribute.Int("gen_ai.usage.output_tokens", usage.OutputTokens),
		attribute.Int("gen_ai.usage.cache_read.input_tokens", usage.CacheReadInputTokens),
	}
	if usage.TotalCostUSD != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.usage.cost_usd", *usage.TotalCostUSD))
	}
	span.SetAttributes(attrs...)

	if usage.IsError {
		span.SetStatus(codes.Error, "claude result error")
	} else {
		span.SetStatus(codes.Ok, "")
	}

	slog.Debug("[telemetry] span emitted",
		"caused_by", causedByID,
		"model", model,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"cache_read_tokens", usage.CacheReadInputTokens,
		"is_error", usage.IsError,
	)
}

// uuidToTraceID は UUID 文字列を OTel trace.TraceID (128-bit) に変換する。
// Python の _uuid_to_trace_id_int 相当。
func uuidToTraceID(uuidStr string) (trace.TraceID, error) {
	h := strings.ReplaceAll(uuidStr, "-", "")
	if len(h) != 32 {
		return trace.TraceID{}, fmt.Errorf("invalid UUID hex length (%d): %q", len(h), uuidStr)
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return trace.TraceID{}, fmt.Errorf("UUID hex decode: %w", err)
	}
	var id trace.TraceID
	copy(id[:], b)
	return id, nil
}

// uuidToSpanID は UUID 文字列を OTel trace.SpanID (64-bit) に変換する。
// UUID の先頭 16 hex 文字 (高位 64 bit) を使う。
// Python の _uuid_to_span_id_int 相当。
func uuidToSpanID(uuidStr string) (trace.SpanID, error) {
	h := strings.ReplaceAll(uuidStr, "-", "")
	if len(h) != 32 {
		return trace.SpanID{}, fmt.Errorf("invalid UUID hex length (%d): %q", len(h), uuidStr)
	}
	b, err := hex.DecodeString(h[:16])
	if err != nil {
		return trace.SpanID{}, fmt.Errorf("UUID spanID hex decode: %w", err)
	}
	var id trace.SpanID
	copy(id[:], b)
	return id, nil
}
