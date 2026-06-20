// cursor.go — 再起動後の重複ディスパッチを防ぐ timestamp cursor (Python: cursor.py の直訳)
//
// bridge 再起動で in-memory の既読状態がリセットされ、未処理メッセージが
// 重複 dispatch されるのを防ぐ。最後に処理した message の timestamp を JSON ファイルに
// 永続化し、再起動後は msg.Timestamp <= cursor のメッセージをスキップする。
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
)

const (
	cursorFileEnv      = "AGENT_HUB_CURSOR_FILE"
	cursorFileTemplate = "/tmp/%s-%s-cursor.json"
)

type cursorData struct {
	LastProcessedAt string `json:"last_processed_at"`
}

// cursorPath は cursor ファイルのパスを返す。
// AGENT_HUB_CURSOR_FILE 環境変数が設定されていればそれを優先する。
func cursorPath(user string) string {
	if v := os.Getenv(cursorFileEnv); v != "" {
		return v
	}
	return fmt.Sprintf(cursorFileTemplate, bridgeType, user)
}

// loadCursor は永続化された cursor timestamp を読む。
// ファイルが存在しない / 読み込み失敗時は "" を返す (fresh start)。
func loadCursor(user string) string {
	path := cursorPath(user)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("cursor: failed to read cursor file", "path", path, "err", err)
		}
		return ""
	}
	var cd cursorData
	if err := json.Unmarshal(data, &cd); err != nil {
		slog.Warn("cursor: failed to parse cursor file", "path", path, "err", err)
		return ""
	}
	if cd.LastProcessedAt == "" {
		return ""
	}
	slog.Info("cursor: loaded", "last_processed_at", cd.LastProcessedAt, "path", path)
	return cd.LastProcessedAt
}

// saveCursor は cursor timestamp を永続化する。
// 書き込み失敗は WARNING ログのみ (= bridge を落とさない)。
func saveCursor(user, timestamp string) {
	path := cursorPath(user)
	data, err := json.Marshal(cursorData{LastProcessedAt: timestamp})
	if err != nil {
		slog.Warn("cursor: failed to marshal", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn("cursor: failed to save", "path", path, "err", err)
		return
	}
	slog.Debug("cursor: saved", "last_processed_at", timestamp, "path", path)
}
