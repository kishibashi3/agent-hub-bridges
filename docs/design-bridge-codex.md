# Design: bridge-codex — Codex CLI を使った agent-hub bridge

> Status: **Reviewed** — 実装前確定  
> Issue: [#53](https://github.com/kishibashi3/agent-hub-bridges/issues/53)  
> Author: @bridges-impl  
> Reference: bridge-gemini (`src/agent_hub_bridges/gemini/`)

---

## 1. 概要

OpenAI Codex CLI (`codex exec`) を agent-hub に接続する bridge。  
受信した DM を `codex exec` に渡し、codex が内部で agent-hub MCP tool を呼んで返信する。  
bridge-gemini と同じ "subprocess ラッパー" パターン。

---

## 2. 動作フロー

```
agent-hub SSE inbox
  → bridge (Python)         # 軽量 watchdog
    → codex exec <prompt>   # subprocess, per-message
      → MCP: agent-hub      # codex が send_message を呼ぶ
```

1. bridge は `agent_hub_sdk.AgentHub.inbox()` で DM を受信
2. `CodexCLIEngine.run()` が `codex exec` を subprocess 起動
3. `codex` は MCP tool `mcp__agent-hub__send_message` を呼んで返信
4. subprocess 終了 → bridge が `hub.ack(msg.id)` → 次のメッセージへ

**返信は codex が MCP 経由で行う** — bridge は subprocess の完了を待つだけ。  
gemini bridge と同一パターン。

---

## 3. 認証 — CODEX_HOME 分離

codex は `CODEX_HOME`（デフォルト `~/.codex/`）から設定と認証情報を読む。  
bridge は gemini bridge の "isolated HOME" パターンを踏襲し、per-bridge の
一時 CODEX_HOME を作成して `CODEX_HOME` 環境変数で指定する。

```
/tmp/bridge-codex-<user>-XXXXXX/   ← mkdtemp (mode 0700)
├── auth.json                        ← ~/.codex/auth.json からシンボリックリンク
└── config.toml                      ← bridge 固有の設定を書き込む
```

### 3.1 auth.json

`~/.codex/auth.json` には ChatGPT Enterprise の idtoken が入っている。  
bridge 固有の temp dir からシンボリックリンクを張り、認証情報を共有する。  
コピーではなくシンボリックリンクにする理由: token refresh 時に元ファイルが  
更新されても自動で追従できる。

```python
(temp_codex_home / "auth.json").symlink_to(Path.home() / ".codex" / "auth.json")
```

auth.json が存在しない場合は `CodexCLIEngine.create()` が `FileNotFoundError`  
を投げる（fail-fast）。

### 3.2 config.toml

```toml
[mcp_servers.agent-hub]
url = "<AGENT_HUB_URL>"
bearer_token_env_var = "GITHUB_PAT"

[mcp_servers.agent-hub.env_http_headers]
X-User-Id = "CODEX_BRIDGE_USER_ID"
X-Tenant-Id = "CODEX_BRIDGE_TENANT_ID"
```

`env_http_headers` の値は **環境変数名**（値ではない）。  
subprocess env に `CODEX_BRIDGE_USER_ID=<handle>` / `CODEX_BRIDGE_TENANT_ID=<tenant>`  
をセットすることで bridge ごとの identity を注入する。  
tenant が未設定の場合は `[mcp_servers.agent-hub.env_http_headers]` から
`X-Tenant-Id` 行を省略する。

---

## 4. codex exec コマンド

```bash
codex exec \
  -s <sandbox_mode> \              # read-only (default)
  -C <workdir> \                   # 作業ディレクトリ
  --skip-git-repo-check \          # workdir が git repo でなくても動作
  --ephemeral \                    # session をディスクに保存しない
  [--dangerously-bypass-approvals-and-sandbox] \  # approval_bypass=True 時のみ
  [-m <model>] \                   # model 指定がある場合
  "<prompt>"
```

### 4.1 sandbox_mode

| CLI option | 意味 |
|---|---|
| `read-only` (default) | ファイル読み取りのみ |
| `workspace-write` | workdir 内への書き込みを許可 |
| `danger-full-access` | 全アクセス許可（要注意）|

### 4.2 approval_bypass

`--dangerously-bypass-approvals-and-sandbox` は bridge daemon として  
人手介在なしで tool を実行するために必要。  
デフォルトは **False**（sandbox + approval あり）とし、  
CLI `--bypass-approvals` フラグで有効化する。

> **Note**: `--dangerously-bypass-approvals-and-sandbox` は sandbox も無効化する。  
> approval のみ無効化したい場合は `-c approval_policy=on-request` 等の設定が  
> 別途必要になる可能性があるが、codex の設定 key が未確認のため M1 では  
> on/off の 2 値とし、詳細な approval 制御は将来 issue で対応する。

### 4.3 prompt 渡し方

`codex exec "<prompt>"` と引数渡しする（stdin ではなく引数）。  
gemini bridge が stdin を使うのとは異なるが、codex exec は  
"If not provided as an argument (or if `-` is used), instructions are read from stdin"  
とあり、引数渡しが推奨される形式。  
prompt に shell の特殊文字が含まれる可能性があるため、subprocess の  
`args` リストに要素として渡す（shell=False）。

---

## 5. ファイル構成

```
src/agent_hub_bridges/codex/
├── __init__.py
├── __main__.py             # python -m agent_hub_bridges.codex
├── cli.py                  # argparse エントリポイント
├── config.py               # Config dataclass (BaseConfig 継承)
├── worker.py               # _run_hub_session / _handle_one / run_worker
└── engine.py               # CodexCLIEngine (subprocess ラッパー)
```

---

## 6. Config フィールド

```python
@dataclass(frozen=True)
class Config(BaseConfig):
    workdir: Path                         # required (BaseConfig では Optional)
    codex_cli_path: str = "codex"         # codex binary path
    model: str | None = None              # -m <model>
    sandbox_mode: str = "read-only"       # -s <mode>
    approval_bypass: bool = False         # --dangerously-bypass-approvals-and-sandbox
```

### 環境変数 / CLI 対応

| 設定 | CLI | 環境変数 | デフォルト |
|---|---|---|---|
| `codex_cli_path` | — | `CODEX_CLI_PATH` | `"codex"` |
| `model` | `--model` | `AGENT_HUB_MODEL` | `None` |
| `sandbox_mode` | `--sandbox` | `CODEX_SANDBOX_MODE` | `"read-only"` |
| `approval_bypass` | `--bypass-approvals` | `CODEX_APPROVAL_BYPASS` | `False` |

`CODEX_APPROVAL_BYPASS` の parsing ルール: **unset または空文字 → `False` / 任意の non-empty 文字列 → `True`**。  
`== "1"` や `== "true"` に限定しない（ecosystem convention: 任意の non-empty で有効）。

```python
approval_bypass = bool(os.environ.get("CODEX_APPROVAL_BYPASS", "").strip())
```

**`approval_bypass` デフォルト `False` の設計意図**:  
codex の `--dangerously-bypass-approvals-and-sandbox` は sandbox も同時に無効化するため、  
デフォルト off とし operator が明示的に有効化する運用にする。  
bridge-claude-p の `permission_bypass` (デフォルト `True`) との非対称は意図的:  
claude-p では `--dangerously-skip-permissions` が permissions のみを対象とし、  
sandbox は別軸なので daemon 運用での安全なデフォルトが異なる。

---

## 7. EngineResult / タイムアウト

`EngineResult` は gemini と同じ構造を採用:

```python
@dataclass(frozen=True)
class EngineResult:
    returncode: int
    stdout: str
    stderr: str
    duration_s: float
```

タイムアウト: デフォルト 600 秒（gemini と同じ）。  
env `CODEX_CLI_TIMEOUT_S` で上書き可能。  
タイムアウト時は `os.killpg(os.getpgid(proc.pid), signal.SIGKILL)` で  
プロセスグループごと kill（issue #17 gemini 実績パターン）。

**retry は M1 では不実装**。  
ChatGPT Enterprise の idtoken auth では rate-limit の発生パターンが  
gemini (Quota exceeded 429) と異なるため、実運用で実態を確認してから M2 で設計する。

---

## 8. worker.py の設計

gemini worker と同一パターン。差分のみ列挙:

| 項目 | gemini | codex |
|---|---|---|
| engine | `GeminiCLIEngine` | `CodexCLIEngine` |
| rate-limit fallback DM | あり | なし (M1) |
| workdir missing check | — | あり (issue #51 実績パターン) |
| retry | あり (max 3) | なし (M1) |

workdir missing check (issue #51 で bridge-claude に追加) を codex でも実装する。

### engine.close() と worker の finally

`CodexCLIEngine.close()` は temp CODEX_HOME (`shutil.rmtree`) を削除する。  
auth.json symlink も config.toml も含めてまとめて削除されるため、個別削除は不要。  
worker は `run_worker()` の `finally` で必ず `engine.close()` を呼ぶ:

```python
async def run_worker(config: Config) -> None:
    engine = CodexCLIEngine.create(config)
    try:
        await run_with_reconnect(...)
    finally:
        engine.close()  # temp CODEX_HOME を shutil.rmtree で削除
```

---

## 9. pyproject.toml への追加

```toml
[project.optional-dependencies]
codex = [
    # codex CLI は npm i -g @openai/codex-cli で入れる。Python 追加 deps なし。
]

[project.scripts]
agent-hub-bridge-codex = "agent_hub_bridges.codex.cli:main"
```

`[all]` extra にも `codex` を追加する。

---

## 10. テスト計画

`tests/codex/` を新設。最低限:

| テストクラス | 内容 |
|---|---|
| `TestCodexCLIEngine` | subprocess mock でコマンドライン組み立てを確認 |
| `TestHandleOneWorkdirMissing` | workdir 不在 → early return (issue #51 パターン) |
| `TestHandleOneSuccess` | 正常系: engine.run が呼ばれる |
| `TestConfigFromEnv` | sandbox_mode / approval_bypass の env 解決 |

---

## 11. 未解決事項（実装前に確認要）

1. **`approval_policy` config key**: `-c approval_policy=...` で approval を細かく  
   制御できるか未確認。M1 では `--dangerously-bypass-approvals-and-sandbox` の  
   on/off に限定。
2. **auth.json refresh**: codex が token refresh を行う際、symlink 先の  
   `~/.codex/auth.json` を in-place 更新するか、別ファイルに書き直すかによって  
   symlink が壊れる可能性がある。実運用で確認し、必要なら copy 方式に変更。  
   確認方法: `strace -e trace=open,openat,rename,unlink codex auth login` で  
   auth.json への syscall を観察する。`rename()` が出れば atomic replace（symlink が壊れる）、  
   `write()` のみなら in-place 更新（symlink は安全）。
3. **複数 bridge の concurrent 起動**: 異なる `--user` で複数 codex bridge を  
   起動した場合、temp CODEX_HOME が衝突しない（mkdtemp で一意）ことは保証できるが、  
   auth.json への symlink 競合は今後の調査事項。

— @bridges-impl (agent-hub bridge · operator-supervised · kishibashi3/agent-hub)
