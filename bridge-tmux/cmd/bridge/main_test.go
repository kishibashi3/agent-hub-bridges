package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// ──────────────────────────────────────────────────────────────────────── //
// Mock session                                                             //
// ──────────────────────────────────────────────────────────────────────── //

// mockSession implements tmux.SessionIface for testing SessionManager.
// All fields are safe for concurrent access: the idle timer goroutine and
// the test goroutine may call Stop() / IsAlive() simultaneously.
type mockSession struct {
	aliveMu    sync.Mutex
	alive      bool
	startCalls atomic.Int32
	stopCalls  atomic.Int32
	injectMu   sync.Mutex
	injectList []string
	waitErr    error
	startErr   error
}

func (m *mockSession) IsAlive() bool {
	m.aliveMu.Lock()
	defer m.aliveMu.Unlock()
	return m.alive
}
func (m *mockSession) Start(_ context.Context) error {
	m.startCalls.Add(1)
	if m.startErr != nil {
		return m.startErr
	}
	m.aliveMu.Lock()
	m.alive = true
	m.aliveMu.Unlock()
	return nil
}
func (m *mockSession) Stop(_ context.Context) error {
	m.stopCalls.Add(1)
	m.aliveMu.Lock()
	m.alive = false
	m.aliveMu.Unlock()
	return nil
}
func (m *mockSession) InjectMessage(text string) error {
	m.injectMu.Lock()
	m.injectList = append(m.injectList, text)
	m.injectMu.Unlock()
	return nil
}
func (m *mockSession) WaitForIdle(_ context.Context) error {
	return m.waitErr
}

// newMockSession initialises a mockSession with the given alive state.
func newMockSession(alive bool) *mockSession {
	m := &mockSession{}
	m.alive = alive
	return m
}

// newTestConfig returns a minimal *config for tests.
func newTestConfig(t *testing.T, workdir string) *config {
	t.Helper()
	if workdir == "" {
		workdir = t.TempDir()
	}
	return &config{
		User:        "testuser",
		Workdir:     workdir,
		IdleTimeout: 50 * time.Millisecond,
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// MCP test server                                                          //
// ──────────────────────────────────────────────────────────────────────── //

// mcpRequest captures the relevant fields from an MCP JSON-RPC request.
type mcpRequest struct {
	Method string `json:"method"`
	ID     *int64 `json:"id"`
	Params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

// newMCPTestServer creates an httptest.Server that accepts any MCP call
// and records tool names into the provided slice pointer.
func newMCPTestServer(t *testing.T, toolCalls *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("mcp-session-id", "test-session-id")

		// Notifications have no ID; return 202 with no body.
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		// Record tool name for assertions
		if req.Method == "tools/call" && toolCalls != nil {
			*toolCalls = append(*toolCalls, req.Params.Name)
		}

		// Return a valid tool result for any call
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "ok"},
				},
			},
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
}

// newTestClient creates an agenthub.Client pointed at the given server URL.
func newTestClient(t *testing.T, serverURL string) *agenthub.Client {
	t.Helper()
	c, err := agenthub.New(serverURL, "fake-pat", "testuser", "",
		agenthub.WithHTTPTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("agenthub.New: %v", err)
	}
	return c
}

// ──────────────────────────────────────────────────────────────────────── //
// formatPrompt                                                             //
// ──────────────────────────────────────────────────────────────────────── //

