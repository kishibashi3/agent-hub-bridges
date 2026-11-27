// leftover.go — 便が終わった後に残った、ツールが & で切り離した子を見つけて止める (issue #377)
//
// bridge-claude2/cmd/bridge/leftover.go のコピー。関数は同じものを保つ。
//
// 背景: engine (claude / codex) のツールが `&` で起動した子は、ツールのシェルが終わった時点で
// engine のプロセスの木から外れ、subreaper (systemd --user 等) に付け替わる。そのため
// proctree.go の descendants() やプロセスグループの kill では届かず、engine の Pdeathsig
// (issue #371) も fork で引き継がれないので届かない。engine が普通に終わった後も残り続ける。
//
// 対処 (#377 案 A): 便ごとに一意な印 (engineRunEnv) を env に付けて engine を起動する。
// `&` の子は setsid 済みでも env を受け継ぐので、engine が終わった後 (cmd.Wait() が返った後) に
// /proc を読み、自分の uid で、その便の印を持つプロセスを集める。
//   - --reap-leftovers 無効 (既定): 見つけたものをログに出すだけで止めない
//   - --reap-leftovers 有効: SIGTERM → reapLeftoverGrace → 残っていれば SIGKILL
//
// 正当な子を誤って止めない条件:
//   - 回収は engine が終わった後だけ。便の途中で `&` で立てて次のツールで使うものは止めない
//   - 印は便ごとに一意 (participant・bridge pid・bridge の起動時刻・連番)。別の便・別の bridge の
//     ものは対象にならない。compact と query が同時に走っても互いのものを止めない
//   - detach の見届け役 (detach.go) には印を渡さない。ターンを越える処理の正規の経路なので止めない
//
// 盲点: env を消して起動したもの (`env -i`、`sudo`、`systemd-run --user` 等) は印が付かず、
// 見つけられない。便をまたいで残したい常駐プロセスは `systemd-run --user` で起動してもらう
// (cannotWaitGuidance で案内する)。
package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// engineRunEnv は engine に付ける便ごとの印の env 名 (issue #377)。
const engineRunEnv = "AGENT_HUB_ENGINE_RUN"

// reapLeftoverGrace は残った子に SIGTERM を送ってから SIGKILL するまでの猶予 (issue #377)。
// 停止の経路でも走るので、engine を止める時間と合わせて agenthubctl bridge stop の猶予 5s に
// 収まるよう短くとる。テストから変えられるよう var にしている。
var reapLeftoverGrace = 1 * time.Second

var (
	engineRunSeq atomic.Uint64
	// engineRunEpoch は bridge の起動時刻。bridge の pid が再利用されても、前の bridge の印と重ならない。
	engineRunEpoch = strconv.FormatInt(time.Now().UnixNano(), 36)
)

// newEngineRunID は便ごとに一意な印を返す。
func newEngineRunID(handle string) string {
	return fmt.Sprintf("%s-%d-%s-%d", handle, os.Getpid(), engineRunEpoch, engineRunSeq.Add(1))
}

// engineRunEnvEntry は engine の env に足す 1 行を返す。
func engineRunEnvEntry(runID string) string {
	return engineRunEnv + "=" + runID
}

// envWithout は env から name の行を除いたものを返す。
func envWithout(env []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// leftoverProc は印を持って残っていたプロセス 1 つ。
type leftoverProc struct {
	PID  int
	Comm string
}

func (p leftoverProc) String() string { return fmt.Sprintf("%d(%s)", p.PID, p.Comm) }

// findLeftovers は /proc を読み、自分の uid で runID の印を持つ (zombie でない) プロセスを返す。
// bridge 自身は含めない。/proc が読めない環境では nil を返す。
func findLeftovers(runID string) []leftoverProc {
	if runID == "" {
		return nil
	}
	want := []byte(engineRunEnvEntry(runID))
	uid := uint32(os.Getuid())
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []leftoverProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		info, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
			continue
		}
		environ, err := readProcEnviron(pid)
		if err != nil || !hasEnvEntry(environ, want) {
			continue
		}
		if isZombie(pid) {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
		out = append(out, leftoverProc{PID: pid, Comm: strings.TrimSpace(string(comm))})
	}
	return out
}

