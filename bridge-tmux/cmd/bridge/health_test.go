package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ─────────────────────────────────────────────
// HealthState unit tests
// ─────────────────────────────────────────────

func TestHealthState_EnsurePersona(t *testing.T) {
	h := NewHealthState("single")
	h.EnsurePersona("@foo")

	snap := h.snapshot()
	if len(snap.Personas) != 1 {
		t.Fatalf("want 1 persona, got %d", len(snap.Personas))
	}
	if snap.Personas[0].Handle != "@foo" {
		t.Errorf("want handle @foo, got %q", snap.Personas[0].Handle)
	}
	if snap.Personas[0].SessionAlive {
		t.Error("want SessionAlive=false on creation")
	}

	// duplicate call must not create extra entry
	h.EnsurePersona("@foo")
	snap2 := h.snapshot()
	if len(snap2.Personas) != 1 {
		t.Errorf("want 1 persona after duplicate EnsurePersona, got %d", len(snap2.Personas))
	}
}

func TestHealthState_RecordMessage(t *testing.T) {
	h := NewHealthState("single")
	h.EnsurePersona("@bar")

	h.RecordMessage("@bar")
	h.RecordMessage("@bar")

	snap := h.snapshot()
	p := snap.Personas[0]
	if p.MessagesProcessed != 2 {
		t.Errorf("want MessagesProcessed=2, got %d", p.MessagesProcessed)
	}
	if p.LastMessageAt == nil {
		t.Error("want LastMessageAt set, got nil")
	}
}

func TestHealthState_RecordMessage_ClearsError(t *testing.T) {
	h := NewHealthState("single")
	h.RecordError("@bar", "some error")
	h.RecordMessage("@bar")

	snap := h.snapshot()
	if snap.Personas[0].LastError != "" {
		t.Errorf("want LastError cleared after RecordMessage, got %q", snap.Personas[0].LastError)
	}
}

func TestHealthState_RecordMessage_CreatesPersona(t *testing.T) {
	// EnsurePersona なしで RecordMessage → auto-create
	h := NewHealthState("single")
	h.RecordMessage("@baz")

	snap := h.snapshot()
	if len(snap.Personas) != 1 {
		t.Fatalf("want 1 persona, got %d", len(snap.Personas))
	}
	if snap.Personas[0].MessagesProcessed != 1 {
		t.Errorf("want MessagesProcessed=1, got %d", snap.Personas[0].MessagesProcessed)
	}
}

func TestHealthState_RecordError(t *testing.T) {
	h := NewHealthState("fleet")
	h.RecordError("@qux", "spawn failed: context deadline exceeded")

	snap := h.snapshot()
	if len(snap.Personas) != 1 {
		t.Fatalf("want 1 persona, got %d", len(snap.Personas))
	}
	p := snap.Personas[0]
	if p.LastError == "" {
		t.Error("want LastError set, got empty")
	}
	if p.Handle != "@qux" {
		t.Errorf("want handle @qux, got %q", p.Handle)
	}
}

func TestHealthState_SetSessionAlive(t *testing.T) {
	h := NewHealthState("single")
	h.EnsurePersona("@s")

	h.SetSessionAlive("@s", true)
	if !h.snapshot().Personas[0].SessionAlive {
		t.Error("want SessionAlive=true after Set(true)")
	}

	h.SetSessionAlive("@s", false)
	if h.snapshot().Personas[0].SessionAlive {
		t.Error("want SessionAlive=false after Set(false)")
	}
}

func TestHealthState_Snapshot_Mode(t *testing.T) {
	for _, mode := range []string{"single", "fleet"} {
		h := NewHealthState(mode)
		snap := h.snapshot()
		if snap.Mode != mode {
			t.Errorf("mode %q: want %q in snapshot, got %q", mode, mode, snap.Mode)
		}
		if snap.Status != "ok" {
			t.Errorf("want status ok, got %q", snap.Status)
		}
	}
}

func TestHealthState_Snapshot_UptimeSec(t *testing.T) {
	h := NewHealthState("single")
	time.Sleep(10 * time.Millisecond)
	snap := h.snapshot()
	if snap.UptimeSec < 0.005 {
		t.Errorf("want uptime > 5ms, got %.4fs", snap.UptimeSec)
	}
}

func TestHealthState_Snapshot_SortedPersonas(t *testing.T) {
	h := NewHealthState("fleet")
	h.EnsurePersona("@zzz")
	h.EnsurePersona("@aaa")
	h.EnsurePersona("@mmm")

	snap := h.snapshot()
	want := []string{"@aaa", "@mmm", "@zzz"}
	for i, w := range want {
		if snap.Personas[i].Handle != w {
			t.Errorf("index %d: want %q, got %q", i, w, snap.Personas[i].Handle)
		}
	}
}

