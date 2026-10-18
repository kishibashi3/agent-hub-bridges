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

func hookCallbackLine(requestID, callbackID, command string) string {
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
				"tool_input":      map[string]any{"command": command, "run_in_background": true},
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
		hookCallbackLine("r1", blockingHookCallbackID, "touch MARK; sleep 618"),
		hookCallbackLine("r2", blockingHookCallbackID, "ls -la"),
		hookCallbackLine("r3", "hook_unknown", "sleep 618"),
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
		_, _ = io.WriteString(stdoutW, hookCallbackLine("req_x", blockingHookCallbackID, "gh run watch 123")+"\n")
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
	big := hookCallbackLine("r_big", blockingHookCallbackID, strings.Repeat("A", 10*maxLine))
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
