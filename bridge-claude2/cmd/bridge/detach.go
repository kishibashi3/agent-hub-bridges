// detach.go — ターンを越えて走らせる処理を切り離し、終わったら peer に便を届ける (issue #395)
//
// bridge は依頼ごとに claude を終わらせる。claude の中の background task (run_in_background・
// タイムアウトで background に移った Bash・background の Agent) は claude と一緒に止まり、
// 依頼側からは走行中に見えたまま成果が消える。そこで:
//
//  1. runner.go が claude の env に CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1 を付け、claude の中に
//     background task が立たないようにする (タイムアウトしたコマンドは background に移らず、
//     タイムアウトとして claude に返る)。
//
//  2. ターンを越えて走らせたい処理は、bridge 自身の `detach` サブコマンドで切り離す:
//
//     "$AGENT_HUB_BRIDGE_BIN" detach -- '<command>'
//
//     detach は見届け役 (`detach-run`) を setsid で起動してすぐ戻る。見届け役は claude の
//     プロセス木・プロセスグループの外にいるので、ターンが終わっても止められない。処理が
//     終わると (終了コードによらず) 終了の記録を書く。
//
//  3. bridge は記録の置き場 (spool) を定期的に見て、終わった処理ごとに 1 回、その処理を
//     流したときの依頼元のセッションで claude を起こす (= 便)。hub は自分宛の DM を受け
//     付けないので、hub を通さず bridge の中で差し込む。記録はファイルなので bridge が
//     再起動しても失われない。見届け役ごと消えた場合 (OOM kill 等) も「消えた」として届ける。
//
// spool のファイル (id ごと):
//
//	<id>.json      開始の記録 (detachRecord)。detach が書く
//	<id>.log       処理の stdout / stderr (見届け役自身のエラーもここ)
//	<id>.done.json 終了の記録 (detachResult)。見届け役が書く
//	<id>.notified  bridge が便を出した印 (空ファイル)。これがあれば二度と出さない
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// claude に渡す env の名前 (issue #395)。
const (
	detachBinEnv      = "AGENT_HUB_BRIDGE_BIN"
	detachDirEnv      = "AGENT_HUB_DETACH_DIR"
	detachSessionEnv  = "AGENT_HUB_DETACH_SESSION"
	detachCausedByEnv = "AGENT_HUB_DETACH_CAUSED_BY"
)

// detachPollInterval は bridge が spool を見に行く間隔。
var detachPollInterval = 5 * time.Second

// detachStartGrace は開始の記録に見届け役の pid がまだ無いときに、待ってやる時間。
// これを過ぎても pid が無ければ、detach が見届け役を起動する前に死んだとみなす。
var detachStartGrace = 30 * time.Second

// detachLogTailBytes は便に載せる出力の末尾の上限。
const detachLogTailBytes = 2000

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
		// 記録は残っている。pid が無いまま detachStartGrace を過ぎると、bridge が「起動できなかった」として便を出す。
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
// bridge 側: spool を見て、終わった処理の便を作る                           //
// ──────────────────────────────────────────────────────────────────────── //

// detachNotice は便 1 通ぶん。
type detachNotice struct {
	Record detachRecord
	Result *detachResult // nil = 見届け役ごと消えた (終了の記録なし)
	Lost   string        // Result が nil のときの理由
}

// pendingDetachNotices は dir にある、終わったのにまだ便を出していない処理を古い順に返す。
func pendingDetachNotices(dir string, now time.Time) []detachNotice {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("detach: read spool dir failed", "dir", dir, "err", err)
		}
		return nil
	}
	var out []detachNotice
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".done.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if detachFileExists(filepath.Join(dir, id+".notified")) {
			continue
		}
		var rec detachRecord
		if err := readJSONFile(filepath.Join(dir, name), &rec); err != nil {
			slog.Warn("detach: unreadable record, skipping", "file", name, "err", err)
			continue
		}
		var res detachResult
		if err := readJSONFile(filepath.Join(dir, id+".done.json"), &res); err == nil {
			out = append(out, detachNotice{Record: rec, Result: &res})
			continue
		}
		// 終了の記録が無い: 見届け役が生きていれば走行中。消えていれば「消えた」として届ける。
		if lost := supervisorLost(rec, now); lost != "" {
			// 見届け役が終了の記録を書いた直後かもしれないので読み直す
			if err := readJSONFile(filepath.Join(dir, id+".done.json"), &res); err == nil {
				out = append(out, detachNotice{Record: rec, Result: &res})
				continue
			}
			out = append(out, detachNotice{Record: rec, Lost: lost})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Record.ID < out[j].Record.ID })
	return out
}

