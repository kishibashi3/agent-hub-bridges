"""agent-hub-new-persona runner — persona spawn in one command.

設計: docs/design-new-persona.md

処理フロー:
  1. 入力検証 (path traversal 対策 / AGENT_HUB_ROLES / workdir 整合)
  2. gh repo create --clone
  3. CLAUDE.md コピー + 自己認識書き換え
  4. git commit + push
  5. bridge spawn + "listening on inbox" 待ち
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path

# ---------------------------------------------------------------------------
# 定数
# ---------------------------------------------------------------------------

# 対応する --model 値とバイナリ名の対応表
_BRIDGE_BINARIES: dict[str, str] = {
    "bridge-claude": "agent-hub-bridge-claude",
    "bridge-gemini": "agent-hub-bridge-gemini",
    "bridge-codex": "agent-hub-bridge-codex",
    "bridge-claude-p": "agent-hub-bridge-claude-p",
}

# bridge が起動して "listening on inbox" を出力するまでの最大待機時間 (秒)
# env NEW_PERSONA_SPAWN_TIMEOUT_S で override 可能
SPAWN_TIMEOUT_S = float(os.environ.get("NEW_PERSONA_SPAWN_TIMEOUT_S", "30"))

# --from / --name に許可する文字: 小文字英数字・ハイフン・アンダースコア
# 先頭は英数字必須 (ディレクトリ名・handle として安全な集合)
_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9\-_]*$")


# ---------------------------------------------------------------------------
# 入力検証
# ---------------------------------------------------------------------------


def _validate_name(value: str, flag: str) -> None:
    """--from / --name の値を allowlist regex で検証する (path traversal 対策)."""
    if not _NAME_RE.fullmatch(value):
        raise ValueError(
            f"{flag} must match [a-z0-9][a-z0-9-_]* (got: {value!r})"
        )


def _resolve_claude_md(from_name: str) -> Path:
    """$AGENT_HUB_ROLES/<from_name>/CLAUDE.md を解決して返す.

    - AGENT_HUB_ROLES 未設定 → ValueError
    - 解決パスが AGENT_HUB_ROLES 外に出る → ValueError (symlink 攻撃対策)
    - CLAUDE.md が存在しない → FileNotFoundError
    """
    if "AGENT_HUB_ROLES" not in os.environ:
        raise ValueError("AGENT_HUB_ROLES environment variable is not set")

    roles_root = Path(os.environ["AGENT_HUB_ROLES"]).resolve()
    src = (roles_root / from_name / "CLAUDE.md").resolve()

    if not src.is_relative_to(roles_root):
        raise ValueError(
            f"Resolved CLAUDE.md path escapes AGENT_HUB_ROLES: {src}"
        )
    if not src.exists():
        raise FileNotFoundError(f"CLAUDE.md not found: {src}")

    return src


def _resolve_bridge_binary(model: str) -> str:
    """--model をバイナリ名に解決する.

    - 未知の model → ValueError
    - binary が PATH にない → FileNotFoundError
    """
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


# ---------------------------------------------------------------------------
# CLAUDE.md 書き換え
# ---------------------------------------------------------------------------


def _rewrite_self_awareness(path: Path, *, name: str, workdir: Path) -> None:
    """CLAUDE.md の自己認識セクション内 handle / workdir 行を書き換える.

    対象行 (設計 §5.1):
      - **handle**: `@<PLACEHOLDER>`
      - **workdir**: `<PLACEHOLDER>`
    """
    text = path.read_text(encoding="utf-8")
    # lambda を使って置換文字列を構築する。
    # rf"..." に name / workdir を直接埋め込むと、パス中の \1 や \g<n> が
    # re.sub のグループ参照として誤解釈される (Critical #1)。
    text = re.sub(
        r"(- \*\*handle\*\*: `).*?(`)",
        lambda m: m.group(1) + "@" + name + m.group(2),
        text,
    )
    text = re.sub(
        r"(- \*\*workdir\*\*: `).*?(`)",
        lambda m: m.group(1) + str(workdir) + "/" + m.group(2),
        text,
    )
    path.write_text(text, encoding="utf-8")


# ---------------------------------------------------------------------------
# bridge spawn 待ち
# ---------------------------------------------------------------------------


def _wait_for_listening(log_path: Path, *, name: str, timeout_s: float) -> None:
    """bridge が 'listening on inbox' をログ出力するまで待つ.

    インクリメンタルに readline() で読み、不必要な全量 read を避ける。
    timeout 超過は TimeoutError。
    """
    deadline = time.monotonic() + timeout_s
    with log_path.open(encoding="utf-8", errors="replace") as f:
        while time.monotonic() < deadline:
            line = f.readline()
            if line:
                if "listening on inbox" in line:
                    return
            else:
                time.sleep(0.5)
    raise TimeoutError(
        f"bridge @{name} did not reach 'listening on inbox' "
        f"within {timeout_s:.0f}s. Check log: {log_path}"
    )


# ---------------------------------------------------------------------------
# メインロジック
# ---------------------------------------------------------------------------


def run_new_persona(
    *,
    model: str,
    from_name: str,
    name: str,
    workdir: Path,
    repos: str,
    tenant: str | None = None,
    public: bool = False,
    display_name: str | None = None,
) -> None:
    """persona を召喚する (設計: docs/design-new-persona.md).

    Raises:
        ValueError: 入力検証失敗
        FileNotFoundError: CLAUDE.md / binary が見つからない
        FileExistsError: workdir が既に存在する
        subprocess.CalledProcessError: gh / git コマンド失敗
        TimeoutError: bridge 起動待ちタイムアウト
    """
    # ---- 1. 入力検証 -------------------------------------------------------
    _validate_name(from_name, "--from")
    _validate_name(name, "--name")

    if workdir.name != repos:
        raise ValueError(
            f"--workdir basename ({workdir.name!r}) must match --repos ({repos!r}). "
            f"Hint: set --workdir to {workdir.parent / repos}"
        )
    if workdir.exists():
        raise FileExistsError(f"--workdir already exists: {workdir}")

    src_claude_md = _resolve_claude_md(from_name)
    binary = _resolve_bridge_binary(model)

    if not shutil.which("gh"):
        raise FileNotFoundError("gh command not found. Install GitHub CLI.")

    print(f"[1/5] Creating GitHub repo '{repos}' ...", file=sys.stderr)

    # ---- 2. gh repo create + clone -----------------------------------------
    visibility = "--public" if public else "--private"
    subprocess.run(
        ["gh", "repo", "create", repos, visibility, "--clone"],
        cwd=workdir.parent,
        check=True,
    )
    # clone 先は workdir.parent/<repos>/ = workdir (§4.1 で検証済み)

    print(f"[2/5] Copying CLAUDE.md from {src_claude_md} ...", file=sys.stderr)

    # ---- 3. CLAUDE.md コピー + 書き換え ------------------------------------
    dst = workdir / "CLAUDE.md"
    shutil.copy2(src_claude_md, dst)
    _rewrite_self_awareness(dst, name=name, workdir=workdir)

    print(f"[3/5] CLAUDE.md written to {dst}", file=sys.stderr)

    # ---- 4. git commit + push ----------------------------------------------
    print("[4/5] Committing CLAUDE.md ...", file=sys.stderr)
    subprocess.run(["git", "add", "CLAUDE.md"], cwd=workdir, check=True)
    subprocess.run(
        [
            "git",
            "commit",
            "-m",
            f"add: {from_name} CLAUDE.md (agent-hub-new-persona)",
        ],
        cwd=workdir,
        check=True,
    )
    subprocess.run(
        ["git", "push", "--set-upstream", "origin", "HEAD"], cwd=workdir, check=True
    )

    # ---- 5. bridge spawn ---------------------------------------------------
    print(f"[5/5] Spawning bridge @{name} ({model}) ...", file=sys.stderr)

    log_path = Path(f"/tmp/bridge-{name}.log")
    log_path.write_text("", encoding="utf-8")

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

    _wait_for_listening(log_path, name=name, timeout_s=SPAWN_TIMEOUT_S)

    print(
        f"bridge @{name} is ready. log: {log_path}",
        file=sys.stderr,
    )


# ---------------------------------------------------------------------------
# dry-run チェック (issue #61 --dry-run)
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class _DryRunResult:
    """dry-run チェック 1 件の結果。"""

    ok: bool
    label: str
    detail: str


def _check_from_dryrun(from_name: str) -> _DryRunResult:
    """``--from`` CLAUDE.md が存在・読み取り可能か確認する。"""
    try:
        _validate_name(from_name, "--from")
    except ValueError as exc:
        return _DryRunResult(ok=False, label="--from", detail=str(exc))
    try:
        path = _resolve_claude_md(from_name)
        return _DryRunResult(ok=True, label="--from", detail=f"{path} (exists)")
    except (ValueError, FileNotFoundError) as exc:
        return _DryRunResult(ok=False, label="--from", detail=str(exc))


def _check_workdir_dryrun(workdir: Path) -> _DryRunResult:
    """``--workdir`` の状態を確認する。実 run では既存なら FileExistsError。"""
    if workdir.exists():
        return _DryRunResult(
            ok=False,
            label="--workdir",
            detail=f"{workdir} (already exists — real run would fail)",
        )
    return _DryRunResult(
        ok=True,
        label="--workdir",
        detail=f"{workdir} (does not exist, will be created)",
    )


def _check_env_dryrun() -> _DryRunResult:
    """必須 env (AGENT_HUB_URL, GITHUB_PAT) + オプション env (AGENT_HUB_TENANT) を確認する。"""
    required = ["AGENT_HUB_URL", "GITHUB_PAT"]
    optional = ["AGENT_HUB_TENANT"]

    missing_required = [k for k in required if not os.environ.get(k)]
    if missing_required:
        return _DryRunResult(
            ok=False,
            label="env",
            detail=f"missing required: {', '.join(missing_required)}",
        )

    set_keys = [k for k in required + optional if os.environ.get(k)]
    not_set_optional = [k for k in optional if not os.environ.get(k)]

    detail = f"{', '.join(set_keys)} set"
    if not_set_optional:
        detail += f" / {', '.join(not_set_optional)} not set (optional)"
    return _DryRunResult(ok=True, label="env", detail=detail)


def _check_repo_dryrun(repos: str) -> _DryRunResult:
    """``gh repo view`` で同名 repo が既に存在しないか確認する。"""
    if not shutil.which("gh"):
        return _DryRunResult(
            ok=True, label="repo", detail="gh not found, check skipped"
        )
    result = subprocess.run(
        ["gh", "repo", "view", repos],
        capture_output=True,
        timeout=15,
    )
    if result.returncode == 0:
        return _DryRunResult(
            ok=False, label="repo", detail=f"{repos} already exists"
        )
    return _DryRunResult(ok=True, label="repo", detail=f"{repos} does not exist")


def _fetch_participants_from_hub(
    hub_url: str, pat: str, tenant: str | None
) -> list[dict]:
    """MCP プロトコル経由で agent-hub の ``get_participants`` ツールを呼ぶ.

    干渉を最小化するため initialize → notifications/initialized →
    tools/call get_participants の 3 ステップのみ実行する。

    Raises:
        RuntimeError: セッション確立失敗 (mcp-session-id が返らない)
        Exception: MCP 呼び出しその他のエラー
    """
    base_headers: dict[str, str] = {
        "Content-Type": "application/json",
        "Accept": "application/json",
        "Authorization": f"Bearer {pat}",
    }
    if tenant:
        base_headers["X-Tenant-Id"] = tenant

    def _post(
        body_obj: object, extra_headers: dict[str, str] | None = None
    ) -> tuple[bytes, str | None]:
        """POST して (レスポンスボディ, mcp-session-id) を返す。"""
        hdrs = {**base_headers, **(extra_headers or {})}
        encoded = json.dumps(body_obj).encode()
        req = urllib.request.Request(hub_url, data=encoded, headers=hdrs, method="POST")
        with urllib.request.urlopen(req, timeout=10) as resp:
            resp_body = resp.read()
            sid = resp.headers.get("Mcp-Session-Id")
        return resp_body, sid

    # 1. initialize → session ID を取得
    _, session_id = _post(
        {
            "jsonrpc": "2.0",
            "method": "initialize",
            "params": {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "clientInfo": {
                    "name": "agent-hub-new-persona-dry-run",
                    "version": "1.0",
                },
            },
            "id": 0,
        }
    )
    if not session_id:
        raise RuntimeError("no mcp-session-id in initialize response")

    sid_header = {"Mcp-Session-Id": session_id}

    # 2. notifications/initialized (202 空ボディでも OK、エラーは無視)
    try:
        _post({"jsonrpc": "2.0", "method": "notifications/initialized"}, sid_header)
    except Exception:
        pass

    # 3. tools/call get_participants
    resp_body, _ = _post(
        {
            "jsonrpc": "2.0",
            "method": "tools/call",
            "params": {"name": "get_participants", "arguments": {}},
            "id": 1,
        },
        sid_header,
    )
    result = json.loads(resp_body.decode())
    # agent-hub MCP レスポンス: {"result": {"content": [{"text": "<json>"}]}}
    content = result.get("result", {}).get("content", [])
    if not content:
        return []
    text = content[0].get("text", "[]")
    if isinstance(text, str):
        return json.loads(text)
    return text if isinstance(text, list) else []


def _check_handle_dryrun(name: str) -> _DryRunResult:
    """同名 handle が agent-hub に既に online でないか確認する。

    AGENT_HUB_URL / GITHUB_PAT が未設定の場合や API エラーはスキップ扱い。
    """
    hub_url = os.environ.get("AGENT_HUB_URL")
    pat = os.environ.get("GITHUB_PAT")

    if not hub_url or not pat:
        return _DryRunResult(
            ok=True,
            label="handle",
            detail=(
                f"@{name} (check skipped: "
                "AGENT_HUB_URL or GITHUB_PAT not set)"
            ),
        )

    try:
        participants = _fetch_participants_from_hub(
            hub_url, pat, os.environ.get("AGENT_HUB_TENANT")
        )
        online_ids = {p.get("userId") for p in participants if p.get("is_online")}
        if name in online_ids:
            return _DryRunResult(
                ok=False, label="handle", detail=f"@{name} is already online"
            )
        return _DryRunResult(ok=True, label="handle", detail=f"@{name} not online")
    except Exception as exc:
        return _DryRunResult(
            ok=True,
            label="handle",
            detail=f"@{name} (check skipped: {exc})",
        )


def run_dry_run(
    *,
    from_name: str,
    name: str,
    workdir: Path,
    repos: str,
) -> int:
    """``--dry-run`` モード: 事前チェックを全件実行して結果を表示する.

    チェック項目 (issue #61):
      1. ``--from``  CLAUDE.md の存在
      2. ``--workdir`` の状態
      3. env (AGENT_HUB_URL / GITHUB_PAT / AGENT_HUB_TENANT)
      4. repo 重複 (gh repo view)
      5. handle 重複 (agent-hub get_participants)

    Returns:
        0 — 全件 OK
        1 — 1 件以上 NG
    """
    print("[DRY-RUN] agent-hub-new-persona")

    results: list[_DryRunResult] = [
        _check_from_dryrun(from_name),
        _check_workdir_dryrun(workdir),
        _check_env_dryrun(),
        _check_repo_dryrun(repos),
        _check_handle_dryrun(name),
    ]

    for r in results:
        mark = "✅" if r.ok else "❌"
        print(f"  {mark} {r.label}: {r.detail}")

    print()

    failed = [r for r in results if not r.ok]
    if not failed:
        print("All checks passed. Ready to run.")
        return 0

    count = len(failed)
    print(f"{count} check(s) failed. Fix before running without --dry-run.")
    return 1
