package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// TestExtractInboundIDs は tool_use input から inbound ID を抜き出す規則を検証する (issue #264)。
func TestExtractInboundIDs(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input string
		want  []string
	}{
		{"send_message caused_by", sendMessageToolName, `{"to":"@a","message":"x","caused_by":"m1"}`, []string{"m1"}},
		{"send_message without caused_by", sendMessageToolName, `{"to":"@a","message":"x"}`, nil},
		{"mark_as_read message_id", markAsReadToolName, `{"message_id":"m1"}`, []string{"m1"}},
		{"mark_as_read message_ids", markAsReadToolName, `{"message_id":"m1","message_ids":["m2","","m3"]}`, []string{"m1", "m2", "m3"}},
		{"mark_as_read all=true", markAsReadToolName, `{"all":true}`, nil},
		{"invalid json", markAsReadToolName, `{`, nil},
		{"empty input", sendMessageToolName, ``, nil},
		{"other tool", "Bash", `{"caused_by":"m1"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractInboundIDs(tt.tool, json.RawMessage(tt.input))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractInboundIDs = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReadUntilResult_InnerHandledIDs は tool_result が成功した send_message /
// mark_as_read の inbound ID だけが usage.InnerHandledIDs に積まれることを検証する (issue #264)。
func TestReadUntilResult_InnerHandledIDs(t *testing.T) {
	tests := []struct {
		name          string
		lines         []string
		want          []string
		wantConfirmed bool
		wantCausedBy  []string
	}{
		{
			name: "send_message + mark_as_read success",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__agent-hub__mark_as_read","input":{"message_ids":["m2","m3"]}},{"type":"tool_use","id":"t2","name":"mcp__agent-hub__send_message","input":{"to":"@a","message":"x","caused_by":"m2"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false},{"type":"tool_result","tool_use_id":"t2","is_error":false}]}}`,
				`{"type":"result","subtype":"success","is_error":false}`,
			},
			want:          []string{"m2", "m3", "m2"},
			wantConfirmed: true,
			wantCausedBy:  []string{"m2"},
		},
		{
			name: "failed tool_result is not recorded",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__agent-hub__mark_as_read","input":{"message_id":"m2"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true}]}}`,
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"mcp__agent-hub__send_message","input":{"caused_by":"m3"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","is_error":true}]}}`,
				`{"type":"result","subtype":"success","is_error":false}`,
			},
			want:          nil,
			wantConfirmed: false,
		},
		{
			name: "killed before tool_result → not recorded",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__agent-hub__send_message","input":{"caused_by":"m2"}}]}}`,
			},
			want:          nil,
			wantConfirmed: false,
		},
		{
			name: "second send_message is confirmed after first failed",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__agent-hub__send_message","input":{"caused_by":"m1"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true}]}}`,
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"mcp__agent-hub__send_message","input":{"caused_by":"m1"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","is_error":false}]}}`,
				`{"type":"result","subtype":"success","is_error":false}`,
			},
			want:          []string{"m1"},
			wantConfirmed: true,
			wantCausedBy:  []string{"m1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scanner := bufio.NewScanner(strings.NewReader(strings.Join(tt.lines, "\n") + "\n"))
			usage, _ := readUntilResult(context.Background(), scanner, io.Discard, nil, false)
			if !reflect.DeepEqual(usage.InnerHandledIDs, tt.want) {
				t.Errorf("usage.InnerHandledIDs = %v, want %v", usage.InnerHandledIDs, tt.want)
			}
			if usage.SentMessageConfirmed != tt.wantConfirmed {
				t.Errorf("usage.SentMessageConfirmed = %v, want %v", usage.SentMessageConfirmed, tt.wantConfirmed)
			}
			if !reflect.DeepEqual(usage.ConfirmedReplyCausedBy, tt.wantCausedBy) {
				t.Errorf("usage.ConfirmedReplyCausedBy = %v, want %v", usage.ConfirmedReplyCausedBy, tt.wantCausedBy)
			}
		})
	}
}

