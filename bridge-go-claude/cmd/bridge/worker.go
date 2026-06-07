// worker.go — Bridge worker main loop (Python: worker.py の直訳)
//
// runWorker: bridge のメインエントリポイント。cursor / journal / tracker / gap_tracker を
//   初期化して runHubSession を reconnect ループで回す。
//   claudeRunner は状態を持たない (on-demand) ため reconnect をまたいで単一インスタンスを共有する。
//   SIGTERM 受信時は runShutdownCompact() で compact してから終了する (issue #178)。
//
// runHubSession: 1 回ぶんの hub session を最後まで走らせる。
//   journal replay → startup catchup → polling loop (CommandRouter + handleOne)
//
// startupCatchup: bridge 起動時に未読メッセージを処理する (issue #98)。
//
// handleOne: message 1 件を Claude に流して応答を待つ。
//   claude subprocess は on-demand で spawn/exit する。
//
// journalledSend: journal write → hub.SendMessage → journal delete の順で送信を永続化する。
//
// replayJournal: 起動時に pending journal entries を replay する (issue #183)。
//
// runShutdownCompact: SIGTERM 時に /compact を実行してから exit する (issue #178)。
//   idle compact watchdog は on-demand bridge では不要なため削除済み (issue #179)。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// shutdownCompactTimeout は SIGTERM 時の compact に与える最大時間。
const shutdownCompactTimeout = 5 * time.Minute

const (
	defaultReconnectBackoffS = 5.0
	maxRetriesEnv            = "AGENT_HUB_BRIDGE_MAX_RETRIES"
	defaultMaxRetries        = 10
)

// runWorker はブリッジの outer loop。
// Python の run_worker() + run_with_reconnect() に相当。
// cursor / journal / tracker / gap_tracker を
// outer loop をまたいで共有する (= reconnect 後も状態を持ち越す)。
//
// claudeRunner は on-demand モードのため状態を持たず、
// runnerHolder ではなく単一インスタンスを複数 hub session をまたいで再利用する。
//
// SIGTERM 受信時は runShutdownCompact() で compact してから終了する (issue #178)。
// idle compact watchdog は on-demand bridge では不要なため削除済み (issue #179)。
func runWorker(ctx context.Context, cfg *config, mcpConfigPath string) {
	// reconnect をまたいで共有する state
	cursor := loadCursor(cfg.User)
	journal := newJournal(cfg.User)
	tracker := &activityTracker{}
	gapTracker := &messageGapTracker{}

	// on-demand モード: runner は状態を持たないため単一インスタンスを使い回す。
	// Python の ClaudeSDKClient と違い、subprocess はフィールドに保持しない。
	runner := newClaudeRunner(cfg, mcpConfigPath)

	// circuit breaker (issue #82)
	maxRetries := cfg.MaxRetries // 0 = unlimited
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			slog.Info("runWorker: shutting down")
			// issue #178: SIGTERM 時に compact してから exit
			runShutdownCompact(runner, cfg)
			return
		default:
		}

		// hub セッション開始
		newCursor, err := runHubSession(
			ctx, cfg, mcpConfigPath,
			runner, cursor, tracker, gapTracker, journal,
		)
		cursor = newCursor // セッション終了時点の cursor を引き継ぐ

		if ctx.Err() != nil {
			slog.Info("runWorker: context cancelled, shutting down")
			// issue #178: SIGTERM 時に compact してから exit
			runShutdownCompact(runner, cfg)
			return
		}

		if err != nil {
			consecutiveFailures++
			slog.Warn("runWorker: hub session ended with error",
				"err", err,
				"consecutive_failures", consecutiveFailures,
			)

			// circuit breaker
			if maxRetries > 0 && consecutiveFailures >= maxRetries {
				slog.Error("[circuit-breaker] ALERT: hub connection assumed lost",
					"user", cfg.User,
					"consecutive_failures", consecutiveFailures,
					"max_retries", maxRetries,
				)
				// dead marker + inventory 通知 (issue #82)
				writeDeadMarker(cfg.User)
				writeLostHubToInventory(cfg.User, os.Getpid())
				slog.Error("[circuit-breaker] dead marker written — run stop-bridge.sh --dead to clean up")
				return
			}
		} else {
			consecutiveFailures = 0
		}

		slog.Info("runWorker: reconnecting",
			"backoff_s", cfg.ReconnectBackoff.Seconds(),
			"consecutive_failures", consecutiveFailures,
		)
		sleepWithContext(ctx, cfg.ReconnectBackoff)
	}
}

