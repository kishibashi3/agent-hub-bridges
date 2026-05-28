"""CODEX_HOME 会話履歴永続化 検証スクリプト v2 (issue #75).

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

## 修正履歴

v2: Step 2 が実行されない問題の修正 (issue #75)
  - --dangerously-bypass-approvals-and-sandbox を追加（承認待ちハング防止）
  - asyncio.wait_for タイムアウト 120s を追加（hang 時に確実に次へ進む）
  - stdin=DEVNULL で TTY 待ちを防止
  - flush=True で出力の順序を保証
  - Step 2 実行前後に明示的なセパレータを追加
  - stdout が空の場合は agent-hub inbox 確認を案内
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

# 1 ステップあたりの最大実行時間 (秒)
_STEP_TIMEOUT_S = 120.0


def _sep(label: str = "") -> None:
    """視認性のためのセパレータを出力する."""
    line = "=" * 60
    print(f"\n{line}", flush=True)
    if label:
        print(f"  {label}", flush=True)
        print(f"{line}", flush=True)
    print(flush=True)


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
    if dst.is_symlink() or dst.exists():
        logger.info("auth.json symlink already exists: %s", dst)
    else:
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
    timeout_s: float = _STEP_TIMEOUT_S,
) -> tuple[int, str, str]:
    """codex exec を 1 回実行する（--ephemeral なし）.

    v2 の変更点:
    - --dangerously-bypass-approvals-and-sandbox を追加（MCP tool 呼び出し時の
      承認待ちハングを防止。bridge-codex でも daemon 運用で必要なオプション）
    - asyncio.wait_for でタイムアウト (120s デフォルト)
    - stdin=DEVNULL で TTY 待ちを防止
    """
    cmd = [
        codex_cli,
        "exec",
        "-s", "read-only",
        "-C", str(workdir),
        "--skip-git-repo-check",
        "--dangerously-bypass-approvals-and-sandbox",  # v2: 承認待ちハング防止
        # NOTE: --ephemeral を意図的に省略することで CODEX_HOME に履歴を書かせる
        prompt,
    ]
    env = os.environ.copy()
    env["CODEX_HOME"] = str(codex_home)
    env["GITHUB_PAT"] = github_pat

    print(f"[{step_label}] Running codex exec (timeout={timeout_s:.0f}s)...", flush=True)
    print(f"[{step_label}] CODEX_HOME={codex_home}", flush=True)
    logger.info("[%s] cmd: %s ...", step_label, " ".join(cmd[:5]))

    proc = await asyncio.create_subprocess_exec(
        *cmd,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
        stdin=asyncio.subprocess.DEVNULL,  # v2: TTY 待ちを防止
        cwd=str(workdir),
        env=env,
    )

    try:
        stdout_bytes, stderr_bytes = await asyncio.wait_for(
            proc.communicate(),
            timeout=timeout_s,
        )
    except TimeoutError:
        print(
            f"\n[{step_label}] ⏰ TIMEOUT ({timeout_s:.0f}s) — killing codex process",
            flush=True,
        )
        try:
            proc.kill()
        except ProcessLookupError:
            pass
        await proc.wait()
        return -1, "", f"TIMEOUT after {timeout_s:.0f}s"

    stdout = stdout_bytes.decode("utf-8", errors="replace")
    stderr = stderr_bytes.decode("utf-8", errors="replace")
    return proc.returncode or 0, stdout, stderr


def _list_codex_home_files(codex_home: Path) -> list[str]:
    """CODEX_HOME 内のファイル一覧（デバッグ用）."""
    result = []
    try:
        for f in sorted(codex_home.rglob("*")):
            if f.is_file():
                result.append(str(f.relative_to(codex_home)))
    except Exception as exc:
        result.append(f"(error listing files: {exc})")
    return result


async def main_async(args: argparse.Namespace) -> int:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    codex_cli = shutil.which("codex") or "codex"
    if not shutil.which("codex"):
        print(
            "ERROR: codex CLI が見つかりません。`npm i -g @openai/codex` でインストールしてください。",
            file=sys.stderr,
        )
        return 1

    # stable CODEX_HOME: 実験終了後も手動で確認できるよう /tmp 以下に固定
    codex_home = Path(tempfile.gettempdir()) / "bridge-codex-history-exp"
    workdir = Path(tempfile.gettempdir()) / "bridge-codex-exp-workdir"
    workdir.mkdir(exist_ok=True)

    _sep("CODEX_HOME 会話履歴 永続化 検証 v2 (issue #75)")
    print(f"  stable CODEX_HOME : {codex_home}", flush=True)
    print(f"  workdir           : {workdir}", flush=True)
    print(f"  codex CLI         : {codex_cli}", flush=True)
    print(f"  step timeout      : {_STEP_TIMEOUT_S:.0f}s", flush=True)
    print(flush=True)

    _setup_codex_home(codex_home, args.agent_hub_url, args.github_pat)

    # ------------------------------------------------------------------ Step 1
    _sep("Step 1 / 2 — 記憶を依頼")
    print(f"Prompt: {_STEP1_PROMPT}\n", flush=True)

    rc1, out1, err1 = await _run_codex(
        codex_home=codex_home,
        prompt=_STEP1_PROMPT,
        github_pat=args.github_pat,
        workdir=workdir,
        codex_cli=codex_cli,
        step_label="Step1",
        timeout_s=_STEP_TIMEOUT_S,
    )
    print(f"\n[Step 1] returncode = {rc1}", flush=True)
    print(f"[Step 1] stdout:\n{out1 or '(empty — codex replied via MCP/agent-hub DM)'}", flush=True)
    if err1.strip():
        print(f"[Step 1] stderr (last 500 chars):\n{err1[-500:]}", flush=True)

    files_after_step1 = _list_codex_home_files(codex_home)
    print(f"\n[Step 1] CODEX_HOME files after run:", flush=True)
    for f in (files_after_step1 or ["(no files found)"]):
        print(f"  {f}", flush=True)

    if rc1 not in (0,):
        print(
            f"\n⚠️  Step 1 の returncode={rc1}。Step 2 は実行しますが結果は参考値です。",
            flush=True,
        )

    # ------------------------------------------------------------------ Step 2
    _sep("Step 2 / 2 — 記憶を確認")
    print(f"Prompt: {_STEP2_PROMPT}\n", flush=True)

    rc2, out2, err2 = await _run_codex(
        codex_home=codex_home,
        prompt=_STEP2_PROMPT,
        github_pat=args.github_pat,
        workdir=workdir,
        codex_cli=codex_cli,
        step_label="Step2",
        timeout_s=_STEP_TIMEOUT_S,
    )
    print(f"\n[Step 2] returncode = {rc2}", flush=True)
    print(f"[Step 2] stdout:\n{out2 or '(empty — codex replied via MCP/agent-hub DM)'}", flush=True)
    if err2.strip():
        print(f"[Step 2] stderr (last 500 chars):\n{err2[-500:]}", flush=True)

    files_after_step2 = _list_codex_home_files(codex_home)
    print(f"\n[Step 2] CODEX_HOME files after run:", flush=True)
    for f in (files_after_step2 or ["(no files found)"]):
        print(f"  {f}", flush=True)

    # ------------------------------------------------------------------ 結果
    history_recalled = _STEP1_PHRASE in out2
    _sep("結果")
    if history_recalled:
        print(f"  ✅ 会話履歴が引き継がれた (stdout に '{_STEP1_PHRASE}' を確認)", flush=True)
    else:
        print(f"  ❌ stdout からは確認できず ('{_STEP1_PHRASE}' not in Step 2 stdout)", flush=True)
        if not out2.strip():
            print(flush=True)
            print(
                "  ⚠️  NOTE: codex が返答を agent-hub DM 経由で送っている場合、\n"
                "     stdout は空になります。\n"
                "     agent-hub の inbox (DM from @bridge-codex-exp or self) を\n"
                "     確認し、Step 2 の返答に '42' が含まれるかを issue #75 に\n"
                "     コメントしてください。",
                flush=True,
            )

    print(flush=True)

    if args.cleanup:
        shutil.rmtree(codex_home, ignore_errors=True)
        print(f"[cleanup] Removed: {codex_home}", flush=True)

    return 0 if history_recalled else 1


def main() -> int:
    parser = argparse.ArgumentParser(
        description="CODEX_HOME 会話履歴 永続化 検証スクリプト v2 (issue #75)",
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
