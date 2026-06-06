# bridge-tmux

Go 製の Tier1 bridge — tmux セッション上で interactive Claude Code を動かす。

## 概要

`claude -p` (headless) ではなく **対話型 tmux セッション** を使うため、
6/15 以降も Claude Code **subscription 課金のまま** 動作する。(issue #110)

## 2-tier アーキテクチャ

```
Tier1: bridge-tmux (Go binary, 常時起動, ~5MB)
  └── agent-hub MCP ポーリング (5秒間隔)
  └── SessionManager (on-demand spawn / idle kill)
        │
        ▼ spawn / inject / wait_for_idle
Tier2: tmux session (on-demand)
  └── claude --mcp-config /tmp/bridge-tmux-<user>-*.json ...
        └── MCP send_message → agent-hub (返信)
```

## ビルド

```bash
make build          # amd64
make build-arm64    # Raspberry Pi 5 用
```

## 起動

```bash
# ANTHROPIC_API_KEY は設定しないこと (subscription auth 優先)
unset ANTHROPIC_API_KEY

export AGENT_HUB_URL=http://localhost:3000
export GITHUB_PAT=ghp_xxxxx

./bridge-tmux \
  --user reviewer \
  --workdir /path/to/agent-hub-roles-kaz/reviewer \
  --idle-timeout 10m
```

## オプション

| フラグ | デフォルト | 説明 |
|---|---|---|
| `--user` | (required) | agent-hub handle (@ なし) |
| `--display-name` | "" | 表示名 |
| `--workdir` | cwd | peer workdir (CLAUDE.md がある dir) |
| `--model` | claude デフォルト | Claude model 名 |
| `--idle-timeout` | 10m | warm session の idle kill 時間 |
| `--no-bypass-permissions` | false | --dangerously-skip-permissions を付けない |

## 環境変数

| 変数 | 必須 | 説明 |
|---|---|---|
| `AGENT_HUB_URL` | ✓ | agent-hub MCP エンドポイント |
| `GITHUB_PAT` | ✓ | GitHub PAT (agent-hub auth) |
| `AGENT_HUB_TENANT` | | tenant 名 (未設定 = default) |
| `CLAUDE_CLI_PATH` | | claude CLI の path (デフォルト: `claude`) |

## systemd service 例 (Pi5)

```ini
[Unit]
Description=bridge-tmux reviewer
After=network.target

[Service]
User=pi
EnvironmentFile=/etc/bridge-tmux/reviewer.env
ExecStart=/usr/local/bin/bridge-tmux --user reviewer --workdir /opt/reviewer
Restart=always
KillSignal=SIGINT
```

## 設計上の特徴

- **応答完了検知**: pane activity 監視 (変化ゼロ 8 秒 = 完了)。プロンプト文字列に依存しない
- **History 継続**: idle kill 後の再起動時に `--continue` フラグで会話を引き継ぐ
- **named buffer**: `tmux load-buffer -b bridge-<name>` で複数 bridge の global buffer 競合を回避
- **circuit breaker**: 連続 10 回エラーで graceful shutdown
- **ANTHROPIC_API_KEY 自動 unset**: main() 起動時に `os.Unsetenv` (subscription auth 優先)
