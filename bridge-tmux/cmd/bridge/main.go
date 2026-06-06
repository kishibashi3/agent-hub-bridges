// bridge-tmux: tmux-backed interactive Claude Code bridge (Tier1 in Go).
//
// 動作フロー:
//  1. MCP initialize ハンドシェイク
//  2. register で agent-hub に自 peer を登録
//  3. ポーリングループ: get_messages → process → mark_as_read
//     - message 受信 → SessionManager.Handle() → tmux session (Tier2) にメッセージを注入
//     - Tier2 (claude) が MCP send_message を呼んで返信
//     - bridge は wait_for_idle で待つだけ (応答テキストを解析しない)
//  4. idle timer: 最終処理完了から idle_timeout 後に tmux session を kill
//  5. SIGTERM/Ctrl+C でグレースフルシャットダウン
//
// 環境変数:
//   AGENT_HUB_URL    required
//   GITHUB_PAT       required
//   AGENT_HUB_TENANT optional
//
// Issue: #110, #142
package main

import (
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
	"sync"
	"syscall"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
	"github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/tmux"
)

// ──────────────────────────────────────────────────────────────────────── //
// 設定                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

type config struct {
	User             string
	DisplayName      string
	AgentHubURL      string
	GitHubPAT        string
	Tenant           string
	Workdir          string
	ClaudeCLI        string
	Model            string
	BypassPerms      bool
	IdleTimeout      time.Duration
	SpawnTimeout     time.Duration
	ActivityIdle     time.Duration
	ResponseTimeout  time.Duration
	PollInterval     time.Duration
	ReconnectBackoff time.Duration
	MaxRetries       int
	// FleetFile は --fleet フラグで指定した YAML ファイルパス。
	// 空文字なら single-persona モード (--user フラグ使用)。
	FleetFile string
	// LogLevel は --log-level フラグ。debug/info/warn/error (default: info)。
	LogLevel string
	// HealthPort は --health-port フラグ。0 なら /health サーバー無効。
	HealthPort int
}

func parseConfig() (*config, error) {
	var (
		user         = flag.String("user", "", "agent-hub handle (single-persona mode, without @)")
		displayName  = flag.String("display-name", "", "display name (optional)")
		workdir      = flag.String("workdir", "", "peer workdir with CLAUDE.md (default: cwd)")
		model        = flag.String("model", "", "Claude model (default: claude default)")
		idleTimeout  = flag.Duration("idle-timeout", 10*time.Minute, "idle kill timeout")
		spawnTimeout = flag.Duration("spawn-timeout", 60*time.Second, "timeout for Tier2 (claude) initial output after spawn")
		noBypass     = flag.Bool("no-bypass-permissions", false, "disable --permission-mode bypassPermissions")
		tenant       = flag.String("tenant", "", "agent-hub tenant ID (overrides AGENT_HUB_TENANT env)")
		fleetFile    = flag.String("fleet", "", "fleet YAML config file (multi-persona mode)")
		logLevel     = flag.String("log-level", "info", "log level: debug|info|warn|error")
		healthPort   = flag.Int("health-port", 0, "HTTP /health port (0 = disabled)")
	)
	flag.Parse()

	// --log-level 不正値は早期エラー (runtime fallback 禁止 — issue #149)
	if err := validateLogLevel(*logLevel); err != nil {
		return nil, err
	}

	// --fleet と --user は排他
	if *fleetFile != "" && *user != "" {
		return nil, fmt.Errorf("--fleet and --user are mutually exclusive")
	}
	if *fleetFile == "" && *user == "" {
		return nil, fmt.Errorf("either --user or --fleet is required")
	}

	url := os.Getenv("AGENT_HUB_URL")
	if url == "" {
		return nil, fmt.Errorf("AGENT_HUB_URL is not set")
	}
	pat := os.Getenv("GITHUB_PAT")
	if pat == "" {
		return nil, fmt.Errorf("GITHUB_PAT is not set")
	}

	// fleet モードでは workdir は persona ごとに YAML で指定するため省略可能。
	// single モードでは必須 (未指定なら cwd を使う)。
	wd := *workdir
	if *fleetFile == "" {
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
	}

	// CLAUDE_CLI_PATH が未設定なら PATH 上の "claude" を使う (Claude Code の default install)
	claudeCLI := os.Getenv("CLAUDE_CLI_PATH")
	if claudeCLI == "" {
		claudeCLI = "claude"
	}
	if _, err := exec.LookPath(claudeCLI); err != nil {
		return nil, fmt.Errorf("claude CLI %q not found in PATH: %w", claudeCLI, err)
	}

	return &config{
		User:             *user,
		DisplayName:      *displayName,
		AgentHubURL:      url,
		GitHubPAT:        pat,
		Tenant:           tenantValue(*tenant),
		Workdir:          wd,
		ClaudeCLI:        claudeCLI,
		Model:            *model,
		BypassPerms:      !*noBypass,
		IdleTimeout:      *idleTimeout,
		SpawnTimeout:     *spawnTimeout,
		ActivityIdle:     8 * time.Second,
		ResponseTimeout:  5 * time.Minute,
		PollInterval:     5 * time.Second,
		ReconnectBackoff: 5 * time.Second,
		MaxRetries:       10,
		FleetFile:        *fleetFile,
		LogLevel:         *logLevel,
		HealthPort:       *healthPort,
	}, nil
}

