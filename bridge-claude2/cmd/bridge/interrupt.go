// interrupt.go — 停止で中断した依頼への通知 (issue #337)
//
// 背景: 作業中に stop / restart すると、依頼は処理前に既読化済みなので再起動しても再配信されず、
// キャンセル起因の失敗にはエラーも返さない (#336 #2) ため、送り主への返信が 0 通になっていた。
// `agenthubctl stop` は SIGTERM の 5 秒後に SIGKILL するので、drain で作業を最後まで終わらせる
// こともできない。
//
// 対処 (約束 C1「1 依頼に 1 返信」・C4「停止・再起動で C1 を破らない」): 中断した依頼には
// 「停止で中断した」旨の通知を 1 通返す。
//   - キャンセル済みの ctx ではなく、キャンセルを切り離した新しい ctx (interruptNoticeTimeout)
//     で送る。SIGKILL までの 5 秒に収める
//   - journal を通す。送信に失敗したら次回起動時に replay され、その依頼への 1 通として届く
//   - 内側 claude セッションが返信済みの依頼 (handleOne 側で判定) と、auto 返信への auto 返信は送らない
//   - 作業を再実行はしない。途中まで進んだ副作用 (GitHub への投稿等) を二重に起こしうるため
package main

import (
	"context"
	"log/slog"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// autoNoticePrefix は中断通知の先頭文字列。受信側の echo 判定 (autoReplyCommonPrefix) に掛かる。
const autoNoticePrefix = "(auto) " + bridgeType + " notice:"

// interruptNoticeTimeout は中断通知の送信に使う時間の上限 (agenthubctl stop の SIGKILL まで 5 秒)。
const interruptNoticeTimeout = 3 * time.Second

// interruptNoticeBody は中断通知の本文。
const interruptNoticeBody = autoNoticePrefix + " bridge の停止 (再起動) で、この依頼の作業を途中で中断しました。" +
	"結果は返りません。必要なら依頼を送り直してください。"

// sendInterruptNotice は停止で中断した msg の送り主へ中断通知を 1 通送る。
func sendInterruptNotice(ctx context.Context, client *agenthub.Client, journal *Journal, msg agenthub.Message) {
	if isAutoErrorEcho(msg.Body) {
		slog.Warn("sendInterruptNotice: inbound is an auto reply — not sending interrupt notice (issue #337)",
			"msg_id", msg.ID, "from", msg.Sender)
		return
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interruptNoticeTimeout)
	defer cancel()
	if err := journalledSend(sendCtx, client, journal, msg.Sender, interruptNoticeBody, msg.ID); err != nil {
		slog.Warn("sendInterruptNotice: send failed — kept in journal for replay on next startup (issue #337)",
			"msg_id", msg.ID, "from", msg.Sender, "err", err)
		return
	}
	slog.Info("sendInterruptNotice: sent interrupt notice (issue #337)", "msg_id", msg.ID, "from", msg.Sender)
}

// logShutdownInFlight は停止が掛かった時点で処理中だった便の扱いを 1 行で残す (issue #342)。
// この便は cursor を進め、再処理しない。内側が返信を出し終えていた場合の二重返信を避けるため。
// 送り主への返信は handleOne が中断通知 (issue #337) または内側の返信で済ませている。
func logShutdownInFlight(logPrefix string, msg agenthub.Message, handleErr error) {
	slog.Info(logPrefix+": in-flight message at shutdown — cursor advanced, will not be reprocessed (issue #342)",
		"msg_id", msg.ID, "from", msg.Sender, "handle_err", handleErr)
}

// logShutdownLeftUnread は停止で手を付けずに残したバッチ残りの便を 1 行で残す (issue #342)。
// MarkAsRead も cursor 前進もしていないので、drain か次回起動の catchup で処理される。
func logShutdownLeftUnread(logPrefix string, rest []agenthub.Message) {
	ids := make([]string, len(rest))
	for i, m := range rest {
		ids[i] = m.ID
	}
	slog.Info(logPrefix+": shutdown — leaving remaining batch messages unread for drain / next startup (issue #342)",
		"count", len(rest), "msg_ids", ids)
}
