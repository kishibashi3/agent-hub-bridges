package main

import (
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
