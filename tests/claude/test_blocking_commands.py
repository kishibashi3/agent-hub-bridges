"""Tests for blocking command detection (issue #101).

カバーするケース:
  - check_blocking_command: 各ブロッキングパターンの検出
  - check_blocking_command: 許可コマンドは None を返す
  - check_blocking_command: sleep 秒数閾値 (60s 未満は許可、60s 以上はブロック)
  - build_blocking_error_message: エラーメッセージに pattern_name と @scheduler ヒントを含む
  - bash_pre_tool_use_hook: 非ブロッキングコマンドは空 dict を返す
  - bash_pre_tool_use_hook: ブロッキングコマンドは deny + permissionDecisionReason を返す
  - bash_pre_tool_use_hook: tool_input が欠落しても安全に空 dict を返す
"""

from __future__ import annotations

import pytest

from agent_hub_bridges.claude.blocking_commands import (
    bash_pre_tool_use_hook,
    build_blocking_error_message,
    check_blocking_command,
)


# ---------------------------------------------------------------------------
# check_blocking_command — パターン検出テスト
# ---------------------------------------------------------------------------


class TestCheckBlockingCommand:
    """check_blocking_command() の検出ロジック。"""

    # --- gh run watch ---

    def test_gh_run_watch_basic(self) -> None:
        assert check_blocking_command("gh run watch") == "gh run watch"

    def test_gh_run_watch_with_run_id(self) -> None:
        assert check_blocking_command("gh run watch 12345678") == "gh run watch"

    def test_gh_run_watch_with_flags(self) -> None:
        assert (
            check_blocking_command("gh run watch --repo owner/repo 12345678")
            == "gh run watch"
        )

    def test_gh_run_list_not_blocked(self) -> None:
        """gh run list はブロッキングではない。"""
        assert check_blocking_command("gh run list") is None

    def test_gh_run_view_not_blocked(self) -> None:
        """gh run view はブロッキングではない。"""
        assert check_blocking_command("gh run view 12345678") is None

    # --- sleep ---

    def test_sleep_60_blocked(self) -> None:
        assert check_blocking_command("sleep 60") == "sleep <N>=60s+"

    def test_sleep_3600_blocked(self) -> None:
        assert check_blocking_command("sleep 3600") == "sleep <N>=60s+"

    def test_sleep_300_blocked(self) -> None:
        assert check_blocking_command("sleep 300") == "sleep <N>=60s+"

    def test_sleep_59_allowed(self) -> None:
        """59s は閾値未満なので許可。"""
        assert check_blocking_command("sleep 59") is None

    def test_sleep_30_allowed(self) -> None:
        assert check_blocking_command("sleep 30") is None

    def test_sleep_0_allowed(self) -> None:
        assert check_blocking_command("sleep 0") is None

    def test_sleep_float_below_threshold_allowed(self) -> None:
        assert check_blocking_command("sleep 59.9") is None

    def test_sleep_float_at_threshold_blocked(self) -> None:
        assert check_blocking_command("sleep 60.0") == "sleep <N>=60s+"

    def test_sleep_in_pipeline_blocked(self) -> None:
        """パイプライン内の sleep も検出する。"""
        assert check_blocking_command("echo start && sleep 300 && echo done") == "sleep <N>=60s+"

    # --- tail -f / --follow ---

    def test_tail_f_basic(self) -> None:
        assert check_blocking_command("tail -f /var/log/syslog") == "tail -f / --follow"

    def test_tail_f_with_lines(self) -> None:
        assert check_blocking_command("tail -n 100 -f /var/log/app.log") == "tail -f / --follow"

    def test_tail_combined_flags(self) -> None:
        """tail -100f など combined flags も検出する。"""
        assert check_blocking_command("tail -100f /var/log/app.log") == "tail -f / --follow"

    def test_tail_follow_long_form(self) -> None:
        assert check_blocking_command("tail --follow /var/log/app.log") == "tail -f / --follow"

    def test_tail_without_f_allowed(self) -> None:
        """tail -n 100 (follow なし) は許可。"""
        assert check_blocking_command("tail -n 100 /var/log/app.log") is None

    def test_tail_n_only_allowed(self) -> None:
        assert check_blocking_command("tail -100 /var/log/app.log") is None

    # --- watch ---

    def test_watch_basic(self) -> None:
        assert check_blocking_command("watch ls") == "watch <command>"

    def test_watch_with_interval(self) -> None:
        assert check_blocking_command("watch -n 5 df -h") == "watch <command>"

    def test_watch_with_color(self) -> None:
        assert check_blocking_command("watch --color df") == "watch <command>"

    def test_watchman_not_blocked(self) -> None:
        """watchman はブロッキング対象外 (word boundary で区別)。"""
        assert check_blocking_command("watchman watch /path/to/dir") is None

    def test_watchdog_not_blocked(self) -> None:
        assert check_blocking_command("watchdog start") is None

    # --- 許可コマンド ---

    def test_echo_allowed(self) -> None:
        assert check_blocking_command("echo hello world") is None

    def test_ls_allowed(self) -> None:
        assert check_blocking_command("ls -la") is None

    def test_git_status_allowed(self) -> None:
        assert check_blocking_command("git status") is None

    def test_gh_run_view_allowed(self) -> None:
        assert check_blocking_command("gh run view 12345678 --repo owner/repo") is None

    def test_empty_command_allowed(self) -> None:
        assert check_blocking_command("") is None


