package main

// @scheduler の自由文 bounce を claude に渡さずに既読にする (issue #392)。
//
// @scheduler は "/" で始まらない便を受け取ると、送信元に固定文の bounce を返す。
// claude がこの bounce に send_message で返すと、それがまた自由文として bounce され、
// 往復が止まらなくなる。#267 の cooldown (autoreply.go) は bridge 自身の auto 返信にしか
// 効かないため、bounce は bridge が claude を起動する前に見分けて既読にする。
//
// 文面の出どころ: agent-hub の packages/scheduler/scheduler.py の bounce
// (send_dm(..., "@scheduler は自由メッセージは受け付けません。コマンドは /help で確認してください。", caused_by=msg_id))。
// 判定は送信元と文面の前方一致だけに依存する。scheduler 側の文面が変わると、この判定は
// エラーを出さずに効かなくなる (README の「@scheduler bounce」節も参照)。

import (
	"strings"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

const (
	schedulerHandle = "@scheduler"
	// schedulerBouncePrefix は agent-hub scheduler.py の bounce 文面と一致させること。
	schedulerBouncePrefix = "@scheduler は自由メッセージは受け付けません。"
)

// isSchedulerBounce は msg が @scheduler の自由文 bounce かどうかを返す。
func isSchedulerBounce(msg agenthub.Message) bool {
	return msg.Sender == schedulerHandle && strings.HasPrefix(msg.Body, schedulerBouncePrefix)
}
