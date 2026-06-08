// Package fleet provides fleet config types shared between bridge and agenthubctl.
//
// Issue: #150
package fleet

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

var envKeyRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// YAMLDuration parses "10m"-style duration strings in YAML.
type YAMLDuration struct {
	time.Duration
}

func (d *YAMLDuration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = dur
	return nil
}

// MarshalYAML serializes the duration as a human-readable string (e.g. "10m0s").
func (d YAMLDuration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

// IsZero enables yaml.v3 omitempty to skip zero durations.
func (d YAMLDuration) IsZero() bool {
	return d.Duration == 0
}

// PersonaConfig is a single persona entry in fleet YAML.
type PersonaConfig struct {
	Handle            string            `yaml:"handle"`
	Workdir           string            `yaml:"workdir"`
	DisplayName       string            `yaml:"display_name,omitempty"`
	Model             string            `yaml:"model,omitempty"`
	BypassPermissions bool              `yaml:"bypass_permissions,omitempty"`
	IdleTimeout       YAMLDuration      `yaml:"idle_timeout,omitempty"`
	Env               map[string]string `yaml:"env,omitempty"`
}

// FleetConfig is the top-level fleet YAML structure.
// HealthPort is the HTTP /health port for bridge-tmux (0 = disabled).
type FleetConfig struct {
	HealthPort int             `yaml:"health_port,omitempty"`
	Personas   []PersonaConfig `yaml:"personas"`
}

// LoadFleetConfig reads and validates a fleet YAML file.
// Unknown fields are rejected (KnownFields strict mode) to catch typos early.
func LoadFleetConfig(path string) (*FleetConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet config %q: %w", path, err)
	}

	var cfg FleetConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse fleet config %q: %w", path, err)
	}

	if len(cfg.Personas) == 0 {
		return nil, fmt.Errorf("fleet config %q: no personas defined", path)
	}
	for i, p := range cfg.Personas {
		if p.Handle == "" {
			return nil, fmt.Errorf("fleet config %q: persona[%d]: handle is required", path, i)
		}
		if p.Workdir == "" {
			return nil, fmt.Errorf("fleet config %q: persona %q: workdir is required", path, p.Handle)
		}
		for k := range p.Env {
			if !envKeyRegex.MatchString(k) {
				return nil, fmt.Errorf("fleet config %q: persona %q: invalid env key %q"+
					" (must match ^[A-Za-z_][A-Za-z0-9_]*$)", path, p.Handle, k)
			}
		}
	}
	return &cfg, nil
}

// WriteFleetConfig serializes and writes a fleet config to disk.
// Note: comments from the original file are not preserved.
func WriteFleetConfig(path string, cfg *FleetConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal fleet config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write fleet config %q: %w", path, err)
	}
	return nil
}
