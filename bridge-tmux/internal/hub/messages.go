// Package hub provides a minimal MCP HTTP client for agent-hub.
package hub

import (
	"encoding/json"
	"fmt"
)

// Message は agent-hub の get_messages が返す 1 件のメッセージ。
// wire format: {id, from, to, message, caused_by, timestamp}
type Message struct {
	ID        string `json:"id"`
	Sender    string `json:"from"`    // wire: "from"
	To        string `json:"to"`
	Body      string `json:"message"` // wire: "message"
	CausedBy  string `json:"caused_by"`
	Timestamp string `json:"timestamp"`
}

// ParseMessages は get_messages ツールのテキスト結果 (JSON 配列) をパースする。
// 壊れた行は silent skip (= Python SDK と同じ defensive 方針)。
func ParseMessages(text string) ([]Message, error) {
	if text == "" {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("get_messages parse: %w", err)
	}
	msgs := make([]Message, 0, len(raw))
	for _, r := range raw {
		var m Message
		if err := json.Unmarshal(r, &m); err != nil {
			continue // skip malformed entry
		}
		if m.ID == "" || m.Sender == "" {
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}
