# Design: bridge-claude-p — `claude -p` を使った on-demand bridge

> Status: **Draft** — レビュー待ち  
> Issue: [#54](https://github.com/kishibashi3/agent-hub-bridges/issues/54)  
> Author: @bridges-impl  
> Reference: bridge-gemini (`src/agent_hub_bridges/gemini/`), bridge-claude (`src/agent_hub_bridges/claude/`)

---

## 1. 概要

`claude -p`（Claude Code print mode）を subprocess として使う on-demand bridge。  
bridge プロセス自体は軽量 watchdog（LLM 不使用）。  
メッセージが来たときだけ `claude -p` を起動してタスクを処理し、終了する。

### 動機

- 2026-06-15 以降、Claude Agent SDK 経由の headless 実行が別課金になる可能性
- `claude -p` は Claude Code subscription 範囲内で動作する可能性がある
- Agent SDK と比べ常駐コストゼロ

---

## 2. 動作フロー

```
agent-hub SSE inbox
  → bridge (Python)           # 軽量 watchdog (LLM 不使用)
    → claude -p <prompt>      # subprocess, per-message
      → MCP: agent-hub        # claude が send_message を呼ぶ
```

1. bridge は `agent_hub_sdk.AgentHub.inbox()` で DM を受信
2. `ClaudePCLIEngine.run()` が `claude -p` を subprocess 起動
3. `claude` は MCP tool `mcp__agent-hub__send_message` を呼んで返信
4. subprocess 終了 → bridge が `hub.ack(msg.id)` → 次のメッセージへ

**返信は claude -p が MCP 経由で行う** — bridge は subprocess の完了を待つだけ。  
gemini / codex bridge と同一パターン。

### bridge-claude との差分

| 項目 | bridge-claude | bridge-claude-p |
|---|---|---|
| LLM 呼び出し | Claude Agent SDK (stateful) | `claude -p` subprocess (stateless) |
| 会話履歴 | session_id で per-peer に保持 | なし（1-shot per message） |
| 常駐コスト | Agent SDK credit | なし |
| workdir | 必須 | 必須 |
| MCP 設定 | SDK の `ClaudeAgentOptions` | `--mcp-config <json_file>` |

---

## 3. 認証

`claude -p` は Claude Code の通常の認証を使う:

- **デフォルト**: keychain / OAuth（`~/.claude/` の認証情報）
- **`ANTHROPIC_API_KEY` 環境変数**: API key 経由（keychain より優先）

bridge-claude-p が cost 削減の目的で存在するため、  
keychain/OAuth（= Claude Code subscription）を使うことが前提。  
`ANTHROPIC_API_KEY` は設定しない（設定されていれば API billing になる）。

> **Note**: `--bare` モードは "OAuth and keychain are never read" となるため使用しない。  
> 通常の `claude -p` モードで keychain auth を利用する。

---

## 4. MCP 設定 — `--mcp-config` フラグ

codex (CODEX_HOME 分離) や gemini (isolated HOME) と異なり、  
`claude -p` は `--mcp-config <json_file>` で MCP サーバを直接指定できる。  
これにより HOME 分離が不要になり、設定が単純になる。

### 4.1 一時 MCP config ファイル

起動時に一時ファイルを作成し、`--mcp-config` で渡す:

```json
{
  "mcpServers": {
    "agent-hub": {
      "type": "http",
      "url": "<AGENT_HUB_URL>",
      "headers": {
        "Authorization": "Bearer <GITHUB_PAT>",
        "X-User-Id": "<user>",
        "X-Tenant-Id": "<tenant>"
      }
    }
  }
}
```

- 一時ファイルは `mkstemp(suffix=".json", prefix="bridge-claude-p-")` で作成
- モード `0o600`（owner read-only）で GITHUB_PAT 等を保護
- `engine.close()` で削除（`try/finally` で保証）

> **MCP type フィールド**: `claude --mcp-config` で streamable HTTP MCP サーバを  
> 指定する際のフィールド名・型は Claude Code のバージョンによって異なる可能性がある。  
> 実装時に `claude mcp --help` / 公式ドキュメントで確認する（未解決事項 §10 参照）。

---

## 5. `claude -p` コマンド

```bash
claude \
  -p \                                  # print mode (non-interactive)
  --mcp-config <mcp_config_file> \     # agent-hub MCP 設定
  --dangerously-skip-permissions \      # tool 実行を自動承認
  --no-session-persistence \            # セッションをディスクに保存しない
  [--model <model>] \                   # model 指定がある場合
  [--permission-mode bypassPermissions] \  # 明示的な permission mode (optional)
  "<prompt>"
```

### 5.1 `--dangerously-skip-permissions`

bridge は daemon として人手介在なしで tool を実行するため必須。  
gemini bridge の `--yolo`、codex bridge の `--dangerously-bypass-approvals-and-sandbox`  
と同等の役割。

### 5.2 prompt 渡し方

`claude -p "<prompt>"` と引数渡し（shell=False で subprocess args リスト）。  
gemini bridge (stdin) と codex bridge (引数) の両方の実績があるが、  
`claude -p` は "Arguments: prompt — Your prompt" と明示しているので引数渡しを採用。

### 5.3 出力

`claude -p` は LLM の最終応答を stdout に出力する。  
ただし、bridge-claude-p のアーキテクチャでは **claude が MCP tool を呼んで  
agent-hub に返信する** ため、stdout の内容は worker がログに残すだけで  
hub.send には使わない。

---

## 6. ファイル構成

```
src/agent_hub_bridges/claude_p/
├── __init__.py
├── __main__.py             # python -m agent_hub_bridges.claude_p
├── cli.py                  # argparse エントリポイント
├── config.py               # Config dataclass (BaseConfig 継承)
├── worker.py               # _run_hub_session / _handle_one / run_worker
└── engine.py               # ClaudePCLIEngine (subprocess ラッパー)
```

パッケージ名は `claude_p`（ハイフン不可のため）。  
console script 名は `agent-hub-bridge-claude-p`。

---

## 7. Config フィールド

```python
@dataclass(frozen=True)
class Config(BaseConfig):
    workdir: Path                           # required
    claudep_cli_path: str = "claude"        # claude binary path
    model: str | None = None               # --model <model>
    permission_bypass: bool = True         # --dangerously-skip-permissions (default: on)
```

### 環境変数 / CLI 対応

| 設定 | CLI | 環境変数 | デフォルト |
|---|---|---|---|
| `claudep_cli_path` | — | `CLAUDE_CLI_PATH` | `"claude"` |
| `model` | `--model` | `AGENT_HUB_MODEL` | `None` |
| `permission_bypass` | `--no-bypass-permissions` で無効化 | — | `True` |

> `ANTHROPIC_API_KEY` は **意図的に渡さない**。  
> API key を使いたい場合は shell env で直接設定する（bridge config の責任範囲外）。

---

## 8. ClaudePCLIEngine

gemini の `GeminiCLIEngine` を参考にしつつ、以下の差分がある:

| 項目 | GeminiCLIEngine | ClaudePCLIEngine |
|---|---|---|
| isolated HOME | あり (mkdtemp + settings.json) | なし |
| 一時ファイル | なし | MCP config JSON (mkstemp) |
| env HOME 上書き | あり | なし |
| retry | あり (rate-limit 429) | なし (M1) |
| start_new_session | あり (issue #17) | あり |

```python
class ClaudePCLIEngine:
    def __init__(self, config, mcp_config_path, cli_path, timeout_s): ...
    
    @classmethod
    def create(cls, config: Config) -> ClaudePCLIEngine:
        # cli_path を解決 (shutil.which)
        # MCP config JSON を mkstemp で作成 (mode 0600)
        # return cls(...)
    
    def close(self) -> None:
        # MCP config 一時ファイルを削除
    
    async def run(self, *, peer: str, prompt: str) -> EngineResult:
        # _invoke_once を呼ぶ (retry なし)
    
    async def _invoke_once(self, *, peer: str, prompt: str) -> EngineResult:
        # subprocess.create_subprocess_exec(claude, -p, --mcp-config, ..., prompt)
        # wait_for(communicate(), timeout_s)
        # on timeout: killpg
```

---

## 9. pyproject.toml への追加

```toml
[project.optional-dependencies]
claude_p = [
    # claude CLI は Claude Code (npm: @anthropic-ai/claude-code) に付属。
    # Python 追加 deps なし。
]

[project.scripts]
agent-hub-bridge-claude-p = "agent_hub_bridges.claude_p.cli:main"
```

`[all]` extra にも `claude_p` を追加する。

---

## 10. テスト計画

`tests/claude_p/` を新設。最低限:

| テストクラス | 内容 |
|---|---|
| `TestClaudePCLIEngine` | subprocess mock でコマンドライン組み立てを確認 |
| `TestMcpConfigFile` | 一時 JSON ファイルの内容・モード・削除を確認 |
| `TestHandleOneWorkdirMissing` | workdir 不在 → early return (issue #51 パターン) |
| `TestHandleOneSuccess` | 正常系: engine.run が呼ばれる |
| `TestConfigFromEnv` | 各 env 変数の解決を確認 |

---

## 11. 未解決事項（実装前に確認要）

1. **`--mcp-config` の JSON スキーマ**: `claude -p --mcp-config` が受け付ける  
   JSON の正確なスキーマ（`type: "http"` の有無、`headers` の形式）を  
   Claude Code の実際のバージョンで確認する。
2. **keychain auth と headless**: `claude -p` が TTY なし環境（daemon）で  
   keychain/OAuth auth を正常に使えるか確認が必要。  
   keychain が interactive な dialog を出す場合、`ANTHROPIC_API_KEY` fallback が必要になる。
3. **subscription billing scope**: `claude -p` が実際に API billing ではなく  
   subscription 範囲内で処理されるかは 2026-06-15 以降の実挙動次第。  
   現時点では「逃げ道として先に実装する」スタンス（issue #54 の動機を参照）。
4. **retry**: rate-limit 等のエラーパターンが分かった段階で追加する（M2 以降）。

— @bridges-impl (agent-hub bridge · operator-supervised · kishibashi3/agent-hub)
