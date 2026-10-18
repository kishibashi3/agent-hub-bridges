// hook.go — blocking command 阻止の PreToolUse フック (issue #345)
//
// claude CLI の control protocol (Python SDK: query.py と同じ形) を使う。
// initialize の control_request で PreToolUse (matcher Bash) のフックを登録すると、CLI は Bash を
// 実行する前に hook_callback の control_request を stdout に出し、stdin への control_response を
// 待つ。ブロック対象なら permissionDecision: deny を返し、コマンドを起動させない。
//
// CLI は応答が来るまでツールの実行を止めたまま待つため、control_request には必ず何かを返す
// (未知の callback_id は許可、未対応の subtype は error)。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// blockingHookCallbackID は initialize で登録する PreToolUse フックの callback ID。
const blockingHookCallbackID = "hook_blocking_bash"

// initializeRequest は initialize の control_request の request 部分を組み立てる。
// withHooks=true のとき blocking command 阻止の PreToolUse フックを登録する。
func initializeRequest(withHooks bool) map[string]any {
	req := map[string]any{"subtype": "initialize"}
	if withHooks {
		req["hooks"] = map[string]any{
			"PreToolUse": []map[string]any{
				{"matcher": "Bash", "hookCallbackIds": []string{blockingHookCallbackID}},
			},
		}
	}
	return req
}

// controlRequest は CLI から届く control_request の request 部分。
type controlRequest struct {
	Subtype    string `json:"subtype"`
	CallbackID string `json:"callback_id"`
	Input      struct {
		ToolName  string `json:"tool_name"`
		ToolUseID string `json:"tool_use_id"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	} `json:"input"`
}

// handleControlRequest は CLI からの control_request に control_response を返す。
// どの分岐でも必ず応答を書く (応答しないと CLI がツール実行を止めたまま待ち続ける)。
func handleControlRequest(w io.Writer, requestID string, raw json.RawMessage) {
	var req controlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		slog.Warn("runner: control_request parse failed, replying error", "request_id", requestID, "err", err)
		writeControlResponse(w, requestID, nil, fmt.Errorf("bridge: parse control_request: %w", err))
		return
	}
	if req.Subtype != "hook_callback" {
		slog.Warn("runner: unsupported control_request subtype, replying error",
			"request_id", requestID, "subtype", req.Subtype)
		writeControlResponse(w, requestID, nil, fmt.Errorf("bridge: unsupported control_request subtype %q", req.Subtype))
		return
	}
	writeControlResponse(w, requestID, blockingHookOutput(req), nil)
}

// blockingHookOutput は hook_callback への応答 (hook output) を返す。
// ブロック対象の Bash コマンドなら deny、それ以外 (未知の callback_id を含む) は空 = 許可。
func blockingHookOutput(req controlRequest) map[string]any {
	if req.CallbackID != blockingHookCallbackID {
		slog.Warn("runner: unknown hook callback_id, allowing", "callback_id", req.CallbackID)
		return map[string]any{}
	}
	command := req.Input.ToolInput.Command
	pattern := checkBlockingCommand(command)
	if pattern == "" {
		return map[string]any{}
	}
	slog.Warn("[blocking-cmd] blocking command detected, denying via PreToolUse hook",
		"pattern", pattern,
		"command_preview", truncate(command, 120),
		"tool_use_id", req.Input.ToolUseID,
	)
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": buildBlockingErrorMessage(pattern),
		},
	}
}

// writeControlResponse は control_response を stdin に書く。err != nil なら error 応答にする。
func writeControlResponse(w io.Writer, requestID string, response map[string]any, err error) {
	body := map[string]any{"request_id": requestID}
	if err != nil {
		body["subtype"] = "error"
		body["error"] = err.Error()
	} else {
		body["subtype"] = "success"
		body["response"] = response
	}
	if werr := writeJSON(w, map[string]any{"type": "control_response", "response": body}); werr != nil {
		slog.Warn("runner: failed to write control_response", "request_id", requestID, "err", werr)
	}
}
