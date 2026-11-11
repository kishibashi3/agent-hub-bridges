package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain は test binary が detach の見届け役 (detach-run) として起動されたときに
// サブコマンドを実行する (runDetach は os.Executable() = test binary を起動するため)。
func TestMain(m *testing.M) {
	if code, ok := runSubcommand(os.Args[1:]); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// setDetachEnv は claude から detach を呼んだときと同じ env を立てる。
func setDetachEnv(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "detached")
	t.Setenv(detachDirEnv, dir)
	t.Setenv(detachSessionEnv, "@requester")
	t.Setenv(detachCausedByEnv, "orig-msg-1")
	return dir
}

// detachAndWait は detach で command を流し、便が出せる状態になるまで待って返す。
func detachAndWait(t *testing.T, dir string, args ...string) detachNotice {
	t.Helper()
	var out, errOut strings.Builder
	if code := runDetach(args, &out, &errOut); code != 0 {
		t.Fatalf("runDetach code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "detached: id=") {
		t.Fatalf("runDetach stdout = %q", out.String())
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ns := pendingDetachNotices(dir, time.Now()); len(ns) > 0 {
			if len(ns) != 1 {
				t.Fatalf("pending = %d, want 1", len(ns))
			}
			return ns[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("detached command did not finish in time")
	return detachNotice{}
}

func TestDetach_ExitZero_RecordsResultAndOutput(t *testing.T) {
	dir := setDetachEnv(t)
	n := detachAndWait(t, dir, "-name", "loop", "--", "for i in 1 2 3; do echo line$i; done")
	if n.Result == nil || n.Result.ExitCode != 0 || n.Result.Signal != "" || n.Result.Error != "" {
		t.Fatalf("result = %+v, want exit 0", n.Result)
	}
	if n.Record.Session != "@requester" || n.Record.CausedBy != "orig-msg-1" || n.Record.Name != "loop" {
		t.Errorf("record = %+v", n.Record)
	}
	body := n.body(time.Now())
	for _, want := range []string{"result: exit code 0", "line1", "line3", "name: loop", n.Record.LogPath} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// 終了コードが 0 でなくても便が出る (静かに消えない)。
func TestDetach_NonZeroExit_StillNotified(t *testing.T) {
	dir := setDetachEnv(t)
	n := detachAndWait(t, dir, "--", "echo boom >&2; exit 3")
	if n.Result == nil || n.Result.ExitCode != 3 {
		t.Fatalf("result = %+v, want exit 3", n.Result)
	}
	if body := n.body(time.Now()); !strings.Contains(body, "result: exit code 3") || !strings.Contains(body, "boom") {
		t.Errorf("body:\n%s", body)
	}
}

func TestDetach_Signal_StillNotified(t *testing.T) {
	dir := setDetachEnv(t)
	n := detachAndWait(t, dir, "--", "kill -TERM $$")
	if n.Result == nil || n.Result.Signal == "" {
		t.Fatalf("result = %+v, want signal", n.Result)
	}
}

// argv が 2 つ以上なら bash -c を通さずにそのまま実行する。
func TestDetach_Argv(t *testing.T) {
	dir := setDetachEnv(t)
	n := detachAndWait(t, dir, "--", "/bin/echo", "a b", "$HOME")
	if tail := readTail(n.Record.LogPath, 1000); tail != "a b $HOME" {
		t.Errorf("output = %q", tail)
	}
}

func TestDetach_StartFailure_StillNotified(t *testing.T) {
	dir := setDetachEnv(t)
	n := detachAndWait(t, dir, "--", "/nonexistent/cmd", "x")
	if n.Result == nil || n.Result.Error == "" {
		t.Fatalf("result = %+v, want start error", n.Result)
	}
}

func TestDetach_RequiresEnv(t *testing.T) {
	t.Setenv(detachDirEnv, "")
	var out, errOut strings.Builder
	if code := runDetach([]string{"--", "true"}, &out, &errOut); code == 0 {
		t.Fatal("want failure without " + detachDirEnv)
	}
	t.Setenv(detachDirEnv, t.TempDir())
	t.Setenv(detachSessionEnv, "")
	if code := runDetach([]string{"--", "true"}, &out, &errOut); code == 0 {
		t.Fatal("want failure without " + detachSessionEnv)
	}
	t.Setenv(detachSessionEnv, "@x")
	if code := runDetach(nil, &out, &errOut); code != 2 {
		t.Fatal("want usage error without command")
	}
}

// 走行中は便を出さず、終わったら出す。印を付けたら二度と出さない。
func TestPendingDetachNotices_RunningThenNotified(t *testing.T) {
	dir := setDetachEnv(t)
	gate := filepath.Join(t.TempDir(), "gate")
	var out, errOut strings.Builder
	if code := runDetach([]string{"--", fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.05; done", gate)}, &out, &errOut); code != 0 {
		t.Fatalf("runDetach: %s", errOut.String())
	}
	time.Sleep(300 * time.Millisecond)
	if ns := pendingDetachNotices(dir, time.Now()); len(ns) != 0 {
		t.Fatalf("running job must not be pending, got %+v", ns)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var n detachNotice
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ns := pendingDetachNotices(dir, time.Now()); len(ns) == 1 {
			n = ns[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n.Result == nil {
		t.Fatal("finished job must be pending")
	}
	if err := markDetachNotified(dir, n.Record.ID); err != nil {
		t.Fatal(err)
	}
	if ns := pendingDetachNotices(dir, time.Now()); len(ns) != 0 {
		t.Fatalf("notified job must not be pending again, got %d", len(ns))
	}
}

// 見届け役が終了の記録を書かずに消えたら「消えた」として便を出す。
func TestPendingDetachNotices_LostSupervisor(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	dead := 1 << 30 // pid_max (最大 4194304) を超える = 存在しない pid
	recs := []detachRecord{
		{ID: "a-dead", Argv: []string{"x"}, Session: "@s", StartedAt: now.UTC().Format(time.RFC3339Nano), SupervisorPID: dead},
		{ID: "b-nopid-old", Argv: []string{"x"}, Session: "@s", StartedAt: now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
		{ID: "c-nopid-new", Argv: []string{"x"}, Session: "@s", StartedAt: now.UTC().Format(time.RFC3339Nano)},
		{ID: "d-alive", Argv: []string{"x"}, Session: "@s", StartedAt: now.UTC().Format(time.RFC3339Nano), SupervisorPID: os.Getpid()},
	}
	if st, ok := readStartTime(os.Getpid()); ok {
		recs[3].SupervisorStart = st
	} else {
		t.Fatal("readStartTime(self) failed")
	}
	recs = append(recs, detachRecord{ID: "e-reused", Argv: []string{"x"}, Session: "@s",
		StartedAt: now.UTC().Format(time.RFC3339Nano), SupervisorPID: os.Getpid(), SupervisorStart: recs[3].SupervisorStart + 1})
	for _, r := range recs {
		if err := writeJSONFile(filepath.Join(dir, r.ID+".json"), r); err != nil {
			t.Fatal(err)
		}
	}
	ns := pendingDetachNotices(dir, now)
	var got []string
	for _, n := range ns {
		if n.Result != nil || n.Lost == "" {
			t.Errorf("%s: want lost, got %+v", n.Record.ID, n)
		}
		got = append(got, n.Record.ID)
	}
	if strings.Join(got, ",") != "a-dead,b-nopid-old,e-reused" {
		t.Errorf("lost = %v", got)
	}
	if body := ns[0].body(now); !strings.Contains(body, "result: LOST") {
		t.Errorf("body:\n%s", body)
	}
}

// 便は処理を流したときの依頼主のセッションで claude を起こし、prompt に結果を載せる。
// 同じ処理の便は 1 回だけ。claude の env には background task の無効化と detach 用の値が付く。
func TestDeliverDetachNotices_WakesRequesterSessionOnce(t *testing.T) {
	dir := setDetachEnv(t)
	n := detachAndWait(t, dir, "--", "echo payload-xyz; exit 7")

	tmp := t.TempDir()
	promptLog := filepath.Join(tmp, "prompts")
	envLog := filepath.Join(tmp, "env")
	script := filepath.Join(tmp, "fake-claude")
	body := fmt.Sprintf("#!/bin/sh\nenv > '%s'\nread -r _init\nread -r msg\nprintf '%%s\\n' \"$msg\" >> '%s'\necho '%s'\n",
		envLog, promptLog, fakeResultLine)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	client, cfg, runner, hub, journal := newTestEnv(t, script)
	cfg.DetachDir = dir
	sleeper := &limitSleeper{}

	deliverDetachNotices(context.Background(), cfg, client, runner, &activityTracker{}, journal, sleeper)
	deliverDetachNotices(context.Background(), cfg, client, runner, &activityTracker{}, journal, sleeper)

	data, err := os.ReadFile(promptLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("claude started %d times, want 1", len(lines))
	}
	for _, want := range []string{`"session_id":"@requester"`, "payload-xyz", "exit code 7", "orig-msg-1", n.Record.ID} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("prompt missing %q: %s", want, lines[0])
		}
	}
	envData, _ := os.ReadFile(envLog)
	for _, want := range []string{disableBackgroundTasksEnv, detachSessionEnv + "=@requester",
		detachCausedByEnv + "=orig-msg-1", detachDirEnv + "=" + dir, detachBinEnv + "="} {
		if !strings.Contains(string(envData), want) {
			t.Errorf("claude env missing %q", want)
		}
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("bridge itself sent %d messages; want 0 on success", n)
	}
}

// 便を受けた claude のターンが失敗したら、依頼主に自動エラー返信が届く (静かに消えない)。
func TestDeliverDetachNotices_FailureRepliesToRequester(t *testing.T) {
	dir := setDetachEnv(t)
	detachAndWait(t, dir, "--", "true")
	script := writeFakeClaude(t, t.TempDir(), genericErrorResultLine(), 1)
	client, cfg, runner, hub, journal := newTestEnv(t, script)
	cfg.DetachDir = dir

	deliverDetachNotices(context.Background(), cfg, client, runner, &activityTracker{}, journal, &limitSleeper{})

	sends := hub.callsNamed("send_message")
	if len(sends) != 1 {
		t.Fatalf("send_message = %d, want 1", len(sends))
	}
	if to, _ := sends[0].Args["to"].(string); to != "@requester" {
		t.Errorf("to = %q", to)
	}
	if cb, _ := sends[0].Args["caused_by"].(string); cb != "orig-msg-1" {
		t.Errorf("caused_by = %q", cb)
	}
}

// limit に当たったら印を外し、休眠明けに出し直す。
func TestDeliverDetachNotices_LimitUnmarks(t *testing.T) {
	dir := setDetachEnv(t)
	detachAndWait(t, dir, "--", "true")
	script := writeFakeClaude(t, t.TempDir(), limitResultLine(), 1)
	client, cfg, runner, _, journal := newTestEnv(t, script)
	cfg.DetachDir = dir
	sleeper := &limitSleeper{}

	deliverDetachNotices(context.Background(), cfg, client, runner, &activityTracker{}, journal, sleeper)

	if sleeping, _, _ := sleeper.state(); !sleeping {
		t.Fatal("want sleeping")
	}
	if ns := pendingDetachNotices(dir, time.Now()); len(ns) != 1 {
		t.Fatalf("pending = %d, want 1 (retry after wake)", len(ns))
	}
}

// result の後に claude を止めたら、依頼主に通知が 1 通届く (issue #395)。
func TestHandleOne_TerminatedAfterResult_NotifiesRequester(t *testing.T) {
	withResultExitLimit(t, 300*time.Millisecond, 500*time.Millisecond)
	script := writeAfterResultFakeClaude(t, "trap 'exit 143' TERM\nwhile :; do sleep 0.1; done")
	client, cfg, runner, hub, journal := newTestEnv(t, script)

	if err := handleOne(context.Background(), client, runner, inbound("hello"), cfg, &activityTracker{}, journal); err != nil {
		t.Fatalf("handleOne: %v", err)
	}
	sends := hub.callsNamed("send_message")
	if len(sends) != 1 {
		t.Fatalf("send_message = %d, want 1", len(sends))
	}
	if body, _ := sends[0].Args["message"].(string); body != terminatedNoticeBody {
		t.Errorf("body = %q", body)
	}
	if to, _ := sends[0].Args["to"].(string); to != "@peer" {
		t.Errorf("to = %q", to)
	}
}

func TestFormatPrompt_HasDetachGuidance(t *testing.T) {
	p := formatPrompt("@me", inbound("hi"), "")
	if !strings.Contains(p, `"$AGENT_HUB_BRIDGE_BIN" detach --`) {
		t.Errorf("prompt lacks detach guidance:\n%s", p)
	}
}
