// inventory.go — Dead marker + bridge inventory (Python: _common/inventory.py の直訳)
//
// circuit breaker 発火時に dead marker ファイルを作成し、
// BRIDGE_INVENTORY が設定されていれば lost-hub エントリを追記する。
// operator の stop-bridge.sh --dead によるクリーンアップに使う。
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
	deadMarkerTemplate = "/tmp/bridge-claude2-%s.dead"
)

// writeDeadMarker は dead marker ファイルを作成する。
// 失敗しても WARNING ログのみ (bridge を落とさない)。
func writeDeadMarker(user string) {
	path := fmt.Sprintf(deadMarkerTemplate, user)
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(ts+"\n"), 0o644); err != nil {
		slog.Warn("inventory: failed to write dead marker", "path", path, "err", err)
		return
	}
	slog.Info("inventory: dead marker written", "path", path)
}

// writeLostHubToInventory は BRIDGE_INVENTORY に lost-hub エントリを追記する。
// BRIDGE_INVENTORY が未設定の場合は no-op。
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
