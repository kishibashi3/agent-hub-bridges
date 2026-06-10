// bridge-claude2: Go ネイティブ bridge for Claude Code.
//
// Python bridge-claude (bridges/claude/worker.py) を Go に直訳した実装。
// 構造は Python bridge と同等: runner.go が ClaudeRunner+ClaudeSDKClient、
// worker.go が worker.py の main loop に相当する。
//
// 動作フロー:
//  1. MCP initialize ハンドシェイク (agent-hub-sdk/go)
//  2. register で agent-hub に自 peer を登録
//  3. hub session ループ: journal replay → startup catchup → SSE push 駆動ループ
//     - push 受信 → GetMessages → CommandRouter → MarkAsRead → handleOne → runner.query (on-demand subprocess)
//     - cursor 永続化 (MarkAsRead は handleOne 前に完了済み — issue #176)
//  4. SIGTERM/Ctrl+C → runGracefulDrain() で compact + 未処理メッセージ処理 → exit (issue #178)
//
// Python bridge との主な対応:
//   worker.py:               → worker.go
//   claude_runner.py:        → runner.go
//   cursor.py:               → cursor.go
//   _common/journal.py:      → journal.go
//   _common/inventory.py:    → inventory.go
//   _common/reconnect.py:    → worker.go (runWorker の reconnect loop)
//   blocking_commands.py:    → blocking.go
//   CommandRouter (SDK):     → commands.go
//   _ActivityTracker:        → tracker.go (activityTracker)
//   _MessageGapTracker:      → tracker.go (messageGapTracker)
//   _IdleCompactWatchdog:    → compact.go
//
// 環境変数:
//   AGENT_HUB_URL               required    agent-hub MCP エンドポイント
//   GITHUB_PAT                  required    GitHub Personal Access Token
//   AGENT_HUB_TENANT            optional    テナント ID (--tenant フラグが優先)
//   CLAUDE_CLI_PATH             optional    claude CLI のパス (省略 = PATH 上の "claude")
//   AGENT_HUB_MODEL             optional    Claude model
//   AGENT_HUB_CURSOR_FILE       optional    cursor ファイルパス
//   AGENT_HUB_JOURNAL_DIR       optional    journal ディレクトリ
//   AGENT_HUB_BUSY_WINDOW_S     optional    /status busy 判定ウィンドウ秒数 (default: 60)
//   AGENT_HUB_PUSH_SILENT_THRESHOLD_S optional gap 警告閾値秒数 (default: 25)
//   AGENT_HUB_SUBPROCESS_TIMEOUT optional   claude subprocess 最大実行時間 (Go duration: "30m", "1h", "0" = 無制限; --subprocess-timeout フラグが優先)
//   AGENT_HUB_MAX_QUERY_RETRIES optional    subprocess timeout 時のリトライ上限 (default: 2; --max-query-retries フラグが優先)
//   BRIDGE_COMPACT_ARCHIVE_DIR  optional    compact archive ディレクトリ (SIGTERM compact 時に使用)
//   BRIDGE_INVENTORY            optional    bridge inventory ファイルパス
//   AGENT_HUB_BRIDGE_MAX_RETRIES optional   circuit breaker 連続失敗上限 (default: 10, 0=無限)
//   AGENT_HUB_INBOX_POLL_INTERVAL_S optional safety-net poll / heartbeat 間隔秒数 (default: 30; issue #234)
//   BRIDGE_LOG_DIR              optional    ログディレクトリ (省略 = ~/.agent-hub/logs/; --log-file が優先)
//   BRIDGE_LOG_FILE             optional    ログファイルパス (省略 = BRIDGE_LOG_DIR/bridge-<user>.log)
//
// Issue: #155 (original), features: #162-#170
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

// version はビルド時に ldflags で inject する。
// go build -ldflags "-X main.version=0.6.0"
var version = "dev"

// ──────────────────────────────────────────────────────────────────────── //
// 設定                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

// stringSlice は --add-dir フラグの繰り返し指定を受け付けるカスタム型。
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ", ") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type config struct {
	User        string
	DisplayName string
	AgentHubURL string
	GitHubPAT   string
	Tenant      string
	Workdir     string
	ClaudeCLI   string
	Model       string
	Mode        string   // stateful | stateless | global (default: stateful)
	AddDirs     []string // --add-dir で指定した追加ディレクトリ (繰り返し可)
	LogLevel    string
	LogFile     string // ログファイルパス ("" = stderr のみ)
	// ReconnectBackoff は MCP セッション再接続待機時間。
	ReconnectBackoff time.Duration
	// MaxRetries は circuit breaker の連続失敗上限 (0 = 無制限)。
	MaxRetries int
	// SubprocessTimeout は runner.query の最大実行時間 (0 = タイムアウトなし)。
	SubprocessTimeout time.Duration
	// MaxQueryRetries は subprocess timeout 時のリトライ上限 (0 = リトライなし)。
	MaxQueryRetries int
	// ScannerBufferSize は stream-json bufio.Scanner のバッファサイズ (bytes)。
	// AGENT_HUB_SCANNER_BUFFER_SIZE env で設定可能 (例: "4MB", "8MB")。デフォルト 4MB。
	ScannerBufferSize int
}

