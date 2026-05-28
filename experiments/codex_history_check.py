"""CODEX_HOME 会話履歴永続化 検証スクリプト (issue #75).

bridge-codex (resident-process) の設計の前提となる「stable CODEX_HOME を使えば
`codex exec` が会話コンテキストを引き継げる」という仮説を手動で検証する。

## 使い方

```bash
cd /home/kishibashi3/app/private/agent-hub-bridges
uv run python experiments/codex_history_check.py \\
    --agent-hub-url "$AGENT_HUB_URL" \\
    --github-pat "$GITHUB_PAT"
```

## 前提条件

- `codex` CLI がインストール済み: `npm i -g @openai/codex`
- `~/.codex/auth.json` が存在: `codex auth login` で認証済み
- `AGENT_HUB_URL` / `GITHUB_PAT` が設定済み（agent-hub MCP 接続用）

## 出力の見方

Step 1 の `codex exec` 後に CODEX_HOME 内に履歴ファイルが生成されているかを確認し、
Step 2 で前回の情報が引き継がれているかを stdout で出力する。
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import shutil
import sys
import tempfile
from pathlib import Path

logger = logging.getLogger(__name__)

# 検証用の固定フレーズ
_STEP1_PHRASE = "42"
_STEP1_PROMPT = (
    f"これはテストです。数字 {_STEP1_PHRASE} を記憶してください。"
    " 「了解、覚えました。」とだけ返答してください。"
)
_STEP2_PROMPT = (
    "先ほど記憶するよう頼んだ数字は何ですか？数字だけを返答してください。"
)


def _setup_codex_home(codex_home: Path, agent_hub_url: str, github_pat: str) -> None:
    """stable CODEX_HOME を初期化する (auth.json symlink + config.toml)."""
    codex_home.mkdir(parents=True, exist_ok=True)
    os.chmod(codex_home, 0o700)

    # auth.json symlink
    user_auth = Path.home() / ".codex" / "auth.json"
    if not user_auth.exists():
        raise FileNotFoundError(
            f"~/.codex/auth.json が見つかりません。`codex auth login` で認証してください。"
        )
    dst = codex_home / "auth.json"
    if not dst.exists():
        dst.symlink_to(user_auth)
        logger.info("Linked: %s -> %s", dst, user_auth)

    # config.toml: agent-hub MCP 設定（bridge-codex と同じ形式）
    toml_content = (
        "[mcp_servers.agent-hub]\n"
        f'url = "{agent_hub_url}"\n'
        f'bearer_token_env_var = "GITHUB_PAT"\n'
    )
    config_path = codex_home / "config.toml"
    config_path.write_text(toml_content, encoding="utf-8")
    os.chmod(config_path, 0o600)
    logger.info("Wrote config.toml: %s", config_path)


async def _run_codex(
    codex_home: Path,
    prompt: str,
    github_pat: str,
    workdir: Path,
    codex_cli: str = "codex",
    step_label: str = "",
) -> tuple[int, str, str]:
    """codex exec を 1 回実行する（--ephemeral なし）."""
    cmd = [
        codex_cli,
        "exec",
        "-s", "read-only",
        "-C", str(workdir),
        "--skip-git-repo-check",
        # NOTE: --ephemeral を意図的に省略することで CODEX_HOME に履歴を書かせる
        prompt,
    ]
    env = os.environ.copy()
    env["CODEX_HOME"] = str(codex_home)
    env["GITHUB_PAT"] = github_pat

    logger.info("[%s] Running: %s", step_label, " ".join(cmd[:4]) + " ...")
    logger.info("[%s] CODEX_HOME=%s", step_label, codex_home)

    proc = await asyncio.create_subprocess_exec(
        *cmd,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        cwd=str(workdir),
        env=env,
    )
    stdout_bytes, stderr_bytes = await proc.communicate()
    stdout = stdout_bytes.decode("utf-8", errors="replace")
    stderr = stderr_bytes.decode("utf-8", errors="replace")
    return proc.returncode or 0, stdout, stderr


def _list_codex_home_files(codex_home: Path) -> list[str]:
    """CODEX_HOME 内のファイル一覧（デバッグ用）."""
    result = []
    for f in sorted(codex_home.rglob("*")):
        if f.is_file():
            result.append(str(f.relative_to(codex_home)))
    return result


async def main_async(args: argparse.Namespace) -> int:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    codex_cli = shutil.which("codex") or "codex"
    if not Path(codex_cli).exists() and shutil.which("codex") is None:
        print("ERROR: codex CLI が見つかりません。`npm i -g @openai/codex` でインストールしてください。", file=sys.stderr)
        return 1

    # stable CODEX_HOME: 実験終了後も手動で確認できるよう /tmp 以下に固定
    codex_home = Path(tempfile.gettempdir()) / "bridge-codex-history-exp"
    workdir = Path(tempfile.gettempdir()) / "bridge-codex-exp-workdir"
    workdir.mkdir(exist_ok=True)

    print(f"\n{'='*60}")
    print(f"  CODEX_HOME 会話履歴 永続化 検証 (issue #75)")
    print(f"  stable CODEX_HOME: {codex_home}")
    print(f"  workdir:           {workdir}")
    print(f"{'='*60}\n")

    _setup_codex_home(codex_home, args.agent_hub_url, args.github_pat)

    # --- Step 1: 記憶を依頼 ---
    print(f"[Step 1] Prompt: {_STEP1_PROMPT}\n")
    rc1, out1, err1 = await _run_codex(
        codex_home=codex_home,
        prompt=_STEP1_PROMPT,
        github_pat=args.github_pat,
        workdir=workdir,
        codex_cli=codex_cli,
        step_label="Step1",
    )
    print(f"[Step 1] returncode={rc1}")
    print(f"[Step 1] stdout:\n{out1}")
    if err1.strip():
        print(f"[Step 1] stderr (last 500 chars):\n{err1[-500:]}")

    files_after_step1 = _list_codex_home_files(codex_home)
    print(f"\n[Step 1] CODEX_HOME files after run:")
    for f in files_after_step1:
        print(f"  {f}")

    # --- Step 2: 記憶を確認 ---
    print(f"\n[Step 2] Prompt: {_STEP2_PROMPT}\n")
    rc2, out2, err2 = await _run_codex(
        codex_home=codex_home,
        prompt=_STEP2_PROMPT,
        github_pat=args.github_pat,
        workdir=workdir,
        codex_cli=codex_cli,
        step_label="Step2",
    )
    print(f"[Step 2] returncode={rc2}")
    print(f"[Step 2] stdout:\n{out2}")
    if err2.strip():
        print(f"[Step 2] stderr (last 500 chars):\n{err2[-500:]}")

    # --- 結果判定 ---
    history_recalled = _STEP1_PHRASE in out2
    print(f"\n{'='*60}")
    print(f"  結果: {'✅ 会話履歴が引き継がれた' if history_recalled else '❌ 会話履歴は引き継がれなかった'}")
    print(f"  (Step 2 の stdout に '{_STEP1_PHRASE}' が含まれるか: {history_recalled})")
    print(f"  → bridge-codex Task 2 アーキテクチャへの影響を issue #75 に追記してください")
    print(f"{'='*60}\n")

    if args.cleanup:
        shutil.rmtree(codex_home, ignore_errors=True)
        print(f"[cleanup] Removed: {codex_home}")

    return 0 if history_recalled else 1


def main() -> int:
    parser = argparse.ArgumentParser(
        description="CODEX_HOME 会話履歴 永続化 検証スクリプト (issue #75)",
    )
    parser.add_argument(
        "--agent-hub-url",
        required=True,
        help="agent-hub MCP URL (例: https://agent-hub-ki.fly.dev/mcp)",
    )
    parser.add_argument(
        "--github-pat",
        default=os.environ.get("GITHUB_PAT", ""),
        help="GitHub PAT (env GITHUB_PAT でも設定可能)",
    )
    parser.add_argument(
        "--cleanup",
        action="store_true",
        default=False,
        help="実験後に stable CODEX_HOME を削除する",
    )
    args = parser.parse_args()

    if not args.github_pat:
        print("ERROR: --github-pat または env GITHUB_PAT が必要です。", file=sys.stderr)
        return 2

    return asyncio.run(main_async(args))


if __name__ == "__main__":
    sys.exit(main())
