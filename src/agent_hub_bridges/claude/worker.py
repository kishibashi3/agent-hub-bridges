"""Bridge worker main loop (claude).

実装段階 (= 旧 repo `agent-hub-bridge-claude` の milestone を引き継ぐ):
- M0: agent-hub MCP に attach、 register tool で @<user> を登録 (疎通) ✅
- M1: inbox subscribe → message を Claude に流す → 応答が send_message で hub へ ✅
- M2: PreToolUse hook で permission propagation (将来)
- M3: CLAUDE.md / settings 注入の正式対応、 session_id resume (将来)
- M_sdk: 旧 ``HubClient`` (= 同梱 ``hub.py``) を ``agent-hub-sdk`` に
  置換。 push + poll + heartbeat の 手書き 3-task ループを
  ``async with hub.inbox() as messages: async for msg in messages: …`` に集約。 ✅
- **M_monorepo (= 本 file)**: `agent-hub-bridges` monorepo に移植 + outer
  reconnect / `_summarize_exc` / `_format_prompt` を `_common/` に
  委譲。 挙動は 1:1 同等 (= 旧 repo PR #M_sdk 完了時の状態)。

reconnect は SDK 内部ではなく caller (= 本 file の `run_with_reconnect`) で
担当する。 SDK の M2 PR #11 で deferred、 SDK 側 reconnect は別 milestone。
"""

from __future__ import annotations

import contextlib
import json
import logging
import os
import sys
import tempfile
import time
from collections.abc import Callable, Iterator
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import anyio
from agent_hub_sdk import AgentHub, CommandRouter, HubSession, IncomingMessage
from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    HookMatcher,
    ResultMessage,
    SystemMessage,
    TextBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

from agent_hub_bridges._common.inventory import write_dead_marker, write_lost_hub_to_inventory
from agent_hub_bridges._common.journal import Journal
from agent_hub_bridges._common.prompt import format_peer_message_prompt
from agent_hub_bridges._common.reconnect import run_with_reconnect
from agent_hub_bridges.claude.blocking_commands import bash_pre_tool_use_hook
from agent_hub_bridges.claude.claude_runner import ClaudeRunner
from agent_hub_bridges.claude.config import Config
from agent_hub_bridges.claude.cursor import load_cursor, save_cursor
from agent_hub_bridges.claude.telemetry import (
    ToolUseRecord,
    build_traceparent,
    emit_span,
    make_subprocess_telemetry_env,
)
from agent_hub_bridges.claude.telemetry import configure as configure_telemetry

logger = logging.getLogger(__name__)

# issue #46: stdout スニフによる busy 判定ウィンドウ (秒)。
# ASSISTANT: ログが直近この秒数以内に流れていれば /status → "busy"。
# 60s に設定した根拠:
#   - Claude の tool use ターンは通常 10〜30s。複数ターンで 60s 超は稀。
#   - 短すぎると LLM が応答中なのに "idle" に見える誤検知が増える。
#   - 長すぎると「作業完了済みなのに "busy"」が続く。
# env AGENT_HUB_BUSY_WINDOW_S (float 秒) で上書き可能。
_BUSY_WINDOW_S = float(os.environ.get("AGENT_HUB_BUSY_WINDOW_S", "60"))

# issue #60: idle 後の自動 /compact。
# デフォルト 30 分 idle で /compact を実行してコンテキストを圧縮する。
# env BRIDGE_COMPACT_IDLE_MINUTES (float 分) で上書き可能。
# watchdog は _COMPACT_CHECK_INTERVAL_S ごとに idle を確認する。
_COMPACT_IDLE_S = float(os.environ.get("BRIDGE_COMPACT_IDLE_MINUTES", "30")) * 60
_COMPACT_CHECK_INTERVAL_S = 60.0

# issue #131: compact サマリー archive。
# BRIDGE_COMPACT_ARCHIVE_DIR が設定されていればそちらを使う。
# 未設定なら workdir/daily/ に保存する。
_COMPACT_ARCHIVE_DIR_ENV = "BRIDGE_COMPACT_ARCHIVE_DIR"


def _compact_archive_dir(workdir: Path | None) -> Path | None:
    """compact サマリーの archive ディレクトリを返す (issue #131).

    解決順位:
      1. ``BRIDGE_COMPACT_ARCHIVE_DIR`` 環境変数 (明示指定)
      2. ``{workdir}/daily/`` (workdir が設定されている場合)
      3. ``None`` (archive 無効 — workdir も env も未設定)
    """
    env_val = os.environ.get(_COMPACT_ARCHIVE_DIR_ENV)
    if env_val:
        return Path(env_val)
    if workdir is not None:
        return workdir / "daily"
    return None  # archive 無効 (workdir も BRIDGE_COMPACT_ARCHIVE_DIR も未設定)


def _append_compact_summary(summary: str, archive_dir: Path) -> None:
    """compaction サマリーを ``daily/YYYY-MM-DD.md`` に追記する (issue #131).

    書き込み失敗は WARNING ログのみで bridge を落とさない。

    フォーマット::

        ## compact @ 2026-06-07T13:45:30Z

        <summary text>

    Args:
        summary: /compact レスポンスから収集したサマリーテキスト。
        archive_dir: 保存先ディレクトリ (通常 ``{workdir}/daily/``)。
    """
    try:
        archive_dir.mkdir(parents=True, exist_ok=True)
        now = datetime.now(UTC)
        date_str = now.strftime("%Y-%m-%d")
        now_str = now.strftime("%Y-%m-%dT%H:%M:%SZ")
        daily_file = archive_dir / f"{date_str}.md"
        entry = f"\n## compact @ {now_str}\n\n{summary}\n"
        with open(daily_file, "a", encoding="utf-8") as f:
            f.write(entry)
        logger.info("[auto-compact] summary appended to %s", daily_file)
    except Exception:
        logger.warning(
            "[auto-compact] failed to write compact summary to %s",
            archive_dir,
            exc_info=True,
        )


