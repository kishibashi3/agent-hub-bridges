package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// ──────────────────────────────────────────────────────────────────────── //
// 返信の作法 (C1) と運転の幅 (C4) を worker loop 経由で検証する (issue #336) //
//                                                                          //
// mock hub (worker_cursor_test.go) が get_messages を返し send_message を   //
// 記録する。fake codex (shell script) は受け取った本文を log に追記したうえで //
// テストごとの振る舞い (sleep / 失敗 / 内側の返信イベント) をする。          //
// ──────────────────────────────────────────────────────────────────────── //

// writeFakeCodex は `body <id>` を logPath に追記してから tail を実行する fake codex を書く。
// tail の中では $id に `body <id>` が入っている。
func writeFakeCodex(t *testing.T, dir, logPath, tail string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-codex")
	script := "#!/bin/sh\nid=$(grep -o 'body [a-z0-9]*' | head -n 1)\necho \"$id\" >> '" + logPath + "'\n" + tail + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// mcpReplyEvent は内側 codex が agent-hub の send_message を呼んだときの item.completed 行
// (codex CLI 0.163.0 の実出力と同じ形) を返す。
func mcpReplyEvent(causedBy, status string) string {
	return `{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"agent-hub",` +
		`"tool":"send_message","arguments":{"to":"@a","message":"reply","caused_by":"` + causedBy + `"},` +
		`"result":null,"error":null,"status":"` + status + `"}}`
}

type c1Env struct {
	dir     string
	logPath string
	hub     *mockHub
	cfg     *config
}

func newC1Env(t *testing.T, timeout time.Duration, tail string, inbox ...string) *c1Env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(cursorFileEnv, filepath.Join(dir, "cursor.json"))
	t.Setenv(journalDirEnv, filepath.Join(dir, "journals"))
	logPath := filepath.Join(dir, "dispatched.log")
	hub := newMockHub(t, inbox...)
	return &c1Env{
		dir:     dir,
		logPath: logPath,
		hub:     hub,
		cfg: &config{
			User:              "codex-test",
			AgentHubURL:       hub.srv.URL,
			GitHubPAT:         "ghp_test",
			Mode:              "stateful",
			Workdir:           dir,
			CodexCLI:          writeFakeCodex(t, dir, logPath, tail),
			CodexHomeDir:      dir,
			PollInterval:      time.Hour,
			SubprocessTimeout: timeout,
			ScannerBufferSize: 64 * 1024,
		},
	}
}

// run は runHubSession を回し、until が true になるか deadline で ctx を cancel して終了を待つ。
func (e *c1Env) run(t *testing.T, until func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = runHubSession(ctx, e.cfg, newCodexRunner(e.cfg), cursorPos{},
			&activityTracker{}, &messageGapTracker{}, newJournal(e.cfg.User))
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !until() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runHubSession did not return after ctx cancel")
	}
}

func (e *c1Env) dispatched(t *testing.T) []string { return readDispatched(t, e.logPath) }

func journalEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "journals"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

const c1TS = "2026-10-17T09:00:01.000Z"

func msgFrom(id, sender, body string) agenthub.Message {
	return agenthub.Message{ID: id, Sender: sender, To: "@codex-test", Body: body, Timestamp: c1TS}
}

// #1 (C4): 上限に達した依頼はリトライせず、送信元へエラーが 1 回だけ届く。
func TestHandleOne_TimeoutSendsOneErrorWithoutRetry(t *testing.T) {
	e := newC1Env(t, 500*time.Millisecond, "exec sleep 30",
		inboxJSON(t, msgAt("m1", c1TS)))
	e.run(t, func() bool { return len(e.hub.sentMessages()) >= 1 })
	// リトライが無いことを見るため、少し待ってから数える
	time.Sleep(time.Second)

	sent := e.hub.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent = %+v, want exactly 1 error reply", sent)
	}
	if sent[0].To != "@a" || sent[0].CausedBy != "m1" {
		t.Errorf("sent to=%q caused_by=%q, want @a / m1", sent[0].To, sent[0].CausedBy)
	}
	if !strings.HasPrefix(sent[0].Message, autoErrorPrefix) || !strings.Contains(sent[0].Message, "SubprocessTimeout=500ms") {
		t.Errorf("sent message = %q, want auto error mentioning the timeout", sent[0].Message)
	}
	if got := e.dispatched(t); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("dispatched = %v, want [m1] (no retry)", got)
	}
}

