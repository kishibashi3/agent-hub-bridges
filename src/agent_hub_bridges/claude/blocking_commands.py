"""Blocking command detection for bridge-claude (issue #101).

bridge は DM を受信してから Claude に prompt を流す「メッセージ駆動型」の
プロセスである。Claude Code が `Bash` ツールで ``gh run watch`` や
``sleep 3600`` のような「無限待ち・長時間 sleep」コマンドを実行すると、
bridge の ``receive_response()`` ループが完了しなくなり、後続の DM を
受信できなくなる (受信ループをブロックする)。

このモジュールは:
  1. ``check_blocking_command(command)`` — コマンド文字列を検査して
     ブロッキングパターンに一致すれば検出パターン名を返す純粋関数。
  2. ``bash_pre_tool_use_hook`` — ``ClaudeAgentOptions.hooks["PreToolUse"]``
     に登録する非同期フック。ブロッキング検出時に ``permissionDecision: "deny"``
     を返して実行を拒否し、@scheduler の代替手段を案内する。

ブロッキングパターン:
  - ``gh run watch``              : GitHub Actions run の監視 (無限待ち)
  - ``sleep <N>=60以上``          : 長時間 sleep (60s 未満は許可)
  - ``sleep 1m / 1h / 1d``       : 時間単位付き sleep (m/h/d suffix)
  - ``sleep infinity / sleep inf``: 無限 sleep
  - ``tail -f / -F / --follow``   : ファイル follow モード (無限)
  - ``watch <command>``           : コマンドの繰り返し実行 (無限)

既知の検出限界 (Known limitations):
  - ``sudo watch df`` / ``nice watch df`` など、prefix コマンドを挟んだ
    ``watch`` は検出できない (shell operator パターンに一致しないため)。
  - ``sleep 1m30s`` のような複合 duration (GNU coreutils 非対応の BSD では
    使えないが一部環境で有効) は未対応。
  - 変数展開 (``sleep $WAIT``) は静的解析では検出不可。

NOTE: ``bypassPermissions`` モードでは ``can_use_tool`` コールバックは
invoked されないため、``hooks["PreToolUse"]`` を使う必要がある。
hooks は permission mode に関わらず全 tool call の前に実行される。
"""

from __future__ import annotations

import logging
import re
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from claude_agent_sdk import HookContext, HookInput, HookJSONOutput

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# ブロッキングパターン定義
# ---------------------------------------------------------------------------

# (パターン名, コンパイル済み正規表現) のリスト。
# 各エントリは _check_* 関数経由で評価される。順序は重要でない。
_BLOCKING_PATTERNS: list[tuple[str, re.Pattern[str]]] = [
    (
        "gh run watch",
        re.compile(r"\bgh\s+run\s+watch\b"),
    ),
    (
        "tail -f / --follow",
        # -[a-zA-Z0-9]* で "-f", "-F", "-100f", "-nF" など combined flags も捕捉する。
        # [fF] で -f (follow-descriptor) と -F (follow-name) の両方に対応。
        re.compile(r"\btail\b.*(?:\s-[a-zA-Z0-9]*[fF]\b|\s--follow\b)"),
    ),
    (
        "watch <command>",
        # 行頭 or シェル演算子 (;, |, &, (, &&, ||) の直後の watch のみ対象。
        # watchman / watchdog 等の別コマンドの引数 "watch" は除外する。
        # Known limitation: sudo/nice/env 等の prefix コマンドを挟んだ watch は未検出。
        re.compile(r"(?:^|&&|\|\||[;&|(])\s*watch\s"),
    ),
]
# sleep は秒数 or 特殊値を抽出して判定するため _BLOCKING_PATTERNS とは別に管理する。

# sleep <N> — 整数・小数値 (秒単位)
_SLEEP_SECONDS_PATTERN = re.compile(r"\bsleep\s+(\d+(?:\.\d+)?)(?:\s|$|[;&|])")
_SLEEP_MIN_SECONDS: float = 60.0

# sleep <N>m / <N>h / <N>d — 時間単位付き (GNU coreutils / macOS sleep)
# m=分, h=時間, d=日 — いずれも 1 単位以上で 60s を超えるためすべてブロッキングとみなす。
_SLEEP_UNIT_PATTERN = re.compile(r"\bsleep\s+\d+(?:\.\d+)?[mhd]\b")

# sleep infinity / sleep inf — 無限 sleep
_SLEEP_INFINITY_PATTERN = re.compile(r"\bsleep\s+(?:infinity|inf)\b", re.IGNORECASE)

# @scheduler ヒントメッセージ (全パターン共通)
_SCHEDULER_HINT = (
    "Instead, use @scheduler:\n"
    "  @scheduler /run_in 10m @<your-handle> gh run view <run_id> --repo <owner/repo>"
)


# ---------------------------------------------------------------------------
# パブリック API
# ---------------------------------------------------------------------------


