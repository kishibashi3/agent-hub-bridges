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

	"github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/hub"
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
}

func parseConfig() (*config, error) {
	var (
		user        = flag.String("user", "", "agent-hub handle (required, without @)")
		displayName = flag.String("display-name", "", "display name (optional)")
		workdir     = flag.String("workdir", "", "peer workdir with CLAUDE.md (default: cwd)")
		model       = flag.String("model", "", "Claude model (default: claude default)")
		idleTimeout = flag.Duration("idle-timeout", 10*time.Minute, "idle kill timeout")
		noBypass    = flag.Bool("no-bypass-permissions", false, "disable --dangerously-skip-permissions")
	)
	flag.Parse()

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
	}, nil
}

// ──────────────────────────────────────────────────────────────────────── //
// SessionManager: on-demand spawn + idle timer                            //
// ──────────────────────────────────────────────────────────────────────── //

// SessionManager は Tier2 セッションのライフサイクルと idle timer を管理する。
type SessionManager struct {
	session    *tmux.Session
	cfg        *config
	idleTimer  *time.Timer
	timerMu    sync.Mutex
}

func newSessionManager(cfg *config, mcpConfigPath string) *SessionManager {
	opts := tmux.SessionOptions{
		UserID:           cfg.User,
		Workdir:          cfg.Workdir,
		MCPConfigPath:    mcpConfigPath,
		ClaudeCLI:        cfg.ClaudeCLI,
		Model:            cfg.Model,
		BypassPerms:      cfg.BypassPerms,
		SpawnTimeout:     cfg.SpawnTimeout,
		ActivityIdleTime: cfg.ActivityIdle,
		ResponseTimeout:  cfg.ResponseTimeout,
	}
	return &SessionManager{
		session: tmux.NewSession(opts),
		cfg:     cfg,
	}
}

// Handle は 1 件のメッセージを処理する (Cold なら spawn → inject → wait_for_idle)。
func (m *SessionManager) Handle(ctx context.Context, prompt string) error {
	// idle timer をキャンセル (wake-on-message)
	m.timerMu.Lock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	m.timerMu.Unlock()

	// Tier2 が停止していれば spawn する
	if !m.session.IsAlive() {
		log.Printf("[manager] session cold — spawning Tier2: %s", m.session.Name)
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
	m.idleTimer = time.AfterFunc(m.cfg.IdleTimeout, func() {
		log.Printf("[manager] idle timeout (%.0fs) — stopping session %s",
			m.cfg.IdleTimeout.Seconds(), m.session.Name)
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
		m.idleTimer.Stop()
	}
	m.timerMu.Unlock()
	_ = m.session.Stop(ctx)
}

// ──────────────────────────────────────────────────────────────────────── //
// プロンプトフォーマット                                                   //
// ──────────────────────────────────────────────────────────────────────── //

func formatPrompt(selfHandle string, msg hub.Message) string {
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

func runBridge(ctx context.Context, cfg *config, client *hub.Client, manager *SessionManager) error {
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
	client *hub.Client,
	manager *SessionManager,
	selfHandle string,
	msg hub.Message,
) {
	// 自己ループ防止
	if msg.Sender == selfHandle {
		log.Printf("[bridge] skip self-sent message %s", msg.ID)
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

	// workdir チェック (存在しない場合は ack して skip)
	if _, err := os.Stat(cfg.Workdir); err != nil {
		log.Printf("[bridge] workdir %q gone — skipping ack for %s", cfg.Workdir, msg.ID)
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

	log.Printf("[main] bridge-tmux starting (user=@%s, workdir=%s, idle=%.0fs)",
		cfg.User, cfg.Workdir, cfg.IdleTimeout.Seconds())

	// MCP config ファイルを書く (Tier2 用)
	mcpConfigPath, err := writeMCPConfig(cfg)
	if err != nil {
		log.Fatalf("writeMCPConfig: %v", err)
	}
	defer os.Remove(mcpConfigPath)

	// ANTHROPIC_API_KEY を unset (subscription auth 優先)
	os.Unsetenv("ANTHROPIC_API_KEY")

	// hub client 初期化
	client := hub.New(cfg.AgentHubURL, cfg.GitHubPAT, cfg.User, cfg.Tenant)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// MCP initialize
	if err := client.Initialize(ctx); err != nil {
		log.Fatalf("MCP initialize: %v", err)
	}

	// register
	registered, err := client.Register(ctx, cfg.DisplayName, "stateful")
	if err != nil {
		log.Fatalf("register: %v", err)
	}
	log.Printf("[main] registered: %s", strings.SplitN(registered, "\n", 2)[0])

	// session manager
	manager := newSessionManager(cfg, mcpConfigPath)
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
		if _, err := client.Register(ctx, cfg.DisplayName, "stateful"); err != nil {
			log.Printf("[main] re-register failed: %v", err)
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
