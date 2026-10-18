package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeToolRunningFakeClaude は initialize と user message を読んだ後、Bash ツールのように
// setsid で子を起動し、その pid を pidPath に書いて待ち続ける claude CLI 代替を書く。
// onTerm は SIGTERM の trap に渡すコマンド (claude 本体の後始末を模す)。"" なら SIGTERM を無視する。
func writeToolRunningFakeClaude(t *testing.T, dir, pidPath, onTerm string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude-tool")
	script := fmt.Sprintf("#!/bin/sh\ntrap '%s' TERM\nread -r _init\nread -r _msg\nsetsid sleep 30 &\nchild=$!\necho $child > '%s'\nwhile :; do wait; done\n",
		onTerm, pidPath)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitPIDFile は fake claude が子の pid を書くまで待つ。
func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child pid not written to %s", path)
	return 0
}

// waitGone は pid が消える (または zombie になる) まで待つ。残っていれば kill して失敗にする。
func waitGone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if st := procState(pid); st == "" || st == "Z" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("tool child %d still alive (orphaned)", pid)
}

// runQueryWithTimeout は SubprocessTimeout 付きで runner.query を回し、経過時間とエラーを返す。
func runQueryWithTimeout(t *testing.T, script string, timeout time.Duration) (time.Duration, error) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config{
		Workdir:           dir,
		ClaudeCLI:         script,
		SubprocessTimeout: timeout,
		ScannerBufferSize: 64 * 1024,
	}
	runner := &claudeRunner{cfg: cfg, mcpConfigPath: filepath.Join(dir, "mcp.json")}
	start := time.Now()
	_, err := runner.query(context.Background(), "run the long job", "sess", &activityTracker{})
	return time.Since(start), err
}

func requireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
}

// TestQuery_SubprocessTimeout_SIGTERMLetsClaudeCleanUp: SubprocessTimeout で止めるとき、claude に
// SIGTERM を送って自分でツールの子を始末させる (issue #344)。SIGKILL だと setsid 済みの子が
// 親を失って残る。猶予 (tree kill) を待たずに終わることも確かめる。
func TestQuery_SubprocessTimeout_SIGTERMLetsClaudeCleanUp(t *testing.T) {
	requireSetsid(t)
	orig := subprocessTermGrace
	subprocessTermGrace = 5 * time.Second
	t.Cleanup(func() { subprocessTermGrace = orig })

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := writeToolRunningFakeClaude(t, t.TempDir(), pidPath, `kill $child; exit 143`)

	type result struct {
		elapsed time.Duration
		err     error
	}
	done := make(chan result, 1)
	go func() {
		elapsed, err := runQueryWithTimeout(t, script, 500*time.Millisecond)
		done <- result{elapsed, err}
	}()
	child := waitPIDFile(t, pidPath)
	r := <-done

	if !errors.Is(r.err, errSubprocessTimeout) {
		t.Errorf("err = %v; want errSubprocessTimeout", r.err)
	}
	if r.elapsed >= subprocessTermGrace {
		t.Errorf("query took %s; want < grace %s (claude should exit on SIGTERM)", r.elapsed, subprocessTermGrace)
	}
	waitGone(t, child, 2*time.Second)
}

// TestQuery_SubprocessTimeout_KillsTreeWhenSIGTERMIgnored: claude が SIGTERM で終わらなければ、
// 猶予の後に子孫ごと SIGKILL する (issue #344)。
func TestQuery_SubprocessTimeout_KillsTreeWhenSIGTERMIgnored(t *testing.T) {
	requireSetsid(t)
	orig := subprocessTermGrace
	subprocessTermGrace = 300 * time.Millisecond
	t.Cleanup(func() { subprocessTermGrace = orig })

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := writeToolRunningFakeClaude(t, t.TempDir(), pidPath, "")

	type result struct {
		elapsed time.Duration
		err     error
	}
	done := make(chan result, 1)
	go func() {
		elapsed, err := runQueryWithTimeout(t, script, 500*time.Millisecond)
		done <- result{elapsed, err}
	}()
	child := waitPIDFile(t, pidPath)
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatal("query did not return after SubprocessTimeout")
	}

	if !errors.Is(r.err, errSubprocessTimeout) {
		t.Errorf("err = %v; want errSubprocessTimeout", r.err)
	}
	waitGone(t, child, 2*time.Second)
}
