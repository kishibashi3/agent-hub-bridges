package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// controlResponseLine は stdin に書かれた control_response 1 行。
type controlResponseLine struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
		Response  struct {
			HookSpecificOutput *struct {
				HookEventName            string `json:"hookEventName"`
				PermissionDecision       string `json:"permissionDecision"`
				PermissionDecisionReason string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		} `json:"response"`
	} `json:"response"`
}

func parseControlResponses(t *testing.T, out string) []controlResponseLine {
	t.Helper()
	var lines []controlResponseLine
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var r controlResponseLine
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("stdin line is not JSON: %q: %v", l, err)
		}
		lines = append(lines, r)
	}
	return lines
}

// background は run_in_background にそのまま入れる (bool 以外の型も試すため any、nil なら省略)。
func hookCallbackLine(requestID, callbackID, command string, background any) string {
	toolInput := map[string]any{"command": command}
	if background != nil {
		toolInput["run_in_background"] = background
	}
	b, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request": map[string]any{
			"subtype":     "hook_callback",
			"callback_id": callbackID,
			"input": map[string]any{
				"hook_event_name": "PreToolUse",
				"tool_name":       "Bash",
				"tool_use_id":     "toolu_1",
				"tool_input":      toolInput,
			},
		},
	})
	return string(b)
}

const resultLine = `{"type":"result","subtype":"success","is_error":false,"result":"ok"}`

func TestInitializeRequest_RegistersPreToolUseHook(t *testing.T) {
	b, err := json.Marshal(initializeRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"hooks":{"PreToolUse":[{"hookCallbackIds":["hook_blocking_bash"],"matcher":"Bash"}]},"subtype":"initialize"}`
	if string(b) != want {
		t.Fatalf("initialize request = %s, want %s", b, want)
	}
	b, _ = json.Marshal(initializeRequest(false))
	if string(b) != `{"subtype":"initialize"}` {
		t.Fatalf("initialize request without hooks = %s", b)
	}
}

// どの control_request にも必ず 1 行の control_response を返すこと (issue #345)。
func TestReadUntilResult_RepliesToEveryControlRequest(t *testing.T) {
	input := strings.Join([]string{
		hookCallbackLine("r1", blockingHookCallbackID, "touch MARK; sleep 618", false),
		hookCallbackLine("r2", blockingHookCallbackID, "ls -la", false),
		hookCallbackLine("r3", "hook_unknown", "sleep 618", true),
		`{"type":"control_request","request_id":"r4","request":{"subtype":"can_use_tool"}}`,
		`{"type":"control_request","request_id":"r5","request":"not-an-object"}`,
		resultLine,
	}, "\n") + "\n"

	var stdin bytes.Buffer
	if _, err := readUntilResult(context.Background(), newStreamScanner(strings.NewReader(input), 1<<20), &stdin, nil, false); err != nil {
		t.Fatalf("readUntilResult err = %v", err)
	}
	got := parseControlResponses(t, stdin.String())
	if len(got) != 5 {
		t.Fatalf("got %d control_responses, want 5: %s", len(got), stdin.String())
	}
	for i, r := range got {
		if r.Type != "control_response" {
			t.Errorf("[%d] type = %q", i, r.Type)
		}
		if want := []string{"r1", "r2", "r3", "r4", "r5"}[i]; r.Response.RequestID != want {
			t.Errorf("[%d] request_id = %q, want %q", i, r.Response.RequestID, want)
		}
	}

	// r1: ブロック対象 → deny + 理由
	h := got[0].Response.Response.HookSpecificOutput
	if got[0].Response.Subtype != "success" || h == nil {
		t.Fatalf("r1 = %+v, want success with hookSpecificOutput", got[0].Response)
	}
	if h.HookEventName != "PreToolUse" || h.PermissionDecision != "deny" {
		t.Errorf("r1 hookSpecificOutput = %+v", h)
	}
	if h.PermissionDecisionReason != buildBlockingErrorMessage("sleep <N>=60s+") {
		t.Errorf("r1 reason = %q", h.PermissionDecisionReason)
	}
	// r2: ブロック対象外 / r3: 未知の callback_id → 空の success (許可)
	for _, i := range []int{1, 2} {
		if got[i].Response.Subtype != "success" || got[i].Response.Response.HookSpecificOutput != nil {
			t.Errorf("[%d] = %+v, want empty success", i, got[i].Response)
		}
	}
	// r4: 未対応の subtype / r5: 壊れた request → error
	for _, i := range []int{3, 4} {
		if got[i].Response.Subtype != "error" || got[i].Response.Error == "" {
			t.Errorf("[%d] = %+v, want error", i, got[i].Response)
		}
	}
}

// 以前の tool_result 注入 (CLI に捨てられていた) を stdin に書かないこと。
func TestReadUntilResult_NoToolResultInjectionOnBashToolUse(t *testing.T) {
	input := `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sleep 600"}}]}}` +
		"\n" + resultLine + "\n"
	var stdin bytes.Buffer
	if _, err := readUntilResult(context.Background(), newStreamScanner(strings.NewReader(input), 1<<20), &stdin, nil, false); err != nil {
		t.Fatalf("readUntilResult err = %v", err)
	}
	if stdin.Len() != 0 {
		t.Fatalf("stdin = %q, want nothing written", stdin.String())
	}
}

