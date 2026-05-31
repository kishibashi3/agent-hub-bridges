"""Codex CLI engine wrapper (client-codex, session-persistent).

client-codex の "LLM engine" 部分。`codex exec` を非対話モードで subprocess
起動し、codex が MCP tool 経由で agent-hub に返信するまで待機する。

設計の核心 (docs/design-bridge-codex.md §3 参照):
  - per-bridge の一時 CODEX_HOME (mkdtemp) を作成
  - auth.json は ~/.codex/auth.json へのシンボリックリンク(token refresh 追従)
  - bridge 固有の config.toml を書き込み、agent-hub MCP を CODEX_HOME から解決
  - subprocess env に CODEX_HOME=<temp> + identity env vars をセット

セッション永続化 (issue #79):
  - --ephemeral フラグを廃止し、セッションを CODEX_HOME に保存する
  - --json フラグで JSONL 出力から session_meta イベントの id を取得
  - 2 回目以降は `codex exec resume <session_id>` で会話を継続
  - peer ごとに session_id を _session_ids dict で管理

返信は codex 自身が `mcp__agent-hub__send_message` を呼ぶ。worker は
subprocess 完了を待つだけ(gemini bridge と同一パターン)。

retry は M1 では実装しない(issue #53 design §7)。
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import shutil
import signal
import tempfile
from dataclasses import dataclass
from pathlib import Path

from agent_hub_bridges.client_codex.config import Config

logger = logging.getLogger(__name__)

# config.toml の MCP セクションで identity を渡すための env 変数名。
# codex の env_http_headers はヘッダ値を「環境変数名」で参照する。
_ENV_USER_ID = "CODEX_BRIDGE_USER_ID"
_ENV_TENANT_ID = "CODEX_BRIDGE_TENANT_ID"

# 1 message あたりの最大実行時間 (秒)。env CODEX_CLI_TIMEOUT_S で override 可能。
DEFAULT_TIMEOUT_S = 600.0


@dataclass(frozen=True)
class EngineResult:
    """`codex exec` 1 回分の実行結果(ログ用)."""

    returncode: int
    stdout: str
    stderr: str
    duration_s: float
    session_id: str | None = None


class CodexCLIEngine:
    """`codex exec` の非対話呼び出しを管理する engine.

    state:
      - `_temp_codex_home`: per-bridge の一時 CODEX_HOME (Path)。
        `create()` で作成、`close()` で `shutil.rmtree` 削除。
      - `_cli_path`: 実行する codex binary path。
      - `_timeout_s`: 1 ターンあたりの timeout。
      - `_session_ids`: peer ごとのセッション ID (peer handle → UUID)。
        issue #79 — セッション永続化。
    """

    def __init__(
        self,
        config: Config,
        temp_codex_home: Path,
        cli_path: str,
        timeout_s: float,
    ) -> None:
        self._config = config
        self._temp_codex_home = temp_codex_home
        self._cli_path = cli_path
        self._timeout_s = timeout_s
        self._session_ids: dict[str, str] = {}

    @classmethod
    def create(cls, config: Config) -> CodexCLIEngine:
        """Engine を初期化する.

        - codex CLI の path を解決(shutil.which)
        - per-bridge 一時 CODEX_HOME を mkdtemp で作成
        - auth.json シンボリックリンクを張る
        - bridge 固有の config.toml を書き込む
        """
        cli_path = shutil.which(config.codex_cli_path) or config.codex_cli_path
        if not cli_path or not Path(cli_path).exists():
            raise FileNotFoundError(
                f"codex CLI not found at '{config.codex_cli_path}'. "
                f"Install with `npm i -g @openai/codex` and ensure it's on PATH."
            )

        # auth.json が存在するか fail-fast チェック
        user_auth = Path.home() / ".codex" / "auth.json"
        if not user_auth.exists():
            raise FileNotFoundError(
                f"codex auth.json not found at {user_auth}. "
                f"Run `codex auth login` to authenticate."
            )

        timeout_s = float(os.environ.get("CODEX_CLI_TIMEOUT_S", DEFAULT_TIMEOUT_S))

        temp_codex_home = Path(
            tempfile.mkdtemp(prefix=f"client-codex-{config.user}-")
        )
        os.chmod(temp_codex_home, 0o700)

        try:
            _setup_codex_home(temp_codex_home, config)
        except Exception:
            shutil.rmtree(temp_codex_home, ignore_errors=True)
            raise

        logger.info(
            "CodexCLIEngine ready: cli=%s codex_home=%s timeout=%.0fs "
            "sandbox=%s approval_bypass=%s",
            cli_path,
            temp_codex_home,
            timeout_s,
            config.sandbox_mode,
            config.approval_bypass,
        )
        return cls(
            config=config,
            temp_codex_home=temp_codex_home,
            cli_path=cli_path,
            timeout_s=timeout_s,
        )

    def close(self) -> None:
        """一時 CODEX_HOME を片付ける(worker 終了時の finally で呼ぶ)。

        auth.json symlink も config.toml も、セッションファイルも含めてまとめて削除される。
        セッションはプロセス生存期間中のみ有効。bridge 再起動後は新規セッションとなる。
        (_session_ids も失われるため、次回起動時は全 peer について初回扱いになる。)
        """
        shutil.rmtree(self._temp_codex_home, ignore_errors=True)

    async def run(self, *, peer: str, prompt: str) -> EngineResult:
        """`codex exec` を起動して 1 ターン処理する(retry なし).

        codex は内部で agent-hub MCP tool を呼んで返信するため、
        この関数の戻り値は「実行ログ」。returncode / stdout / stderr をログに残す。
        timeout 超過時は subprocess を kill して RuntimeError を投げる。
        """
        return await self._invoke_once(peer=peer, prompt=prompt)

    async def _invoke_once(self, *, peer: str, prompt: str) -> EngineResult:
        """`codex exec` を 1 回起動して結果を返す.

        issue #79: 既知の session_id があれば `codex exec resume <id>` で継続、
        なければ新規セッションとして `codex exec` を起動する。
        """
        session_id = self._session_ids.get(peer)
        cmd = self._build_cmd(prompt, session_id=session_id)
        env = self._build_env()

        logger.info(
            "-> spawning codex for peer=%s (sandbox=%s, cwd=%s, session=%s)",
            peer,
            self._config.sandbox_mode,
            self._config.workdir,
            session_id or "new",
        )

        loop = asyncio.get_running_loop()
        start = loop.time()

        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd=str(self._config.workdir),
            env=env,
            # issue #17 パターン: process group 分離で timeout kill が確実に届く。
            start_new_session=True,
        )

        try:
            stdout_bytes, stderr_bytes = await asyncio.wait_for(
                proc.communicate(),
                timeout=self._timeout_s,
            )
        except TimeoutError as err:
            logger.warning(
                "codex CLI timeout (%.0fs) for peer=%s; killing pgid=%d",
                self._timeout_s,
                peer,
                proc.pid,
            )
            try:
                os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
            except ProcessLookupError:
                pass
            await proc.wait()
            raise RuntimeError(
                f"codex CLI exceeded timeout ({self._timeout_s:.0f}s) for peer={peer}"
            ) from err

        duration = loop.time() - start
        stdout_str = stdout_bytes.decode("utf-8", errors="replace")
        stderr_str = stderr_bytes.decode("utf-8", errors="replace")

        # issue #79: --json フラグ経由で session_meta イベントから session id を取得。
        extracted_id = _extract_session_id(stdout_str)
        if extracted_id:
            if peer not in self._session_ids:
                logger.info(
                    "[SESSION] new session %s for peer=%s", extracted_id, peer
                )
            self._session_ids[peer] = extracted_id

        result = EngineResult(
            returncode=proc.returncode or 0,
            stdout=stdout_str,
            stderr=stderr_str,
            duration_s=duration,
            session_id=extracted_id,
        )

        if result.returncode != 0:
            logger.warning(
                "codex CLI exit=%d (peer=%s, %.1fs); stderr tail: %s",
                result.returncode,
                peer,
                duration,
                result.stderr[-500:].strip(),
            )
        else:
            logger.info(
                "codex CLI done (peer=%s, %.1fs, %d stdout bytes, session=%s)",
                peer,
                duration,
                len(result.stdout),
                extracted_id or "unknown",
            )
        return result

    def _build_cmd(self, prompt: str, *, session_id: str | None = None) -> list[str]:
        """codex exec コマンドライン(設計: docs/design-bridge-codex.md §4).

        issue #79: セッション永続化のため --ephemeral を廃止し --json を追加。
          - session_id なし (初回):
              codex exec -s <sandbox> -C <workdir> --skip-git-repo-check --json ...
          - session_id あり (継続):
              codex exec resume <id> -s <sandbox> --skip-git-repo-check --json ...
              (resume サブコマンドに -C フラグはないが、subprocess の cwd= で
              workdir を指定するため実害なし)
        """
        if session_id is None:
            cmd: list[str] = [
                self._cli_path,
                "exec",
                "-s", self._config.sandbox_mode,
                "-C", str(self._config.workdir),
                "--skip-git-repo-check",
                "--json",
            ]
        else:
            cmd = [
                self._cli_path,
                "exec", "resume", session_id,
                "-s", self._config.sandbox_mode,
                "--skip-git-repo-check",
                "--json",
            ]
        if self._config.approval_bypass:
            cmd.append("--dangerously-bypass-approvals-and-sandbox")
        if self._config.model:
            cmd.extend(["-m", self._config.model])
        cmd.append(prompt)
        return cmd

    def _build_env(self) -> dict[str, str]:
        """subprocess に渡す env を組み立てる.

        - 既存 env を継承(PATH 等)
        - CODEX_HOME を一時 dir で上書き
        - GITHUB_PAT を確実に export(config.toml の bearer_token_env_var が参照)
        - identity env 変数(_ENV_USER_ID / _ENV_TENANT_ID)をセット
        """
        env = os.environ.copy()
        env["CODEX_HOME"] = str(self._temp_codex_home)
        env["GITHUB_PAT"] = self._config.github_pat
        env[_ENV_USER_ID] = self._config.user
        if self._config.tenant:
            env[_ENV_TENANT_ID] = self._config.tenant
        else:
            env.pop(_ENV_TENANT_ID, None)
        return env


def _extract_session_id(stdout: str) -> str | None:
    """--json JSONL 出力から session_meta イベントの id を取得する.

    `codex exec --json` は各イベントを 1 行の JSON で出力する。
    最初の `session_meta` イベントの `payload.id` がセッション UUID。

    issue #79: セッション永続化のためにセッション ID を取得する。
    """
    for line in stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if event.get("type") == "session_meta":
            # payload が null の場合も安全に処理する (S2: `or {}` で null guard)
            return (event.get("payload") or {}).get("id")
    return None


def _setup_codex_home(temp_codex_home: Path, config: Config) -> None:
    """一時 CODEX_HOME に auth.json symlink と config.toml を書く."""
    _link_auth_json(temp_codex_home)
    _write_config_toml(temp_codex_home, config)


def _link_auth_json(temp_codex_home: Path) -> None:
    """~/.codex/auth.json へのシンボリックリンクを張る.

    コピーではなくシンボリックリンクにする理由: token refresh 時に元ファイルが
    in-place 更新されても自動で追従できる。
    atomic replace(rename syscall)で更新される場合は symlink が壊れる可能性があり、
    その際は copy 方式への変更を検討する(docs/design-bridge-codex.md §11 #2 参照)。
    """
    src = Path.home() / ".codex" / "auth.json"
    dst = temp_codex_home / "auth.json"
    dst.symlink_to(src)
    logger.debug("Linked auth.json: %s -> %s", dst, src)


def _write_config_toml(temp_codex_home: Path, config: Config) -> None:
    """bridge 固有の config.toml を一時 CODEX_HOME に書く.

    env_http_headers の値は環境変数名(実値ではない)。subprocess env に
    _ENV_USER_ID / _ENV_TENANT_ID をセットすることで bridge identity を注入する。
    tenant が未設定の場合は X-Tenant-Id 行を省略する。
    """
    tenant_line = (
        f'X-Tenant-Id = "{_ENV_TENANT_ID}"\n' if config.tenant else ""
    )
    toml_content = (
        f"[mcp_servers.agent-hub]\n"
        f'url = "{config.agent_hub_url}"\n'
        f'bearer_token_env_var = "GITHUB_PAT"\n'
        f"\n"
        f"[mcp_servers.agent-hub.env_http_headers]\n"
        f'X-User-Id = "{_ENV_USER_ID}"\n'
        f"{tenant_line}"
    )
    config_path = temp_codex_home / "config.toml"
    config_path.write_text(toml_content, encoding="utf-8")
    os.chmod(config_path, 0o600)
    logger.info("Wrote codex config.toml: %s", config_path)