class _ActivityTracker:
    """ASSISTANT: ログの最終時刻を追跡して ``/status`` の busy 判定に使う.

    issue #46: bridge が Claude LLM を呼び出し中 (ASSISTANT: ログが流れている)
    にもかかわらず ``/status`` が ``idle`` を返す問題への対処。

    ``set_status()`` による自己申告ではなく、実際に ASSISTANT: メッセージが
    受信されたタイミングを記録する「stdout スニフ」相当のアプローチ。

    ``_ActivityTracker`` は ``run_worker`` で 1 度だけ作成し、reconnect を
    またいで共有する (= cursor と同様の扱い)。
    """

    def __init__(self) -> None:
        self._last_active: float | None = None

    def mark_active(self) -> None:
        """ASSISTANT: メッセージを受信するたびに呼ぶ。"""
        self._last_active = time.monotonic()

    def status(self) -> str:
        """直近 ``_BUSY_WINDOW_S`` 秒以内にアクティブなら ``"busy"``。"""
        if self._last_active is None:
            return "idle"
        elapsed = time.monotonic() - self._last_active
        return "busy" if elapsed < _BUSY_WINDOW_S else "idle"


# issue #26: safety-net 発火推定のウィンドウ (秒)。
# NOTE: Approach C (bridge 側近似実装): SDK が push/poll の source を
# IncomingMessage に expose していないため、連続メッセージ間の gap で
# poll 経由 (= safety-net 発火) を推定する。
# gap >= _PUSH_SILENT_THRESHOLD_S → SSE push が黙っていた可能性あり → WARNING。
# 閾値の根拠: SDK の poll 間隔デフォルトは 30s (_DEFAULT_INBOX_POLL_INTERVAL_S)。
# 25s に設定することで poll 間隔より短い gap は無視し、長い gap を捕捉する。
# env AGENT_HUB_PUSH_SILENT_THRESHOLD_S (float 秒) で上書き可能。
_PUSH_SILENT_THRESHOLD_S = float(
    os.environ.get("AGENT_HUB_PUSH_SILENT_THRESHOLD_S", "25")
)


class _MessageGapTracker:
    """メッセージ受信間の gap を計測して safety-net 発火を推定する.

    issue #26: SDK の push_loop / poll_loop は session.py 内部に実装されており、
    bridge 側から push/poll の source に直接アクセスできない (Approach C: 近似)。
    連続メッセージ間の gap が ``_PUSH_SILENT_THRESHOLD_S`` 以上なら、
    poll 経由 (= safety-net 発火) の可能性があると WARNING を出す。

    精度の限界:
      - 単純に「しばらくメッセージが来なかっただけ」との区別が不可能。
      - SDK が push/poll の source を IncomingMessage に expose するまでは
        近似に留まる。正確な実装は SDK 側 follow-up issue で対応予定。
      - ``_MessageGapTracker`` は ``run_worker`` で 1 度だけ作成し、
        reconnect をまたいで共有する (= cursor / tracker と同様)。
    """

    def __init__(self) -> None:
        self._last_received_at: float | None = None

    def on_message_received(self, msg_id: str) -> None:
        """message 受信時に呼ぶ。gap が閾値以上なら WARNING を emit する。"""
        now = time.monotonic()
        if self._last_received_at is not None:
            gap = now - self._last_received_at
            if gap >= _PUSH_SILENT_THRESHOLD_S:
                logger.warning(
                    "[safety-net] message %s arrived %.0fs after previous "
                    "(>= %.0fs threshold) — SSE push may have been silent; "
                    "poll fallback likely fired "
                    "(approximate: SDK source not exposed to bridge)",
                    msg_id,
                    gap,
                    _PUSH_SILENT_THRESHOLD_S,
                )
        self._last_received_at = now


