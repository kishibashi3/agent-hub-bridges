// statefile.go — 記録ファイル (cursor / journal / deferred) の名前と旧名からの移行 (issue #288 項目 1)
//
// 記録ファイルはこれまで participant 名だけで名前を決めていたため、同じ HOME で同名の
// bridge を別 tenant で動かすと互いの記録を読み書きしてしまう。tenant を指定したときは
// `<tenant>__<participant>` をファイル名に使う。tenant 未指定なら従来どおり participant のみ。
//
// 移行: tenant 付きのファイルがまだなく旧名のファイルがあれば、旧ファイルを新しい名前に
// copy し、旧ファイルは `<旧名>.pre-tenant` として残す (不具合時に戻せるように)。
// 旧名のまま残さないのは、同名の別 tenant の bridge が同じ記録を引き継がないようにするため。
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// stateKeySep は記録ファイル名で tenant と participant をつなぐ区切り。
const stateKeySep = "__"

// preTenantSuffix は移行元の旧ファイルに付ける suffix。
const preTenantSuffix = ".pre-tenant"

// stateKey は記録ファイル名に使うキーを返す。
func (c *config) stateKey() string {
	if c.Tenant == "" {
		return c.Participant
	}
	return c.Tenant + stateKeySep + c.Participant
}

// validateTenantForFileName は tenant がファイル名の一部として使えるかを確認する。
func validateTenantForFileName(tenant string) error {
	if tenant == "." || tenant == ".." || strings.ContainsAny(tenant, `/\`) || strings.ContainsRune(tenant, 0) {
		return fmt.Errorf("invalid tenant %q: must not contain path separators", tenant)
	}
	return nil
}

// migrateStateFiles は旧名 (participant のみ) の記録ファイルを tenant 付きの名前へ移す。
// tenant 未指定なら何もしない。失敗しても bridge は止めない (WARN のみ)。
func migrateStateFiles(cfg *config) {
	if cfg.Tenant == "" {
		return
	}
	oldKey, newKey := cfg.Participant, cfg.stateKey()
	pairs := [][2]string{
		{filepath.Join(cfg.JournalDir, oldKey+".journal"), filepath.Join(cfg.JournalDir, newKey+".journal")},
		{filepath.Join(cfg.JournalDir, oldKey+".deferred"), filepath.Join(cfg.JournalDir, newKey+".deferred")},
	}
	// AGENT_HUB_CURSOR_FILE 指定時は key によらず同じパスなので移行しない
	if os.Getenv(cursorFileEnv) == "" {
		pairs = append(pairs, [2]string{cursorPath(oldKey), cursorPath(newKey)})
	}
	for _, p := range pairs {
		migrateStateFile(p[0], p[1])
	}
}

// migrateStateFile は oldPath を newPath に copy し、oldPath を oldPath+".pre-tenant" に退避する。
// newPath が既にあれば移行済みとみなして何もしない。
func migrateStateFile(oldPath, newPath string) {
	if _, err := os.Stat(oldPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("state: failed to stat legacy file", "path", oldPath, "err", err)
		}
		return
	}
	if _, err := os.Stat(newPath); err == nil {
		slog.Warn("state: legacy file ignored (tenant-scoped file already exists)",
			"legacy", oldPath, "path", newPath)
		return
	}
	if err := copyFileAtomic(oldPath, newPath); err != nil {
		slog.Warn("state: failed to copy legacy file", "from", oldPath, "to", newPath, "err", err)
		return
	}
	backup := oldPath + preTenantSuffix
	if err := os.Rename(oldPath, backup); err != nil {
		slog.Warn("state: failed to keep legacy file as backup", "from", oldPath, "to", backup, "err", err)
	}
	slog.Info("state: migrated legacy file", "from", oldPath, "to", newPath, "backup", backup)
}

// copyFileAtomic は src を dst に copy する (tmpfile + fsync + rename、パーミッションは src に合わせる)。
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmpPath := dst + fmt.Sprintf(".%d.tmp", os.Getpid())
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpPath, dst)
	}
	if err != nil {
		os.Remove(tmpPath)
	}
	return err
}
