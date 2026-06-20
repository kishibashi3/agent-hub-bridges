"""Targeted unit tests for `_common.base_config` (= Suggestion 3 from PR #2 review).

`tests/common/test_smoke.py` の M0 smoke を 補強し、 env loader の挙動を
具体的に押さえる。

- `load_required_env`: 存在 → 値、 不在 → ValueError、 空文字列 → ValueError
  (= fail-fast 原則、 redline #1 spirit application)
- `load_optional_env`: 存在 → 値、 不在 → default、 空文字列 → default
- `load_base_config`: 必須欠落で raise、 CLI 引数 > env > None の優先順、
  workdir 存在 / 非ディレクトリ / None の挙動
"""

from __future__ import annotations

import os

import pytest

from agent_hub_bridges._common.base_config import (
    load_base_config,
    load_github_pat,
    load_optional_env,
    load_required_env,
)

# ---------------------------------------------------------------------------
# load_required_env
# ---------------------------------------------------------------------------


def test_load_required_env_present(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("FOO", "bar")
    assert load_required_env("FOO") == "bar"


def test_load_required_env_missing(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("FOO", raising=False)
    with pytest.raises(ValueError, match="FOO"):
        load_required_env("FOO")


def test_load_required_env_empty_string_treated_as_missing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """空文字列も「未設定」 扱い。 fail-fast 原則 (= silent default 禁止)。

    旧 bridge 群でも 同 挙動だった: env を `=` 後に空で書くと None と同じ。
    """
    monkeypatch.setenv("FOO", "")
    with pytest.raises(ValueError, match="FOO"):
        load_required_env("FOO")


# ---------------------------------------------------------------------------
# load_optional_env
# ---------------------------------------------------------------------------


def test_load_optional_env_present(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OPT", "x")
    assert load_optional_env("OPT", default="d") == "x"


def test_load_optional_env_missing_uses_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OPT", raising=False)
    assert load_optional_env("OPT", default="d") == "d"


def test_load_optional_env_missing_default_none(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OPT", raising=False)
    assert load_optional_env("OPT") is None


def test_load_optional_env_empty_uses_default(monkeypatch: pytest.MonkeyPatch) -> None:
    """空文字列は不在扱いで default に倒れる (= `load_required_env` と整合)."""
    monkeypatch.setenv("OPT", "")
    assert load_optional_env("OPT", default="d") == "d"


# ---------------------------------------------------------------------------
# load_base_config
# ---------------------------------------------------------------------------


@pytest.fixture
def _hub_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)


# ---------------------------------------------------------------------------
# load_github_pat (AGENT_HUB_GITHUB_PAT 統一 + GITHUB_PAT deprecated alias)
# ---------------------------------------------------------------------------


def test_load_github_pat_prefers_canonical(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_GITHUB_PAT", "ghp_new")
    monkeypatch.setenv("GITHUB_PAT", "ghp_legacy")
    assert load_github_pat() == "ghp_new"


def test_load_github_pat_legacy_alias_accepted(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    monkeypatch.delenv("AGENT_HUB_GITHUB_PAT", raising=False)
    monkeypatch.setenv("GITHUB_PAT", "ghp_legacy")
    import logging

    with caplog.at_level(logging.WARNING):
        assert load_github_pat() == "ghp_legacy"
    # deprecation を WARN で通知している (ハード破壊しない段階的 deprecation)。
    assert any("deprecated" in r.message for r in caplog.records)


def test_load_github_pat_missing_both_raises(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("AGENT_HUB_GITHUB_PAT", raising=False)
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    with pytest.raises(ValueError, match="AGENT_HUB_GITHUB_PAT"):
        load_github_pat()


def test_load_base_config_missing_url(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    monkeypatch.setenv("GITHUB_PAT", "x")
    with pytest.raises(ValueError, match="AGENT_HUB_URL"):
        load_base_config(user="alice")


def test_load_base_config_missing_pat(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://h")
    monkeypatch.delenv("AGENT_HUB_GITHUB_PAT", raising=False)
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    with pytest.raises(ValueError, match="GITHUB_PAT"):
        load_base_config(user="alice")


def test_load_base_config_no_workdir_returns_none(_hub_env: None) -> None:
    cfg = load_base_config(user="alice")
    assert cfg.workdir is None


def test_load_base_config_workdir_from_arg(_hub_env: None, tmp_path: os.PathLike[str]) -> None:
    cfg = load_base_config(user="alice", workdir=str(tmp_path))
    assert cfg.workdir is not None
    assert cfg.workdir.is_dir()


def test_load_base_config_workdir_from_env(
    monkeypatch: pytest.MonkeyPatch,
    _hub_env: None,
    tmp_path: os.PathLike[str],
) -> None:
    monkeypatch.setenv("AGENT_HUB_WORKDIR", str(tmp_path))
    cfg = load_base_config(user="alice")
    assert cfg.workdir is not None
    assert cfg.workdir.is_dir()


def test_load_base_config_cli_arg_overrides_env_workdir(
    monkeypatch: pytest.MonkeyPatch,
    _hub_env: None,
    tmp_path: os.PathLike[str],
) -> None:
    other = tmp_path / "other"
    other.mkdir()
    monkeypatch.setenv("AGENT_HUB_WORKDIR", "/nonexistent/path/that/doesnt/exist")
    cfg = load_base_config(user="alice", workdir=str(other))
    assert cfg.workdir is not None
    assert cfg.workdir.resolve() == other.resolve()


def test_load_base_config_bad_workdir_raises(_hub_env: None) -> None:
    with pytest.raises(ValueError, match="workdir does not exist"):
        load_base_config(user="alice", workdir="/nonexistent/path/that/does/not/exist")


def test_load_base_config_display_name_cli_overrides_env(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None
) -> None:
    monkeypatch.setenv("AGENT_HUB_DISPLAY_NAME", "from-env")
    cfg = load_base_config(user="alice", display_name="from-cli")
    assert cfg.display_name == "from-cli"


def test_load_base_config_display_name_env_fallback(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None
) -> None:
    monkeypatch.setenv("AGENT_HUB_DISPLAY_NAME", "from-env")
    cfg = load_base_config(user="alice")
    assert cfg.display_name == "from-env"


def test_load_base_config_tenant_resolution(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None
) -> None:
    monkeypatch.setenv("AGENT_HUB_TENANT", "env-tenant")
    # CLI 引数優先
    cfg = load_base_config(user="alice", tenant="cli-tenant")
    assert cfg.tenant == "cli-tenant"
    # CLI 未指定なら env
    cfg = load_base_config(user="alice")
    assert cfg.tenant == "env-tenant"