def check_blocking_command(command: str) -> str | None:
    """コマンド文字列を検査してブロッキングパターン名を返す。

    どのパターンにも一致しなければ ``None`` を返す。

    Args:
        command: Bash ツールの ``command`` フィールド値。

    Returns:
        ブロッキングパターン名 (例: ``"gh run watch"``), または ``None``。

    Examples:
        >>> check_blocking_command("gh run watch 1234")
        'gh run watch'
        >>> check_blocking_command("sleep 3600")
        'sleep <N>=60s+'
        >>> check_blocking_command("sleep 1m")
        'sleep <N>=60s+'
        >>> check_blocking_command("sleep infinity")
        'sleep <N>=60s+'
        >>> check_blocking_command("sleep 30")  # 30s は許可
        >>> check_blocking_command("tail -f /var/log/syslog")
        'tail -f / --follow'
        >>> check_blocking_command("tail -F /var/log/syslog")
        'tail -f / --follow'
        >>> check_blocking_command("watch df -h")
        'watch <command>'
        >>> check_blocking_command("echo hello")
    """
    # sleep: 秒数の閾値チェック
    for m in _SLEEP_SECONDS_PATTERN.finditer(command):
        try:
            seconds = float(m.group(1))
        except ValueError:
            continue
        if seconds >= _SLEEP_MIN_SECONDS:
            return "sleep <N>=60s+"

    # sleep: 時間単位付き (m/h/d) — いずれも 60s 超なので無条件ブロック
    if _SLEEP_UNIT_PATTERN.search(command):
        return "sleep <N>=60s+"

    # sleep: infinity / inf — 無限 sleep
    if _SLEEP_INFINITY_PATTERN.search(command):
        return "sleep <N>=60s+"

    # その他のパターンは正規表現一致のみ
    for name, pattern in _BLOCKING_PATTERNS:
        if pattern.search(command):
            return name

    return None


def build_blocking_error_message(pattern_name: str) -> str:
    """ブロッキングコマンド検出時の deny メッセージを組み立てる。

    Args:
        pattern_name: ``check_blocking_command`` が返したパターン名。

    Returns:
        人間可読なエラーメッセージ文字列。
    """
    return (
        f"Error: blocking command detected (`{pattern_name}`).\n"
        "Blocking waits prevent this bridge from receiving messages.\n"
        "\n"
        f"{_SCHEDULER_HINT}"
    )


async def bash_pre_tool_use_hook(
    hook_input: HookInput,
    tool_use_id: str | None,
    context: HookContext,
) -> HookJSONOutput:
    """PreToolUse フック: Bash ツールのブロッキングコマンドを検出して拒否する.

    ``ClaudeAgentOptions.hooks["PreToolUse"]`` に
    ``HookMatcher(matcher="Bash", hooks=[bash_pre_tool_use_hook])`` で登録する。

    ブロッキングコマンドが検出された場合:
      - ``permissionDecision: "deny"`` を返して Bash ツールの実行を中止させる。
      - ``permissionDecisionReason`` に @scheduler の代替案内を含むエラーメッセージを設定する。
      - WARNING ログを出力する。

    ブロッキングコマンドでない場合:
      - 空 dict を返して通常実行を継続させる。

    Args:
        hook_input: SDK から渡される TypedDict。``PreToolUse`` イベント時は
            ``tool_name`` / ``tool_input`` / ``tool_use_id`` を含む。
        tool_use_id: ツール呼び出し識別子 (ログ用)。
        context: フックコンテキスト (現在は signal のみ、常に None)。

    Returns:
        空 dict (許可) または ``hookSpecificOutput`` 付き deny 応答。
    """
    # hook_input は TypedDict (dict プロトコル)。
    # HookMatcher(matcher="Bash") で登録しているため、ここに到達する時点で
    # tool_name == "Bash" のはず。tool_input["command"] にコマンド文字列がある。
    raw_input: dict = hook_input  # type: ignore[assignment]
    tool_input = raw_input.get("tool_input")
    if not isinstance(tool_input, dict):
        return {}  # tool_input 欠落 or 非 dict — pass-through
    # .get("command", "") ではなく .get("command") を使い、値が None の場合も
    # 安全に pass-through する (デフォルト値 "" は key が存在しない場合のみ発動するため、
    # 値が None だと None が返り finditer(None) で TypeError になる)。
    raw_command = tool_input.get("command")
    if not isinstance(raw_command, str):
        return {}  # command キー欠落 or 非 str (None 等) — pass-through
    command: str = raw_command

    detected = check_blocking_command(command)
    if detected is None:
        return {}  # 許可 — SDK はデフォルト動作を継続する

    msg = build_blocking_error_message(detected)
    logger.warning(
        "[blocking-cmd] blocking command detected (`%s`), denying Bash tool use "
        "(tool_use_id=%s, command_preview=%.120s)",
        detected,
        tool_use_id,
        command,
    )
    return {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "deny",
            "permissionDecisionReason": msg,
        }
    }
