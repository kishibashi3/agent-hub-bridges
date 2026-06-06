// fleet.go — multi-persona fleet management for bridge-tmux (issue #141).
//
// 設計方針 (@planner 回答):
//   - 全 persona を 1 YAML ファイルに列挙 (fleet-wide 1ファイル形式)
//   - 複数 persona は並列起動・並列 shutdown (順序要件なし)
//
// YAML schema:
//
//	personas:
//	  - handle: reviewer
//	    workdir: /path/to/reviewer
//	    idle_timeout: 10m
//	    bypass_permissions: true
//	  - handle: planner
//	    workdir: /path/to/planner
//	    idle_timeout: 15m
//	    model: claude-opus-4-7
//	    env:
//	      MY_CUSTOM_VAR: value
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sync"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
	"github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/tmux"
	"gopkg.in/yaml.v3"
)

// envKeyRegex は有効な環境変数名のパターン (POSIX 準拠)。
// キー名の無検証によるシェルインジェクションを防ぐ (Critical #1)。
var envKeyRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ──────────────────────────────────────────────────────────────────────── //
// YAML 型定義                                                               //
// ──────────────────────────────────────────────────────────────────────── //

// yamlDuration は YAML で "10m" のような duration 文字列をパースするカスタム型。
type yamlDuration struct {
	time.Duration
}

func (d *yamlDuration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = dur
	return nil
}

// ──────────────────────────────────────────────────────────────────────── //
// Config 型                                                                 //
// ──────────────────────────────────────────────────────────────────────── //

// PersonaConfig は fleet YAML の 1 persona エントリ。
// bypass_permissions はデフォルト false (無効) — 有効にするには true を明示すること。
type PersonaConfig struct {
	Handle            string            `yaml:"handle"`
	Workdir           string            `yaml:"workdir"`
	DisplayName       string            `yaml:"display_name,omitempty"`
	Model             string            `yaml:"model,omitempty"`
	BypassPermissions bool              `yaml:"bypass_permissions,omitempty"`
	IdleTimeout       yamlDuration      `yaml:"idle_timeout,omitempty"`
	Env               map[string]string `yaml:"env,omitempty"`
}

// FleetConfig は bridge-fleet.yaml のトップレベル構造。
type FleetConfig struct {
	Personas []PersonaConfig `yaml:"personas"`
}

// ──────────────────────────────────────────────────────────────────────── //
// ロード                                                                    //
// ──────────────────────────────────────────────────────────────────────── //

// LoadFleetConfig は YAML ファイルを読んで FleetConfig を返す。
// 以下をバリデートする:
//   - personas が空でないこと
//   - 各 persona に handle / workdir が設定されていること
//   - env キー名が POSIX 環境変数名パターンに一致すること (シェルインジェクション防止)
//
// YAML unknown フィールドは strict モードで拒否する (typo 早期検出)。
func LoadFleetConfig(path string) (*FleetConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet config %q: %w", path, err)
	}

	// KnownFields(true): unknown フィールドをエラーとして扱う (typo 早期検出)
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

// ──────────────────────────────────────────────────────────────────────── //
// Config 変換                                                               //
// ──────────────────────────────────────────────────────────────────────── //

// personaToConfig は PersonaConfig をグローバル設定とマージして *config を返す。
// IdleTimeout が 0 の場合はグローバル値を引き継ぐ。
func personaToConfig(p PersonaConfig, global *config) *config {
	c := *global // コピー
	c.User = p.Handle
	c.DisplayName = p.DisplayName
	c.Workdir = p.Workdir
	c.Model = p.Model
	c.BypassPerms = p.BypassPermissions
	if p.IdleTimeout.Duration > 0 {
		c.IdleTimeout = p.IdleTimeout.Duration
	}
	return &c
}

// ──────────────────────────────────────────────────────────────────────── //
// Fleet 起動                                                                //
// ──────────────────────────────────────────────────────────────────────── //

// RunFleet は全 persona を並列に起動し、ctx がキャンセルされるまで動かす。
// 全 persona goroutine が終了したあとに最初のエラーを返す。
// health は fleet 全体で共有する HealthState (health server 無効時でも non-nil)。
func RunFleet(ctx context.Context, global *config, fleet *FleetConfig, health *HealthState) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for _, p := range fleet.Personas {
		wg.Add(1)
		p := p // ループ変数キャプチャ
		go func() {
			defer wg.Done()
			cfg := personaToConfig(p, global)
			slog.Info("fleet persona starting", "handle", "@"+cfg.User, "workdir", cfg.Workdir)
			if err := runPersona(ctx, cfg, p.Env, health); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("persona @%s: %w", cfg.User, err))
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	return errors.Join(errs...)
}

// runPersona は 1 persona の MCP 接続・ポーリングループを管理する。
// ctx がキャンセルされた場合は nil を返す。
func runPersona(ctx context.Context, cfg *config, extraEnv map[string]string, health *HealthState) error {
	selfHandle := "@" + cfg.User
	mcpConfigPath, err := writeMCPConfig(cfg)
	if err != nil {
		return fmt.Errorf("writeMCPConfig: %w", err)
	}
	defer os.Remove(mcpConfigPath)

	client, err := agenthub.New(
		cfg.AgentHubURL, cfg.GitHubPAT, cfg.User, cfg.Tenant,
		agenthub.WithClientName("bridge-tmux"),
	)
	if err != nil {
		return fmt.Errorf("agenthub.New: %w", err)
	}

	if err := client.Initialize(ctx); err != nil {
		if ctx.Err() != nil {
			return nil // ctx キャンセル → 正常シャットダウン
		}
		return fmt.Errorf("MCP initialize: %w", err)
	}
	registered, err := client.Register(ctx, cfg.DisplayName, "stateful")
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("register: %w", err)
	}
	slog.Info("fleet persona registered", "handle", selfHandle, "result", firstLine(registered))

	session := tmux.NewSession(tmux.SessionOptions{
		UserID:           cfg.User,
		Workdir:          cfg.Workdir,
		MCPConfigPath:    mcpConfigPath,
		ClaudeCLI:        cfg.ClaudeCLI,
		Model:            cfg.Model,
		BypassPerms:      cfg.BypassPerms,
		SpawnTimeout:     cfg.SpawnTimeout,
		ActivityIdleTime: cfg.ActivityIdle,
		ResponseTimeout:  cfg.ResponseTimeout,
		Env:              extraEnv,
	})
	manager := newSessionManager(cfg, session)
	// idle timer が発火したとき health state を更新する
	manager.onIdle = func() {
		health.SetSessionAlive(selfHandle, false)
		slog.Info("persona session killed by idle timer", "handle", selfHandle)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		manager.Shutdown(shutCtx)
	}()

	// 接続・再接続ループ
	for {
		if err := runBridge(ctx, cfg, client, manager, health); ctx.Err() != nil {
			return nil
		} else if err != nil {
			slog.Warn("fleet persona runBridge ended", "handle", selfHandle, "err", err)
		}

		sleepWithContext(ctx, cfg.ReconnectBackoff)
		if ctx.Err() != nil {
			return nil
		}

		if err := client.Initialize(ctx); err != nil {
			slog.Warn("fleet persona re-initialize failed", "handle", selfHandle, "err", err)
			continue
		}
		if _, err := client.Register(ctx, cfg.DisplayName, "stateful"); err != nil {
			slog.Warn("fleet persona re-register failed", "handle", selfHandle, "err", err)
			continue
		}
	}
}

// firstLine は文字列の最初の行を返す。
func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
