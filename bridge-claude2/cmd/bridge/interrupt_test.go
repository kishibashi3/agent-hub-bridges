package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSleepingFakeClaude は initialize と user message を読んだ後、result を出さずに眠り続ける
// claude CLI 代替を書く (作業中に停止されるケースの再現用)。
func writeSleepingFakeClaude(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude-sleep")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nread -r _init\nread -r _msg\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// handleOneCancelledAfter は handleOne を回し、delay 後に ctx を cancel して終了を待つ。
func handleOneCancelledAfter(t *testing.T, body string, delay time.Duration) (*mockHub, *Journal) {
	t.Helper()
	client, cfg, runner, hub, journal := newTestEnv(t, writeSleepingFakeClaude(t, t.TempDir()))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(delay, cancel)
	done := make(chan error, 1)
	go func() { done <- handleOne(ctx, client, runner, inbound(body), cfg, &activityTracker{}, journal) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("handleOne did not return after ctx cancel")
	}
	return hub, journal
}

// TestHandleOne_ShutdownSendsOneInterruptNotice: 作業中に停止したら、エラーではなく中断通知を
// ちょうど 1 通、caused_by 付きで送り主へ返す。送信に成功した通知は journal に残さない (issue #337)。
func TestHandleOne_ShutdownSendsOneInterruptNotice(t *testing.T) {
	hub, journal := handleOneCancelledAfter(t, "hello", 300*time.Millisecond)

	sends := hub.callsNamed("send_message")
	if len(sends) != 1 {
		t.Fatalf("send_message called %d times; want exactly 1 interrupt notice", len(sends))
	}
	body, _ := sends[0].Args["message"].(string)
	if !strings.HasPrefix(body, autoNoticePrefix) || strings.HasPrefix(body, autoErrorPrefix) {
		t.Errorf("message = %q, want an interrupt notice (not an error)", body)
	}
	if to, _ := sends[0].Args["to"].(string); to != "@peer" {
		t.Errorf("to = %q, want @peer", to)
	}
	if cb, _ := sends[0].Args["caused_by"].(string); cb != "msg-1" {
		t.Errorf("caused_by = %q, want msg-1", cb)
	}
	if entries := journal.loadAll(); len(entries) != 0 {
		t.Errorf("journal must be empty after a successful send (else replayed as a 2nd reply), got %d", len(entries))
	}
}

// TestHandleOne_ShutdownNoNoticeForAutoReply: auto 返信への中断通知は送らない (往復防止)。
func TestHandleOne_ShutdownNoNoticeForAutoReply(t *testing.T) {
	hub, _ := handleOneCancelledAfter(t, autoErrorPrefix+" something broke", 300*time.Millisecond)
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("send_message called %d times; want 0", n)
	}
}
