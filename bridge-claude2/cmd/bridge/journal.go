// journal.go — Outgoing message journal (Python: _common/journal.py の直訳)
//
// bridge が hub.SendMessage() を呼ぶ前に journal entry を書き出し、
// 送信完了後に削除する。再起動時は pending entry を replay することで
// 「bridge クラッシュによる outgoing メッセージ消失」を防止する。
//
// at-least-once セマンティクス: journal に書いてから送信するため、
// クラッシュ後に再送されると重複する可能性がある (idempotency は TODO)。
//
// フォーマット: JSONL (1 行 1 エントリ)。
// 保存先: ~/.agent-hub/journals/<user>.journal (env AGENT_HUB_JOURNAL_DIR で上書き可)。
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const journalDirEnv = "AGENT_HUB_JOURNAL_DIR"

// journalEntry は journal の 1 エントリ。hub.SendMessage の引数と同じ形式。
type journalEntry struct {
	ID        string `json:"id"`
	To        string `json:"to"`
	Message   string `json:"message"`
	CausedBy  string `json:"caused_by,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Journal は JSONL 形式の outgoing message journal。
// スレッドセーフ: mu で write/delete/loadAll を保護する。
type Journal struct {
	mu   sync.Mutex
	path string
}

// newJournal は指定 user の Journal を生成する。
func newJournal(user string) *Journal {
	dir := os.Getenv(journalDirEnv)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/tmp"
		}
		dir = filepath.Join(home, ".agent-hub", "journals")
	}
	return &Journal{path: filepath.Join(dir, user+".journal")}
}

// makeEntry は新規 journalEntry を生成する (書き込みは行わない)。
func (j *Journal) makeEntry(to, message, causedBy string) journalEntry {
	return journalEntry{
		ID:        newRandomID(),
		To:        to,
		Message:   message,
		CausedBy:  causedBy,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// newRandomID は crypto/rand を使った UUID-like なランダム ID を生成する。
func newRandomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// フォールバック: 時刻ベース (本番では発生しない想定)
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	// UUID v4 形式 (xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// write は journal に entry を末尾 append する。
// Sync で即時ディスク反映。失敗時は false を返す (例外は上げない)。
// 呼び出し側は false を受け取ったら送信を中止すること。
func (j *Journal) write(entry journalEntry) bool {
	j.mu.Lock()
	defer j.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		slog.Warn("journal: failed to create journal dir", "path", j.path, "err", err)
		return false
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("journal: failed to open journal file", "path", j.path, "err", err)
		return false
	}
	defer f.Close()

	data, _ := json.Marshal(entry)
	if _, err := fmt.Fprintf(f, "%s\n", data); err != nil {
		slog.Warn("journal: failed to write entry", "id", entry.ID, "err", err)
		return false
	}
	if err := f.Sync(); err != nil {
		slog.Warn("journal: fsync failed", "id", entry.ID, "err", err)
		return false
	}
	slog.Debug("journal: write", "id", entry.ID, "path", j.path)
	return true
}

// loadAll は journal の全エントリを読み込む。
// 破損行はスキップして WARNING ログのみ (bridge を落とさない)。
// ファイルが存在しない場合は nil を返す。
func (j *Journal) loadAll() []journalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.loadAllLocked()
}

func (j *Journal) loadAllLocked() []journalEntry {
	f, err := os.Open(j.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("journal: failed to open for read", "path", j.path, "err", err)
		}
		return nil
	}
	defer f.Close()

	var entries []journalEntry
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			slog.Warn("journal: skipping corrupt line", "line", lineNo, "path", j.path, "err", err)
			continue
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("journal: scan error", "path", j.path, "err", err)
	}
	return entries
}

// delete は entry_id に一致するエントリを journal から削除する。
// tmpfile + atomic rename で crash-safe に更新する。
// 失敗しても例外を上げない (WARNING ログのみ)。
func (j *Journal) delete(entryID string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	entries := j.loadAllLocked()
	remaining := make([]journalEntry, 0, len(entries))
	found := false
	for _, e := range entries {
		if e.ID == entryID {
			found = true
			continue
		}
		remaining = append(remaining, e)
	}
	if !found {
		slog.Debug("journal: entry not found (already deleted?)", "id", entryID)
		return
	}
	j.writeAllLocked(remaining)
	slog.Debug("journal: deleted entry", "id", entryID, "path", j.path)
}

// writeAllLocked は journal を remaining で上書きする (atomic rename)。
// remaining が空の場合はファイルを削除する。
// j.mu は呼び出し側が保持していること。
func (j *Journal) writeAllLocked(remaining []journalEntry) {
	if len(remaining) == 0 {
		if err := os.Remove(j.path); err != nil && !os.IsNotExist(err) {
			slog.Warn("journal: failed to remove empty journal", "path", j.path, "err", err)
		}
		return
	}
	tmpPath := j.path + fmt.Sprintf(".%d.tmp", os.Getpid())
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		slog.Warn("journal: failed to create tmp file for rewrite", "tmp", tmpPath, "err", err)
		return
	}
	defer func() {
		if f != nil {
			f.Close()
			os.Remove(tmpPath)
		}
	}()
	w := bufio.NewWriter(f)
	for _, e := range remaining {
		data, _ := json.Marshal(e)
		fmt.Fprintf(w, "%s\n", data)
	}
	if err := w.Flush(); err != nil {
		slog.Warn("journal: flush error on rewrite", "err", err)
		return
	}
	if err := f.Sync(); err != nil {
		slog.Warn("journal: fsync error on rewrite", "err", err)
		return
	}
	f.Close()
	f = nil // suppress defer cleanup
	if err := os.Rename(tmpPath, j.path); err != nil {
		slog.Warn("journal: atomic rename failed", "tmp", tmpPath, "dst", j.path, "err", err)
	}
}