func parseConfig() (*config, error) {
	var (
		user              = flag.String("user", "", "agent-hub handle (without @) [required]")
		displayName       = flag.String("display-name", "", "display name (optional)")
		tenant            = flag.String("tenant", "", "agent-hub tenant ID (overrides AGENT_HUB_TENANT env)")
		workdir           = flag.String("workdir", "", "peer workdir with CLAUDE.md (default: cwd)")
		model             = flag.String("model", "", "Claude model override (default: AGENT_HUB_MODEL env or claude default)")
		mode              = flag.String("mode", "stateful", "peer mode: stateful|stateless|global")
		logLevel          = flag.String("log-level", "info", "log level: debug|info|warn|error")
		logFile           = flag.String("log-file", "", "log file path (default: ~/.agent-hub/logs/bridge-<user>.log; overrides BRIDGE_LOG_DIR/BRIDGE_LOG_FILE)")
		reconnectBackoff  = flag.Duration("reconnect-backoff", 5*time.Second, "backoff on MCP reconnect")
		maxRetries        = flag.Int("max-retries", 10, "circuit breaker: max consecutive get_messages failures (0 = unlimited)")
		subprocessTimeout = flag.Duration("subprocess-timeout", -1, "claude subprocess max runtime per query (0 = no timeout; -1 = use AGENT_HUB_SUBPROCESS_TIMEOUT env or default 30m)")
		maxQueryRetries   = flag.Int("max-query-retries", -1, "max retries on subprocess timeout per message (-1 = use AGENT_HUB_MAX_QUERY_RETRIES env or default 2; 0 = no retry)")
		showVersion       = flag.Bool("version", false, "print version and exit")
		addDirs           stringSlice
	)
	flag.Var(&addDirs, "add-dir", "add extra directory to Claude context (repeatable)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("bridge-claude2/%s (agent-hub-bridges)\n", version)
		os.Exit(0)
	}

	if err := validateLogLevel(*logLevel); err != nil {
		return nil, err
	}
	if *user == "" {
		return nil, fmt.Errorf("--user is required")
	}
	if *mode != "stateful" && *mode != "stateless" && *mode != "global" {
		return nil, fmt.Errorf("--mode must be stateful|stateless|global, got %q", *mode)
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

	// --add-dir: 各パスの存在確認と絶対パス化
	normalizedAddDirs := make([]string, 0, len(addDirs))
	for _, d := range addDirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, fmt.Errorf("--add-dir %q: %w", d, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("--add-dir %q: %w", d, err)
		}
		normalizedAddDirs = append(normalizedAddDirs, abs)
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

	// subprocess-timeout: --subprocess-timeout フラグ (0 = 未指定) > AGENT_HUB_SUBPROCESS_TIMEOUT env > 30m
	resolvedSubprocessTimeout, err := resolveSubprocessTimeout(*subprocessTimeout)
	if err != nil {
		return nil, err
	}

	// max-query-retries: --max-query-retries フラグ (-1 = 未指定) > AGENT_HUB_MAX_QUERY_RETRIES env > 2
	resolvedMaxQueryRetries, err := resolveMaxQueryRetries(*maxQueryRetries)
	if err != nil {
		return nil, err
	}

	// scanner-buffer-size: AGENT_HUB_SCANNER_BUFFER_SIZE env > 4MB
	resolvedScannerBufferSize, err := resolveScannerBufferSize()
	if err != nil {
		return nil, err
	}

	// display_name: --display-name フラグ > "{user} — go bridge"
	resolvedDisplayName := *displayName
	if resolvedDisplayName == "" {
		resolvedDisplayName = *user + " — go bridge"
	}

	// log file: --log-file フラグ > BRIDGE_LOG_FILE env > BRIDGE_LOG_DIR env / ~/.agent-hub/logs/
	resolvedLogFile := *logFile
	if resolvedLogFile == "" {
		resolvedLogFile = os.Getenv("BRIDGE_LOG_FILE")
	}
	if resolvedLogFile == "" {
		logDir := os.Getenv("BRIDGE_LOG_DIR")
		if logDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("get home dir for log path: %w", err)
			}
			logDir = filepath.Join(home, ".agent-hub", "logs")
		}
		resolvedLogFile = filepath.Join(logDir, "bridge-"+*user+".log")
	}

	return &config{
		User:              *user,
		DisplayName:       resolvedDisplayName,
		AgentHubURL:       url,
		GitHubPAT:         pat,
		Tenant:            tenantValue(*tenant),
		Workdir:           wd,
		ClaudeCLI:         claudeCLI,
		Model:             resolvedModel,
		Mode:              *mode,
		AddDirs:           normalizedAddDirs,
		LogLevel:          *logLevel,
		LogFile:           resolvedLogFile,
		ReconnectBackoff:  *reconnectBackoff,
		MaxRetries:        *maxRetries,
		SubprocessTimeout: resolvedSubprocessTimeout,
		MaxQueryRetries:   resolvedMaxQueryRetries,
		ScannerBufferSize: resolvedScannerBufferSize,
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

// setupLogger はログレベルとログファイルパスに基づいて slog ハンドラを設定する。
// logFile が空文字列でない場合、ログはファイル (append) と stderr の両方に書き出される。
// 返り値の close 関数はプロセス終了前に呼ぶこと (ファイルクローズ)。
func setupLogger(level, logFile string) (close func()) {
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

	if logFile == "" {
		handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})
		slog.SetDefault(slog.New(handler))
		return func() {}
	}

	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "setupLogger: cannot create log dir %q: %v — falling back to stderr\n", filepath.Dir(logFile), err)
		handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})
		slog.SetDefault(slog.New(handler))
		return func() {}
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setupLogger: cannot open log file %q: %v — falling back to stderr\n", logFile, err)
		handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})
		slog.SetDefault(slog.New(handler))
		return func() {}
	}

	w := io.MultiWriter(f, os.Stderr)
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(handler))
	return func() { f.Close() }
}

