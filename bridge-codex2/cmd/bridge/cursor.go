// cursor.go — 再起動後の重複ディスパッチを防ぐ timestamp cursor
//
// bridge-claude2 の cursor.go と同等。パス template のみ bridge-codex2 用に変更。
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

const (
	cursorFileEnv      = "AGENT_HUB_CURSOR_FILE"
	cursorFileTemplate = "/tmp/bridge-codex2-%s-cursor.json"
)

type cursorData struct {
	LastProcessedAt string `json:"last_processed_at"`
}

func cursorPath(user string) string {
	if v := os.Getenv(cursorFileEnv); v != "" {
		return v
	}
	return fmt.Sprintf(cursorFileTemplate, user)
}

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