// #2 (C1・C4) / #337: 作業中に停止したら、送信元へ偽エラーは送らず、中断通知をちょうど 1 通送る。
// 送信に成功した通知は journal に残さない (次回起動時に 2 通目として replay されないように)。
func TestHandleOne_ShutdownDuringQuerySendsOneInterruptNotice(t *testing.T) {
	e := newC1Env(t, time.Minute, "exec sleep 30",
		inboxJSON(t, msgAt("m1", c1TS)))
	e.run(t, func() bool { return len(e.dispatched(t)) >= 1 })

	sent := e.hub.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent = %+v, want exactly 1 interrupt notice", sent)
	}
	if sent[0].To != "@a" || sent[0].CausedBy != "m1" {
		t.Errorf("sent to=%q caused_by=%q, want @a / m1", sent[0].To, sent[0].CausedBy)
	}
	if !strings.HasPrefix(sent[0].Message, autoNoticePrefix) || strings.HasPrefix(sent[0].Message, autoErrorPrefix) {
		t.Errorf("sent message = %q, want an interrupt notice (not an error)", sent[0].Message)
	}
	if j := journalEntries(t, e.dir); len(j) != 0 {
		t.Errorf("journal entries = %v, want none (would be replayed as a second reply on next startup)", j)
	}
}

// #337: 内側 codex が処理中の依頼へ返信した後に停止した場合は、中断通知を重ねて送らない。
// auto 返信への中断通知も送らない。
func TestHandleOne_ShutdownNoNoticeWhenRepliedOrAutoReply(t *testing.T) {
	cases := []struct {
		name string
		tail string
		msg  agenthub.Message
	}{
		{"inner replied", "echo '" + mcpReplyEvent("m1", "completed") + "'\nexec sleep 30", msgAt("m1", c1TS)},
		{"auto reply inbound", "exec sleep 30", msgFrom("m1", "@a", "(auto) bridge-claude2 error: body m1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newC1Env(t, time.Minute, tc.tail, inboxJSON(t, tc.msg))
			start := time.Now()
			// 内側の返信イベントを runner が読み終えるまで少し待ってから止める
			e.run(t, func() bool { return len(e.dispatched(t)) >= 1 && time.Since(start) > 500*time.Millisecond })
			if sent := e.hub.sentMessages(); len(sent) != 0 {
				t.Errorf("sent = %+v, want no interrupt notice", sent)
			}
		})
	}
}

// #3 (C1): auto 返信への auto 返信はしない。同じ送信元への auto 返信は cooldown 内 1 回まで。
func TestHandleOne_AutoReplyEchoAndCooldown(t *testing.T) {
	e := newC1Env(t, 10*time.Second, "exit 1", inboxJSON(t,
		msgFrom("m1", "@a", "body m1"),
		msgFrom("m2", "@a", "body m2"),
		msgFrom("m3", "@b", "(auto) bridge-claude2 error: body m3"),
		msgFrom("m4", "@c", "(自動応答) body m4"),
	))
	e.run(t, func() bool { return len(e.dispatched(t)) >= 4 })

	if got := e.dispatched(t); !reflect.DeepEqual(got, []string{"m1", "m2", "m3", "m4"}) {
		t.Fatalf("dispatched = %v, want all 4", got)
	}
	sent := e.hub.sentMessages()
	if len(sent) != 1 || sent[0].To != "@a" || sent[0].CausedBy != "m1" {
		t.Errorf("sent = %+v, want exactly 1 auto reply to @a for m1", sent)
	}
}

// #4 (C1): 内側 codex が先回りで返信した inbound は bridge から流さない。
func TestRunHubSession_SkipsMessageRepliedByInnerCodex(t *testing.T) {
	tail := `if [ "$id" = "body m1" ]; then echo '` + mcpReplyEvent("m2", "completed") + `'; fi
echo '{"type":"turn.completed"}'`
	e := newC1Env(t, 10*time.Second, tail,
		inboxJSON(t, msgAt("m1", c1TS), msgAt("m2", c1TS), msgAt("m3", c1TS)))
	e.run(t, func() bool { return len(e.dispatched(t)) >= 2 })

	if got := e.dispatched(t); !reflect.DeepEqual(got, []string{"m1", "m3"}) {
		t.Errorf("dispatched = %v, want [m1 m3] (m2 was already answered by the inner codex)", got)
	}
}