// runHubSession は 1 回ぶんの hub session を最後まで走らせる。
// Python の _run_hub_session() に相当。
// エラーが発生した場合は cursor とエラーを返す。
func runHubSession(
	ctx context.Context,
	cfg *config,
	mcpConfigPath string,
	runner *claudeRunner,
	cursor string,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
) (string, error) {
	// --- hub client 初期化 ---
	client, err := agenthub.New(
		cfg.AgentHubURL, cfg.GitHubPAT, cfg.User, cfg.Tenant,
		agenthub.WithClientName("bridge-go-claude"),
	)
	if err != nil {
		return cursor, fmt.Errorf("agenthub.New: %w", err)
	}
	if err := client.Initialize(ctx); err != nil {
		return cursor, fmt.Errorf("initialize: %w", err)
	}
	if _, err := client.Register(ctx, cfg.DisplayName, cfg.Mode); err != nil {
		return cursor, fmt.Errorf("register: %w", err)
	}
	// SSE keepalive: claude subprocess 実行中の MCP セッション expire を防ぐ (issue #41)
	if err := client.StartSSE(ctx); err != nil {
		return cursor, fmt.Errorf("start SSE: %w", err)
	}
	defer client.StopSSE()

	slog.Info("runHubSession: registered and listening",
		"handle", "@"+cfg.User,
		"mode", cfg.Mode,
		"display_name", cfg.DisplayName,
	)

	// CommandRouter を生成 (Python の router = CommandRouter() に相当)
	// SDK の CommandRouter を使う (issue #43)。
	// on-demand モードでは /restart は no-op になる。
	router := agenthub.NewCommandRouter()
	router.SetStatusFunc(tracker.status)
	router.SetRestartHandler(func(ctx context.Context) error {
		return runner.restart(ctx)
	})

	// journal replay: 前回クラッシュ時の pending entries を再送 (issue #183)
	replayJournal(ctx, client, journal)

	// startup catchup: bridge 起動時に未読メッセージを処理する (issue #98)
	cursor, err = startupCatchup(
		ctx, cfg, client,
		runner, cursor,
		tracker, gapTracker, journal,
	)
	if err != nil {
		slog.Warn("runHubSession: startup catchup error (continuing)", "err", err)
	}

	selfHandle := "@" + cfg.User

	// --- メインポーリングループ ---
	for {
		select {
		case <-ctx.Done():
			return cursor, ctx.Err()
		default:
		}

		msgs, err := client.GetMessages(ctx)
		if err != nil {
			slog.Warn("runHubSession: get_messages error", "err", err)
			return cursor, fmt.Errorf("get_messages: %w", err)
		}

		for _, msg := range msgs {
			// 自己ループ防止
			if msg.Sender == selfHandle {
				slog.Debug("runHubSession: skip self-sent message", "msg_id", msg.ID)
				_ = client.MarkAsRead(ctx, msg.ID)
				continue
			}

			// スラッシュコマンドを CommandRouter で処理 (MarkAsRead は Handle 内部で呼ばれる)
			if router.Handle(ctx, client, msg) {
				continue
			}

			// issue #26: safety-net 発火推定 (gap 計測)
			gapTracker.onMessageReceived(msg.ID)

			// issue #37: cursor skip — 再起動後の重複 dispatch 防止
			if cursor != "" && msg.Timestamp <= cursor {
				slog.Info("runHubSession: skipping already-seen message",
					"msg_id", msg.ID, "ts", msg.Timestamp, "cursor", cursor)
				_ = client.MarkAsRead(ctx, msg.ID)
				continue
			}

			// issue #176: MarkAsRead を handleOne 前に呼ぶ。
			// polling bridge では処理前に MarkAsRead しないと次回 GetMessages で
			// 同一メッセージが返ってきて二重 dispatch が発生する。
			// cursor check が secondary guard として機能するが、in-memory cursor は
			// reconnect でリセットされるため、server-side の既読状態を先に確定させる。
			if err := client.MarkAsRead(ctx, msg.ID); err != nil {
				slog.Warn("runHubSession: pre-process mark_as_read failed; cursor will guard on retry",
					"msg_id", msg.ID, "err", err)
			}

			handleErr := handleOne(ctx, client, runner, msg, cfg, tracker, journal)
			if handleErr != nil {
				slog.Error("runHubSession: handleOne error", "msg_id", msg.ID, "err", handleErr)
			}

			// issue #37, #176: process → save_cursor の順 (crash-safe secondary guard)。
			// MarkAsRead は上記で処理前に呼び済み。
			saveCursor(cfg.User, msg.Timestamp)
			cursor = msg.Timestamp
		}

		sleepWithContext(ctx, cfg.PollInterval)
	}
}

