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
// 上限で止めてはいないので terminating の Warn は出ない。stdout を握っている子孫は claude の
// プロセスグループごと SIGKILL し、そのことを Warn で残す (issue #383)。
func TestQuery_ExitedWithStdoutHeldByDescendant_ReturnsWithoutLimit(t *testing.T) {
	withResultExitLimit(t, 5*time.Second, 2*time.Second)
	logBuf := captureSlog(t)

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	// stdout を引き継いだ子を残して claude 本体は終わる
	script := writeAfterResultFakeClaude(t, fmt.Sprintf("sleep 30 &\necho $! > '%s'\nexit 0", pidPath))

	elapsed, err := runQueryWithTimeout(t, script, time.Minute)
	child := waitPIDFile(t, pidPath)
	// query が戻った時点で子孫は kill 済み (reap 待ちの zombie は許す)
	waitGone(t, child, 200*time.Millisecond)
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
	if !strings.Contains(logs, "stdout is still open, killing its process group") {
		t.Errorf("held-stdout warn not logged:\n%s", logs)
	}
}

// TestQuery_TerminatedWithStdoutHeldByDescendant_KillsGroup: 上限で SIGTERM を送り claude は
// 終わったが、同じプロセスグループの子孫が stdout を握ったまま残る場合も、tree kill の猶予
// (subprocessTermGrace) を待たずにグループごと止めて戻る (issue #383)。
func TestQuery_TerminatedWithStdoutHeldByDescendant_KillsGroup(t *testing.T) {
	withResultExitLimit(t, 500*time.Millisecond, 2*time.Second)
	logBuf := captureSlog(t)

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	// SIGTERM を受けると子を始末せずに終わる
	script := writeAfterResultFakeClaude(t, fmt.Sprintf(
		"sleep 30 &\necho $! > '%s'\ntrap 'exit 143' TERM\nwhile :; do wait; done", pidPath))

	elapsed, err := runQueryWithTimeout(t, script, time.Minute)
	child := waitPIDFile(t, pidPath)
	waitGone(t, child, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v; want nil (result was received)", err)
	}
	if elapsed < resultExitLimit {
		t.Errorf("query took %s; want >= limit %s", elapsed, resultExitLimit)
	}
	if elapsed >= resultExitLimit+subprocessTermGrace {
		t.Errorf("query took %s; want < limit %s + grace %s (group must be killed once claude exited)",
			elapsed, resultExitLimit, subprocessTermGrace)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "did not exit after result, terminating") {
		t.Errorf("termination warn not logged:\n%s", logs)
	}
	if !strings.Contains(logs, "killing its process group") {
		t.Errorf("group kill warn not logged:\n%s", logs)
	}
}

// TestQuery_WaitExitedFails_LoggedAndLimitApplies: waitid が EINTR 以外で失敗したら (seccomp の
// ENOSYS 等)、ログに残し、claude の終了を見ずに stdout の EOF と上限だけで待つ (issue #383)。
// 失敗を「終わった」と扱うと上限なしの cmd.Wait() に進んでしまう。
func TestQuery_WaitExitedFails_LoggedAndLimitApplies(t *testing.T) {
	withResultExitLimit(t, 500*time.Millisecond, 2*time.Second)
	logBuf := captureSlog(t)
	orig := waitExitedFunc
	waitExitedFunc = func(int) error { return syscall.ENOSYS }
	t.Cleanup(func() { waitExitedFunc = orig })

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := writeAfterResultFakeClaude(t, fmt.Sprintf(
		"sleep 30 &\nchild=$!\necho $child > '%s'\ntrap 'kill $child; exit 143' TERM\nwhile :; do wait; done", pidPath))

	elapsed, err := runQueryWithTimeout(t, script, time.Minute)
	if err != nil {
		t.Fatalf("err = %v; want nil (result was received)", err)
	}
	if elapsed < resultExitLimit || elapsed > resultExitLimit+subprocessTermGrace {
		t.Errorf("query took %s; want limit %s .. limit + grace %s", elapsed, resultExitLimit, subprocessTermGrace)
	}
	waitGone(t, waitPIDFile(t, pidPath), 2*time.Second)
	logs := logBuf.String()
	if !strings.Contains(logs, "cannot wait for Claude subprocess exit") || !strings.Contains(logs, syscall.ENOSYS.Error()) {
		t.Errorf("waitid failure not logged:\n%s", logs)
	}
	if !strings.Contains(logs, "did not exit after result, terminating") {
		t.Errorf("limit did not apply:\n%s", logs)
	}
}

// TestWaitExited_NotChild_ReturnsError: 自分の子でない pid は待てないのでエラーを返す (issue #383)。
func TestWaitExited_NotChild_ReturnsError(t *testing.T) {
	if err := waitExited(os.Getppid()); !errors.Is(err, syscall.ECHILD) {
		t.Errorf("err = %v; want ECHILD", err)
	}
}

// TestQuery_ErrorResult_WaitsAfterResult: is_error の result でも result の後の待ちを通る
// (issue #383)。query は result のエラーを返し、result の後のイベントは Warn で残る。
func TestQuery_ErrorResult_WaitsAfterResult(t *testing.T) {
	withResultExitLimit(t, 5*time.Second, 2*time.Second)
	logBuf := captureSlog(t)

	path := filepath.Join(t.TempDir(), "fake-claude-error-result")
	script := "#!/bin/sh\nread -r _init\nread -r _msg\n" +
		`echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}'` + "\n" +
		`echo '{"type":"system","subtype":"status"}'` + "\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	elapsed, err := runQueryWithTimeout(t, path, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "claude result error") {
		t.Errorf("err = %v; want claude result error", err)
	}
	if elapsed >= resultExitLimit {
		t.Errorf("query took %s; want < limit %s", elapsed, resultExitLimit)
	}
	if logs := logBuf.String(); !strings.Contains(logs, "event after result") {
		t.Errorf("after-result wait did not run for is_error result:\n%s", logs)
	}
}

// TestQuery_ParentCancelAfterResult_StopsWithoutLimit: result の後の待ちの最中に親 ctx が
// cancel されたら (bridge の停止)、上限を待たずに SIGTERM で止めて戻る (issue #383)。
func TestQuery_ParentCancelAfterResult_StopsWithoutLimit(t *testing.T) {
	withResultExitLimit(t, 5*time.Second, 2*time.Second)
	logBuf := captureSlog(t)

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	script := writeAfterResultFakeClaude(t, fmt.Sprintf(
		"sleep 30 &\nchild=$!\necho $child > '%s'\ntrap 'kill $child; exit 143' TERM\nwhile :; do wait; done", pidPath))

	dir := t.TempDir()
	runner := &claudeRunner{
		cfg:           &config{Workdir: dir, ClaudeCLI: script, ScannerBufferSize: 64 * 1024},
		mcpConfigPath: filepath.Join(dir, "mcp.json"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// pid ファイルは result の後に書かれる
		for {
			if _, err := os.Stat(pidPath); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	usage, err := runner.query(ctx, "prompt", "sess", &activityTracker{})
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("err = %v; want nil (result was received)", err)
	}
	if !usage.ResultReceived {
		t.Errorf("ResultReceived = false; want true")
	}
	if elapsed >= resultExitLimit {
		t.Errorf("query took %s; want < limit %s (parent cancel must stop claude)", elapsed, resultExitLimit)
	}
	waitGone(t, waitPIDFile(t, pidPath), 2*time.Second)
	if logs := logBuf.String(); strings.Contains(logs, "did not exit after result, terminating") {
		t.Errorf("stopped by limit, not by parent cancel:\n%s", logs)
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
