package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// ──────────────────────────────────────────────────────────────────────── //
// handleOne / processMessages 回帰テスト (issue #268)                     //
//                                                                          //
// - fake claude CLI (shell script) で limit エラー / 一般エラーを再現する    //
// - mock hub (httptest) で tools/call を記録し、send_message の有無と       //
//   register の display_name を検証する                                    //
// ──────────────────────────────────────────────────────────────────────── //

// toolCall は mock hub が記録した tools/call 1 件。
type toolCall struct {
	Name string
	Args map[string]any
}

// mockHub は MCP initialize + tools/call を最低限処理し、呼び出しを記録する。
type mockHub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []toolCall
	// inbox は get_messages が順に返す JSON 配列テキスト。尽きたら "[]" を返す。
	inbox []string
}

func newMockHub(t *testing.T) *mockHub {
	t.Helper()
	h := &mockHub{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("mcp-session-id", "test-session")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound) // SSE は使わない
			return
		}
		var req struct {
			Method string `json:"method"`
			ID     *int64 `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := int64(0)
		if req.ID != nil {
			id = *req.ID
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"mock","version":"0"}}}`, id)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			h.mu.Lock()
			h.calls = append(h.calls, toolCall{Name: req.Params.Name, Args: req.Params.Arguments})
			text := "ok"
			if req.Params.Name == "get_messages" {
				text = "[]"
				if len(h.inbox) > 0 {
					text, h.inbox = h.inbox[0], h.inbox[1:]
				}
			}
			h.mu.Unlock()
			quoted, _ := json.Marshal(text)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%s}],"isError":false}}`, id, quoted)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, id)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *mockHub) callsNamed(name string) []toolCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []toolCall
	for _, c := range h.calls {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// writeFakeClaude は stream-json の result イベントを 1 行出力して終了する claude CLI 代替を書く。
func writeFakeClaude(t *testing.T, dir string, resultLine string, exitCode int) string {
	t.Helper()
	return writeRecordingFakeClaude(t, dir, "/dev/null", resultLine, exitCode)
}

// writeRecordingFakeClaude は writeFakeClaude と同じだが、受け取った user message 行を
// logPath に追記する (呼び出し順の検証用)。
//
// runner は initialize → user message の 2 行を書いてから result を読み、result 到達まで
// stdin を開けておく。そのため EOF は待たず 2 行読んでから result を出す。読む前に exit
// すると runner の write が broken pipe になり、result line が返らない (issue #271 追記 M2)。
func writeRecordingFakeClaude(t *testing.T, dir, logPath, resultLine string, exitCode int) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude")
	script := fmt.Sprintf("#!/bin/sh\nread -r _init\nread -r msg\nprintf '%%s\\n' \"$msg\" >> '%s'\nprintf '%%s\\n' '%s'\nexit %d\n",
		logPath, strings.ReplaceAll(resultLine, "'", `'\''`), exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func limitResultLine() string {
	return `{"type":"result","subtype":"success","is_error":true,"result":"You` + "'" + `ve hit your individual spend limit · your session limit resets 11:59pm (Asia/Tokyo)"}`
}

func genericErrorResultLine() string {
	return `{"type":"result","subtype":"error","is_error":true,"result":"something broke"}`
}