// TestInnerHandledSet は集合の追加・重複・空文字・FIFO eviction・nil receiver を検証する。
func TestInnerHandledSet(t *testing.T) {
	s := newInnerHandledSet()
	s.add("a", "", "b", "a")
	if s.size() != 2 || !s.has("a") || !s.has("b") || s.has("") || s.has("c") {
		t.Fatalf("unexpected set state: size=%d", s.size())
	}
	// has は削除しない
	if !s.has("a") {
		t.Fatal("has must not remove the entry")
	}

	for i := 0; i < innerHandledMaxEntries; i++ {
		s.add("x" + string(rune('0'+i%10)) + strings.Repeat("y", i/10))
	}
	if s.size() != innerHandledMaxEntries {
		t.Fatalf("size = %d, want %d", s.size(), innerHandledMaxEntries)
	}
	if s.has("a") || s.has("b") {
		t.Error("oldest entries should be evicted")
	}

	var n *innerHandledSet
	n.add("a")
	if n.has("a") || n.size() != 0 {
		t.Error("nil set must be a no-op")
	}
}

// TestRecordInnerHandled は現在処理中の inbound 自身を除外して記録することを検証する。
func TestRecordInnerHandled(t *testing.T) {
	r := &claudeRunner{innerHandled: newInnerHandledSet()}
	recordInnerHandled(r, "cur", []string{"cur", "m2", "cur", "m3"})
	if r.innerHandled.has("cur") {
		t.Error("current message must not be recorded")
	}
	if !r.innerHandled.has("m2") || !r.innerHandled.has("m3") || r.innerHandled.size() != 2 {
		t.Errorf("unexpected set size=%d", r.innerHandled.size())
	}

	// nil runner / nil set でも panic しない
	recordInnerHandled(nil, "cur", []string{"m2"})
	recordInnerHandled(&claudeRunner{}, "cur", []string{"m2"})
}

// TestSpawnSubprocess_SetsBridgeEnv は子プロセスに AGENT_HUB_BRIDGE=1 が渡ることを検証する
// (issue #264 方針 B)。親環境の値 (=0) より後ろに付与されるため子では 1 が有効になる。
func TestSpawnSubprocess_SetsBridgeEnv(t *testing.T) {
	t.Setenv("AGENT_HUB_BRIDGE", "0")
	script := filepath.Join(t.TempDir(), "fake-claude.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"AGENT_HUB_BRIDGE=$AGENT_HUB_BRIDGE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &claudeRunner{cfg: &config{ClaudeCLI: script, Workdir: t.TempDir()}}
	cmd, stdin, scanner, err := r.spawnSubprocess(context.Background())
	if err != nil {
		t.Fatalf("spawnSubprocess: %v", err)
	}
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
	}()
	if !scanner.Scan() {
		t.Fatalf("no output: %v", scanner.Err())
	}
	if got := scanner.Text(); got != "AGENT_HUB_BRIDGE=1" {
		t.Errorf("child env = %q, want AGENT_HUB_BRIDGE=1", got)
	}
}