// ──────────────────────────────────────────────────────────────────────── //
// SessionManager: on-demand spawn + idle timer                            //
// ──────────────────────────────────────────────────────────────────────── //

// SessionManager は Tier2 セッションのライフサイクルと idle timer を管理する。
//
// # AfterFunc goroutine との race 防止
//
// time.AfterFunc の goroutine が session.Stop() を実行中に Handle() が
// session.Start() を呼ぶと race になる。timerDone channel を使って
// timer.Stop() が false (= 発火済み) だった場合に goroutine の完了を待つ。
type SessionManager struct {
	session   tmux.SessionIface
	cfg       *config
	idleTimer *time.Timer
	timerDone chan struct{} // AfterFunc goroutine 完了通知; nil = timer 未設定
	timerMu   sync.Mutex
	// onIdle は idle timeout で session が停止したときに呼ばれるコールバック。
	// nil なら何もしない。HealthState.SetSessionAlive(false) の通知用。
	onIdle func()
}

func newSessionManager(cfg *config, session tmux.SessionIface) *SessionManager {
	return &SessionManager{
		session: session,
		cfg:     cfg,
	}
}

// Handle は 1 件のメッセージを処理する (Cold なら spawn → inject → wait_for_idle)。
func (m *SessionManager) Handle(ctx context.Context, prompt string) error {
	// idle timer をキャンセルする (wake-on-message)。
	// timer.Stop() が false = goroutine が既に発火済み → 完了を待ってから進む。
	// これにより session.Stop() (goroutine) と session.Start() (Handle) の race を防ぐ。
	m.timerMu.Lock()
	if m.idleTimer != nil {
		if !m.idleTimer.Stop() && m.timerDone != nil {
			done := m.timerDone
			m.timerMu.Unlock()
			<-done // AfterFunc goroutine の session.Stop() 完了を待つ
			m.timerMu.Lock()
		}
		m.idleTimer = nil
		m.timerDone = nil
	}
	m.timerMu.Unlock()

	// Tier2 が停止していれば spawn する
	if !m.session.IsAlive() {
		slog.Info("session cold — spawning Tier2", "handle", "@"+m.cfg.User)
		if err := m.session.Start(ctx); err != nil {
			return fmt.Errorf("spawn: %w", err)
		}
	}

	// メッセージを注入する
	if err := m.session.InjectMessage(prompt); err != nil {
		return fmt.Errorf("inject: %w", err)
	}

	// 応答完了まで待つ
	if err := m.session.WaitForIdle(ctx); err != nil {
		slog.Warn("WaitForIdle error — resetting session", "handle", "@"+m.cfg.User, "err", err)
		_ = m.session.Stop(ctx) // タイムアウト時はセッションをリセット
		return err
	}

	// 処理完了後 idle timer を開始する
	m.timerMu.Lock()
	done := make(chan struct{})
	m.timerDone = done
	m.idleTimer = time.AfterFunc(m.cfg.IdleTimeout, func() {
		defer close(done) // Handle() が待てるよう完了を通知する
		slog.Info("idle timeout — stopping session",
			"handle", "@"+m.cfg.User,
			"timeout_s", m.cfg.IdleTimeout.Seconds())
		ctx2, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = m.session.Stop(ctx2)
		if m.onIdle != nil {
			m.onIdle()
		}
	})
	m.timerMu.Unlock()

	return nil
}

