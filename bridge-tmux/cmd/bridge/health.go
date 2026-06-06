// Package main — health.go
//
// HealthState は bridge-tmux デーモン + 各 persona の健全性情報を保持する。
// HTTP /health エンドポイントで JSON として公開する。
//
// スレッドセーフ設計:
//   - 書き込み (RecordMessage / RecordError / SetSessionAlive) は mu.Lock()
//   - 読み取り (snapshot / HTTP handler) は mu.RLock()
//   - snapshot() は値コピーを返す (ポインタ共有なし)
//
// Issue: #142
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// PersonaHealth は 1 persona の健全性情報。
// snapshot() で値コピーされるため、フィールドはすべて値型または nil 可能な *time.Time。
type PersonaHealth struct {
	Handle            string     `json:"handle"`
	SessionAlive      bool       `json:"session_alive"`
	MessagesProcessed int64      `json:"messages_processed"`
	LastMessageAt     *time.Time `json:"last_message_at,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
}

// HealthState はデーモン全体 + 各 persona の健全性を保持する。
// 複数 goroutine (handleMessage / idle timer / fleet goroutines) から
// 同時アクセスされるため sync.RWMutex で保護する。
type HealthState struct {
	mu        sync.RWMutex
	startedAt time.Time
	mode      string // "single" or "fleet"
	personas  map[string]*PersonaHealth
}

// NewHealthState は HealthState を生成する。
// mode は "single" または "fleet"。
func NewHealthState(mode string) *HealthState {
	return &HealthState{
		startedAt: time.Now(),
		mode:      mode,
		personas:  make(map[string]*PersonaHealth),
	}
}

// EnsurePersona は handle の PersonaHealth が存在しなければ作成する。
// bridge 起動時に persona を事前登録するために使う。
func (h *HealthState) EnsurePersona(handle string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.personas[handle]; !ok {
		h.personas[handle] = &PersonaHealth{Handle: handle}
	}
}

// RecordMessage は 1 件のメッセージ処理成功を記録する。
func (h *HealthState) RecordMessage(handle string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.getOrCreate(handle)
	p.MessagesProcessed++
	now := time.Now()
	p.LastMessageAt = &now
	p.LastError = ""
}

// RecordError は処理エラーを記録する。
// 連続エラーの場合も LastError を上書きする。
func (h *HealthState) RecordError(handle, errMsg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.getOrCreate(handle)
	p.LastError = errMsg
}

// SetSessionAlive は session の生死状態を更新する。
// spawn 時は true、kill/idle timeout 時は false を渡す。
func (h *HealthState) SetSessionAlive(handle string, alive bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.getOrCreate(handle)
	p.SessionAlive = alive
}

// getOrCreate は mu.Lock() を保持したまま呼ぶこと (呼び出し元が Lock 取得済み前提)。
func (h *HealthState) getOrCreate(handle string) *PersonaHealth {
	p, ok := h.personas[handle]
	if !ok {
		p = &PersonaHealth{Handle: handle}
		h.personas[handle] = p
	}
	return p
}

// healthSnapshot は /health レスポンス用の値スナップショット。
type healthSnapshot struct {
	Status    string          `json:"status"`
	Mode      string          `json:"mode"`
	UptimeSec float64         `json:"uptime_s"`
	Personas  []PersonaHealth `json:"personas"`
}

// snapshot は現在の健全性情報を値コピーで返す (スレッドセーフ)。
// personas はアルファベット順にソートして決定的な出力にする。
func (h *HealthState) snapshot() healthSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()

	snap := healthSnapshot{
		Status:    "ok",
		Mode:      h.mode,
		UptimeSec: time.Since(h.startedAt).Seconds(),
		Personas:  make([]PersonaHealth, 0, len(h.personas)),
	}
	for _, p := range h.personas {
		snap.Personas = append(snap.Personas, *p) // 値コピー
	}
	sort.Slice(snap.Personas, func(i, j int) bool {
		return snap.Personas[i].Handle < snap.Personas[j].Handle
	})
	return snap
}

// StartHealthServer は HTTP /health サーバーを goroutine で起動する。
// ctx がキャンセルされると Graceful Shutdown される。
// port が 0 の場合は何もしない (health server 無効)。
func StartHealthServer(ctx context.Context, port int, state *HealthState) {
	if port == 0 {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		snap := state.snapshot()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			slog.Error("health handler: encode failed", "err", err)
		}
	})
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	go func() {
		slog.Info("health server starting", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health server error", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		slog.Info("health server stopped", "port", port)
	}()
}