// TestProcessMessages_SkipsInnerHandled は内側セッションが先回りで返信した inbound (m2) を
// bridge が再 dispatch せず、MarkAsRead のみ行うことを end-to-end で検証する (issue #264 方針 A)。
func TestProcessMessages_SkipsInnerHandled(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	lines := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__agent-hub__send_message","input":{"to":"@a","message":"reply to m1","caused_by":"m1"}},{"type":"tool_use","id":"t2","name":"mcp__agent-hub__send_message","input":{"to":"@b","message":"early reply to m2","caused_by":"m2"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false},{"type":"tool_result","tool_use_id":"t2","is_error":false}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done"}`,
	}, "\n")
	script := filepath.Join(dir, "fake-claude")
	body := "#!/bin/sh\nexec 3<&0; cat <&3 >/dev/null &\necho x >> '" + countFile + "'\nprintf '%s\\n' '" + lines + "'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	client, cfg, runner, hub, journal := newTestEnv(t, script)
	runner.innerHandled = newInnerHandledSet()

	msgs := []agenthub.Message{
		{ID: "m1", Sender: "@a", To: "@limit-test", Body: "first", Timestamp: "2026-09-16T09:00:01.000Z"},
		{ID: "m2", Sender: "@b", To: "@limit-test", Body: "second", Timestamp: "2026-09-16T09:00:02.000Z"},
	}
	processMessages(context.Background(), cfg, client, runner, nil, cursorPos{},
		&activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{}, msgs, "test")

	data, _ := os.ReadFile(countFile)
	if n := strings.Count(string(data), "x"); n != 1 {
		t.Errorf("claude spawned %d times; want 1 (m2 must be skipped)", n)
	}
	if !runner.innerHandled.has("m2") || runner.innerHandled.has("m1") {
		t.Errorf("innerHandled: has(m2)=%v has(m1)=%v", runner.innerHandled.has("m2"), runner.innerHandled.has("m1"))
	}
	var marked []string
	for _, c := range hub.callsNamed("mark_as_read") {
		id, _ := c.Args["message_id"].(string)
		marked = append(marked, id)
	}
	if !reflect.DeepEqual(marked, []string{"m1", "m2"}) {
		t.Errorf("mark_as_read ids = %v, want [m1 m2]", marked)
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("bridge send_message called %d times; want 0", n)
	}
}

// earlyReplyLines は send_message (caused_by=causedBy) の成功を流した後に resultLine で終わる
// stream-json を返す (PR #273 レビュー M1 の経路用)。
func earlyReplyLines(causedBy, resultLine string) string {
	return strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__agent-hub__send_message","input":{"to":"@x","message":"reply","caused_by":"` + causedBy + `"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false}]}}`,
		resultLine,
	}, "\n")
}

// TestHandleOne_EarlyReplyToOtherInbound_DoesNotSuppress: 処理中 inbound (msg-1) の失敗時、
// 別 inbound (m2) への返信成功だけではエラー通知 / limit 休眠を抑止しない (PR #273 レビュー M1)。
func TestHandleOne_EarlyReplyToOtherInbound_DoesNotSuppress(t *testing.T) {
	t.Run("generic error still auto-replies", func(t *testing.T) {
		script := writeFakeClaude(t, t.TempDir(), earlyReplyLines("m2", genericErrorResultLine()), 1)
		client, cfg, runner, hub, journal := newTestEnv(t, script)
		err := handleOne(context.Background(), client, runner, inbound("hello"), cfg, &activityTracker{}, journal)
		if err == nil {
			t.Fatal("want error")
		}
		if n := len(hub.callsNamed("send_message")); n != 1 {
			t.Errorf("send_message called %d times; want 1 (auto error reply to msg-1)", n)
		}
	})
	t.Run("limit still enters sleep", func(t *testing.T) {
		script := writeFakeClaude(t, t.TempDir(), earlyReplyLines("m2", limitResultLine()), 1)
		client, cfg, runner, _, journal := newTestEnv(t, script)
		err := handleOne(context.Background(), client, runner, inbound("hello"), cfg, &activityTracker{}, journal)
		if asLimitError(err) == nil {
			t.Fatalf("want limitReachedError, got %v", err)
		}
	})
	t.Run("reply to current inbound suppresses auto-reply", func(t *testing.T) {
		script := writeFakeClaude(t, t.TempDir(), earlyReplyLines("msg-1", genericErrorResultLine()), 1)
		client, cfg, runner, hub, journal := newTestEnv(t, script)
		err := handleOne(context.Background(), client, runner, inbound("hello"), cfg, &activityTracker{}, journal)
		if err == nil {
			t.Fatal("want error")
		}
		if n := len(hub.callsNamed("send_message")); n != 0 {
			t.Errorf("send_message called %d times; want 0 (already replied to msg-1)", n)
		}
	})
}
