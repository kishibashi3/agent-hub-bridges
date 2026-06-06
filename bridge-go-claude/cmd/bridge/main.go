// bridge-go-claude: Go ネイティブ on-demand bridge for Claude Code.
//
// tmux を使わず claude を subprocess で直接起動する。
//
// 動作フロー:
//  1. MCP initialize ハンドシェイク (agent-hub-sdk/go)
//  2. register で agent-hub に自 peer を登録
//  3. ポーリングループ: get_messages → process → mark_as_read
//     - message 受信 → claude を subprocess で起動 (-p, --output-format stream-json)
//     - claude が mcp__agent-hub__send_message ツールを呼んで返信する
//     - bridge は subprocess の完了を待つ (stream-json を監視してエラー検知)
//     - subprocess エラー時のみ Go SDK SendMessage でエラー通知を送る
//  4. SIGTERM/Ctrl+C でグレースフルシャットダウン
//
// bridge-tmux との違い:
//   - tmux 不要 — セッション管理・spawn 検知の複雑さがない
//   - on-demand: メッセージごとに claude subprocess を起動して終了を待つ
//   - stream-json 出力を解析してエラー検知 (tmux は stdout スニフで idle 判定)
//
// 参考実装:
//   - bridge-tmux (bridge-go-claude/../bridge-tmux): MCP config, polling, エラー処理
//   - agent-hub-bridge-claude / bridges/claude (Python): on-demand 設計, prompt format
//
// 環境変数:
//   AGENT_HUB_URL      required    agent-hub MCP エンドポイント
//   GITHUB_PAT         required    GitHub Personal Access Token
//   AGENT_HUB_TENANT   optional    テナント ID (省略 = default tenant)
//   CLAUDE_CLI_PATH    optional    claude CLI のパス (省略 = PATH 上の "claude")
//   AGENT_HUB_MODEL    optional    Claude model (省略 = --model フラグ > claude default)
//
// Issue: #155
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// ──────────────────────────────────────────────────────────────────────── //
// 設定                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

const defaultModel = "claude-sonnet-4-6"

type config struct {
	User        string
	DisplayName string
	AgentHubURL string
	GitHubPAT   string
	Tenant      string
	Workdir     string
	ClaudeCLI   string
	Model       string
	LogLevel    string
	// PollInterval は get_messages のポーリング間隔。
	PollInterval time.Duration
	// ReconnectBackoff は MCP セッション再接続待機時間。
	ReconnectBackoff time.Duration
	// MaxRetries は circuit breaker の連続失敗上限 (0 = 無制限)。
	MaxRetries int
	// SubprocessTimeout は claude subprocess の最大実行時間。
	// 0 はタイムアウトなし (ctx のキャンセルのみ)。
	SubprocessTimeout time.Duration
}

func parseConfig() (*config, error) {
	var (
		user              = flag.String("user", "", "agent-hub handle (without @) [required]")
		displayName       = flag.String("display-name", "", "display name (optional)")
		workdir           = flag.String("workdir", "", "peer workdir with CLAUDE.md (default: cwd)")
		model             = flag.String("model", "", "Claude model override (default: AGENT_HUB_MODEL env or claude default)")
		logLevel          = flag.String("log-level", "info", "log level: debug|info|warn|error")
		pollInterval      = flag.Duration("poll-interval", 5*time.Second, "get_messages polling interval")
		reconnectBackoff  = flag.Duration("reconnect-backoff", 5*time.Second, "backoff on MCP reconnect")
		maxRetries        = flag.Int("max-retries", 10, "circuit breaker: max consecutive get_messages failures (0 = unlimited)")
		subprocessTimeout = flag.Duration("subprocess-timeout", 10*time.Minute, "claude subprocess max runtime (0 = no timeout)")
	)
	flag.Parse()

	if err := validateLogLevel(*logLevel); err != nil {
		return nil, err
	}
	if *user == "" {
		return nil, fmt.Errorf("--user is required")
	}

	url := os.Getenv("AGENT_HUB_URL")
	if url == "" {
		return nil, fmt.Errorf("AGENT_HUB_URL is not set")
	}
	pat := os.Getenv("GITHUB_PAT")
	if pat == "" {
		return nil, fmt.Errorf("GITHUB_PAT is not set")
	}

	wd := *workdir
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("getwd: %w", err)
		}
	}
	wd, _ = filepath.Abs(wd)
	if _, err := os.Stat(wd); err != nil {
		return nil, fmt.Errorf("workdir %q: %w", wd, err)
	}

	// CLAUDE_CLI_PATH が未設定なら PATH 上の "claude" を使う
	claudeCLI := os.Getenv("CLAUDE_CLI_PATH")
	if claudeCLI == "" {
		claudeCLI = "claude"
	}
	if _, err := exec.LookPath(claudeCLI); err != nil {
		return nil, fmt.Errorf("claude CLI %q not found in PATH: %w", claudeCLI, err)
	}

	// model: --model フラグ > AGENT_HUB_MODEL env > "" (= claude default)
	resolvedModel := *model
	if resolvedModel == "" {
		resolvedModel = os.Getenv("AGENT_HUB_MODEL")
	}

	// display_name: --display-name フラグ > "{user} — go bridge (on-demand)"
	resolvedDisplayName := *displayName
	if resolvedDisplayName == "" {
		resolvedDisplayName = *user + " — go bridge (on-demand)"
	}

	return &config{
		User:              *user,
		DisplayName:       resolvedDisplayName,
		AgentHubURL:       url,
		GitHubPAT:         pat,
		Tenant:            os.Getenv("AGENT_HUB_TENANT"),
		Workdir:           wd,
		ClaudeCLI:         claudeCLI,
		Model:             resolvedModel,
		LogLevel:          *logLevel,
		PollInterval:      *pollInterval,
		ReconnectBackoff:  *reconnectBackoff,
		MaxRetries:        *maxRetries,
		SubprocessTimeout: *subprocessTimeout,
	}, nil
}

