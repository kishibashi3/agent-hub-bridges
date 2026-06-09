"""Gemini CLI engine wrapper.

bridge-gemini の "LLM engine" 部分。M2 で `google-genai` SDK 直叩きから
`gemini` CLI (Google 公式の Gemini CLI) の non-interactive モード呼び出しへ
切り替えた。これにより bridge は CLI が提供する tool 群 (file 読み書き /
shell / agent-hub MCP) を素のまま受け取れる。

bridge-claude が Claude Agent SDK + MCP 設定一時 file で同じ事をしているのに
対し、こちらは `gemini` CLI 配下の `~/.gemini/settings.json` を per-bridge に
切り替えるため、worker 専用の "isolated HOME" に MCP 設定を書き込んで
`HOME=<temp>` 環境で `gemini -p` を起動する。

設計上の差分:
  - 応答 text を Python 側で組み立てて hub に送り返す必要は無くなった。
    返信は `gemini` 自身が `mcp__agent-hub__send_message` tool を呼ぶ。
    worker は subprocess を spawn / 待機するだけ。
  - 会話履歴の保持: 現状は `gemini` を **session_id 引数なしで 1 ターン
    処理** で 呼んでいる (= peer ごとの 会話 context は 持たない)。
    将来的に `gemini --session-id <uuid>` で chat 永続化に拡張する余地
    はあるが、 monorepo M3 時点では **未実装**。 旧 repo の M3 milestone
    (= chat session 永続化) は future scope として 残置。
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import shutil
import signal
import tempfile
from dataclasses import dataclass
from pathlib import Path

from agent_hub_bridges.gemini.config import Config

logger = logging.getLogger(__name__)

# `gemini` CLI が config を探す場所。HOME を差し替えれば自然と
# `<HOME>/.gemini/settings.json` を読みに行く。
GEMINI_CONFIG_SUBDIR = ".gemini"
GEMINI_SETTINGS_FILENAME = "settings.json"

# 1 message あたりの最大実行時間 (秒)。tool use で長く回ることがあるが、
# 暴走を止めるための上限。env GEMINI_CLI_TIMEOUT_S で override 可能。
DEFAULT_TIMEOUT_S = 600.0

# rate limit (429) で gemini CLI が落ちた時の retry 設定。
#  - DEFAULT_MAX_RETRIES: 初回失敗後に追加で何回 retry するか (0 で retry 無効)
#  - DEFAULT_BACKOFF_BASE_S: exponential backoff の base。base * 2**(attempt-1)
#  - DEFAULT_BACKOFF_CAP_S: backoff が無限に伸びないための上限
# いずれも env (GEMINI_MAX_RETRIES / GEMINI_BACKOFF_BASE_S / GEMINI_BACKOFF_CAP_S)
# で override 可能。stderr に `retryDelay: Xs` のような明示秒数があれば
# そちらを優先する。
DEFAULT_MAX_RETRIES = 3
DEFAULT_BACKOFF_BASE_S = 2.0
DEFAULT_BACKOFF_CAP_S = 60.0

# stderr に含まれていたら rate limit と判定するマーカー (case-insensitive 比較)。
# gemini CLI 自身のメッセージと、その下の Google API SDK が透過してくる
# REST error 文字列の両方を拾えるように複数登録してある。
_RATE_LIMIT_MARKERS: tuple[str, ...] = (
    "quota exceeded",
    "rate limit",
    "resource_exhausted",
    "429",
    "too many requests",
)

# stderr から「次まで何秒待て」を抽出するための正規表現。
# Google API の retryDelay フィールド (`"retryDelay": "13s"`)、CLI の
# 人間向け表示 (`Please retry in 12.5s` / `retry after 30 seconds`) などを
# 一通り拾う。match group 1 が秒数 (str)。
_RETRY_DELAY_PATTERNS: tuple[re.Pattern[str], ...] = (
    re.compile(r'"?retry[_ ]?delay"?\s*[:=]\s*"?(\d+(?:\.\d+)?)\s*s?"?', re.IGNORECASE),
    re.compile(r"retry\s+(?:in|after)\s+(\d+(?:\.\d+)?)\s*s(?:ec(?:onds?)?)?\b", re.IGNORECASE),
)


@dataclass(frozen=True)
class EngineResult:
    """`gemini` CLI 1 回分の実行結果 (ログ用).

    `attempts` は retry も含めた実行回数。初回成功なら 1、rate limit で
    1 回 retry して成功したなら 2。最大 retry に達して諦めた場合も
    最後の試行回数 (= max_retries + 1) が入る。
    """

    returncode: int
    stdout: str
    stderr: str
    duration_s: float
    attempts: int = 1


def is_rate_limit_error(stderr: str) -> bool:
    """stderr が rate limit (429 / Quota exceeded) を示すかを判定する.

    issue #22 で worker.py からも参照するため public 化 (underscore を除去)。
    """
    if not stderr:
        return False
    lower = stderr.lower()
    return any(marker in lower for marker in _RATE_LIMIT_MARKERS)


def _parse_retry_delay_s(stderr: str) -> float | None:
    """stderr に明示された retry 待機秒数 (e.g. `retryDelay: 13s`) を取り出す.

    見つからなければ None。0 以下の値は noise 扱いで無視する。
    """
    if not stderr:
        return None
    for pattern in _RETRY_DELAY_PATTERNS:
        match = pattern.search(stderr)
        if not match:
            continue
        try:
            value = float(match.group(1))
        except (ValueError, IndexError):
            continue
        if value > 0:
            return value
    return None


def _compute_backoff_s(
    stderr: str,
    attempt: int,
    *,
    base_s: float,
    cap_s: float,
) -> float:
    """次の retry までの待機秒数を計算する.

    優先順位:
      1. stderr に明示された `retryDelay` 系の秒数 (cap_s で頭打ち)
      2. exponential backoff: base_s * 2**(attempt-1) (cap_s で頭打ち)

    `attempt` は 1-indexed (= 何回目の失敗か)。1 回目失敗なら base、
    2 回目なら base*2、3 回目なら base*4 ... となる。
    """
    parsed = _parse_retry_delay_s(stderr)
    if parsed is not None:
        return min(parsed, cap_s)
    return min(base_s * (2 ** max(attempt - 1, 0)), cap_s)


class GeminiCLIEngine:
    """`gemini` CLI の non-interactive 呼び出しを管理する engine.

    state:
      - `_home_dir`: per-bridge の isolated HOME (Path)。`__init__` で作成し、
        `close()` で削除する。中の `.gemini/settings.json` には worker の
        identity (X-User-Id) を埋めた agent-hub MCP 設定が入っている。
      - `_cli_path`: 実行する gemini binary path。
      - `_timeout_s`: 1 ターンあたりの timeout。
    """

    def __init__(
        self,
        config: Config,
        home_dir: Path,
        cli_path: str,
        timeout_s: float,
        max_retries: int = DEFAULT_MAX_RETRIES,
        backoff_base_s: float = DEFAULT_BACKOFF_BASE_S,
        backoff_cap_s: float = DEFAULT_BACKOFF_CAP_S,
    ) -> None:
        self._config = config
        self._home_dir = home_dir
        self._cli_path = cli_path
        self._timeout_s = timeout_s
        self._max_retries = max(max_retries, 0)
        self._backoff_base_s = max(backoff_base_s, 0.0)
        self._backoff_cap_s = max(backoff_cap_s, self._backoff_base_s)

    @classmethod
    def create(cls, config: Config) -> GeminiCLIEngine:
        """Engine を初期化する.

        - gemini CLI の path を解決
        - per-bridge HOME (mkdtemp) を作成
        - user の `~/.gemini/settings.json` から `mcpServers.agent-hub` を
          コピーし、X-User-Id / X-Tenant-Id を bridge の identity で上書き
        - HOME 内に `.gemini/settings.json` を書く
        - retry / backoff のチューニングを env から読む
          (GEMINI_MAX_RETRIES / GEMINI_BACKOFF_BASE_S / GEMINI_BACKOFF_CAP_S)
        """
        cli_path = shutil.which(config.gemini_cli_path) or config.gemini_cli_path
        if not cli_path or not Path(cli_path).exists():
            raise FileNotFoundError(
                f"gemini CLI not found at '{config.gemini_cli_path}'. "
                f"Install with `npm i -g @google/gemini-cli` and ensure it's on PATH."
            )

        timeout_s = float(os.environ.get("GEMINI_CLI_TIMEOUT_S", DEFAULT_TIMEOUT_S))
        max_retries = int(os.environ.get("GEMINI_MAX_RETRIES", DEFAULT_MAX_RETRIES))
        backoff_base_s = float(
            os.environ.get("GEMINI_BACKOFF_BASE_S", DEFAULT_BACKOFF_BASE_S)
        )
        backoff_cap_s = float(
            os.environ.get("GEMINI_BACKOFF_CAP_S", DEFAULT_BACKOFF_CAP_S)
        )

        home_dir = Path(tempfile.mkdtemp(prefix=f"bridge-gemini-{config.user}-"))
        os.chmod(home_dir, 0o700)
        try:
            _write_isolated_settings(home_dir, config)
        except Exception:
            shutil.rmtree(home_dir, ignore_errors=True)
            raise

        logger.info(
            "GeminiCLIEngine ready: cli=%s home=%s timeout=%.0fs "
            "max_retries=%d backoff_base=%.1fs backoff_cap=%.1fs",
            cli_path,
            home_dir,
            timeout_s,
            max_retries,
            backoff_base_s,
            backoff_cap_s,
        )
        return cls(
            config=config,
            home_dir=home_dir,
            cli_path=cli_path,
            timeout_s=timeout_s,
            max_retries=max_retries,
            backoff_base_s=backoff_base_s,
            backoff_cap_s=backoff_cap_s,
        )

    def close(self) -> None:
        """isolated HOME を片付ける (worker 終了時に呼ぶ)。"""
        shutil.rmtree(self._home_dir, ignore_errors=True)

    async def run(self, *, peer: str, prompt: str) -> EngineResult:
        """`gemini -p` を起動して 1 ターン処理する (rate-limit 時は retry あり).

        gemini は内部で agent-hub の MCP tool を呼んで返信するので、
        この関数の戻り値は「実行ログ」であって message 本文ではない。
        呼び出し側は returncode / stdout / stderr をログに残すのが主目的。

        retry 仕様:
          - subprocess の stderr に rate limit マーカーが含まれていて、かつ
            残り retry 回数があれば backoff 待機後に再実行する。
          - timeout (`RuntimeError`) や rate-limit 以外の失敗は即時 return。
          - 最終的に成功 or 諦めた結果の `EngineResult` を 1 つ返す。
            呼び出し側 (worker) はその後で `mark_as_read` する。

        timeout 超過時は subprocess を kill して RuntimeError を投げる
        (retry はしない)。
        """
        max_attempts = self._max_retries + 1
        last_result: EngineResult | None = None
        for attempt in range(1, max_attempts + 1):
            result = await self._invoke_once(peer=peer, prompt=prompt, attempt=attempt)
            last_result = result

            if result.returncode == 0:
                return result
            if not is_rate_limit_error(result.stderr):
                # rate-limit 以外の失敗は retry しても無駄なので即返す
                return result
            if attempt >= max_attempts:
                logger.warning(
                    "gemini CLI rate-limited and max_retries (%d) exhausted "
                    "for peer=%s; giving up",
                    self._max_retries,
                    peer,
                )
                return result

            wait_s = _compute_backoff_s(
                result.stderr,
                attempt,
                base_s=self._backoff_base_s,
                cap_s=self._backoff_cap_s,
            )
            # issue #19: grep 可能な [RATE_LIMIT_RETRY] マーカー付き WARNING。
            # `grep RATE_LIMIT_RETRY` で retry 発生行だけを抽出できる。
            # attempt / max_attempts / peer / wait_s を structured に出すことで
            # log aggregator での集計 (retry frequency / backoff distribution) に使える。
            logger.warning(
                "[RATE_LIMIT_RETRY] attempt=%d/%d peer=%s backoff=%.1fs — "
                "gemini CLI rate-limited; sleeping before retry",
                attempt,
                max_attempts,
                peer,
                wait_s,
            )
            await asyncio.sleep(wait_s)

        # for-loop は必ず return で抜けるので来ない。type checker 用 fallback。
        assert last_result is not None
        return last_result

    async def _invoke_once(
        self, *, peer: str, prompt: str, attempt: int = 1
    ) -> EngineResult:
        """`gemini -p` を 1 回起動して結果を返す (retry なし)."""
        cmd = self._build_cmd()
        env = self._build_env()
        logger.info(
            "→ spawning gemini for peer=%s attempt=%d (cmd=%s, cwd=%s)",
            peer,
            attempt,
            " ".join(cmd),
            self._config.workdir,
        )

        loop = asyncio.get_running_loop()
        start = loop.time()

        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd=str(self._config.workdir),
            env=env,
            # issue #17: subprocess tree leak on timeout.
            # start_new_session=True で gemini とその子プロセス (node 等) を
            # 独立した process group に入れる。timeout 時に SIGKILL を
            # os.killpg でグループ全体に送ることで孤児プロセスの残留を防ぐ。
            start_new_session=True,
        )

        try:
            stdout_bytes, stderr_bytes = await asyncio.wait_for(
                proc.communicate(input=prompt.encode("utf-8")),
                timeout=self._timeout_s,
            )
        except TimeoutError as err:
            logger.warning(
                "gemini CLI timeout (%.0fs) for peer=%s; killing pgid=%d",
                self._timeout_s,
                peer,
                proc.pid,
            )
            # issue #17: proc.kill() は leader プロセスだけを kill し、
            # gemini が spawn した node 等の子プロセスが孤児になる。
            # start_new_session=True により proc.pid == pgid なので、
            # SIGKILL をプロセスグループ全体に送ってツリーごと終了させる。
            try:
                os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
            except ProcessLookupError:
                # プロセスが既に終了していた場合は無視
                pass
            await proc.wait()
            raise RuntimeError(
                f"gemini CLI exceeded timeout ({self._timeout_s:.0f}s) for peer={peer}"
            ) from err

        duration = loop.time() - start
        result = EngineResult(
            returncode=proc.returncode or 0,
            stdout=stdout_bytes.decode("utf-8", errors="replace"),
            stderr=stderr_bytes.decode("utf-8", errors="replace"),
            duration_s=duration,
            attempts=attempt,
        )

        if result.returncode != 0:
            logger.warning(
                "gemini CLI exit=%d (peer=%s, attempt=%d, %.1fs); stderr tail: %s",
                result.returncode,
                peer,
                attempt,
                duration,
                result.stderr[-500:].strip(),
            )
        else:
            logger.info(
                "gemini CLI done (peer=%s, attempt=%d, %.1fs, %d stdout bytes)",
                peer,
                attempt,
                duration,
                len(result.stdout),
            )
        return result

    def _build_cmd(self) -> list[str]:
        """gemini CLI コマンドライン.

        - `-p ""` で stdin 経由 prompt の non-interactive モードを明示
          (stdin に prompt を流す。引数渡しだと shell quoting で詰む)
        - `--yolo` で全 tool を auto-approve (bridge は人手介在なしの daemon)
        - `--skip-trust` で workspace の信用確認 prompt を skip
        - `-o text` で plain stdout (json は M2 の段階では使わない)
        - `--allowed-mcp-server-names agent-hub` で agent-hub だけを許可
          (isolated HOME には agent-hub しか書いてないので冗長だが safety net)
        """
        cmd = [
            self._cli_path,
            "-p",
            "",  # prompt は stdin から
            "--yolo",
            "--skip-trust",
            "--allowed-mcp-server-names",
            "agent-hub",
            "-o",
            "text",
        ]
        if self._config.gemini_model:
            cmd.extend(["-m", self._config.gemini_model])
        return cmd

    def _build_env(self) -> dict[str, str]:
        """subprocess に渡す env を組み立てる.

        - 既存 env を継承 (PATH, GEMINI_API_KEY など)
        - HOME を isolated dir で上書き → gemini は per-bridge の settings.json
          を読む
        - GITHUB_PAT は settings.json の `${GITHUB_PAT}` interpolation で
          参照されるので、確実に export しておく
        - GH_TOKEN: GITHUB_APP_* が設定されている場合は IAT を注入 (issue #73)。
        """
        from agent_hub_bridges._common.github_iat import IATManager

        env = os.environ.copy()
        env["HOME"] = str(self._home_dir)
        env["GITHUB_PAT"] = self._config.github_pat
        env["GEMINI_API_KEY"] = self._config.gemini_api_key

        # GitHub App IAT モード (issue #73)
        mgr = IATManager.from_env()
        if mgr is not None:
            try:
                env["GH_TOKEN"] = mgr.get_token()
            except Exception:
                logger.warning(
                    "github_iat: IAT fetch failed, falling back to default gh auth",
                    exc_info=True,
                )
        # gemini CLI の interactive/telemetry 検出を抑える hint。
        # 両方とも setdefault なので env で明示指定があれば尊重する。
        #
        #   CI=1: 未設定だと gemini CLI が "Update available: ..." 等の
        #         interactive な update 案内バナーを stderr に出力する。
        #         headless daemon では邪魔なため抑制 (= CLI が CI 環境と
        #         判定すると interactive プロンプト系を全てスキップする)。
        #
        #   TERM=dumb: 未設定 (または xterm-256color 等) だと gemini CLI が
        #              ANSI エスケープシーケンス (色 / カーソル制御: \x1b[...m 等)
        #              を stdout に混入させる。agent-hub 経由の返信本文に
        #              制御文字が漏れるため、TERM=dumb で plain text 出力に落とす。
        env.setdefault("CI", "1")
        env.setdefault("TERM", "dumb")
        return env


def _write_isolated_settings(home_dir: Path, config: Config) -> None:
    """isolated HOME 配下に `.gemini/settings.json` を書く.

    user の `~/.gemini/settings.json` から `mcpServers.agent-hub` ブロックを
    コピーし、X-User-Id / X-Tenant-Id を worker の handle で上書きする。
    user 設定が無ければ最小 config を新規生成する。
    """
    config_dir = home_dir / GEMINI_CONFIG_SUBDIR
    config_dir.mkdir(parents=True, exist_ok=True)
    settings_path = config_dir / GEMINI_SETTINGS_FILENAME

    user_settings = _read_user_settings()

    # 既存の agent-hub ブロックをベースに、identity headers だけ差し替える。
    base_block = (
        user_settings.get("mcpServers", {}).get("agent-hub")
        if isinstance(user_settings.get("mcpServers"), dict)
        else None
    )
    if isinstance(base_block, dict):
        agent_hub_block = dict(base_block)
        headers = dict(agent_hub_block.get("headers", {}))
    else:
        # user 設定なし: 最小値を bridge config から組み立てる
        agent_hub_block = {"httpUrl": config.agent_hub_url}
        headers = {"Authorization": "Bearer ${GITHUB_PAT}"}

    # bridge の identity で上書き (user 設定の X-User-Id は伝播させない)
    headers["X-User-Id"] = config.user
    if config.tenant:
        headers["X-Tenant-Id"] = config.tenant
    else:
        headers.pop("X-Tenant-Id", None)

    agent_hub_block["headers"] = headers

    # url 系の key を normalize (httpUrl と url が混在する CLI バージョン差を吸収)
    if "httpUrl" not in agent_hub_block and "url" in agent_hub_block:
        agent_hub_block["httpUrl"] = agent_hub_block.pop("url")
    elif "httpUrl" not in agent_hub_block:
        agent_hub_block["httpUrl"] = config.agent_hub_url

    payload = {"mcpServers": {"agent-hub": agent_hub_block}}
    settings_path.write_text(json.dumps(payload, indent=2), encoding="utf-8")
    os.chmod(settings_path, 0o600)
    logger.info("Wrote isolated settings: %s", settings_path)


def _read_user_settings() -> dict:
    """user-level `~/.gemini/settings.json` を読む (無ければ空 dict)。"""
    path = Path.home() / GEMINI_CONFIG_SUBDIR / GEMINI_SETTINGS_FILENAME
    if not path.exists():
        return {}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        logger.warning("Failed to read user settings %s: %s", path, exc)
        return {}
