// fleet.go — multi-persona fleet management for bridge-tmux (issue #141).
//
// 設計方針 (@planner 回答):
//   - 全 persona を 1 YAML ファイルに列挙 (fleet-wide 1ファイル形式)
//   - 複数 persona は並列起動・並列 shutdown (順序要件なし)
//
// YAML schema:
//
//	health_port: 8080          # optional — /health port for agenthubctl status
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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
	fleetpkg "github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/fleet"
	"github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/tmux"
)

// Type aliases so the rest of package main continues using the original names.
type PersonaConfig = fleetpkg.PersonaConfig
type FleetConfig = fleetpkg.FleetConfig
type yamlDuration = fleetpkg.YAMLDuration

// LoadFleetConfig delegates to the internal fleet package.
func LoadFleetConfig(path string) (*FleetConfig, error) {
	return fleetpkg.LoadFleetConfig(path)
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
				health.RecordError("@"+cfg.User, err.Error())
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

	// SSE ストリームを開いてサーバー ping に自動応答する (issue #41)
	if err := client.StartSSE(ctx); err != nil {
		return fmt.Errorf("StartSSE: %w", err)
	}
	defer client.StopSSE()

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

		// SSE goroutine を先に停止してから sleep → re-initialize → re-register → re-StartSSE
		client.StopSSE()
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
		if err := client.StartSSE(ctx); err != nil {
			slog.Warn("fleet persona re-StartSSE failed", "handle", selfHandle, "err", err)
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
