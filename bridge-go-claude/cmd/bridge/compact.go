// compact.go — compact サマリーのアーカイブ (issue #131)
//
// idle compact watchdog は on-demand bridge には不要なため削除済み (issue #179)。
// compact は SIGTERM 受信時に runGracefulDrain() から呼ばれる (issue #178)。
//
// appendCompactSummary: compact サマリーを daily/YYYY-MM-DD.md に追記する。
// compactArchiveDirFor: archive ディレクトリを解決するヘルパー。
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const compactArchiveDirEnv = "BRIDGE_COMPACT_ARCHIVE_DIR"

// compactArchiveDirFor は archive ディレクトリを解決する。
// BRIDGE_COMPACT_ARCHIVE_DIR env > workdir/daily > "" (archive 無効)。
func compactArchiveDirFor(workdir string) string {
	if v := os.Getenv(compactArchiveDirEnv); v != "" {
		return v
	}
	if workdir != "" {
		return filepath.Join(workdir, "daily")
	}
	return ""
}

// appendCompactSummary は compact サマリーを daily/YYYY-MM-DD.md に追記する (issue #131)。
// 書き込み失敗は WARNING ログのみ (bridge を落とさない)。
func appendCompactSummary(summary, archiveDir string) {
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		slog.Warn("[compact] failed to create archive dir", "dir", archiveDir, "err", err)
		return
	}
	now := time.Now().UTC()
	dateStr := now.Format("2006-01-02")
	nowStr := now.Format("2006-01-02T15:04:05Z")
	dailyFile := filepath.Join(archiveDir, dateStr+".md")

	entry := fmt.Sprintf("\n## compact @ %s\n\n%s\n", nowStr, summary)
	f, err := os.OpenFile(dailyFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("[compact] failed to open archive file", "path", dailyFile, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		slog.Warn("[compact] failed to write archive entry", "path", dailyFile, "err", err)
		return
	}
	slog.Info("[compact] summary appended", "path", dailyFile)
}
