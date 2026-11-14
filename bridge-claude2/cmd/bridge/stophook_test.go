package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stopCallbackLine は CLI がターンを終える前に出す Stop の hook_callback (issue #393)。
func stopCallbackLine(requestID string, active bool) string {
	b, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request": map[string]any{
			"subtype":     "hook_callback",
			"callback_id": stopHookCallbackID,
			"input": map[string]any{
				"hook_event_name":  "Stop",
				"stop_hook_active": active,
			},
		},
	})
	return string(b)
}

// toolUseLine / toolResultLine は agent-hub の MCP ツール呼び出しとその結果。
func toolUseLine(id, name, input string) string {
	return `{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"` + id +
		`","name":"` + name + `","input":` + input + `}]}}`
}

func toolResultLine(id string, isError bool) string {
	e := "false"
	if isError {
		e = "true"
	}
	return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
		`","is_error":` + e + `}]}}`
}

// stopResponse は Stop の hook_callback への応答 1 行。
type stopResponse struct {
	Response struct {
		RequestID string `json:"request_id"`
		Subtype   string `json:"subtype"`
		Response  struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		} `json:"response"`
	} `json:"response"`
}

func runStopScenario(t *testing.T, ctx context.Context, lines ...string) []stopResponse {
	t.Helper()
	input := strings.Join(append(lines, resultLine), "\n") + "\n"
	var stdin bytes.Buffer
	if _, err := readUntilResult(ctx, newStreamScanner(strings.NewReader(input), 1<<20), &stdin, nil, false); err != nil {
		t.Fatalf("readUntilResult err = %v", err)
	}
	var out []stopResponse
	for _, l := range strings.Split(strings.TrimSpace(stdin.String()), "\n") {
		if l == "" {
			continue
		}
		var r stopResponse
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("stdin line is not JSON: %q: %v", l, err)
		}
		out = append(out, r)
	}
	return out
}

func decisions(rs []stopResponse) []string {
	var d []string
	for _, r := range rs {
		if r.Response.Subtype != "success" {
			d = append(d, "error")
			continue
		}
		if r.Response.Response.Decision == "" {
			d = append(d, "allow")
		} else {
			d = append(d, r.Response.Response.Decision)
		}
	}
	return d
}

func TestStopHook_Decisions(t *testing.T) {
	const msgID = "m-393"
	inflight := withReplyCheck(context.Background(), msgID, "@planner")
	cases := []struct {
		name  string
		ctx   context.Context
		lines []string
		want  string
	}{
		{
			name:  "no reply: block once, then allow on stop_hook_active",
			ctx:   inflight,
			lines: []string{stopCallbackLine("s1", false), stopCallbackLine("s2", true)},
			want:  "block,allow",
		},
		{
			name:  "no reply: block only once even without stop_hook_active",
			ctx:   inflight,
			lines: []string{stopCallbackLine("s1", false), stopCallbackLine("s2", false)},
			want:  "block,allow",
		},
		{
			name: "replied with caused_by: allow",
			ctx:  inflight,
			lines: []string{
				toolUseLine("t1", sendMessageToolName, `{"to":"@planner","body":"hi","caused_by":"`+msgID+`"}`),
				toolResultLine("t1", false),
				stopCallbackLine("s1", false),
			},
			want: "allow",
		},
		{
			name: "marked as read: allow",
			ctx:  inflight,
			lines: []string{
				toolUseLine("t1", markAsReadToolName, `{"message_id":"`+msgID+`"}`),
				toolResultLine("t1", false),
				stopCallbackLine("s1", false),
			},
			want: "allow",
		},
		{
			name: "replied to another message: block",
			ctx:  inflight,
			lines: []string{
				toolUseLine("t1", sendMessageToolName, `{"to":"@x","body":"hi","caused_by":"other"}`),
				toolResultLine("t1", false),
				stopCallbackLine("s1", false),
			},
			want: "block",
		},
		{
			name: "send_message failed: block",
			ctx:  inflight,
			lines: []string{
				toolUseLine("t1", sendMessageToolName, `{"to":"@planner","body":"hi","caused_by":"`+msgID+`"}`),
				toolResultLine("t1", true),
				stopCallbackLine("s1", false),
			},
			want: "block",
		},
		{
			name: "replied after the block: allow",
			ctx:  inflight,
			lines: []string{
				stopCallbackLine("s1", false),
				toolUseLine("t1", sendMessageToolName, `{"to":"@planner","body":"hi","caused_by":"`+msgID+`"}`),
				toolResultLine("t1", false),
				stopCallbackLine("s2", true),
			},
			want: "block,allow",
		},
		{
			name:  "no in-flight message (compact / tests): allow",
			ctx:   context.Background(),
			lines: []string{stopCallbackLine("s1", false)},
			want:  "allow",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(decisions(runStopScenario(t, tc.ctx, tc.lines...)), ",")
			if got != tc.want {
				t.Fatalf("decisions = %s, want %s", got, tc.want)
			}
		})
	}
}

// 差し戻しの理由文に、依頼 ID・依頼主・send_message と mark_as_read の両方の案内が入ること。
func TestStopHook_BlockReason(t *testing.T) {
	ctx := withReplyCheck(context.Background(), "m-393", "@planner")
	rs := runStopScenario(t, ctx, stopCallbackLine("s1", false))
	if len(rs) != 1 || rs[0].Response.RequestID != "s1" {
		t.Fatalf("responses = %+v", rs)
	}
	reason := rs[0].Response.Response.Reason
	for _, want := range []string{"m-393", "@planner", "send_message", `caused_by="m-393"`, "mark_as_read", `message_id="m-393"`, "NOT delivered", "will not reply for you"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason does not contain %q: %s", want, reason)
		}
	}
}

func TestWithReplyCheck_EmptyIDIsNoop(t *testing.T) {
	if rc := newReplyCheck(withReplyCheck(context.Background(), "", "@x")); rc.enabled {
		t.Fatal("empty msg ID should not enable reply check")
	}
}