// 偽の CLI: hook_callback を出したら control_response が来るまで待ち、来たら result を出す。
// readUntilResult が応答を返さないとここで止まる。
func TestReadUntilResult_HookCallbackRoundTrip(t *testing.T) {
	stdoutR, stdoutW := io.Pipe()
	stdinR, stdinW := io.Pipe()
	defer stdoutW.Close()
	defer stdinR.Close()

	cliDone := make(chan string, 1)
	go func() {
		_, _ = io.WriteString(stdoutW, hookCallbackLine("req_x", blockingHookCallbackID, "gh run watch 123", false)+"\n")
		line, err := bufio.NewReader(stdinR).ReadString('\n')
		if err != nil {
			cliDone <- "read error: " + err.Error()
			return
		}
		cliDone <- line
		_, _ = io.WriteString(stdoutW, resultLine+"\n")
	}()

	errCh := make(chan error, 1)
	go func() {
		_, err := readUntilResult(context.Background(), newStreamScanner(stdoutR, 1<<20), stdinW, nil, false)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("readUntilResult err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readUntilResult did not finish: control_response was not sent")
	}
	got := parseControlResponses(t, <-cliDone)
	h := got[0].Response.Response.HookSpecificOutput
	if got[0].Response.RequestID != "req_x" || h == nil || h.PermissionDecision != "deny" ||
		!strings.Contains(h.PermissionDecisionReason, "gh run watch") {
		t.Fatalf("control_response = %+v", got[0])
	}
}

// scanner 上限を超えた control_request も、request_id を拾って error を返すこと。
func TestReadUntilResult_OversizeControlRequestGetsReply(t *testing.T) {
	const maxLine = 512
	big := hookCallbackLine("r_big", blockingHookCallbackID, strings.Repeat("A", 10*maxLine), false)
	input := big + "\n" + resultLine + "\n"
	var stdin bytes.Buffer
	if _, err := readUntilResult(context.Background(), newStreamScanner(strings.NewReader(input), maxLine), &stdin, nil, false); err != nil {
		t.Fatalf("readUntilResult err = %v", err)
	}
	got := parseControlResponses(t, stdin.String())
	if len(got) != 1 || got[0].Response.RequestID != "r_big" || got[0].Response.Subtype != "error" {
		t.Fatalf("control_responses = %+v", got)
	}
}

// run_in_background: true の Bash は、コマンドの中身によらず deny すること (issue #374)。
func TestBlockingHookOutput_DeniesRunInBackground(t *testing.T) {
	input := strings.Join([]string{
		hookCallbackLine("b1", blockingHookCallbackID, "touch MARK; sleep 3", true),
		hookCallbackLine("b2", blockingHookCallbackID, "tail -f /dev/null", true),
		hookCallbackLine("b3", blockingHookCallbackID, "touch MARK; sleep 3", false),
		resultLine,
	}, "\n") + "\n"
	var stdin bytes.Buffer
	if _, err := readUntilResult(context.Background(), newStreamScanner(strings.NewReader(input), 1<<20), &stdin, nil, false); err != nil {
		t.Fatalf("readUntilResult err = %v", err)
	}
	got := parseControlResponses(t, stdin.String())
	if len(got) != 3 {
		t.Fatalf("got %d control_responses, want 3: %s", len(got), stdin.String())
	}
	for _, i := range []int{0, 1} {
		h := got[i].Response.Response.HookSpecificOutput
		if got[i].Response.Subtype != "success" || h == nil || h.PermissionDecision != "deny" {
			t.Fatalf("[%d] = %+v, want deny", i, got[i].Response)
		}
		if h.PermissionDecisionReason != backgroundDeniedMessage {
			t.Errorf("[%d] reason = %q, want backgroundDeniedMessage", i, h.PermissionDecisionReason)
		}
	}
	// フォアグラウンドのブロック対象外コマンドは今までどおり許可
	if got[2].Response.Subtype != "success" || got[2].Response.Response.HookSpecificOutput != nil {
		t.Errorf("[2] = %+v, want empty success", got[2].Response)
	}
}

