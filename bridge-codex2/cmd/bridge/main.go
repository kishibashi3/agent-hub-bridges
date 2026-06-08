// bridge-codex2: Go ネイティブ bridge for Codex CLI.
//
// bridge-claude2 をベースに runner を codex CLI に差し替えた実装。
// `codex exec resume --last` を使うことで on-demand でもセッション継続ができる。
//
// 動作フロー:
//  1. 永続 CODEX_HOME セットアップ: config.toml 書き込み + auth.json symlink 作成
//  2. MCP initialize ハンドシェイク (agent-hub-sdk/go)
//  3. register で agent-hub に自 peer を登録
//  4. hub session ループ: journal replay → startup catchup → polling loop
//     - message 受信 → CommandRouter → MarkAsRead → handleOne → runner.query (on-demand subprocess)
//     - cursor 永続化 (MarkAsRead は handleOne 前に完了済み — issue #176)
//  5. SIGTERM/Ctrl+C → runGracefulDrain() で未処理メッセージ処理 → exit
//
// 環境変数:
//   AGENT_HUB_URL               required    agent-hub MCP エンドポイント
//   GITHUB_PAT                  required    GitHub Personal Access Token
//   AGENT_HUB_TENANT            optional    テナント ID (--tenant フラグが優先)
//   CODEX_CLI_PATH              optional    codex CLI のパス (省略 = PATH 上の "codex")
//   CODEX_HOME_DIR              optional    永続 CODEX_HOME ディレクトリ (省略 = ~/.agent-hub/codex-home/<user>)
//   AGENT_HUB_MODEL             optional    codex model (-m フラグ相当)
//   AGENT_HUB_CURSOR_FILE       optional    cursor ファイルパス
//   AGENT_HUB_JOURNAL_DIR       optional    journal ディレクトリ
//   AGENT_HUB_BUSY_WINDOW_S     optional    /status busy 判定ウィンドウ秒数 (default: 60)
//   AGENT_HUB_PUSH_SILENT_THRESHOLD_S optional gap 警告閾値秒数 (default: 25)
//   BRIDGE_INVENTORY            optional    bridge inventory ファイルパス
//   AGENT_HUB_BRIDGE_MAX_RETRIES optional   circuit breaker 連続失敗上限 (default: 10, 0=無限)
//   AGENT_HUB_TELEMETRY_URL     optional    OTLP endpoint
//   BRIDGE_LOG_DIR              optional    ログディレクトリ (省略 = ~/.agent-hub/logs/; --log-file が優先)
//   BRIDGE_LOG_FILE             optional    ログファイルパス (省略 = BRIDGE_LOG_DIR/bridge-<user>.log)
//
// Issue: #186
package main

import (
	"context"
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
	CodexCLI    string
	Model       string
	Mode        string   // stateful | stateless | global (default: stateful)
	AddDirs     []string // --add-dir で指定した追加ディレクトリ (繰り返し可)
	LogLevel    string
	LogFile     string // ログファイルパス ("" = stderr のみ)
	CodexHomeDir string // 永続 CODEX_HOME ディレクトリ
	// PollInterval は get_messages のポーリング間隔。
	PollInterval time.Duration
	// ReconnectBackoff は MCP セッション再接続待機時間。
	ReconnectBackoff time.Duration
	// MaxRetries は circuit breaker の連続失敗上限 (0 = 無制限)。
	MaxRetries int
	// SubprocessTimeout は runner.query の最大実行時間 (0 = タイムアウトなし)。
	SubprocessTimeout time.Duration
}

