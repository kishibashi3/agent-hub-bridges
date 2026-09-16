// innerhandled.go — 内側 Claude セッションが先回りで処理した inbound の skip 集合 (issue #264)
//
// 背景: bridge が spawn した Claude セッションは、agent-hub plugin の SessionStart
// オープニング手順で `mcp__agent-hub__get_messages` を自分で呼び、bridge がまだ
// dispatch していない別の未読 inbound に `send_message(caused_by=X)` で返信することがある。
// bridge はその後 X を GetMessages で受け取り、新規 inbound として別セッションに dispatch
// するため、X に対する返信が 2 通 (しかも文脈の異なるセッションから) 送られる。
//
// 対処 (方針 A): runner が stream-json から
//   - `mcp__agent-hub__send_message` の input.caused_by
//   - `mcp__agent-hub__mark_as_read` の input.message_id / message_ids
//
// を収集し (いずれも tool_result が is_error=false で確認できたもののみ)、handleOne が
// この集合に積む。processMessages / runGracefulDrain は GetMessages 結果の ID が集合に
// あれば MarkAsRead + skip する。cursor (issue #37) と同じ「secondary guard」の位置づけ。
//
// 集合は claudeRunner に 1 つ持たせ、reconnect をまたいで共有する (hub 側で未読のまま
// 残っている限り再配信されうるため、session 単位でリセットしてはいけない)。
// nil receiver は何も記録せず常に「未処理」を返す (テスト・後方互換)。
package main

import "sync"

// innerHandledMaxEntries は集合の上限。超えた分は古い順に捨てる。
// hub の未読に残る期間だけ意味があるので、数千件あれば十分。
const innerHandledMaxEntries = 4096

// innerHandledSet は内側セッションが返信済み / 既読化済みの inbound ID 集合。
type innerHandledSet struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string // 挿入順 (上限超過時の FIFO eviction 用)
}

func newInnerHandledSet() *innerHandledSet {
	return &innerHandledSet{ids: map[string]struct{}{}}
}

// add は ID 群を集合に追加する。空文字と重複は無視する。
func (s *innerHandledSet) add(ids ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := s.ids[id]; ok {
			continue
		}
		s.ids[id] = struct{}{}
		s.order = append(s.order, id)
		for len(s.order) > innerHandledMaxEntries {
			delete(s.ids, s.order[0])
			s.order = s.order[1:]
		}
	}
}

// has は ID が集合に含まれるかを返す。
// 集合からは削除しない: MarkAsRead 失敗等で同じ ID が再配信された場合にも skip が効くようにする。
func (s *innerHandledSet) has(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[id]
	return ok
}

// size は集合の要素数を返す (テスト・ログ用)。
func (s *innerHandledSet) size() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ids)
}