// Shutdown は idle timer をキャンセルしてセッションを停止する。
func (m *SessionManager) Shutdown(ctx context.Context) {
	m.timerMu.Lock()
	if m.idleTimer != nil {
		if !m.idleTimer.Stop() && m.timerDone != nil {
			done := m.timerDone
			m.timerMu.Unlock()
			<-done
			m.timerMu.Lock()
		}
	}
	m.timerMu.Unlock()
	_ = m.session.Stop(ctx)
}

// ──────────────────────────────────────────────────────────────────────── //
// プロンプトフォーマット                                                   //
// ──────────────────────────────────────────────────────────────────────── //

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
// MCP config ファイル                                                      //
// ──────────────────────────────────────────────────────────────────────── //

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

	f, err := os.CreateTemp("", fmt.Sprintf("bridge-tmux-%s-*.json", cfg.User))
	if err != nil {
		return "", err
	}
	defer f.Close()

	if err := os.Chmod(f.Name(), 0o600); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := json.NewEncoder(f).Encode(payload); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	slog.Debug("wrote MCP config", "path", f.Name())
	return f.Name(), nil
}

// ──────────────────────────────────────────────────────────────────────── //
// ポーリングループ                                                         //
// ──────────────────────────────────────────────────────────────────────── //

func runBridge(ctx context.Context, cfg *config, client *agenthub.Client, manager *SessionManager, health *HealthState) error {
	selfHandle := "@" + cfg.User
	consecutiveFailures := 0

	slog.Info("polling inbox",
		"handle", selfHandle,
		"poll_s", cfg.PollInterval.Seconds(),
		"idle_timeout_s", cfg.IdleTimeout.Seconds())

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msgs, err := client.GetMessages(ctx)
		if err != nil {
			consecutiveFailures++
			slog.Warn("get_messages error", "consecutive", consecutiveFailures, "err", err)
			if cfg.MaxRetries > 0 && consecutiveFailures >= cfg.MaxRetries {
				return fmt.Errorf("circuit breaker: %d consecutive get_messages failures", consecutiveFailures)
			}
			sleepWithContext(ctx, cfg.ReconnectBackoff)
			continue
		}
		consecutiveFailures = 0

		for _, msg := range msgs {
			handleMessage(ctx, cfg, client, manager, health, selfHandle, msg)
		}

		sleepWithContext(ctx, cfg.PollInterval)
	}
}

