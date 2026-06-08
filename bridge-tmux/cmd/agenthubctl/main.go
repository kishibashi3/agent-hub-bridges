// agenthubctl — fleet management CLI for bridge-tmux.
//
// Commands:
//
//	start [--health-port PORT]   Start the fleet (launches bridge-tmux in background)
//	stop                         Stop the fleet
//	status                       Show fleet status (/health endpoint or PID check)
//	remove @handle               Remove a persona from fleet.yaml
//
// Global flag:
//
//	--fleet FILE   Fleet YAML config file (default: fleet.yaml)
//
// The bridge-tmux binary is resolved from:
//  1. Same directory as agenthubctl
//  2. PATH
//
// Issue: #150
package main

import (
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
		fmt.Fprintf(os.Stderr, "  start [--health-port PORT]  Start the fleet\n")
		fmt.Fprintf(os.Stderr, "  stop                        Stop the fleet\n")
		fmt.Fprintf(os.Stderr, "  status                      Show fleet status\n")
		fmt.Fprintf(os.Stderr, "  remove @handle              Remove a persona from fleet.yaml\n\n")
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
	_ = proc.Signal(syscall.SIGKILL)
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
	body, err := io.ReadAll(resp.Body)
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
