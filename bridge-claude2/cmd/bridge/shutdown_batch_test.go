package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// TestProcessMessages_ShutdownLeavesRestUnread は、バッチの 1 便目を処理中に停止したとき、
// 2 便目以降を MarkAsRead も cursor 前進もせずに残すことを検証する (issue #342)。
// 1 便目は cursor を進め、再処理しない。
func TestProcessMessages_ShutdownLeavesRestUnread(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatched.log")
	// user message を記録してから、停止されるまで result を返さない claude
	script := filepath.Join(dir, "fake-claude")
	body := fmt.Sprintf("#!/bin/sh\nread -r _init\nread -r msg\nprintf '%%s\\n' \"$msg\" >> '%s'\nexec sleep 30\n", logPath)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	client, cfg, runner, hub, journal := newTestEnv(t, script)

	msgs := []agenthub.Message{
		msgAt("m1", "2026-09-16T09:00:01.000Z"),
		msgAt("m2", "2026-09-16T09:00:02.000Z"),
		msgAt("m3", "2026-09-16T09:00:03.000Z"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(logPath); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()

	cursor := processMessages(ctx, cfg, client, runner, nil, cursorPos{},
		&activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{}, msgs, nil, "test")

	if n := countLines(t, logPath); n != 1 {
		t.Fatalf("dispatched %d messages; want 1 (m2, m3 must not be dispatched after shutdown)", n)
	}
	want := cursorPos{TS: "2026-09-16T09:00:01.000Z", IDs: []string{"m1"}}
	if !reflect.DeepEqual(cursor, want) {
		t.Errorf("cursor = %+v, want %+v", cursor, want)
	}
	if saved := loadCursor(cfg.stateKey()); !reflect.DeepEqual(saved, want) {
		t.Errorf("saved cursor = %+v, want %+v", saved, want)
	}
	var marked []any
	for _, c := range hub.callsNamed("mark_as_read") {
		marked = append(marked, c.Args["message_id"])
	}
	if !reflect.DeepEqual(marked, []any{"m1"}) {
		t.Errorf("mark_as_read = %v, want [m1] (m2, m3 must stay unread for drain / next startup)", marked)
	}
	// drain / 次回起動で m2・m3 が cursor.seen で捨てられないこと
	for _, m := range msgs[1:] {
		if cursor.seen(m) {
			t.Errorf("%s must not be seen after shutdown", m.ID)
		}
	}
}

// limitResumeDeferred は limit 復帰時に processMessages に渡す deferred と markedRead を、
// limitSleeper の enter → wake で作る (d1 が limit に当たった便、d2 は MarkAsRead 前の便)。
func limitResumeDeferred() ([]agenthub.Message, map[string]bool) {
	s := &limitSleeper{}
	s.enter(&limitReachedError{Kind: "spend limit", Until: time.Now().Add(time.Hour)}, []agenthub.Message{
		{ID: "d1", Sender: "@a", To: "@limit-test", Body: "first", Timestamp: "2026-09-16T09:00:01.000Z"},
		{ID: "d2", Sender: "@b", To: "@limit-test", Body: "second", Timestamp: "2026-09-16T09:00:02.000Z"},
	})
	return s.wake()
}

// TestProcessMessages_ShutdownBeforeLimitResume_NotifiesMarkedRead は、limit 復帰後の deferred に
// 手を付ける前に停止したとき、MarkAsRead 済みの d1 の送り主へ中断通知を 1 通送り、
// MarkAsRead 前の d2 は触らずに残すことを検証する (issue #369)。
// d1 は drain / 次回起動の GetMessages に出てこないので、通知しないと返信 0 通で消える。
func TestProcessMessages_ShutdownBeforeLimitResume_NotifiesMarkedRead(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatched.log")
	client, cfg, runner, hub, journal := newTestEnv(t, writeRecordingFakeClaude(t, dir, logPath, okResultLine(), 0))
	deferred, markedRead := limitResumeDeferred()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cursor := processMessages(ctx, cfg, client, runner, nil, cursorPos{},
		&activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{}, deferred, markedRead, "[limit-resume]")

	if _, err := os.Stat(logPath); err == nil {
		t.Errorf("no message must be dispatched to claude after shutdown")
	}
	sends := hub.callsNamed("send_message")
	if len(sends) != 1 {
		t.Fatalf("send_message called %d times; want 1 interrupt notice for d1", len(sends))
	}
	if to, _ := sends[0].Args["to"].(string); to != "@a" {
		t.Errorf("to = %q, want @a", to)
	}
	if cb, _ := sends[0].Args["caused_by"].(string); cb != "d1" {
		t.Errorf("caused_by = %q, want d1", cb)
	}
	if body, _ := sends[0].Args["message"].(string); body != interruptNoticeBody {
		t.Errorf("message = %q, want interrupt notice", body)
	}
	if n := len(hub.callsNamed("mark_as_read")); n != 0 {
		t.Errorf("mark_as_read called %d times; want 0 (d2 must stay unread for drain / next startup)", n)
	}
	if cursor.seen(deferred[1]) {
		t.Error("d2 must not be seen after shutdown")
	}
}

// TestProcessMessages_ShutdownDuringLimitResume_NoDuplicateNotice は、limit 復帰後に d1 を
// 処理中に停止したとき、d1 への中断通知は handleOne の 1 通だけで、MarkAsRead 前の d2 には
// 通知も MarkAsRead もしないことを検証する (issue #369)。
func TestProcessMessages_ShutdownDuringLimitResume_NoDuplicateNotice(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatched.log")
	script := filepath.Join(dir, "fake-claude")
	body := fmt.Sprintf("#!/bin/sh\nread -r _init\nread -r msg\nprintf '%%s\\n' \"$msg\" >> '%s'\nexec sleep 30\n", logPath)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	client, cfg, runner, hub, journal := newTestEnv(t, script)
	deferred, markedRead := limitResumeDeferred()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(logPath); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	cursor := processMessages(ctx, cfg, client, runner, nil, cursorPos{},
		&activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{}, deferred, markedRead, "[limit-resume]")

	var causedBy []any
	for _, c := range hub.callsNamed("send_message") {
		causedBy = append(causedBy, c.Args["caused_by"])
	}
	if !reflect.DeepEqual(causedBy, []any{"d1"}) {
		t.Errorf("send_message caused_by = %v, want [d1] (one interrupt notice for in-flight d1 only)", causedBy)
	}
	var marked []any
	for _, c := range hub.callsNamed("mark_as_read") {
		marked = append(marked, c.Args["message_id"])
	}
	if !reflect.DeepEqual(marked, []any{"d1"}) {
		t.Errorf("mark_as_read = %v, want [d1]", marked)
	}
	if cursor.seen(deferred[1]) {
		t.Error("d2 must not be seen after shutdown")
	}
}
