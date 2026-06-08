// worker.go — Bridge worker main loop
//
// bridge-claude2 の worker.go に相当するが、compact ステップを省略している。
// codex CLI には /compact 相当の機能がないため、runGracefulDrain では
// compact をスキップして未処理メッセージの drain のみ実施する。
//
// runWorker: bridge のメインエントリポイント。cursor / journal / tracker / gap_tracker を
//   初期化して runHubSession を reconnect ループで回す。
//   codexRunner は状態を持つ (hasSession) ため、reconnect をまたいで単一インスタンスを共有する。
//
// runHubSession: 1 回ぶんの hub session を最後まで走らせる。
//   journal replay → startup catchup → polling loop (CommandRouter + handleOne)
//   SIGTERM 受信時は polling loop 内で runGracefulDrain() を呼んでから exit する。
//
// startupCatchup: bridge 起動時に未読メッセージを処理する。
//
// handleOne: message 1 件を codex に流して応答を待つ。
//   codex subprocess は on-demand で spawn/exit する。
//
// journalledSend: journal write → hub.SendMessage → journal delete の順で送信を永続化する。
//
// replayJournal: 起動時に pending journal entries を replay する。
//
// runGracefulDrain: SIGTERM 時の graceful drain。
//   compact なし → 未処理メッセージ確認 → メッセージがあれば処理 → exit。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// runWorker はブリッジの outer loop。
// cursor / journal / tracker / gap_tracker を outer loop をまたいで共有する。
// codexRunner は hasSession 状態を持つため reconnect をまたいで単一インスタンスを共有する。
func runWorker(ctx context.Context, cfg *config) {
	cursor := loadCursor(cfg.User)
	journal := newJournal(cfg.User)
	tracker := &activityTracker{}
	gapTracker := &messageGapTracker{}
	runner := newCodexRunner(cfg)

	maxRetries := cfg.MaxRetries
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			slog.Info("runWorker: shutting down (no active session)")
			return
		default:
		}

		newCursor, established, err := runHubSession(
			ctx, cfg,
			runner, cursor, tracker, gapTracker, journal,
		)
		cursor = newCursor

		if ctx.Err() != nil {
			slog.Info("runWorker: context cancelled, shutting down")
			return
		}

		if err != nil {
			if established {
				consecutiveFailures = 0
			}
			consecutiveFailures++
			slog.Warn("runWorker: hub session ended with error",
				"err", err,
				"consecutive_failures", consecutiveFailures,
			)

			if maxRetries > 0 && consecutiveFailures >= maxRetries {
				slog.Error("[circuit-breaker] ALERT: hub connection assumed lost",
					"user", cfg.User,
					"consecutive_failures", consecutiveFailures,
					"max_retries", maxRetries,
				)
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
func runHubSession(
	ctx context.Context,
	cfg *config,
	runner *codexRunner,
	cursor string,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
) (string, bool, error) {
	client, err := agenthub.New(
		cfg.AgentHubURL, cfg.GitHubPAT, cfg.User, cfg.Tenant,
		agenthub.WithClientName("bridge-codex2"),
	)
	if err != nil {
		return cursor, false, fmt.Errorf("agenthub.New: %w", err)
	}
	if err := client.Initialize(ctx); err != nil {
		return cursor, false, fmt.Errorf("initialize: %w", err)
	}
	if _, err := client.Register(ctx, cfg.DisplayName, cfg.Mode); err != nil {
		return cursor, false, fmt.Errorf("register: %w", err)
	}
	if err := client.StartSSE(ctx); err != nil {
		return cursor, false, fmt.Errorf("start SSE: %w", err)
	}
	defer client.StopSSE()

	slog.Info("runHubSession: registered and listening",
		"handle", "@"+cfg.User,
		"mode", cfg.Mode,
		"display_name", cfg.DisplayName,
	)

	router := agenthub.NewCommandRouter()
	router.SetStatusFunc(tracker.status)
	router.SetRestartHandler(func(ctx context.Context) error {
		return runner.restart(ctx)
	})

	replayJournal(ctx, client, journal)

	cursor, err = startupCatchup(
		ctx, cfg, client,
		runner, cursor,
		tracker, gapTracker, journal,
	)
	if err != nil {
		slog.Warn("runHubSession: startup catchup error (continuing)", "err", err)
	}

	selfHandle := "@" + cfg.User

	for {
		select {
		case <-ctx.Done():
			runGracefulDrain(client, runner, cfg, cursor, tracker, journal, selfHandle)
			return cursor, true, ctx.Err()
		default:
		}

		msgs, err := client.GetMessages(ctx)
		if err != nil {
			slog.Warn("runHubSession: get_messages error", "err", err)
			return cursor, true, fmt.Errorf("get_messages: %w", err)
		}

		for _, msg := range msgs {
			if msg.Sender == selfHandle {
				slog.Debug("runHubSession: skip self-sent message", "msg_id", msg.ID)
				// best-effort: 失敗は致命的でなく、次回 polling で再試行される
				_ = client.MarkAsRead(ctx, msg.ID)
				continue
			}

			if router.Handle(ctx, client, msg) {
				continue
			}

			gapTracker.onMessageReceived(msg.ID)

			if cursor != "" && msg.Timestamp <= cursor {
				slog.Info("runHubSession: skipping already-seen message",
					"msg_id", msg.ID, "ts", msg.Timestamp, "cursor", cursor)
				// best-effort: 失敗は致命的でなく、次回 polling で再試行される
				_ = client.MarkAsRead(ctx, msg.ID)
				continue
			}

			// issue #176: MarkAsRead を handleOne 前に呼ぶ
			if err := client.MarkAsRead(ctx, msg.ID); err != nil {
				slog.Warn("runHubSession: pre-process mark_as_read failed; cursor will guard on retry",
					"msg_id", msg.ID, "err", err)
			}

			handleErr := handleOne(ctx, client, runner, msg, cfg, tracker, journal)
			if handleErr != nil {
				slog.Error("runHubSession: handleOne error", "msg_id", msg.ID, "err", handleErr)
			}

			saveCursor(cfg.User, msg.Timestamp)
			cursor = msg.Timestamp
		}

		sleepWithContext(ctx, cfg.PollInterval)
	}
}

// startupCatchup は bridge 起動時に未読メッセージを処理する。
func startupCatchup(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	runner *codexRunner,
	cursor string,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
) (string, error) {
	msgs, err := client.GetMessages(ctx)
	if err != nil {
		slog.Warn("[startup-catchup] get_messages failed; skipping", "err", err)
		return cursor, nil
	}

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
		if msg.Sender == selfHandle {
			// best-effort: 失敗は致命的でなく、次回 polling で再試行される
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		if cursor != "" && msg.Timestamp <= cursor {
			slog.Info("[startup-catchup] skipping seen message",
				"msg_id", msg.ID, "ts", msg.Timestamp, "cursor", cursor)
			// best-effort: 失敗は致命的でなく、次回 polling で再試行される
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		gapTracker.onMessageReceived(msg.ID)

		if err := client.MarkAsRead(ctx, msg.ID); err != nil {
			slog.Warn("[startup-catchup] pre-process mark_as_read failed; cursor will guard on retry",
				"msg_id", msg.ID, "err", err)
		}

		handleErr := handleOne(ctx, client, runner, msg, cfg, tracker, journal)
		if handleErr != nil {
			slog.Error("[startup-catchup] handleOne error", "msg_id", msg.ID, "err", handleErr)
		}

		saveCursor(cfg.User, msg.Timestamp)
		cursor = msg.Timestamp
	}

	return cursor, nil
}

// handleOne は message 1 件を codex に流して応答を待つ。
func handleOne(
	ctx context.Context,
	client *agenthub.Client,
	runner *codexRunner,
	msg agenthub.Message,
	cfg *config,
	tracker *activityTracker,
	journal *Journal,
) error {
	if _, err := os.Stat(cfg.Workdir); err != nil {
		slog.Error("handleOne: workdir gone",
			"workdir", cfg.Workdir, "msg_id", msg.ID)
		errMsg := fmt.Sprintf("(自動応答) bridge の workdir が存在しません: %s", cfg.Workdir)
		_ = journalledSend(ctx, client, journal, msg.Sender, errMsg, msg.ID)
		return nil
	}

	slog.Info("← message",
		"msg_id", msg.ID, "from", msg.Sender, "body_preview", truncate(msg.Body, 120))

	prompt := formatPrompt("@"+cfg.User, msg)
	usage, err := runner.query(ctx, prompt, msg.Sender, tracker)
	emitSpan(msg.ID, cfg.Model, usage)
	if err != nil {
		slog.Error("handleOne: codex query error", "msg_id", msg.ID, "err", err)
		errMsg := fmt.Sprintf("(auto) bridge-codex2 error: %v", err)
		_ = journalledSend(ctx, client, journal, msg.Sender, errMsg, msg.ID)
		return err
	}

	slog.Info("→ message processed", "msg_id", msg.ID, "from", msg.Sender)
	return nil
}

// journalledSend は journal write → hub.SendMessage → journal delete の順で送信を永続化する。
func journalledSend(
	ctx context.Context,
	client *agenthub.Client,
	journal *Journal,
	to, message, causedBy string,
) error {
	entry := journal.makeEntry(to, message, causedBy)
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

// replayJournal は起動時に pending journal entries を replay する。
func replayJournal(ctx context.Context, client *agenthub.Client, journal *Journal) {
	entries := journal.loadAll()
	if len(entries) == 0 {
		return
	}
	slog.Warn("replayJournal: pending entries found — replaying", "count", len(entries))
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

// runGracefulDrain は SIGTERM 受信後の graceful drain を実行する。
//
// bridge-claude2 の runGracefulDrain と異なり compact ステップを省略する。
// codex CLI には /compact 相当の機能がないため。
//
// フロー:
//  1. GetMessages で未処理メッセージを確認
//  2. メッセージがあれば処理してから exit
//  3. メッセージがなければそのまま exit
func runGracefulDrain(
	client *agenthub.Client,
	runner *codexRunner,
	cfg *config,
	cursor string,
	tracker *activityTracker,
	journal *Journal,
	selfHandle string,
) {
	drainTimeout := cfg.SubprocessTimeout + time.Minute
	if drainTimeout <= time.Minute {
		drainTimeout = 6 * time.Minute // 最小バッファ
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	slog.Info("[drain] graceful drain started (no compact — codex bridge)",
		"drain_timeout_s", drainTimeout.Seconds(),
	)

	msgs, err := client.GetMessages(drainCtx)
	if err != nil {
		slog.Warn("[drain] final get_messages failed", "err", err)
		return
	}

	var pending []agenthub.Message
	for _, m := range msgs {
		if m.Sender == selfHandle {
			// best-effort: 失敗は致命的でなく、次回 polling で再試行される
			_ = client.MarkAsRead(drainCtx, m.ID)
			continue
		}
		if cursor != "" && m.Timestamp <= cursor {
			// best-effort: 失敗は致命的でなく、次回 polling で再試行される
			_ = client.MarkAsRead(drainCtx, m.ID)
			continue
		}
		if len(m.Body) > 0 && m.Body[0] == '/' {
			slog.Info("[drain] skipping command message during shutdown (留保 → 次回起動で処理)", "msg_id", m.ID)
			continue
		}
		pending = append(pending, m)
	}

	if len(pending) == 0 {
		slog.Info("[drain] no pending messages, exiting cleanly")
		return
	}

	slog.Info("[drain] processing pending messages", "count", len(pending))
	for _, msg := range pending {
		if err := client.MarkAsRead(drainCtx, msg.ID); err != nil {
			slog.Warn("[drain] mark_as_read failed", "msg_id", msg.ID, "err", err)
		}
		if err := handleOne(drainCtx, client, runner, msg, cfg, tracker, journal); err != nil {
			slog.Error("[drain] handleOne error", "msg_id", msg.ID, "err", err)
		}
		saveCursor(cfg.User, msg.Timestamp)
	}
	slog.Info("[drain] graceful drain completed")
}
