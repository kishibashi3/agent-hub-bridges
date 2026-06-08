// Package bridges provides the bridges.json registry loader for agenthubctl.
//
// Config file location (first match wins):
//  1. $AGENTHUBCTL_CONFIG_DIR/bridges.json
//  2. ~/.config/agenthubctl/bridges.json
//
// Issue: #215
package bridges

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is a single bridge definition in bridges.json.
type Entry struct {
	Handle      string `json:"handle"`
	Workdir     string `json:"workdir"`
	Tenant      string `json:"tenant,omitempty"`
	Model       string `json:"model,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Timeout     string `json:"timeout,omitempty"` // e.g. "10m", "1h"
}

// Config is the top-level bridges.json structure.
type Config struct {
	Bridges []Entry `json:"bridges"`
}

// DefaultConfigPath returns the default path for bridges.json.
// Uses $AGENTHUBCTL_CONFIG_DIR if set, otherwise ~/.config/agenthubctl/.
func DefaultConfigPath() (string, error) {
	if dir := os.Getenv("AGENTHUBCTL_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "bridges.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".config", "agenthubctl", "bridges.json"), nil
}

// Load reads and validates the bridges.json file at path.
// Returns os.ErrNotExist if the file is absent (caller may treat as empty registry).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bridges config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse bridges config %q: %w", path, err)
	}
	for i := range cfg.Bridges {
		b := &cfg.Bridges[i]
		b.Handle = strings.TrimPrefix(b.Handle, "@")
		if b.Handle == "" {
			return nil, fmt.Errorf("bridges[%d]: handle is required", i)
		}
		if b.Workdir == "" {
			return nil, fmt.Errorf("bridges[%d] %q: workdir is required", i, b.Handle)
		}
		if b.Timeout != "" {
			if _, err := time.ParseDuration(b.Timeout); err != nil {
				return nil, fmt.Errorf("bridges[%d] %q: invalid timeout %q: %w", i, b.Handle, b.Timeout, err)
			}
		}
	}
	return &cfg, nil
}

// Lookup finds an entry by handle (@ prefix optional). Returns nil if not found.
func (c *Config) Lookup(handle string) *Entry {
	h := strings.TrimPrefix(handle, "@")
	for i := range c.Bridges {
		if c.Bridges[i].Handle == h {
			return &c.Bridges[i]
		}
	}
	return nil
}
