"""Config.from_env_and_args の最低限のスモークテスト."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from agent_hub_bridges.gemini.config import (
    DEFAULT_GEMINI_CLI_PATH,
    DEFAULT_GEMINI_MODEL,
    Config,
)


@pytest.fixture
def env(monkeypatch, tmp_path):
    monkeypatch.setenv("GEMINI_API_KEY", "test-key")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.delenv("GEMINI_MODEL", raising=False)
    monkeypatch.delenv("GEMINI_CLI_PATH", raising=False)
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    return tmp_path


def test_from_env_and_args_minimal(env: Path) -> None:
    cfg = Config.from_env_and_args(
        user="bridge-gemini",
        display_name=None,
        tenant=None,
        workdir=str(env),
        model=None,
    )
    assert cfg.user == "bridge-gemini"
    assert cfg.gemini_api_key == "test-key"
    assert cfg.github_pat == "ghp_test"
    assert cfg.agent_hub_url == "http://localhost:3000/mcp"
    assert cfg.gemini_model == DEFAULT_GEMINI_MODEL
    assert cfg.workdir == env
    assert cfg.tenant is None
    assert cfg.display_name is None
    assert cfg.gemini_cli_path == DEFAULT_GEMINI_CLI_PATH


def test_cli_overrides_env(env: Path, monkeypatch) -> None:
    monkeypatch.setenv("GEMINI_MODEL", "from-env")
    monkeypatch.setenv("AGENT_HUB_TENANT", "team-env")
    cfg = Config.from_env_and_args(
        user="bridge-gemini",
        display_name="Display",
        tenant="cli-tenant",
        workdir=str(env),
        model="cli-model",
    )
    assert cfg.tenant == "cli-tenant"
    assert cfg.gemini_model == "cli-model"
    assert cfg.display_name == "Display"


def test_missing_required_env_raises(monkeypatch, tmp_path) -> None:
    # GEMINI_API_KEY / GITHUB_PAT / AGENT_HUB_URL を全部消す。
    # M3 SDK 移行で `load_required_env` 経由に切り替わり、 最初に欠落した
    # 1 つを raise する fail-fast に挙動が変わった。 旧版は 「全部を 1 度に
    # 報告」 していたが、 新版 (= `_common.base_config.load_base_config`)
    # は AGENT_HUB_URL → GITHUB_PAT → GEMINI_API_KEY の順で 1 つずつ。
    for k in ("GEMINI_API_KEY", "GITHUB_PAT", "AGENT_HUB_URL"):
        monkeypatch.delenv(k, raising=False)
    with pytest.raises(ValueError) as exc:
        Config.from_env_and_args(
            user="bridge-gemini",
            display_name=None,
            tenant=None,
            workdir=str(tmp_path),
            model=None,
        )
    # base loader が 最初に check する `AGENT_HUB_URL` で raise する
    assert "AGENT_HUB_URL" in str(exc.value)
    assert "missing" in str(exc.value).lower() or "required" in str(exc.value).lower()


def test_workdir_must_exist(env: Path) -> None:
    bogus = env / "nope"
    with pytest.raises(ValueError) as exc:
        Config.from_env_and_args(
            user="bridge-gemini",
            display_name=None,
            tenant=None,
            workdir=str(bogus),
            model=None,
        )
    assert "workdir does not exist" in str(exc.value)


def test_workdir_defaults_to_cwd(env: Path, monkeypatch) -> None:
    monkeypatch.chdir(env)
    cfg = Config.from_env_and_args(
        user="bridge-gemini",
        display_name=None,
        tenant=None,
        workdir=None,
        model=None,
    )
    # tmp path may resolve through symlinks (macOS); compare resolved.
    assert cfg.workdir == Path(os.getcwd()).resolve()