// run_in_background は bool でも文字列 "true" でも deny されること (issue #379)。
func TestBlockingHookOutput_DeniesRunInBackgroundString(t *testing.T) {
	input := strings.Join([]string{
		hookCallbackLine("s1", blockingHookCallbackID, "touch MARK; sleep 3", true),
		hookCallbackLine("s2", blockingHookCallbackID, "touch MARK; sleep 3", "true"),
		hookCallbackLine("s3", blockingHookCallbackID, "touch MARK; sleep 3", "false"),
		resultLine,
	}, "\n") + "\n"
	var stdin bytes.Buffer
	if _, err := readUntilResult(context.Background(), newStreamScanner(strings.NewReader(input), 1<<20), &stdin, nil, false); err != nil {
		t.Fatalf("readUntilResult err = %v", err)
	}
	got := parseControlResponses(t, stdin.String())
	if len(got) != 3 {
		t.Fatalf("got %d control_responses, want 3: %s", len(got), stdin.String())
	}
	for _, i := range []int{0, 1} {
		h := got[i].Response.Response.HookSpecificOutput
		if got[i].Response.Subtype != "success" || h == nil || h.PermissionDecision != "deny" ||
			h.PermissionDecisionReason != backgroundDeniedMessage {
			t.Fatalf("[%d] = %+v, want deny with backgroundDeniedMessage", i, got[i].Response)
		}
	}
	if got[2].Response.Subtype != "success" || got[2].Response.Response.HookSpecificOutput != nil {
		t.Errorf("[2] = %+v, want empty success", got[2].Response)
	}
}

// run_in_background が想定外の型でも control_request の parse は失敗せず、
// blocking command の pattern deny が効くこと (issue #379)。
func TestBlockingHookOutput_PatternDenyWithUnexpectedBackgroundType(t *testing.T) {
	cases := []struct {
		name       string
		background any
	}{
		{"number", 1},
		{"object", map[string]any{"x": 1}},
		{"array", []any{true}},
		{"unparsable string", "yes please"},
		{"null", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := hookCallbackLine("u1", blockingHookCallbackID, "gh run watch 123", tc.background)
			if tc.background == nil {
				// nil は helper では省略になるので、明示的な null を入れる
				line = strings.Replace(line, `"command":"gh run watch 123"`, `"command":"gh run watch 123","run_in_background":null`, 1)
			}
			var stdin bytes.Buffer
			if _, err := readUntilResult(context.Background(), newStreamScanner(strings.NewReader(line+"\n"+resultLine+"\n"), 1<<20), &stdin, nil, false); err != nil {
				t.Fatalf("readUntilResult err = %v", err)
			}
			got := parseControlResponses(t, stdin.String())
			if len(got) != 1 {
				t.Fatalf("got %d control_responses, want 1: %s", len(got), stdin.String())
			}
			h := got[0].Response.Response.HookSpecificOutput
			if got[0].Response.Subtype != "success" || h == nil || h.PermissionDecision != "deny" {
				t.Fatalf("response = %+v, want pattern deny", got[0].Response)
			}
			if h.PermissionDecisionReason == backgroundDeniedMessage {
				t.Errorf("reason = backgroundDeniedMessage, want pattern deny reason")
			}
		})
	}
}

func TestRunInBackground(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"null", false},
		{"true", true},
		{"false", false},
		{`"true"`, true},
		{`"TRUE"`, true},
		{`"false"`, false},
		{`"yes please"`, false},
		{"1", false},
		{`{"x":1}`, false},
	}
	for _, tc := range cases {
		if got := runInBackground(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("runInBackground(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