// #4 (C1): 送信が失敗 (status=failed) した inbound は返信済み扱いしない。
func TestRunHubSession_FailedInnerReplyNotSkipped(t *testing.T) {
	tail := `if [ "$id" = "body m1" ]; then echo '` + mcpReplyEvent("m2", "failed") + `'; fi
echo '{"type":"turn.completed"}'`
	e := newC1Env(t, 10*time.Second, tail,
		inboxJSON(t, msgAt("m1", c1TS), msgAt("m2", c1TS)))
	e.run(t, func() bool { return len(e.dispatched(t)) >= 2 })

	if got := e.dispatched(t); !reflect.DeepEqual(got, []string{"m1", "m2"}) {
		t.Errorf("dispatched = %v, want [m1 m2]", got)
	}
}

// C1: 内側 codex が処理中の inbound へ返信した後に失敗しても、エラーを重ねて返さない。
// 返信が失敗していた場合はエラーを返す。
func TestHandleOne_ErrorAfterReply(t *testing.T) {
	cases := []struct {
		status   string
		wantSent int
	}{
		{"completed", 0},
		{"failed", 1},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			tail := `echo '` + mcpReplyEvent("m1", tc.status) + `'
exit 1`
			e := newC1Env(t, 10*time.Second, tail, inboxJSON(t, msgAt("m1", c1TS)))
			e.run(t, func() bool { return len(e.dispatched(t)) >= 1 && len(e.hub.sentMessages()) >= tc.wantSent })
			time.Sleep(200 * time.Millisecond)
			if sent := e.hub.sentMessages(); len(sent) != tc.wantSent {
				t.Errorf("sent = %+v, want %d", sent, tc.wantSent)
			}
		})
	}
}

// repliedCausedBy は codex CLI 0.163.0 の実出力 (2026-10-17 採取) から send_message の成功だけを拾う。
func TestRepliedCausedBy(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{"send_message completed", mcpReplyEvent("abc", "completed"), "abc", true},
		{"send_message failed", mcpReplyEvent("abc", "failed"), "", false},
		{"send_message started", strings.Replace(mcpReplyEvent("abc", "in_progress"), "item.completed", "item.started", 1), "", false},
		{"mark_as_read", `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"agent-hub","tool":"mark_as_read","arguments":{"message_id":"abc"},"status":"completed"}}`, "", false},
		{"other server", strings.Replace(mcpReplyEvent("abc", "completed"), `"agent-hub"`, `"other"`, 1), "", false},
		{"no caused_by", `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"agent-hub","tool":"send_message","arguments":{"to":"@a","message":"x"},"status":"completed"}}`, "", false},
		{"agent_message", `{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hi"}}`, "", false},
		{"turn.completed", `{"type":"turn.completed","usage":{"input_tokens":1}}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ev codexEvent
			if err := json.Unmarshal([]byte(tc.line), &ev); err != nil {
				t.Fatal(err)
			}
			got, ok := repliedCausedBy(ev)
			if got != tc.want || ok != tc.ok {
				t.Errorf("repliedCausedBy = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestResolveSubprocessTimeout(t *testing.T) {
	cases := []struct {
		name    string
		flag    time.Duration
		env     string
		want    time.Duration
		wantErr bool
	}{
		{"default", -1, "", 30 * time.Minute, false},
		{"env", -1, "45m", 45 * time.Minute, false},
		{"env zero = unlimited", -1, "0", 0, false},
		{"flag wins over env", 20 * time.Minute, "45m", 20 * time.Minute, false},
		{"flag zero", 0, "45m", 0, false},
		{"env invalid", -1, "15", 0, true},
		{"env negative", -1, "-5m", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_HUB_SUBPROCESS_TIMEOUT", tc.env)
			got, err := resolveSubprocessTimeout(tc.flag)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestBuildGitHubFooter(t *testing.T) {
	cases := []struct {
		model, want string
	}{
		{"", "@codex-test [bridge-codex2] (operator-supervised)"},
		{"gpt-5.5", "@codex-test [bridge-codex2 · gpt-5.5] (operator-supervised)"},
	}
	for _, tc := range cases {
		if got := buildGitHubFooter("codex-test", bridgeType, tc.model); got != tc.want {
			t.Errorf("buildGitHubFooter(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

func TestFormatPrompt_Footer(t *testing.T) {
	msg := msgAt("m1", c1TS)
	footer := "@codex-test [bridge-codex2] (operator-supervised)"
	if got := formatPrompt("@codex-test", msg, footer); !strings.HasSuffix(got, "\n"+footer) {
		t.Errorf("prompt does not end with the footer:\n%s", got)
	}
	if got := formatPrompt("@codex-test", msg, ""); strings.Contains(got, "GitHub 投稿ルール") {
		t.Errorf("prompt has footer rule although footer is empty:\n%s", got)
	}
}
