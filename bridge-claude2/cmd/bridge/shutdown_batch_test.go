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
		&activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{}, msgs, "test")

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