func parseConfig() (*config, error) {
	var (
		user              = flag.String("user", "", "agent-hub handle (without @) [required]")
		displayName       = flag.String("display-name", "", "display name (optional)")
		tenant            = flag.String("tenant", "", "agent-hub tenant ID (overrides AGENT_HUB_TENANT env)")
		workdir           = flag.String("workdir", "", "peer workdir (default: cwd)")
		model             = flag.String("model", "", "codex model override (default: AGENT_HUB_MODEL env or codex default)")
		mode              = flag.String("mode", "stateful", "peer mode: stateful|stateless|global")
		logLevel          = flag.String("log-level", "info", "log level: debug|info|warn|error")
		logFile           = flag.String("log-file", "", "log file path (default: ~/.agent-hub/logs/bridge-<user>.log; overrides BRIDGE_LOG_DIR/BRIDGE_LOG_FILE)")
		pollInterval      = flag.Duration("poll-interval", 5*time.Second, "get_messages polling interval")
		reconnectBackoff  = flag.Duration("reconnect-backoff", 5*time.Second, "backoff on MCP reconnect")
		maxRetries        = flag.Int("max-retries", 10, "circuit breaker: max consecutive get_messages failures (0 = unlimited)")
		subprocessTimeout = flag.Duration("subprocess-timeout", 10*time.Minute, "codex subprocess max runtime per query (0 = no timeout)")
		showVersion       = flag.Bool("version", false, "print version and exit")
		addDirs           stringSlice
	)
	flag.Var(&addDirs, "add-dir", "add extra directory to codex context (repeatable; only applied on new sessions)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("bridge-codex2/%s (agent-hub-bridges)\n", version)
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

	// CODEX_CLI_PATH が未設定なら PATH 上の "codex" を使う
	codexCLI := os.Getenv("CODEX_CLI_PATH")
	if codexCLI == "" {
		codexCLI = "codex"
	}
	if _, err := exec.LookPath(codexCLI); err != nil {
		return nil, fmt.Errorf("codex CLI %q not found in PATH: %w", codexCLI, err)
	}

	// model: --model フラグ > AGENT_HUB_MODEL env > "" (= codex default)
	resolvedModel := *model
	if resolvedModel == "" {
		resolvedModel = os.Getenv("AGENT_HUB_MODEL")
	}

	// display_name: --display-name フラグ > "{user} — codex bridge"
	resolvedDisplayName := *displayName
	if resolvedDisplayName == "" {
		resolvedDisplayName = *user + " — codex bridge"
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

	// codex home dir: CODEX_HOME_DIR env > ~/.agent-hub/codex-home/<user>
	codexHomeDir := os.Getenv("CODEX_HOME_DIR")
	if codexHomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		codexHomeDir = filepath.Join(home, ".agent-hub", "codex-home", *user)
	}

	return &config{
		User:              *user,
		DisplayName:       resolvedDisplayName,
		AgentHubURL:       url,
		GitHubPAT:         pat,
		Tenant:            tenantValue(*tenant),
		Workdir:           wd,
		CodexCLI:          codexCLI,
		Model:             resolvedModel,
		Mode:              *mode,
		AddDirs:           normalizedAddDirs,
		LogLevel:          *logLevel,
		LogFile:           resolvedLogFile,
		CodexHomeDir:      codexHomeDir,
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
// CODEX_HOME セットアップ                                                  //
// ──────────────────────────────────────────────────────────────────────── //

// envUserID / envTenantID は config.toml の env_http_headers が参照する環境変数名。
// subprocess env にこれらの値をセットすることで bridge identity を codex に注入する。
const (
	envUserID   = "CODEX_BRIDGE_USER_ID"
	envTenantID = "CODEX_BRIDGE_TENANT_ID"
)

// setupCodexHome は永続 CODEX_HOME ディレクトリを初期化する。
// - config.toml を書き込む (agent-hub MCP 設定)
// - auth.json を ~/.codex/auth.json へのシンボリックリンクとして作成する
// 既存の symlink / config.toml は上書きする (設定変更を反映するため)。
func setupCodexHome(cfg *config) error {
	dir := cfg.CodexHomeDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create codex home dir %q: %w", dir, err)
	}

	if err := writeCodexConfigTOML(dir, cfg); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}

	if err := linkAuthJSON(dir); err != nil {
		// auth.json は必須ではないケースもあるため警告のみ
		slog.Warn("setupCodexHome: failed to link auth.json (codex auth may fail)", "err", err)
	}

	slog.Info("CODEX_HOME ready", "dir", dir)
	return nil
}

// writeCodexConfigTOML は CODEX_HOME/config.toml を書き込む。
// env_http_headers の値は環境変数名 (実値ではない)。
// subprocess env に envUserID / envTenantID をセットすることで解決される。
func writeCodexConfigTOML(codexHomeDir string, cfg *config) error {
	tenantLine := ""
	if cfg.Tenant != "" {
		tenantLine = fmt.Sprintf("X-Tenant-Id = %q\n", envTenantID)
	}
	content := fmt.Sprintf(
		"[mcp_servers.agent-hub]\n"+
			"url = %q\n"+
			"bearer_token_env_var = \"GITHUB_PAT\"\n"+
			"\n"+
			"[mcp_servers.agent-hub.env_http_headers]\n"+
			"X-User-Id = %q\n"+
			"%s",
		cfg.AgentHubURL,
		envUserID,
		tenantLine,
	)
	path := filepath.Join(codexHomeDir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	slog.Debug("wrote codex config.toml", "path", path)
	return nil
}

// linkAuthJSON は ~/.codex/auth.json → CODEX_HOME/auth.json のシンボリックリンクを作成する。
// token refresh 後の自動追従のためコピーではなく symlink を使う。
// 既存の symlink/ファイルがあれば削除して再作成する。
func linkAuthJSON(codexHomeDir string) error {
	src := filepath.Join(os.Getenv("HOME"), ".codex", "auth.json")
	if src == "/.codex/auth.json" {
		// HOME 未設定の場合は UserHomeDir で再試行
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home dir: %w", err)
		}
		src = filepath.Join(home, ".codex", "auth.json")
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("~/.codex/auth.json not found — run `codex login` first: %w", err)
	}

	dst := filepath.Join(codexHomeDir, "auth.json")
	// 既存の symlink/ファイルを削除して再作成
	_ = os.Remove(dst)
	if err := os.Symlink(src, dst); err != nil {
		return fmt.Errorf("symlink %q -> %q: %w", dst, src, err)
	}
	slog.Debug("linked auth.json", "src", src, "dst", dst)
	return nil
}

// ──────────────────────────────────────────────────────────────────────── //
// プロンプトフォーマット                                                   //
// ──────────────────────────────────────────────────────────────────────── //

// formatPrompt は受信メッセージを codex への user prompt に変換する。
// bridge-claude2 の formatPrompt と同等。
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

	slog.Info("bridge-codex2 starting",
		"version", version,
		"handle", "@"+cfg.User,
		"workdir", cfg.Workdir,
		"mode", cfg.Mode,
		"model", orDefault(cfg.Model, "(codex default)"),
		"poll_interval_s", cfg.PollInterval.Seconds(),
		"subprocess_timeout_s", cfg.SubprocessTimeout.Seconds(),
		"codex_home", cfg.CodexHomeDir,
		"log_file", orDefault(cfg.LogFile, "(stderr only)"),
	)

	// CODEX_HOME セットアップ (config.toml + auth.json symlink)
	if err := setupCodexHome(cfg); err != nil {
		slog.Error("setupCodexHome failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// issue #267 相当: telemetry 初期化 (AGENT_HUB_TELEMETRY_URL 未設定なら no-op)
	initTelemetry("@" + cfg.User)
	defer shutdownTelemetry()

	// worker を起動 (内部で reconnect loop を回す)
	runWorker(ctx, cfg)

	slog.Info("bridge-codex2 stopped")
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
