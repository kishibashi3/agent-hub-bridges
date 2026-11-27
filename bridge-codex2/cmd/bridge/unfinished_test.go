package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// issue #397: codex CLI 0.163.0 の実出力と同じ形の command_execution 行。
// fake codex は sh の echo で出すので、command に単引用符やバックスラッシュを含めないこと。
func cmdStartedEvent(id, command string) string {
	return `{"type":"item.started","item":{"id":"` + id + `","type":"command_execution","command":"` + command +
		`","aggregated_output":"","exit_code":null,"status":"in_progress"}}`
}

func cmdCompletedEvent(id, command string) string {
	return `{"type":"item.completed","item":{"id":"` + id + `","type":"command_execution","command":"` + command +
		`","aggregated_output":"done","exit_code":0,"status":"completed"}}`
}

const turnCompletedEvent = `{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}`

func TestUnfinishedCommands(t *testing.T) {
	var u unfinishedCommands
	for _, ev := range []codexEvent{
		{Type: "item.started", Item: &codexItem{ID: "item_1", Type: "command_execution", Command: "a"}},
		{Type: "item.started", Item: &codexItem{ID: "item_2", Type: "command_execution", Command: "b"}},
		{Type: "item.started", Item: &codexItem{ID: "item_3", Type: "mcp_tool_call"}},
		{Type: "item.completed", Item: &codexItem{ID: "item_1", Type: "command_execution", Command: "a"}},
		{Type: "item.started", Item: &codexItem{ID: "item_4", Type: "command_execution", Command: "c"}},
	} {
		u.observe(ev)
	}
	if got, want := u.remaining(), []string{"b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
	var empty unfinishedCommands
	if got := empty.remaining(); got != nil {
		t.Errorf("remaining of empty = %v, want nil", got)
	}
}

func TestUnfinishedNoticeBody(t *testing.T) {
	body := unfinishedNoticeBody([]string{"c1", "c2", "c3", "c4", "c5"})
	if !strings.HasPrefix(body, autoNoticePrefix) {
		t.Errorf("body = %q, want auto notice prefix", body)
	}
	for _, want := range []string{"- c1", "- c3", "- ほか 2 件"} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %q, want it to contain %q", body, want)
		}
	}
	if strings.Contains(body, "- c4") {
		t.Errorf("body = %q, want at most %d commands", body, unfinishedNoticeMaxCommands)
	}
}

// issue #397: 終わっていないコマンドを残してターンが終わったら、依頼主に通知が 1 通届く。
// 内側が先に「started」と返信していても届く (#397 の B2: 返信は届くが処理は消えていた)。
// コマンドが終わっていれば何も送らない。
func TestHandleOne_UnfinishedCommandNotice(t *testing.T) {
	const cmd = "/bin/bash -lc python3-job.py"
	cases := []struct {
		name       string
		events     []string
		wantNotice bool
		wantSent   int
	}{
		{"unfinished", []string{cmdStartedEvent("item_1", cmd), turnCompletedEvent}, true, 1},
		{"unfinished after inner reply",
			[]string{cmdStartedEvent("item_1", cmd), mcpReplyEvent("m1", "completed"), turnCompletedEvent}, true, 1},
		{"completed", []string{cmdStartedEvent("item_1", cmd), cmdCompletedEvent("item_1", cmd), turnCompletedEvent}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tail strings.Builder
			for _, ev := range tc.events {
				tail.WriteString("echo '" + ev + "'\n")
			}
			e := newC1Env(t, time.Minute, tail.String(), inboxJSON(t, msgAt("m1", c1TS)))
			start := time.Now()
			e.run(t, func() bool {
				if tc.wantSent > 0 {
					return len(e.hub.sentMessages()) >= tc.wantSent
				}
				return len(e.dispatched(t)) >= 1 && time.Since(start) > 500*time.Millisecond
			})
			sent := e.hub.sentMessages()
			if len(sent) != tc.wantSent {
				t.Fatalf("sent = %+v, want %d", sent, tc.wantSent)
			}
			if !tc.wantNotice {
				return
			}
			if sent[0].To != "@a" || sent[0].CausedBy != "m1" {
				t.Errorf("sent to=%q caused_by=%q, want @a / m1", sent[0].To, sent[0].CausedBy)
			}
			if !strings.HasPrefix(sent[0].Message, autoNoticePrefix) || !strings.Contains(sent[0].Message, cmd) {
				t.Errorf("sent message = %q, want an unfinished-command notice naming %q", sent[0].Message, cmd)
			}
		})
	}
}

// auto 返信への通知は送らない (echo ループを避ける)。
func TestHandleOne_UnfinishedCommandNoNoticeForAutoReply(t *testing.T) {
	e := newC1Env(t, time.Minute, "echo '"+cmdStartedEvent("item_1", "x")+"'",
		inboxJSON(t, msgFrom("m1", "@a", "(auto) bridge-claude2 error: body m1")))
	start := time.Now()
	e.run(t, func() bool { return len(e.dispatched(t)) >= 1 && time.Since(start) > 500*time.Millisecond })
	if sent := e.hub.sentMessages(); len(sent) != 0 {
		t.Errorf("sent = %+v, want no notice", sent)
	}
}
