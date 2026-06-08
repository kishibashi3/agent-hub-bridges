// Package bridges manages the bridges.json runtime registry.
//
// bridges.json tracks individually-spawned bridge processes:
// handle, bin name, workdir, tenant, PID, and start time.
// It lives at ~/.agent-hub/bridges.json by default.
//
// Issue: #150
package bridges

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Entry is a single bridge entry in bridges.json.
type Entry struct {
	Handle    string `json:"handle"`
	Bin       string `json:"bin"`
	Workdir   string `json:"workdir"`
	Tenant    string `json:"tenant,omitempty"`
	PID       int    `json:"pid,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// Registry is the top-level bridges.json structure, keyed by handle.
type Registry map[string]*Entry

// DefaultPath returns ~/.agent-hub/bridges.json.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agent-hub", "bridges.json"), nil
}

// Load reads bridges.json from path.
// Returns an empty Registry (not an error) when the file does not exist.
func Load(path string) (Registry, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(Registry), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read bridges.json %q: %w", path, err)
	}
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse bridges.json %q: %w", path, err)
	}
	return r, nil
}

// Save writes r to path, creating parent directories as needed.
func Save(path string, r Registry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create dir for bridges.json: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bridges.json: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write bridges.json %q: %w", path, err)
	}
	return nil
}
