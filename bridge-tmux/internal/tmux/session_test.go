package tmux

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────── //
// Mock runner                                                              //
// ──────────────────────────────────────────────────────────────────────── //

// mockRunner records all tmux calls and returns preset results.
// runQueue / outQueue are consumed FIFO; when exhausted, runDef / outDef apply.
type mockRunner struct {
	runCalls [][]string
	outCalls [][]string
	runQueue []error
	outQueue []outItem
	runDef   error
	outDef   []byte
}

type outItem struct {
	out []byte
	err error
}

func (m *mockRunner) run(args ...string) error {
	m.runCalls = append(m.runCalls, args)
	if len(m.runQueue) > 0 {
		e := m.runQueue[0]
		m.runQueue = m.runQueue[1:]
		return e
	}
	return m.runDef
}

func (m *mockRunner) runCtx(_ context.Context, args ...string) error {
	return m.run(args...)
}

func (m *mockRunner) runWithStdin(_ io.Reader, args ...string) error {
	return m.run(args...)
}

func (m *mockRunner) output(args ...string) ([]byte, error) {
	m.outCalls = append(m.outCalls, args)
	if len(m.outQueue) > 0 {
		item := m.outQueue[0]
		m.outQueue = m.outQueue[1:]
		return item.out, item.err
	}
	return m.outDef, nil
}

// counterRunner always returns unique, ever-increasing output content.
// Useful for testing ResponseTimeout (content never stabilises).
type counterRunner struct {
	n atomic.Int64
}

func (r *counterRunner) run(_ ...string) error                            { return nil }
func (r *counterRunner) runCtx(_ context.Context, _ ...string) error     { return nil }
func (r *counterRunner) runWithStdin(_ io.Reader, _ ...string) error     { return nil }
func (r *counterRunner) output(_ ...string) ([]byte, error) {
	n := r.n.Add(1)
	return []byte(fmt.Sprintf("content-%d", n)), nil
}

