// threadctx.go — 文脈の段 2: 同じ鎖の前の便を hub の get_thread で引いて prompt に添える
// (issue #340、epic #338 段 2)。bridge-claude2/cmd/bridge/threadctx.go と同じ内容に保つ
// (添える文字列は engine に依存しない)。
//
// AGENT_HUB_CONTEXT_STAGE=thread のときだけ動く。便ごとに:
//
//  1. caused_by が空なら hub を呼ばない (この便が鎖の root なので前の便は無い = 段 1 と同じ)
//  2. get_thread(便 ID, limit=500) で鎖を引く。server は root + 返信を古い順に返す
//  3. 鎖の中でこの便より前に並ぶ便を、直近 N 便 / 合計 M 文字の上限で新しい側から選び、
//     古い順に並べて prompt に添える
//
// 失敗の扱い (#340 着手 GO の条件 1、最優先): get_thread が error を返したら (server の
// {"error":...} テキストも SDK の GetThread が error にする)、文脈を添えずにその便を処理する
// (便は届く = 段 1 と同じ)。添えたか添えなかったかは便ごとに固定キーの 1 行 (msg "thread
// context") をログに出し、後から outcome で数えられるようにする:
//
//	outcome=attached  添えた (attached = 添えた便数)
//	outcome=no_chain  caused_by が空 / 鎖にこの便より前の便が無い。添える物が無い (失敗ではない)
//	outcome=skipped   添えなかった。reason に理由 (get_thread の error・鎖が長すぎて直近を取れない)
//
// 制限: caused_by が null の便には効かない (#340 の「制限」節)。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

const (
	// threadMaxMessagesEnv は添える便数の上限を変える env 名 (bridge-codex2 と同じ)。
	threadMaxMessagesEnv = "AGENT_HUB_THREAD_MAX_MESSAGES"
	// threadMaxCharsEnv は添える本文の合計文字数 (rune 数) の上限を変える env 名 (bridge-codex2 と同じ)。
	threadMaxCharsEnv = "AGENT_HUB_THREAD_MAX_CHARS"

	defaultThreadMaxMessages = 10
	defaultThreadMaxChars    = 8000

	// threadFetchLimit は get_thread に渡す limit (root を除く返信の件数)。server の上限 500。
	// server は古い順に limit 件を返すので、上限は bridge 側で新しい側から掛ける。
	threadFetchLimit = 500
	// threadFetchTimeout は get_thread 1 回の待ち時間。超えたら添えずに処理する。
	threadFetchTimeout = 10 * time.Second
)

// threadContextConfig は段 2 で添える量の上限。
type threadContextConfig struct {
	MaxMessages int
	MaxChars    int
}

// resolveThreadContextConfig は上限の env を解決する。未指定・空なら既定値。
// 1 未満や数値でない値は起動エラーにする (黙って既定値にしない)。
func resolveThreadContextConfig() (threadContextConfig, error) {
	maxMsgs, err := positiveIntEnv(threadMaxMessagesEnv, defaultThreadMaxMessages)
	if err != nil {
		return threadContextConfig{}, err
	}
	maxChars, err := positiveIntEnv(threadMaxCharsEnv, defaultThreadMaxChars)
	if err != nil {
		return threadContextConfig{}, err
	}
	return threadContextConfig{MaxMessages: maxMsgs, MaxChars: maxChars}, nil
}

func positiveIntEnv(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s %q: must be a positive integer", name, v)
	}
	return n, nil
}

// logThreadContextConfig は段 2 の上限を起動ログに 1 行出す。
func logThreadContextConfig(cfg threadContextConfig) {
	slog.Info("thread context config",
		"max_messages", cfg.MaxMessages, "max_chars", cfg.MaxChars,
		"env_max_messages", threadMaxMessagesEnv, "env_max_chars", threadMaxCharsEnv)
}

// threadFetcher は get_thread を呼ぶ口 (*agenthub.Client が満たす。テストで差し替える)。
type threadFetcher interface {
	GetThread(ctx context.Context, messageID string, limit int) (*agenthub.Thread, error)
}

// threadContextOutcome は「添えた / 添えなかった」の区分。ログの outcome キーの値。
type threadContextOutcome string

const (
	threadOutcomeAttached threadContextOutcome = "attached"
	threadOutcomeNoChain  threadContextOutcome = "no_chain"
	threadOutcomeSkipped  threadContextOutcome = "skipped"
)

// threadContextResult は buildThreadContext の結果 (ログ用の値を含む)。
type threadContextResult struct {
	Text      string // prompt に添える文字列。添えないときは ""
	Outcome   threadContextOutcome
	Reason    string // skipped / no_chain の理由
	Attached  int    // 添えた便数
	Earlier   int    // 鎖の中でこの便より前の便の数 (取得できた範囲)
	Truncated bool   // 文字数上限で一番古い添えた便の本文を切った
}

