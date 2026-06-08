// Package tmux manages interactive Claude Code sessions via tmux (Tier2).
//
// 各 peer に 1 つの tmux セッションを割り当て、以下を行う:
//   - Start(): tmux new-session + claude 起動コマンド送信
//   - Stop(): Ctrl+C → 5 秒後に kill-session
//   - InjectMessage(): named buffer 経由でメッセージを貼り付け + Enter
//   - WaitForIdle(): pane 変化ゼロ N 秒 → 応答完了
//
// 応答は claude が MCP tool (send_message) 経由で自律的に送信する。
// bridge (Tier1) は応答テキストを解析する必要がない。
//
// Issue: #110
package tmux

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	defaultPollInterval    = 500 * time.Millisecond
	defaultGracefulWait    = 5 * time.Second
	defaultMinActivityWait = 1 * time.Second
)

// tmuxRunner abstracts tmux exec.Command calls for testability.
// Production uses realRunner; tests inject a mock.
type tmuxRunner interface {
	run(args ...string) error
	runCtx(ctx context.Context, args ...string) error
	runWithStdin(stdin io.Reader, args ...string) error
	output(args ...string) ([]byte, error)
}

// SessionIface is the interface implemented by *Session.
// Use it to inject mock sessions in tests (e.g. in SessionManager).
type SessionIface interface {
	IsAlive() bool
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	InjectMessage(text string) error
	WaitForIdle(ctx context.Context) error
}

// Compile-time assertion: *Session must implement SessionIface.
var _ SessionIface = (*Session)(nil)

// realRunner is the production tmuxRunner using exec.Command.
type realRunner struct{}

func (realRunner) run(args ...string) error {
	return exec.Command("tmux", args...).Run()
}

func (realRunner) runCtx(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "tmux", args...).Run()
}

func (realRunner) runWithStdin(stdin io.Reader, args ...string) error {
	cmd := exec.Command("tmux", args...)
	cmd.Stdin = stdin
	return cmd.Run()
}

func (realRunner) output(args ...string) ([]byte, error) {
	return exec.Command("tmux", args...).Output()
}

// Session は 1 peer に対応する tmux セッション。
type Session struct {
	Name             string
	Workdir          string
	MCPConfigPath    string
	ClaudeCLI        string
	Model            string
	BypassPerms      bool
	startedBefore    bool
	SpawnTimeout     time.Duration
	ActivityIdleTime time.Duration
	ResponseTimeout  time.Duration
	// Env は Tier2 (claude) 起動時に追加設定する環境変数。
	// fleet mode で persona ごとに異なる env を渡す用途。
	// キーはソートされてコマンド先頭に KEY='val' 形式で付加される。
	Env map[string]string
	// Timing overrides — zero means use default constant.
	PollInterval    time.Duration // default: 500ms
	GracefulWait    time.Duration // default: 5s
	MinActivityWait time.Duration // default: 1s
	runner          tmuxRunner
}

// SessionOptions は NewSession のオプション。
type SessionOptions struct {
	UserID           string
	Workdir          string
	MCPConfigPath    string
	ClaudeCLI        string
	Model            string
	BypassPerms      bool
	SpawnTimeout     time.Duration
	ActivityIdleTime time.Duration
	ResponseTimeout  time.Duration
	// Env は persona ごとの追加環境変数 (fleet mode 用)。
	Env map[string]string
}

// NewSession は Session を生成する (tmux セッションはまだ作らない)。
func NewSession(opts SessionOptions) *Session {
	name := "claude-bridge-" + opts.UserID
	return &Session{
		Name:             name,
		Workdir:          opts.Workdir,
		MCPConfigPath:    opts.MCPConfigPath,
		ClaudeCLI:        opts.ClaudeCLI,
		Model:            opts.Model,
		BypassPerms:      opts.BypassPerms,
		SpawnTimeout:     opts.SpawnTimeout,
		ActivityIdleTime: opts.ActivityIdleTime,
		ResponseTimeout:  opts.ResponseTimeout,
		Env:              opts.Env,
		runner:           realRunner{},
	}
}