func handleMessage(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	manager *SessionManager,
	health *HealthState,
	selfHandle string,
	msg agenthub.Message,
) {
	// 自己ループ防止
	if msg.Sender == selfHandle {
		slog.Debug("skip self-sent message", "msg_id", msg.ID)
		_ = client.MarkAsRead(ctx, msg.ID)
		return
	}

	// issue #51: workdir がなければ error DM を返して ack して早期 return
	// (crash-ack ループ防止。workdir が存在しなくなった場合は bridge を再起動すること)
	if _, err := os.Stat(cfg.Workdir); err != nil {
		slog.Error("workdir gone — sending error DM and acking", "workdir", cfg.Workdir, "msg_id", msg.ID)
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = client.SendMessage(sendCtx, msg.Sender,
			fmt.Sprintf("(auto) bridge workdir does not exist: %s", cfg.Workdir), msg.ID)
		_ = client.MarkAsRead(ctx, msg.ID)
		return
	}

	slog.Info("message received",
		"msg_id", msg.ID,
		"from", msg.Sender,
		"body_preview", truncate(msg.Body, 120))

	prompt := formatPrompt(selfHandle, msg)
	if err := manager.Handle(ctx, prompt); err != nil {
		slog.Error("Handle error", "msg_id", msg.ID, "from", msg.Sender, "err", err)
		health.RecordError(selfHandle, err.Error())
		health.SetSessionAlive(selfHandle, false)
		// エラーを送信元に返す
		errMsg := fmt.Sprintf("(auto) bridge-tmux error: %v", err)
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if sendErr := client.SendMessage(sendCtx, msg.Sender, errMsg, msg.ID); sendErr != nil {
			slog.Error("fallback send_message failed", "err", sendErr)
		}
	} else {
		slog.Info("message processed", "msg_id", msg.ID, "from", msg.Sender)
		health.RecordMessage(selfHandle)
		health.SetSessionAlive(selfHandle, true)
	}

	if err := client.MarkAsRead(ctx, msg.ID); err != nil {
		slog.Warn("mark_as_read failed", "msg_id", msg.ID, "err", err)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// main                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

// validateLogLevel は log-level 文字列が有効か検証する。
// parseConfig から呼び出し、不正値を早期エラーにする (issue #149)。
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
		// validateLogLevel in parseConfig guarantees this branch is unreachable.
		panic(fmt.Sprintf("setupLogger: unexpected log level %q", level))
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(handler))
}

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(2)
	}

	setupLogger(cfg.LogLevel)

	// ANTHROPIC_API_KEY を unset: Tier2 (claude CLI) が API キー課金ではなく
	// subscription (claude.ai) で動くよう強制する。fleet / single 両モード共通。
	os.Unsetenv("ANTHROPIC_API_KEY")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// fleet モード: YAML ファイルから複数 persona を並列起動
	if cfg.FleetFile != "" {
		slog.Info("fleet mode", "fleet_file", cfg.FleetFile)
		fleet, err := LoadFleetConfig(cfg.FleetFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fleet config error: %v\n", err)
			os.Exit(2)
		}
		slog.Info("fleet personas", "count", len(fleet.Personas))
		// fleet モードの health state (health server 無効でも常に作成)
		fleetHealth := NewHealthState("fleet")
		for _, p := range fleet.Personas {
			fleetHealth.EnsurePersona("@" + p.Handle)
		}
		StartHealthServer(ctx, cfg.HealthPort, fleetHealth)
		if err := RunFleet(ctx, cfg, fleet, fleetHealth); err != nil && ctx.Err() == nil {
			slog.Error("fleet error", "err", err)
			os.Exit(1)
		}
		slog.Info("fleet shutdown complete")
		return
	}

	slog.Info("bridge-tmux starting",
		"handle", "@"+cfg.User,
		"workdir", cfg.Workdir,
		"idle_timeout_s", cfg.IdleTimeout.Seconds(),
		"spawn_timeout_s", cfg.SpawnTimeout.Seconds())

	// MCP config ファイルを書く (Tier2 用; ANTHROPIC_API_KEY は含めない)
	mcpConfigPath, err := writeMCPConfig(cfg)
	if err != nil {
		slog.Error("writeMCPConfig failed", "err", err)
		os.Exit(1)
	}
	// defer os.Remove の後に os.Exit を呼ぶと defer がスキップされ
	// PAT を含む tempfile が /tmp に残留する。以降の fatal は fatalCleanup を使う。
	fatalCleanup := func(msg string, args ...any) {
		os.Remove(mcpConfigPath)
		slog.Error(msg, args...)
		os.Exit(1)
	}
	defer os.Remove(mcpConfigPath)

	// hub client 初期化 (agent-hub-sdk/go)
	// 必須パラメータは parseConfig() で検証済み。New() も fail-fast 検証を行う。
	client, err := agenthub.New(
		cfg.AgentHubURL, cfg.GitHubPAT, cfg.User, cfg.Tenant,
		agenthub.WithClientName("bridge-tmux"),
	)
	if err != nil {
		fatalCleanup("agenthub.New failed", "err", err)
	}

	// MCP initialize
	if err := client.Initialize(ctx); err != nil {
		fatalCleanup("MCP initialize failed", "err", err)
	}

	// register
	registered, err := client.Register(ctx, cfg.DisplayName, "stateful")
	if err != nil {
		fatalCleanup("register failed", "err", err)
	}
	slog.Info("registered", "result", strings.SplitN(registered, "\n", 2)[0])

	// SSE ストリームを開いてサーバー ping に自動応答する (issue #41)
	if err := client.StartSSE(ctx); err != nil {
		fatalCleanup("StartSSE failed", "err", err)
	}
	defer client.StopSSE()

	// health state + server (single mode)
	selfHandle := "@" + cfg.User
	health := NewHealthState("single")
	health.EnsurePersona(selfHandle)
	StartHealthServer(ctx, cfg.HealthPort, health)

	// session manager
	session := tmux.NewSession(tmux.SessionOptions{
		UserID:           cfg.User,
		Workdir:          cfg.Workdir,
		MCPConfigPath:    mcpConfigPath,
		ClaudeCLI:        cfg.ClaudeCLI,
		Model:            cfg.Model,
		BypassPerms:      cfg.BypassPerms,
		SpawnTimeout:     cfg.SpawnTimeout,
		ActivityIdleTime: cfg.ActivityIdle,
		ResponseTimeout:  cfg.ResponseTimeout,
	})
	manager := newSessionManager(cfg, session)
	// idle timer が発火したとき、health state の session_alive を false にする
	manager.onIdle = func() {
		health.SetSessionAlive(selfHandle, false)
		slog.Info("persona session killed by idle timer", "handle", selfHandle)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		manager.Shutdown(shutCtx)
	}()

	// ポーリングループ (reconnect あり)
	for {
		err := runBridge(ctx, cfg, client, manager, health)
		if ctx.Err() != nil {
			slog.Info("shutting down")
			return
		}
		if err != nil {
			slog.Warn("runBridge ended with error", "err", err)
		}

		// MCP セッションを再確立する
		// SSE goroutine を先に停止してから sleep → re-initialize → re-register → re-StartSSE
		client.StopSSE()
		slog.Info("reconnecting", "backoff_s", cfg.ReconnectBackoff.Seconds())
		sleepWithContext(ctx, cfg.ReconnectBackoff)
		if ctx.Err() != nil {
			return
		}

		// re-initialize (Initialize() が sessionID をクリアしてから新規ハンドシェイク)
		if err := client.Initialize(ctx); err != nil {
			slog.Warn("re-initialize failed", "err", err)
			continue
		}
		// re-register 失敗時も continue して再 initialize から試みる
		if _, err := client.Register(ctx, cfg.DisplayName, "stateful"); err != nil {
			slog.Warn("re-register failed", "err", err)
			continue
		}
		// SSE ストリームを再開する
		if err := client.StartSSE(ctx); err != nil {
			slog.Warn("re-StartSSE failed", "err", err)
			continue
		}
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// ユーティリティ                                                           //
// ──────────────────────────────────────────────────────────────────────── //

// tenantValue は --tenant フラグと AGENT_HUB_TENANT 環境変数を統合して返す。
// フラグが空なら env var にフォールバック。spawn-bridge.sh との互換性を保つ。
func tenantValue(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("AGENT_HUB_TENANT")
}

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
