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
// Issue: #110
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
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
}

func parseConfig() (*config, error) {
	var (
		user        = flag.String("user", "", "agent-hub handle (single-persona mode, without @)")
		displayName = flag.String("display-name", "", "display name (optional)")
		workdir     = flag.String("workdir", "", "peer workdir with CLAUDE.md (default: cwd)")
		model       = flag.String("model", "", "Claude model (default: claude default)")
		idleTimeout = flag.Duration("idle-timeout", 10*time.Minute, "idle kill timeout")
		noBypass    = flag.Bool("no-bypass-permissions", false, "disable --dangerously-skip-permissions")
		fleetFile   = flag.String("fleet", "", "fleet YAML config file (multi-persona mode)")
	)
	flag.Parse()

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
		Tenant:           os.Getenv("AGENT_HUB_TENANT"),
		Workdir:          wd,
		ClaudeCLI:        claudeCLI,
		Model:            *model,
		BypassPerms:      !*noBypass,
		IdleTimeout:      *idleTimeout,
		SpawnTimeout:     60 * time.Second,
		ActivityIdle:     8 * time.Second,
		ResponseTimeout:  5 * time.Minute,
		PollInterval:     5 * time.Second,
		ReconnectBackoff: 5 * time.Second,
		MaxRetries:       10,
		FleetFile:        *fleetFile,
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
		log.Printf("[manager] session cold — spawning Tier2: claude-bridge-%s", m.cfg.User)
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
		log.Printf("[manager] WaitForIdle error — resetting session: %v", err)
		_ = m.session.Stop(ctx) // タイムアウト時はセッションをリセット
		return err
	}

	// 処理完了後 idle timer を開始する
	m.timerMu.Lock()
	done := make(chan struct{})
	m.timerDone = done
	m.idleTimer = time.AfterFunc(m.cfg.IdleTimeout, func() {
		defer close(done) // Handle() が待てるよう完了を通知する
		log.Printf("[manager] idle timeout (%.0fs) — stopping session claude-bridge-%s",
			m.cfg.IdleTimeout.Seconds(), m.cfg.User)
		ctx2, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = m.session.Stop(ctx2)
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
	log.Printf("[config] wrote MCP config: %s", f.Name())
	return f.Name(), nil
}

// ──────────────────────────────────────────────────────────────────────── //
// ポーリングループ                                                         //
// ──────────────────────────────────────────────────────────────────────── //

func runBridge(ctx context.Context, cfg *config, client *agenthub.Client, manager *SessionManager) error {
	selfHandle := "@" + cfg.User
	consecutiveFailures := 0

	log.Printf("[bridge] polling inbox every %.0fs (user=%s, idle_timeout=%.0fs)",
		cfg.PollInterval.Seconds(), selfHandle, cfg.IdleTimeout.Seconds())

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msgs, err := client.GetMessages(ctx)
		if err != nil {
			consecutiveFailures++
			log.Printf("[bridge] get_messages error (#%d): %v", consecutiveFailures, err)
			if cfg.MaxRetries > 0 && consecutiveFailures >= cfg.MaxRetries {
				return fmt.Errorf("circuit breaker: %d consecutive get_messages failures", consecutiveFailures)
			}
			sleepWithContext(ctx, cfg.ReconnectBackoff)
			continue
		}
		consecutiveFailures = 0

		for _, msg := range msgs {
			handleMessage(ctx, cfg, client, manager, selfHandle, msg)
		}

		sleepWithContext(ctx, cfg.PollInterval)
	}
}

