// autoreply.go — auto エラー返信の送信元別 cooldown (issue #267)
//
// 背景: claude 起動失敗時の `(auto) bridge-claude2 error: …` は送信元へ無条件に返していた。
// 送信元が「受け取ったら必ず何か返す存在」(@scheduler の自由文 bounce / 同じく失敗中の
// bridge / 自動応答する peer) だと、bridge が毎回 auto 返信を返す限りピンポンが止まらない。
// limit 起因の失敗は limit.go (休眠 + auto 返信なし) で止まるが、それ以外の失敗
// (MCP config 破損 / claude CLI 不在 / 恒常的な crash 等) でも同じ構造で往復する。
//
// 対処: auto エラー返信を **送信元ごとに cooldown 内 1 回** に制限する。往復は
// 「bridge が返信する」ことで初めて次の周回が始まるので、2 回目以降を抑止すれば
// 相手の挙動に依存せず必ず 1 往復で止まる。通常 peer / 人間宛の 1 回目の auto 返信は
// 従来どおり送る (挙動不変)。
//
// caused_by チェーンでの抑止 (issue #267 対処 1) は採用しない: SDK の SendMessage が
// 送信した message ID を返さないため、bridge 側で「自分の auto 返信への返信」を
// 特定できない (SDK + server の変更が必要)。cooldown は相手が caused_by を伝播するか
// どうかに依存しないため、単独で構造的に止まる。
package main

import (
	"sync"
	"time"
)

// autoReplyCooldown は同一送信元へ auto エラー返信を送る最小間隔。
// 観測されたピンポン周期 (30 秒 / 3 秒 / 1 秒) より十分長く、かつ人間が同じ bridge に
// 再度話しかけて失敗した場合に「何も返ってこない」時間が長すぎない値。
const autoReplyCooldown = 10 * time.Minute

// autoReplyLimiter は送信元ごとの直近 auto エラー返信時刻を保持する。
// claudeRunner に 1 つ持たせ、reconnect をまたいで共有する (往復は reconnect でも
// 続くため、session 単位でリセットしてはいけない)。nil receiver は常に許可する
// (テスト・後方互換)。
type autoReplyLimiter struct {
	mu       sync.Mutex
	cooldown time.Duration
	last     map[string]time.Time
}

func newAutoReplyLimiter(cooldown time.Duration) *autoReplyLimiter {
	return &autoReplyLimiter{cooldown: cooldown, last: map[string]time.Time{}}
}

// allow は sender へ auto エラー返信を送ってよいかを返す。許可した場合は送信時刻を
// 記録する。拒否した場合は次に許可されるまでの残り時間を返す。
// cooldown を過ぎた古い entry は呼び出しのたびに掃除する (送信元数で無制限に
// 増えないようにする)。
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
