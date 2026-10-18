// proctree.go — claude subprocess を子孫ごと止める (issue #344)
//
// bridge-codex2/cmd/bridge/proctree.go (issue #336 #1) のコピー。関数は同じものを保つ。
//
// 背景: claude の Bash ツールはシェルを setsid で起動する (別 session・別プロセスグループ)。
// 停止や SubprocessTimeout で claude だけを SIGKILL すると、シェルとその子は親を失って
// subreaper (WSL では PID 620 の Relay) に付け替わり、走り続ける。claude は SIGTERM を受ければ
// ツールの子 (run_in_background のものも) を始末してから終わるので、runner.go はまず SIGTERM を
// 送り、猶予内に終わらなかったときだけここの killProcessTree で子孫ごと SIGKILL する。
//
// 対処 (fallback): claude を新しいプロセスグループで起動し、止めるときは
//   - /proc から子孫 (別 session に移ったものも含む) を集めて SIGKILL
//   - プロセスグループごと SIGKILL (/proc が読めない環境の fallback を兼ねる)
//
// を行う。
package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// newProcessGroupAttr は subprocess を新しいプロセスグループで起動する SysProcAttr を返す。
func newProcessGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree は pid とその子孫、および pid のプロセスグループを SIGKILL する。
// 子孫は kill する前に全部集める (親が先に死ぬと ppid が付け替わって辿れなくなるため)。
func killProcessTree(pid int) error {
	targets := descendants(pid)
	for _, p := range targets {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return syscall.Kill(pid, syscall.SIGKILL)
}

// descendants は /proc を読んで pid の子孫 pid を返す (pid 自身は含まない)。
// /proc が読めない環境では nil を返す。
func descendants(pid int) []int {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, ok := readPPID(p); ok {
			children[ppid] = append(children[ppid], p)
		}
	}
	var out []int
	queue := []int{pid}
	seen := map[int]bool{pid: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// readPPID は /proc/<pid>/stat から ppid を読む。comm に空白や括弧が入りうるので、
// 最後の ')' の後ろから数える。
func readPPID(pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
