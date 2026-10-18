package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer は slog の出力を goroutine 間で安全に集める (drain の goroutine も書くため)。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	var logBuf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logBuf
}

const fakeResultLine = `{"type":"result","subtype":"success","is_error":false,"result":"STARTED"}`

// writeAfterResultFakeClaude は initialize と user message を読んで result を出した後、
// afterResult を実行する claude CLI 代替を書く。
func writeAfterResultFakeClaude(t *testing.T, afterResult string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude-after-result")
	script := fmt.Sprintf("#!/bin/sh\nread -r _init\nread -r _msg\necho '%s'\n%s\n", fakeResultLine, afterResult)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func withResultExitLimit(t *testing.T, limit, grace time.Duration) {
	t.Helper()
	origLimit, origGrace := resultExitLimit, subprocessTermGrace
	resultExitLimit, subprocessTermGrace = limit, grace
	t.Cleanup(func() { resultExitLimit, subprocessTermGrace = origLimit, origGrace })
}

// TestQuery_ResultThenLingering_TerminatedAfterLimit: result の後も claude が終わらない
// (run_in_background のタスクが残っている) とき、query は上限で claude を止めて戻る (issue #374)。
// result は届いているので err は nil のまま。止めたことは Warn で残る。
func TestQuery_ResultThenLingering_TerminatedAfterLimit(t *testing.T) {
	withResultExitLimit(t, 500*time.Millisecond, 2*time.Second)
	logBuf := captureSlog(t)

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	// stdin が閉じても終わらず、バックグラウンドのタスクを待ち続ける。SIGTERM でタスクを止めて終わる。
	script := writeAfterResultFakeClaude(t, fmt.Sprintf(
		"sleep 30 &\nchild=$!\necho $child > '%s'\ntrap 'kill $child; exit 143' TERM\nwhile :; do wait; done", pidPath))

	elapsed, err := runQueryWithTimeout(t, script, time.Minute)
	if err != nil {
		t.Fatalf("err = %v; want nil (result was received)", err)
	}
	if elapsed < resultExitLimit {
		t.Errorf("query took %s; want >= limit %s (must not terminate before the limit)", elapsed, resultExitLimit)
	}
	if elapsed > resultExitLimit+subprocessTermGrace {
		t.Errorf("query took %s; want <= limit %s + grace %s", elapsed, resultExitLimit, subprocessTermGrace)
	}
	waitGone(t, waitPIDFile(t, pidPath), 2*time.Second)
	if !strings.Contains(logBuf.String(), "did not exit after result, terminating") {
		t.Errorf("termination warn not logged:\n%s", logBuf.String())
	}
}

// TestQuery_EventsAfterResult_LoggedAsWarn: result の後に claude が 2 回目のターンを始めたら、
// その出力を Warn で残す (issue #374)。ツールを使っていればツール名も残る。
func TestQuery_EventsAfterResult_LoggedAsWarn(t *testing.T) {
	withResultExitLimit(t, 5*time.Second, 2*time.Second)
	logBuf := captureSlog(t)

	script := writeAfterResultFakeClaude(t, strings.Join([]string{
		`echo '{"type":"system","subtype":"init"}'`,
		`echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__agent-hub__send_message","input":{}}]}}'`,
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"second"}'`,
	}, "\n"))

	elapsed, err := runQueryWithTimeout(t, script, time.Minute)
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if elapsed >= resultExitLimit {
		t.Errorf("query took %s; claude exited by itself, should not wait for limit %s", elapsed, resultExitLimit)
	}
	logs := logBuf.String()
	if got := strings.Count(logs, "event after result"); got != 3 {
		t.Errorf("event-after-result warns = %d; want 3\n%s", got, logs)
	}
	if !strings.Contains(logs, "mcp__agent-hub__send_message") {
		t.Errorf("tool name of second turn not logged:\n%s", logs)
	}
	if strings.Contains(logs, "terminating") {
		t.Errorf("claude exited by itself; should not be terminated:\n%s", logs)
	}
}

