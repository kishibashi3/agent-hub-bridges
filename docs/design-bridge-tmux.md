# Design: bridge-tmux — on-demand tmux セッションで動く interactive Claude Code bridge

> Status: **Implemented** (M1)
> Issue: [#110](https://github.com/kishibashi3/agent-hub-bridges/issues/110)
> Author: @bridges-impl

---

## 1. 概要と動機

`bridge-tmux` は Claude Code を **interactive tmux セッション** として動かす bridge。

### なぜ tmux か

2026-06-15 以降、Claude Code の headless 実行（`-p` / `--print` 等）が API 課金に
移行する予定。一方、tmux で動かすインタラクティブセッションは Claude Code subscription
課金のまま使えると判断されている (operator 確認済み)。

```
claude_p bridge: claude -p (headless) → 6/15 以降は API 課金
bridge-tmux    : claude (interactive, tmux) → subscription 課金維持
```

### on-demand spawn (issue #110 の核心)

60 peer が登録されていても同時処理するのは 10 前後。
全 peer の claude セッションを常時起動するのは無駄なので、
**メッセージが来た時だけ Tier2 を spawn する** (wake-on-message)。

---

## 2. 2-tier アーキテクチャ

```
┌─────────────────────────────────────────────────┐
│  Tier 1: bridge-tmux Python process (常時起動)  │
│                                                  │
│  AgentHub.inbox() ──▶ _handle_one()             │
│                           │                      │
│                     SessionManager.handle()      │
│                           │ spawn / inject / wait│
└─────────────────────────────┼────────────────────┘
                              ▼
┌─────────────────────────────────────────────────┐
│  Tier 2: tmux session (on-demand)               │
│                                                  │
│  tmux new-session -d -s claude-bridge-<user>    │
│  └── claude --mcp-config /tmp/bridge-*.json ... │
│       └── (MCP) mcp__agent-hub__send_message    │
└─────────────────────────────────────────────────┘
```

**Tier 1 (Python)**:
- agent-hub SSE inbox 購読 (agent-hub-sdk)
- SessionManager 経由で Tier2 のライフサイクル管理
- メッセージキューイング (inbox loop が serialize する)

**Tier 2 (claude interactive)**:
- `claude --mcp-config <tmp>.json [--continue] [--dangerously-skip-permissions] [--model xxx]`
- claude が MCP tool `mcp__agent-hub__send_message` を呼んで返信
- Tier 1 は pane の変化を監視するだけ (テキスト解析不要)

> **Future**: Tier 1 を Go で書き直すとメモリフットプリントを Python の 1/10 に削減できる。
> Go の agent-hub-sdk が未整備なため M1 では Python。

---

## 3. 動作フロー

```
1. AgentHub.inbox() → IncomingMessage 受信
2. SessionManager.handle(prompt):
   a. idle timer をキャンセル (wake-on-message)
   b. is_alive() == False → TmuxSession.start() (Cold → Warm)
   c. TmuxSession.inject_message(prompt) (tmux paste-buffer + Enter)
   d. TmuxSession.wait_for_idle() で応答完了待ち
   e. 完了 → idle timer 開始
3. hub.ack(msg.id)
```

---

## 4. メッセージ注入方式

`tmux send-keys` に長いマルチライン文字列を渡すと問題が出るため、
**named buffer** 方式を採用:

```python
# 1. named buffer に書き込む (競合防止)
tmux load-buffer -b bridge-<session> - < message.txt

# 2. ペインに貼り付ける
tmux paste-buffer -b bridge-<session> -t <session>

# 3. Enter を送る
tmux send-keys -t <session> "" Enter

# 4. buffer を削除 (PAT 等の機密情報をメモリから消す)
tmux delete-buffer -b bridge-<session>
```

---

## 5. 応答完了検知

claude が応答を終えたかどうかは **pane activity 監視** で判断する:

```
Phase 1: pane 変化待ち (claude が処理を開始した証拠)
  └─ baseline と比較、変化が現れるまでポーリング

Phase 2: pane 変化が止まるまで待つ
  └─ 最終変化から activity_idle_s 秒 = 応答完了
```

パラメータ:
| env | デフォルト | 説明 |
|---|---|---|
| `BRIDGE_TMUX_ACTIVITY_IDLE_S` | 8.0 秒 | pane 変化ゼロ → 完了 |
| `BRIDGE_TMUX_RESPONSE_TIMEOUT_S` | 300 秒 | 1 メッセージ上限 |

> **Note**: プロンプト文字列 (例: `❯`) による検知も可能だが、
> Claude Code のバージョンで変わる可能性があるため M1 では採用しない。
> activity timeout の方が安定的。

---

## 6. Idle タイムアウトと on-demand spawn

```
                  メッセージ受信
Cold ─────────────────────────▶ Starting ──▶ Warm
 ▲                                              │
 │  idle_timeout_s 後                           │ idle timer 開始
 │  ◀── SessionManager._run_idle_timer ◀──── (Warm)
Cooling
```

| env | デフォルト | 説明 |
|---|---|---|
| `BRIDGE_TMUX_IDLE_TIMEOUT_S` | 600 秒 (10 分) | warm kill まで |
| `--idle-timeout <sec>` | CLI でも指定可 | |

---

## 7. History 継続 (`--continue`)

```python
# 初回 (Cold → Starting, _started_before=False)
claude --mcp-config /tmp/bridge-tmux-user-xxx.json ...

# 2 回目以降 (kill 後 re-spawn, _started_before=True)
claude --continue --mcp-config /tmp/bridge-tmux-user-xxx.json ...
```

`--continue` により直前の会話コンテキストを引き継ぐ。
`~/.claude/` の会話ファイルは workdir が同じなら再利用される。

---

## 8. 認証 (subscription 優先)

Tier 2 の tmux セッションは Tier 1 の Python プロセス環境を引き継ぐ。
**`ANTHROPIC_API_KEY` が設定されているとそちらが優先される** ため、
本 bridge を使う際は `ANTHROPIC_API_KEY` を unset しておくこと。

```bash
# 推奨起動方法
unset ANTHROPIC_API_KEY
agent-hub-bridge-tmux --user reviewer --workdir /path/to/reviewer
```

将来的には起動スクリプト内で自動 unset する予定。

---

## 9. MCP config

`claude_p` bridge と同一形式の一時 JSON ファイル (`/tmp/bridge-tmux-<user>-*.json`):

```json
{
  "mcpServers": {
    "agent-hub": {
      "type": "http",
      "url": "<AGENT_HUB_URL>",
      "headers": {
        "Authorization": "Bearer <GITHUB_PAT>",
        "X-User-Id": "<user>",
        "X-Tenant-Id": "<tenant>"  // tenant が設定されている場合のみ
      }
    }
  }
}
```

- mode `0o600` で GITHUB_PAT を保護
- `SessionManager.shutdown()` (= bridge 終了 finally) で削除

---

## 10. 起動例

```bash
# basic
agent-hub-bridge-tmux --user reviewer --workdir /path/to/reviewer/workdir

# with idle timeout override
agent-hub-bridge-tmux --user planner --workdir /path/to/planner --idle-timeout 1800

# with model
agent-hub-bridge-tmux --user writer --workdir /path/to/writer --model claude-opus-4-5

# env-only
AGENT_HUB_URL=http://... GITHUB_PAT=ghp_... \
  agent-hub-bridge-tmux --user reviewer --workdir .
```

---

## 11. 設計上の将来課題

1. **Tier 1 を Go で書き直す**: メモリフットプリント削減、arm64 cross-compile
2. **プロンプトパターン検知**: `activity_idle_s` の代わりに Claude Code のプロンプト文字列を検知する
3. **`ANTHROPIC_API_KEY` の自動 unset**: 起動スクリプトまたは env manipulation
4. **複数 peer の 1 プロセス管理**: peers.yaml で複数 peer を 1 bridge で管理
5. **メッセージキューの永続化**: Tier 1 クラッシュ時のキュー消失対策
6. **Crash recovery の retry**: Tier 2 crash 時の自動再起動 (M1 では未実装)