func handleMessage(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	manager *SessionManager,
	selfHandle string,
	msg agenthub.Message,
) {
	// 自己ループ防止
	if msg.Sender == selfHandle {
		log.Printf("[bridge] skip self-sent message %s", msg.ID)
		_ = client.MarkAsRead(ctx, msg.ID)
		return
	}

	// issue #51: workdir がなければ error DM を返して ack して早期 return
	// (crash-ack ループ防止。workdir が存在しなくなった場合は bridge を再起動すること)
	if _, err := os.Stat(cfg.Workdir); err != nil {
		log.Printf("[bridge] workdir %q gone — sending error DM and acking %s (issue #51)",
			cfg.Workdir, msg.ID)
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = client.SendMessage(sendCtx, msg.Sender,
			fmt.Sprintf("(auto) bridge workdir does not exist: %s", cfg.Workdir), msg.ID)
		_ = client.MarkAsRead(ctx, msg.ID)
		return
	}

	log.Printf("[bridge] <- message %s from %s: %s", msg.ID, msg.Sender, truncate(msg.Body, 120))

	prompt := formatPrompt(selfHandle, msg)
	if err := manager.Handle(ctx, prompt); err != nil {
		log.Printf("[bridge] Handle error for %s: %v", msg.ID, err)
		// エラーを送信元に返す
		errMsg := fmt.Sprintf("(auto) bridge-tmux error: %v", err)
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if sendErr := client.SendMessage(sendCtx, msg.Sender, errMsg, msg.ID); sendErr != nil {
			log.Printf("[bridge] fallback send_message failed: %v", sendErr)
		}
	} else {
		log.Printf("[bridge] ✓ processed %s from %s", msg.ID, msg.Sender)
	}

	if err := client.MarkAsRead(ctx, msg.ID); err != nil {
		log.Printf("[bridge] mark_as_read %s failed: %v", msg.ID, err)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// main                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(2)
	}

	// ANTHROPIC_API_KEY を unset: Tier2 (claude CLI) が API キー課金ではなく
	// subscription (claude.ai) で動くよう強制する。fleet / single 両モード共通。
	os.Unsetenv("ANTHROPIC_API_KEY")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// fleet モード: YAML ファイルから複数 persona を並列起動
	if cfg.FleetFile != "" {
		log.Printf("[main] bridge-tmux fleet mode (fleet=%s)", cfg.FleetFile)
		fleet, err := LoadFleetConfig(cfg.FleetFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fleet config error: %v\n", err)
			os.Exit(2)
		}
		log.Printf("[main] fleet: %d personas", len(fleet.Personas))
		if err := RunFleet(ctx, cfg, fleet); err != nil && ctx.Err() == nil {
			log.Fatalf("fleet error: %v", err)
		}
		log.Println("[main] fleet shutdown complete")
		return
	}

	log.Printf("[main] bridge-tmux starting (user=@%s, workdir=%s, idle=%.0fs)",
		cfg.User, cfg.Workdir, cfg.IdleTimeout.Seconds())

	// MCP config ファイルを書く (Tier2 用; ANTHROPIC_API_KEY は含めない)
	mcpConfigPath, err := writeMCPConfig(cfg)
	if err != nil {
		log.Fatalf("writeMCPConfig: %v", err)
	}
	// defer os.Remove の後に log.Fatalf を呼ぶと os.Exit(1) で defer がスキップされ
	// PAT を含む tempfile が /tmp に残留する。以降の fatal は fatalCleanup を使う。
	fatalCleanup := func(format string, args ...any) {
		os.Remove(mcpConfigPath)
		log.Fatalf(format, args...)
	}
	defer os.Remove(mcpConfigPath)

	// hub client 初期化 (agent-hub-sdk/go)
	// 必須パラメータは parseConfig() で検証済み。New() も fail-fast 検証を行う。
	client, err := agenthub.New(
		cfg.AgentHubURL, cfg.GitHubPAT, cfg.User, cfg.Tenant,
		agenthub.WithClientName("bridge-tmux"),
	)
	if err != nil {
		fatalCleanup("agenthub.New: %v", err)
	}

	// MCP initialize
	if err := client.Initialize(ctx); err != nil {
		fatalCleanup("MCP initialize: %v", err)
	}

	// register
	registered, err := client.Register(ctx, cfg.DisplayName, "stateful")
	if err != nil {
		fatalCleanup("register: %v", err)
	}
	log.Printf("[main] registered: %s", strings.SplitN(registered, "\n", 2)[0])

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
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		manager.Shutdown(shutCtx)
	}()

	// ポーリングループ (reconnect あり)
	for {
		err := runBridge(ctx, cfg, client, manager)
		if ctx.Err() != nil {
			log.Println("[main] shutting down")
			return
		}
		if err != nil {
			log.Printf("[main] runBridge ended: %v", err)
		}

		// MCP セッションを再確立する
		log.Printf("[main] reconnecting in %.0fs...", cfg.ReconnectBackoff.Seconds())
		sleepWithContext(ctx, cfg.ReconnectBackoff)
		if ctx.Err() != nil {
			return
		}

		// re-initialize
		if err := client.Initialize(ctx); err != nil {
			log.Printf("[main] re-initialize failed: %v", err)
			continue
		}
		// re-register 失敗時も continue して再 initialize から試みる
		if _, err := client.Register(ctx, cfg.DisplayName, "stateful"); err != nil {
			log.Printf("[main] re-register failed: %v", err)
			continue
		}
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
