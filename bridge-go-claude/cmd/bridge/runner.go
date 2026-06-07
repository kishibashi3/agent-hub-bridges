// runner.go — claudeRunner (on-demand subprocess モード)
//
// Python の ClaudeRunner + ClaudeSDKClient と同じ役割を担うが、
// subprocess の寿命は 1 query/compact に限定する (on-demand)。
//
// Python bridge との構造的な違いはここだけ:
//   Python: ClaudeSDKClient が subprocess を alive に保ち複数 query を流す (persistent)
//   Go:     query()/compact() ごとに spawn して完了後に exit させる (on-demand)
//
// それ以外 (agent-hub 接続 / reconnect / journal / cursor / commands / compact watchdog) は
// worker.py と同じ構造を worker.go で維持する。
//
// session_id を per-sender に設定することで Claude が自身のセッションストレージから
// per-peer 会話コンテキストを復元できる (= Python bridge と同等の continuity)。
//
// blocking command 阻止 (issue #101):
// stdout の stream-json で tool_use{Bash} を検出後、stdin に tool_result deny を inject する。
// stdin は result イベント到達まで開放しておくことで inject を可能にする。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// streamAssistantContent は stream-json の assistant メッセージ内の content block。
// tool_use の blocking 検出に使う。
type streamAssistantContent struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
	Text  string          `json:"text,omitempty"` // text block 用 (compact サマリー収集)
}

// streamAssistantMessage は stream-json の assistant イベントの message フィールド。
type streamAssistantMessage struct {
	Role    string                   `json:"role"`
	Content []streamAssistantContent `json:"content"`
}

// streamEvent は stream-json の 1 行イベント (extended 版)。
type streamEvent struct {
	Type    string                  `json:"type"`
	Subtype string                  `json:"subtype"`
	IsError bool                    `json:"is_error"`
	Result  string                  `json:"result"`
	Message *streamAssistantMessage `json:"message,omitempty"`
}

// claudeRunner は Claude CLI subprocess の設定を保持する。
// on-demand モードでは subprocess はフィールドとして保持せず、
// query()/compact() が呼ばれるたびに新しい subprocess を spawn する。
// 状態を持たないため、複数の hub session をまたいで単一インスタンスを共有できる。
type claudeRunner struct {
	cfg           *config
	mcpConfigPath string
}

// newClaudeRunner は claudeRunner を生成する。
func newClaudeRunner(cfg *config, mcpConfigPath string) *claudeRunner {
	return &claudeRunner{
		cfg:           cfg,
		mcpConfigPath: mcpConfigPath,
	}
}

// restart は on-demand モードでは no-op。
// CommandRouter の /restart ハンドラから呼ばれる。
// on-demand では「再起動すべき持続 subprocess」が存在しないため何もしない。
// 会話の継続性は Claude 自身のセッションストレージが担う。
func (r *claudeRunner) restart(_ context.Context) error {
	slog.Info("runner: /restart — on-demand mode, no persistent subprocess to restart")
	return nil
}

// query は user prompt を Claude に送り、result イベントまで待つ。
// メッセージ受信ごとに新しい subprocess を spawn し、完了後に終了させる (on-demand)。
// Python の ClaudeSDKClient.query(prompt, session_id=msg.sender) に相当。
func (r *claudeRunner) query(ctx context.Context, prompt, sessionID string, tracker *activityTracker) error {
	// SubprocessTimeout の適用
	queryCtx := ctx
	if r.cfg.SubprocessTimeout > 0 {
		var cancel context.CancelFunc
		queryCtx, cancel = context.WithTimeout(ctx, r.cfg.SubprocessTimeout)
		defer cancel()
	}

	cmd, stdinPipe, scanner, err := r.spawnSubprocess(queryCtx)
	if err != nil {
		return err
	}
	pid := cmd.Process.Pid
	slog.Info("runner: Claude subprocess started (on-demand)", "pid", pid, "session_id", sessionID)

	// initialize control_request (Python SDK: query.py と同一フォーマット)
	reqID := fmt.Sprintf("req_1_%d", time.Now().UnixNano())
	if err := writeJSON(stdinPipe, map[string]any{
		"type":       "control_request",
		"request_id": reqID,
		"request":    map[string]any{"subtype": "initialize"},
	}); err != nil {
		stdinPipe.Close()
		_ = cmd.Wait()
		return fmt.Errorf("runner: write initialize: %w", err)
	}

	// user message (Python SDK: client.py と同一フォーマット)
	if err := writeJSON(stdinPipe, map[string]any{
		"type":               "user",
		"session_id":         sessionID,
		"message":            map[string]any{"role": "user", "content": prompt},
		"parent_tool_use_id": nil,
	}); err != nil {
		stdinPipe.Close()
		_ = cmd.Wait()
		return fmt.Errorf("runner: write user message: %w", err)
	}

	// stdin は result イベントまで開放する (blocking command deny inject のため)
	resultErr := readUntilResult(queryCtx, scanner, stdinPipe, tracker, false)

	// EOF を送って subprocess を自然終了させる
	stdinPipe.Close()
	if err := cmd.Wait(); err != nil {
		slog.Debug("runner: subprocess exited", "pid", pid, "err", err)
	}
	slog.Info("runner: Claude subprocess finished (on-demand)", "pid", pid)
	return resultErr
}

