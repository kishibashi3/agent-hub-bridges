"""Claude -p CLI engine wrapper.

bridge-claude-p の "LLM engine" 部分。`claude -p` を non-interactive
subprocess として起動し、claude が MCP tool 経由で agent-hub に返信する
まで待機する。

設計の核心 (docs/design-bridge-claude-p.md §4 参照):
  - MCP config を mkstemp で一時 JSON ファイルに書く (mode 0o600)
  - `--mcp-config <tmp_json>` で claude に渡す
  - HOME 分離不要 (gemini/codex より単純)
  - subprocess env に GITHUB_PAT 等を渡す

返信は claude 自身が `mcp__agent-hub__send_message` を呼ぶ。worker は
subprocess 完了を待つだけ (gemini / codex bridge と同一パターン)。

retry は M1 では実装しない (issue #54 design §10)。
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

from agent_hub_bridges.claude_p.config import Config

logger = logging.getLogger(__name__)

# 1 message あたりの最大実行時間 (秒)。env CLAUDEP_CLI_TIMEOUT_S で override 可能。
DEFAULT_TIMEOUT_S = 600.0


@dataclass(frozen=True)
class EngineResult:
    """`claude -p` 1 回分の実行結果 (ログ用)."""

    returncode: int
    stdout: str
    stderr: str
    duration_s: float


class ClaudePCLIEngine:
    """`claude -p` の non-interactive 呼び出しを管理する engine.

    state:
      - `_mcp_config_path`: MCP config の一時 JSON ファイル (Path)。
        `create()` で作成、`close()` で削除。
      - `_cli_path`: 実行する claude binary path。
      - `_timeout_s`: 1 ターンあたりの timeout。
    """

    def __init__(
        self,
        config: Config,
        mcp_config_path: Path,
        cli_path: str,
        timeout_s: float,
    ) -> None:
        self._config = config
        self._mcp_config_path = mcp_config_path
        self._cli_path = cli_path
        self._timeout_s = timeout_s

    @classmethod
    def create(cls, config: Config) -> ClaudePCLIEngine:
        """Engine を初期化する.

        - claude CLI の path を解決 (shutil.which)
        - MCP config JSON を mkstemp で作成 (mode 0o600)
        """
        cli_path = shutil.which(config.claudep_cli_path) or config.claudep_cli_path
        if not cli_path or not Path(cli_path).exists():
            raise FileNotFoundError(
                f"claude CLI not found at '{config.claudep_cli_path}'. "
                f"Install Claude Code and ensure it's on PATH."
            )

        timeout_s = float(os.environ.get("CLAUDEP_CLI_TIMEOUT_S", DEFAULT_TIMEOUT_S))
        mcp_config_path = _write_mcp_config(config)

        logger.info(
            "ClaudePCLIEngine ready: cli=%s mcp_config=%s timeout=%.0fs "
            "permission_bypass=%s",
            cli_path,
            mcp_config_path,
            timeout_s,
            config.permission_bypass,
        )
        return cls(
            config=config,
            mcp_config_path=mcp_config_path,
            cli_path=cli_path,
            timeout_s=timeout_s,
        )

    def close(self) -> None:
        """MCP config 一時ファイルを削除する (worker 終了時の finally で呼ぶ)。"""
        try:
            self._mcp_config_path.unlink(missing_ok=True)
        except Exception:
            logger.warning("Failed to delete MCP config file: %s", self._mcp_config_path)

    async def run(self, *, peer: str, prompt: str) -> EngineResult:
        """`claude -p` を起動して 1 ターン処理する (retry なし).

        claude は内部で agent-hub MCP tool を呼んで返信するため、
        この関数の戻り値は「実行ログ」。returncode / stdout / stderr をログに残す。
        timeout 超過時は subprocess を kill して RuntimeError を投げる。
        """
        return await self._invoke_once(peer=peer, prompt=prompt)

    async def _invoke_once(self, *, peer: str, prompt: str) -> EngineResult:
        """`claude -p` を 1 回起動して結果を返す."""
        cmd = self._build_cmd(prompt)
        env = self._build_env()

        logger.info(
            "-> spawning claude -p for peer=%s (cwd=%s)",
            peer,
            self._config.workdir,
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
                "claude -p timeout (%.0fs) for peer=%s; killing pgid=%d",
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
                f"claude -p exceeded timeout ({self._timeout_s:.0f}s) for peer={peer}"
            ) from err

        duration = loop.time() - start
        result = EngineResult(
            returncode=proc.returncode or 0,
            stdout=stdout_bytes.decode("utf-8", errors="replace"),
            stderr=stderr_bytes.decode("utf-8", errors="replace"),
            duration_s=duration,
        )

        if result.returncode != 0:
            logger.warning(
                "claude -p exit=%d (peer=%s, %.1fs); stderr tail: %s",
                result.returncode,
                peer,
                duration,
                result.stderr[-500:].strip(),
            )
        else:
            logger.info(
                "claude -p done (peer=%s, %.1fs, %d stdout bytes)",
                peer,
                duration,
                len(result.stdout),
            )
        return result

    def _build_cmd(self, prompt: str) -> list[str]:
        """claude -p コマンドライン (設計: docs/design-bridge-claude-p.md §5)."""
        cmd = [
            self._cli_path,
            "-p",
            "--mcp-config",
            str(self._mcp_config_path),
            "--no-session-persistence",
        ]
        if self._config.permission_bypass:
            cmd.append("--dangerously-skip-permissions")
        if self._config.model:
            cmd.extend(["--model", self._config.model])
        cmd.append(prompt)
        return cmd

    def _build_env(self) -> dict[str, str]:
        """subprocess に渡す env を組み立てる.

        ANTHROPIC_API_KEY は意図的に削除する (subscription auth を使うため)。
        親プロセスに設定されていても subprocess には渡さない。
        GITHUB_PAT は MCP config の `headers.Authorization` で参照される可能性を
        考慮して export する。
        GH_TOKEN: GITHUB_APP_* が設定されている場合は IAT を注入し、gh CLI が
        AgentHub [bot] 名義でコメントを投稿できるようにする (issue #73)。
        未設定の場合は GH_TOKEN を変更しない (PAT fallback = 従来動作)。
        """
        from agent_hub_bridges._common.github_iat import IATManager

        env = os.environ.copy()
        env["GITHUB_PAT"] = self._config.github_pat
        # ANTHROPIC_API_KEY を除外: subscription auth 優先、API billing 回避。
        # 親 env にあっても subprocess には渡さない (設計: design-bridge-claude-p.md §4)。
        env.pop("ANTHROPIC_API_KEY", None)

        # GITHUB_APP_* (private key / app ID) はサブプロセスに渡さない — security。
        for k in [k for k in env if k.startswith("GITHUB_APP_")]:
            del env[k]

        # GitHub App IAT モード (issue #73): GITHUB_APP_* が揃っていれば IAT を注入。
        mgr = IATManager.from_env()
        if mgr is not None:
            try:
                env["GH_TOKEN"] = mgr.get_token()
            except Exception:
                logger.warning(
                    "github_iat: IAT fetch failed, falling back to default gh auth",
                    exc_info=True,
                )

        return env


def _write_mcp_config(config: Config) -> Path:
    """bridge 固有の MCP config JSON を一時ファイルに書く.

    `--mcp-config <path>` で claude -p に渡す。
    ファイルには GITHUB_PAT が含まれるため mode 0o600 で保護する。

    tenant が None の場合は X-Tenant-Id ヘッダを省略する。
    """
    headers: dict[str, str] = {
        "Authorization": f"Bearer {config.github_pat}",
        "X-User-Id": config.user,
    }
    if config.tenant:
        headers["X-Tenant-Id"] = config.tenant

    mcp_payload = {
        "mcpServers": {
            "agent-hub": {
                "type": "http",
                "url": config.agent_hub_url,
                "headers": headers,
            }
        }
    }

    fd, tmp_path_str = tempfile.mkstemp(
        suffix=".json", prefix=f"bridge-claude-p-{config.user}-"
    )
    tmp_path = Path(tmp_path_str)
    try:
        os.chmod(tmp_path, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(mcp_payload, f, indent=2)
    except Exception:
        tmp_path.unlink(missing_ok=True)
        raise

    logger.info("Wrote MCP config: %s", tmp_path)
    return tmp_path
