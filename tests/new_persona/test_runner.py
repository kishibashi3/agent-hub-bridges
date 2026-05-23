"""Unit tests for agent_hub_bridges.new_persona.runner.

subprocess / shutil.which は mock。ファイル操作は tmp_path で実施。
"""

from __future__ import annotations

import time
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from agent_hub_bridges.new_persona.runner import (
    _BRIDGE_BINARIES,
    _resolve_bridge_binary,
    _resolve_claude_md,
    _rewrite_self_awareness,
    _validate_name,
    _wait_for_listening,
    run_new_persona,
)

# ---------------------------------------------------------------------------
# TestValidateName
# ---------------------------------------------------------------------------


class TestValidateName:
    @pytest.mark.parametrize(
        "value",
        ["agent-hub-coder", "hoge", "hoge123", "a", "my_persona", "a-b_c"],
    )
    def test_valid_names_pass(self, value: str) -> None:
        _validate_name(value, "--from")  # should not raise

    @pytest.mark.parametrize(
        "value",
        [
            "../evil",
            "../../etc/passwd",
            "",
            "-starts-with-dash",
            "_starts-with-underscore",
            "UPPER",
            "has space",
            "has/slash",
        ],
    )
    def test_invalid_names_raise(self, value: str) -> None:
        with pytest.raises(ValueError, match="--from"):
            _validate_name(value, "--from")


# ---------------------------------------------------------------------------
# TestResolveClaudeMd
# ---------------------------------------------------------------------------


