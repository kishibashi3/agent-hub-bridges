// agenthubctl — fleet management CLI for bridge-tmux.
//
// Commands:
//
//	start [--health-port PORT]          Start the fleet (launches bridge-tmux in background)
//	stop                                Stop the fleet
//	status                              Show fleet status (/health endpoint or PID check)
//	remove @handle                      Remove a persona from fleet.yaml
//	bridge spawn @handle [flags]        Spawn a single bridge from bridges.json registry
//
// Global flag:
//
//	--fleet FILE   Fleet YAML config file (default: fleet.yaml)
//
// The bridge-tmux binary is resolved from:
//  1. Same directory as agenthubctl
//  2. PATH
//
// Issue: #150, #215
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	bridges "github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/bridges"
	fleet "github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/fleet"
)

// ──────────────────────────────────────────────────────────────────────── //
// main                                                                      //
// ──────────────────────────────────────────────────────────────────────── //

func main() {
	fs := flag.NewFlagSet("agenthubctl", flag.ExitOnError)
	fleetFile := fs.String("fleet", "fleet.yaml", "fleet YAML config file")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: agenthubctl [--fleet FILE] <command> [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  start [--health-port PORT]         Start the fleet\n")
		fmt.Fprintf(os.Stderr, "  stop                               Stop the fleet\n")
		fmt.Fprintf(os.Stderr, "  status                             Show fleet status\n")
		fmt.Fprintf(os.Stderr, "  remove @handle                     Remove a persona from fleet.yaml\n")
		fmt.Fprintf(os.Stderr, "  bridge spawn @handle [flags]       Spawn a bridge from bridges.json registry\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}

	cmd := args[0]
	rest := args[1:]

	var err error
	switch cmd {
	case "start":
		err = runStart(*fleetFile, rest)
	case "stop":
		err = runStop(*fleetFile)
	case "status":
		err = runStatus(*fleetFile)
	case "remove":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "error: remove requires @handle argument")
			os.Exit(2)
		}
		err = runRemove(*fleetFile, rest[0])
	case "bridge":
		err = runBridgeCmd(rest)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n", cmd)
		fs.Usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// start                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

func runStart(fleetFile string, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	healthPort := fs.Int("health-port", 0, "HTTP /health port (overrides fleet.yaml health_port)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absFleet, err := filepath.Abs(fleetFile)
	if err != nil {
		return fmt.Errorf("resolve fleet path: %w", err)
	}

	// fleet.yaml を読んで health_port を取得 (--health-port フラグで上書き可能)
	cfg, err := fleet.LoadFleetConfig(absFleet)
	if err != nil {
		return err
	}

	port := cfg.HealthPort
	if *healthPort != 0 {
		port = *healthPort
	}

	// 既に起動中かチェック
	pidFile := pidFilePath(absFleet)
	if pid, err := readPIDFile(pidFile); err == nil {
		if isProcessAlive(pid) {
			return fmt.Errorf("fleet is already running (pid=%d, pid_file=%s)", pid, pidFile)
		}
		// stale PID file — remove and continue
		_ = os.Remove(pidFile)
	}

	bridgeBin, err := findBridgeBinary()
	if err != nil {
		return err
	}

	// ログファイル
	logPath, err := fleetLogPath()
	if err != nil {
		return fmt.Errorf("resolve log path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", logPath, err)
	}
	defer logFile.Close()

	// bridge-tmux 引数を組み立てる
	bridgeArgs := []string{"--fleet", absFleet}
	if port > 0 {
		bridgeArgs = append(bridgeArgs, "--health-port", strconv.Itoa(port))
	}

	cmd := exec.Command(bridgeBin, bridgeArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// 親プロセス終了後も生き残るよう SysProcAttr で新しいプロセスグループを作成する
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bridge-tmux: %w", err)
	}

	// PID file に記録
	pidData := pidFileData{
		PID:        cmd.Process.Pid,
		HealthPort: port,
		FleetFile:  absFleet,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := writePIDFile(pidFile, pidData); err != nil {
		// PID file の書き込みに失敗してもプロセスは動いているので警告のみ
		fmt.Fprintf(os.Stderr, "warning: failed to write pid file: %v\n", err)
	}

	fmt.Printf("fleet started (pid=%d)\n", cmd.Process.Pid)
	fmt.Printf("  fleet:  %s\n", absFleet)
	fmt.Printf("  log:    %s\n", logPath)
	if port > 0 {
		fmt.Printf("  health: http://127.0.0.1:%d/health\n", port)
	}
	fmt.Printf("  pid:    %s\n", pidFile)
	return nil
}

// ──────────────────────────────────────────────────────────────────────── //
// stop                                                                      //
// ──────────────────────────────────────────────────────────────────────── //

func runStop(fleetFile string) error {
	absFleet, err := filepath.Abs(fleetFile)
	if err != nil {
		return fmt.Errorf("resolve fleet path: %w", err)
	}
	pidFile := pidFilePath(absFleet)

	data, err := readPIDFileData(pidFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "warning: no pid file found — fleet may not be running")
			return nil
		}
		return fmt.Errorf("read pid file: %w", err)
	}

	pid := data.PID
	if !isProcessAlive(pid) {
		fmt.Printf("fleet process (pid=%d) is not running — removing stale pid file\n", pid)
		_ = os.Remove(pidFile)
		return nil
	}

	// SIGTERM を送信して最大 15 秒待つ
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM to pid %d: %w", pid, err)
	}
	fmt.Printf("sent SIGTERM to fleet (pid=%d)\n", pid)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if !isProcessAlive(pid) {
			fmt.Println("fleet stopped")
			_ = os.Remove(pidFile)
			return nil
		}
	}

	// タイムアウト → SIGKILL
	fmt.Fprintf(os.Stderr, "warning: SIGTERM timeout — sending SIGKILL to pid %d\n", pid)
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		fmt.Fprintf(os.Stderr, "warning: SIGKILL failed: %v\n", err)
	}
	_ = os.Remove(pidFile)
	fmt.Println("fleet killed")
	return nil
}

