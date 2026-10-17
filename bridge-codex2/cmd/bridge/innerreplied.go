// innerreplied.go — 内側 codex が先回りで返信した inbound の skip 集合 (issue #336 #4)
//
// 背景: 内側 codex は agent-hub MCP を持っているので、`get_messages` で bridge がまだ
// dispatch していない別の inbound Y を読み、`send_message(caused_by=Y)` で返信することがある。
// bridge がその後 Y を新規 inbound として流すと、Y への返信が 2 通になる (bridge-claude2 #264)。
//
// 対処 (約束 C1「二重に返さない」): runner が `codex exec --json` の
// `item.completed` / `mcp_tool_call` (tool=send_message, status=completed) から
// arguments.caused_by を集め、handleOne がこの集合に積む。polling loop / startup catchup /
// graceful drain は GetMessages 結果の ID が集合にあれば MarkAsRead して skip する。
//
// bridge-claude2 (innerhandled.go) との違い: claude2 は mark_as_read だけの ID も skip するが、
// codex2 は **返信が成功した ID だけ** を skip する。既読化だけで返信していない依頼を skip すると、
// その依頼への返信が 0 通になり C1「1 依頼に 1 返信」を破るため。
//
// 集合は codexRunner に 1 つ持たせ、reconnect をまたいで共有する。nil receiver は何も記録せず
// 常に「未返信」を返す。
package main

import "sync"

// innerRepliedMaxEntries は集合の上限。超えた分は古い順に捨てる。
const innerRepliedMaxEntries = 4096

// innerRepliedSet は内側 codex が返信済みの inbound ID 集合。
type innerRepliedSet struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string // 挿入順 (上限超過時の FIFO eviction 用)
}

func newInnerRepliedSet() *innerRepliedSet {
	return &innerRepliedSet{ids: map[string]struct{}{}}
}

// add は ID 群を集合に追加する。空文字と重複は無視する。
func (s *innerRepliedSet) add(ids ...string) {
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
		for len(s.order) > innerRepliedMaxEntries {
			delete(s.ids, s.order[0])
			s.order = s.order[1:]
		}
	}
}

// has は ID が集合に含まれるかを返す。MarkAsRead 失敗で再配信された場合にも skip が
// 効くよう、集合からは削除しない。
func (s *innerRepliedSet) has(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[id]
	return ok
}