// fetchThreadContext は段 2 の文脈を引き、結果を固定キーで 1 行ログに出して返す。
// 失敗しても error は返さない (呼び出し側は Text が空なら段 1 と同じ prompt を組む)。
func fetchThreadContext(ctx context.Context, f threadFetcher, msg agenthub.Message, cfg threadContextConfig) threadContextResult {
	res := buildThreadContext(ctx, f, msg, cfg)
	logThreadContext(msg, res)
	return res
}

func logThreadContext(msg agenthub.Message, res threadContextResult) {
	attrs := []any{
		"msg_id", msg.ID, "caused_by", msg.CausedBy,
		"outcome", string(res.Outcome), "attached", res.Attached, "earlier", res.Earlier,
	}
	if res.Reason != "" {
		attrs = append(attrs, "reason", res.Reason)
	}
	if res.Truncated {
		attrs = append(attrs, "truncated", true)
	}
	if res.Outcome == threadOutcomeSkipped {
		slog.Warn("thread context", attrs...)
		return
	}
	slog.Info("thread context", attrs...)
}

func buildThreadContext(ctx context.Context, f threadFetcher, msg agenthub.Message, cfg threadContextConfig) threadContextResult {
	if msg.CausedBy == "" {
		return threadContextResult{Outcome: threadOutcomeNoChain, Reason: "caused_by is empty"}
	}

	fctx, cancel := context.WithTimeout(ctx, threadFetchTimeout)
	defer cancel()
	th, err := f.GetThread(fctx, msg.ID, threadFetchLimit)
	if err != nil {
		return threadContextResult{Outcome: threadOutcomeSkipped, Reason: truncate(err.Error(), 300)}
	}

	// server は root + 返信を古い順に返す。この便より前に並ぶ便が「前の便」。
	idx := -1
	for i, m := range th.Messages {
		if m.ID == msg.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		// 鎖が limit を超えて新しい側 (この便を含む) が切り落とされた。直近を取れないので
		// 古い側を添えることはしない (上限を超える鎖で直近だけを添える、の約束を守る)。
		return threadContextResult{
			Outcome: threadOutcomeSkipped,
			Reason:  fmt.Sprintf("message not in get_thread result (thread longer than %d replies?)", threadFetchLimit),
			Earlier: len(th.Messages),
		}
	}
	earlier := th.Messages[:idx]
	if len(earlier) == 0 {
		return threadContextResult{Outcome: threadOutcomeNoChain, Reason: "no earlier message in thread"}
	}

	// 新しい側から便数・文字数の上限で選ぶ。一番新しい便は文字数上限を超えても本文を切って載せる。
	start := len(earlier)
	budget := cfg.MaxChars
	truncated := false
	var lastBody string
	for i := len(earlier) - 1; i >= 0 && len(earlier)-i <= cfg.MaxMessages; i-- {
		n := len([]rune(earlier[i].Body))
		if n > budget {
			if start == len(earlier) {
				// 一番新しい便すら入らない: 本文の末尾側を budget 文字だけ残す
				lastBody = tailRunes(earlier[i].Body, budget)
				truncated = true
				start = i
			}
			break
		}
		budget -= n
		start = i
	}
	picked := earlier[start:]
	omitted := start

	var b strings.Builder
	b.WriteString("【同じ鎖の前の便 (文脈の段 2、issue #340)】\n")
	fmt.Fprintf(&b, "この便は caused_by でつながった鎖の中にあります。hub から引いた同じ鎖の前の便のうち直近 %d 便を古い順に載せます", len(picked))
	if omitted > 0 {
		fmt.Fprintf(&b, " (これより前の %d 便は上限のため省略)", omitted)
	}
	b.WriteString("。参考情報です。依頼の内容は上の「本文」を正としてください。\n")
	for i, m := range picked {
		body := m.Body
		if truncated && i == 0 {
			body = "(前略) " + lastBody
		}
		fmt.Fprintf(&b, "--- 前の便 %d/%d id=%s from %s to %s 送信時刻 %s ---\n%s\n",
			i+1, len(picked), m.ID, m.Sender, m.To, m.Timestamp, body)
	}
	b.WriteString("--- 前の便ここまで ---")

	return threadContextResult{
		Text:      b.String(),
		Outcome:   threadOutcomeAttached,
		Attached:  len(picked),
		Earlier:   len(earlier),
		Truncated: truncated,
	}
}

// tailRunes は s の末尾 n rune を返す。
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