// supervisorLost は見届け役が終了の記録を書かずに消えていれば理由を返す。走行中なら "".
func supervisorLost(rec detachRecord, now time.Time) string {
	if rec.SupervisorPID <= 0 {
		started, err := time.Parse(time.RFC3339Nano, rec.StartedAt)
		if err != nil || now.Sub(started) > detachStartGrace {
			return "見届け役 (detach-run) を起動できなかった"
		}
		return ""
	}
	st, ok := readStartTime(rec.SupervisorPID)
	if !ok {
		return "見届け役 (detach-run) が終了の記録を書かずに消えた (kill・OOM・再起動など)"
	}
	if rec.SupervisorStart != 0 && st != rec.SupervisorStart {
		return "見届け役 (detach-run) が終了の記録を書かずに消えた (pid は別のプロセスが再利用)"
	}
	return ""
}

// markDetachNotified は便を出した印を付ける。
func markDetachNotified(dir, id string) error {
	return os.WriteFile(filepath.Join(dir, id+".notified"), nil, 0o600)
}

// unmarkDetachNotified は印を外す (limit 休眠で便を出せなかったとき、後で出し直すため)。
func unmarkDetachNotified(dir, id string) {
	_ = os.Remove(filepath.Join(dir, id+".notified"))
}

// deliverDetachNotices は終わった処理ごとに 1 回、処理を流したときの依頼主のセッションで
// claude を起こす (= 便)。runHubSession のループから呼ぶ (hub client と同じ goroutine)。
//
// 便を出す前に印 (<id>.notified) を付ける。claude のターンが途中で落ちても同じ便を二度出さない
// (二重の作業より、依頼主への自動エラー返信・中断通知で知らせる方を取る)。limit で休眠に
// 入るときだけ印を外し、復帰後に出し直す。
func deliverDetachNotices(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	runner *claudeRunner,
	tracker *activityTracker,
	journal *Journal,
	sleeper *limitSleeper,
) {
	if cfg.DetachDir == "" {
		return
	}
	for _, n := range pendingDetachNotices(cfg.DetachDir, time.Now()) {
		if ctx.Err() != nil {
			return
		}
		if sleeping, _, _ := sleeper.state(); sleeping {
			return
		}
		id := n.Record.ID
		if err := markDetachNotified(cfg.DetachDir, id); err != nil {
			slog.Warn("[detach] cannot mark as notified, skipping for now", "id", id, "err", err)
			continue
		}
		now := time.Now()
		msg := agenthub.Message{
			ID:        n.noticeMessageID(),
			Sender:    n.Record.Session,
			To:        "@" + cfg.Participant,
			Body:      "[detach] " + id,
			CausedBy:  n.Record.CausedBy,
			Timestamp: now.UTC().Format(time.RFC3339Nano),
		}
		slog.Info("← detach notice (issue #395)",
			"id", id, "session", n.Record.Session, "caused_by", n.Record.CausedBy,
			"exit_code", resultField(n), "lost", n.Lost)
		err := runPrompt(ctx, client, runner, msg,
			formatDetachPrompt("@"+cfg.Participant, n, now, cfg.GitHubFooter), cfg, tracker, journal)
		if lim := asLimitError(err); lim != nil {
			unmarkDetachNotified(cfg.DetachDir, id)
			sleeper.enter(lim, nil)
			slog.Info("[limit] entering sleep during detach notice — will retry after wake",
				"kind", lim.Kind, "until", lim.Until.Format(time.RFC3339), "id", id)
			if _, rerr := client.Register(ctx, sleepingDisplayName(cfg.DisplayName, lim.Until, lim.Kind), cfg.Mode); rerr != nil {
				slog.Warn("[limit] failed to update display_name for sleep (continuing)", "err", rerr)
			}
			return
		}
		if err != nil {
			slog.Error("[detach] notice handling error", "id", id, "err", err)
		}
	}
}

// resultField はログ用の終了コード表記。
func resultField(n detachNotice) string {
	switch {
	case n.Result == nil:
		return "lost"
	case n.Result.Error != "":
		return "error"
	case n.Result.Signal != "":
		return "signal " + n.Result.Signal
	default:
		return strconv.Itoa(n.Result.ExitCode)
	}
}

