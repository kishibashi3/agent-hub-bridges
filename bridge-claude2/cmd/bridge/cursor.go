// cursor.go — 再起動後の重複ディスパッチを防ぐ timestamp cursor (Python: cursor.py の直訳)
//
// bridge 再起動で in-memory の既読状態がリセットされ、未処理メッセージが
// 重複 dispatch されるのを防ぐ。最後に処理した message の timestamp と、その timestamp で
// 処理済みの message ID を JSON ファイルに永続化し、再起動後は cursor より前のメッセージと、
// cursor と同じ timestamp で処理済みの ID をスキップする。
//
// issue #322: timestamp は ms 単位なので、同じ ms のメッセージが複数あり得る。
// timestamp だけで `<=` 判定すると、同じ ms の 2 通目以降を処理せずに捨ててしまう。
//
// 保存順: process → saveCursor → MarkAsRead (crash-safe)
//
// 環境変数 AGENT_HUB_CURSOR_FILE でパスを上書き可能。
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
	cursorFileTemplate = "/tmp/%s-%s-cursor.json"
)

type cursorData struct {
	LastProcessedAt string `json:"last_processed_at"`
	// IDsAtLastProcessedAt は LastProcessedAt と同じ timestamp で処理済みの message ID (issue #322)。
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

// seen は msg が処理済み位置以前にあるかを返す (issue #37 の skip 判定)。
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

// cursorPath は key (config.stateKey()) の cursor ファイルのパスを返す。
// AGENT_HUB_CURSOR_FILE 環境変数が設定されていればそれを優先する。
func cursorPath(key string) string {
	if v := os.Getenv(cursorFileEnv); v != "" {
		return v
	}
	return fmt.Sprintf(cursorFileTemplate, bridgeType, key)
}

// loadCursor は永続化された cursor を読む。
// ファイルが存在しない / 読み込み失敗時はゼロ値を返す (fresh start)。
func loadCursor(key string) cursorPos {
	path := cursorPath(key)
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

// saveCursor は cursor を永続化する。
// 書き込み失敗は WARNING ログのみ (= bridge を落とさない)。
//
// issue #326: journal / deferred と同じく 0o600 で書く。os.WriteFile は既存ファイルの
// パーミッションを変えないので、以前の版が 0o644 で作ったファイルも Chmod で 0o600 に直す。
func saveCursor(key string, c cursorPos) {
	path := cursorPath(key)
	data, err := json.Marshal(cursorData{LastProcessedAt: c.TS, IDsAtLastProcessedAt: c.IDs})
	if err != nil {
		slog.Warn("cursor: failed to marshal", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		slog.Warn("cursor: failed to save", "path", path, "err", err)
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		slog.Warn("cursor: failed to chmod", "path", path, "err", err)
	}
	slog.Debug("cursor: saved", "last_processed_at", c.TS, "ids_at_last_processed_at", len(c.IDs), "path", path)
}
