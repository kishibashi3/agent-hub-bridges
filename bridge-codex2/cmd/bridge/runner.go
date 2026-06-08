// runner.go — codexRunner (on-demand subprocess モード)
//
// bridge-claude2 の claudeRunner に相当するが、claude CLI の代わりに
// codex CLI を使う。subprocess の寿命は 1 query に限定する (on-demand)。
//
// セッション継続の仕組み:
//   - 初回: `codex exec ... -` でプロンプトを stdin から渡す (新規セッション)
//   - 2回目以降: `codex exec resume --last ... -` で最新セッションを継続
//   - セッション状態は CODEX_HOME (永続ディレクトリ) に保存されるため bridge 再起動後も継続可能
//   - hasSession フラグ: 起動時に CODEX_HOME 内の marker file を確認、実行成功後に書き込む
//
// プロンプト受け渡し:
//   - `-` を引数に渡して stdin 経由でプロンプトを渡す (長いプロンプトに対応)
//   - agent-hub MCP config は CODEX_HOME/config.toml 経由で codex に渡す
//   - identity env vars (CODEX_BRIDGE_USER_ID / CODEX_BRIDGE_TENANT_ID) を subprocess env にセット
//
// Issue: #186
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const sessionMarkerFile = ".bridge_has_session"

// codexEvent は codex --json JSONL 出力の 1 イベント。
// 全フィールドを網羅する必要はなく、activity tracking に必要な部分のみ定義する。
type codexEvent struct {
	Type string `json:"type"`
}

// codexRunner は codex CLI subprocess の設定を保持する。
// on-demand モードでは subprocess はフィールドとして保持せず、
// query() が呼ばれるたびに新しい subprocess を spawn する。
type codexRunner struct {
	cfg           *config
	hasSession    bool   // true = 継続すべき session が CODEX_HOME に存在する
	sessionMarker string // hasSession を永続化する marker file のパス
}

// newCodexRunner は codexRunner を生成する。
// CODEX_HOME 内の marker file が存在すれば hasSession = true で初期化する。
func newCodexRunner(cfg *config) *codexRunner {
	marker := filepath.Join(cfg.CodexHomeDir, sessionMarkerFile)
	r := &codexRunner{
		cfg:           cfg,
		sessionMarker: marker,
	}
	if _, err := os.Stat(marker); err == nil {
		r.hasSession = true
		slog.Info("runner: previous session detected — will resume with --last", "marker", marker)
	} else {
		slog.Info("runner: no previous session — first exec will create a new session")
	}
	return r
}

// restart は on-demand モードでは no-op。
// CommandRouter の /restart ハンドラから呼ばれる。
func (r *codexRunner) restart(_ context.Context) error {
	slog.Info("runner: /restart — on-demand mode, no persistent subprocess to restart")
	return nil
}

// query は user prompt を codex に送り、subprocess 完了を待つ。
// メッセージ受信ごとに新しい subprocess を spawn し、完了後に終了させる (on-demand)。
// 返り値の queryUsage は telemetry.emitSpan に渡す。codex は token usage を
// JSON イベントとして出力しないため、フィールドは基本的に 0 となる。
func (r *codexRunner) query(ctx context.Context, prompt, _ string, tracker *activityTracker) (queryUsage, error) {
	queryCtx := ctx
	if r.cfg.SubprocessTimeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, r.cfg.SubprocessTimeout)
		defer cancel()
	}

	args := r.buildArgs()
	cmd := exec.CommandContext(queryCtx, r.cfg.CodexCLI, args...)
	cmd.Dir = r.cfg.Workdir
	cmd.Stderr = os.Stderr
	cmd.Env = r.buildEnv()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return queryUsage{IsError: true}, fmt.Errorf("runner: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		return queryUsage{IsError: true}, fmt.Errorf("runner: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		return queryUsage{IsError: true}, fmt.Errorf("runner: start codex subprocess: %w", err)
	}

	pid := cmd.Process.Pid
	mode := "new-session"
	if r.hasSession {
		mode = "resume"
	}
	slog.Info("runner: codex subprocess started", "pid", pid, "mode", mode)

	// subprocess が active であることを tracker に通知
	if tracker != nil {
		tracker.markActive()
	}

	// プロンプトを stdin に書き込み、EOF を送って codex に処理を開始させる
	if _, err := fmt.Fprintln(stdinPipe, prompt); err != nil {
		stdinPipe.Close()
		_ = cmd.Wait()
		return queryUsage{IsError: true}, fmt.Errorf("runner: write prompt to stdin: %w", err)
	}
	stdinPipe.Close()

	// stdout (JSONL) を読み取りながら activity tracking を行う
	usage := r.readOutput(queryCtx, stdoutPipe, tracker)

	if err := cmd.Wait(); err != nil {
		usage.IsError = true
		slog.Error("runner: codex subprocess exited with error", "pid", pid, "err", err)
		return usage, fmt.Errorf("codex subprocess error (pid=%d): %w", pid, err)
	}

	slog.Info("runner: codex subprocess finished", "pid", pid)

	// 成功: 次回から resume を使えるようにする
	r.markSessionExists()

	return usage, nil
}

