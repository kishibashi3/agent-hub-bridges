// cursor.go — 再起動後の重複ディスパッチを防ぐ timestamp cursor
//
// bridge-claude2 の cursor.go と同等。パス template のみ bridge-codex2 用に変更。
//
// issue #323 (#322 と同じパターン): timestamp は ms 単位なので、同じ ms のメッセージが
// 複数あり得る。timestamp だけで `<=` 判定すると、同じ ms の 2 通目以降を処理せずに
// 捨ててしまう。最後の timestamp に加えて、その timestamp で処理済みの message ID を持つ。
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

const (
	cursorFileEnv      = "AGENT_HUB_CURSOR_FILE"
	cursorFileTemplate = "/tmp/bridge-codex2-%s-cursor.json"
)

type cursorData struct {
	LastProcessedAt string `json:"last_processed_at"`
	// IDsAtLastProcessedAt は LastProcessedAt と同じ timestamp で処理済みの message ID (issue #323)。
	// このフィールドが無い旧形式のファイルは空として読む。
	IDsAtLastProcessedAt []string `json:"ids_at_last_processed_at,omitempty"`
}

// cursorPos は処理済み位置。TS が "" なら cursor なし (fresh start)。
//
// 値として持ち回るので、advance は IDs を共有しない新しい slice を作る。
type cursorPos struct {
	TS  string
	IDs []string
}

// seen は msg が処理済み位置以前にあるかを返す (skip 判定)。
//
// TS と同じ timestamp のメッセージは、処理済み ID に入っているものだけを seen とする。
// 旧形式の cursor ファイル (ID なし) から読んだ直後は、同じ timestamp のメッセージを
// seen としない。捨てるより、MarkAsRead 済みで hub が返さないはずのものを 1 度
// 通すほうを選ぶ。
func (c cursorPos) seen(msg agenthub.Message) bool {
	if c.TS == "" || msg.Timestamp > c.TS {
		return false
	}
	if msg.Timestamp < c.TS {
		return true
	}
	return slices.Contains(c.IDs, msg.ID)
}

// advance は msg を処理済みにした cursor を返す。
// msg が今の位置より前 (通常は起きない) なら位置を戻さない。
func (c cursorPos) advance(msg agenthub.Message) cursorPos {
	switch {
	case c.TS == "" || msg.Timestamp > c.TS:
		return cursorPos{TS: msg.Timestamp, IDs: []string{msg.ID}}
	case msg.Timestamp == c.TS:
		if slices.Contains(c.IDs, msg.ID) {
			return c
		}
		ids := make([]string, 0, len(c.IDs)+1)
		ids = append(ids, c.IDs...)
		return cursorPos{TS: c.TS, IDs: append(ids, msg.ID)}
	default:
		return c
	}
}

func cursorPath(user string) string {
	if v := os.Getenv(cursorFileEnv); v != "" {
		return v
	}
	return fmt.Sprintf(cursorFileTemplate, user)
}

func loadCursor(user string) cursorPos {
	path := cursorPath(user)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cursor: failed to read cursor file", "path", path, "err", err)
		}
		return cursorPos{}
	}
	var cd cursorData
	if err := json.Unmarshal(data, &cd); err != nil {
		slog.Warn("cursor: failed to parse cursor file", "path", path, "err", err)
		return cursorPos{}
	}
	if cd.LastProcessedAt == "" {
		return cursorPos{}
	}
	slog.Info("cursor: loaded", "last_processed_at", cd.LastProcessedAt,
		"ids_at_last_processed_at", len(cd.IDsAtLastProcessedAt), "path", path)
	return cursorPos{TS: cd.LastProcessedAt, IDs: cd.IDsAtLastProcessedAt}
}

func saveCursor(user string, c cursorPos) {
	path := cursorPath(user)
	data, err := json.Marshal(cursorData{LastProcessedAt: c.TS, IDsAtLastProcessedAt: c.IDs})
	if err != nil {
		slog.Warn("cursor: failed to marshal", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn("cursor: failed to save", "path", path, "err", err)
		return
	}
	slog.Debug("cursor: saved", "last_processed_at", c.TS, "ids_at_last_processed_at", len(c.IDs), "path", path)
}
