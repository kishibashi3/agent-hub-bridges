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

// stoppedCtx は「停止が掛かった」状態の ctx。Err() は Canceled を返すが Done() は閉じない
// (Background のまま) ので、fake claude は kill されずに result まで出せる。実機では stop の
// SIGTERM が内側 claude にも届き、claude が is_error=false の result で終わる (issue #337)。
type stoppedCtx struct{ context.Context }

func (stoppedCtx) Err() error { return context.Canceled }

// TestHandleOne_ShutdownWithNonErrorResultSendsOneInterruptNotice: 停止中に内側 claude が
// is_error=false の result で終わり、この依頼に返信していなければ、中断通知を 1 通返す (issue #337)。
func TestHandleOne_ShutdownWithNonErrorResultSendsOneInterruptNotice(t *testing.T) {
	client, cfg, runner, hub, journal := newTestEnv(t, writeFakeClaude(t, t.TempDir(), okResultLine(), 0))
	if err := handleOne(stoppedCtx{context.Background()}, client, runner, inbound("hello"), cfg, &activityTracker{}, journal); err != nil {
		t.Fatalf("handleOne: %v", err)
	}
	sends := hub.callsNamed("send_message")
	if len(sends) != 1 {
		t.Fatalf("send_message called %d times; want exactly 1 interrupt notice", len(sends))
	}
	if body, _ := sends[0].Args["message"].(string); !strings.HasPrefix(body, autoNoticePrefix) {
		t.Errorf("message = %q, want an interrupt notice", body)
	}
	if cb, _ := sends[0].Args["caused_by"].(string); cb != "msg-1" {
		t.Errorf("caused_by = %q, want msg-1", cb)
	}
	if entries := journal.loadAll(); len(entries) != 0 {
		t.Errorf("journal must be empty after a successful send, got %d", len(entries))
	}
}

// TestHandleOne_ShutdownAfterInnerReplyNoNotice: 内側がこの依頼に返信済みなら、停止中に
// 終わっても中断通知は送らない (返信は 1 通のまま)。
func TestHandleOne_ShutdownAfterInnerReplyNoNotice(t *testing.T) {
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__agent-hub__send_message","input":{"to":"@peer","message":"x","caused_by":"msg-1"}}]}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false}]}}` + "\n" +
		okResultLine()
	client, cfg, runner, hub, journal := newTestEnv(t, writeFakeClaude(t, t.TempDir(), lines, 0))
	if err := handleOne(stoppedCtx{context.Background()}, client, runner, inbound("hello"), cfg, &activityTracker{}, journal); err != nil {
		t.Fatalf("handleOne: %v", err)
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("send_message called %d times by bridge; want 0 (inner already replied)", n)
	}
}

// TestHandleOne_NonErrorResultWithoutShutdownNoNotice: 停止が掛かっていなければ、従来どおり
// bridge からは何も送らない (中断していない依頼の返信数は変わらない)。
func TestHandleOne_NonErrorResultWithoutShutdownNoNotice(t *testing.T) {
	client, cfg, runner, hub, journal := newTestEnv(t, writeFakeClaude(t, t.TempDir(), okResultLine(), 0))
	if err := handleOne(context.Background(), client, runner, inbound("hello"), cfg, &activityTracker{}, journal); err != nil {
		t.Fatalf("handleOne: %v", err)
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("send_message called %d times; want 0", n)
	}
}
