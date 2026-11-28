package main

import (
	"bytes"
	"context"
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

// startMarked は env に runID の印 (runID が "" なら印なし) を付けて script を起動する。
// 終わったら reap されるよう Wait を goroutine で回す。テストの終わりに必ず止める。
func startMarked(t *testing.T, runID, script string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = envWithout(os.Environ(), engineRunEnv)
	if runID != "" {
		cmd.Env = append(cmd.Env, engineRunEnvEntry(runID))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func leftoverPIDs(procs []leftoverProc) map[int]bool {
	m := map[int]bool{}
	for _, p := range procs {
		m[p.PID] = true
	}
	return m
}

// alive は pid が生きていて zombie でないかを返す。
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !isZombie(pid)
}

// 便の印を持つものだけが対象になる。別の便・別の bridge の印、前方一致するだけの印、印なしは対象外。
func TestFindLeftovers_OnlyThisRunsMark(t *testing.T) {
	run := newEngineRunID("me")
	other := newEngineRunID("me")                            // 同じ bridge の別の便
	otherBridge := fmt.Sprintf("me-%d-zzz-1", os.Getpid()+1) // 別の bridge
	mine := startMarked(t, run, "exec sleep 30")
	pOther := startMarked(t, other, "exec sleep 30")
	pOtherBridge := startMarked(t, otherBridge, "exec sleep 30")
	pPrefix := startMarked(t, run+"0", "exec sleep 30") // 連番 1 と 10 のような前方一致
	pNone := startMarked(t, "", "exec sleep 30")

	got := leftoverPIDs(findLeftovers(run))
	if !got[mine] {
		t.Errorf("own-run process %d not found: %v", mine, got)
	}
	for name, pid := range map[string]int{"other run": pOther, "other bridge": pOtherBridge, "prefix": pPrefix, "unmarked": pNone} {
		if got[pid] {
			t.Errorf("%s process %d must not be a target", name, pid)
		}
	}
	if len(got) != 1 {
		t.Errorf("targets = %v, want only %d", got, mine)
	}
}

func TestNewEngineRunID_Unique(t *testing.T) {
	a, b := newEngineRunID("p"), newEngineRunID("p")
	if a == b {
		t.Fatalf("run IDs must differ: %q", a)
	}
	if !strings.HasPrefix(a, "p-"+strconv.Itoa(os.Getpid())+"-") {
		t.Errorf("run ID %q should carry participant and bridge pid", a)
	}
}

// 既定 (kill=false) では止めずにログに出す。
func TestReapLeftovers_LogOnly_DoesNotStop(t *testing.T) {
	logs := captureSlog(t)
	run := newEngineRunID("me")
	pid := startMarked(t, run, "exec sleep 30")

	reapLeftovers(run, false)

	if !alive(pid) {
		t.Fatalf("process %d was stopped with --reap-leftovers off", pid)
	}
	out := logs.String()
	if !strings.Contains(out, "not stopped") || !strings.Contains(out, strconv.Itoa(pid)+"(sleep)") {
		t.Errorf("log should name the leftover: %s", out)
	}
}

// 有効 (kill=true) では止める。SIGTERM を無視するものは猶予の後に SIGKILL する。
func TestReapLeftovers_Kill_StopsAndEscalates(t *testing.T) {
	orig := reapLeftoverGrace
	reapLeftoverGrace = 300 * time.Millisecond
	t.Cleanup(func() { reapLeftoverGrace = orig })
	logs := captureSlog(t)
	run := newEngineRunID("me")
	polite := startMarked(t, run, "exec sleep 30")
	stubborn := startMarked(t, run, "trap '' TERM; while :; do sleep 0.05; done")
	other := startMarked(t, newEngineRunID("me"), "exec sleep 30")

	reapLeftovers(run, true)

	waitGone(t, polite, 2*time.Second)
	waitGone(t, stubborn, 2*time.Second)
	if !alive(other) {
		t.Errorf("other run's process %d was stopped", other)
	}
	out := logs.String()
	if !strings.Contains(out, "stopped processes") || !strings.Contains(out, "sigkilled="+strconv.Itoa(stubborn)) {
		t.Errorf("log should list stopped and SIGKILLed processes: %s", out)
	}
}

// ── runner の経路 ─────────────────────────────────────────────────────── //

// writeLeftoverFakeClaude は initialize と user message を読んだ後、ツールの `&` のように setsid で
// 子を起動して pid を pidPath に書き、gate が "" でなければそのファイルができるまで待ってから
// result を出して終わる claude CLI 代替を書く。
func writeLeftoverFakeClaude(t *testing.T, pidPath, gate string) string {
	t.Helper()
	wait := ""
	if gate != "" {
		wait = fmt.Sprintf("while [ ! -e '%s' ]; do sleep 0.05; done\n", gate)
	}
	path := filepath.Join(t.TempDir(), "fake-claude-leftover")
	script := fmt.Sprintf("#!/bin/sh\nread -r _init\nread -r _msg\nsetsid sleep 30 >/dev/null 2>&1 &\necho $! > '%s'\n%secho '%s'\nexit 0\n",
		pidPath, wait, fakeResultLine)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func leftoverRunner(t *testing.T, script string, reap bool) *claudeRunner {
	t.Helper()
	dir := t.TempDir()
	return &claudeRunner{cfg: &config{
		Participant:       "leftover-test",
		Workdir:           dir,
		ClaudeCLI:         script,
		ScannerBufferSize: 64 * 1024,
		ReapLeftovers:     reap,
	}, mcpConfigPath: filepath.Join(dir, "mcp.json")}
}

// engine が終わった後に残った、その便の `&` の子は、有効なら止まる。
func TestQuery_ReapLeftovers_StopsChildAfterEngineExit(t *testing.T) {
	requireSetsid(t)
	withResultExitLimit(t, 5*time.Second, 300*time.Millisecond)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	r := leftoverRunner(t, writeLeftoverFakeClaude(t, pidPath, ""), true)

	if _, err := r.query(context.Background(), "start a server with &", "sess", &activityTracker{}); err != nil {
		t.Fatalf("query: %v", err)
	}
	child := waitPIDFile(t, pidPath)
	waitGone(t, child, 3*time.Second)
}

// 既定 (無効) では engine の後に残った子を止めず、ログに出す。
func TestQuery_ReapLeftoversOff_LogsOnly(t *testing.T) {
	requireSetsid(t)
	withResultExitLimit(t, 5*time.Second, 300*time.Millisecond)
	logs := captureSlog(t)
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	r := leftoverRunner(t, writeLeftoverFakeClaude(t, pidPath, ""), false)

	if _, err := r.query(context.Background(), "start a server with &", "sess", &activityTracker{}); err != nil {
		t.Fatalf("query: %v", err)
	}
	child := waitPIDFile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	if !alive(child) {
		t.Fatalf("child %d was stopped with --reap-leftovers off", child)
	}
	if out := logs.String(); !strings.Contains(out, "not stopped") || !strings.Contains(out, strconv.Itoa(child)) {
		t.Errorf("log should name the leftover child: %s", out)
	}
}

// operator GO の条件: 便の途中で動いている子は止めない。
// 便 A の engine が `&` の子を立てて動いている間に、別の便 (compact と query) が終わって回収しても、
// A の子は止まらない。A の engine が終わった後に、A の回収で初めて止まる。
func TestQuery_ChildOfRunningRunNotStopped(t *testing.T) {
	requireSetsid(t)
	withResultExitLimit(t, 5*time.Second, 300*time.Millisecond)
	tmp := t.TempDir()
	pidA, gateA := filepath.Join(tmp, "a.pid"), filepath.Join(tmp, "a.gate")
	rA := leftoverRunner(t, writeLeftoverFakeClaude(t, pidA, gateA), true)

	doneA := make(chan error, 1)
	go func() {
		_, err := rA.query(context.Background(), "A", "sess-a", &activityTracker{})
		doneA <- err
	}()
	childA := waitPIDFile(t, pidA)

	// 便 A の途中で、同じ bridge の別の便が終わって回収する (query と compact)
	pidB := filepath.Join(tmp, "b.pid")
	rB := leftoverRunner(t, writeLeftoverFakeClaude(t, pidB, ""), true)
	if _, err := rB.query(context.Background(), "B", "sess-b", &activityTracker{}); err != nil {
		t.Fatalf("query B: %v", err)
	}
	pidC := filepath.Join(tmp, "c.pid")
	rC := leftoverRunner(t, writeLeftoverFakeClaude(t, pidC, ""), true)
	if _, err := rC.compact(context.Background()); err != nil {
		t.Fatalf("compact: %v", err)
	}
	waitGone(t, waitPIDFile(t, pidB), 3*time.Second)
	waitGone(t, waitPIDFile(t, pidC), 3*time.Second)

	// A はまだ途中。A の子は生きている
	time.Sleep(200 * time.Millisecond)
	if !alive(childA) {
		t.Fatalf("child %d of the running run was stopped by another run's reap", childA)
	}
	select {
	case err := <-doneA:
		t.Fatalf("run A ended before the gate: %v", err)
	default:
	}

	// A の engine を終わらせると、A の回収で止まる
	if err := os.WriteFile(gateA, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-doneA; err != nil {
		t.Fatalf("query A: %v", err)
	}
	waitGone(t, childA, 3*time.Second)
}

// engine に印が渡り、親環境の同名の値は上書きされる。
func TestSpawnSubprocess_SetsEngineRunEnv(t *testing.T) {
	t.Setenv(engineRunEnv, "parent-value")
	script := filepath.Join(t.TempDir(), "fake-claude.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv | grep '^"+engineRunEnv+"='\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &claudeRunner{cfg: &config{ClaudeCLI: script, Workdir: t.TempDir()}}
	cmd, stdin, scanner, err := r.spawnSubprocess(context.Background(), "run-xyz")
	if err != nil {
		t.Fatalf("spawnSubprocess: %v", err)
	}
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	stdin.Close()
	_ = cmd.Wait()
	if len(lines) != 1 || lines[0] != engineRunEnv+"=run-xyz" {
		t.Errorf("engine env lines = %q, want only %s=run-xyz", lines, engineRunEnv)
	}
}

// detach の見届け役 (と処理) には印を渡さない。便の終わりに止められないようにする。
func TestDetach_SupervisorDoesNotInheritRunMark(t *testing.T) {
	dir := setDetachEnv(t)
	t.Setenv(engineRunEnv, "run-detach")
	rec := startDetach(t, dir, "--", "echo mark=${"+engineRunEnv+"-unset}")
	if res := waitDone(t, dir, rec); res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	if tail := readTail(rec.LogPath, 1000); tail != "mark=unset" {
		t.Errorf("detached command saw %q, want mark=unset", tail)
	}
}

func TestResolveReapLeftovers(t *testing.T) {
	cases := []struct {
		name            string
		flagSet, flagV  bool
		env             string
		want, wantError bool
	}{
		{"default", false, false, "", false, false},
		{"env on", false, false, "1", true, false},
		{"env off", false, false, "false", false, false},
		{"flag wins over env", true, false, "1", false, false},
		{"flag on", true, true, "", true, false},
		{"bad env", false, false, "yes-please", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(reapLeftoversEnv, c.env)
			got, err := resolveReapLeftovers(c.flagSet, c.flagV)
			if (err != nil) != c.wantError || got != c.want {
				t.Errorf("got %v, %v; want %v, error=%v", got, err, c.want, c.wantError)
			}
		})
	}
}

// exec の途中のプロセスは environ が空や途中までで読め、印が見えないことがある (CI で
// TestQuery_ReapLeftoversOff_LogsOnly が落ちた原因)。collectLeftovers は 2 回目の走査で見つける。
// 手元では /proc の走査が子に届く前に exec が終わって再現しないので、読み出しを差し替えて
// 1 回目の読みを空・途中までにする。
func TestCollectLeftovers_ChildMidExec(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first func(full []byte) []byte
	}{
		{"empty", func([]byte) []byte { return nil }},
		{"truncated", func(full []byte) []byte { return full[:bytes.LastIndex(full, []byte(engineRunEnv))] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newEngineRunID("midexec")
			pid := startMarked(t, run, "exec sleep 30")
			reads := 0 // findLeftovers は同期的に読むので lock は要らない
			orig := readProcEnviron
			t.Cleanup(func() { readProcEnviron = orig })
			readProcEnviron = func(p int) ([]byte, error) {
				b, err := orig(p)
				if p != pid || err != nil {
					return b, err
				}
				if reads++; reads == 1 {
					return tc.first(b), nil
				}
				return b, nil
			}
			waitMarked(t, pid, run)

			if got := leftoverPIDs(findLeftovers(run)); got[pid] {
				t.Fatalf("setup: the first scan should miss %d", pid)
			}
			reads = 0
			if got := leftoverPIDs(collectLeftovers(run)); !got[pid] || len(got) != 1 {
				t.Fatalf("collectLeftovers = %v, want only %d", got, pid)
			}
		})
	}
}

// waitMarked は pid の environ に run の印が見えるまで待つ (startMarked 直後は exec の途中のことがある)。
func waitMarked(t *testing.T, pid int, run string) {
	t.Helper()
	want := []byte(engineRunEnvEntry(run))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ")); err == nil && hasEnvEntry(b, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("mark of %d not visible", pid)
}