// readProcEnviron は /proc/<pid>/environ を読む。テストから差し替えられるよう var にしている。
var readProcEnviron = func(pid int) ([]byte, error) {
	return os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
}

// leftoverRescanDelay は collectLeftovers が /proc を 2 回走査する間隔。
// exec の途中のプロセスは /proc/<pid>/environ が空や途中までで読め、印が見えないことがある。
// engine が `setsid cmd &` の直後に終わると、回収はちょうどその子の exec の途中に当たりうる
// (CI で TestQuery_ReapLeftoversOff_LogsOnly が落ちた原因。手元の計測では 1000 回中 5 回が
// 1 回目に印が見えず、100ms 後に読み直すと 1000 回すべて見えた)。テストから変えられるよう var にしている。
var leftoverRescanDelay = 50 * time.Millisecond

// collectLeftovers は findLeftovers を leftoverRescanDelay 空けて 2 回行い、合わせたものを返す。
// 1 回目で exec の途中だったものは 2 回目で見える。1 回目だけで見えて 2 回目までに居なくなったものは除く。
func collectLeftovers(runID string) []leftoverProc {
	first := findLeftovers(runID)
	time.Sleep(leftoverRescanDelay)
	second := findLeftovers(runID)
	seen := map[int]bool{}
	out := make([]leftoverProc, 0, len(first)+len(second))
	for _, p := range second {
		seen[p.PID] = true
		out = append(out, p)
	}
	for _, p := range first {
		if !seen[p.PID] && !isZombie(p.PID) {
			seen[p.PID] = true
			out = append(out, p)
		}
	}
	return out
}

// hasEnvEntry は /proc/<pid>/environ (NUL 区切り) に want と完全に一致する行があるかを返す。
func hasEnvEntry(environ, want []byte) bool {
	for _, kv := range bytes.Split(environ, []byte{0}) {
		if bytes.Equal(kv, want) {
			return true
		}
	}
	return false
}

// isZombie は /proc/<pid>/stat の状態が Z (または読めない = もう無い) かを返す。
func isZombie(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return false
	}
	fields := strings.Fields(s[i+1:])
	return len(fields) > 0 && fields[0] == "Z"
}

// reapLeftovers は engine が終わった後に呼ぶ。runID の印を持って残っているプロセスを探し、
// kill が false ならログに出すだけ、true なら SIGTERM → 猶予 → SIGKILL で止める (issue #377)。
// 止めたものの pid・comm は Info で出す。
func reapLeftovers(runID string, kill bool) {
	procs := collectLeftovers(runID)
	if len(procs) == 0 {
		return
	}
	if !kill {
		slog.Info("[leftover] processes started during this run are still running after the engine exited — not stopped (--reap-leftovers is off) (issue #377)",
			"run", runID, "count", len(procs), "procs", formatLeftovers(procs))
		return
	}
	for _, p := range procs {
		_ = syscall.Kill(p.PID, syscall.SIGTERM)
	}
	deadline := time.Now().Add(reapLeftoverGrace)
	remaining := procs
	for len(remaining) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		remaining = findLeftovers(runID)
	}
	// 猶予の後は、いまも同じ印を持っているものを SIGKILL する。印で選び直すので、pid が再利用された
	// 先は止めず、SIGTERM の後に増えたもの (同じ便の印を受け継いだ子) は止める
	remaining = findLeftovers(runID)
	for _, p := range remaining {
		_ = syscall.Kill(p.PID, syscall.SIGKILL)
	}
	slog.Info("[leftover] stopped processes started during this run that were still running after the engine exited (issue #377)",
		"run", runID, "count", len(procs), "procs", formatLeftovers(procs),
		"sigkilled", formatLeftovers(remaining))
}

func formatLeftovers(procs []leftoverProc) string {
	s := make([]string, len(procs))
	for i, p := range procs {
		s[i] = p.String()
	}
	return strings.Join(s, ",")
}
