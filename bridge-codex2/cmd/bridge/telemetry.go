// telemetry.go — OTLP span emit for bridge-codex2
//
// bridge-claude2 の telemetry.go 相当 (issue #267)。
// codex は token usage を JSON 出力しないため usage フィールドは基本的に 0。
//
// AGENT_HUB_TELEMETRY_URL 未設定の場合は全操作がサイレント skip (opt-in)。
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

// queryUsage は runner.query() が返す token usage + エラー情報。
// codex は token usage を出力しないため InputTokens / OutputTokens は常に 0。
type queryUsage struct {
	InputTokens          int
	OutputTokens         int
	CacheReadInputTokens int
	TotalCostUSD         *float64
	IsError              bool
}

var (
	globalTracer trace.Tracer
	globalTP     *sdktrace.TracerProvider
	telOnce      sync.Once
)

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
		globalTracer = tp.Tracer("bridge-codex2")
		slog.Info("[telemetry] OTLP span emit enabled", "endpoint", endpoint, "service", serviceName)
	})
}

func shutdownTelemetry() {
	if globalTP == nil {
		return
	}
	if err := globalTP.Shutdown(context.Background()); err != nil {
		slog.Warn("[telemetry] TracerProvider shutdown error", "err", err)
	}
}

func emitSpan(causedByID, model string, usage queryUsage) {
	if globalTracer == nil {
		return
	}
	model = orDefault(model, "codex-default")
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

	parentCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     parentSpanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), parentCtx)

	_, span := globalTracer.Start(ctx, "bridge_codex2.send_message")
	defer span.End()

	attrs := []attribute.KeyValue{
		attribute.String("message.id", causedByID),
		attribute.String("gen_ai.system", "openai"),
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
		span.SetStatus(codes.Error, "codex result error")
	} else {
		span.SetStatus(codes.Ok, "")
	}

	slog.Debug("[telemetry] span emitted",
		"caused_by", causedByID,
		"model", model,
		"is_error", usage.IsError,
	)
}

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
