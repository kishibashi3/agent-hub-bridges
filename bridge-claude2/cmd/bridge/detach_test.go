package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// startDetach は detach で command を流し、開始の記録を返す。
func startDetach(t *testing.T, dir string, args ...string) detachRecord {
	t.Helper()
	var out, errOut strings.Builder
	if code := runDetach(args, &out, &errOut); code != 0 {
		t.Fatalf("runDetach code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "detached: id=") {
		t.Fatalf("runDetach stdout = %q", out.String())
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var recs []string
	for _, e := range entries {
		if !strings.HasSuffix(e, ".done.json") {
			recs = append(recs, e)
		}
	}
	if len(recs) != 1 {
		t.Fatalf("records = %v, want 1", recs)
	}
	var rec detachRecord
	if err := readJSONFile(recs[0], &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// waitDone は終了の記録が書かれるまで待って返す。
func waitDone(t *testing.T, dir string, rec detachRecord) detachResult {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var res detachResult
		if err := readJSONFile(filepath.Join(dir, rec.ID+".done.json"), &res); err == nil {
			return res
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("detached command did not finish in time")
	return detachResult{}
}

func TestDetach_ExitZero_RecordsResultAndOutput(t *testing.T) {
	dir := setDetachEnv(t)
	rec := startDetach(t, dir, "-name", "loop", "--", "for i in 1 2 3; do echo line$i; done")
	res := waitDone(t, dir, rec)
	if res.ExitCode != 0 || res.Signal != "" || res.Error != "" {
		t.Fatalf("result = %+v, want exit 0", res)
	}
	if rec.Session != "@requester" || rec.CausedBy != "orig-msg-1" || rec.Name != "loop" || rec.SupervisorPID == 0 {
		t.Errorf("record = %+v", rec)
	}
	if rec.LogPath != filepath.Join(dir, rec.ID+".log") {
		t.Errorf("log path = %q", rec.LogPath)
	}
	if tail := readTail(rec.LogPath, 1000); tail != "line1\nline2\nline3" {
		t.Errorf("output = %q", tail)
	}
}

// 終了コードが 0 でなくても終了の記録が残る。
func TestDetach_NonZeroExit_Recorded(t *testing.T) {
	dir := setDetachEnv(t)
	rec := startDetach(t, dir, "--", "echo boom >&2; exit 3")
	if res := waitDone(t, dir, rec); res.ExitCode != 3 {
		t.Fatalf("result = %+v, want exit 3", res)
	}
	if tail := readTail(rec.LogPath, 1000); tail != "boom" {
		t.Errorf("output = %q", tail)
	}
}

func TestDetach_Signal_Recorded(t *testing.T) {
	dir := setDetachEnv(t)
	rec := startDetach(t, dir, "--", "kill -TERM $$")
	if res := waitDone(t, dir, rec); res.ExitCode != -1 || res.Signal == "" {
		t.Fatalf("result = %+v, want signal", res)
	}
}

// argv が 2 つ以上なら bash -c を通さずにそのまま実行する。
func TestDetach_Argv(t *testing.T) {
	dir := setDetachEnv(t)
	rec := startDetach(t, dir, "--", "/bin/echo", "a b", "$HOME")
	waitDone(t, dir, rec)
	if tail := readTail(rec.LogPath, 1000); tail != "a b $HOME" {
		t.Errorf("output = %q", tail)
	}
}

func TestDetach_StartFailure_Recorded(t *testing.T) {
	dir := setDetachEnv(t)
	rec := startDetach(t, dir, "--", "/nonexistent/cmd", "x")
	if res := waitDone(t, dir, rec); res.ExitCode != -1 || res.Error == "" {
		t.Fatalf("result = %+v, want start error", res)
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

// 見届け役だけが消えたら終了の記録は書かれず、見届け役は「無い」と判定でき、
// 処理のプロセスグループはまだ残っていると判定できる。
func TestDetach_LostSupervisor_ChildStillRunning(t *testing.T) {
	dir := setDetachEnv(t)
	gate := filepath.Join(t.TempDir(), "gate")
	rec := startDetach(t, dir, "--", fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.05; done", gate))
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o600) })
	if st, ok := readStartTime(rec.SupervisorPID); !ok || st != rec.SupervisorStart {
		t.Fatalf("supervisor start time = %d,%v; record has %d", st, ok, rec.SupervisorStart)
	}
	if err := syscall.Kill(rec.SupervisorPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := readStartTime(rec.SupervisorPID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor still alive after SIGKILL")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !processGroupAlive(rec.SupervisorPID) {
		t.Error("process group of the job should still be alive")
	}
	if _, err := os.Stat(filepath.Join(dir, rec.ID+".done.json")); !os.IsNotExist(err) {
		t.Errorf("done record must not exist: %v", err)
	}
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
// 同じ処理の便は 1 回だけ。claude の env には detach 用の値が付く。
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
	for _, want := range []string{detachSessionEnv + "=@requester",
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

func TestFormatPrompt_HasDetachGuidance(t *testing.T) {
	p := formatPrompt("@me", inbound("hi"), "")
	if !strings.Contains(p, `"$AGENT_HUB_BRIDGE_BIN" detach --`) {
		t.Errorf("prompt lacks detach guidance:\n%s", p)
	}
}

// 見届け役だけが消えて処理が残っているときは、そのことを便に書く。
func TestDetachNotice_LostSupervisor_SaysJobMayStillRun(t *testing.T) {
	dir := setDetachEnv(t)
	gate := filepath.Join(t.TempDir(), "gate")
	rec := startDetach(t, dir, "--", fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.05; done", gate))
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o600) })
	if err := syscall.Kill(rec.SupervisorPID, syscall.SIGKILL); err != nil {
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
	if n.Lost == "" {
		t.Fatal("want lost notice")
	}
	if body := n.body(time.Now()); !strings.Contains(body, "result: LOST") || !strings.Contains(body, "まだ残っている") {
		t.Errorf("body should say the job may still be running:\n%s", body)
	}
}
