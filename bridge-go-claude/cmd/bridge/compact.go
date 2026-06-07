// compact.go — Idle Compact Watchdog (Python: _IdleCompactWatchdog の直訳)
//
// idle 後に /compact を自動実行して Claude のコンテキストを圧縮する (issue #60)。
// compact サマリーを daily/YYYY-MM-DD.md に追記する (issue #131)。
//
// 使い方:
//   - メッセージ受信ごとに reset() を呼ぶ。
//   - handleOne の前後で setBusy() / clearBusy() を呼ぶ (stream 競合防止 issue #102)。
//   - watchAndCompactLazy(ctx, getRunner) を goroutine として起動する。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

var (
	compactIdleS = func() float64 {
		if v := os.Getenv("BRIDGE_COMPACT_IDLE_MINUTES"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f * 60
			}
		}
		return 30 * 60 // 30 分
	}()
	compactCheckIntervalS = 60.0 // 1 分ごとにチェック
	compactArchiveDirEnv  = "BRIDGE_COMPACT_ARCHIVE_DIR"
)

// idleCompactWatchdog は idle 後に /compact を自動実行する watchdog。
// reconnect をまたいで 1 インスタンスを共有する。
type idleCompactWatchdog struct {
	mu           sync.Mutex
	lastActivity time.Time
	processing   bool
	idleS        float64
	checkS       float64
	archiveDir   string // "" = archive 無効
}

// newIdleCompactWatchdog は idleCompactWatchdog を生成する。
// workdir が "" でない場合 workdir/daily/ を archive に使う (issue #131)。
func newIdleCompactWatchdog(workdir string) *idleCompactWatchdog {
	archiveDir := os.Getenv(compactArchiveDirEnv)
	if archiveDir == "" && workdir != "" {
		archiveDir = filepath.Join(workdir, "daily")
	}
	return &idleCompactWatchdog{
		lastActivity: time.Now(),
		idleS:        compactIdleS,
		checkS:       compactCheckIntervalS,
		archiveDir:   archiveDir,
	}
}

// reset はメッセージ受信時に呼ぶ。idle タイマーをリセットする。
func (w *idleCompactWatchdog) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastActivity = time.Now()
}

// setBusy は handleOne 開始時に呼ぶ。stream 競合防止 (issue #102)。
func (w *idleCompactWatchdog) setBusy() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.processing = true
}

// clearBusy は handleOne 終了時に呼ぶ (defer で必ず呼ぶこと)。
func (w *idleCompactWatchdog) clearBusy() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.processing = false
}

func (w *idleCompactWatchdog) isProcessing() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.processing
}

func (w *idleCompactWatchdog) isIdle() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Since(w.lastActivity).Seconds() >= w.idleS
}

func (w *idleCompactWatchdog) idleElapsed() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Since(w.lastActivity).Seconds()
}

// watchAndCompactLazy は background goroutine として起動する。
// processing フラグが立っている間はスキップ (stream 競合防止 — issue #102)。
// compactFn は runner.compact を渡す (on-demand モードでは常に非 nil)。
func (w *idleCompactWatchdog) watchAndCompactLazy(ctx context.Context, compactFn func(context.Context) (string, error)) {
	ticker := time.NewTicker(time.Duration(w.checkS * float64(time.Second)))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if w.isProcessing() {
			// handleOne 実行中 — stream 競合防止のためスキップ
			w.reset()
			slog.Debug("[auto-compact] handleOne in progress, skip compact")
			continue
		}
		if !w.isIdle() {
			continue
		}

		elapsed := w.idleElapsed()
		slog.Info("[auto-compact] idle, running /compact",
			"elapsed_s", fmt.Sprintf("%.0f", elapsed),
			"threshold_s", fmt.Sprintf("%.0f", w.idleS),
		)

		summary, err := compactFn(ctx)
		if err != nil {
			slog.Warn("[auto-compact] /compact failed", "err", err)
		} else {
			slog.Info("[auto-compact] /compact completed")
			if w.archiveDir != "" {
				appendCompactSummary(summary, w.archiveDir)
			}
		}
		w.reset()
	}
}

// appendCompactSummary は compact サマリーを daily/YYYY-MM-DD.md に追記する (issue #131)。
// 書き込み失敗は WARNING ログのみ (bridge を落とさない)。
func appendCompactSummary(summary, archiveDir string) {
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		slog.Warn("[auto-compact] failed to create archive dir", "dir", archiveDir, "err", err)
		return
	}
	now := time.Now().UTC()
	dateStr := now.Format("2006-01-02")
	nowStr := now.Format("2006-01-02T15:04:05Z")
	dailyFile := filepath.Join(archiveDir, dateStr+".md")

	entry := fmt.Sprintf("\n## compact @ %s\n\n%s\n", nowStr, summary)
	f, err := os.OpenFile(dailyFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("[auto-compact] failed to open archive file", "path", dailyFile, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		slog.Warn("[auto-compact] failed to write archive entry", "path", dailyFile, "err", err)
		return
	}
	slog.Info("[auto-compact] summary appended", "path", dailyFile)
}
