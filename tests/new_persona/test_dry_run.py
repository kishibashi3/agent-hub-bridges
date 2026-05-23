"""Tests for run_dry_run() and its individual check helpers (issue #61 --dry-run).

カバーするケース:
  - _check_config_dryrun: basename 一致 (OK) / 不一致 (NG)
  - _check_from_dryrun: CLAUDE.md 存在 / 不在 / 名前バリデーション失敗
  - _check_workdir_dryrun: workdir 不在 (OK) / 既存 (NG)
  - _check_env_dryrun: 全揃い / TENANT 不在 / 必須不在
  - _check_repo_dryrun: repo 不在 (OK) / 重複 (NG) / gh なし (skip) / タイムアウト (skip)
  - _check_handle_dryrun: env 不在 (skip) / online (NG) / offline (OK)
      / API エラー (skip, type のみ)
  - run_dry_run: 全 OK / 一部 NG / 出力フォーマット / 終了コード
  - CLI: --dry-run フラグが run_dry_run を呼ぶことの確認
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from agent_hub_bridges.new_persona.runner import (
    _check_config_dryrun,
    _check_env_dryrun,
    _check_from_dryrun,
    _check_handle_dryrun,
    _check_repo_dryrun,
    _check_workdir_dryrun,
    _fetch_participants_from_hub,
    run_dry_run,
)

# ---------------------------------------------------------------------------
# _check_config_dryrun
# ---------------------------------------------------------------------------


class TestCheckConfigDryrun:
    def test_basename_matches_repos_ok(self, tmp_path: Path) -> None:
        workdir = tmp_path / "my-repos"
        result = _check_config_dryrun(workdir, "my-repos")

        assert result.ok
        assert result.label == "config"
        assert "my-repos" in result.detail

    def test_basename_mismatch_ng(self, tmp_path: Path) -> None:
        workdir = tmp_path / "different-name"
        result = _check_config_dryrun(workdir, "my-repos")

        assert not result.ok
        assert result.label == "config"
        assert "different-name" in result.detail
        assert "my-repos" in result.detail


# ---------------------------------------------------------------------------
# _check_from_dryrun
# ---------------------------------------------------------------------------


class TestCheckFromDryrun:
    def test_valid_claude_md_exists(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        roles = tmp_path / "roles"
        (roles / "agent-hub-coder").mkdir(parents=True)
        (roles / "agent-hub-coder" / "CLAUDE.md").write_text("# CLAUDE", encoding="utf-8")
        monkeypatch.setenv("AGENT_HUB_ROLES", str(roles))

        result = _check_from_dryrun("agent-hub-coder")

        assert result.ok
        assert "CLAUDE.md" in result.detail
        assert "(exists)" in result.detail

    def test_claude_md_not_found(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        roles = tmp_path / "roles"
        roles.mkdir()
        monkeypatch.setenv("AGENT_HUB_ROLES", str(roles))

        result = _check_from_dryrun("ghost-persona")

        assert not result.ok

    def test_invalid_name_fails_validation(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("AGENT_HUB_ROLES", "/tmp")

        result = _check_from_dryrun("../../../etc/passwd")

        assert not result.ok
        assert "--from" in result.detail

    def test_roles_env_not_set(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.delenv("AGENT_HUB_ROLES", raising=False)

        result = _check_from_dryrun("agent-hub-coder")

        assert not result.ok


# ---------------------------------------------------------------------------
# _check_workdir_dryrun
# ---------------------------------------------------------------------------


class TestCheckWorkdirDryrun:
    def test_workdir_not_exist_ok(self, tmp_path: Path) -> None:
        target = tmp_path / "new-persona-repo"

        result = _check_workdir_dryrun(target)

        assert result.ok
        assert "does not exist" in result.detail

    def test_workdir_exists_ng(self, tmp_path: Path) -> None:
        target = tmp_path / "existing-dir"
        target.mkdir()

        result = _check_workdir_dryrun(target)

        assert not result.ok
        assert "already exists" in result.detail

    def test_workdir_exists_as_file_ng(self, tmp_path: Path) -> None:
        target = tmp_path / "existing-file"
        target.write_text("oops", encoding="utf-8")

        result = _check_workdir_dryrun(target)

        assert not result.ok


# ---------------------------------------------------------------------------
# _check_env_dryrun
# ---------------------------------------------------------------------------


class TestCheckEnvDryrun:
    def test_all_required_set(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("AGENT_HUB_URL", "http://hub:3000/mcp")
        monkeypatch.setenv("GITHUB_PAT", "ghp_test")
        monkeypatch.setenv("AGENT_HUB_TENANT", "my-tenant")

        result = _check_env_dryrun()

        assert result.ok
        assert "AGENT_HUB_URL" in result.detail
        assert "GITHUB_PAT" in result.detail
        assert "AGENT_HUB_TENANT" in result.detail

    def test_tenant_not_set_still_ok(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("AGENT_HUB_URL", "http://hub:3000/mcp")
        monkeypatch.setenv("GITHUB_PAT", "ghp_test")
        monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)

        result = _check_env_dryrun()

        assert result.ok
        assert "optional" in result.detail.lower()

    def test_hub_url_missing_ng(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.delenv("AGENT_HUB_URL", raising=False)
        monkeypatch.setenv("GITHUB_PAT", "ghp_test")

        result = _check_env_dryrun()

        assert not result.ok
        assert "AGENT_HUB_URL" in result.detail

    def test_github_pat_missing_ng(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("AGENT_HUB_URL", "http://hub:3000/mcp")
        monkeypatch.delenv("GITHUB_PAT", raising=False)

        result = _check_env_dryrun()

        assert not result.ok
        assert "GITHUB_PAT" in result.detail

    def test_both_required_missing_ng(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.delenv("AGENT_HUB_URL", raising=False)
        monkeypatch.delenv("GITHUB_PAT", raising=False)

        result = _check_env_dryrun()

        assert not result.ok


# ---------------------------------------------------------------------------
# _check_repo_dryrun
# ---------------------------------------------------------------------------


class TestCheckRepoDryrun:
    def test_repo_not_exist_ok(self) -> None:
        with patch("subprocess.run") as mock_run:
            mock_run.return_value = MagicMock(returncode=1)
            result = _check_repo_dryrun("new-fresh-repo")

        assert result.ok
        assert "does not exist" in result.detail

    def test_repo_already_exists_ng(self) -> None:
        with patch("subprocess.run") as mock_run:
            mock_run.return_value = MagicMock(returncode=0)
            result = _check_repo_dryrun("existing-repo")

        assert not result.ok
        assert "already exists" in result.detail

    def test_gh_not_found_skip_ok(self) -> None:
        with patch("shutil.which", return_value=None):
            result = _check_repo_dryrun("any-repo")

        assert result.ok
        assert "skipped" in result.detail.lower()

    def test_gh_called_with_repo_view(self) -> None:
        with patch("subprocess.run") as mock_run, patch("shutil.which", return_value="/usr/bin/gh"):
            mock_run.return_value = MagicMock(returncode=1)
            _check_repo_dryrun("my-repos")

        args = mock_run.call_args[0][0]
        assert args == ["gh", "repo", "view", "my-repos"]

    def test_timeout_skipped_ok(self) -> None:
        with (
            patch("shutil.which", return_value="/usr/bin/gh"),
            patch("subprocess.run", side_effect=subprocess.TimeoutExpired(cmd="gh", timeout=15)),
        ):
            result = _check_repo_dryrun("any-repo")

        assert result.ok
        assert result.label == "repo"
        assert "skipped" in result.detail.lower()


# ---------------------------------------------------------------------------
# _check_handle_dryrun
# ---------------------------------------------------------------------------


class TestCheckHandleDryrun:
    def test_env_not_set_skip(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.delenv("AGENT_HUB_URL", raising=False)
        monkeypatch.delenv("GITHUB_PAT", raising=False)

        result = _check_handle_dryrun("hoge-coder")

        assert result.ok
        assert "skipped" in result.detail.lower()

    def test_handle_online_ng(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("AGENT_HUB_URL", "http://hub:3000/mcp")
        monkeypatch.setenv("GITHUB_PAT", "ghp_test")
        monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)

        participants = [{"userId": "hoge-coder", "is_online": True}]
        with patch(
            "agent_hub_bridges.new_persona.runner._fetch_participants_from_hub",
            return_value=participants,
        ):
            result = _check_handle_dryrun("hoge-coder")

        assert not result.ok
        assert "already online" in result.detail

    def test_handle_offline_ok(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("AGENT_HUB_URL", "http://hub:3000/mcp")
        monkeypatch.setenv("GITHUB_PAT", "ghp_test")

        with patch(
            "agent_hub_bridges.new_persona.runner._fetch_participants_from_hub",
            return_value=[],
        ):
            result = _check_handle_dryrun("hoge-coder")

        assert result.ok
        assert "not online" in result.detail

    def test_api_error_skip_ok(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("AGENT_HUB_URL", "http://hub:3000/mcp")
        monkeypatch.setenv("GITHUB_PAT", "ghp_test")

        with patch(
            "agent_hub_bridges.new_persona.runner._fetch_participants_from_hub",
            side_effect=ConnectionError("hub unreachable"),
        ):
            result = _check_handle_dryrun("hoge-coder")

        assert result.ok
        assert "skipped" in result.detail.lower()

    def test_exception_message_uses_type_name_not_str(
        self, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """例外 detail が type 名のみで、str(exc) (URL 等) を含まないことを確認する (Minor #2)。"""
        monkeypatch.setenv("AGENT_HUB_URL", "http://secret-hub:9999/mcp")
        monkeypatch.setenv("GITHUB_PAT", "ghp_secret_token")

        sensitive_message = "Connection refused: http://secret-hub:9999/mcp"

        with patch(
            "agent_hub_bridges.new_persona.runner._fetch_participants_from_hub",
            side_effect=ConnectionError(sensitive_message),
        ):
            result = _check_handle_dryrun("hoge-coder")

        assert result.ok
        assert "ConnectionError" in result.detail
        assert sensitive_message not in result.detail
        assert "secret-hub" not in result.detail


# ---------------------------------------------------------------------------
# _fetch_participants_from_hub (unit tests with mocked urllib)
# ---------------------------------------------------------------------------


def _make_mock_response(body: bytes, headers: dict[str, str]) -> MagicMock:
    """urllib.request.urlopen の戻り値を模倣する mock を作る。"""
    mock_resp = MagicMock()
    mock_resp.read.return_value = body
    mock_resp.headers = MagicMock()
    mock_resp.headers.get.side_effect = lambda k: headers.get(k)
    mock_resp.__enter__ = lambda self: self
    mock_resp.__exit__ = MagicMock(return_value=False)
    return mock_resp


class TestFetchParticipantsFromHub:
    def test_returns_participants_list(self) -> None:
        participants = [
            {"userId": "alice", "is_online": True},
            {"userId": "bob", "is_online": False},
        ]
        init_resp = _make_mock_response(
            body=json.dumps({"jsonrpc": "2.0", "result": {}, "id": 0}).encode(),
            headers={"Mcp-Session-Id": "sid-abc-123"},
        )
        notif_resp = _make_mock_response(body=b"", headers={})
        tool_resp = _make_mock_response(
            body=json.dumps({
                "jsonrpc": "2.0",
                "result": {"content": [{"text": json.dumps(participants)}]},
                "id": 1,
            }).encode(),
            headers={},
        )

        call_order = [init_resp, notif_resp, tool_resp]
        with patch("urllib.request.urlopen", side_effect=call_order):
            result = _fetch_participants_from_hub("http://hub:3000/mcp", "pat", None)

        assert result == participants

    def test_no_session_id_raises(self) -> None:
        init_resp = _make_mock_response(
            body=json.dumps({"jsonrpc": "2.0", "result": {}, "id": 0}).encode(),
            headers={},  # no Mcp-Session-Id
        )
        with patch("urllib.request.urlopen", return_value=init_resp):
            with pytest.raises(RuntimeError, match="no mcp-session-id"):
                _fetch_participants_from_hub("http://hub:3000/mcp", "pat", None)

    def test_empty_content_returns_empty_list(self) -> None:
        init_resp = _make_mock_response(
            body=b"{}",
            headers={"Mcp-Session-Id": "sid-xyz"},
        )
        notif_resp = _make_mock_response(body=b"", headers={})
        tool_resp = _make_mock_response(
            body=json.dumps({
                "jsonrpc": "2.0",
                "result": {"content": []},
                "id": 1,
            }).encode(),
            headers={},
        )
        with patch("urllib.request.urlopen", side_effect=[init_resp, notif_resp, tool_resp]):
            result = _fetch_participants_from_hub("http://hub:3000/mcp", "pat", None)

        assert result == []


# ---------------------------------------------------------------------------
# run_dry_run: 出力フォーマット・終了コード
# ---------------------------------------------------------------------------


class TestRunDryRun:
    def _make_all_ok_patches(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> tuple[Path, str]:
        """全チェック OK になるパッチを当てる。(workdir, repos) を返す。"""
        roles = tmp_path / "roles"
        (roles / "agent-hub-coder").mkdir(parents=True)
        (roles / "agent-hub-coder" / "CLAUDE.md").write_text("# CLAUDE", encoding="utf-8")
        monkeypatch.setenv("AGENT_HUB_ROLES", str(roles))
        monkeypatch.setenv("AGENT_HUB_URL", "http://hub:3000/mcp")
        monkeypatch.setenv("GITHUB_PAT", "ghp_test")
        monkeypatch.setenv("AGENT_HUB_TENANT", "test-tenant")

        workdir = tmp_path / "my-repos"  # does not exist → OK
        repos = "my-repos"
        return workdir, repos

    def test_all_ok_returns_0(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
    ) -> None:
        workdir, repos = self._make_all_ok_patches(tmp_path, monkeypatch)

        with (
            patch("subprocess.run", return_value=MagicMock(returncode=1)),  # repo not exist
            patch(
                "agent_hub_bridges.new_persona.runner._fetch_participants_from_hub",
                return_value=[],
            ),
        ):
            rc = run_dry_run(
                from_name="agent-hub-coder",
                name="my-coder",
                workdir=workdir,
                repos=repos,
            )

        assert rc == 0
        out = capsys.readouterr().out
        assert "[DRY-RUN]" in out
        assert "All checks passed" in out
        assert "✅" in out
        assert "❌" not in out

    def test_ng_returns_1(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
    ) -> None:
        workdir, repos = self._make_all_ok_patches(tmp_path, monkeypatch)
        workdir.mkdir()  # make it exist → NG

        with (
            patch("subprocess.run", return_value=MagicMock(returncode=1)),
            patch(
                "agent_hub_bridges.new_persona.runner._fetch_participants_from_hub",
                return_value=[],
            ),
        ):
            rc = run_dry_run(
                from_name="agent-hub-coder",
                name="my-coder",
                workdir=workdir,
                repos=repos,
            )

        assert rc == 1
        out = capsys.readouterr().out
        assert "❌" in out
        assert "check(s) failed" in out

    def test_output_shows_all_labels(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
    ) -> None:
        workdir, repos = self._make_all_ok_patches(tmp_path, monkeypatch)

        with (
            patch("subprocess.run", return_value=MagicMock(returncode=1)),
            patch(
                "agent_hub_bridges.new_persona.runner._fetch_participants_from_hub",
                return_value=[],
            ),
        ):
            run_dry_run(
                from_name="agent-hub-coder",
                name="my-coder",
                workdir=workdir,
                repos=repos,
            )

        out = capsys.readouterr().out
        for label in ["config", "--from", "--workdir", "env", "repo", "handle"]:
            assert label in out, f"label {label!r} missing from output"

    def test_fail_count_reported(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture
    ) -> None:
        """NG が 2 件の場合「2 check(s) failed」と出力される。"""
        workdir, repos = self._make_all_ok_patches(tmp_path, monkeypatch)
        workdir.mkdir()  # workdir NG
        # repo NG
        with (
            patch("subprocess.run", return_value=MagicMock(returncode=0)),  # repo exists
            patch(
                "agent_hub_bridges.new_persona.runner._fetch_participants_from_hub",
                return_value=[],
            ),
        ):
            run_dry_run(
                from_name="agent-hub-coder",
                name="my-coder",
                workdir=workdir,
                repos=repos,
            )

        out = capsys.readouterr().out
        assert "2 check(s) failed" in out


# ---------------------------------------------------------------------------
# CLI: --dry-run フラグ
# ---------------------------------------------------------------------------


class TestCliDryRunFlag:
    def test_dry_run_flag_calls_run_dry_run(self) -> None:
        """--dry-run が指定されたとき run_dry_run() が呼ばれる。"""
        from agent_hub_bridges.new_persona.cli import main

        with patch(
            "agent_hub_bridges.new_persona.cli.run_dry_run", return_value=0
        ) as mock_dry:
            rc = main(
                [
                    "--model", "bridge-claude",
                    "--from", "agent-hub-coder",
                    "--name", "test-coder",
                    "--workdir", "/tmp/test-coder",
                    "--repos", "test-coder",
                    "--dry-run",
                ]
            )

        mock_dry.assert_called_once()
        assert rc == 0

    def test_dry_run_exit_code_propagated(self) -> None:
        """run_dry_run() が返す終了コードが main() から伝播する。"""
        from agent_hub_bridges.new_persona.cli import main

        with patch(
            "agent_hub_bridges.new_persona.cli.run_dry_run", return_value=1
        ):
            rc = main(
                [
                    "--model", "bridge-claude",
                    "--from", "agent-hub-coder",
                    "--name", "test-coder",
                    "--workdir", "/tmp/test-coder",
                    "--repos", "test-coder",
                    "--dry-run",
                ]
            )

        assert rc == 1

    def test_dry_run_does_not_call_run_new_persona(self) -> None:
        """--dry-run 時は run_new_persona() を呼ばない。"""
        from agent_hub_bridges.new_persona.cli import main

        with (
            patch("agent_hub_bridges.new_persona.cli.run_dry_run", return_value=0),
            patch("agent_hub_bridges.new_persona.cli.run_new_persona") as mock_spawn,
        ):
            main(
                [
                    "--model", "bridge-claude",
                    "--from", "agent-hub-coder",
                    "--name", "test-coder",
                    "--workdir", "/tmp/test-coder",
                    "--repos", "test-coder",
                    "--dry-run",
                ]
            )

        mock_spawn.assert_not_called()