class _IdleCompactWatchdog:
    """idle 後に ``/compact`` を自動実行する watchdog.

    issue #60: bridge が一定時間メッセージを受信しない (idle) 状態になったら
    ``/compact`` をトリガーしてコンテキストを圧縮する。

    使い方:
      - メッセージ受信ごとに ``reset()`` を呼ぶ。
      - LLM 呼び出し中 (``_handle_one`` の実行期間) は ``set_busy()`` /
        ``clear_busy()`` で busy 状態を通知する。busy 中は /compact をスキップ
        して stream の競合 (= 2 つのコルーチンが同じ ``receive_response()``
        ストリームを読む競合) を防ぐ (issue #102)。
      - ``watch_and_compact(runner)`` を background task として起動する
        (``anyio.create_task_group`` で並走)。

    NOTE: 実運用では ``run_worker`` が ``watch_and_compact_lazy`` を直接
    起動する (lazy runner 初期化構造に対応するため)。``watch_and_compact``
    はテスト・runner が確定している埋め込みシナリオ向け。

    ``idle_s`` のデフォルトは ``_COMPACT_IDLE_S`` (= ``BRIDGE_COMPACT_IDLE_MINUTES``
    環境変数、未設定なら 30 分)。テストでは小さな値を渡して動作を確認できる。
    ``check_interval_s`` のデフォルトは ``_COMPACT_CHECK_INTERVAL_S`` (60 秒)。
    """

    def __init__(
        self,
        idle_s: float = _COMPACT_IDLE_S,
        check_interval_s: float = _COMPACT_CHECK_INTERVAL_S,
        workdir: Path | None = None,
    ) -> None:
        self._idle_s = idle_s
        self._check_interval_s = check_interval_s
        self._last_activity: float = time.monotonic()
        self._processing: bool = False
        # issue #131: archive ディレクトリ (None なら保存しない)
        self._archive_dir: Path | None = _compact_archive_dir(workdir)

    def reset(self) -> None:
        """メッセージ受信時に呼ぶ。idle タイマーをリセットする。"""
        self._last_activity = time.monotonic()

    def set_busy(self) -> None:
        """``_handle_one`` 開始時に呼ぶ。watchdog が /compact をスキップするようにする。

        issue #102: サブエージェント実行中などの長時間処理で watchdog が
        ``query()``/``receive_response()`` を並走させると、2 つのコルーチンが
        同じ ``MemoryObjectReceiveStream`` を競合読みしてプロトコルが壊れる。
        ``_processing`` フラグで busy 期間を明示的に通知してスキップする。
        """
        self._processing = True

    def clear_busy(self) -> None:
        """``_handle_one`` 終了時に呼ぶ (finally で必ず呼ぶこと)。"""
        self._processing = False

    def is_processing(self) -> bool:
        """``_handle_one`` が実行中 (LLM が応答待ち) なら ``True``。"""
        return self._processing

    def idle_elapsed(self) -> float:
        """最後にリセットしてから経過した秒数を返す。"""
        return time.monotonic() - self._last_activity

    def is_idle(self) -> bool:
        """idle 閾値を超えていれば ``True``。"""
        return self.idle_elapsed() >= self._idle_s

    async def _run_compact_and_archive(self, client: ClaudeSDKClient) -> None:
        """/compact を実行しサマリーを archive に追記する (issue #160).

        ``watch_and_compact`` / ``watch_and_compact_lazy`` の共通実装。
        以下を順に行う:

        1. ``client.query("/compact")`` で /compact をトリガー。
        2. ``receive_response()`` ストリームから ``AssistantMessage`` の
           ``TextBlock`` を収集してサマリーテキストを組み立てる。
        3. ``self._archive_dir`` が設定されていれば
           :func:`_append_compact_summary` で daily ファイルに追記する。

        RuntimeError / Exception のキャッチは呼び出し元 (``watch_and_compact``
        / ``watch_and_compact_lazy``) で行う。本メソッドは例外をそのまま上げる。

        Args:
            client: ``ClaudeSDKClient`` インスタンス (``runner.client``)。
        """
        await client.query("/compact")
        # issue #131: サマリーテキストを AssistantMessage から収集する
        summary_parts: list[str] = []
        async for sdk_msg in client.receive_response():
            if isinstance(sdk_msg, AssistantMessage):
                for block in sdk_msg.content:
                    if isinstance(block, TextBlock):
                        summary_parts.append(block.text)
            if isinstance(sdk_msg, ResultMessage):
                break
        logger.info("[auto-compact] /compact completed, timer reset")
        # issue #131: サマリーを daily ファイルに追記する
        if self._archive_dir is not None:
            summary = "\n\n".join(summary_parts).strip()
            if not summary:
                summary = "(no summary text captured from /compact response)"
            _append_compact_summary(summary, self._archive_dir)

    async def watch_and_compact(self, runner: ClaudeRunner) -> None:
        """background task: idle 検知 → ``/compact`` 実行 → タイマーリセット。

        ``anyio.create_task_group`` で ``run_with_reconnect`` と並走させる。
        以下の例外は安全に読み捨てて継続する (bridge 全体を落とさないため):
          - ``RuntimeError``: runner が restart 中 (``_client is None``)
          - それ以外の ``Exception``: /compact 失敗

        ``anyio.get_cancelled_exc_class()`` は伝播する
        (= task group の tear-down 時に正常終了)。

        issue #102: ``_handle_one`` が実行中 (``_processing == True``) の間は
        /compact をスキップして stream 競合を防ぐ。タイマーはリセットして
        tight retry を防ぐ。
        """
        cancelled_exc = anyio.get_cancelled_exc_class()
        while True:
            await anyio.sleep(self._check_interval_s)
            if self.is_processing():
                # _handle_one が実行中: stream 競合を防ぐためスキップ。
                # タイマーをリセットして次の check interval まで待つ。
                self.reset()
                logger.debug("[auto-compact] _handle_one in progress, skip compact")
                continue
            if not self.is_idle():
                continue
            elapsed = self.idle_elapsed()
            logger.info(
                "[auto-compact] idle %.0fs >= threshold %.0fs, running /compact ...",
                elapsed,
                self._idle_s,
            )
            try:
                client = runner.client  # RuntimeError if restarting
                await self._run_compact_and_archive(client)
            except cancelled_exc:
                raise
            except RuntimeError:
                # runner が restart 中は /compact を skip して timer だけリセット
                logger.debug(
                    "[auto-compact] runner not ready (restart in progress), skip"
                )
            except Exception as exc:
                logger.warning("[auto-compact] /compact failed: %s", exc)
            finally:
                # 失敗・skip 問わずタイマーをリセットし、tight retry を防ぐ
                self.reset()

    async def watch_and_compact_lazy(
        self, get_runner: Callable[[], ClaudeRunner | None]
    ) -> None:
        """background task 版 (lazy runner 対応): runner が None の間はスキップする.

        issue #91: runner が lazily 初期化される構造に対応するため、
        ``get_runner()`` callable を受け取る。``None`` を返す間はタイマーを
        リセットしてスキップする。runner が確定してから ``watch_and_compact``
        と同じ動作をする。

        issue #102: ``_handle_one`` が実行中 (``_processing == True``) の間は
        /compact をスキップして stream 競合を防ぐ。タイマーはリセットして
        tight retry を防ぐ。

        ``get_runner``: ``ClaudeRunner | None`` を返す callable。
        """
        cancelled_exc = anyio.get_cancelled_exc_class()
        while True:
            await anyio.sleep(self._check_interval_s)
            runner = get_runner()
            if runner is None:
                # Runner not yet initialized — reset timer to avoid spurious compact
                self.reset()
                continue
            if self.is_processing():
                # _handle_one が実行中: stream 競合を防ぐためスキップ。
                # タイマーをリセットして次の check interval まで待つ。
                self.reset()
                logger.debug("[auto-compact] _handle_one in progress, skip compact")
                continue
            if not self.is_idle():
                continue
            elapsed = self.idle_elapsed()
            logger.info(
                "[auto-compact] idle %.0fs >= threshold %.0fs, running /compact ...",
                elapsed,
                self._idle_s,
            )
            try:
                client = runner.client  # RuntimeError if restarting
                await self._run_compact_and_archive(client)
            except cancelled_exc:
                raise
            except RuntimeError:
                logger.debug(
                    "[auto-compact] runner not ready (restart in progress), skip"
                )
            except Exception as exc:
                logger.warning("[auto-compact] /compact failed: %s", exc)
            finally:
                self.reset()