// TestQuery_LargeOutputAfterResult_DoesNotBlock: result の後の出力がパイプのバッファ (64KB) を
// 超えても、stdout を読み続けるので claude の書き込みは止まらず、上限を待たずに戻る (issue #374)。
func TestQuery_LargeOutputAfterResult_DoesNotBlock(t *testing.T) {
	withResultExitLimit(t, 5*time.Second, 2*time.Second)
	_ = captureSlog(t)

	// 1 行 ~1KB の assistant イベントを 300 行 (~300KB) 出してから終わる
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 1000) + `"}]}}`
	script := writeAfterResultFakeClaude(t, fmt.Sprintf("i=0\nwhile [ $i -lt 300 ]; do echo '%s'; i=$((i+1)); done", line))

	elapsed, err := runQueryWithTimeout(t, script, time.Minute)
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if elapsed >= resultExitLimit {
		t.Errorf("query took %s; want < limit %s (stdout must keep being read)", elapsed, resultExitLimit)
	}
}

// TestQuery_ExitedWithStdoutHeldByDescendant_ReturnsWithoutLimit: result の後に claude が
// 終わっていれば、子孫が stdout を握っていても上限を待たずに戻る (issue #378)。
// 止めてはいないので terminating の Warn は出ず、stdout が残っていることを Warn で残す。
func TestQuery_ExitedWithStdoutHeldByDescendant_ReturnsWithoutLimit(t *testing.T) {
	withResultExitLimit(t, 5*time.Second, 2*time.Second)
	logBuf := captureSlog(t)

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	// stdout を引き継いだ子を残して claude 本体は終わる
	script := writeAfterResultFakeClaude(t, fmt.Sprintf("sleep 30 &\necho $! > '%s'\nexit 0", pidPath))

	elapsed, err := runQueryWithTimeout(t, script, time.Minute)
	child := waitPIDFile(t, pidPath)
	_ = syscall.Kill(child, syscall.SIGKILL)
	if err != nil {
		t.Fatalf("err = %v; want nil", err)
	}
	if elapsed >= resultExitLimit {
		t.Errorf("query took %s; claude exited, should not wait for limit %s", elapsed, resultExitLimit)
	}
	logs := logBuf.String()
	if strings.Contains(logs, "terminating") {
		t.Errorf("claude exited by itself; should not be terminated:\n%s", logs)
	}
	if !strings.Contains(logs, "stdout is still open") {
		t.Errorf("held-stdout warn not logged:\n%s", logs)
	}
}

// TestQuery_TimeoutBeforeResult_NoAfterResultLogs: result を受け取る前に SubprocessTimeout で
// 止めた経路では、result の後の待ち・ログを通さない (issue #378)。SIGTERM を受けた claude が
// 終わる前に出したイベントを after-result として Warn しない。
func TestQuery_TimeoutBeforeResult_NoAfterResultLogs(t *testing.T) {
	withResultExitLimit(t, 5*time.Second, 2*time.Second)
	logBuf := captureSlog(t)

	path := filepath.Join(t.TempDir(), "fake-claude-no-result")
	ev := `{"type":"system","subtype":"status"}`
	script := fmt.Sprintf("#!/bin/sh\nread -r _init\nread -r _msg\nsleep 30 &\nc=$!\n"+
		"trap 'kill $c; echo '\\''%[1]s'\\''; echo '\\''%[1]s'\\''; exit 143' TERM\nwait\n", ev)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	elapsed, err := runQueryWithTimeout(t, path, 500*time.Millisecond)
	if !errors.Is(err, errSubprocessTimeout) {
		t.Errorf("err = %v; want errSubprocessTimeout", err)
	}
	if elapsed >= resultExitLimit {
		t.Errorf("query took %s; want < limit %s", elapsed, resultExitLimit)
	}
	logs := logBuf.String()
	if strings.Contains(logs, "after result") || strings.Contains(logs, "stdout is still open") {
		t.Errorf("after-result wait/log ran without a result:\n%s", logs)
	}
}
