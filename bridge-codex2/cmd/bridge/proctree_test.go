package main

import (
	"os/exec"
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
	if err := cmd.Start(); err != nil {
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