func validateLogLevel(level string) error {
	switch level {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("--log-level %q is invalid: must be debug|info|warn|error", level)
	}
}

func setupLogger(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "info":
		l = slog.LevelInfo
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		panic(fmt.Sprintf("setupLogger: unexpected log level %q", level))
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(handler))
}

// ──────────────────────────────────────────────────────────────────────── //
// MCP config ファイル                                                      //
// ──────────────────────────────────────────────────────────────────────── //

// writeMCPConfig は claude subprocess に渡す agent-hub MCP config を
// 一時ファイルに書き出す。ファイルパスを返す。呼出元が defer os.Remove を担当する。
//
// PAT をコマンドライン引数 (ps で見える) に渡さないためファイル経由にする。
// bridge-tmux の writeMCPConfig と同一設計。
func writeMCPConfig(cfg *config) (string, error) {
	headers := map[string]string{
		"Authorization": "Bearer " + cfg.GitHubPAT,
		"X-User-Id":     cfg.User,
	}
	if cfg.Tenant != "" {
		headers["X-Tenant-Id"] = cfg.Tenant
	}

	payload := map[string]any{
		"mcpServers": map[string]any{
			"agent-hub": map[string]any{
				"type":    "http",
				"url":     cfg.AgentHubURL,
				"headers": headers,
			},
		},
	}

	f, err := os.CreateTemp("", fmt.Sprintf("bridge-go-claude-%s-*.json", cfg.User))
	if err != nil {
		return "", fmt.Errorf("create mcp config temp file: %w", err)
	}
	defer f.Close()

	if err := os.Chmod(f.Name(), 0o600); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("chmod mcp config: %w", err)
	}
	if err := json.NewEncoder(f).Encode(payload); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write mcp config: %w", err)
	}

	slog.Debug("wrote MCP config", "path", f.Name())
	return f.Name(), nil
}

// ──────────────────────────────────────────────────────────────────────── //
// プロンプトフォーマット                                                   //
// ──────────────────────────────────────────────────────────────────────── //

// formatPrompt は受信メッセージを claude への user prompt に変換する。
//
// Python bridge (bridges/claude/_common/prompt.py) の format_peer_message_prompt
// と同等。claude が mcp__agent-hub__send_message を使って返信するよう促す。
// caused_by に受信メッセージ ID を設定するよう指示する (issue #162)。
func formatPrompt(selfHandle string, msg agenthub.Message) string {
	return fmt.Sprintf(
		"あなたは agent-hub の peer worker `%s` として動いています。\n"+
			"agent-hub 経由で %s から以下の message が届きました。\n"+
			"宛先: %s\n"+
			"本文:\n%s\n\n"+
			"内容に応じて作業し、返答が必要なら "+
			"`mcp__agent-hub__send_message` で %s へ送り返してください。\n"+
			"その際、`caused_by` に今回の受信メッセージ ID `%s` を設定してください"+
			" (因果チェーン追跡 — issue #162)。\n"+
			"宛先 (to) は必ず `%s` を指定。team 宛 broadcast は避け、"+
			"送信者個人へ DM で返すこと。",
		selfHandle,
		msg.Sender, msg.To, msg.Body,
		msg.Sender, msg.ID,
		msg.Sender,
	)
}

// ──────────────────────────────────────────────────────────────────────── //
// claude subprocess                                                        //
// ──────────────────────────────────────────────────────────────────────── //