// ──────────────────────────────────────────────────────────────────────── //
// status                                                                    //
// ──────────────────────────────────────────────────────────────────────── //

// healthSnapshot mirrors the JSON response from bridge-tmux /health.
type healthSnapshot struct {
	Status    string          `json:"status"`
	Mode      string          `json:"mode"`
	UptimeSec float64         `json:"uptime_s"`
	Personas  []personaHealth `json:"personas"`
}

type personaHealth struct {
	Handle            string     `json:"handle"`
	SessionAlive      bool       `json:"session_alive"`
	MessagesProcessed int64      `json:"messages_processed"`
	LastMessageAt     *time.Time `json:"last_message_at,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
}

func runStatus(fleetFile string) error {
	absFleet, err := filepath.Abs(fleetFile)
	if err != nil {
		return fmt.Errorf("resolve fleet path: %w", err)
	}
	pidFile := pidFilePath(absFleet)

	data, pidErr := readPIDFileData(pidFile)
	if pidErr != nil && !errors.Is(pidErr, os.ErrNotExist) {
		return fmt.Errorf("read pid file: %w", pidErr)
	}

	// プロセス状態
	if pidErr != nil || data == nil {
		fmt.Println("fleet: stopped (no pid file)")
	} else if isProcessAlive(data.PID) {
		fmt.Printf("fleet: running (pid=%d, started=%s)\n", data.PID, data.StartedAt)
	} else {
		fmt.Printf("fleet: dead (pid=%d — stale pid file)\n", data.PID)
		_ = os.Remove(pidFile)
	}

	// /health エンドポイントを問い合わせる
	port := 0
	if data != nil {
		port = data.HealthPort
	}
	// fleet.yaml の health_port も確認 (pid file にない場合のフォールバック)
	if port == 0 {
		if cfg, err := fleet.LoadFleetConfig(absFleet); err == nil {
			port = cfg.HealthPort
		}
	}

	if port == 0 {
		fmt.Println("\n(no health_port configured — add health_port to fleet.yaml for detailed status)")
		return nil
	}

	snap, err := queryHealth(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nwarning: /health unavailable: %v\n", err)
		return nil
	}

	fmt.Printf("\nhealth: %s  mode: %s  uptime: %.0fs\n\n", snap.Status, snap.Mode, snap.UptimeSec)
	fmt.Printf("%-30s  %-7s  %8s  %s\n", "HANDLE", "SESSION", "MSG", "LAST_ERROR")
	fmt.Println(strings.Repeat("-", 70))
	for _, p := range snap.Personas {
		alive := "dead"
		if p.SessionAlive {
			alive = "alive"
		}
		lastErr := p.LastError
		if len(lastErr) > 30 {
			lastErr = lastErr[:27] + "..."
		}
		fmt.Printf("%-30s  %-7s  %8d  %s\n", p.Handle, alive, p.MessagesProcessed, lastErr)
	}
	return nil
}

func queryHealth(port int) (*healthSnapshot, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var snap healthSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, fmt.Errorf("parse health response: %w", err)
	}
	return &snap, nil
}

// ──────────────────────────────────────────────────────────────────────── //
// remove                                                                    //
// ──────────────────────────────────────────────────────────────────────── //

func runRemove(fleetFile, handle string) error {
	absFleet, err := filepath.Abs(fleetFile)
	if err != nil {
		return fmt.Errorf("resolve fleet path: %w", err)
	}

	// @ prefix を正規化
	h := strings.TrimPrefix(handle, "@")
	if h == "" {
		return fmt.Errorf("handle must not be empty")
	}

	cfg, err := fleet.LoadFleetConfig(absFleet)
	if err != nil {
		return err
	}

	found := false
	remaining := cfg.Personas[:0]
	for _, p := range cfg.Personas {
		if p.Handle == h {
			found = true
			continue
		}
		remaining = append(remaining, p)
	}
	if !found {
		return fmt.Errorf("persona %q not found in %s", handle, absFleet)
	}
	if len(remaining) == 0 {
		return fmt.Errorf("cannot remove last persona — fleet.yaml would become empty; delete the file manually if intended")
	}
	cfg.Personas = remaining

	if err := fleet.WriteFleetConfig(absFleet, cfg); err != nil {
		return err
	}
	fmt.Printf("removed @%s from %s\n", h, absFleet)

	// 稼働中なら再起動を促す
	pidFile := pidFilePath(absFleet)
	if data, err := readPIDFileData(pidFile); err == nil && isProcessAlive(data.PID) {
		fmt.Printf("\nnote: fleet is running (pid=%d) — restart to apply changes:\n", data.PID)
		fmt.Printf("  agenthubctl --fleet %s stop\n", fleetFile)
		fmt.Printf("  agenthubctl --fleet %s start\n", fleetFile)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────── //
// PID file                                                                  //
// ──────────────────────────────────────────────────────────────────────── //

type pidFileData struct {
	PID        int    `json:"pid"`
	HealthPort int    `json:"health_port,omitempty"`
	FleetFile  string `json:"fleet_file"`
	StartedAt  string `json:"started_at"`
}

// pidFilePath returns the PID file path for a fleet config file.
// e.g. /path/to/fleet.yaml → /path/to/fleet.yaml.pid
func pidFilePath(absFleetFile string) string {
	return absFleetFile + ".pid"
}

func writePIDFile(path string, data pidFileData) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readPIDFile(path string) (int, error) {
	data, err := readPIDFileData(path)
	if err != nil {
		return 0, err
	}
	return data.PID, nil
}

func readPIDFileData(path string) (*pidFileData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data pidFileData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("parse pid file %q: %w", path, err)
	}
	if data.PID <= 0 {
		return nil, fmt.Errorf("invalid pid in %q: %d", path, data.PID)
	}
	return &data, nil
}

// isProcessAlive は PID が生きているか確認する (kill -0 相当)。
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal(0) は process の存在チェック
	return proc.Signal(syscall.Signal(0)) == nil
}

// ──────────────────────────────────────────────────────────────────────── //
// binary 解決                                                               //
// ──────────────────────────────────────────────────────────────────────── //

// findBridgeBinary は bridge-tmux バイナリのパスを返す。
// agenthubctl と同じディレクトリを先に探し、次に PATH を確認する。
func findBridgeBinary() (string, error) {
	// agenthubctl と同じディレクトリ
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "bridge-tmux")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	// PATH
	if path, err := exec.LookPath("bridge-tmux"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("bridge-tmux not found: place it next to agenthubctl or add it to PATH")
}

// ──────────────────────────────────────────────────────────────────────── //
// log path                                                                  //
// ──────────────────────────────────────────────────────────────────────── //

func fleetLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agent-hub", "logs", "bridge-fleet.log"), nil
}

// ──────────────────────────────────────────────────────────────────────── //
// bridge subcommand                                                         //
// ──────────────────────────────────────────────────────────────────────── //

func runBridgeCmd(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: agenthubctl bridge <subcommand> [flags]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Subcommands:")
		fmt.Fprintln(os.Stderr, "  spawn @handle [flags]  Spawn a bridge from bridges.json registry")
		return fmt.Errorf("subcommand required")
	}
	switch args[0] {
	case "spawn":
		return runSpawn(args[1:])
	default:
		return fmt.Errorf("unknown bridge subcommand %q; available: spawn", args[0])
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// bridge spawn                                                              //
// ──────────────────────────────────────────────────────────────────────── //

func runSpawn(args []string) error {
	fs := flag.NewFlagSet("bridge spawn", flag.ContinueOnError)
	workdirFlag := fs.String("workdir", "", "override workdir (default: value from bridges.json or cwd)")
	tenantFlag := fs.String("tenant", "", "override tenant ID (default: bridges.json → AGENT_HUB_TENANT)")
	timeoutFlag := fs.String("timeout", "", "override idle timeout (e.g. 10m; default: bridges.json → bridge-tmux default)")
	modelFlag := fs.String("model", "", "override model (default: bridges.json → bridge-tmux default)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: agenthubctl bridge spawn @handle [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Spawn a bridge-tmux process for @handle in background.\n")
		fmt.Fprintf(os.Stderr, "Config is read from bridges.json (CLI flags override registry values).\n\n")
		fmt.Fprintf(os.Stderr, "Config file location (first match):\n")
		fmt.Fprintf(os.Stderr, "  1. $AGENTHUBCTL_CONFIG_DIR/bridges.json\n")
		fmt.Fprintf(os.Stderr, "  2. ~/.config/agenthubctl/bridges.json\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment:\n")
		fmt.Fprintf(os.Stderr, "  AGENT_HUB_URL           required\n")
		fmt.Fprintf(os.Stderr, "  GITHUB_PAT              required\n")
		fmt.Fprintf(os.Stderr, "  AGENT_HUB_TENANT        optional tenant ID (can be overridden by --tenant)\n")
		fmt.Fprintf(os.Stderr, "  AGENTHUBCTL_CONFIG_DIR  optional config dir\n\n")
		fmt.Fprintf(os.Stderr, "Note: --weight is not yet implemented (TODO).\n")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return fmt.Errorf("@handle is required")
	}
	handle := strings.TrimPrefix(rest[0], "@")
	if handle == "" {
		return fmt.Errorf("handle must not be empty")
	}

	// Required env vars
	hubURL := os.Getenv("AGENT_HUB_URL")
	if hubURL == "" {
		return fmt.Errorf("AGENT_HUB_URL is not set")
	}
	pat := os.Getenv("GITHUB_PAT")
	if pat == "" {
		return fmt.Errorf("GITHUB_PAT is not set")
	}

	// Load bridges.json (optional — missing file is not an error)
	var entry *bridges.Entry
	cfgPath, err := bridges.DefaultConfigPath()
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	if cfg, err := bridges.Load(cfgPath); err == nil {
		entry = cfg.Lookup(handle)
	} else if !os.IsNotExist(err) {
		// File exists but is malformed
		return err
	}

	// Resolve spawn params: bridges.json defaults → env → CLI flags
	spawnWorkdir := ""
	spawnTenant := os.Getenv("AGENT_HUB_TENANT")
	spawnTimeout := ""
	spawnModel := ""

	if entry != nil {
		spawnWorkdir = entry.Workdir
		if entry.Tenant != "" {
			spawnTenant = entry.Tenant
		}
		spawnTimeout = entry.Timeout
		spawnModel = entry.Model
	}

	if *workdirFlag != "" {
		spawnWorkdir = *workdirFlag
	}
	if *tenantFlag != "" {
		spawnTenant = *tenantFlag
	}
	if *timeoutFlag != "" {
		spawnTimeout = *timeoutFlag
	}
	if *modelFlag != "" {
		spawnModel = *modelFlag
	}

	// Fallback workdir to cwd
	if spawnWorkdir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		spawnWorkdir = cwd
	}
	spawnWorkdir, _ = filepath.Abs(spawnWorkdir)
	if _, err := os.Stat(spawnWorkdir); err != nil {
		return fmt.Errorf("workdir %q: %w", spawnWorkdir, err)
	}

	// callerUser is this process's identity for X-User-Id.
	// Use AGENT_HUB_USER if set (operator's real handle); fall back to spawn target.
	callerUser := os.Getenv("AGENT_HUB_USER")
	if callerUser == "" {
		callerUser = handle
	}

	// Pre-flight: check if already online
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if online, err := checkIsOnline(ctx, hubURL, pat, callerUser, spawnTenant, handle); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pre-flight check failed (skipping): %v\n", err)
	} else if online {
		fmt.Fprintf(os.Stderr, "warning: @%s is already online in agent-hub — spawning another instance anyway\n", handle)
	}

	// Find bridge-tmux binary
	bridgeBin, err := findBridgeBinary()
	if err != nil {
		return err
	}

	// Log file
	logPath, err := spawnLogPath(handle)
	if err != nil {
		return fmt.Errorf("resolve log path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", logPath, err)
	}
	defer logFile.Close()

	// Build bridge-tmux args
	bridgeArgs := []string{"--user", handle, "--workdir", spawnWorkdir}
	if spawnTenant != "" {
		bridgeArgs = append(bridgeArgs, "--tenant", spawnTenant)
	}
	if spawnTimeout != "" {
		bridgeArgs = append(bridgeArgs, "--idle-timeout", spawnTimeout)
	}
	if spawnModel != "" {
		bridgeArgs = append(bridgeArgs, "--model", spawnModel)
	}
	if entry != nil && entry.DisplayName != "" {
		bridgeArgs = append(bridgeArgs, "--display-name", entry.DisplayName)
	}

	cmd := exec.Command(bridgeBin, bridgeArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bridge-tmux: %w", err)
	}

	fmt.Printf("bridge spawned (pid=%d)\n", cmd.Process.Pid)
	fmt.Printf("  handle:  @%s\n", handle)
	fmt.Printf("  workdir: %s\n", spawnWorkdir)
	fmt.Printf("  log:     %s\n", logPath)
	return nil
}

func spawnLogPath(handle string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agent-hub", "logs", "bridge-"+handle+".log"), nil
}

// ──────────────────────────────────────────────────────────────────────── //
// pre-flight: is_online check via MCP get_participants                      //
// ──────────────────────────────────────────────────────────────────────── //

// checkIsOnline returns true if the given handle is currently online in agent-hub.
// This is a best-effort check — callers should treat errors as non-fatal.
// userID is the caller's identity for X-User-Id (AGENT_HUB_USER or spawn target handle).
func checkIsOnline(ctx context.Context, hubURL, pat, userID, tenantID, handle string) (bool, error) {
	hc := &http.Client{Timeout: 10 * time.Second}

	sid, err := mcpInitialize(ctx, hc, hubURL, pat, userID, tenantID)
	if err != nil {
		return false, err
	}
	text, err := mcpCallTool(ctx, hc, hubURL, pat, userID, tenantID, sid, "get_participants", nil)
	if err != nil {
		return false, err
	}
	return parseHandleIsOnline(text, handle)
}

func mcpInitialize(ctx context.Context, hc *http.Client, endpoint, pat, userID, tenantID string) (string, error) {
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "agenthubctl", "version": "0.1.0"},
		},
	}
	body, _ := json.Marshal(initReq)
	sid, err := mcpPost(ctx, hc, endpoint, pat, userID, tenantID, "", body)
	if err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}

	// notifications/initialized (no id = notification, response body ignored)
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	}
	notifBody, _ := json.Marshal(notif)
	if _, err := mcpPost(ctx, hc, endpoint, pat, userID, tenantID, sid, notifBody); err != nil {
		return "", fmt.Errorf("notifications/initialized: %w", err)
	}
	return sid, nil
}

// setMCPHeaders applies the standard agent-hub MCP auth headers to req.
func setMCPHeaders(req *http.Request, pat, userID, tenantID, sessionID string) {
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("X-User-Id", userID)
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if sessionID != "" {
		req.Header.Set("mcp-session-id", sessionID)
	}
}

// mcpPost sends a POST to the MCP endpoint and returns the mcp-session-id from the response header.
// The response body is read and discarded (body parsing is handled in mcpCallTool).
func mcpPost(ctx context.Context, hc *http.Client, endpoint, pat, userID, tenantID, sessionID string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	setMCPHeaders(req, pat, userID, tenantID, sessionID)

	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	sid := resp.Header.Get("mcp-session-id")
	if sid == "" {
		sid = sessionID // keep existing sid if header absent (e.g. notifications/initialized)
	}
	return sid, nil
}

func mcpCallTool(ctx context.Context, hc *http.Client, endpoint, pat, userID, tenantID, sessionID, toolName string, toolArgs map[string]any) (string, error) {
	callReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": toolArgs,
		},
	}
	body, _ := json.Marshal(callReq)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	setMCPHeaders(req, pat, userID, tenantID, sessionID)

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("tools/call %s: %w", toolName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("tools/call %s HTTP %d: %s", toolName, resp.StatusCode, strings.TrimSpace(string(b)))
	}

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	// Handle SSE response (single-event; server closes connection after it)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		rawBody, err = extractFirstSSEData(rawBody)
		if err != nil {
			return "", err
		}
	}

	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rawBody, &rpcResp); err != nil {
		return "", fmt.Errorf("parse rpc response: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rpcResp.Result, &result); err != nil {
		return "", fmt.Errorf("parse tool result: %w", err)
	}
	if result.IsError {
		for _, c := range result.Content {
			if c.Type == "text" {
				return "", fmt.Errorf("tool error: %s", c.Text)
			}
		}
		return "", fmt.Errorf("tool %s returned isError", toolName)
	}

	var parts []string
	for _, c := range result.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// extractFirstSSEData returns the first data: payload from an SSE response body.
func extractFirstSSEData(body []byte) ([]byte, error) {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), nil
		}
	}
	return nil, fmt.Errorf("no data found in SSE response")
}

type participantEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	IsOnline bool   `json:"is_online"`
}

// parseHandleIsOnline parses a get_participants JSON response and returns is_online for the handle.
func parseHandleIsOnline(text, handle string) (bool, error) {
	h := strings.TrimPrefix(handle, "@")
	var participants []participantEntry
	if err := json.Unmarshal([]byte(text), &participants); err != nil {
		return false, fmt.Errorf("parse participants: %w", err)
	}
	for _, p := range participants {
		if p.Type == "person" && p.Name == h {
			return p.IsOnline, nil
		}
	}
	return false, nil // handle not found → not online
}
