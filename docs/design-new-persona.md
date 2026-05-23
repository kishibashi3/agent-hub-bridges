# design: agent-hub-new-persona (issue #61)

Status: **Reviewed — 実装前確定**

---

## 1. 概要

`agent-hub-new-persona` は persona の召喚を一発で行うユーティリティコマンド。
手作業で行っていた以下の手順を自動化する。

1. GitHub リポジトリ作成 (`gh repo create`)
2. リポジトリを `--workdir` に clone
3. meta-persona の CLAUDE.md をコピー
4. 自己認識セクションを自動書き換え (handle / workdir)
5. git commit + push
6. bridge spawn (完了待ち)

opening ceremony は bridge が CLAUDE.md を読んで自律実行するため、
コマンド側は spawn するだけ (initial prompt 送信は不要)。

---

## 2. インターフェース

```bash
agent-hub-new-persona \
  --model bridge-claude \        # 使用 bridge (必須)
  --from agent-hub-coder \       # meta-persona 名 (必須)
  --name hoge-coder \            # 新 persona の handle (必須、@ なし)
  --workdir /path/to/hoge \      # clone 先ディレクトリ (必須)
  --repos hoge                   # 作成する GitHub repo 名 (必須)
```

### オプション一覧

| flag | 型 | 必須 | 説明 |
|------|----|------|------|
| `--model` | str | ✅ | bridge エンジン種別。`bridge-claude` / `bridge-gemini` / `bridge-codex` / `bridge-claude-p` |
| `--from` | str | ✅ | `$AGENT_HUB_ROLES` 以下の meta-persona ディレクトリ名 |
| `--name` | str | ✅ | 新 persona の handle (@ なし) |
| `--workdir` | str | ✅ | リポジトリを clone する先の絶対パス |
| `--repos` | str | ✅ | `gh repo create` で作成するリポジトリ名 |
| `--tenant` | str | — | agent-hub tenant 名 |
| `--public` | flag | — | repo を public にする (デフォルト: private) |
| `--display-name` | str | — | bridge の display_name |

---

## 3. 入力検証 (fail-fast)

### 3.1 `--from` / `--name` の値検証 (path traversal 対策)

`--from` と `--name` はファイルシステムパスの一部として使われるため、
**受け付け前に allowlist regex で検証する**。

```python
_NAME_RE = re.compile(r'^[a-z0-9][a-z0-9\-_]*$')

def _validate_name(value: str, flag: str) -> None:
    if not _NAME_RE.fullmatch(value):
        raise ValueError(
            f"{flag} must match [a-z0-9][a-z0-9-_]* (got: {value!r})"
        )
```

`--from` と `--name` の両方に適用する。

### 3.2 解決パスが AGENT_HUB_ROLES 内に収まることを確認

```python
roles_root = Path(os.environ["AGENT_HUB_ROLES"]).resolve()
src = (roles_root / from_name / "CLAUDE.md").resolve()

if not src.is_relative_to(roles_root):
    raise ValueError(
        f"Resolved CLAUDE.md path escapes AGENT_HUB_ROLES: {src}"
    )
if not src.exists():
    raise FileNotFoundError(f"CLAUDE.md not found: {src}")
```

### 3.3 AGENT_HUB_ROLES 未設定

```python
if "AGENT_HUB_ROLES" not in os.environ:
    raise ValueError("AGENT_HUB_ROLES environment variable is not set")
```

### 3.4 その他の fail-fast

- `gh` コマンドが PATH にない → `FileNotFoundError`
- `--model` に対応する bridge binary が PATH にない → `FileNotFoundError`

---

## 4. リポジトリ作成とクローン

```python
visibility = "--public" if public else "--private"

# gh repo create <repos> --clone は cwd に <repos>/ を作成する
subprocess.run(
    ["gh", "repo", "create", repos, visibility, "--clone"],
    cwd=workdir.parent,
    check=True,
)
# clone 先は workdir.parent/<repos>/
# --workdir と --repos の末尾が一致することを事前に検証する (§3 参照)
```

### 4.1 `--workdir` と `--repos` の整合性検証

`gh repo create <repos> --clone` は `cwd/<repos>/` を作成する。
`--workdir` にはその絶対パスを指定する必要があるため、
コマンド起動時に `workdir.name == repos` を検証する。

```python
if workdir.name != repos:
    raise ValueError(
        f"--workdir basename ({workdir.name!r}) must match --repos ({repos!r}). "
        f"Set --workdir to {workdir.parent / repos}"
    )
```

これにより `workdir.parent / repos` と `--workdir` が常に一致することが保証される。

### 4.2 既存 workdir はエラー

冪等性よりシンプルさを優先し、`workdir` が既に存在する場合はエラーとして終了する。
再実行が必要な場合はディレクトリを手動で削除してから再試行する。

```python
if workdir.exists():
    raise FileExistsError(f"--workdir already exists: {workdir}")
```

---

## 5. CLAUDE.md コピーと自己認識書き換え

```python
# コピー (§3 で検証済みの src を使う)
dst = workdir / "CLAUDE.md"
shutil.copy2(src, dst)

# 自己認識セクションを書き換え
_rewrite_self_awareness(dst, name=name, workdir=workdir)
```

### 5.1 書き換えロジック

対象は `## 自己認識` セクション内の以下 2 行。

```markdown
- **handle**: `@<PLACEHOLDER>`
- **workdir**: `<PLACEHOLDER>`
```

正規表現で行単位に置換する。

