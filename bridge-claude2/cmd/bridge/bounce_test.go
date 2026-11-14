package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// agent-hub scheduler.py の bounce 文面そのまま (issue #392)。
const schedulerBounceText = "@scheduler は自由メッセージは受け付けません。コマンドは /help で確認してください。"

func TestIsSchedulerBounce(t *testing.T) {
	cases := []struct {
		name string
		msg  agenthub.Message
		want bool
	}{
		{"bounce from @scheduler", agenthub.Message{Sender: "@scheduler", Body: schedulerBounceText}, true},
		{"same text from another sender", agenthub.Message{Sender: "@a", Body: schedulerBounceText}, false},
		{"@scheduler fired /run_in message", agenthub.Message{Sender: "@scheduler", Body: "@reviewer PR #1 のレビューをお願いします。"}, false},
		{"@scheduler command reply", agenthub.Message{Sender: "@scheduler", Body: "pong"}, false},
		{"bounce text not at start", agenthub.Message{Sender: "@scheduler", Body: "fwd: " + schedulerBounceText}, false},
	}
	for _, c := range cases {
		if got := isSchedulerBounce(c.msg); got != c.want {
			t.Errorf("[%s] isSchedulerBounce = %v, want %v", c.name, got, c.want)
		}
	}
}

// bounce は claude を起動せずに既読にし、それ以外の @scheduler 便は従来どおり dispatch されること。
func TestProcessMessages_SchedulerBounceSkipped(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatched.log")
	script := writeRecordingFakeClaude(t, dir, logPath, okResultLine(), 0)
	client, cfg, runner, hub, journal := newTestEnv(t, script)

	msgs := []agenthub.Message{
		{ID: "b1", Sender: "@scheduler", To: "@limit-test", Body: schedulerBounceText, Timestamp: "2026-11-14T09:00:01.000Z"},
		{ID: "f1", Sender: "@scheduler", To: "@limit-test", Body: "fired reminder", Timestamp: "2026-11-14T09:00:02.000Z"},
	}
	processMessages(context.Background(), cfg, client, runner, nil, cursorPos{},
		&activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{}, msgs, nil, "test")

	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "自由メッセージ") {
		t.Errorf("bounce must not be dispatched to claude: %q", data)
	}
	if !strings.Contains(string(data), "fired reminder") {
		t.Errorf("non-bounce @scheduler message must be dispatched: %q", data)
	}
	var marked []string
	for _, c := range hub.callsNamed("mark_as_read") {
		id, _ := c.Args["message_id"].(string)
		marked = append(marked, id)
	}
	if !reflect.DeepEqual(marked, []string{"b1", "f1"}) {
		t.Errorf("mark_as_read ids = %v, want [b1 f1]", marked)
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("bridge send_message called %d times; want 0", n)
	}
}

// drain 中に届いた bounce も claude に渡さず既読にすること。
func TestRunGracefulDrain_SchedulerBounceSkipped(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatched.log")
	script := writeRecordingFakeClaude(t, dir, logPath, okResultLine(), 0)
	client, cfg, runner, hub, journal := newTestEnv(t, script)

	hub.setInbox(`[{"id":"b1","from":"@scheduler","to":"@limit-test","message":"` + schedulerBounceText +
		`","timestamp":"2026-11-14T09:00:01.000Z"}]`)

	runGracefulDrain(client, runner, cfg, cursorPos{}, &activityTracker{}, journal, "@limit-test", &limitSleeper{})

	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "自由メッセージ") {
		t.Errorf("bounce must not be dispatched to claude during drain: %q", data)
	}
	var marked []string
	for _, c := range hub.callsNamed("mark_as_read") {
		id, _ := c.Args["message_id"].(string)
		marked = append(marked, id)
	}
	if !reflect.DeepEqual(marked, []string{"b1"}) {
		t.Errorf("mark_as_read ids = %v, want [b1]", marked)
	}
}
