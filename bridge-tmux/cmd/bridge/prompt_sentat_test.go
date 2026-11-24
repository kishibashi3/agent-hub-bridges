package main

import (
	"strings"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// issue #405: prompt に送信時刻 (server の timestamp そのまま) と経過時間を入れる。

func TestFormatSentAtLine(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ts   string
		want string
	}{
		{"empty は行ごと省く", "", ""},
		{"30 秒前は 1 分以内", "2026-11-24T11:59:30.000Z", "送信時刻: 2026-11-24T11:59:30.000Z (受信処理の 1 分以内)\n"},
		{"ちょうど 1 分", "2026-11-24T11:59:00Z", "送信時刻: 2026-11-24T11:59:00Z (受信処理の 1 分前)\n"},
		{"59 分 59 秒は切り捨てで 59 分", "2026-11-24T11:00:01Z", "送信時刻: 2026-11-24T11:00:01Z (受信処理の 59 分前)\n"},
		{"ちょうど 1 時間", "2026-11-24T11:00:00Z", "送信時刻: 2026-11-24T11:00:00Z (受信処理の 1 時間前)\n"},
		{"23 時間 59 分は 23 時間", "2026-11-23T12:01:00Z", "送信時刻: 2026-11-23T12:01:00Z (受信処理の 23 時間前)\n"},
		{"ちょうど 1 日", "2026-11-23T12:00:00Z", "送信時刻: 2026-11-23T12:00:00Z (受信処理の 1 日前)\n"},
		{"2 か月前 (agent-hub#542 の @* )", "2026-09-19T21:33:18.123Z", "送信時刻: 2026-09-19T21:33:18.123Z (受信処理の 65 日前)\n"},
		{"30 秒先 (時計のずれ) は 1 分以内", "2026-11-24T12:00:30Z", "送信時刻: 2026-11-24T12:00:30Z (受信処理の 1 分以内)\n"},
		{"1 時間先は経過時間を付けない", "2026-11-24T13:00:00Z", "送信時刻: 2026-11-24T13:00:00Z\n"},
		{"RFC 3339 でなければそのまま載せる", "2026-11-24 11:00:00", "送信時刻: 2026-11-24 11:00:00\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatSentAtLine(tc.ts, now); got != tc.want {
				t.Errorf("formatSentAtLine(%q) = %q, want %q", tc.ts, got, tc.want)
			}
		})
	}
}

func TestFormatPrompt_SentAt(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	msg := agenthub.Message{
		ID:        "msg-1",
		Sender:    "@alice",
		To:        "@*",
		Body:      "hello",
		Timestamp: "2026-11-21T09:00:00.000Z",
	}
	got := formatPrompt("@me", msg, now)
	want := "宛先: @*\n送信時刻: 2026-11-21T09:00:00.000Z (受信処理の 3 日前)\n本文:\nhello\n"
	if !strings.Contains(got, want) {
		t.Errorf("prompt に送信時刻の行が宛先と本文の間に入っていない\nwant 部分: %q\nfull:\n%s", want, got)
	}

	msg.Timestamp = ""
	got = formatPrompt("@me", msg, now)
	if strings.Contains(got, "送信時刻") {
		t.Errorf("timestamp が空なのに送信時刻の行がある:\n%s", got)
	}
	if !strings.Contains(got, "宛先: @*\n本文:\nhello\n") {
		t.Errorf("timestamp が空のとき宛先の次が本文になっていない:\n%s", got)
	}
}
