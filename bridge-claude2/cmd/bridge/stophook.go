// stophook.go — 依頼主に返さずにターンを終えるのを 1 回だけ差し戻す Stop フック (issue #393)
//
// bridge は claude の result の text を依頼主に送らない。返信が届くのは claude が send_message を
// 呼んだときだけなので、text だけでターンを終えると依頼主には何も届かず、依頼は黙って消える。
//
// initialize で Stop フックを登録すると、CLI はターンを終える前に hook_callback (hook_event_name
// "Stop") を出して応答を待つ。処理中の依頼に返信 (send_message, caused_by = 依頼 ID) も既読化
// (mark_as_read) もしていなければ decision: block と理由を返し、claude にターンを続けさせる。
// 返信の要否と文面は claude に任せる (返信不要なら mark_as_read で既読にして終える)。
//
// 差し戻すのは 1 依頼につき 1 回だけ。2 回目 (stop_hook_active=true を含む) は許可して終わらせ、
// WARN を残す。処理中の依頼が ctx に無いとき (compact・テスト) は何もしない。
package main

import (
	"context"
	"fmt"
	"log/slog"
)

// stopHookCallbackID は initialize で登録する Stop フックの callback ID (issue #393)。
const stopHookCallbackID = "hook_stop_reply_check"

// replyCheckKey は処理中の依頼を ctx に載せるキー (issue #393)。
type replyCheckKey struct{}

// replyCheckTarget は Stop フックで返信の有無を確かめる依頼。
type replyCheckTarget struct {
	MsgID  string
	Sender string
}

// withReplyCheck は処理中の依頼を ctx に載せる。readUntilResult が Stop フックの判定に使う。
func withReplyCheck(ctx context.Context, msgID, sender string) context.Context {
	if msgID == "" {
		return ctx
	}
	return context.WithValue(ctx, replyCheckKey{}, replyCheckTarget{MsgID: msgID, Sender: sender})
}

// replyCheck は 1 回の query の間、Stop フックの状態を持つ。
type replyCheck struct {
	target  replyCheckTarget
	enabled bool
	blocked bool // 既に 1 回差し戻したか
}

// newReplyCheck は ctx から処理中の依頼を取り出す。無ければ判定しない (常に許可)。
func newReplyCheck(ctx context.Context) *replyCheck {
	t, ok := ctx.Value(replyCheckKey{}).(replyCheckTarget)
	return &replyCheck{target: t, enabled: ok && t.MsgID != ""}
}

// stopHookOutput は Stop の hook_callback への応答を返す。
// handled は tool_result で成功を確かめた send_message の caused_by と mark_as_read の
// message_id 群 (queryUsage.InnerHandledIDs)。stopHookActive は CLI が付ける
// stop_hook_active (Stop フックの差し戻しで続いているターンなら true)。
func (rc *replyCheck) stopHookOutput(handled []string, stopHookActive bool) map[string]any {
	if rc == nil || !rc.enabled {
		return map[string]any{}
	}
	for _, id := range handled {
		if id == rc.target.MsgID {
			return map[string]any{}
		}
	}
	if rc.blocked || stopHookActive {
		slog.Warn("[reply-check] turn ending without reply or mark_as_read after one block — allowing (issue #393)",
			"msg_id", rc.target.MsgID, "from", rc.target.Sender, "stop_hook_active", stopHookActive)
		return map[string]any{}
	}
	rc.blocked = true
	slog.Warn("[reply-check] turn ending without reply or mark_as_read — blocking once via Stop hook (issue #393)",
		"msg_id", rc.target.MsgID, "from", rc.target.Sender)
	return map[string]any{
		"decision": "block",
		"reason":   stopBlockReason(rc.target),
	}
}

// stopBlockReason は差し戻すときに claude に渡す理由文。
func stopBlockReason(t replyCheckTarget) string {
	return fmt.Sprintf("You have not replied to message %s from %s yet.\n"+
		"Text you write at the end of a turn is NOT delivered to the requester; only send_message is.\n"+
		"This process ends with this turn: background agents, scheduled routines and anything else left running "+
		"will not reply for you. If the task is not done (or cannot be done here), say so in the reply now.\n"+
		"- If a reply is needed: call send_message to %s with caused_by=%q, then end the turn.\n"+
		"- Only if you already replied without caused_by, or no reply is needed at all (e.g. a bounce or a plain notice): "+
		"call mark_as_read with message_id=%q, then end the turn. Do not send a duplicate reply.",
		t.MsgID, t.Sender, t.Sender, t.MsgID, t.MsgID)
}