// TestHealthState_ConcurrentAccess は go test -race でクリーンなことを確認する。
func TestHealthState_ConcurrentAccess(t *testing.T) {
	h := NewHealthState("fleet")
	h.EnsurePersona("@p1")
	h.EnsurePersona("@p2")

	const workers = 8
	const iters = 100
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := range workers {
		go func(i int) {
			defer wg.Done()
			handle := "@p1"
			if i%2 == 0 {
				handle = "@p2"
			}
			for range iters {
				h.RecordMessage(handle)
				h.SetSessionAlive(handle, i%3 == 0)
				h.RecordError(handle, "transient")
				_ = h.snapshot()
			}
		}(i)
	}
	wg.Wait()

	snap := h.snapshot()
	for _, p := range snap.Personas {
		if p.MessagesProcessed < 0 {
			t.Errorf("unexpected negative MessagesProcessed for %s", p.Handle)
		}
	}
}

// ─────────────────────────────────────────────
// HTTP /health handler tests
// ─────────────────────────────────────────────

// newHealthHandler は health.go の StartHealthServer 内ロジックと同じ handler を返す。
// unit test 用ヘルパー。
func newHealthHandler(state *HealthState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		snap := state.snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snap)
	})
	return mux
}

func TestHealthHandler_ResponseShape(t *testing.T) {
	h := NewHealthState("single")
	h.EnsurePersona("@handler-test")
	h.RecordMessage("@handler-test")
	h.SetSessionAlive("@handler-test", true)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	newHealthHandler(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("want Content-Type application/json, got %q", ct)
	}

	var result healthSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Status != "ok" {
		t.Errorf("want status ok, got %q", result.Status)
	}
	if result.Mode != "single" {
		t.Errorf("want mode single, got %q", result.Mode)
	}
	if len(result.Personas) != 1 {
		t.Fatalf("want 1 persona, got %d", len(result.Personas))
	}
	p := result.Personas[0]
	if p.Handle != "@handler-test" {
		t.Errorf("want handle @handler-test, got %q", p.Handle)
	}
	if p.MessagesProcessed != 1 {
		t.Errorf("want MessagesProcessed=1, got %d", p.MessagesProcessed)
	}
	if !p.SessionAlive {
		t.Error("want SessionAlive=true")
	}
}

func TestHealthHandler_FleetMultiPersona(t *testing.T) {
	h := NewHealthState("fleet")
	h.EnsurePersona("@writer")
	h.EnsurePersona("@researcher")
	h.RecordMessage("@writer")
	h.RecordMessage("@researcher")
	h.RecordMessage("@researcher")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	newHealthHandler(h).ServeHTTP(rec, req)

	var result healthSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Mode != "fleet" {
		t.Errorf("want mode fleet, got %q", result.Mode)
	}
	if len(result.Personas) != 2 {
		t.Fatalf("want 2 personas, got %d", len(result.Personas))
	}
	// sorted: @researcher < @writer
	if result.Personas[0].Handle != "@researcher" {
		t.Errorf("want @researcher first, got %q", result.Personas[0].Handle)
	}
	if result.Personas[0].MessagesProcessed != 2 {
		t.Errorf("want researcher=2, got %d", result.Personas[0].MessagesProcessed)
	}
	if result.Personas[1].MessagesProcessed != 1 {
		t.Errorf("want writer=1, got %d", result.Personas[1].MessagesProcessed)
	}
}

// TestStartHealthServer_PortZero は port=0 で StartHealthServer が何もせず
// panic しないことを確認する。
func TestStartHealthServer_PortZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := NewHealthState("single")
	// no-op: port=0 → no goroutine started
	StartHealthServer(ctx, 0, h)
}

// TestStartHealthServer_Live は実際に HTTP サーバーを起動して /health を叩く。
// StartHealthServer が固定ポートを要求するため、同等の handler を httptest.Server で
// テストすることでポート競合を回避する。
func TestStartHealthServer_Live(t *testing.T) {
	h := NewHealthState("single")
	h.EnsurePersona("@live-test")
	h.RecordMessage("@live-test")

	srv := httptest.NewServer(newHealthHandler(h))
	defer srv.Close()

	resp, err := http.Get(fmt.Sprintf("%s/health", srv.URL))
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result healthSnapshot
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Status != "ok" {
		t.Errorf("want status ok, got %q", result.Status)
	}
	if len(result.Personas) != 1 || result.Personas[0].MessagesProcessed != 1 {
		t.Errorf("unexpected personas: %+v", result.Personas)
	}
}
