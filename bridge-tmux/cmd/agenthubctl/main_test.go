package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────── //
// pidFilePath                                                               //
// ──────────────────────────────────────────────────────────────────────── //

func TestPIDFilePath(t *testing.T) {
	got := pidFilePath("/path/to/fleet.yaml")
	want := "/path/to/fleet.yaml.pid"
	if got != want {
		t.Errorf("pidFilePath = %q, want %q", got, want)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// writePIDFile / readPIDFileData                                            //
// ──────────────────────────────────────────────────────────────────────── //

func TestPIDFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml.pid")

	original := pidFileData{
		PID:        12345,
		HealthPort: 8080,
		FleetFile:  "/path/to/fleet.yaml",
		StartedAt:  "2026-06-08T12:00:00Z",
	}
	if err := writePIDFile(path, original); err != nil {
		t.Fatalf("writePIDFile: %v", err)
	}

	got, err := readPIDFileData(path)
	if err != nil {
		t.Fatalf("readPIDFileData: %v", err)
	}
	if got.PID != 12345 {
		t.Errorf("PID = %d, want 12345", got.PID)
	}
	if got.HealthPort != 8080 {
		t.Errorf("HealthPort = %d, want 8080", got.HealthPort)
	}
	if got.FleetFile != "/path/to/fleet.yaml" {
		t.Errorf("FleetFile = %q, want /path/to/fleet.yaml", got.FleetFile)
	}
}

func TestReadPIDFileData_NotExist(t *testing.T) {
	_, err := readPIDFileData("/nonexistent/fleet.yaml.pid")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestReadPIDFileData_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pid")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := readPIDFileData(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestReadPIDFileData_InvalidPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zero.pid")
	b, _ := json.Marshal(pidFileData{PID: 0})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := readPIDFileData(path)
	if err == nil {
		t.Fatal("expected error for pid=0, got nil")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// isProcessAlive                                                            //
// ──────────────────────────────────────────────────────────────────────── //

func TestIsProcessAlive_Self(t *testing.T) {
	// 自プロセスは必ず alive
	if !isProcessAlive(os.Getpid()) {
		t.Error("expected self to be alive")
	}
}

func TestIsProcessAlive_InvalidPID(t *testing.T) {
	// PID 0 や極端に大きな PID は dead
	if isProcessAlive(0) {
		t.Error("expected pid=0 to be not alive")
	}
	if isProcessAlive(999999999) {
		t.Error("expected pid=999999999 to be not alive")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// queryHealth                                                               //
// ──────────────────────────────────────────────────────────────────────── //

func TestQueryHealth_OK(t *testing.T) {
	snap := healthSnapshot{
		Status:    "ok",
		Mode:      "fleet",
		UptimeSec: 120.5,
		Personas: []personaHealth{
			{Handle: "@reviewer", SessionAlive: true, MessagesProcessed: 5},
			{Handle: "@planner", SessionAlive: false, MessagesProcessed: 3, LastError: "timeout"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap)
	}))
	defer srv.Close()

	// extract port from server URL
	addr := srv.Listener.Addr().String()
	portStr := addr[strings.LastIndex(addr, ":")+1:]
	port := 0
	if _, err := parsePort(portStr, &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}

	got, err := queryHealth(port)
	if err != nil {
		t.Fatalf("queryHealth: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if len(got.Personas) != 2 {
		t.Fatalf("got %d personas, want 2", len(got.Personas))
	}
	if got.Personas[0].Handle != "@reviewer" {
		t.Errorf("Handle = %q, want @reviewer", got.Personas[0].Handle)
	}
	if got.Personas[1].LastError != "timeout" {
		t.Errorf("LastError = %q, want timeout", got.Personas[1].LastError)
	}
}

func TestQueryHealth_Unreachable(t *testing.T) {
	// 存在しないポートへの問い合わせはエラー
	_, err := queryHealth(1) // port 1 は通常 refused
	if err == nil {
		t.Fatal("expected error for unreachable port, got nil")
	}
}

// parsePort は文字列をポート番号に変換するヘルパー。
func parsePort(s string, out *int) (string, error) {
	for i, c := range s {
		if c < '0' || c > '9' {
			return s[i:], nil
		}
		*out = *out*10 + int(c-'0')
	}
	return "", nil
}

// ──────────────────────────────────────────────────────────────────────── //
// runRemove                                                                 //
// ──────────────────────────────────────────────────────────────────────── //

func TestRunRemove_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(path, []byte(`personas:
  - handle: reviewer
    workdir: /tmp/reviewer
  - handle: planner
    workdir: /tmp/planner
`), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}

	if err := runRemove(path, "@reviewer"); err != nil {
		t.Fatalf("runRemove: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after remove: %v", err)
	}
	if strings.Contains(string(data), "reviewer") {
		t.Errorf("expected reviewer to be removed, got:\n%s", data)
	}
	if !strings.Contains(string(data), "planner") {
		t.Errorf("expected planner to remain, got:\n%s", data)
	}
}

func TestRunRemove_WithoutAtPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(path, []byte(`personas:
  - handle: planner
    workdir: /tmp/planner
`), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}
	// @ なしでも動作する
	if err := runRemove(path, "planner"); err != nil {
		t.Fatalf("runRemove without @: %v", err)
	}
}

func TestRunRemove_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(path, []byte(`personas:
  - handle: planner
    workdir: /tmp/planner
`), 0o644); err != nil {
		t.Fatalf("write fleet: %v", err)
	}
	err := runRemove(path, "@nonexistent")
	if err == nil {
		t.Fatal("expected error for missing handle, got nil")
	}
}

func TestRunRemove_EmptyHandle(t *testing.T) {
	err := runRemove("fleet.yaml", "@")
	if err == nil {
		t.Fatal("expected error for empty handle, got nil")
	}
}

func TestRunRemove_MissingFleet(t *testing.T) {
	err := runRemove("/nonexistent/fleet.yaml", "@reviewer")
	if err == nil {
		t.Fatal("expected error for missing fleet file, got nil")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// fleetLogPath                                                              //
// ──────────────────────────────────────────────────────────────────────── //

func TestFleetLogPath(t *testing.T) {
	path, err := fleetLogPath()
	if err != nil {
		t.Fatalf("fleetLogPath: %v", err)
	}
	if !strings.HasSuffix(path, filepath.Join(".agent-hub", "logs", "bridge-fleet.log")) {
		t.Errorf("unexpected log path: %q", path)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// runStatus (process-only, no health server)                               //
// ──────────────────────────────────────────────────────────────────────── //

func TestRunStatus_NoFleet(t *testing.T) {
	// fleet.yaml が存在しない場合でも status は pid file not found を gracefully 処理する
	dir := t.TempDir()
	// fleet.yaml なしで status を呼ぶ — pid file もないので "stopped" になる
	// fleet.yaml はなくても pid file チェックは通るが fleet config 読み込みでエラーになる
	// → health_port 取得の fallback で LoadFleetConfig が失敗するが status は warning を出すだけ
	// ここでは pid file がない場合の動作のみテストする
	pidFile := pidFilePath(filepath.Join(dir, "fleet.yaml"))
	_, err := readPIDFileData(pidFile)
	if !os.IsNotExist(err) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// health snapshot parsing                                                   //
// ──────────────────────────────────────────────────────────────────────── //

func TestHealthSnapshotParsing(t *testing.T) {
	now := time.Now().UTC()
	raw := healthSnapshot{
		Status:    "ok",
		Mode:      "fleet",
		UptimeSec: 300,
		Personas: []personaHealth{
			{Handle: "@reviewer", SessionAlive: true, MessagesProcessed: 10, LastMessageAt: &now},
		},
	}
	b, _ := json.Marshal(raw)
	var got healthSnapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Personas[0].MessagesProcessed != 10 {
		t.Errorf("MessagesProcessed = %d, want 10", got.Personas[0].MessagesProcessed)
	}
}
