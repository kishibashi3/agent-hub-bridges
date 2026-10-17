// autoreply.go — auto エラー返信の echo 抑止と送信元別 cooldown (issue #336 #3)
//
// 背景: codex 起動失敗時の `(auto) bridge-codex2 error: …` を送信元へ無条件に返すと、
// 送信元が「受け取ったら必ず何か返す存在」(@scheduler の bounce / 同じく失敗中の bridge /
// 自動応答する peer) のときに往復が止まらない (bridge-claude2 #267 と同じ構造)。
//
// 対処 (約束 C1「二重に返さない」):
//   - inbound 自体が auto 返信 (`(auto) ` / `(自動応答)` 始まり) なら返さない
//   - 同一送信元への auto 返信は cooldown 内 1 回まで。往復は bridge が返信することで
//     次の周回が始まるので、2 回目以降を抑止すれば相手が何を返しても 1 往復で止まる
//
// caused_by チェーンでの抑止は使えない: SDK の SendMessage が送信した message ID を
// 返さないため、「自分の auto 返信への返信」を bridge 側で特定できない。
package main

import (
	"strings"
	"sync"
	"time"
)

// bridgeType は auto 返信と GitHub footer に出す bridge 種別。
const bridgeType = "bridge-codex2"

// autoErrorPrefix は bridge が送信元へ返す auto 返信の先頭文字列 (issue #275 で揃えた形)。
const autoErrorPrefix = "(auto) " + bridgeType + " error:"

// autoReplyCommonPrefix は fleet 内の bridge が auto 返信の先頭に付ける共通文字列 (issue #275)。
// 受信判定は他 bridge の auto 返信も拾うため、この共通部分の前方一致で行う。
const autoReplyCommonPrefix = "(auto) "

// legacyAutoReplyPrefix は Python 版 bridge が今も使う auto 返信の先頭文字列。
const legacyAutoReplyPrefix = "(自動応答)"

// autoReplyCooldown は同一送信元へ auto エラー返信を送る最小間隔。
const autoReplyCooldown = 10 * time.Minute

// isAutoErrorEcho は inbound message が他 bridge (または自分) の auto 返信かどうかを返す。
func isAutoErrorEcho(body string) bool {
	return strings.HasPrefix(body, autoReplyCommonPrefix) || strings.HasPrefix(body, legacyAutoReplyPrefix)
}

// autoReplyLimiter は送信元ごとの直近 auto エラー返信時刻を保持する。
// codexRunner に 1 つ持たせ、reconnect をまたいで共有する (往復は reconnect でも続くため)。
// nil receiver は常に許可する。
type autoReplyLimiter struct {
	mu       sync.Mutex
	cooldown time.Duration
	last     map[string]time.Time
}

func newAutoReplyLimiter(cooldown time.Duration) *autoReplyLimiter {
	return &autoReplyLimiter{cooldown: cooldown, last: map[string]time.Time{}}
}

// allow は sender へ auto エラー返信を送ってよいかを返す。許可した場合は送信時刻を記録する。
// 拒否した場合は次に許可されるまでの残り時間を返す。cooldown を過ぎた entry は掃除する。
func (l *autoReplyLimiter) allow(sender string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for s, t := range l.last {
		if now.Sub(t) >= l.cooldown {
			delete(l.last, s)
		}
	}
	if t, ok := l.last[sender]; ok {
		return false, l.cooldown - now.Sub(t)
	}
	l.last[sender] = now
	return true, 0
}