```python
def _rewrite_self_awareness(path: Path, *, name: str, workdir: Path) -> None:
    text = path.read_text(encoding="utf-8")
    text = re.sub(
        r"(- \*\*handle\*\*: `).*?(`)",
        rf"\g<1>@{name}\g<2>",
        text,
    )
    text = re.sub(
        r"(- \*\*workdir\*\*: `).*?(`)",
        rf"\g<1>{workdir}/\g<2>",
        text,
    )
    path.write_text(text, encoding="utf-8")
```

---

## 6. git commit + push

commit メッセージには `from_name` を使い、meta-persona 名を明示する。

```python
subprocess.run(["git", "add", "CLAUDE.md"], cwd=workdir, check=True)
subprocess.run(
    ["git", "commit", "-m", f"add: {from_name} CLAUDE.md (agent-hub-new-persona)"],
    cwd=workdir,
    check=True,
)
subprocess.run(["git", "push", "origin", "main"], cwd=workdir, check=True)
```

---

## 7. Bridge spawn

`spawn-bridge.sh` と同等のロジックを Python で実装する。

```python
log_path = Path(f"/tmp/bridge-{name}.log")
log_path.write_text("", encoding="utf-8")

binary = _resolve_bridge_binary(model)
cmd = [binary, "--user", name, "--workdir", str(workdir)]
if tenant:
    cmd += ["--tenant", tenant]
if display_name:
    cmd += ["--display-name", display_name]

with log_path.open("a") as log_fh:
    subprocess.Popen(
        cmd,
        stdout=log_fh,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )

# "listening on inbox" を待つ
_wait_for_listening(log_path, timeout_s=SPAWN_TIMEOUT_S)
print(f"bridge @{name} is ready. log: {log_path}", file=sys.stderr)
```

### 7.1 バイナリ解決

```python
# 対応表 (--model → binary 名)
_BRIDGE_BINARIES = {
    "bridge-claude":   "agent-hub-bridge-claude",
    "bridge-gemini":   "agent-hub-bridge-gemini",
    "bridge-codex":    "agent-hub-bridge-codex",
    "bridge-claude-p": "agent-hub-bridge-claude-p",
}

def _resolve_bridge_binary(model: str) -> str:
    binary_name = _BRIDGE_BINARIES.get(model)
    if binary_name is None:
        valid = ", ".join(sorted(_BRIDGE_BINARIES))
        raise ValueError(f"Unknown --model {model!r}. Valid: {valid}")
    resolved = shutil.which(binary_name)
    if not resolved:
        raise FileNotFoundError(
            f"binary not found: {binary_name}. Is agent-hub-bridges installed?"
        )
    return resolved
```

### 7.2 起動待ち

ファイルをインクリメンタルに読み、行単位で検索する。

```python
# env で override 可能 (単位: 秒)
SPAWN_TIMEOUT_S = float(os.environ.get("NEW_PERSONA_SPAWN_TIMEOUT_S", "30"))

def _wait_for_listening(log_path: Path, *, timeout_s: float) -> None:
    """bridge が 'listening on inbox' をログ出力するまで待つ."""
    deadline = time.monotonic() + timeout_s
    with log_path.open(encoding="utf-8") as f:
        while time.monotonic() < deadline:
            line = f.readline()
            if line:
                if "listening on inbox" in line:
                    return
            else:
                time.sleep(0.5)
    raise TimeoutError(
        f"bridge @{name!r} did not reach 'listening on inbox' "  # noqa: F821
        f"within {timeout_s:.0f}s. Check log: {log_path}"
    )
```

---

## 8. ファイル構成

```
src/agent_hub_bridges/new_persona/
    __init__.py
    cli.py        # argparse + main()
    runner.py     # run_new_persona() + 内部ヘルパー
pyproject.toml    # console_scripts に agent-hub-new-persona を追加
tests/new_persona/
    __init__.py
    test_runner.py
```

`new_persona` は bridge worker ではなく一回限りのユーティリティコマンドなので、
`config.py` / `engine.py` / `worker.py` の分割は不要。
`runner.py` に全ロジックを集約する。

---

## 9. pyproject.toml 変更

```toml
[project.scripts]
agent-hub-new-persona = "agent_hub_bridges.new_persona.cli:main"
```

新規 extra は不要 (依存は `subprocess` + `shutil` + `re` のみ、全て stdlib)。

---

## 10. テスト方針

subprocess 呼び出しはすべて mock する。

| テストクラス | 確認内容 |
|---|---|
| `TestValidateName` | 有効値通過 / `../evil` や空文字で ValueError |
| `TestResolveAgentHubRoles` | AGENT_HUB_ROLES 未設定で ValueError / path traversal で ValueError |
| `TestRewriteSelfAwareness` | handle / workdir の置換が正しく行われる |
| `TestResolveBridgeBinary` | 有効 model で binary 解決 / 未知 model で ValueError |
| `TestWaitForListening` | 正常 return / timeout で TimeoutError / インクリメンタル読み取り |
| `TestRunNewPersona` | gh / git / bridge spawn の各 subprocess が正しい引数で呼ばれる |
| `TestWorkdirReposConsistency` | workdir.name != repos で ValueError |

---

## 11. 未解決・スコープ外

1. **`--repos` の複数指定**: M1 では 1 つに限定。
2. **GitHub org 指定**: `gh repo create` のデフォルト (個人 namespace) を使う。org 指定は M2 以降。
3. **SIGTERM temp file**: 本コマンドが spawn した bridge は nohup 扱いのため issue #58 と同様。
4. **opening ceremony 完了の検知**: bridge が自律実行するため、コマンドは spawn 完了まで
   しか関与しない。ceremony 完了 (= ready 報告 DM) の検知は本コマンドのスコープ外。