// newTestEnv は mock hub に接続した client と、fake claude を使う cfg / runner を組み立てる。
func newTestEnv(t *testing.T, claudeScript string) (*agenthub.Client, *config, *claudeRunner, *mockHub, *Journal) {
	t.Helper()
	hub := newMockHub(t)
	dir := t.TempDir()
	t.Setenv(journalDirEnv, filepath.Join(dir, "journals"))
	t.Setenv("AGENT_HUB_CURSOR_FILE", filepath.Join(dir, "cursor"))

	cfg := &config{
		Participant:       "limit-test",
		DisplayName:       "limit-test — go bridge",
		Mode:              "stateful",
		Workdir:           dir,
		ClaudeCLI:         claudeScript,
		SubprocessTimeout: 10 * time.Second,
		MaxQueryRetries:   0,
		ScannerBufferSize: 64 * 1024,
	}
	client, err := agenthub.New(hub.srv.URL, "ghp_test", cfg.Participant, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	runner := &claudeRunner{cfg: cfg, mcpConfigPath: filepath.Join(dir, "mcp.json")}
	return client, cfg, runner, hub, newJournal(cfg.Participant)
}

func inbound(body string) agenthub.Message {
	return agenthub.Message{
		ID: "msg-1", Sender: "@peer", To: "@limit-test", Body: body,
		Timestamp: "2026-09-16T09:00:00.000Z",
	}
}

// TestHandleOne_LimitError_NoAutoReply: limit 系エラーは auto 返信せず limitReachedError を返す。
func TestHandleOne_LimitError_NoAutoReply(t *testing.T) {
	script := writeFakeClaude(t, t.TempDir(), limitResultLine(), 1)
	client, cfg, runner, hub, journal := newTestEnv(t, script)

	err := handleOne(context.Background(), client, runner, inbound("hello"), cfg, &activityTracker{}, journal)
	lim := asLimitError(err)
	if lim == nil {
		t.Fatalf("want limitReachedError, got %v", err)
	}
	if lim.Kind != "spend limit" || !lim.Parsed {
		t.Errorf("kind=%q parsed=%v", lim.Kind, lim.Parsed)
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("send_message called %d times; want 0 (no auto-reply on limit)", n)
	}
	if entries := journal.loadAll(); len(entries) != 0 {
		t.Errorf("journal must stay empty (no auto-reply journalled), got %d", len(entries))
	}
}

// TestHandleOne_AutoErrorEcho_NoAutoReply: inbound が `(auto) bridge-claude2 error:` なら
// limit 以外の失敗でも auto 返信しない (二重防御)。
func TestHandleOne_AutoErrorEcho_NoAutoReply(t *testing.T) {
	script := writeFakeClaude(t, t.TempDir(), genericErrorResultLine(), 1)
	client, cfg, runner, hub, journal := newTestEnv(t, script)

	echo := autoErrorPrefix + " claude result error (subtype=success): You've hit your session limit"
	err := handleOne(context.Background(), client, runner, inbound(echo), cfg, &activityTracker{}, journal)
	if err == nil {
		t.Fatal("want error from generic failure")
	}
	if asLimitError(err) != nil {
		t.Fatal("generic failure must not be classified as limit")
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("send_message called %d times; want 0 (no auto-reply to auto error echo)", n)
	}
}

// TestHandleOne_GenericError_StillReplies: 通常 inbound の limit 以外の失敗は従来どおり auto 返信する。
func TestHandleOne_GenericError_StillReplies(t *testing.T) {
	script := writeFakeClaude(t, t.TempDir(), genericErrorResultLine(), 1)
	client, cfg, runner, hub, journal := newTestEnv(t, script)

	err := handleOne(context.Background(), client, runner, inbound("hello"), cfg, &activityTracker{}, journal)
	if err == nil {
		t.Fatal("want error")
	}
	sends := hub.callsNamed("send_message")
	if len(sends) != 1 {
		t.Fatalf("send_message called %d times; want 1", len(sends))
	}
	body, _ := sends[0].Args["message"].(string)
	if !strings.HasPrefix(body, autoErrorPrefix) {
		t.Errorf("auto reply must start with %q, got %q", autoErrorPrefix, body)
	}
	if to, _ := sends[0].Args["to"].(string); to != "@peer" {
		t.Errorf("auto reply to=%q, want @peer", to)
	}
}

// TestProcessMessages_LimitEntersSleep: limit 到達で休眠に入り、display_name が更新され、
// 残りのメッセージが deferred になり、cursor が進まないことを検証する。
func TestProcessMessages_LimitEntersSleep(t *testing.T) {
	script := writeFakeClaude(t, t.TempDir(), limitResultLine(), 1)
	client, cfg, runner, hub, journal := newTestEnv(t, script)
	sleeper := &limitSleeper{}

	msgs := []agenthub.Message{
		{ID: "m1", Sender: "@a", To: "@limit-test", Body: "first", Timestamp: "2026-09-16T09:00:01.000Z"},
		{ID: "m2", Sender: "@b", To: "@limit-test", Body: "second", Timestamp: "2026-09-16T09:00:02.000Z"},
	}
	cursor := processMessages(context.Background(), cfg, client, runner, nil, "",
		&activityTracker{}, &messageGapTracker{}, journal, sleeper, msgs, "test")

	sleeping, until, kind := sleeper.state()
	if !sleeping || kind != "spend limit" || !until.After(time.Now()) {
		t.Fatalf("sleeper state = %v %v %q", sleeping, until, kind)
	}
	if cursor != "" {
		t.Errorf("cursor advanced to %q; limit-hit message must be re-processed after wake", cursor)
	}
	if got := sleeper.deferredCount(); got != 2 {
		t.Errorf("deferred = %d, want 2 (trigger + remaining)", got)
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("send_message called %d times; want 0", n)
	}
	regs := hub.callsNamed("register")
	if len(regs) != 1 {
		t.Fatalf("register called %d times; want 1 (sleeping display_name)", len(regs))
	}
	dn, _ := regs[0].Args["display_name"].(string)
	if !strings.Contains(dn, "sleeping until") || !strings.Contains(dn, "spend limit") {
		t.Errorf("display_name = %q; want sleeping state", dn)
	}
	// fake claude は m1 の 1 回だけ呼ばれる (m2 は deferred、subprocess 起動なし)
	if calls := hub.callsNamed("mark_as_read"); len(calls) != 1 {
		t.Errorf("mark_as_read called %d times; want 1 (only m1 before limit)", len(calls))
	}
}
