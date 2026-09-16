// deferred.go — limit 休眠中の deferred メッセージの記録 (issue #271 M2)
//
// limit 休眠 (limit.go) の deferred は「MarkAsRead 済み・cursor 未保存・未処理」のため
// hub の queue にも cursor にも残らない。in-memory だけだと crash / OOM / kill -9 /
// reconnect 上限到達 exit で無通知に消え、次回起動の catchup でも取れない。
// 休眠が最長 24h 続くため、露出窓は subprocess 1 回分より大幅に長い。
//
// 対処: 休眠に入るたびに deferred 一覧を journal と同じディレクトリの
// `<user>.deferred` (JSONL) に書き、正常に復帰して処理し終えたら消す。次回起動時に
// ファイルが残っていれば ID ごとに WARN を出して operator が追えるようにする。
// 再処理はしない (crash 直前に返信済みだったメッセージへの二重応答を避けるため)。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// deferredBodyPreviewLen は記録する本文の先頭文字数 (WARN で内容を推測できる程度)。
const deferredBodyPreviewLen = 120

// deferredRecord は `<user>.deferred` の 1 行。
type deferredRecord struct {
	ID          string `json:"id"`
	From        string `json:"from"`
	Timestamp   string `json:"timestamp"`
	BodyPreview string `json:"body_preview"`
	Kind        string `json:"kind"`
	Until       string `json:"until"`
	DeferredAt  string `json:"deferred_at"`
}

// deferredStore は deferred 一覧のファイル。limitSleeper.mu の下でのみ呼ばれるため
// 自前の lock は持たない (loadAndClear は起動時の単一 goroutine から呼ぶ)。
type deferredStore struct {
	path string
}

func newDeferredStore(user string) *deferredStore {
	return &deferredStore{path: filepath.Join(journalDir(), user+".deferred")}
}

// save は deferred 一覧でファイルを上書きする (tmpfile + fsync + atomic rename)。
// msgs が空ならファイルを消す。失敗しても休眠は続けるため WARN のみ。
func (d *deferredStore) save(msgs []agenthub.Message, kind string, until time.Time) {
	if d == nil {
		return
	}
	if len(msgs) == 0 {
		d.clear()
		return
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		slog.Warn("[limit] deferred: failed to create dir", "path", d.path, "err", err)
		return
	}
	tmpPath := d.path + fmt.Sprintf(".%d.tmp", os.Getpid())
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		slog.Warn("[limit] deferred: failed to create tmp file", "tmp", tmpPath, "err", err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	w := bufio.NewWriter(f)
	for _, m := range msgs {
		data, _ := json.Marshal(deferredRecord{
			ID:          m.ID,
			From:        m.Sender,
			Timestamp:   m.Timestamp,
			BodyPreview: truncate(m.Body, deferredBodyPreviewLen),
			Kind:        kind,
			Until:       until.UTC().Format(time.RFC3339Nano),
			DeferredAt:  now,
		})
		fmt.Fprintf(w, "%s\n", data)
	}
	err = w.Flush()
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err == nil {
		err = os.Rename(tmpPath, d.path)
	}
	if err != nil {
		os.Remove(tmpPath)
		slog.Warn("[limit] deferred: failed to persist deferred ids", "path", d.path, "err", err)
		return
	}
	slog.Debug("[limit] deferred: persisted", "path", d.path, "count", len(msgs))
}

// clear はファイルを消す (存在しなければ何もしない)。
func (d *deferredStore) clear() {
	if d == nil {
		return
	}
	if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) {
		slog.Warn("[limit] deferred: failed to remove", "path", d.path, "err", err)
	}
}

// loadAndClear は前回プロセスが残した deferred 記録を読み、ファイルを消して返す。
// 破損行はスキップする。ファイルがなければ nil。
func (d *deferredStore) loadAndClear() []deferredRecord {
	if d == nil {
		return nil
	}
	f, err := os.Open(d.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("[limit] deferred: failed to open for read", "path", d.path, "err", err)
		}
		return nil
	}
	var recs []deferredRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var r deferredRecord
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			slog.Warn("[limit] deferred: skipping corrupt line", "path", d.path, "err", err)
			continue
		}
		recs = append(recs, r)
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("[limit] deferred: scan error", "path", d.path, "err", err)
	}
	f.Close()
	d.clear()
	return recs
}

// warnLostDeferred は起動時に前回プロセスの deferred 記録を WARN で出す (issue #271 M2)。
// これらは前回 MarkAsRead 済みのため hub の未読にも catchup にも現れない。
func warnLostDeferred(store *deferredStore) {
	recs := store.loadAndClear()
	if len(recs) == 0 {
		return
	}
	slog.Warn("[limit] previous process exited during limit sleep — deferred messages were NOT processed "+
		"(already marked as read on hub; not re-dispatched)",
		"count", len(recs), "path", store.path)
	for _, r := range recs {
		slog.Warn("[limit] lost deferred message",
			"msg_id", r.ID, "from", r.From, "ts", r.Timestamp,
			"kind", r.Kind, "until", r.Until, "deferred_at", r.DeferredAt,
			"body_preview", r.BodyPreview)
	}
}
