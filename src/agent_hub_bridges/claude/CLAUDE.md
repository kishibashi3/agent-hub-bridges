# bridges/claude

Claude Agent SDK を使う stateful bridge worker。
`agent-hub-bridge-claude` (archived) からの M1 移植版。

## 概要

- **engine**: Claude Agent SDK (`claude_agent_sdk.ClaudeSDKClient`)
- **接続モード**: `stateful` (peer ごとに `session_id` で会話 context を分離)
- **inbox**: `agent_hub_sdk.AgentHub.inbox()` の async iterator で DM を受信
- **認証**: 一時ファイルに MCP config を書き出して Claude SDK に渡す (`_mcp_config_file`)
- **reconnect**: `_common.reconnect.run_with_reconnect` で outer loop を担当
- **restart cursor**: 再起動後のメッセージ重複 dispatch を防ぐ timestamp cursor
  (`cursor.py`) — issue #37 fix

## ファイル構成

```
claude/
├── cli.py          # エントリポイント (argparse)
├── config.py       # Config dataclass (env + CLI args)
├── worker.py       # メインループ (_run_hub_session / _handle_one)
├── cursor.py       # timestamp cursor (restart-safe inbox dedup)
└── claude_runner.py  # ClaudeRunner (ClaudeSDKClient の per-peer wrapper)
```

## Known issues

### MCP SDK `MAX_RECONNECTION_ATTEMPTS=2` trap

`mcp.client.streamable_http.handle_get_stream` は SSE GET の連続失敗が
**2 回** (httpx の既定 `sse_read_timeout=300s` × 2 = 最大 10 分) で
**永続的に諦める**。長時間 idle 後に push が黙って死ぬ故障モードがある。

**現在の緩和策**: pull 経路の poll loop が併走しており、push が死んでも
poll で message を拾い続ける (worker.py の `INBOX_POLL_INTERVAL_S` = 30s
default、旧 repo PR #3 から移植)。

**根本対処**: MCP SDK 側で `MAX_RECONNECTION_ATTEMPTS` を env / option で
expose してもらう必要がある。upstream issue 起票予定 (issue #23 参照)。

詳細 (知見 doc):
[agent-hub-knowledge bridges/bridge-claude/2026-05-17-sse-push-silent-death.md](https://github.com/kishibashi3/agent-hub-knowledge/blob/main/bridges/bridge-claude/2026-05-17-sse-push-silent-death.md)