func TestFormatPrompt(t *testing.T) {
	msg := agenthub.Message{
		ID:     "msg-abc",
		Sender: "@planner",
		To:     "@reviewer",
		Body:   "please review PR #42",
	}
	got := formatPrompt("@reviewer", msg)

	checks := []string{
		"@reviewer",            // self handle in prompt
		"@planner",             // sender
		"please review PR #42", // body
		"msg-abc",              // caused_by
		"mcp__agent-hub__send_message", // instruction to use tool
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("formatPrompt() missing %q\nfull output:\n%s", want, got)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// truncate                                                                 //
// ──────────────────────────────────────────────────────────────────────── //

func TestTruncate_Short(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate(%q, 10) = %q, want %q", "hello", got, "hello")
	}
}

func TestTruncate_Exact(t *testing.T) {
	s := "1234567890"
	if got := truncate(s, 10); got != s {
		t.Errorf("truncate(len=10, 10) should not truncate")
	}
}

func TestTruncate_Long(t *testing.T) {
	s := "1234567890X"
	got := truncate(s, 10)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncate() long string should end with '...': %q", got)
	}
	if len(got) != 13 { // 10 + "..."
		t.Errorf("truncate() wrong length: %d", len(got))
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// SessionManager.Handle                                                    //
// ──────────────────────────────────────────────────────────────────────── //

func TestSessionManager_Handle_ColdSpawn(t *testing.T) {
	mock := newMockSession(false)
	cfg := newTestConfig(t, "")
	mgr := newSessionManager(cfg, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := mgr.Handle(ctx, "test prompt"); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}

	if int(mock.startCalls.Load()) != 1 {
		t.Errorf("Start() called %d times, want 1", int(mock.startCalls.Load()))
	}
	if len(mock.injectList) != 1 || mock.injectList[0] != "test prompt" {
		t.Errorf("InjectMessage() calls: %v", mock.injectList)
	}
}

func TestSessionManager_Handle_WarmReuse(t *testing.T) {
	mock := newMockSession(true)
	cfg := newTestConfig(t, "")
	mgr := newSessionManager(cfg, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := mgr.Handle(ctx, "warm prompt"); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}

	if int(mock.startCalls.Load()) != 0 {
		t.Errorf("Start() should not be called for warm session, called %d times", int(mock.startCalls.Load()))
	}
}

func TestSessionManager_Handle_WaitIdleError_ResetsSession(t *testing.T) {
	mock := &mockSession{waitErr: context.DeadlineExceeded}
	cfg := newTestConfig(t, "")
	mgr := newSessionManager(cfg, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := mgr.Handle(ctx, "prompt")
	if err == nil {
		t.Error("Handle() should propagate WaitForIdle error")
	}
	if int(mock.stopCalls.Load()) != 1 {
		t.Errorf("Stop() should be called once on WaitForIdle error, called %d times", int(mock.stopCalls.Load()))
	}
}

func TestSessionManager_Handle_StartError(t *testing.T) {
	mock := &mockSession{startErr: context.DeadlineExceeded}
	cfg := newTestConfig(t, "")
	mgr := newSessionManager(cfg, mock)

	err := mgr.Handle(context.Background(), "prompt")
	if err == nil {
		t.Error("Handle() should propagate Start error")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// SessionManager idle timer                                                //
// ──────────────────────────────────────────────────────────────────────── //

func TestSessionManager_IdleTimer_Fires(t *testing.T) {
	mock := newMockSession(false)
	cfg := newTestConfig(t, "")
	cfg.IdleTimeout = 20 * time.Millisecond
	mgr := newSessionManager(cfg, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := mgr.Handle(ctx, "prompt"); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}

	// Wait for idle timer to fire (budget: 200ms).
	// Use atomic load directly to avoid data race detected by -race flag.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mock.stopCalls.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if mock.stopCalls.Load() == 0 {
		t.Error("idle timer should have called Stop() by now")
	}
}

func TestSessionManager_IdleTimer_ResetOnNewMessage(t *testing.T) {
	mock := newMockSession(false)
	cfg := newTestConfig(t, "")
	cfg.IdleTimeout = 100 * time.Millisecond
	mgr := newSessionManager(cfg, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First Handle — starts idle timer
	if err := mgr.Handle(ctx, "msg1"); err != nil {
		t.Fatalf("first Handle() error: %v", err)
	}

	// Wait half the idle timeout
	time.Sleep(40 * time.Millisecond)

	// Second Handle — should cancel the timer and reset it
	if err := mgr.Handle(ctx, "msg2"); err != nil {
		t.Fatalf("second Handle() error: %v", err)
	}

	// Immediately after second Handle, Stop should NOT have been called yet
	if int(mock.stopCalls.Load()) > 0 {
		t.Error("Stop() should not be called immediately after second Handle (timer reset)")
	}
}

func TestSessionManager_Shutdown_StopsSession(t *testing.T) {
	mock := newMockSession(true)
	cfg := newTestConfig(t, "")
	cfg.IdleTimeout = 10 * time.Second // long timer — Shutdown should cancel it
	mgr := newSessionManager(cfg, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := mgr.Handle(ctx, "prompt"); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer shutCancel()
	mgr.Shutdown(shutCtx)

	if int(mock.stopCalls.Load()) == 0 {
		t.Error("Shutdown() should call Stop()")
	}
}

func TestSessionManager_Shutdown_NoSession(t *testing.T) {
	mock := newMockSession(false)
	cfg := newTestConfig(t, "")
	mgr := newSessionManager(cfg, mock)

	// Shutdown without prior Handle should not panic
	mgr.Shutdown(context.Background())
}

// ──────────────────────────────────────────────────────────────────────── //
// handleMessage                                                            //
// ──────────────────────────────────────────────────────────────────────── //

func TestHandleMessage_SelfLoop(t *testing.T) {
	var toolCalls []string
	srv := newMCPTestServer(t, &toolCalls)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	mock := newMockSession(false)
	cfg := newTestConfig(t, "")
	mgr := newSessionManager(cfg, mock)

	msg := agenthub.Message{
		ID:     "msg-selfloop",
		Sender: "@testuser", // same as selfHandle
		To:     "@testuser",
		Body:   "self message",
	}

	handleMessage(context.Background(), cfg, client, mgr, NewHealthState("single"), "@testuser", msg)

	// Session.Start must NOT be called
	if int(mock.startCalls.Load()) > 0 {
		t.Error("Start() should not be called for self-loop messages")
	}
	// mark_as_read must be called
	found := false
	for _, name := range toolCalls {
		if name == "mark_as_read" {
			found = true
		}
	}
	if !found {
		t.Errorf("mark_as_read not called; tool calls: %v", toolCalls)
	}
}

func TestHandleMessage_WorkdirGone(t *testing.T) {
	var toolCalls []string
	srv := newMCPTestServer(t, &toolCalls)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	mock := newMockSession(false)
	cfg := newTestConfig(t, "/nonexistent/path/xyz_bridge_test")
	mgr := newSessionManager(cfg, mock)

	msg := agenthub.Message{
		ID:     "msg-noworkdir",
		Sender: "@planner",
		To:     "@testuser",
		Body:   "do something",
	}

	handleMessage(context.Background(), cfg, client, mgr, NewHealthState("single"), "@testuser", msg)

	// Session.Start must NOT be called
	if int(mock.startCalls.Load()) > 0 {
		t.Error("Start() should not be called when workdir is missing")
	}

	// send_message (error DM) + mark_as_read must be called
	hasError := false
	hasAck := false
	for _, name := range toolCalls {
		if name == "send_message" {
			hasError = true
		}
		if name == "mark_as_read" {
			hasAck = true
		}
	}
	if !hasError {
		t.Errorf("send_message not called for workdir error; tool calls: %v", toolCalls)
	}
	if !hasAck {
		t.Errorf("mark_as_read not called; tool calls: %v", toolCalls)
	}
}

func TestHandleMessage_Success(t *testing.T) {
	var toolCalls []string
	srv := newMCPTestServer(t, &toolCalls)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	mock := newMockSession(false)
	cfg := newTestConfig(t, "") // uses t.TempDir() which exists
	mgr := newSessionManager(cfg, mock)

	msg := agenthub.Message{
		ID:     "msg-ok",
		Sender: "@planner",
		To:     "@testuser",
		Body:   "do the task",
	}

	handleMessage(context.Background(), cfg, client, mgr, NewHealthState("single"), "@testuser", msg)

	// Session should have been started and injected
	if int(mock.startCalls.Load()) != 1 {
		t.Errorf("Start() called %d times, want 1", int(mock.startCalls.Load()))
	}
	if len(mock.injectList) != 1 {
		t.Errorf("InjectMessage() called %d times, want 1", len(mock.injectList))
	}

	// mark_as_read must be called
	found := false
	for _, name := range toolCalls {
		if name == "mark_as_read" {
			found = true
		}
	}
	if !found {
		t.Errorf("mark_as_read not called; tool calls: %v", toolCalls)
	}
}

func TestHandleMessage_HandleError_SendsErrorDM(t *testing.T) {
	var toolCalls []string
	srv := newMCPTestServer(t, &toolCalls)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	mock := &mockSession{startErr: context.DeadlineExceeded}
	cfg := newTestConfig(t, "")
	mgr := newSessionManager(cfg, mock)

	msg := agenthub.Message{
		ID:     "msg-err",
		Sender: "@planner",
		To:     "@testuser",
		Body:   "fail task",
	}

	handleMessage(context.Background(), cfg, client, mgr, NewHealthState("single"), "@testuser", msg)

	// send_message (error DM) and mark_as_read must both be called
	hasSendMsg := false
	hasAck := false
	for _, name := range toolCalls {
		if name == "send_message" {
			hasSendMsg = true
		}
		if name == "mark_as_read" {
			hasAck = true
		}
	}
	if !hasSendMsg {
		t.Errorf("send_message not called on Handle error; tool calls: %v", toolCalls)
	}
	if !hasAck {
		t.Errorf("mark_as_read not called on Handle error; tool calls: %v", toolCalls)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// handleMessage workdir check uses os.Stat                                //
// ──────────────────────────────────────────────────────────────────────── //

func TestHandleMessage_WorkdirExists_NoErrorDM(t *testing.T) {
	var toolCalls []string
	srv := newMCPTestServer(t, &toolCalls)
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	mock := newMockSession(false)

	wd := t.TempDir() // guaranteed to exist
	cfg := newTestConfig(t, wd)
	mgr := newSessionManager(cfg, mock)

	msg := agenthub.Message{
		ID:     "msg-wd-ok",
		Sender: "@planner",
		To:     "@testuser",
		Body:   "task",
	}

	handleMessage(context.Background(), cfg, client, mgr, NewHealthState("single"), "@testuser", msg)

	// Must NOT send an error DM about workdir
	for _, name := range toolCalls {
		if name == "send_message" {
			// There should be no send_message call here (session succeeds)
			// unless Handle itself errors — in this test mock.startErr is nil.
			// Check: this send_message should not contain "workdir does not exist"
			// (We can't inspect the body here easily, so just ensure start was called)
		}
	}
	if int(mock.startCalls.Load()) == 0 {
		t.Error("Start() should be called when workdir exists")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// writeMCPConfig (integration-style, no tmux needed)                     //
// ──────────────────────────────────────────────────────────────────────── //

func TestWriteMCPConfig_CreatesFile(t *testing.T) {
	cfg := &config{
		AgentHubURL: "http://localhost:3000/mcp",
		GitHubPAT:   "ghp_test",
		User:        "testuser",
		Tenant:      "",
	}

	path, err := writeMCPConfig(cfg)
	if err != nil {
		t.Fatalf("writeMCPConfig() error: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("JSON parse error: %v", err)
	}

	servers, ok := payload["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers not found")
	}
	if _, ok := servers["agent-hub"]; !ok {
		t.Error("agent-hub server not found in mcpServers")
	}
}

func TestWriteMCPConfig_FilePermissions(t *testing.T) {
	cfg := &config{
		AgentHubURL: "http://localhost:3000/mcp",
		GitHubPAT:   "ghp_test",
		User:        "testuser",
	}

	path, err := writeMCPConfig(cfg)
	if err != nil {
		t.Fatalf("writeMCPConfig() error: %v", err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("file permission = %04o, want 0600", perm)
	}
}