// buildArgs は hasSession に応じて codex CLI 引数を組み立てる。
func (r *codexRunner) buildArgs() []string {
	if r.hasSession {
		return r.buildResumeArgs()
	}
	return r.buildExecArgs()
}

// buildExecArgs は新規セッション用の引数を組み立てる。
//
//	codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --json
//	           -C <workdir> [--add-dir <dir>]... [-m <model>] -
func (r *codexRunner) buildExecArgs() []string {
	args := []string{
		"exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		"--json",
		"-C", r.cfg.Workdir,
	}
	if r.cfg.Model != "" {
		args = append(args, "-m", r.cfg.Model)
	}
	for _, dir := range r.cfg.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	args = append(args, "-") // stdin からプロンプトを読む
	return args
}

// buildResumeArgs は継続セッション用の引数を組み立てる。
//
//	codex exec resume --last --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --json [-m <model>] -
//
// resume サブコマンドは -C / --add-dir をサポートしないが、cmd.Dir で workdir を指定するため問題なし。
func (r *codexRunner) buildResumeArgs() []string {
	args := []string{
		"exec", "resume", "--last",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		"--json",
	}
	if r.cfg.Model != "" {
		args = append(args, "-m", r.cfg.Model)
	}
	args = append(args, "-") // stdin からプロンプトを読む
	return args
}

// buildEnv は subprocess に渡す環境変数を組み立てる。
//
// CODEX_HOME を永続ディレクトリで上書きし、bridge identity 変数をセットする。
// config.toml の env_http_headers はこれらの変数名を参照して値を解決する。
func (r *codexRunner) buildEnv() []string {
	env := os.Environ()
	// 既存の CODEX_HOME / identity 変数を上書きするためマップを作る
	overrides := map[string]string{
		"CODEX_HOME":   r.cfg.CodexHomeDir,
		envUserID:      r.cfg.User,
		"GITHUB_PAT":   r.cfg.GitHubPAT,
	}
	if r.cfg.Tenant != "" {
		overrides[envTenantID] = r.cfg.Tenant
	}

	// 既存 env の中で上書き対象のキーを置換
	result := make([]string, 0, len(env)+len(overrides))
	replaced := make(map[string]bool)
	for _, kv := range env {
		for k, v := range overrides {
			if before, _, found := strings.Cut(kv, "="); found && before == k {
				kv = k + "=" + v
				replaced[k] = true
				break
			}
		}
		result = append(result, kv)
	}
	// 既存 env になかったキーを追加
	for k, v := range overrides {
		if !replaced[k] {
			result = append(result, k+"="+v)
		}
	}
	return result
}

// readOutput は codex --json JSONL 出力を読み取り、activity tracking を行う。
// subprocess が exit するまでブロックする。
//
// ctx キャンセル時は scanner.Scan() を即抜けするが、stdout の残データを drain しない。
// subprocess 側はパイプバッファが詰まると write ブロックまたは SIGPIPE で hang する恐れがある。
// cmd.Wait() はその後も呼ばれるため、subprocess が自己終了しない場合は
// SubprocessTimeout (WithTimeout) による強制終了に委ねる設計。
func (r *codexRunner) readOutput(ctx context.Context, stdout io.Reader, tracker *activityTracker) queryUsage {
	var usage queryUsage
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return usage
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		var ev codexEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			slog.Debug("runner: codex JSONL parse skip", "line", truncate(line, 120))
			continue
		}

		slog.Debug("runner: codex event", "type", ev.Type)

		// agent 系イベントで tracker を更新
		switch ev.Type {
		case "agent_message", "message", "reasoning", "tool_call", "tool_result":
			if tracker != nil {
				tracker.markActive()
			}
		}
	}

	if err := scanner.Err(); err != nil {
		slog.Warn("runner: codex stdout scan error", "err", err)
	}
	return usage
}

// markSessionExists は session marker file を書き込む。
// 次回起動時に hasSession = true で初期化できるようにする。
func (r *codexRunner) markSessionExists() {
	if r.hasSession {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(r.sessionMarker, []byte(ts+"\n"), 0o600); err != nil {
		slog.Warn("runner: failed to write session marker", "path", r.sessionMarker, "err", err)
		return
	}
	r.hasSession = true
	slog.Info("runner: session marker written — next startup will resume", "path", r.sessionMarker)
}
