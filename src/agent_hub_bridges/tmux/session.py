"""tmux セッション管理 (Tier2) — TmuxSession + SessionManager.

TmuxSession: 1 peer に対する 1 つの tmux セッションのライフサイクルを管理する。
  - 起動 (start): `tmux new-session` + `claude --mcp-config ...` を送信
  - メッセージ注入 (inject_message): `tmux load-buffer` + `paste-buffer` + Enter
  - 応答完了検知 (wait_for_idle): pane 変化ゼロ N 秒で完了と判断
  - 停止 (stop): graceful kill → 5 秒後に force kill

SessionManager: idle タイムアウトによる自動 kill と on-demand 再起動を担う。
  - handle(prompt): 未起動なら start()、inject、wait_for_idle の一連をラップ
  - idle timer: 最終メッセージ処理完了から idle_timeout_s 後に stop()

設計: docs/design-bridge-tmux.md §Tier2
Issue: #110
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import shlex
import shutil
import subprocess
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path

from agent_hub_bridges.tmux.config import Config

logger = logging.getLogger(__name__)

# pane 変化ポーリング間隔
_POLL_INTERVAL_S = 0.5

# graceful shutdown 後に force kill するまで待つ秒数
_GRACEFUL_KILL_WAIT_S = 5.0

# cold start 後に pane に何か出力が来るまで待つ最小時間
_MIN_ACTIVITY_WAIT_S = 1.0


@dataclass
class TmuxSession:
    """1 peer に対応する interactive Claude Code tmux セッション.

    Tier2 (claude) の実体。起動・停止・メッセージ注入・応答完了検知を提供する。

    Attributes:
        session_name: tmux セッション名 (例: `claude-bridge-reviewer`)。
        workdir: peer の作業 root (CLAUDE.md が置かれた dir)。
        _config: bridge-tmux 全体の runtime config。
        _mcp_config_path: claude に渡す MCP config 一時ファイル (close() で削除)。
        _cli_path: claude CLI の resolved path。
        _started_before: True なら次の start() で `--continue` を使う。
    """

    session_name: str
    workdir: Path
    _config: Config = field(repr=False)
    _mcp_config_path: Path = field(repr=False)
    _cli_path: str = field(repr=False)
    _started_before: bool = field(default=False, repr=False)

    # ------------------------------------------------------------------ #
    # factory                                                              #
    # ------------------------------------------------------------------ #

    @classmethod
    def create(cls, config: Config) -> TmuxSession:
        """Session オブジェクトを生成する (まだ tmux session は作らない)."""
        cli_path = shutil.which(config.claude_cli_path) or config.claude_cli_path
        if not Path(cli_path).exists() and not shutil.which(cli_path):
            raise FileNotFoundError(
                f"claude CLI not found: '{config.claude_cli_path}'. "
                "Install Claude Code and make sure it's on PATH."
            )
        mcp_config_path = _write_mcp_config(config)
        session_name = f"claude-bridge-{config.user}"
        logger.info(
            "TmuxSession created: name=%s workdir=%s cli=%s",
            session_name, config.workdir, cli_path,
        )
        return cls(
            session_name=session_name,
            workdir=config.workdir,
            _config=config,
            _mcp_config_path=mcp_config_path,
            _cli_path=cli_path,
        )

    # ------------------------------------------------------------------ #
    # lifecycle                                                            #
    # ------------------------------------------------------------------ #

    def is_alive(self) -> bool:
        """tmux セッションが存在するか確認する."""
        result = subprocess.run(
            ["tmux", "has-session", "-t", self.session_name],
            capture_output=True,
        )
        return result.returncode == 0

    async def start(self) -> None:
        """tmux セッションを新規作成して claude を起動する.

        _started_before が True なら `--continue` で直前の会話を継続する。
        起動後 spawn_timeout_s 以内に pane に出力が現れるのを待つ。
        """
        if self.is_alive():
            logger.warning("Session %s already exists — stopping first", self.session_name)
            await self.stop()

        # tmux セッション作成 (detached)
        logger.info("Creating tmux session %s (workdir=%s)", self.session_name, self.workdir)
        subprocess.run(
            [
                "tmux", "new-session", "-d",
                "-s", self.session_name,
                "-c", str(self.workdir),
            ],
            check=True,
        )

        # claude 起動コマンドを送信
        cmd_parts = self._build_cmd()
        cmd_str = " ".join(shlex.quote(p) for p in cmd_parts)
        subprocess.run(
            ["tmux", "send-keys", "-t", self.session_name, cmd_str, "Enter"],
            check=True,
        )
        logger.info("Sent claude start command to %s: %s", self.session_name, cmd_str)

        # pane に出力が来るまで待機 (= claude が起動した証拠)
        deadline = time.monotonic() + self._config.spawn_timeout_s
        baseline = self._capture_pane()

        # send-keys した直後はコマンド文字列が pane に出るので少し待ってから判定
        await asyncio.sleep(_MIN_ACTIVITY_WAIT_S)

        while time.monotonic() < deadline:
            content = self._capture_pane()
            if content != baseline and content.strip():
                logger.info("Session %s started — got initial output", self.session_name)
                self._started_before = True
                return
            await asyncio.sleep(_POLL_INTERVAL_S)

        # タイムアウト: セッションは起動したが claude から出力なし
        logger.error(
            "Session %s spawn timeout (%.0fs)", self.session_name, self._config.spawn_timeout_s
        )
        await self.stop()
        raise TimeoutError(
            f"claude did not produce output within {self._config.spawn_timeout_s:.0f}s "
            f"in tmux session '{self.session_name}'"
        )

    async def stop(self) -> None:
        """tmux セッションを停止する (graceful → force)."""
        if not self.is_alive():
            return
        logger.info("Stopping tmux session %s", self.session_name)

        # Ctrl+C を送って graceful 終了を試みる
        subprocess.run(
            ["tmux", "send-keys", "-t", self.session_name, "C-c", ""],
            check=False,
        )
        await asyncio.sleep(_GRACEFUL_KILL_WAIT_S)

        # まだ生きていれば force kill
        if self.is_alive():
            subprocess.run(
                ["tmux", "kill-session", "-t", self.session_name],
                check=False,
            )
        logger.info("tmux session %s stopped", self.session_name)

    def close(self) -> None:
        """リソースのクリーンアップ (MCP config 一時ファイル削除)."""
        try:
            self._mcp_config_path.unlink(missing_ok=True)
        except Exception:
            logger.warning("Failed to delete MCP config: %s", self._mcp_config_path)

    # ------------------------------------------------------------------ #
    # messaging                                                            #
    # ------------------------------------------------------------------ #

    async def inject_message(self, text: str) -> None:
        """メッセージテキストを tmux ペインに paste して Enter を送る.

        Named buffer (`bridge-<session_name>`) を使うことで、
        同一ホストで複数 bridge が動いても global buffer が競合しない。
        """
        buffer_name = f"bridge-{self.session_name}"
        try:
            # テキストをバッファに書き込む
            subprocess.run(
                ["tmux", "load-buffer", "-b", buffer_name, "-"],
                input=text.encode("utf-8"),
                check=True,
            )
            # バッファをペインに貼り付ける
            subprocess.run(
                ["tmux", "paste-buffer", "-b", buffer_name, "-t", self.session_name],
                check=True,
            )
            # Enter を送信
            subprocess.run(
                ["tmux", "send-keys", "-t", self.session_name, "", "Enter"],
                check=True,
            )
        finally:
            # 使い終わったバッファを削除 (PAT 等の機密情報をメモリから消す)
            subprocess.run(
                ["tmux", "delete-buffer", "-b", buffer_name],
                check=False,
            )
        logger.info(
            "Injected message to session %s (%d chars)",
            self.session_name, len(text),
        )

    async def wait_for_idle(self) -> None:
        """claude の応答完了を待つ.

        アルゴリズム:
          1. 注入直後の pane 内容を baseline として記録する
          2. pane が変化するまでポーリング (= claude が応答を始めた)
          3. pane 変化が止まって activity_idle_s 秒経過 → 完了と判断
          4. response_timeout_s 超過 → TimeoutError

        応答は claude が MCP tool (send_message) 経由で送信するため、
        bridge 側は pane テキストを解析する必要はない。
        idle 検知後に worker が hub.ack() を呼ぶだけでよい。
        """
        activity_idle_s = self._config.activity_idle_s
        response_timeout_s = self._config.response_timeout_s
        deadline = time.monotonic() + response_timeout_s

        baseline = self._capture_pane()
        activity_started = False
        last_change_time = time.monotonic()
        last_content = baseline

        # Phase 1: pane 変化待ち (claude が処理を開始した証拠)
        while time.monotonic() < deadline:
            await asyncio.sleep(_POLL_INTERVAL_S)
            content = self._capture_pane()
            if content != baseline:
                activity_started = True
                last_content = content
                last_change_time = time.monotonic()
                logger.debug("Session %s: activity started", self.session_name)
                break

        if not activity_started:
            raise TimeoutError(
                f"Session {self.session_name}: claude did not start processing "
                f"within {response_timeout_s:.0f}s"
            )

        # Phase 2: pane 変化が止まるまで待つ
        while time.monotonic() < deadline:
            await asyncio.sleep(_POLL_INTERVAL_S)
            content = self._capture_pane()
            if content != last_content:
                last_content = content
                last_change_time = time.monotonic()
            elif time.monotonic() - last_change_time >= activity_idle_s:
                logger.info(
                    "Session %s: idle for %.1fs — response complete",
                    self.session_name, activity_idle_s,
                )
                return

        raise TimeoutError(
            f"Session {self.session_name}: response timeout ({response_timeout_s:.0f}s)"
        )

    # ------------------------------------------------------------------ #
    # internals                                                            #
    # ------------------------------------------------------------------ #

    def _capture_pane(self) -> str:
        """tmux capture-pane -p で pane のテキストを取得する."""
        result = subprocess.run(
            ["tmux", "capture-pane", "-p", "-S", "-", "-t", self.session_name],
            capture_output=True,
            text=True,
        )
        return result.stdout

    def _build_cmd(self) -> list[str]:
        """claude 起動コマンドを組み立てる.

        _started_before が True なら --continue を付けて直前の会話を継続する。
        ANTHROPIC_API_KEY はスタートアップスクリプト経由で unset する。
        """
        cmd = [self._cli_path, "--mcp-config", str(self._mcp_config_path)]
        if self._started_before:
            cmd.append("--continue")
        if self._config.permission_bypass:
            cmd.append("--dangerously-skip-permissions")
        if self._config.model:
            cmd.extend(["--model", self._config.model])
        return cmd


# ──────────────────────────────────────────────────────────────────────── #
# SessionManager: on-demand spawn + idle timer                            #
# ──────────────────────────────────────────────────────────────────────── #


class SessionManager:
    """on-demand spawn と idle タイムアウト kill を担うセッションマネージャ.

    TmuxSession のライフサイクル (Cold -> Warm -> Cooling -> Cold) を管理し、
    worker からは `handle(prompt)` を呼ぶだけでよい抽象を提供する。

    Attributes:
        _session: 管理対象の TmuxSession。
        _idle_timer: idle タイムアウト後に stop() を呼ぶ asyncio.Task。
        _lock: 同一セッションへの同時アクセスを防ぐ Mutex。
    """

    def __init__(self, session: TmuxSession) -> None:
        self._session = session
        self._idle_timer: asyncio.Task[None] | None = None
        self._lock = asyncio.Lock()

    @classmethod
    def create(cls, config: Config) -> SessionManager:
        """SessionManager を生成する."""
        session = TmuxSession.create(config)
        return cls(session)

    async def handle(self, prompt: str) -> None:
        """メッセージを処理する.

        Cold なら Tier2 を spawn してから inject → wait_for_idle。
        完了後は idle timer をセット (idle_timeout_s 後に kill)。
        """
        async with self._lock:
            # idle timer をキャンセル (新メッセージ到着で wake)
            self._cancel_idle_timer()

            # Cold なら Tier2 を起動する
            if not self._session.is_alive():
                logger.info(
                    "Session %s is cold — spawning Tier2", self._session.session_name
                )
                await self._session.start()

            # メッセージを注入する
            await self._session.inject_message(prompt)

        # wait_for_idle は lock 外で実行 (長時間 block しないよう)
        try:
            await self._session.wait_for_idle()
        except TimeoutError as exc:
            logger.error("Response timeout for session %s: %s", self._session.session_name, exc)
            # タイムアウト時はセッションをリセットして次のメッセージに備える
            async with self._lock:
                await self._session.stop()
            raise

        async with self._lock:
            # idle timer を開始する
            self._start_idle_timer()

    def close(self) -> None:
        """リソースのクリーンアップ (idle timer キャンセル + MCP config 削除)."""
        self._cancel_idle_timer()
        self._session.close()

    async def shutdown(self) -> None:
        """セッションを graceful に停止する (bridge 終了時)."""
        self._cancel_idle_timer()
        await self._session.stop()
        self._session.close()

    # ------------------------------------------------------------------ #
    # idle timer                                                           #
    # ------------------------------------------------------------------ #

    def _start_idle_timer(self) -> None:
        self._idle_timer = asyncio.create_task(
            self._run_idle_timer(),
            name=f"idle-timer-{self._session.session_name}",
        )

    def _cancel_idle_timer(self) -> None:
        if self._idle_timer and not self._idle_timer.done():
            self._idle_timer.cancel()
            self._idle_timer = None

    async def _run_idle_timer(self) -> None:
        """idle_timeout_s 後に Tier2 を kill する."""
        try:
            await asyncio.sleep(self._session._config.idle_timeout_s)
            logger.info(
                "Idle timeout (%.0fs) — killing session %s",
                self._session._config.idle_timeout_s,
                self._session.session_name,
            )
            await self._session.stop()
        except asyncio.CancelledError:
            pass  # 新メッセージ到着でキャンセルされた (正常)


# ──────────────────────────────────────────────────────────────────────── #
# MCP config 生成                                                          #
# ──────────────────────────────────────────────────────────────────────── #


def _write_mcp_config(config: Config) -> Path:
    """claude --mcp-config に渡す一時 JSON ファイルを書く.

    claude_p bridge と同一形式。mode 0o600 で GITHUB_PAT を保護する。
    `TmuxSession.close()` 時に削除される。

    Issue: #84 caused_by 因果チェーン対応のため X-User-Id ヘッダを送る。
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
        suffix=".json", prefix=f"bridge-tmux-{config.user}-"
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
