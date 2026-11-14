// detach.go — ターンを越えて走らせる処理を切り離し、終わったら記録を残す (issue #395)
//
// bridge は依頼ごとに claude を終わらせる。claude の中の background task は claude と一緒に
// 止まり、依頼側からは走行中に見えたまま成果が消える。そこで、ターンを越えて走らせたい処理は
// bridge 自身の `detach` サブコマンドで切り離す:
//
//	"$AGENT_HUB_BRIDGE_BIN" detach -- '<command>'
//
// detach は見届け役 (`detach-run`) を setsid で起動してすぐ戻る。見届け役は claude の
// プロセス木・プロセスグループの外にいるので、ターンが終わっても止められない。処理が
// 終わると (終了コードによらず) 終了の記録を書く。
//
// 記録の置き場 (spool) を見て、終わった処理の便を claude に差し込むのは bridge 側
// (別 PR で追加する)。このファイルはサブコマンドと記録の書き出しだけを持つ。
//
// spool のファイル (id ごと):
//
//	<id>.json      開始の記録 (detachRecord)。detach が書く
//	<id>.log       処理の stdout / stderr (見届け役自身のエラーもここ)
//	<id>.done.json 終了の記録 (detachResult)。見届け役が書く
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// claude に渡す env の名前 (issue #395)。
const (
	detachBinEnv      = "AGENT_HUB_BRIDGE_BIN"
	detachDirEnv      = "AGENT_HUB_DETACH_DIR"
	detachSessionEnv  = "AGENT_HUB_DETACH_SESSION"
	detachCausedByEnv = "AGENT_HUB_DETACH_CAUSED_BY"
)

// detachRecord は開始の記録。
type detachRecord struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	Argv            []string `json:"argv"`
	Cwd             string   `json:"cwd"`
	Session         string   `json:"session"`
	CausedBy        string   `json:"caused_by,omitempty"`
	StartedAt       string   `json:"started_at"`
	LogPath         string   `json:"log_path"`
	SupervisorPID   int      `json:"supervisor_pid,omitempty"`
	SupervisorStart uint64   `json:"supervisor_start,omitempty"` // /proc/<pid>/stat の starttime (pid 再利用の判定用)
}

// detachResult は終了の記録。
type detachResult struct {
	ExitCode int    `json:"exit_code"`        // シグナルで終わったときは -1
	Signal   string `json:"signal,omitempty"` // シグナルで終わったときのシグナル名
	Error    string `json:"error,omitempty"`  // 起動できなかったとき等
	EndedAt  string `json:"ended_at"`
}

// ──────────────────────────────────────────────────────────────────────── //
// サブコマンド (claude のツールから呼ばれる側)                              //
// ──────────────────────────────────────────────────────────────────────── //

// runSubcommand は os.Args[1] がサブコマンドならそれを実行して終了コードを返す。
// サブコマンドでなければ ok=false。
func runSubcommand(args []string) (code int, ok bool) {
	if len(args) < 1 {
		return 0, false
	}
	switch args[0] {
	case "detach":
		return runDetach(args[1:], os.Stdout, os.Stderr), true
	case "detach-run":
		return runDetachSupervisor(args[1:]), true
	}
	return 0, false
}

const detachUsage = `usage: "$AGENT_HUB_BRIDGE_BIN" detach [-name <label>] -- <command>

ターンが終わった後も走らせたい処理を切り離して起動し、すぐ戻ります。
処理が終わると (終了コードによらず) 終了コード・経過時間・出力の末尾を載せた便が
このセッションに届きます (issue #395)。
<command> が 1 つの文字列なら bash -c で実行し、2 つ以上なら argv としてそのまま実行します。
`