@contextlib.contextmanager
def _mcp_config_file(config: Config) -> Iterator[Path]:
    """agent-hub の MCP config を一時 file に書き出す (PAT を ps に出さないため).

    本 helper は Claude Agent SDK 側 が ``mcp__agent-hub__*`` tools を呼ぶ
    ために必要 (= bridge 自身の inbox subscribe 用 session とは別接続)。
    Claude 側 が agent-hub を呼ぶ path は file-based config なので本関数は
    引き続き必要 (= claude bridge 専用、 `_common` に抽出しない)。
    """
    headers: dict[str, str] = {
        "Authorization": f"Bearer {config.github_pat}",
        "X-User-Id": config.user,
    }
    if config.tenant:
        headers["X-Tenant-Id"] = config.tenant

    payload = {
        "mcpServers": {
            "agent-hub": {
                "type": "http",
                "url": config.agent_hub_url,
                "headers": headers,
            },
        },
    }

    fd, path_str = tempfile.mkstemp(prefix="agent-hub-bridge-claude-", suffix=".json")
    path = Path(path_str)
    try:
        os.chmod(path, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(payload, f)
        yield path
    finally:
        with contextlib.suppress(FileNotFoundError):
            path.unlink()


def _build_options(
    config: Config,
    mcp_config_path: Path,
    *,
    traceparent: str | None = None,
    telemetry_url: str | None = None,
) -> ClaudeAgentOptions:
    """Claude SDK の options を組み立てる.

    bridge は「入力経路を agent-hub に差し替えただけの Claude Code」を目指す。
    振る舞いは workdir の CLAUDE.md / project .claude/settings に従う。

    ``model`` は ``Config`` 経由で CLI ``--model`` / env ``AGENT_HUB_MODEL`` /
    内蔵 default (= ``claude-sonnet-4-6``) のいずれかが解決済 で入る。
    SDK の alias resolver が ``claude-sonnet-4-6`` のような family alias を
    受け付ける (= 同 family の point release で勝手に上がる) ので、 bridge は
    date-pinned form ではなく family alias を default にしてる。

    issue #91: ``telemetry_url`` が設定されている場合、Claude CLI subprocess の
    OTel telemetry を有効化する環境変数を ``ClaudeAgentOptions.env`` に設定する。
    ``traceparent`` が渡された場合はさらに ``TRACEPARENT`` を env に注入することで、
    ``claude_code.llm_request`` span が受信 msg_id の trace の子 span になる。
    """
    env: dict[str, str] = {}
    # None (未設定) と "" (空文字) をいずれも「telemetry 無効」として扱う (opt-in)。
    if telemetry_url:
        env.update(make_subprocess_telemetry_env(telemetry_url))
        if traceparent:
            env["TRACEPARENT"] = traceparent

    return ClaudeAgentOptions(
        # str (file path) として渡し、 CLI 引数経由の PAT 露出を回避
        mcp_servers=str(mcp_config_path),
        cwd=str(config.workdir),
        model=config.model,
        # issue #20: workdir 以外の追加ディレクトリ (--add-dir で指定)。
        # 空 list なら SDK は --add-dir を渡さない (= 旧来挙動と同じ)。
        add_dirs=list(config.add_dirs),
        # 確認 UI は出さない (CLI なので元々出ないが明示)。 M2 で hook 経由の
        # propagation に置き換える。
        permission_mode="bypassPermissions",
        # user-level の plugin marketplace は読まない (agent-hub-plugin の
        # auto-engage を防ぐ)。 workdir の CLAUDE.md / .claude/settings は読む。
        setting_sources=["project", "local"],
        # issue #101: Bash ツール実行前にブロッキングコマンドを検出して拒否する。
        # bypassPermissions では can_use_tool は呼ばれないため hooks を使う。
        # hooks は permission mode に関わらず全 tool call の前に実行される。
        hooks={
            "PreToolUse": [
                HookMatcher(matcher="Bash", hooks=[bash_pre_tool_use_hook]),
            ],
        },
        **({"env": env} if env else {}),
    )


def _format_message(msg: object) -> str:
    """SDK message を 1 行に整形してログ出力用."""
    if isinstance(msg, AssistantMessage):
        parts = []
        for block in msg.content:
            if isinstance(block, TextBlock):
                parts.append(f"[text] {block.text}")
            elif isinstance(block, ToolUseBlock):
                parts.append(f"[tool_use] {block.name}({block.input})")
            else:
                parts.append(f"[{type(block).__name__}]")
        return "ASSISTANT: " + " | ".join(parts)
    if isinstance(msg, UserMessage):
        parts = []
        for block in msg.content if isinstance(msg.content, list) else [msg.content]:
            if isinstance(block, ToolResultBlock):
                parts.append(f"[tool_result] {str(block.content)[:200]}")
            elif isinstance(block, str):
                parts.append(f"[text] {block}")
            else:
                parts.append(f"[{type(block).__name__}]")
        return "USER: " + " | ".join(parts)
    if isinstance(msg, SystemMessage):
        return f"SYSTEM: {msg.subtype}"
    if isinstance(msg, ResultMessage):
        return (
            f"RESULT: turns={msg.num_turns}, "
            f"cost=${msg.total_cost_usd or 0:.4f}, "
            f"duration={msg.duration_ms}ms"
        )
    return f"{type(msg).__name__}: {msg!r}"


async def run_worker(config: Config) -> None:
    """Bridge worker メインループ.

    `_common.reconnect.run_with_reconnect` で outer reconnect loop を回す。
    `ClaudeRunner` (= peer ごとの会話履歴を持つ `ClaudeSDKClient` の in-place
    restart 対応 wrapper) は hub session の最初のメッセージ受信時に lazily 初期化する
    (issue #91)。これにより最初の msg.id から TRACEPARENT を生成して subprocess の
    trace root に設定できる。hub reconnect 時は新しい runner を作り直す。

    ``/restart`` (= agent-hub-sdk M6, issue #26) を受信した時は runner 内部で
    ``ClaudeSDKClient`` の close + open が走り、 conversation history が リセットされる。
    """
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    # issue #96: telemetry service.name を @handle 名に設定する。
    # _get_tracer() の遅延初期化より前に呼ぶ必要があるため、 run_worker 先頭で設定する。
    configure_telemetry(service_name=f"@{config.user}")

    logger.info(
        "Starting agent-hub-bridge-claude as @%s (workdir=%s, tenant=%s, model=%s)",
        config.user,
        config.workdir,
        config.tenant or "default",
        config.model,
    )

    # issue #37: bridge 再起動後のメッセージ重複 dispatch を防ぐ timestamp cursor。
    # cursor は outer reconnect loop を跨いで 1 度だけ load し、 セッション間で
    # 共有する (= 再接続しても同じ cursor を使い回す)。
    cursor = load_cursor(config.user)

    # issue #183 / agent-hub#168: bridge が直接呼ぶ hub.send() を journal で保護。
    # cursor と同様に outer reconnect loop を跨いで 1 インスタンスを共有する。
    journal = Journal(config.user)

    # issue #46: stdout スニフによる /status busy 判定。cursor と同様に
    # reconnect をまたいで 1 インスタンスを共有する。
    tracker = _ActivityTracker()

    # issue #26: メッセージ受信間 gap による safety-net 発火推定。
    # reconnect をまたいで 1 インスタンスを共有する。
    gap_tracker = _MessageGapTracker()

    # issue #60: idle 後の自動 /compact watchdog。
    # reconnect をまたいで 1 インスタンスを共有する (= cursor / tracker と同様)。
    # issue #131: workdir を渡して compact サマリーを daily ファイルに保存する。
    compact_watchdog = _IdleCompactWatchdog(workdir=config.workdir)

    # issue #91: AGENT_HUB_TELEMETRY_URL が設定されている場合、subprocess の OTel を有効化。
    telemetry_url = os.environ.get("AGENT_HUB_TELEMETRY_URL")

    # issue #91: runner は hub session の最初のメッセージ受信時に lazily 初期化する。
    # runner_holder[0] が None の間は runner 未初期化。
    # hub reconnect ごとに新しい runner を作り直す (_run_hub_session 内で tear down)。
    runner_holder: list[ClaudeRunner | None] = [None]

    with _mcp_config_file(config) as mcp_config_path:
        async def _one_session() -> None:
            nonlocal cursor
            cursor = await _run_hub_session(
                config,
                mcp_config_path,
                runner_holder,
                cursor,
                tracker,
                gap_tracker,
                compact_watchdog,
                journal,
                telemetry_url=telemetry_url,
            )

        async def _on_circuit_open() -> None:
            """circuit breaker 発火時コールバック: dead marker + inventory 更新.

            issue #82: hub 接続が N 回連続で失敗したら graceful shutdown する前に
            dead marker file を書いて operator の stop-bridge.sh --dead に通知する。
            BRIDGE_INVENTORY が設定されていれば inventory に lost-hub エントリを追記。
            """
            pid = os.getpid()
            write_dead_marker(config.user)
            write_lost_hub_to_inventory(config.user, pid=pid)
            logger.critical(
                "[circuit-breaker] ALERT: @%s hub connection lost. "
                "Dead marker written. Run 'stop-bridge.sh --dead' to clean up.",
                config.user,
            )

        async def _run_reconnect() -> None:
            await run_with_reconnect(
                _one_session,
                name="hub session (claude)",
                on_circuit_open=_on_circuit_open,
            )

        # issue #60: compact_watchdog を run_with_reconnect と並走させる。
        # issue #91: runner が lazy init される構造に対応するため watch_and_compact_lazy を使う。
        # どちらか一方が例外で死ぬともう一方も cancel される (anyio 標準挙動)。
        async with anyio.create_task_group() as tg:
            tg.start_soon(_run_reconnect)
            tg.start_soon(compact_watchdog.watch_and_compact_lazy, lambda: runner_holder[0])


async def _startup_catchup(
    hub: HubSession,
    config: Config,
    mcp_config_path: Path,
    runner_holder: list[ClaudeRunner | None],
    cursor: str | None,
    tracker: _ActivityTracker,
    gap_tracker: _MessageGapTracker,
    compact_watchdog: _IdleCompactWatchdog,
    journal: Journal,
    router: CommandRouter,
    *,
    telemetry_url: str | None = None,
) -> str | None:
    """bridge 起動時に未読メッセージを処理する startup catchup (issue #98).

    hub 接続確立後・inbox ループ開始前に ``get_messages`` を呼んで、
    オフライン中に届いたメッセージを処理する。

    コマンドメッセージ (body が "/" で始まる) は inbox ループの
    CommandRouter に委ねるためスキップする (= ack しない)。
    自然言語メッセージは通常の inbox ループと同じ処理を行う:

      1. ``gap_tracker.on_message_received()`` でメッセージ間隔を計測
      2. ``compact_watchdog.reset()`` で idle タイマーをリセット
      3. runner が未初期化なら lazy init (issue #91 と同じロジック)
      4. cursor check: 既読メッセージは ack して skip
      5. ``_handle_one()`` でメッセージ処理
      6. ``save_cursor()`` + ``hub.ack()`` で状態永続化

    ``get_unread()`` 呼び出しが失敗した場合は WARNING を記録して
    cursor をそのまま返す (graceful degradation)。

    Returns:
        処理後の cursor 値。未読がなければ入力 cursor をそのまま返す。
    """
    try:
        msgs = await hub.get_unread()
    except Exception:
        logger.warning(
            "[startup-catchup] get_messages failed; skipping startup catchup",
            exc_info=True,
        )
        return cursor

    # コマンドメッセージ (/ で始まる) は inbox loop の CommandRouter に委ねる
    nl_msgs = [m for m in msgs if not m.body.strip().startswith("/")]
    cmd_count = len(msgs) - len(nl_msgs)

    if not nl_msgs:
        if cmd_count > 0:
            logger.info(
                "[startup-catchup] %d command message(s) only; deferred to inbox loop",
                cmd_count,
            )
        else:
            logger.info("[startup-catchup] no unread messages at startup")
        return cursor

    logger.info(
        "[startup-catchup] %d unread message(s) to process (+ %d command(s) deferred)",
        len(nl_msgs),
        cmd_count,
    )

    for msg in nl_msgs:
        # issue #37: 再起動後の重複 dispatch 防止。cursor-skip を
        # runner lazy init より先に置くことで、replay メッセージ
        # (= cursor 以前) の msg.id が trace root にならないようにする。
        if cursor is not None and msg.timestamp <= cursor:
            logger.info(
                "[startup-catchup] skipping seen message %s (ts=%s, cursor=%s)",
                msg.id,
                msg.timestamp,
                cursor,
            )
            await hub.ack(msg.id)
            continue

        gap_tracker.on_message_received(msg.id)
        compact_watchdog.reset()

        # issue #91: runner を最初の非 cursor-skip メッセージ受信時に lazy init する。
        # 最初の msg.id から TRACEPARENT を生成して subprocess の trace root に設定する。
        if runner_holder[0] is None:
            traceparent = build_traceparent(msg.id) if telemetry_url else None
            opts = _build_options(
                config,
                mcp_config_path,
                traceparent=traceparent,
                telemetry_url=telemetry_url,
            )
            _runner = ClaudeRunner(opts)
            await _runner.__aenter__()
            runner_holder[0] = _runner
            router.set_restart_handler(_runner.restart)
            logger.info(
                "[startup-catchup] Claude session started (first msg: %s, traceparent: %s)",
                msg.id,
                traceparent or "none",
            )

        # issue #102: stream 競合防止のため _handle_one の前後で
        # compact_watchdog の busy フラグを set/clear する。
        compact_watchdog.set_busy()
        try:
            await _handle_one(
                hub,
                runner_holder[0].client,
                msg,
                config,
                tracker,
                journal,
            )
        finally:
            compact_watchdog.clear_busy()
        # process → save_cursor → ack の順 (crash-safe)。
        save_cursor(config.user, msg.timestamp)
        cursor = msg.timestamp
        await hub.ack(msg.id)

    return cursor


async def _run_hub_session(
    config: Config,
    mcp_config_path: Path,
    runner_holder: list[ClaudeRunner | None],
    cursor: str | None,
    tracker: _ActivityTracker,
    gap_tracker: _MessageGapTracker,
    compact_watchdog: _IdleCompactWatchdog,
    journal: Journal,
    *,
    telemetry_url: str | None = None,
) -> str | None:
    """1 回分の hub session を最後まで走らせる.

    `AgentHub.connect` → `hub.inbox(commands=router)` の async iterator を
    `async for` で 回すだけ。 push / poll / heartbeat / `/ping` intercept は
    全部 SDK 側。 session が死ぬと iterator 内部 task が例外を上げ、
    `hub.inbox()` の `async with` 出口で transport が tear down し、 本関数
    から例外が伝播して 上位 `run_with_reconnect` の retry に乗る。

    ``CommandRouter`` を構築して ``commands=router`` で inbox に渡すと、
    SDK が ``/ping`` / ``/status`` / ``/help`` / ``/restart`` を自動 handle
    する。 ``async for msg in messages:`` に到達するのは natural language
    の DM のみ (= ``_handle_one`` で LLM に流す対象)。

    ``/restart`` の動作 (= agent-hub-sdk M6, issue #26):
      - SDK が ``"restarting..."`` を sender に送信
      - ``router.set_restart_handler`` で注入した ``runner.restart`` を await
      - ``runner.restart`` が old ``ClaudeSDKClient`` を close → 新規 open
      - return 後、 SDK が ``"ready"`` を送信 + ack
      - 失敗時は SDK が generic warning を送信 + ack (= どちらも ack 必ず走る)

    SDK M5 (agent-hub-sdk#27, merge ``fc4a4cd``) auto-registers as part
    of ``AgentHub.connect``. We additionally call ``hub.register()``
    explicitly after connect (issue #83) to log the confirmed
    display_name/mode for operator visibility, and to ensure the
    registration is reflected immediately even when SDK auto-register
    timing varies. Server-side ``register`` is idempotent.

    issue #37: ``cursor`` は再起動をまたいで最後に処理した message の
    timestamp を保持する。 ``msg.timestamp <= cursor`` のメッセージは
    skip + ack することで重複 dispatch を防ぐ。 正常処理時の順序は:
      1. ``_handle_one`` で LLM に流す (process)
      2. ``save_cursor`` で timestamp を永続化
      3. ``hub.ack`` でサーバに既読通知

    issue #91: ``ClaudeRunner`` は最初のメッセージ受信時に lazily 初期化する
    (``runner_holder[0]`` が None の間は未初期化)。最初の msg.id から
    TRACEPARENT を生成して subprocess の trace root に設定する。
    hub session 終了 (inbox loop 脱出) 時に runner を tear down する。

    Returns: セッション終了時点の cursor 値 (= 上位 reconnect loop で持ち越す)。
    """
    # CommandRouter (= agent-hub-sdk M2.1) を built-in commands ON で構築。
    # issue #91: runner が lazy init されるため、初期化前は router への restart
    # handler 注入を遅延する。最初のメッセージ受信時に set_restart_handler を呼ぶ。
    #
    # issue #46: ``/status`` をカスタムハンドラで上書き。SDK 組み込みの
    # ``hub._status`` (= 常 "idle") ではなく ``_ActivityTracker.status()``
    # を返す。これにより ASSISTANT: ログが直近流れていれば "busy" を返せる。
    router = CommandRouter()

    @router.command("/status", description="bridge state (idle/busy)")
    async def _status_handler(
        _msg: IncomingMessage, _hub: HubSession, _args: str
    ) -> str:
        return tracker.status()

    async with AgentHub.connect(
        participant=config.user,
        tenant=config.tenant,
        display_name=config.display_name,
        url=config.agent_hub_url,
        pat=config.github_pat,
        client_type="agent-hub-bridge/claude",
    ) as hub:
        # SDK auto-registers in connect(). We call register() again explicitly
        # to surface the confirmed display_name in the log for operator visibility.
        # server-side register is idempotent.
        try:
            confirmed = await hub.register()
            logger.info(
                "Registered @%s (display_name=%r): %s",
                config.user,
                config.display_name,
                confirmed,
            )
        except Exception:
            logger.warning(
                "Explicit register() after connect failed for @%s — "
                "auto-register from connect() should still be active",
                config.user,
                exc_info=True,
            )

        logger.info(
            "Hub session ready (@%s), listening on inbox...",
            config.user,
        )

        # issue #183 / agent-hub#168: 前回クラッシュ時の pending entry を replay。
        # hub 接続確立直後に行うことで、送信先が online になってから再送できる。
        await _replay_journal(hub, journal)

        # issue #98: bridge 起動時に未読メッセージを処理する。
        # オフライン中に届いたメッセージを inbox ループ開始前に処理する。
        cursor = await _startup_catchup(
            hub,
            config,
            mcp_config_path,
            runner_holder,
            cursor,
            tracker,
            gap_tracker,
            compact_watchdog,
            journal,
            router,
            telemetry_url=telemetry_url,
        )

        try:
            async with hub.inbox(commands=router) as messages:
                async for msg in messages:
                    # ``commands=router`` が ``/ping`` / ``/status`` / ``/help``
                    # / ``/restart`` を SDK 側 で intercept 済。 ここに到達する
                    # のは natural language メッセージのみ。 ``runner.client``
                    # は per-message に読むので、 ``/restart`` で session が
                    # re-spawn された直後の次メッセージから新 client が使われる。

                    # issue #26: safety-net 発火推定。cursor skip より前に呼ぶことで
                    # 重複 skip されたメッセージも含む全着信 gap を計測する。
                    gap_tracker.on_message_received(msg.id)

                    # issue #60: メッセージ受信で idle タイマーをリセット。
                    compact_watchdog.reset()

                    # issue #37: 再起動後の重複 dispatch 防止。cursor-skip を
                    # runner lazy init より先に置くことで、replay メッセージ
                    # (= cursor 以前) の msg.id が trace root にならないようにする。
                    # NOTE: ISO-8601 UTC 文字列 (例: "2026-05-21T12:00:00.000Z") は
                    # 辞書順比較 (<=) が時系列順と一致する。これは server が
                    # 一貫した形式を返す前提。server 実装を変えた場合は
                    # `datetime.fromisoformat()` でのパースに切り替えること。
                    if cursor is not None and msg.timestamp <= cursor:
                        logger.info(
                            "Skipping already-seen message %s (ts=%s, cursor=%s)",
                            msg.id,
                            msg.timestamp,
                            cursor,
                        )
                        await hub.ack(msg.id)
                        continue

                    # issue #91: runner を最初の非 cursor-skip メッセージ受信時に
                    # lazy init する。最初の msg.id から TRACEPARENT を生成して
                    # subprocess の trace root に設定する。hub reconnect ごとに
                    # 新しい runner を作り直す。
                    if runner_holder[0] is None:
                        # None (未設定) と "" (空文字) をいずれも「telemetry 無効」
                        # として扱う (opt-in)。
                        traceparent = (
                            build_traceparent(msg.id) if telemetry_url else None
                        )
                        opts = _build_options(
                            config,
                            mcp_config_path,
                            traceparent=traceparent,
                            telemetry_url=telemetry_url,
                        )
                        _runner = ClaudeRunner(opts)
                        await _runner.__aenter__()
                        runner_holder[0] = _runner
                        router.set_restart_handler(_runner.restart)
                        logger.info(
                            "Claude session started (first msg: %s, traceparent: %s)",
                            msg.id,
                            traceparent or "none",
                        )

                    # issue #102: stream 競合防止のため _handle_one の前後で
                    # compact_watchdog の busy フラグを set/clear する。
                    compact_watchdog.set_busy()
                    try:
                        await _handle_one(
                            hub,
                            runner_holder[0].client,
                            msg,
                            config,
                            tracker,
                            journal,
                        )
                    finally:
                        compact_watchdog.clear_busy()
                    # process → save_cursor → ack の順 (crash-safe)。
                    # save_cursor 後 ack 前にクラッシュしても、 再起動後に
                    # cursor で skip されるので二重 dispatch にならない。
                    save_cursor(config.user, msg.timestamp)
                    cursor = msg.timestamp
                    await hub.ack(msg.id)
        finally:
            # issue #91: hub session 終了時に runner を tear down する。
            # 次の hub session では新しい runner を作り直す。
            _r = runner_holder[0]
            if _r is not None:
                runner_holder[0] = None
                await _r.__aexit__(None, None, None)

    return cursor


async def _journalled_send(
    hub: HubSession,
    journal: Journal,
    *,
    to: str,
    message: str,
    caused_by: str | None = None,
) -> None:
    """journal write → hub.send → journal delete の順で送信を永続化する。

    bridge が直接呼ぶ ``hub.send()`` を wrap するヘルパー。

    フロー::

        1. journal.write(entry)  ← クラッシュしても次回起動時に replay される
        2. hub.send(...)
        3. journal.delete(entry.id)  ← 送信成功時のみ削除

    hub.send が失敗した場合、 entry は journal に残り次回起動時に
    :func:`_replay_journal` で再送される。

    NOTE: 冪等性 (idempotency_key) は TODO (issue #183)。
          現時点では at-least-once セマンティクス (replay 時に重複送信の可能性あり)。
    """
    entry = journal.make_entry(to=to, message=message, caused_by=caused_by)
    # write → send → delete の順。write 失敗時は send を中止して不変式を守る
    # (reviewer Critical: issue #183)。
    # 「journal に書いてから send」が crash-safety の核心であり、
    # write 失敗のまま send すると crash 後にメッセージが消失する。
    if not journal.write(entry):
        raise RuntimeError(
            f"Journal write failed for entry {entry.id} (to={to}); send aborted"
        )
    try:
        await hub.send(to=to, message=message, caused_by=caused_by)
    except Exception:
        logger.warning(
            "hub.send failed for journal entry %s (to=%s); "
            "entry kept in journal for replay on next startup",
            entry.id,
            to,
        )
        raise
    journal.delete(entry.id)


async def _replay_journal(hub: HubSession, journal: Journal) -> None:
    """起動時に pending journal entries を replay する (issue #183 / agent-hub#168)。

    bridge クラッシュ時に送信できなかったメッセージを再送する。
    失敗したエントリは journal に残し、次回起動時に再試行する。

    NOTE: 冪等性 (idempotency_key) は TODO (issue #183)。
          現時点では at-least-once セマンティクス (重複送信の可能性あり)。
    """
    entries = journal.load_all()
    if not entries:
        return
    logger.warning(
        "Journal replay: %d pending entry(ies) found — replaying (crash recovery)",
        len(entries),
    )
    for entry in entries:
        logger.info(
            "Replaying journal entry %s (to=%s, created_at=%s)",
            entry.id,
            entry.to,
            entry.created_at,
        )
        try:
            await hub.send(to=entry.to, message=entry.message, caused_by=entry.caused_by)
            journal.delete(entry.id)
            logger.info("Journal entry %s replayed successfully", entry.id)
        except Exception:
            logger.exception(
                "Failed to replay journal entry %s (to=%s); will retry on next startup",
                entry.id,
                entry.to,
            )
            # 失敗しても次のエントリを試みる


async def _handle_one(
    hub: HubSession,
    claude: ClaudeSDKClient,
    msg: IncomingMessage,
    config: Config,
    tracker: _ActivityTracker,
    journal: Journal,
) -> None:
    """message 1 件を Claude に流して応答を待つ.

    `claude.query` の `session_id` を sender にすることで、 peer ごとに
    会話 context が 分離される (= M3 の stateful 化の基礎)。

    NOTE: `hub.ack(msg.id)` は呼出元 (= `_run_hub_session` の `async for`
    body) で 1 行下に書く (= caller が ack)。

    issue #46: SDK から ``AssistantMessage`` を受信するたびに
    ``tracker.mark_active()`` を呼ぶ。これにより ``/status`` が
    ``_handle_one`` 完了直後に処理された際に ``"busy"`` を返せる。

    issue #51: ``config.workdir`` の存在確認を tool 実行前に行う。
    workdir が存在しない場合は ERROR ログ + sender への fallback DM を
    送って early return する。``hub.ack`` は caller が担当するため、
    ここで return するだけで ack が実行されて再配信ループを防げる。
    ``Config.from_env_and_args`` の起動時検証と二重になるが、bridge
    起動後に workdir が削除される edge case への guard として必要。

    issue #183 / agent-hub#168: bridge が直接呼ぶ hub.send() は
    ``_journalled_send()`` 経由にして crash-safe にする。
    Claude が MCP tool 経由で呼ぶ ``mcp__agent-hub__send_message`` は
    bridge 側でインターセプトできないため対象外 (server 側 WAL で保護 = PR #182)。
    """
    # issue #51: workdir が存在しない場合は early return。
    # caller (_run_hub_session) が hub.ack を呼ぶので ack は保証される。
    if not config.workdir.is_dir():
        logger.error(
            "workdir does not exist or is not a directory: %s — "
            "skipping message %s to prevent crash-ack loop (issue #51)",
            config.workdir,
            msg.id,
        )
        with anyio.move_on_after(10):
            try:
                # issue #183: _journalled_send で crash-safe に送信
                await _journalled_send(
                    hub,
                    journal,
                    to=msg.sender,
                    message=(
                        f"(自動応答) bridge の workdir が存在しません: "
                        f"{config.workdir}"
                    ),
                    caused_by=msg.id,
                )
            except Exception:
                logger.exception("workdir-missing fallback DM to %s failed", msg.sender)
        return

    logger.info("← message %s from %s: %s", msg.id, msg.sender, msg.body[:120])

    prompt = format_peer_message_prompt(msg)
    await claude.query(prompt, session_id=msg.sender)

    result_msg: ResultMessage | None = None
    # issue #92: send_message ツール結果から送信 msg_id を捕捉する。
    # AssistantMessage の ToolUseBlock で send_message 呼び出しを検知し、
    # 対応する UserMessage の ToolResultBlock から {"id": "<uuid>"} を取得する。
    sent_msg_id: str | None = None
    _send_msg_tool_ids: set[str] = set()

    # issue #109: 全 tool_use を追跡して OTel child span に記録する。
    # _pending_tool_uses: tool_use_id → (name, input, start_time_ns)
    # tool_uses: 完了した ToolUseRecord のリスト (emit_span に渡す)
    _pending_tool_uses: dict[str, tuple[str, dict[str, Any], int]] = {}  # issue #113
    tool_uses: list[ToolUseRecord] = []

    async for sdk_msg in claude.receive_response():
        formatted = _format_message(sdk_msg)
        logger.info(formatted)
        # issue #46: ASSISTANT: ログが出るタイミング (= AssistantMessage 受信)
        # でアクティビティを記録する。stdout スニフと同等の外部観測ベース。
        if isinstance(sdk_msg, AssistantMessage):
            tracker.mark_active()
            for block in sdk_msg.content:
                if isinstance(block, ToolUseBlock):
                    # issue #109: 全 tool_use を start_time_ns と共に記録する。
                    _pending_tool_uses[block.id] = (
                        block.name,
                        block.input if isinstance(block.input, dict) else {},
                        time.time_ns(),
                    )
                    # issue #92: send_message tool_use_id を追跡する。
                    # 完全一致で "mcp__agent-hub__send_message" のみを対象にする
                    # (部分一致だと将来追加されるツールで誤検知するリスクがある — reviewer Minor)。
                    if block.name == "mcp__agent-hub__send_message":
                        _send_msg_tool_ids.add(block.id)
        elif isinstance(sdk_msg, UserMessage):
            end_ns = time.time_ns()
            blocks = (
                sdk_msg.content
                if isinstance(sdk_msg.content, list)
                else [sdk_msg.content]
            )
            for block in blocks:
                if isinstance(block, ToolResultBlock):
                    tool_id = block.tool_use_id
                    # issue #109: 対応する pending tool_use を完了させて記録する。
                    if tool_id in _pending_tool_uses:
                        t_name, t_input, start_ns = _pending_tool_uses.pop(tool_id)
                        tool_uses.append(
                            ToolUseRecord(
                                name=t_name,
                                input=t_input,
                                start_time_ns=start_ns,
                                end_time_ns=end_ns,
                                is_error=bool(block.is_error),
                            )
                        )
                    # issue #92: 最初の成功した send_message 結果から送信 msg_id を取得。
                    if (
                        sent_msg_id is None
                        and tool_id in _send_msg_tool_ids
                        and not block.is_error
                        and block.content
                    ):
                        try:
                            content_str = (
                                block.content
                                if isinstance(block.content, str)
                                else block.content[0].get("text", "")  # type: ignore[union-attr]
                            )
                            sent_msg_id = json.loads(content_str).get("id")
                        except (
                            json.JSONDecodeError,
                            AttributeError,
                            IndexError,
                            KeyError,
                        ):
                            pass
        if isinstance(sdk_msg, ResultMessage):
            result_msg = sdk_msg
            break

    # issue #90 + #92 + #109: OTLP span emit (opt-in — AGENT_HUB_TELEMETRY_URL で有効化)。
    # ResultMessage が得られた場合のみ span を emit する。
    # emit_span は内部で例外を読み捨てるため bridge を停止させない。
    # issue #109: tool_uses を渡して tool_use ごとの child span を emit する。
    if result_msg is not None:
        emit_span(
            caused_by_id=msg.id,
            sent_msg_id=sent_msg_id,
            model=config.model,
            result=result_msg,
            tool_uses=tool_uses if tool_uses else None,
        )