// compact は /compact を Claude に送り、サマリーテキストを収集して返す。
// Python の IdleCompactWatchdog._run_compact_and_archive 内の client.query("/compact") に相当。
// on-demand: 専用の subprocess を spawn する。
func (r *claudeRunner) compact(ctx context.Context) (string, error) {
	cmd, stdinPipe, scanner, err := r.spawnSubprocess(ctx)
	if err != nil {
		return "", err
	}
	pid := cmd.Process.Pid
	slog.Info("runner: compact subprocess started (on-demand)", "pid", pid)

	reqID := fmt.Sprintf("req_compact_%d", time.Now().UnixNano())
	if err := writeJSON(stdinPipe, map[string]any{
		"type":       "control_request",
		"request_id": reqID,
		"request":    map[string]any{"subtype": "initialize"},
	}); err != nil {
		stdinPipe.Close()
		_ = cmd.Wait()
		return "", fmt.Errorf("runner: compact write initialize: %w", err)
	}
	if err := writeJSON(stdinPipe, map[string]any{
		"type":               "user",
		"session_id":         "_compact_",
		"message":            map[string]any{"role": "user", "content": "/compact"},
		"parent_tool_use_id": nil,
	}); err != nil {
		stdinPipe.Close()
		_ = cmd.Wait()
		return "", fmt.Errorf("runner: compact write message: %w", err)
	}
	// compact は tool_result inject が不要なので stdin を即時 close する
	stdinPipe.Close()

	summary, resultErr := readUntilResultWithSummary(ctx, scanner)
	if err := cmd.Wait(); err != nil {
		slog.Debug("runner: compact subprocess exited", "pid", pid, "err", err)
	}
	slog.Info("runner: compact subprocess finished (on-demand)", "pid", pid)
	return summary, resultErr
}

// spawnSubprocess は claude subprocess を起動して stdin/stdout を返す。
// 呼び出し元が stdinPipe.Close() と cmd.Wait() を担当する。
func (r *claudeRunner) spawnSubprocess(ctx context.Context) (*exec.Cmd, io.WriteCloser, *bufio.Scanner, error) {
	args := r.buildArgs()
	cmd := exec.CommandContext(ctx, r.cfg.ClaudeCLI, args...)
	cmd.Dir = r.cfg.Workdir
	cmd.Stderr = os.Stderr

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("runner: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdinPipe.Close()
		return nil, nil, nil, fmt.Errorf("runner: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdinPipe.Close()
		return nil, nil, nil, fmt.Errorf("runner: start subprocess: %w", err)
	}
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 512*1024), 512*1024) // 512 KB
	return cmd, stdinPipe, scanner, nil
}

// writeJSON は v を JSON としてエンコードして w に書き込む。
func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("json marshal: %w", err)
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// readUntilResult は stdout の stream-json を読んで result イベントまで待つ。
// stdinWriter: blocking command deny inject に使う (result 受信まで開放しておくこと)。
// isCompact=true のとき blocking command 検出をスキップする。
func readUntilResult(ctx context.Context, scanner *bufio.Scanner, stdinWriter io.Writer, tracker *activityTracker, isCompact bool) error {
	var resultErr error

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			slog.Debug("runner: stream-json parse skip", "line", truncate(line, 120))
			continue
		}

		slog.Debug("runner: stream-json event", "type", ev.Type, "subtype", ev.Subtype)

		if ev.Type == "assistant" {
			// activity tracking: Claude が応答を生成中
			if tracker != nil {
				tracker.markActive()
			}
			// blocking command 検出 (issue #101)
			if !isCompact && ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "tool_use" && block.Name == "Bash" {
						var input struct {
							Command string `json:"command"`
						}
						if err := json.Unmarshal(block.Input, &input); err == nil && input.Command != "" {
							if pattern := checkBlockingCommand(input.Command); pattern != "" {
								slog.Warn("[blocking-cmd] blocking command detected, injecting deny",
									"pattern", pattern,
									"command_preview", truncate(input.Command, 120),
									"tool_use_id", block.ID,
								)
								denyMsg := buildBlockingErrorMessage(pattern)
								if err := writeJSON(stdinWriter, map[string]any{
									"type":        "tool_result",
									"tool_use_id": block.ID,
									"content":     denyMsg,
									"is_error":    true,
								}); err != nil {
									slog.Warn("[blocking-cmd] failed to inject tool_result deny", "err", err)
								}
							}
						}
					}
				}
			}
		}

		if ev.Type == "result" {
			if ev.IsError {
				resultErr = fmt.Errorf("claude result error (subtype=%s): %s",
					ev.Subtype, truncate(ev.Result, 300))
			}
			break
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		slog.Warn("runner: stream-json scan error", "err", scanErr)
	}
	return resultErr
}

// readUntilResultWithSummary は compact 用: assistant の text block からサマリーを収集する。
func readUntilResultWithSummary(ctx context.Context, scanner *bufio.Scanner) (string, error) {
	var summaryParts []string
	var resultErr error

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}

		if ev.Type == "assistant" && ev.Message != nil {
			for _, block := range ev.Message.Content {
				if block.Type == "text" && block.Text != "" {
					summaryParts = append(summaryParts, block.Text)
				}
			}
		}

		if ev.Type == "result" {
			if ev.IsError {
				resultErr = fmt.Errorf("compact result error (subtype=%s): %s",
					ev.Subtype, truncate(ev.Result, 300))
			}
			break
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		slog.Warn("runner: compact scan error", "err", scanErr)
	}
	return strings.Join(summaryParts, "\n\n"), resultErr
}

// buildArgs は claude CLI の引数リストを組み立てる。
func (r *claudeRunner) buildArgs() []string {
	args := []string{
		"--output-format", "stream-json",
		"--verbose",
		"--input-format", "stream-json",
		"--permission-mode", "bypassPermissions",
		"--mcp-config", r.mcpConfigPath,
	}
	if r.cfg.Model != "" {
		args = append(args, "--model", r.cfg.Model)
	}
	for _, dir := range r.cfg.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	return args
}