class TestResolveClaudeMd:
    def test_missing_env_raises(self, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.delenv("AGENT_HUB_ROLES", raising=False)
        with pytest.raises(ValueError, match="AGENT_HUB_ROLES"):
            _resolve_claude_md("agent-hub-coder")

    def test_valid_path_returned(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        persona_dir = tmp_path / "agent-hub-coder"
        persona_dir.mkdir()
        (persona_dir / "CLAUDE.md").write_text("# template")
        monkeypatch.setenv("AGENT_HUB_ROLES", str(tmp_path))

        result = _resolve_claude_md("agent-hub-coder")
        assert result == (persona_dir / "CLAUDE.md").resolve()

    def test_missing_claude_md_raises(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        (tmp_path / "no-claude").mkdir()
        monkeypatch.setenv("AGENT_HUB_ROLES", str(tmp_path))
        with pytest.raises(FileNotFoundError):
            _resolve_claude_md("no-claude")

    def test_path_traversal_raises(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """symlink で roles_root 外を指す CLAUDE.md は拒否する."""
        persona_dir = tmp_path / "evil"
        persona_dir.mkdir()
        outside = tmp_path.parent / "secret.md"
        outside.write_text("secret")
        link = persona_dir / "CLAUDE.md"
        link.symlink_to(outside)
        monkeypatch.setenv("AGENT_HUB_ROLES", str(tmp_path))

        with pytest.raises(ValueError, match="escapes AGENT_HUB_ROLES"):
            _resolve_claude_md("evil")


# ---------------------------------------------------------------------------
# TestRewriteSelfAwareness
# ---------------------------------------------------------------------------


class TestRewriteSelfAwareness:
    _TEMPLATE = """\
## 自己認識

- **handle**: `@agent-hub-coder`
- **workdir**: `/path/to/template/`
- **mode**: `stateful`
"""

    def test_handle_replaced(self, tmp_path: Path) -> None:
        f = tmp_path / "CLAUDE.md"
        f.write_text(self._TEMPLATE, encoding="utf-8")
        _rewrite_self_awareness(f, name="hoge-coder", workdir=tmp_path / "hoge")
        assert "`@hoge-coder`" in f.read_text(encoding="utf-8")

    def test_workdir_replaced(self, tmp_path: Path) -> None:
        f = tmp_path / "CLAUDE.md"
        f.write_text(self._TEMPLATE, encoding="utf-8")
        _rewrite_self_awareness(f, name="hoge-coder", workdir=tmp_path / "hoge")
        assert str(tmp_path / "hoge") + "/" in f.read_text(encoding="utf-8")

    def test_mode_line_unchanged(self, tmp_path: Path) -> None:
        f = tmp_path / "CLAUDE.md"
        f.write_text(self._TEMPLATE, encoding="utf-8")
        _rewrite_self_awareness(f, name="hoge-coder", workdir=tmp_path / "hoge")
        assert "`stateful`" in f.read_text(encoding="utf-8")

    def test_original_handle_removed(self, tmp_path: Path) -> None:
        f = tmp_path / "CLAUDE.md"
        f.write_text(self._TEMPLATE, encoding="utf-8")
        _rewrite_self_awareness(f, name="hoge-coder", workdir=tmp_path / "hoge")
        assert "@agent-hub-coder" not in f.read_text(encoding="utf-8")


# ---------------------------------------------------------------------------
# TestResolveBridgeBinary
# ---------------------------------------------------------------------------


class TestResolveBridgeBinary:
    @pytest.mark.parametrize("model", sorted(_BRIDGE_BINARIES))
    def test_valid_model_resolves(self, model: str) -> None:
        with patch("shutil.which", return_value="/usr/bin/dummy"):
            result = _resolve_bridge_binary(model)
        assert result == "/usr/bin/dummy"

    def test_unknown_model_raises(self) -> None:
        with pytest.raises(ValueError, match="Unknown --model"):
            _resolve_bridge_binary("bridge-unknown")

    def test_binary_not_in_path_raises(self) -> None:
        with patch("shutil.which", return_value=None):
            with pytest.raises(FileNotFoundError, match="binary not found"):
                _resolve_bridge_binary("bridge-claude")


# ---------------------------------------------------------------------------
# TestWaitForListening
# ---------------------------------------------------------------------------


class TestWaitForListening:
    def test_returns_when_line_found(self, tmp_path: Path) -> None:
        log = tmp_path / "bridge.log"
        log.write_text(
            "starting\n[INFO] Hub session ready, listening on inbox...\n",
            encoding="utf-8",
        )
        _wait_for_listening(log, name="test-persona", timeout_s=5.0)  # should not raise

    def test_timeout_raises(self, tmp_path: Path) -> None:
        log = tmp_path / "bridge.log"
        log.write_text("starting\n", encoding="utf-8")
        with pytest.raises(TimeoutError, match="test-persona"):
            _wait_for_listening(log, name="test-persona", timeout_s=0.3)

    def test_incremental_read(self, tmp_path: Path) -> None:
        """別プロセスがログを追記するシナリオ: 最初は空、後から行が追加される."""
        log = tmp_path / "bridge.log"
        log.write_text("", encoding="utf-8")

        def _append_after_delay() -> None:
            time.sleep(0.2)
            with log.open("a", encoding="utf-8") as f:
                f.write("listening on inbox\n")

        import threading

        t = threading.Thread(target=_append_after_delay, daemon=True)
        t.start()
        _wait_for_listening(log, name="test-persona", timeout_s=3.0)
        t.join()


# ---------------------------------------------------------------------------
# TestWorkdirReposConsistency
# ---------------------------------------------------------------------------


class TestWorkdirReposConsistency:
    def test_mismatch_raises(self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("AGENT_HUB_ROLES", str(tmp_path))
        with pytest.raises(ValueError, match="must match"):
            run_new_persona(
                model="bridge-claude",
                from_name="agent-hub-coder",
                name="hoge-coder",
                workdir=tmp_path / "hoge",
                repos="different-name",
            )

    def test_existing_workdir_raises(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        existing = tmp_path / "hoge"
        existing.mkdir()
        monkeypatch.setenv("AGENT_HUB_ROLES", str(tmp_path))
        with pytest.raises(FileExistsError, match="already exists"):
            run_new_persona(
                model="bridge-claude",
                from_name="agent-hub-coder",
                name="hoge-coder",
                workdir=existing,
                repos="hoge",
            )


# ---------------------------------------------------------------------------
# TestRunNewPersona — subprocess mock による happy path
# ---------------------------------------------------------------------------


def _make_roles(tmp_path: Path, persona: str = "agent-hub-coder") -> Path:
    """テスト用 AGENT_HUB_ROLES を構築する."""
    roles = tmp_path / "roles"
    persona_dir = roles / persona
    persona_dir.mkdir(parents=True)
    (persona_dir / "CLAUDE.md").write_text(
        "## 自己認識\n"
        "- **handle**: `@agent-hub-coder`\n"
        "- **workdir**: `/placeholder/`\n",
        encoding="utf-8",
    )
    return roles


class TestRunNewPersona:
    def _run(
        self,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
        *,
        mock_popen: MagicMock,
        mock_run: MagicMock,
        model: str = "bridge-claude",
        name: str = "hoge-coder",
        repos: str = "hoge",
        tenant: str | None = None,
        public: bool = False,
        display_name: str | None = None,
    ) -> None:
        roles = _make_roles(tmp_path)
        monkeypatch.setenv("AGENT_HUB_ROLES", str(roles))
        workdir = tmp_path / repos

        # gh repo create の副作用として workdir を作成する
        def _side_effect(cmd, **_kwargs):
            if cmd[0] == "gh":
                workdir.mkdir(parents=True, exist_ok=True)
            return MagicMock(returncode=0)

        mock_run.side_effect = _side_effect

        # _wait_for_listening は TestWaitForListening で個別テスト済み。
        # ここでは subprocess 呼び出し構造のみ確認するため no-op にする。
        monkeypatch.setattr(
            "agent_hub_bridges.new_persona.runner._wait_for_listening",
            lambda *_a, **_kw: None,
        )

        run_new_persona(
            model=model,
            from_name="agent-hub-coder",
            name=name,
            workdir=workdir,
            repos=repos,
            tenant=tenant,
            public=public,
            display_name=display_name,
        )

    @patch("subprocess.Popen")
    @patch("subprocess.run")
    @patch("shutil.which", return_value="/usr/bin/agent-hub-bridge-claude")
    def test_gh_repo_create_called(
        self,
        mock_which: MagicMock,
        mock_run: MagicMock,
        mock_popen: MagicMock,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        self._run(tmp_path, monkeypatch, mock_popen=mock_popen, mock_run=mock_run)
        gh_calls = [c for c in mock_run.call_args_list if c.args[0][0] == "gh"]
        assert len(gh_calls) == 1
        assert "repo" in gh_calls[0].args[0]
        assert "create" in gh_calls[0].args[0]

    @patch("subprocess.Popen")
    @patch("subprocess.run")
    @patch("shutil.which", return_value="/usr/bin/agent-hub-bridge-claude")
    def test_gh_private_by_default(
        self,
        mock_which: MagicMock,
        mock_run: MagicMock,
        mock_popen: MagicMock,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        self._run(tmp_path, monkeypatch, mock_popen=mock_popen, mock_run=mock_run)
        gh_calls = [c for c in mock_run.call_args_list if c.args[0][0] == "gh"]
        assert "--private" in gh_calls[0].args[0]
        assert "--public" not in gh_calls[0].args[0]

    @patch("subprocess.Popen")
    @patch("subprocess.run")
    @patch("shutil.which", return_value="/usr/bin/agent-hub-bridge-claude")
    def test_gh_public_when_flag_set(
        self,
        mock_which: MagicMock,
        mock_run: MagicMock,
        mock_popen: MagicMock,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        self._run(
            tmp_path, monkeypatch, mock_popen=mock_popen, mock_run=mock_run, public=True
        )
        gh_calls = [c for c in mock_run.call_args_list if c.args[0][0] == "gh"]
        assert "--public" in gh_calls[0].args[0]

    @patch("subprocess.Popen")
    @patch("subprocess.run")
    @patch("shutil.which", return_value="/usr/bin/agent-hub-bridge-claude")
    def test_git_commit_message_contains_from_name(
        self,
        mock_which: MagicMock,
        mock_run: MagicMock,
        mock_popen: MagicMock,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        self._run(tmp_path, monkeypatch, mock_popen=mock_popen, mock_run=mock_run)
        commit_calls = [
            c for c in mock_run.call_args_list if "commit" in c.args[0]
        ]
        assert len(commit_calls) == 1
        commit_cmd = commit_calls[0].args[0]
        msg = commit_cmd[commit_cmd.index("-m") + 1]
        assert "agent-hub-coder" in msg

    @patch("subprocess.Popen")
    @patch("subprocess.run")
    @patch("shutil.which", return_value="/usr/bin/agent-hub-bridge-claude")
    def test_bridge_spawn_user_and_workdir(
        self,
        mock_which: MagicMock,
        mock_run: MagicMock,
        mock_popen: MagicMock,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        self._run(tmp_path, monkeypatch, mock_popen=mock_popen, mock_run=mock_run)
        assert mock_popen.called
        cmd = mock_popen.call_args.args[0]
        assert "--user" in cmd
        assert "hoge-coder" in cmd
        assert "--workdir" in cmd

    @patch("subprocess.Popen")
    @patch("subprocess.run")
    @patch("shutil.which", return_value="/usr/bin/agent-hub-bridge-claude")
    def test_bridge_spawn_with_tenant(
        self,
        mock_which: MagicMock,
        mock_run: MagicMock,
        mock_popen: MagicMock,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        self._run(
            tmp_path, monkeypatch, mock_popen=mock_popen, mock_run=mock_run, tenant="kaz"
        )
        cmd = mock_popen.call_args.args[0]
        assert "--tenant" in cmd
        assert "kaz" in cmd

    @patch("subprocess.Popen")
    @patch("subprocess.run")
    @patch("shutil.which", return_value="/usr/bin/agent-hub-bridge-claude")
    def test_bridge_spawn_no_tenant_by_default(
        self,
        mock_which: MagicMock,
        mock_run: MagicMock,
        mock_popen: MagicMock,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        self._run(tmp_path, monkeypatch, mock_popen=mock_popen, mock_run=mock_run)
        cmd = mock_popen.call_args.args[0]
        assert "--tenant" not in cmd

    @patch("subprocess.Popen")
    @patch("subprocess.run")
    @patch("shutil.which", return_value="/usr/bin/agent-hub-bridge-claude")
    def test_claude_md_written_to_workdir(
        self,
        mock_which: MagicMock,
        mock_run: MagicMock,
        mock_popen: MagicMock,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        roles = _make_roles(tmp_path)
        monkeypatch.setenv("AGENT_HUB_ROLES", str(roles))
        monkeypatch.setattr(
            "agent_hub_bridges.new_persona.runner._wait_for_listening",
            lambda *_a, **_kw: None,
        )
        workdir = tmp_path / "hoge"

        def _side_effect(cmd, **_kwargs):
            if cmd[0] == "gh":
                workdir.mkdir(parents=True, exist_ok=True)
            return MagicMock(returncode=0)

        mock_run.side_effect = _side_effect

        run_new_persona(
            model="bridge-claude",
            from_name="agent-hub-coder",
            name="hoge-coder",
            workdir=workdir,
            repos="hoge",
        )

        assert (workdir / "CLAUDE.md").exists()
        content = (workdir / "CLAUDE.md").read_text(encoding="utf-8")
        assert "@hoge-coder" in content
        assert str(workdir) + "/" in content
