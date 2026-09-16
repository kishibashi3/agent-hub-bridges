package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAutoReplyLimiter: 送信元ごとに cooldown 内 1 回だけ許可し、cooldown 経過で再許可する。
func TestAutoReplyLimiter(t *testing.T) {
	l := newAutoReplyLimiter(10 * time.Minute)
	t0 := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

	if ok, _ := l.allow("@scheduler", t0); !ok {
		t.Fatal("first reply to @scheduler must be allowed")
	}
	ok, wait := l.allow("@scheduler", t0.Add(3*time.Second))
	if ok {
		t.Fatal("second reply within cooldown must be suppressed")
	}
	if wait <= 0 || wait > 10*time.Minute {
		t.Errorf("retry_after = %v, want (0, 10m]", wait)
	}
	// 別送信元は独立
	if ok, _ := l.allow("@human", t0.Add(3*time.Second)); !ok {
		t.Fatal("first reply to a different sender must be allowed")
	}
	// cooldown 経過で再許可 + 古い entry は掃除される
	if ok, _ := l.allow("@scheduler", t0.Add(11*time.Minute)); !ok {
		t.Fatal("reply after cooldown must be allowed again")
	}
	if _, stale := l.last["@human"]; stale {
		t.Error("expired entry for @human should have been pruned")
	}

	var nilLimiter *autoReplyLimiter
	if ok, _ := nilLimiter.allow("@x", t0); !ok {
		t.Fatal("nil limiter must allow")
	}
}

// TestHandleOne_BouncingSender_AutoReplyOnce (issue #267 回帰テスト):
// 「auto 返信を受け取ると必ず何か返す」fake sender (@scheduler の自由文 bounce を模倣) に
// 対して、claude が毎回一般エラーで失敗しても auto エラー返信は 1 回だけ送られる。
func TestHandleOne_BouncingSender_AutoReplyOnce(t *testing.T) {
	script := writeFakeClaude(t, t.TempDir(), genericErrorResultLine(), 1)
	client, cfg, runner, hub, journal := newTestEnv(t, script)
	runner.autoReply = newAutoReplyLimiter(autoReplyCooldown)

	const bounce = "@scheduler は自由メッセージは受け付けません。コマンドは /help で確認してください。"
	// 1 周目: scheduler の cron DM → claude 失敗 → auto 返信 (1 回目、許可)
	// 2〜4 周目: scheduler が auto 返信を bounce → claude 失敗 → auto 返信は抑止
	bodies := []string{"ntv-pr-watch: PR を確認してください", bounce, bounce, bounce}
	for i, body := range bodies {
		msg := inbound(body)
		msg.ID = "sched-" + string(rune('a'+i))
		msg.Sender = "@scheduler"
		if err := handleOne(context.Background(), client, runner, msg, cfg, &activityTracker{}, journal); err == nil {
			t.Fatalf("round %d: want error from generic failure", i+1)
		}
	}
	sends := hub.callsNamed("send_message")
	if len(sends) != 1 {
		t.Fatalf("send_message called %d times; want exactly 1 (bounce loop must stop after one auto-reply)", len(sends))
	}
	if to, _ := sends[0].Args["to"].(string); to != "@scheduler" {
		t.Errorf("auto reply to=%q, want @scheduler", to)
	}

	// 別の送信元 (人間 / 通常 peer) への 1 回目は従来どおり送られる
	human := inbound("hello")
	human.ID = "human-1"
	human.Sender = "@kishibashi3"
	_ = handleOne(context.Background(), client, runner, human, cfg, &activityTracker{}, journal)
	if n := len(hub.callsNamed("send_message")); n != 2 {
		t.Errorf("send_message total = %d; want 2 (first auto-reply to a different sender is unaffected)", n)
	}
}

// TestHandleOne_WorkdirGone_BouncingSender_AutoReplyOnce (issue #272 回帰テスト):
// workdir 不在の auto 返信も送信元別 cooldown に掛かり、bounce が連続しても 1 回だけ送られる。
func TestHandleOne_WorkdirGone_BouncingSender_AutoReplyOnce(t *testing.T) {
	script := writeFakeClaude(t, t.TempDir(), genericErrorResultLine(), 1)
	client, cfg, runner, hub, journal := newTestEnv(t, script)
	runner.autoReply = newAutoReplyLimiter(autoReplyCooldown)
	cfg.Workdir = filepath.Join(t.TempDir(), "removed")

	const bounce = "@scheduler は自由メッセージは受け付けません。コマンドは /help で確認してください。"
	bodies := []string{"ntv-pr-watch: PR を確認してください", bounce, bounce, bounce}
	for i, body := range bodies {
		msg := inbound(body)
		msg.ID = "sched-" + string(rune('a'+i))
		msg.Sender = "@scheduler"
		if err := handleOne(context.Background(), client, runner, msg, cfg, &activityTracker{}, journal); err != nil {
			t.Fatalf("round %d: workdir-gone path must return nil (caller marks as read), got %v", i+1, err)
		}
	}
	sends := hub.callsNamed("send_message")
	if len(sends) != 1 {
		t.Fatalf("send_message called %d times; want exactly 1 (bounce loop must stop after one auto-reply)", len(sends))
	}
	body, _ := sends[0].Args["message"].(string)
	if !strings.HasPrefix(body, autoErrorPrefix) || !strings.Contains(body, cfg.Workdir) {
		t.Errorf("auto reply = %q; want prefix %q and the workdir path", body, autoErrorPrefix)
	}

	// 別の送信元への 1 回目は従来どおり送られる
	human := inbound("hello")
	human.ID = "human-1"
	human.Sender = "@kishibashi3"
	_ = handleOne(context.Background(), client, runner, human, cfg, &activityTracker{}, journal)
	if n := len(hub.callsNamed("send_message")); n != 2 {
		t.Errorf("send_message total = %d; want 2 (first auto-reply to a different sender is unaffected)", n)
	}
}

// TestHandleOne_WorkdirGone_EchoNoAutoReply (issue #272): workdir 不在でも、inbound が
// auto 返信 (自分の echo / Python 版 bridge の `(自動応答)`) なら返さない。cooldown が
// 無効 (nil limiter) でも echo guard 単独で止まることを見る。
func TestHandleOne_WorkdirGone_EchoNoAutoReply(t *testing.T) {
	script := writeFakeClaude(t, t.TempDir(), genericErrorResultLine(), 1)
	client, cfg, runner, hub, journal := newTestEnv(t, script)
	runner.autoReply = nil
	cfg.Workdir = filepath.Join(t.TempDir(), "removed")

	echoes := []string{
		autoErrorPrefix + " bridge の workdir が存在しません: " + cfg.Workdir,
		"(自動応答) bridge の workdir が存在しません: /tmp/other",
		"(自動応答) gemini CLI engine でエラー: boom",
	}
	for i, body := range echoes {
		msg := inbound(body)
		msg.ID = "echo-" + string(rune('a'+i))
		_ = handleOne(context.Background(), client, runner, msg, cfg, &activityTracker{}, journal)
	}
	if n := len(hub.callsNamed("send_message")); n != 0 {
		t.Errorf("send_message called %d times; want 0 (no auto-reply to auto reply echo)", n)
	}
}