func (s *Session) pollInterval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return defaultPollInterval
}

func (s *Session) gracefulWait() time.Duration {
	if s.GracefulWait > 0 {
		return s.GracefulWait
	}
	return defaultGracefulWait
}

func (s *Session) minActivityWait() time.Duration {
	if s.MinActivityWait > 0 {
		return s.MinActivityWait
	}
	return defaultMinActivityWait
}

// ──────────────────────────────────────────────────────────────────────── //
// ライフサイクル                                                           //
// ──────────────────────────────────────────────────────────────────────── //

// IsAlive は tmux セッションが存在するか確認する。
func (s *Session) IsAlive() bool {
	return s.runner.run("has-session", "-t", s.Name) == nil
}

// Start は tmux セッションを新規作成して claude を起動する。
// s.startedBefore が true なら --continue で会話を継続する。
func (s *Session) Start(ctx context.Context) error {
	if s.IsAlive() {
		log.Printf("[tmux] session %s already exists — stopping first", s.Name)
		if err := s.Stop(ctx); err != nil {
			return err
		}
	}

	log.Printf("[tmux] creating session %s (workdir=%s)", s.Name, s.Workdir)
	if err := s.runner.runCtx(ctx,
		"new-session", "-d",
		"-s", s.Name,
		"-c", s.Workdir,
	); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}

	cmdStr := s.buildCLICommand()
	log.Printf("[tmux] starting claude: %s", cmdStr)
	if err := s.runner.runCtx(ctx, "send-keys", "-t", s.Name, cmdStr, "Enter"); err != nil {
		// send-keys 失敗時は空セッションが残留しないよう best-effort cleanup する
		_ = s.Stop(ctx)
		return fmt.Errorf("tmux send-keys (start): %w", err)
	}

	// claude が起動して pane に何か出力するまで待つ
	if wait := s.minActivityWait(); wait > 0 {
		time.Sleep(wait)
	}
	deadline := time.Now().Add(s.SpawnTimeout)
	baseline := s.capturePaneText()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		content := s.capturePaneText()
		if content != baseline && strings.TrimSpace(content) != "" {
			log.Printf("[tmux] session %s started — got initial output", s.Name)
			s.startedBefore = true
			return nil
		}
		time.Sleep(s.pollInterval())
	}

	_ = s.Stop(ctx)
	return fmt.Errorf("claude did not start within %.0fs in session %s",
		s.SpawnTimeout.Seconds(), s.Name)
}

// Stop は tmux セッションを停止する (graceful → force kill)。
func (s *Session) Stop(ctx context.Context) error {
	if !s.IsAlive() {
		return nil
	}
	log.Printf("[tmux] stopping session %s", s.Name)

	// Ctrl+C で graceful 終了を試みる
	_ = s.runner.run("send-keys", "-t", s.Name, "C-c", "")

	// gracefulWait 待って、まだ生きていれば force kill
	gracefulTimer := time.NewTimer(s.gracefulWait())
	defer gracefulTimer.Stop()
	select {
	case <-gracefulTimer.C:
	case <-ctx.Done():
	}

	if s.IsAlive() {
		_ = s.runner.run("kill-session", "-t", s.Name)
	}
	log.Printf("[tmux] session %s stopped", s.Name)
	return nil
}

// ──────────────────────────────────────────────────────────────────────── //
// メッセージング                                                           //
// ──────────────────────────────────────────────────────────────────────── //

// InjectMessage はプロンプトテキストを tmux ペインに貼り付け Enter を送る。
// named buffer を使うことで、同一ホストで複数 bridge が動いても buffer 競合しない。
func (s *Session) InjectMessage(text string) error {
	bufName := "bridge-" + s.Name

	if err := s.runner.runWithStdin(strings.NewReader(text),
		"load-buffer", "-b", bufName, "-",
	); err != nil {
		return fmt.Errorf("tmux load-buffer: %w", err)
	}

	if err := s.runner.run("paste-buffer", "-b", bufName, "-t", s.Name); err != nil {
		return fmt.Errorf("tmux paste-buffer: %w", err)
	}

	if err := s.runner.run("send-keys", "-t", s.Name, "", "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys (enter): %w", err)
	}

	// buffer を削除 (PAT 等の機密情報をメモリから消す)
	_ = s.runner.run("delete-buffer", "-b", bufName)

	log.Printf("[tmux] injected %d chars to session %s", len(text), s.Name)
	return nil
}

