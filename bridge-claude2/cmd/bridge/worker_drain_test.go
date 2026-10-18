package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────── //
// 停止時の graceful drain の回帰テスト (issue #355)                         //
// ──────────────────────────────────────────────────────────────────────── //

// writeSlowFakeClaude は、user message に slowMarker を含む依頼では result を出さずに眠り、
// それ以外 (/compact 等) では okResultLine を返す claude CLI 代替を書く。受け取った
// user message 行は logPath に追記する。
func writeSlowFakeClaude(t *testing.T, dir, logPath, slowMarker string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude-slow")
	script := fmt.Sprintf("#!/bin/sh\nread -r _init\nread -r msg\nprintf '%%s\\n' \"$msg\" >> '%s'\ncase \"$msg\" in *%s*) exec sleep 30;; esac\nprintf '%%s\\n' '%s'\n",
		logPath, slowMarker, okResultLine())
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runHubSessionUntilCancel は runHubSession を回し、ready が true を返したら ctx を cancel して
// 終了を待つ。runHubSession が出したログを返す。
func runHubSessionUntilCancel(t *testing.T, cfg *config, runner *claudeRunner, journal *Journal, ready func() bool) string {
	t.Helper()
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := runHubSession(ctx, cfg, "", runner, cursorPos{}, &activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{})
		done <- err
	}()

	deadline := time.Now().Add(15 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("runHubSession did not reach the expected state; log:\n%s", logBuf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runHubSession err = %v; want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runHubSession did not return after cancel")
	}
	return logBuf.String()
}

// assertLogOrder は want の各行が log にこの順で出ていることを確かめる。
func assertLogOrder(t *testing.T, log string, want ...string) {
	t.Helper()
	rest := log
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("log line %q not found (in order %q); log:\n%s", w, want, log)
		}
		rest = rest[i+len(w):]
	}
}

// TestRunHubSession_ShutdownWhileProcessingRunsDrain: 処理中に停止したとき、中断通知のあとに
// 必ず graceful drain が走る (issue #355)。
//
// poll 間隔を 10ms にして、処理を終えて select に戻った時点で ctx.Done() と pollTicker.C が
// 必ず同時に準備完了になるようにする。修正前は select がランダムに pollTicker.C を選ぶと
// キャンセル済み ctx で GetMessages を呼んで drain を飛ばしていた (1 回あたり約 1/2)。
// 10 回繰り返して、毎回 drain が走ることを確かめる。
func TestRunHubSession_ShutdownWhileProcessingRunsDrain(t *testing.T) {
	t.Setenv(inboxPollIntervalEnv, "0.01")
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprintf("run%d", i), func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "claude-prompts.log")
			_, cfg, runner, hub, journal := newTestEnv(t, writeSlowFakeClaude(t, dir, logPath, "slow-body"))
			cfg.AgentHubURL = hub.srv.URL
			cfg.GitHubPAT = "ghp_test"
			hub.setInbox(
				`[{"id":"s1","from":"@peer","to":"@limit-test","message":"slow-body","timestamp":"2026-10-19T09:00:01.000Z"}]`,
			)

			log := runHubSessionUntilCancel(t, cfg, runner, journal, func() bool {
				data, _ := os.ReadFile(logPath)
				return strings.Contains(string(data), "slow-body")
			})

			assertLogOrder(t, log,
				"sendInterruptNotice: sent interrupt notice",
				"[drain] graceful drain started",
				"[drain] /compact completed",
				"[drain] no pending messages, exiting cleanly",
			)
			if strings.Contains(log, "runHubSession: get_messages error") {
				t.Errorf("GetMessages must not be called with the cancelled ctx; log:\n%s", log)
			}
		})
	}
}

// TestRunHubSession_ShutdownWhileIdleRunsDrain: 待機中の停止でも、これまでどおり drain が走る。
func TestRunHubSession_ShutdownWhileIdleRunsDrain(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "claude-prompts.log")
	_, cfg, runner, hub, journal := newTestEnv(t, writeSlowFakeClaude(t, dir, logPath, "slow-body"))
	cfg.AgentHubURL = hub.srv.URL
	cfg.GitHubPAT = "ghp_test"

	log := runHubSessionUntilCancel(t, cfg, runner, journal, func() bool {
		// startup catchup の get_messages が終わり、select で待っている状態
		return len(hub.callsNamed("get_messages")) >= 1
	})

	assertLogOrder(t, log,
		"[drain] graceful drain started",
		"[drain] /compact completed",
		"[drain] no pending messages, exiting cleanly",
	)
}