// startupCatchup は bridge 起動時に未読メッセージを処理する (issue #98)。
// hub 接続確立後・polling ループ開始前に GetMessages を呼んで
// オフライン中に届いたメッセージを処理する。
// コマンドメッセージ (body が "/" で始まる) は polling ループの CommandRouter に委ねるためスキップ。
func startupCatchup(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	runner *claudeRunner,
	cursor string,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
) (string, error) {
	msgs, err := client.GetMessages(ctx)
	if err != nil {
		slog.Warn("[startup-catchup] get_messages failed; skipping", "err", err)
		return cursor, nil // graceful degradation
	}

	// コマンドメッセージを分離
	var nlMsgs []agenthub.Message
	cmdCount := 0
	for _, m := range msgs {
		if len(m.Body) > 0 && m.Body[0] == '/' {
			cmdCount++
			continue
		}
		nlMsgs = append(nlMsgs, m)
	}

	if len(nlMsgs) == 0 {
		if cmdCount > 0 {
			slog.Info("[startup-catchup] command messages only; deferred to polling loop",
				"cmd_count", cmdCount)
		} else {
			slog.Info("[startup-catchup] no unread messages at startup")
		}
		return cursor, nil
	}

	slog.Info("[startup-catchup] processing unread messages",
		"nl_count", len(nlMsgs), "cmd_count", cmdCount)

	selfHandle := "@" + cfg.User

	for _, msg := range nlMsgs {
		// 自己ループ防止
		if msg.Sender == selfHandle {
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		// cursor skip (issue #37)
		if cursor != "" && msg.Timestamp <= cursor {
			slog.Info("[startup-catchup] skipping seen message",
				"msg_id", msg.ID, "ts", msg.Timestamp, "cursor", cursor)
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		gapTracker.onMessageReceived(msg.ID)

		// issue #176: MarkAsRead を handleOne 前に呼ぶ。
		// polling bridge では処理前に MarkAsRead しないと次回 GetMessages で
		// 同一メッセージが返ってきて二重 dispatch が発生する。
		// cursor check が secondary guard として機能するが、in-memory cursor は
		// reconnect でリセットされるため、server-side の既読状態を先に確定させる。
		if err := client.MarkAsRead(ctx, msg.ID); err != nil {
			slog.Warn("[startup-catchup] pre-process mark_as_read failed; cursor will guard on retry",
				"msg_id", msg.ID, "err", err)
		}

		handleErr := handleOne(ctx, client, runner, msg, cfg, tracker, journal)
		if handleErr != nil {
			slog.Error("[startup-catchup] handleOne error", "msg_id", msg.ID, "err", handleErr)
		}

		// issue #37, #176: process → save_cursor の順 (crash-safe secondary guard)。
		// MarkAsRead は上記で処理前に呼び済み。
		saveCursor(cfg.User, msg.Timestamp)
		cursor = msg.Timestamp
	}

	return cursor, nil
}

// handleOne は message 1 件を Claude に流して応答を待つ。
// Python の _handle_one() に相当。
// hub.MarkAsRead は caller が handleOne 呼び出し前に担当する (issue #176)。
// claude subprocess は on-demand で spawn/exit される (runner.query 内部で処理)。
func handleOne(
	ctx context.Context,
	client *agenthub.Client,
	runner *claudeRunner,
	msg agenthub.Message,
	cfg *config,
	tracker *activityTracker,
	journal *Journal,
) error {
	// issue #51: workdir が存在しない場合は early return
	if _, err := os.Stat(cfg.Workdir); err != nil {
		slog.Error("handleOne: workdir gone",
			"workdir", cfg.Workdir, "msg_id", msg.ID)
		errMsg := fmt.Sprintf("(自動応答) bridge の workdir が存在しません: %s", cfg.Workdir)
		_ = journalledSend(ctx, client, journal, msg.Sender, errMsg, msg.ID)
		return nil // caller が MarkAsRead する
	}

	slog.Info("← message",
		"msg_id", msg.ID, "from", msg.Sender, "body_preview", truncate(msg.Body, 120))

	prompt := formatPrompt("@"+cfg.User, msg)
	if err := runner.query(ctx, prompt, msg.Sender, tracker); err != nil {
		slog.Error("handleOne: claude query error", "msg_id", msg.ID, "err", err)
		errMsg := fmt.Sprintf("(auto) bridge-go-claude error: %v", err)
		_ = journalledSend(ctx, client, journal, msg.Sender, errMsg, msg.ID)
		return err
	}

	slog.Info("→ message processed", "msg_id", msg.ID, "from", msg.Sender)
	return nil
}

// journalledSend は journal write → hub.SendMessage → journal delete の順で送信を永続化する。
// Python の _journalled_send() に相当。
// hub.SendMessage が失敗した場合、entry は journal に残り次回起動時に replay される。
func journalledSend(
	ctx context.Context,
	client *agenthub.Client,
	journal *Journal,
	to, message, causedBy string,
) error {
	entry := journal.makeEntry(to, message, causedBy)
	// write → send → delete の順。write 失敗時は send を中止する (reviewer Critical: issue #183)。
	if !journal.write(entry) {
		return fmt.Errorf("journal write failed for entry %s (to=%s); send aborted", entry.ID, to)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.SendMessage(sendCtx, to, message, causedBy); err != nil {
		slog.Warn("journalledSend: hub.SendMessage failed; entry kept for replay",
			"entry_id", entry.ID, "to", to, "err", err)
		return err
	}
	journal.delete(entry.ID)
	return nil
}

// replayJournal は起動時に pending journal entries を replay する (issue #183)。
// bridge クラッシュ時に送信できなかったメッセージを再送する。
func replayJournal(ctx context.Context, client *agenthub.Client, journal *Journal) {
	entries := journal.loadAll()
	if len(entries) == 0 {
		return
	}
	slog.Warn("replayJournal: pending entries found — replaying",
		"count", len(entries))
	for _, entry := range entries {
		slog.Info("replayJournal: replaying entry",
			"id", entry.ID, "to", entry.To, "created_at", entry.CreatedAt)
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := client.SendMessage(sendCtx, entry.To, entry.Message, entry.CausedBy)
		cancel()
		if err != nil {
			slog.Error("replayJournal: failed to replay entry; will retry on next startup",
				"id", entry.ID, "to", entry.To, "err", err)
			continue
		}
		journal.delete(entry.ID)
		slog.Info("replayJournal: entry replayed successfully", "id", entry.ID)
	}
}

// runShutdownCompact は SIGTERM 受信時に /compact を実行する (issue #178)。
// cancelled context ではなく context.Background() + shutdownCompactTimeout で実行する。
// compact 失敗は WARN ログのみ (bridge の終了を妨げない)。
func runShutdownCompact(runner *claudeRunner, cfg *config) {
	slog.Info("[shutdown-compact] SIGTERM received — running /compact before exit")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownCompactTimeout)
	defer cancel()
	summary, err := runner.compact(ctx)
	if err != nil {
		slog.Warn("[shutdown-compact] /compact failed", "err", err)
		return
	}
	slog.Info("[shutdown-compact] /compact completed")
	if archiveDir := compactArchiveDirFor(cfg.Workdir); archiveDir != "" {
		appendCompactSummary(summary, archiveDir)
	}
}