// streamEvent は claude --output-format stream-json の 1 行を表す。
// 完全なスキーマではなく、エラー検知に必要なフィールドのみ。
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// runClaude は claude を -p (print) モードで subprocess として起動し、
// stream-json 出力を監視してエラーを検知する。
//
// 設計:
//   - claude は MCP config (agent-hub) を持つため、mcp__agent-hub__send_message で
//     自分で返信する。bridge は send_message を呼ばない (エラー時を除く)。
//   - stream-json の "result" イベントで is_error=true を検知した場合、error を返す。
//   - subprocess の exit code が非ゼロでも result イベントが得られていれば
//     そちらを優先する (claude が partial result を出して失敗するケースを考慮)。
//   - ctx がキャンセルされると exec.CommandContext が subprocess を kill する。
//
// cfg.SubprocessTimeout > 0 の場合は追加タイムアウトを設ける。
// 0 の場合は ctx のキャンセルのみで制御する。
func runClaude(ctx context.Context, cfg *config, mcpConfigPath, prompt string) error {
	runCtx := ctx
	if cfg.SubprocessTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cfg.SubprocessTimeout)
		defer cancel()
	}

	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--permission-mode", "bypassPermissions",
		"--mcp-config", mcpConfigPath,
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}

	cmd := exec.CommandContext(runCtx, cfg.ClaudeCLI, args...)
	cmd.Dir = cfg.Workdir
	cmd.Stderr = os.Stderr // claude の stderr をそのまま bridge の stderr に流す

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude subprocess: %w", err)
	}
	slog.Debug("claude subprocess started", "pid", cmd.Process.Pid)

	// stream-json をスキャンして result イベントを探す。
	// エラーがあれば resultErr に記録して後で返す。
	var resultErr error
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 512*1024), 512*1024) // 512 KB — tool result は大きくなる可能性あり
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			slog.Debug("stream-json parse skip (non-JSON line)", "line", truncate(line, 120))
			continue
		}
		slog.Debug("stream-json event", "type", ev.Type, "subtype", ev.Subtype)
		if ev.Type == "result" {
			if ev.IsError {
				resultErr = fmt.Errorf("claude result error (subtype=%s): %s",
					ev.Subtype, truncate(ev.Result, 300))
			}
			break // result イベントを受信したら読み取り完了
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		slog.Warn("stream-json scan error", "err", scanErr)
	}

	// subprocess の終了を待つ。
	waitErr := cmd.Wait()
	slog.Debug("claude subprocess finished", "exit_err", waitErr)

	// result イベントでエラーを検知していた場合はそちらを優先する。
	if resultErr != nil {
		return resultErr
	}
	// result イベントがなくて exit code が非ゼロの場合は wait エラーを返す。
	if waitErr != nil {
		return fmt.Errorf("claude subprocess exited with error: %w", waitErr)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────── //
// メッセージ処理                                                           //
// ──────────────────────────────────────────────────────────────────────── //

// handleMessage は 1 件のメッセージを処理する。
//
// 処理順:
//  1. 自己ループ防止 (sender == selfHandle は skip + ack)
//  2. workdir 存在確認 (なければエラー DM + ack)
//  3. プロンプト生成 → claude subprocess 起動 → 完了待ち
//  4. subprocess エラー時: SendMessage でエラー通知
//  5. MarkAsRead (成功・失敗問わず)
func handleMessage(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	mcpConfigPath string,
	selfHandle string,
	msg agenthub.Message,
) {
	// 自己ループ防止: bridge が送信したメッセージを再処理しない
	if msg.Sender == selfHandle {
		slog.Debug("skip self-sent message", "msg_id", msg.ID)
		if err := client.MarkAsRead(ctx, msg.ID); err != nil {
			slog.Warn("mark_as_read (self-skip) failed", "msg_id", msg.ID, "err", err)
		}
		return
	}

	slog.Info("message received",
		"msg_id", msg.ID,
		"from", msg.Sender,
		"body_preview", truncate(msg.Body, 120))

	// issue #51: workdir が存在しない場合はエラー DM を返して ack して早期 return
	// (crash-ack ループ防止。workdir がなくなった場合は bridge を再起動すること)
	if _, err := os.Stat(cfg.Workdir); err != nil {
		slog.Error("workdir gone — sending error DM and acking",
			"workdir", cfg.Workdir, "msg_id", msg.ID)
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = client.SendMessage(sendCtx, msg.Sender,
			fmt.Sprintf("(auto) bridge workdir does not exist: %s", cfg.Workdir), msg.ID)
		if err := client.MarkAsRead(ctx, msg.ID); err != nil {
			slog.Warn("mark_as_read (workdir-missing) failed", "msg_id", msg.ID, "err", err)
		}
		return
	}

	prompt := formatPrompt(selfHandle, msg)
	if err := runClaude(ctx, cfg, mcpConfigPath, prompt); err != nil {
		slog.Error("claude subprocess error",
			"msg_id", msg.ID, "from", msg.Sender, "err", err)
		// エラーを送信元に通知する
		errMsg := fmt.Sprintf("(auto) bridge-go-claude error: %v", err)
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if sendErr := client.SendMessage(sendCtx, msg.Sender, errMsg, msg.ID); sendErr != nil {
			slog.Error("fallback send_message failed", "msg_id", msg.ID, "err", sendErr)
		}
	} else {
		slog.Info("message processed", "msg_id", msg.ID, "from", msg.Sender)
	}

	if err := client.MarkAsRead(ctx, msg.ID); err != nil {
		slog.Warn("mark_as_read failed", "msg_id", msg.ID, "err", err)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// ポーリングループ                                                         //
// ──────────────────────────────────────────────────────────────────────── //

// runBridge はポーリングループを回す。circuit breaker で MaxRetries 回連続失敗したら
// error を返して呼出元の reconnect ループに入る。
func runBridge(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	mcpConfigPath string,
	selfHandle string,
) error {
	consecutiveFailures := 0

	slog.Info("polling inbox",
		"handle", selfHandle,
		"poll_interval_s", cfg.PollInterval.Seconds())

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msgs, err := client.GetMessages(ctx)
		if err != nil {
			consecutiveFailures++
			slog.Warn("get_messages error",
				"consecutive", consecutiveFailures, "err", err)
			if cfg.MaxRetries > 0 && consecutiveFailures >= cfg.MaxRetries {
				return fmt.Errorf("circuit breaker: %d consecutive get_messages failures",
					consecutiveFailures)
			}
			sleepWithContext(ctx, cfg.ReconnectBackoff)
			continue
		}
		consecutiveFailures = 0

		for _, msg := range msgs {
			handleMessage(ctx, cfg, client, mcpConfigPath, selfHandle, msg)
		}

		sleepWithContext(ctx, cfg.PollInterval)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// main                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(2)
	}

	setupLogger(cfg.LogLevel)

	slog.Info("bridge-go-claude starting",
		"handle", "@"+cfg.User,
		"workdir", cfg.Workdir,
		"model", orDefault(cfg.Model, "(claude default)"),
		"poll_interval_s", cfg.PollInterval.Seconds(),
		"subprocess_timeout_s", cfg.SubprocessTimeout.Seconds())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// MCP config ファイルを書く (claude subprocess が mcp__agent-hub__* tools を使うため)
	mcpConfigPath, err := writeMCPConfig(cfg)
	if err != nil {
		slog.Error("writeMCPConfig failed", "err", err)
		os.Exit(1)
	}
	// NOTE: defer os.Remove の後に os.Exit を呼ぶと defer がスキップされるため、
	// 以降の fatal は fatalCleanup を経由する。
	fatalCleanup := func(msg string, args ...any) {
		os.Remove(mcpConfigPath)
		slog.Error(msg, args...)
		os.Exit(1)
	}
	defer os.Remove(mcpConfigPath)

	// agent-hub SDK クライアント初期化
	client, err := agenthub.New(
		cfg.AgentHubURL, cfg.GitHubPAT, cfg.User, cfg.Tenant,
		agenthub.WithClientName("bridge-go-claude"),
	)
	if err != nil {
		fatalCleanup("agenthub.New failed", "err", err)
	}

	// MCP initialize
	if err := client.Initialize(ctx); err != nil {
		fatalCleanup("MCP initialize failed", "err", err)
	}

	// register
	registered, err := client.Register(ctx, cfg.DisplayName, "stateless")
	if err != nil {
		fatalCleanup("register failed", "err", err)
	}
	slog.Info("registered", "result", strings.SplitN(registered, "\n", 2)[0])

	selfHandle := "@" + cfg.User

	// ポーリングループ (reconnect あり)
	for {
		err := runBridge(ctx, cfg, client, mcpConfigPath, selfHandle)
		if ctx.Err() != nil {
			slog.Info("bridge-go-claude shutting down")
			return
		}
		if err != nil {
			slog.Warn("runBridge ended with error — reconnecting", "err", err)
		}

		slog.Info("reconnecting", "backoff_s", cfg.ReconnectBackoff.Seconds())
		sleepWithContext(ctx, cfg.ReconnectBackoff)
		if ctx.Err() != nil {
			slog.Info("bridge-go-claude shutting down")
			return
		}

		// MCP セッションを再確立する
		if err := client.Initialize(ctx); err != nil {
			slog.Warn("re-initialize failed", "err", err)
			continue
		}
		if _, err := client.Register(ctx, cfg.DisplayName, "stateless"); err != nil {
			slog.Warn("re-register failed", "err", err)
			continue
		}
		slog.Info("reconnected and re-registered", "handle", selfHandle)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// ユーティリティ                                                           //
// ──────────────────────────────────────────────────────────────────────── //

func sleepWithContext(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func orDefault(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
