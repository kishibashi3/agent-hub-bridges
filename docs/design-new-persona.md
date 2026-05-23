# design: agent-hub-new-persona (issue #61)

Status: **Draft — レビュー待ち**

---

## 1. 概要

`agent-hub-new-persona` は persona の召喚を一発で行うユーティリティコマンド。
手作業で行っていた以下の手順を自動化する。

1. GitHub リポジトリ作成 (`gh repo create`)
2. meta-persona の CLAUDE.md をコピー
3. 自己認識セクションを自動書き換え (handle / workdir)
4. git commit + push
5. bridge spawn (完了待ち)

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

## 3. AGENT_HUB_ROLES 解決

`--from <name>` は以下のパスに解決する。

```
$AGENT_HUB_ROLES/<name>/CLAUDE.md
```

- `AGENT_HUB_ROLES` 未設定 → `ValueError` で終了 (exit 2)
- `CLAUDE.md` が存在しない → `FileNotFoundError` で終了 (exit 2)

---

## 4. リポジトリ作成とクローン

```python
# 1. gh repo create
subprocess.run(
    ["gh", "repo", "create", repos, "--private", "--clone"],
    cwd=workdir.parent,
    check=True,
)
```

`gh repo create <repos> --clone` が `workdir.parent` に `<repos>/` を作成する。

- `--public` 指定時は `--private` → `--public` に切り替え
- `workdir` が既に存在する場合は `--no-clone` を使い clone をスキップ
  (冪等性のため: 再実行時にエラーにしない)
- 作成後: `workdir = workdir.parent / repos` として以降の処理に使う

---

## 5. CLAUDE.md コピーと自己認識書き換え

```python
# コピー
src = Path(os.environ["AGENT_HUB_ROLES"]) / from_name / "CLAUDE.md"
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
import re

def _rewrite_self_awareness(path: Path, *, name: str, workdir: Path) -> None:
    text = path.read_text()
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
    path.write_text(text)
```

---

## 6. git commit + push

```python
subprocess.run(["git", "add", "CLAUDE.md"], cwd=workdir, check=True)
subprocess.run(
    ["git", "commit", "-m", "add: coder CLAUDE.md (agent-hub-new-persona)"],
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
log_path.write_text("")

binary = _resolve_bridge_binary(model)
cmd = [binary, "--user", name, "--workdir", str(workdir)]
if tenant:
    cmd += ["--tenant", tenant]
if display_name:
    cmd += ["--display-name", display_name]

proc = subprocess.Popen(
    cmd,
    stdout=log_path.open("a"),
    stderr=subprocess.STDOUT,
    start_new_session=True,
)

# "listening on inbox" を待つ (最大 SPAWN_TIMEOUT_S 秒)
_wait_for_listening(log_path, timeout_s=SPAWN_TIMEOUT_S)
```

### 7.1 バイナリ解決

```python
def _resolve_bridge_binary(model: str) -> str:
    binary_name = f"agent-hub-{model}"   # "agent-hub-bridge-claude" 等
    resolved = shutil.which(binary_name)
    if not resolved:
        raise FileNotFoundError(f"binary not found: {binary_name}")
    return resolved
```

### 7.2 起動待ち

```python
SPAWN_TIMEOUT_S = 30.0

def _wait_for_listening(log_path: Path, *, timeout_s: float) -> None:
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if "listening on inbox" in log_path.read_text():
            return
        time.sleep(0.5)
    raise TimeoutError(
        f"bridge did not reach 'listening on inbox' within {timeout_s:.0f}s"
    )
```

---

## 8. ファイル構成

```
src/agent_hub_bridges/new_persona/
    __init__.py
    cli.py        # argparse + main()
    runner.py     # run_new_persona() 本体
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

新規 extra は不要 (依存は `subprocess` + `shutil` のみ)。

---

## 10. テスト方針

subprocess 呼び出しはすべて mock する。

| テストクラス | 確認内容 |
|---|---|
| `TestResolveAgentHubRoles` | AGENT_HUB_ROLES 未設定で ValueError / 正常解決 |
| `TestRewriteSelfAwareness` | handle / workdir の置換が正しく行われる |
| `TestRunNewPersona` | gh / git / bridge spawn の各 subprocess が正しい引数で呼ばれる |
| `TestResolveBridgeBinary` | 有効な model 名で binary が解決される / 未知 model でエラー |
| `TestWaitForListening` | ログに "listening on inbox" が来たら return / timeout で TimeoutError |

---

## 11. 未解決・スコープ外

1. **`--repos` の複数指定**: issue では `--repos hoge` と単数。M1 では 1 つに限定。
2. **GitHub org 指定**: `gh repo create` のデフォルト (個人 namespace) を使う。org 指定は M2 以降。
3. **既存 repo の場合**: `gh repo create` が失敗するが、`--no-clone` で clone のみ再実行する
   パスは設けない。エラーとして終了する (冪等性よりシンプルさを優先)。
4. **SIGTERM temp file**: issue #58 と同様のパターン (spawn した bridge は `nohup` 扱い)。