// newTestSession builds a Session wired to the given runner with fast timing.
func newTestSession(r tmuxRunner) *Session {
	return &Session{
		Name:             "test-session",
		Workdir:          "/tmp",
		MCPConfigPath:    "/tmp/mcp.json",
		ClaudeCLI:        "claude",
		BypassPerms:      true,
		SpawnTimeout:     200 * time.Millisecond,
		ActivityIdleTime: 50 * time.Millisecond,
		ResponseTimeout:  500 * time.Millisecond,
		PollInterval:     10 * time.Millisecond,
		GracefulWait:     20 * time.Millisecond,
		MinActivityWait:  0, // no startup sleep in tests
		runner:           r,
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// shellQuote                                                               //
// ──────────────────────────────────────────────────────────────────────── //

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hello", "'hello'"},
		{"", "''"},
		{"/path/to/file.json", "'/path/to/file.json'"},
		{"it's", "'it'\\''s'"},
		{"'quoted'", "''\\''quoted'\\'''"},
	}
	for _, tc := range cases {
		got := shellQuote(tc.in)
		if got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// buildCLICommand                                                          //
// ──────────────────────────────────────────────────────────────────────── //

func TestBuildCLICommand_Basic(t *testing.T) {
	s := &Session{ClaudeCLI: "claude", MCPConfigPath: "/tmp/c.json"}
	got := s.buildCLICommand()
	want := "'claude' --mcp-config '/tmp/c.json'"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildCLICommand_BypassPerms(t *testing.T) {
	s := &Session{ClaudeCLI: "claude", MCPConfigPath: "/tmp/c.json", BypassPerms: true}
	got := s.buildCLICommand()
	if !strings.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("missing --dangerously-skip-permissions: %q", got)
	}
}

func TestBuildCLICommand_Model(t *testing.T) {
	s := &Session{ClaudeCLI: "claude", MCPConfigPath: "/tmp/c.json", Model: "claude-sonnet-4-5"}
	got := s.buildCLICommand()
	if !strings.Contains(got, "--model 'claude-sonnet-4-5'") {
		t.Errorf("missing --model: %q", got)
	}
}

func TestBuildCLICommand_Continue(t *testing.T) {
	s := &Session{ClaudeCLI: "claude", MCPConfigPath: "/tmp/c.json", startedBefore: true}
	got := s.buildCLICommand()
	if !strings.Contains(got, "--continue") {
		t.Errorf("missing --continue: %q", got)
	}
}

func TestBuildCLICommand_NoContinueFirstStart(t *testing.T) {
	s := &Session{ClaudeCLI: "claude", MCPConfigPath: "/tmp/c.json", startedBefore: false}
	got := s.buildCLICommand()
	if strings.Contains(got, "--continue") {
		t.Errorf("unexpected --continue on first start: %q", got)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// IsAlive                                                                  //
// ──────────────────────────────────────────────────────────────────────── //

func TestSession_IsAlive_True(t *testing.T) {
	r := &mockRunner{} // runDef = nil → has-session succeeds
	s := newTestSession(r)
	if !s.IsAlive() {
		t.Error("IsAlive() = false, want true")
	}
}

func TestSession_IsAlive_False(t *testing.T) {
	r := &mockRunner{runDef: fmt.Errorf("no session")}
	s := newTestSession(r)
	if s.IsAlive() {
		t.Error("IsAlive() = true, want false")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// Start                                                                    //
// ──────────────────────────────────────────────────────────────────────── //

func TestSession_Start_Success(t *testing.T) {
	r := &mockRunner{}
	// IsAlive (has-session): not alive → skip "already alive" branch
	r.runQueue = []error{fmt.Errorf("no session")}
	// runCtx calls: new-session (nil), send-keys (nil) via runDef=nil
	// capturePaneText: baseline "init", then "changed" (activity)
	r.outQueue = []outItem{
		{out: []byte("init output")},    // baseline
		{out: []byte("changed output")}, // poll → activity detected
	}

	s := newTestSession(r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if !s.startedBefore {
		t.Error("startedBefore should be true after successful Start")
	}
}

func TestSession_Start_AlreadyAlive_StopsFirst(t *testing.T) {
	r := &mockRunner{}
	// First IsAlive (in Start): alive (nil)
	// Stop() calls: IsAlive (nil=alive), send-keys C-c (nil), wait, IsAlive (error=dead)
	// Then new-session, send-keys start, capturePaneText x2
	r.runQueue = []error{
		nil,                       // Start: IsAlive → alive → will Stop
		nil,                       // Stop: IsAlive → alive
		nil,                       // Stop: send-keys C-c
		fmt.Errorf("no session"),  // Stop: IsAlive after wait → dead (skip kill)
		// new-session: nil (runDef)
		// send-keys: nil (runDef)
	}
	r.outQueue = []outItem{
		{out: []byte("baseline")},
		{out: []byte("started!")},
	}

	s := newTestSession(r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
}

func TestSession_Start_SpawnTimeout(t *testing.T) {
	r := &mockRunner{}
	// IsAlive: not alive
	r.runQueue = []error{fmt.Errorf("no session")} // initial IsAlive
	// output always returns same content → no activity → timeout
	r.outDef = []byte("static content")
	// Stop() on timeout: IsAlive (nil=alive), send-keys, wait, IsAlive (nil=alive), kill-session

	s := newTestSession(r)
	s.SpawnTimeout = 30 * time.Millisecond

	err := s.Start(context.Background())
	if err == nil {
		t.Error("Start() should return error on spawn timeout")
	}
	if !strings.Contains(err.Error(), "did not start within") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSession_Start_ContextCancelled(t *testing.T) {
	r := &mockRunner{}
	r.runQueue = []error{fmt.Errorf("no session")} // initial IsAlive
	r.outDef = []byte("static")

	s := newTestSession(r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := s.Start(ctx)
	if err == nil {
		t.Error("Start() should return error on cancelled context")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// Stop                                                                     //
// ──────────────────────────────────────────────────────────────────────── //

func TestSession_Stop_NotAlive(t *testing.T) {
	r := &mockRunner{runDef: fmt.Errorf("no session")} // IsAlive → false
	s := newTestSession(r)
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop() on dead session = %v, want nil", err)
	}
	// No tmux kill commands should be called
	for _, call := range r.runCalls {
		if len(call) > 0 && call[0] == "kill-session" {
			t.Error("kill-session should not be called on dead session")
		}
	}
}

func TestSession_Stop_GracefulExit(t *testing.T) {
	r := &mockRunner{}
	// IsAlive (first): alive
	// send-keys C-c: success
	// gracefulWait (20ms in test) passes
	// IsAlive (after wait): dead → skip kill-session
	r.runQueue = []error{
		nil,                      // IsAlive → alive
		nil,                      // send-keys C-c
		fmt.Errorf("no session"), // IsAlive after wait → dead
	}

	s := newTestSession(r)
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop() = %v, want nil", err)
	}

	// Verify kill-session was NOT called
	for _, call := range r.runCalls {
		if len(call) > 0 && call[0] == "kill-session" {
			t.Error("kill-session should not be called when graceful exit succeeds")
		}
	}
}

func TestSession_Stop_ForceKill(t *testing.T) {
	r := &mockRunner{}
	// IsAlive (first): alive
	// send-keys C-c: success
	// gracefulWait passes
	// IsAlive (after wait): still alive → force kill
	// kill-session: success
	r.runQueue = []error{
		nil, // IsAlive → alive
		nil, // send-keys C-c
		nil, // IsAlive after wait → still alive
		nil, // kill-session
	}

	s := newTestSession(r)
	if err := s.Stop(context.Background()); err != nil {
		t.Errorf("Stop() = %v, want nil", err)
	}

	// Verify kill-session was called
	killCalled := false
	for _, call := range r.runCalls {
		if len(call) > 0 && call[0] == "kill-session" {
			killCalled = true
		}
	}
	if !killCalled {
		t.Error("kill-session should be called when graceful exit fails")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// InjectMessage                                                            //
// ──────────────────────────────────────────────────────────────────────── //

func TestSession_InjectMessage_Success(t *testing.T) {
	r := &mockRunner{} // all calls succeed (runDef=nil)
	s := newTestSession(r)

	if err := s.InjectMessage("hello world"); err != nil {
		t.Fatalf("InjectMessage() error: %v", err)
	}

	// Verify correct tmux calls were made
	calls := make(map[string]bool)
	for _, call := range r.runCalls {
		if len(call) > 0 {
			calls[call[0]] = true
		}
	}
	for _, expected := range []string{"load-buffer", "paste-buffer", "send-keys", "delete-buffer"} {
		if !calls[expected] {
			t.Errorf("expected tmux %s call, not found", expected)
		}
	}
}

func TestSession_InjectMessage_LoadBufferError(t *testing.T) {
	r := &mockRunner{}
	r.runQueue = []error{fmt.Errorf("load-buffer failed")}
	s := newTestSession(r)

	err := s.InjectMessage("hello")
	if err == nil {
		t.Error("InjectMessage() should fail when load-buffer errors")
	}
	if !strings.Contains(err.Error(), "load-buffer") {
		t.Errorf("expected 'load-buffer' in error, got: %v", err)
	}
}

func TestSession_InjectMessage_PasteFail(t *testing.T) {
	r := &mockRunner{}
	// load-buffer: success, paste-buffer: fail
	r.runQueue = []error{nil, fmt.Errorf("paste failed")}
	s := newTestSession(r)

	err := s.InjectMessage("hello")
	if err == nil {
		t.Error("InjectMessage() should fail when paste-buffer errors")
	}
	if !strings.Contains(err.Error(), "paste-buffer") {
		t.Errorf("expected 'paste-buffer' in error, got: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// WaitForIdle                                                              //
// ──────────────────────────────────────────────────────────────────────── //

func TestSession_WaitForIdle_Success(t *testing.T) {
	r := &mockRunner{
		outQueue: []outItem{
			{out: []byte("baseline")}, // initial capturePaneText
			{out: []byte("baseline")}, // Phase 1 poll: no change yet
			{out: []byte("changed!")}, // Phase 1 poll: activity detected
		},
		outDef: []byte("changed!"), // Phase 2: stable content
		// runDef = nil → IsAlive always true
	}
	s := newTestSession(r)

	if err := s.WaitForIdle(context.Background()); err != nil {
		t.Fatalf("WaitForIdle() error: %v", err)
	}
}

func TestSession_WaitForIdle_ActivityTimeout(t *testing.T) {
	r := &mockRunner{
		outDef: []byte("never changes"), // pane never changes → Phase 1 times out
	}
	s := newTestSession(r)
	s.ResponseTimeout = 50 * time.Millisecond

	err := s.WaitForIdle(context.Background())
	if err == nil {
		t.Error("WaitForIdle() should error when activity never starts")
	}
	if !strings.Contains(err.Error(), "did not start processing") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSession_WaitForIdle_SessionDied(t *testing.T) {
	r := &mockRunner{
		outQueue: []outItem{
			{out: []byte("baseline")}, // initial
			{out: []byte("changed!")}, // Phase 1: activity detected
		},
		outDef: []byte("changed!"),
		// Phase 2: IsAlive → dead
		runDef: fmt.Errorf("session gone"),
	}
	s := newTestSession(r)

	err := s.WaitForIdle(context.Background())
	if err == nil {
		t.Error("WaitForIdle() should error when session dies")
	}
	if !strings.Contains(err.Error(), "died") {
		t.Errorf("expected 'died' in error, got: %v", err)
	}
}

func TestSession_WaitForIdle_ResponseTimeout(t *testing.T) {
	// Content always changes → Phase 2 never stabilises → ResponseTimeout
	cr := &counterRunner{}
	s := newTestSession(cr)
	s.ResponseTimeout = 60 * time.Millisecond
	s.ActivityIdleTime = 1 * time.Second // very long, never reached

	err := s.WaitForIdle(context.Background())
	if err == nil {
		t.Error("WaitForIdle() should error on response timeout")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSession_WaitForIdle_ContextCancelled(t *testing.T) {
	r := &mockRunner{outDef: []byte("static")}
	s := newTestSession(r)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.WaitForIdle(ctx)
	if err == nil {
		t.Error("WaitForIdle() should error on cancelled context")
	}
}
