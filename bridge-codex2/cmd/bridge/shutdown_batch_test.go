package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestRunHubSession_ShutdownLeavesRestUnread は、バッチの 1 便目を処理中に停止したとき、
// 2 便目以降を MarkAsRead も cursor 前進もせずに残し、graceful drain で処理することを
// startup catchup / polling loop の両経路で検証する (issue #342)。
// 1 便目は cursor を進め、再処理しない (中断通知 1 通のみ)。
func TestRunHubSession_ShutdownLeavesRestUnread(t *testing.T) {
	cases := []struct {
		name string
		// inbox は get_messages の返す順。最後の 1 回は drain が取る (hub は未読の m2・m3 を返す)。
		inbox func(batch, rest string) []string
	}{
		{
			name:  "startup catchup",
			inbox: func(batch, rest string) []string { return []string{batch, rest} },
		},
		{
			name:  "polling loop",
			inbox: func(batch, rest string) []string { return []string{"[]", batch, rest} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(cursorFileEnv, filepath.Join(dir, "cursor.json"))
			t.Setenv(journalDirEnv, filepath.Join(dir, "journals"))
			logPath := filepath.Join(dir, "dispatched.log")

			// 本文を記録し、m1 だけは停止されるまで返さない codex
			codex := filepath.Join(dir, "fake-codex")
			script := fmt.Sprintf("#!/bin/sh\nb=$(grep -o 'body [a-z0-9]*' | head -n 1)\necho \"$b\" >> '%s'\n"+
				"[ \"$b\" = 'body m1' ] && exec sleep 30\necho '{\"type\":\"turn.completed\"}'\n", logPath)
			if err := os.WriteFile(codex, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			m1 := msgAt("m1", "2026-09-16T09:00:01.000Z")
			m2 := msgAt("m2", "2026-09-16T09:00:02.000Z")
			m3 := msgAt("m3", "2026-09-16T09:00:03.000Z")
			hub := newMockHub(t, tc.inbox(inboxJSON(t, m1, m2, m3), inboxJSON(t, m2, m3))...)
			cfg := &config{
				User:              "codex-test",
				AgentHubURL:       hub.srv.URL,
				GitHubPAT:         "ghp_test",
				Mode:              "stateful",
				Workdir:           dir,
				CodexCLI:          codex,
				CodexHomeDir:      dir,
				PollInterval:      time.Hour,
				SubprocessTimeout: 10 * time.Second,
				ScannerBufferSize: 64 * 1024,
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _, _ = runHubSession(ctx, cfg, newCodexRunner(cfg), cursorPos{},
					&activityTracker{}, &messageGapTracker{}, newJournal(cfg.User))
			}()

			deadline := time.Now().Add(5 * time.Second)
			for len(readDispatched(t, logPath)) < 1 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()

			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("runHubSession did not return after ctx cancel")
			}

			if got, want := readDispatched(t, logPath), []string{"m1", "m2", "m3"}; !reflect.DeepEqual(got, want) {
				t.Errorf("dispatched = %v, want %v (m2, m3 must be processed by drain, not dropped)", got, want)
			}
			want := cursorPos{TS: "2026-09-16T09:00:03.000Z", IDs: []string{"m3"}}
			if saved := loadCursor(cfg.User); !reflect.DeepEqual(saved, want) {
				t.Errorf("saved cursor = %+v, want %+v", saved, want)
			}
			// 送り主への返信は m1 の中断通知 1 通だけ (m2・m3 に偽エラー・中断通知を出さない)
			var causedBy []string
			for _, s := range hub.sentMessages() {
				causedBy = append(causedBy, s.CausedBy)
			}
			if !reflect.DeepEqual(causedBy, []string{"m1"}) {
				t.Errorf("sent caused_by = %v, want [m1] (interrupt notice for m1 only)", causedBy)
			}
		})
	}
}
