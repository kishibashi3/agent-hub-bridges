// inventory.go — Dead marker + bridge inventory
//
// circuit breaker 発火時に dead marker ファイルを作成し、
// BRIDGE_INVENTORY が設定されていれば lost-hub エントリを追記する。
// bridge-claude2 の inventory.go と同等 (パス template のみ変更)。
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"
)

const (
	inventoryEnv       = "BRIDGE_INVENTORY"
	deadMarkerTemplate = "/tmp/bridge-codex2-%s.dead"
)

func writeDeadMarker(user string) {
	path := fmt.Sprintf(deadMarkerTemplate, user)
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(ts+"\n"), 0o644); err != nil {
		slog.Warn("inventory: failed to write dead marker", "path", path, "err", err)
		return
	}
	slog.Info("inventory: dead marker written", "path", path)
}

func writeLostHubToInventory(user string, pid int) {
	inventoryPath := os.Getenv(inventoryEnv)
	if inventoryPath == "" {
		return
	}
	entry := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339),
		"event": "lost-hub",
		"user":  user,
		"pid":   pid,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		slog.Warn("inventory: failed to marshal lost-hub entry", "err", err)
		return
	}
	f, err := os.OpenFile(inventoryPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("inventory: failed to open inventory", "path", inventoryPath, "err", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n", data)
	slog.Info("inventory: lost-hub entry written", "path", inventoryPath, "user", user)
}
