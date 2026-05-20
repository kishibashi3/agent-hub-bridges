"""Unit tests for `agent_hub_bridges.claude.config`.

claude 固有の field (`anthropic_api_key`, `model`) + base の
`workdir: Optional[Path]` を required に絞り直した部分の挙動を 押さえる。
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from agent_hub_bridges.claude.config import DEFAULT_MODEL, Config


@pytest.fixture
def _hub_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_HUB_URL", "http://localhost:3000/mcp")
    monkeypatch.setenv("GITHUB_PAT", "ghp_test")
    monkeypatch.delenv("AGENT_HUB_DISPLAY_NAME", raising=False)
    monkeypatch.delenv("AGENT_HUB_TENANT", raising=False)
    monkeypatch.delenv("AGENT_HUB_WORKDIR", raising=False)
    monkeypatch.delenv("AGENT_HUB_MODEL", raising=False)


def test_config_happy_path(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-xxx")
    cfg = Config.from_env_and_args(
        user="claude-impl",
        display_name="Claude Implementer",
        tenant="alice",
        workdir=str(tmp_path),
    )
    assert cfg.user == "claude-impl"
    assert cfg.display_name == "Claude Implementer"
    assert cfg.tenant == "alice"
    assert cfg.agent_hub_url == "http://localhost:3000/mcp"
    assert cfg.github_pat == "ghp_test"
    assert cfg.anthropic_api_key == "sk-ant-xxx"
    assert cfg.workdir == tmp_path.resolve()


def test_config_anthropic_key_optional(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """`ANTHROPIC_API_KEY` 未設定でも 起動可 (= CLI auth fallback 想定)."""
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    cfg = Config.from_env_and_args(
        user="claude-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.anthropic_api_key is None


def test_config_workdir_defaults_to_cwd(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """`workdir=None` で `os.getcwd()` を 使う (= claude では required field)."""
    monkeypatch.chdir(tmp_path)
    cfg = Config.from_env_and_args(
        user="claude-impl", display_name=None, tenant=None, workdir=None
    )
    assert cfg.workdir == tmp_path.resolve()


def test_config_missing_required_env(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.delenv("AGENT_HUB_URL", raising=False)
    monkeypatch.delenv("GITHUB_PAT", raising=False)
    with pytest.raises(ValueError, match="AGENT_HUB_URL"):
        Config.from_env_and_args(
            user="claude-impl", display_name=None, tenant=None, workdir=str(tmp_path)
        )


def test_config_bad_workdir(monkeypatch: pytest.MonkeyPatch, _hub_env: None) -> None:
    with pytest.raises(ValueError, match="workdir does not exist"):
        Config.from_env_and_args(
            user="claude-impl",
            display_name=None,
            tenant=None,
            workdir="/nonexistent/path/that/does/not/exist",
        )


def test_config_is_frozen(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """`@dataclass(frozen=True)` で 不変、 misuse 防止."""
    cfg = Config.from_env_and_args(
        user="claude-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    with pytest.raises((AttributeError, Exception)):
        cfg.user = "evil"  # type: ignore[misc]


## --- Sonnet 4.6 model pin (= legacy bridge-claude#10 catch-up) ---
##
## Adds coverage for the ``model`` field 解決順位: CLI ``--model`` > env
## ``AGENT_HUB_MODEL`` > :data:`DEFAULT_MODEL` (= ``claude-sonnet-4-6``).
## Origin: operator DM 2026-05-21、 planner DM ``79f656f6-...`` (legacy
## bridge-claude#10), monorepo catch-up dispatch @bridges-impl DM
## ``6992f13a-...``.


def test_model_default_when_no_cli_no_env(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """CLI ``--model`` 未指定 + env ``AGENT_HUB_MODEL`` 未設定 →
    :data:`DEFAULT_MODEL` (= ``claude-sonnet-4-6``) が解決される.

    これが Sonnet 4.6 切替の core acceptance test。 default が 4.6 で固定
    されている事を locking する。 将来 4.7 に上げる時はこの assertion を
    更新する (= 意図的に手で動かす点が audit trail にもなる)。
    """
    cfg = Config.from_env_and_args(
        user="claude-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.model == "claude-sonnet-4-6"
    assert cfg.model == DEFAULT_MODEL


def test_model_env_overrides_default(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """env ``AGENT_HUB_MODEL`` が default を上書きする (CLI 未指定時)."""
    monkeypatch.setenv("AGENT_HUB_MODEL", "claude-opus-4-7")
    cfg = Config.from_env_and_args(
        user="claude-impl", display_name=None, tenant=None, workdir=str(tmp_path)
    )
    assert cfg.model == "claude-opus-4-7"


def test_model_cli_overrides_env(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """CLI ``--model`` が env を上書きする (= 優先順位 CLI > env > default)."""
    monkeypatch.setenv("AGENT_HUB_MODEL", "claude-opus-4-7")
    cfg = Config.from_env_and_args(
        user="claude-impl",
        display_name=None,
        tenant=None,
        workdir=str(tmp_path),
        model="claude-sonnet-4-6-20260501",  # date-pinned form も受け入れる
    )
    assert cfg.model == "claude-sonnet-4-6-20260501"


def test_model_cli_overrides_default_no_env(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """CLI ``--model`` が default を上書きする (env も未設定の場合)."""
    cfg = Config.from_env_and_args(
        user="claude-impl",
        display_name=None,
        tenant=None,
        workdir=str(tmp_path),
        model="claude-haiku-4-5",
    )
    assert cfg.model == "claude-haiku-4-5"


def test_model_empty_string_treated_as_unset(
    monkeypatch: pytest.MonkeyPatch, _hub_env: None, tmp_path: Path
) -> None:
    """``model=""`` (= falsy) は未指定扱い、 env や default に fallback する.

    argparse の ``default=None`` 経由なら None が来るが、 caller が誤って
    空文字を渡した時に「空文字で SDK call」のような壊れた挙動にならない
    保証。
    """
    monkeypatch.setenv("AGENT_HUB_MODEL", "claude-opus-4-7")
    cfg = Config.from_env_and_args(
        user="claude-impl",
        display_name=None,
        tenant=None,
        workdir=str(tmp_path),
        model="",
    )
    # env が拾われる (default ではなく)
    assert cfg.model == "claude-opus-4-7"


# silence unused import warning in some IDEs
_ = os
