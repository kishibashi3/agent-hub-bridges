// hook.go — blocking command 阻止の PreToolUse フック (issue #345)
//
// claude CLI の control protocol (Python SDK: query.py と同じ形) を使う。
// initialize の control_request で PreToolUse (matcher Bash) のフックを登録すると、CLI は Bash を
// 実行する前に hook_callback の control_request を stdout に出し、stdin への control_response を
// 待つ。ブロック対象なら permissionDecision: deny を返し、コマンドを起動させない。
// Monitor (issue #388) と ScheduleWakeup / CronCreate (issue #389) は常に deny する。
// どれもターンが終わった後に harness が起こす前提のツールで、依頼ごとに終わる bridge では予約ごと消える。
//
// CLI は応答が来るまでツールの実行を止めたまま待つため、control_request には必ず何かを返す
// (未知の callback_id は許可、未対応の subtype は error)。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
)

// blockingHookCallbackID は initialize で登録する PreToolUse フックの callback ID。
const blockingHookCallbackID = "hook_blocking_bash"

// monitorHookCallbackID は Monitor ツールを deny する PreToolUse フックの callback ID (issue #388)。
const monitorHookCallbackID = "hook_deny_monitor"

// scheduleWakeupHookCallbackID / cronCreateHookCallbackID は ScheduleWakeup / CronCreate を
// deny する PreToolUse フックの callback ID (issue #389)。
const (
	scheduleWakeupHookCallbackID = "hook_deny_schedule_wakeup"
	cronCreateHookCallbackID     = "hook_deny_cron_create"
)

// deniedToolMessages は中身によらず deny するツールの callback ID と理由文。
var deniedToolMessages = map[string]string{
	monitorHookCallbackID:        monitorDeniedMessage,
	scheduleWakeupHookCallbackID: scheduleWakeupDeniedMessage,
	cronCreateHookCallbackID:     cronCreateDeniedMessage,
}

// initializeRequest は initialize の control_request の request 部分を組み立てる。
// withHooks=true のとき blocking command 阻止の PreToolUse フックを登録する。
func initializeRequest(withHooks bool) map[string]any {
	req := map[string]any{"subtype": "initialize"}
	if withHooks {
		req["hooks"] = map[string]any{
			"PreToolUse": []map[string]any{
				{"matcher": "Bash", "hookCallbackIds": []string{blockingHookCallbackID}},
				{"matcher": "Monitor", "hookCallbackIds": []string{monitorHookCallbackID}},
				{"matcher": "ScheduleWakeup", "hookCallbackIds": []string{scheduleWakeupHookCallbackID}},
				{"matcher": "CronCreate", "hookCallbackIds": []string{cronCreateHookCallbackID}},
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
			// RunInBackground は型を決めずに受ける (issue #379)。bool で受けると "true" などの
			// 想定外の型で control_request 全体の parse が失敗し、pattern の deny まで効かなくなる。
			RunInBackground json.RawMessage `json:"run_in_background"`
		} `json:"tool_input"`
	} `json:"input"`
}

// runInBackground は run_in_background を寛容に解釈する (issue #379)。
// bool と、strconv.ParseBool で読める文字列 ("true" 等) を受け付ける。
// 省略・null・解釈できない値は false (解釈できない値は warn を残す)。
func runInBackground(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, perr := strconv.ParseBool(s); perr == nil {
			return v
		}
	}
	slog.Warn("runner: unexpected run_in_background value, treating as false", "value", truncate(string(raw), 120))
	return false
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
// run_in_background: true (issue #374) かブロック対象の Bash コマンドなら deny、
// Monitor / ScheduleWakeup / CronCreate は中身によらず deny (issue #388 / #389)。それ以外 (未知の callback_id を含む) は空 = 許可。
func blockingHookOutput(req controlRequest) map[string]any {
	if msg, ok := deniedToolMessages[req.CallbackID]; ok {
		slog.Warn("[blocking-cmd] session-bound tool detected, denying via PreToolUse hook",
			"callback_id", req.CallbackID,
			"tool_name", req.Input.ToolName,
			"command_preview", truncate(req.Input.ToolInput.Command, 120),
			"tool_use_id", req.Input.ToolUseID,
		)
		return denyHookOutput(msg)
	}
	if req.CallbackID != blockingHookCallbackID {
		slog.Warn("runner: unknown hook callback_id, allowing", "callback_id", req.CallbackID)
		return map[string]any{}
	}
	command := req.Input.ToolInput.Command
	if runInBackground(req.Input.ToolInput.RunInBackground) {
		slog.Warn("[blocking-cmd] run_in_background detected, denying via PreToolUse hook",
			"command_preview", truncate(command, 120),
			"tool_use_id", req.Input.ToolUseID,
		)
		return denyHookOutput(backgroundDeniedMessage)
	}
	pattern := checkBlockingCommand(command)
	if pattern == "" {
		return map[string]any{}
	}
	slog.Warn("[blocking-cmd] blocking command detected, denying via PreToolUse hook",
		"pattern", pattern,
		"command_preview", truncate(command, 120),
		"tool_use_id", req.Input.ToolUseID,
	)
	return denyHookOutput(buildBlockingErrorMessage(pattern))
}

// backgroundDeniedMessage は run_in_background: true を deny するときの理由 (issue #374)。
// bridge は依頼ごとに claude を終わらせるので、バックグラウンドのタスクは残らない。
const backgroundDeniedMessage = "Error: run_in_background is not available in this bridge.\n" +
	"This bridge ends the claude process after each request, so background tasks do not survive.\n\n" +
	"If you need to wait, use @scheduler instead:\n" +
	"  @scheduler /run_in 10m @<your-handle> <alternative-command>"

// monitorDeniedMessage は Monitor を deny するときの理由 (issue #388)。
// Monitor で待つとターンが先に終わり、result 後に bridge が claude を止めるので返信が届かない。
const monitorDeniedMessage = "Error: Monitor is not available in this bridge.\n" +
	"This bridge ends the claude process after each request, so monitors do not survive.\n\n" +
	"If you need to wait, use @scheduler instead:\n" +
	"  @scheduler /run_in 10m @<your-handle> <alternative-command>"

// scheduleWakeupDeniedMessage は ScheduleWakeup を deny するときの理由 (issue #389)。
// 予約した wakeup はセッションと一緒に消えるので、ターンを終えると返信が届かない。
const scheduleWakeupDeniedMessage = "Error: ScheduleWakeup is not available in this bridge.\n" +
	"This bridge ends the claude process after each request, so scheduled wakeups do not survive.\n\n" +
	"If you need to wait, use @scheduler instead:\n" +
	"  @scheduler /run_in 10m @<your-handle> <alternative-command>"

// cronCreateDeniedMessage は CronCreate を deny するときの理由 (issue #389)。
// CronCreate の予約もセッションと一緒に消える。
const cronCreateDeniedMessage = "Error: CronCreate is not available in this bridge.\n" +
	"This bridge ends the claude process after each request, so cron jobs do not survive.\n\n" +
	"If you need to wait or repeat, use @scheduler instead:\n" +
	"  @scheduler /run_in 10m @<your-handle> <alternative-command>\n" +
	"  @scheduler /add <name> <cron-5> @<your-handle> <command>"

// denyHookOutput は PreToolUse を deny する hook output を返す。
func denyHookOutput(reason string) map[string]any {
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": reason,
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