// ──────────────────────────────────────────────────────────────────────── //
// MCP config ファイル                                                      //
// ──────────────────────────────────────────────────────────────────────── //

// writeMCPConfig は claude subprocess に渡す agent-hub MCP config を
// 一時ファイルに書き出す。ファイルパスを返す。呼出元が defer os.Remove を担当する。
//
// PAT をコマンドライン引数 (ps で見える) に渡さないためファイル経由にする。
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

	f, err := os.CreateTemp("", fmt.Sprintf("bridge-claude2-%s-*.json", cfg.User))
	if err != nil {
		return "", fmt.Errorf("create mcp config temp file: %w", err)
	}
	tmpPath := f.Name()
	f.Close()

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("chmod mcp config: %w", err)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("write mcp config: %w", err)
	}

	slog.Debug("wrote MCP config", "path", tmpPath)
	return tmpPath, nil
}

// ──────────────────────────────────────────────────────────────────────── //
// プロンプトフォーマット                                                   //
// ──────────────────────────────────────────────────────────────────────── //

// formatPrompt は受信メッセージを claude への user prompt に変換する。
// Python bridge (_common/prompt.py: format_peer_message_prompt) と同等。
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
// main                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(2)
	}

	closeLogger := setupLogger(cfg.LogLevel, cfg.LogFile)
	defer closeLogger()

	slog.Info("bridge-claude2 starting",
		"version", version,
		"handle", "@"+cfg.User,
		"workdir", cfg.Workdir,
		"mode", cfg.Mode,
		"model", orDefault(cfg.Model, "(claude default)"),
		"subprocess_timeout_s", cfg.SubprocessTimeout.Seconds(),
		"max_query_retries", cfg.MaxQueryRetries,
		"scanner_buffer_size", cfg.ScannerBufferSize,
		"add_dirs_count", len(cfg.AddDirs),
		"log_file", orDefault(cfg.LogFile, "(stderr only)"),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// MCP config ファイルを書く (claude subprocess が mcp__agent-hub__* tools を使うため)
	mcpConfigPath, err := writeMCPConfig(cfg)
	if err != nil {
		slog.Error("writeMCPConfig failed", "err", err)
		os.Exit(1)
	}
	defer os.Remove(mcpConfigPath)

	// issue #267: telemetry 初期化 (AGENT_HUB_TELEMETRY_URL 未設定なら no-op)
	initTelemetry("@" + cfg.User)
	defer shutdownTelemetry()

	// worker を起動 (内部で reconnect loop を回す)
	runWorker(ctx, cfg, mcpConfigPath)

	slog.Info("bridge-claude2 stopped")
}