# ---------------------------------------------------------------------------
# build_blocking_error_message
# ---------------------------------------------------------------------------


class TestBuildBlockingErrorMessage:
    def test_contains_pattern_name(self) -> None:
        msg = build_blocking_error_message("gh run watch")
        assert "gh run watch" in msg

    def test_contains_scheduler_hint(self) -> None:
        msg = build_blocking_error_message("tail -f / --follow")
        assert "@scheduler" in msg
        assert "/run_in" in msg

    def test_contains_blocking_warning(self) -> None:
        msg = build_blocking_error_message("watch <command>")
        assert "blocking" in msg.lower()

    def test_error_prefix(self) -> None:
        msg = build_blocking_error_message("sleep <N>=60s+")
        assert msg.startswith("Error:")


# ---------------------------------------------------------------------------
# bash_pre_tool_use_hook — 非同期テスト
# ---------------------------------------------------------------------------


class TestBashPreToolUseHook:
    """bash_pre_tool_use_hook() の非同期挙動。"""

    _CONTEXT: dict = {"signal": None}

    @pytest.mark.asyncio
    async def test_non_blocking_command_returns_empty_dict(self) -> None:
        """ブロッキングでないコマンドは空 dict を返す (allow)。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {"command": "echo hello"},
            "tool_use_id": "tu_001",
        }
        result = await bash_pre_tool_use_hook(hook_input, "tu_001", self._CONTEXT)  # type: ignore[arg-type]
        assert result == {}

    @pytest.mark.asyncio
    async def test_gh_run_watch_denied(self) -> None:
        """gh run watch は deny を返す。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {"command": "gh run watch 12345678"},
            "tool_use_id": "tu_002",
        }
        result = await bash_pre_tool_use_hook(hook_input, "tu_002", self._CONTEXT)  # type: ignore[arg-type]
        specific = result.get("hookSpecificOutput", {})
        assert specific.get("hookEventName") == "PreToolUse"
        assert specific.get("permissionDecision") == "deny"
        assert "gh run watch" in specific.get("permissionDecisionReason", "")
        assert "@scheduler" in specific.get("permissionDecisionReason", "")

    @pytest.mark.asyncio
    async def test_sleep_long_denied(self) -> None:
        """sleep 300 は deny を返す。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {"command": "sleep 300"},
            "tool_use_id": "tu_003",
        }
        result = await bash_pre_tool_use_hook(hook_input, "tu_003", self._CONTEXT)  # type: ignore[arg-type]
        specific = result.get("hookSpecificOutput", {})
        assert specific.get("permissionDecision") == "deny"

    @pytest.mark.asyncio
    async def test_sleep_short_allowed(self) -> None:
        """sleep 30 は空 dict (allow)。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {"command": "sleep 30"},
            "tool_use_id": "tu_004",
        }
        result = await bash_pre_tool_use_hook(hook_input, "tu_004", self._CONTEXT)  # type: ignore[arg-type]
        assert result == {}

    @pytest.mark.asyncio
    async def test_tail_f_denied(self) -> None:
        """tail -f は deny を返す。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {"command": "tail -f /var/log/app.log"},
            "tool_use_id": "tu_005",
        }
        result = await bash_pre_tool_use_hook(hook_input, "tu_005", self._CONTEXT)  # type: ignore[arg-type]
        assert result.get("hookSpecificOutput", {}).get("permissionDecision") == "deny"

    @pytest.mark.asyncio
    async def test_watch_command_denied(self) -> None:
        """watch df は deny を返す。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {"command": "watch df -h"},
            "tool_use_id": "tu_006",
        }
        result = await bash_pre_tool_use_hook(hook_input, "tu_006", self._CONTEXT)  # type: ignore[arg-type]
        assert result.get("hookSpecificOutput", {}).get("permissionDecision") == "deny"

    @pytest.mark.asyncio
    async def test_missing_tool_input_safe(self) -> None:
        """tool_input が欠落しても例外を出さず空 dict を返す。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            # tool_input なし
            "tool_use_id": "tu_007",
        }
        result = await bash_pre_tool_use_hook(hook_input, "tu_007", self._CONTEXT)  # type: ignore[arg-type]
        assert result == {}

    @pytest.mark.asyncio
    async def test_missing_command_key_safe(self) -> None:
        """tool_input に command キーがなくても安全に空 dict を返す。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {},
            "tool_use_id": "tu_008",
        }
        result = await bash_pre_tool_use_hook(hook_input, "tu_008", self._CONTEXT)  # type: ignore[arg-type]
        assert result == {}

    @pytest.mark.asyncio
    async def test_tool_use_id_none_safe(self) -> None:
        """tool_use_id が None でも安全に動作する (ログ用フィールド)。"""
        hook_input = {
            "hook_event_name": "PreToolUse",
            "tool_name": "Bash",
            "tool_input": {"command": "echo hello"},
            "tool_use_id": "tu_009",
        }
        result = await bash_pre_tool_use_hook(hook_input, None, self._CONTEXT)  # type: ignore[arg-type]
        assert result == {}
