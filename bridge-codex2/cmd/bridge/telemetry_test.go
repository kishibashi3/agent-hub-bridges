package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestShutdownTelemetryBounded は OTLP 先が accept するが応答しないとき、
// shutdownTelemetry が telemetryShutdownTimeout 程度で戻ることを確かめる (issue #411)。
// 期限なしだと BatchSpanProcessor の ExportTimeout (既定 30s) まで戻らない。
func TestShutdownTelemetryBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c) // 読みも書きもしない (応答しない OTLP 先)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	}()

	exp, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL("http://"+ln.Addr().String()+"/v1/traces"),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	_, span := tp.Tracer("test").Start(context.Background(), "pending")
	span.End()

	prev := globalTP
	globalTP = tp
	defer func() { globalTP = prev }()

	start := time.Now()
	shutdownTelemetry()
	elapsed := time.Since(start)
	if elapsed > telemetryShutdownTimeout+2*time.Second {
		t.Fatalf("shutdownTelemetry took %v, want <= %v", elapsed, telemetryShutdownTimeout+2*time.Second)
	}
}