// noticeMessageID は便に使う message ID。処理を流したときの依頼の ID があればそれを使う
// (返信の caused_by・自動エラー返信がその依頼につながるように)。
func (n detachNotice) noticeMessageID() string {
	if n.Record.CausedBy != "" {
		return n.Record.CausedBy
	}
	return "detach-" + n.Record.ID
}

// body は便の本文 (処理の結果)。
func (n detachNotice) body(now time.Time) string {
	r := n.Record
	var b strings.Builder
	fmt.Fprintf(&b, "detach id: %s\n", r.ID)
	if r.Name != "" {
		fmt.Fprintf(&b, "name: %s\n", r.Name)
	}
	fmt.Fprintf(&b, "command: %s\n", truncate(strings.Join(r.Argv, " "), 300))
	fmt.Fprintf(&b, "cwd: %s\n", r.Cwd)
	fmt.Fprintf(&b, "started_at: %s\n", r.StartedAt)
	ended := now
	switch {
	case n.Result == nil:
		fmt.Fprintf(&b, "result: LOST — %s。処理が最後まで走ったかは分からない\n", n.Lost)
		if r.SupervisorPID > 0 && processGroupAlive(r.SupervisorPID) {
			// 見届け役は自分のプロセスグループ (pgid = 見届け役の pid) で処理を起動している
			fmt.Fprintf(&b, "note: 処理のプロセスグループ (pgid %d) はまだ残っている。処理は終了の便なしで走り続けている可能性がある\n",
				r.SupervisorPID)
		}
	case n.Result.Error != "":
		fmt.Fprintf(&b, "result: FAILED — %s\n", n.Result.Error)
	case n.Result.Signal != "":
		fmt.Fprintf(&b, "result: killed by signal %s\n", n.Result.Signal)
	default:
		fmt.Fprintf(&b, "result: exit code %d\n", n.Result.ExitCode)
	}
	if n.Result != nil {
		if t, err := time.Parse(time.RFC3339Nano, n.Result.EndedAt); err == nil {
			ended = t
		}
		fmt.Fprintf(&b, "ended_at: %s\n", n.Result.EndedAt)
	}
	if started, err := time.Parse(time.RFC3339Nano, r.StartedAt); err == nil {
		fmt.Fprintf(&b, "elapsed: %s\n", ended.Sub(started).Round(time.Second))
	}
	fmt.Fprintf(&b, "output: %s\n", r.LogPath)
	if tail := readTail(r.LogPath, detachLogTailBytes); tail != "" {
		fmt.Fprintf(&b, "output tail:\n```\n%s\n```\n", tail)
	}
	return b.String()
}

// formatDetachPrompt は便を受けた claude への prompt。
func formatDetachPrompt(selfHandle string, n detachNotice, now time.Time, footer string) string {
	r := n.Record
	causedBy := n.noticeMessageID()
	origin := fmt.Sprintf("%s からの依頼", r.Session)
	if r.CausedBy != "" {
		origin += fmt.Sprintf(" (message ID `%s`)", r.CausedBy)
	}
	base := fmt.Sprintf(
		"あなたは agent-hub の peer worker `%s` として動いています。\n"+
			"あなたが以前 `detach` で切り離して流した処理が終わりました (bridge からの便 — issue #395)。\n"+
			"この処理は %s の作業中に流したものです。\n\n"+
			"%s\n"+
			"結果を確かめて作業の続きを進め、結果や状況を "+
			"`mcp__agent-hub__send_message` で %s へ送ってください。\n"+
			"その際、`caused_by` に `%s` を設定してください (因果チェーン追跡 — issue #162)。\n"+
			"宛先 (to) は必ず `%s` を指定。team 宛 broadcast は避け、送信者個人へ DM で返すこと。",
		selfHandle, origin, n.body(now), r.Session, causedBy, r.Session,
	)
	return base + detachGuidance + footerInstruction(footer)
}

// detachGuidance は毎回の prompt に添える案内 (issue #395)。
const detachGuidance = "\n\n【ターンを越える処理 (issue #395)】このターンが終わると claude は止まり、" +
	"claude の中の background task も止まります。このターンの中で終わらない処理は、次のように切り離して" +
	"流してください。終わると (終了コードによらず) 終了コード・出力の末尾を載せた便がこのセッションに届くので、" +
	"続きはそこで進めます。@scheduler で確認の便を予約する必要はありません。\n" +
	"  \"$AGENT_HUB_BRIDGE_BIN\" detach -- '<command>'"

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

func detachFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