// WaitForIdle は claude の応答完了を待つ。
//
// アルゴリズム:
//  1. pane 変化待ち (claude が処理を開始した証拠)
//  2. pane 変化が止まって ActivityIdleTime 秒経過 → 完了と判断
//  3. ResponseTimeout 超過 → error
func (s *Session) WaitForIdle(ctx context.Context) error {
	deadline := time.Now().Add(s.ResponseTimeout)
	baseline := s.capturePaneText()

	// Phase 1: pane が変化するまで待つ
	activityStarted := false
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		content := s.capturePaneText()
		if content != baseline {
			activityStarted = true
			baseline = content
			break
		}
		time.Sleep(s.pollInterval())
	}
	if !activityStarted {
		return fmt.Errorf("session %s: claude did not start processing within %.0fs",
			s.Name, s.ResponseTimeout.Seconds())
	}

	// Phase 2: pane 変化が止まるまで待つ
	lastContent := baseline
	lastChange := time.Now()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		time.Sleep(s.pollInterval())

		// tmux セッションが死んだ場合は capturePaneText が "" を返し続ける。
		// 「変化なし」→ idle timeout で false success にならないよう死活確認する。
		if !s.IsAlive() {
			return fmt.Errorf("session %s: tmux session died while waiting for response", s.Name)
		}

		content := s.capturePaneText()
		if content != lastContent {
			lastContent = content
			lastChange = time.Now()
		} else if time.Since(lastChange) >= s.ActivityIdleTime {
			log.Printf("[tmux] session %s idle for %.1fs — response complete",
				s.Name, s.ActivityIdleTime.Seconds())
			return nil
		}
	}

	return fmt.Errorf("session %s: response timeout (%.0fs)", s.Name, s.ResponseTimeout.Seconds())
}

// ──────────────────────────────────────────────────────────────────────── //
// 内部実装                                                                 //
// ──────────────────────────────────────────────────────────────────────── //

func (s *Session) capturePaneText() string {
	out, err := s.runner.output("capture-pane", "-p", "-S", "-", "-t", s.Name)
	if err != nil {
		return ""
	}
	return string(out)
}

// buildCLICommand は claude 起動コマンド文字列を組み立てる。
// シェル展開を避けるため各引数を単引用符でエスケープする。
// Env が設定されている場合は "KEY='val'" 形式でコマンド先頭に付加する。
// キーはソートして決定的な順序を保証する。
func (s *Session) buildCLICommand() string {
	var parts []string

	// 追加環境変数をコマンド先頭に付加 (KEY='val' 形式)
	if len(s.Env) > 0 {
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, k+"="+shellQuote(s.Env[k]))
		}
	}

	parts = append(parts, shellQuote(s.ClaudeCLI))
	parts = append(parts, "--mcp-config", shellQuote(s.MCPConfigPath))
	if s.startedBefore {
		parts = append(parts, "--continue")
	}
	if s.BypassPerms {
		// --dangerously-skip-permissions は interactive dialog を表示して pane が静止し
		// Start() の spawn タイムアウトを引き起こす。--permission-mode bypassPermissions
		// は dialog なしで即起動する (bridge-claude と同じ方式)。
		parts = append(parts, "--permission-mode", "bypassPermissions")
	}
	if s.Model != "" {
		parts = append(parts, "--model", shellQuote(s.Model))
	}
	return strings.Join(parts, " ")
}

// shellQuote は安全な shell 引用符を付ける (単引用符ベース)。
// 値に単引用符が含まれる場合は置換する (= POSIX 準拠)。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
