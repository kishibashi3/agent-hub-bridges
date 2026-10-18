package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKillProcessTree_KillsSetsidGrandchild は、別 session に移った孫プロセスも
// killProcessTree で止まることを確かめる (issue #336 #1)。
func TestKillProcessTree_KillsSetsidGrandchild(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	cmd := exec.Command("sh", "-c", "setsid sleep 30 & echo $!; wait")
	cmd.SysProcAttr = newProcessGroupAttr()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := startProcess(cmd); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, err := stdout.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("parse grandchild pid %q: %v", buf[:n], err)
	}

	// setsid 済みの孫は別 session・別プロセスグループにいる
	time.Sleep(100 * time.Millisecond)
	if err := killProcessTree(cmd.Process.Pid); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ppid, ok := readPPID(grandchild); !ok || ppid == 0 {
			return
		}
		if err := syscall.Kill(grandchild, 0); err != nil {
			return
		}
		// zombie (reparent 先が回収前) は死んでいる扱いにする
		if st := procState(grandchild); st == "Z" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(grandchild, syscall.SIGKILL)
	t.Fatalf("grandchild %d still alive after killProcessTree", grandchild)
}

func procState(pid int) string {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))[:1]
}

// fakeEngineScript は SIGTERM を受けたら $1 にファイルを書いて終わる偽 engine。
// trap を張った後に $2 を書いて準備完了を知らせる (issue #371)。
const fakeEngineScript = `trap 'echo term > "$1"; exit 0' TERM; echo ready > "$2"; while :; do sleep 0.1; done`

func startFakeEngine(marker, ready string) (*exec.Cmd, error) {
	cmd := exec.Command("sh", "-c", fakeEngineScript, "sh", marker, ready)
	cmd.SysProcAttr = newProcessGroupAttr()
	if err := startProcess(cmd); err != nil {
		return nil, err
	}
	if !waitForFile(ready, 5*time.Second) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("fake engine %d did not become ready", cmd.Process.Pid)
	}
	return cmd, nil
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestHelperIntermediate はテストではなく、TestStartProcess_SIGTERMOnBridgeKill が子として
// 起動する中間プロセス (bridge 本体の代わり)。偽 engine を startProcess で起動し、pid を出して待つ。
func TestHelperIntermediate(t *testing.T) {
	if os.Getenv("I371_HELPER") != "1" {
		t.Skip("helper process")
	}
	cmd, err := startFakeEngine(os.Getenv("I371_MARKER"), os.Getenv("I371_READY"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(cmd.Process.Pid)
	select {}
}

// TestStartProcess_SIGTERMOnBridgeKill は、bridge 本体が SIGKILL されたら startProcess で起動した
// engine に SIGTERM が届くことを確かめる (issue #371)。
func TestStartProcess_SIGTERMOnBridgeKill(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "term")
	helper := exec.Command(os.Args[0], "-test.run=^TestHelperIntermediate$")
	helper.Env = append(os.Environ(), "I371_HELPER=1", "I371_MARKER="+marker, "I371_READY="+filepath.Join(dir, "ready"))
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = helper.Process.Kill()
		t.Fatalf("read engine pid: %v", err)
	}
	engine, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		_ = helper.Process.Kill()
		t.Fatalf("parse engine pid %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(engine, syscall.SIGKILL) })

	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()

	if !waitForFile(marker, 5*time.Second) {
		t.Fatalf("engine %d did not receive SIGTERM after bridge was SIGKILLed", engine)
	}
}

// TestStartProcess_SurvivesThreadChurn は、bridge 内でスレッドが終了しても (LockOSThread したまま
// goroutine が終わる) startProcess で起動した engine が止まらないことを確かめる (issue #371)。
// 呼び出し元の goroutine のスレッドが終了する場合も含める (専用スレッドを通さずに Start すると、
// ここで engine が止まる)。
func TestStartProcess_SurvivesThreadChurn(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "term")
	var (
		cmd      *exec.Cmd
		startErr error
	)
	started := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		cmd, startErr = startFakeEngine(marker, filepath.Join(dir, "ready"))
		close(started)
		// UnlockOSThread せずに終わるので、このスレッドは終了する
	}()
	<-started
	if startErr != nil {
		t.Fatal(startErr)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	for i := 0; i < 500; i++ {
		done := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			close(done)
			// UnlockOSThread せずに終わるので、このスレッドは終了する
		}()
		<-done
	}

	if waitForFile(marker, 500*time.Millisecond) {
		t.Fatal("engine received SIGTERM while bridge is alive")
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("engine stopped while bridge is alive: %v", err)
	}
}