// ──────────────────────────────────────────────────────────────────────── //
// ユーティリティ                                                           //
// ──────────────────────────────────────────────────────────────────────── //

func tenantValue(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("AGENT_HUB_TENANT")
}

func sleepWithContext(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
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

// resolveInboxPollInterval は AGENT_HUB_INBOX_POLL_INTERVAL_S env を解決する。
// SSE が silently dead の場合に GetMessages を定期的に呼ぶ safety-net poll / heartbeat の間隔。
// 優先順位: AGENT_HUB_INBOX_POLL_INTERVAL_S env > 30s (default)
// 値は秒数 (小数点可)。例: "30", "60", "15.5"。
func resolveInboxPollInterval() time.Duration {
	if envVal := os.Getenv(inboxPollIntervalEnv); envVal != "" {
		var n float64
		if _, err := fmt.Sscan(envVal, &n); err != nil || n <= 0 {
			slog.Warn("resolveInboxPollInterval: invalid value, using default",
				"env", inboxPollIntervalEnv, "val", envVal,
				"default_s", defaultInboxPollInterval.Seconds(),
			)
			return defaultInboxPollInterval
		}
		return time.Duration(n * float64(time.Second))
	}
	return defaultInboxPollInterval
}

// resolveSubprocessTimeout は --subprocess-timeout フラグ値 (-1 = 未指定) を解決する。
// 優先順位: flagVal (>=0) > AGENT_HUB_SUBPROCESS_TIMEOUT env > 30m (default)
// 0 はタイムアウト無効を意味するため、未指定 sentinel は -1 を使う。
func resolveSubprocessTimeout(flagVal time.Duration) (time.Duration, error) {
	if flagVal >= 0 {
		return flagVal, nil
	}
	if envVal := os.Getenv("AGENT_HUB_SUBPROCESS_TIMEOUT"); envVal != "" {
		d, err := time.ParseDuration(envVal)
		if err != nil {
			return 0, fmt.Errorf("AGENT_HUB_SUBPROCESS_TIMEOUT %q: %w", envVal, err)
		}
		return d, nil
	}
	return 30 * time.Minute, nil
}

// resolveMaxQueryRetries は --max-query-retries フラグ値 (-1 = 未指定) を解決する。
// 優先順位: flagVal (>=0) > AGENT_HUB_MAX_QUERY_RETRIES env > 2 (default)
func resolveMaxQueryRetries(flagVal int) (int, error) {
	if flagVal >= 0 {
		return flagVal, nil
	}
	if envVal := os.Getenv("AGENT_HUB_MAX_QUERY_RETRIES"); envVal != "" {
		var n int
		if _, err := fmt.Sscan(envVal, &n); err != nil || n < 0 {
			return 0, fmt.Errorf("AGENT_HUB_MAX_QUERY_RETRIES %q: must be a non-negative integer", envVal)
		}
		return n, nil
	}
	return 2, nil
}

// resolveScannerBufferSize は AGENT_HUB_SCANNER_BUFFER_SIZE env を解決する。
// 優先順位: AGENT_HUB_SCANNER_BUFFER_SIZE env > 4MB (default)
// 受け付けるフォーマット: "<n>MB" または "<n>KB" (大文字小文字不問)。例: "4MB", "8MB", "512KB"。
func resolveScannerBufferSize() (int, error) {
	const defaultSize = 4 * 1024 * 1024 // 4MB
	envVal := os.Getenv("AGENT_HUB_SCANNER_BUFFER_SIZE")
	if envVal == "" {
		return defaultSize, nil
	}
	n, err := parseByteSize(envVal)
	if err != nil {
		return 0, fmt.Errorf("AGENT_HUB_SCANNER_BUFFER_SIZE %q: %w", envVal, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("AGENT_HUB_SCANNER_BUFFER_SIZE %q: must be positive", envVal)
	}
	return n, nil
}

// parseByteSize は "<n>MB" / "<n>KB" 形式の文字列を bytes に変換する。
func parseByteSize(s string) (int, error) {
	upper := strings.ToUpper(strings.TrimSpace(s))
	var multiplier int
	var numStr string
	switch {
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1024 * 1024
		numStr = strings.TrimSuffix(upper, "MB")
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1024
		numStr = strings.TrimSuffix(upper, "KB")
	default:
		return 0, fmt.Errorf("unsupported unit: must end with MB or KB (e.g. \"4MB\", \"512KB\")")
	}
	var n int
	if _, err := fmt.Sscan(strings.TrimSpace(numStr), &n); err != nil {
		return 0, fmt.Errorf("cannot parse number %q: %w", numStr, err)
	}
	return n * multiplier, nil
}