// runDetach は `detach` サブコマンド。見届け役を setsid で起動し、id と出力先を表示して戻る。
func runDetach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("detach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, detachUsage) }
	name := fs.String("name", "", "label shown in the notice (optional)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 || (len(argv) == 1 && strings.TrimSpace(argv[0]) == "") {
		fs.Usage()
		return 2
	}
	dir := os.Getenv(detachDirEnv)
	if dir == "" {
		fmt.Fprintf(stderr, "detach: %s is not set (run this from a bridge-claude2 session)\n", detachDirEnv)
		return 2
	}
	session := os.Getenv(detachSessionEnv)
	if session == "" {
		fmt.Fprintf(stderr, "detach: %s is not set (run this while handling a message)\n", detachSessionEnv)
		return 2
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "detach: create spool dir: %v\n", err)
		return 1
	}
	cwd, _ := os.Getwd()
	id := newDetachID(time.Now())
	rec := detachRecord{
		ID:        id,
		Name:      *name,
		Argv:      argv,
		Cwd:       cwd,
		Session:   session,
		CausedBy:  os.Getenv(detachCausedByEnv),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		LogPath:   filepath.Join(dir, id+".log"),
	}
	if err := writeJSONFile(filepath.Join(dir, id+".json"), rec); err != nil {
		fmt.Fprintf(stderr, "detach: write record: %v\n", err)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "detach: resolve own executable: %v\n", err)
		return 1
	}
	logFile, err := os.OpenFile(rec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "detach: open log: %v\n", err)
		return 1
	}
	defer logFile.Close()
	sup := exec.Command(exe, "detach-run", filepath.Join(dir, id+".json"))
	sup.Dir = cwd
	sup.Stdin = nil // /dev/null
	sup.Stdout = logFile
	sup.Stderr = logFile
	// 別セッションにして claude のプロセスグループ・ツールのシェルの片付けから外す。
	sup.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sup.Start(); err != nil {
		fmt.Fprintf(stderr, "detach: start supervisor: %v\n", err)
		// 記録は残っている (supervisor_pid なし = 見届け役を起動できなかった)。
		return 1
	}
	rec.SupervisorPID = sup.Process.Pid
	rec.SupervisorStart, _ = readStartTime(rec.SupervisorPID)
	if err := writeJSONFile(filepath.Join(dir, id+".json"), rec); err != nil {
		fmt.Fprintf(stderr, "detach: update record: %v\n", err)
	}
	_ = sup.Process.Release()

	fmt.Fprintf(stdout, "detached: id=%s pid=%d\noutput: %s\n"+
		"When it ends (any exit code), a notice with the exit code and the tail of the output "+
		"will arrive in this session. Do not wait for it in this turn.\n",
		id, rec.SupervisorPID, rec.LogPath)
	return 0
}

// runDetachSupervisor は `detach-run <record>`。処理を起動して終わるまで待ち、終了の記録を書く。
// stdout / stderr は detach が処理の出力先につないでいる。
func runDetachSupervisor(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "detach-run: usage: detach-run <record.json>")
		return 2
	}
	recPath := args[0]
	donePath := strings.TrimSuffix(recPath, ".json") + ".done.json"
	var rec detachRecord
	if err := readJSONFile(recPath, &rec); err != nil {
		fmt.Fprintf(os.Stderr, "[detach-run] read record: %v\n", err)
		_ = writeJSONFile(donePath, detachResult{ExitCode: -1, Error: "read record: " + err.Error(),
			EndedAt: time.Now().UTC().Format(time.RFC3339Nano)})
		return 1
	}
	res := runDetachedCommand(rec)
	if err := writeJSONFile(donePath, res); err != nil {
		fmt.Fprintf(os.Stderr, "[detach-run] write result: %v\n", err)
		return 1
	}
	return 0
}

// runDetachedCommand は rec の処理を起動して終わるまで待ち、結果を返す。
func runDetachedCommand(rec detachRecord) detachResult {
	argv := rec.Argv
	if len(argv) == 1 {
		argv = []string{"/bin/bash", "-c", argv[0]}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = rec.Cwd
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	res := detachResult{EndedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err == nil {
		return res
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.ExitCode = -1
			res.Signal = ws.Signal().String()
			return res
		}
		res.ExitCode = exitErr.ExitCode()
		return res
	}
	res.ExitCode = -1
	res.Error = err.Error()
	fmt.Fprintf(os.Stderr, "[detach-run] %v\n", err)
	return res
}

// ──────────────────────────────────────────────────────────────────────── //
// ファイル・/proc のユーティリティ                                          //
// ──────────────────────────────────────────────────────────────────────── //

// newDetachID は時刻順に並ぶ一意な id を返す。
func newDetachID(now time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return now.UTC().Format("20060102T150405.000Z") + "-" + hex.EncodeToString(b)
}

// writeJSONFile は v を path に atomic に書く (tmp → rename)。
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// readTail は path の末尾 max バイトを返す (先頭の途中の行は落とす)。
func readTail(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - max
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	s := string(buf)
	if off > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return strings.ToValidUTF8(strings.TrimRight(s, "\n"), "")
}

// processGroupAlive は pgid のプロセスグループにまだプロセスが居るかを返す。
func processGroupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

// readStartTime は /proc/<pid>/stat の starttime (22 番目) を返す。プロセスが無ければ ok=false。
// zombie (終わって reap 待ち) も「無い」として扱う。
func readStartTime(pid int) (uint64, bool) {
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
	// fields[0] は state (3 番目)。starttime は 22 番目 = fields[19]
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return 0, false
	}
	v, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
