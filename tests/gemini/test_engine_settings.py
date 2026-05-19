"""GeminiCLIEngine の isolated settings.json 生成ロジックのテスト.

`gemini` 本体は呼び出さない。`_write_isolated_settings` が user 設定の
agent-hub MCP 設定を継承しつつ、X-User-Id / X-Tenant-Id を bridge の
identity で上書きする挙動だけを検証する。
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agent_hub_bridges.gemini.config import Config
from agent_hub_bridges.gemini.engine import _write_isolated_settings


def _make_config(*, user: str, tenant: str | None, tmp_workdir: Path) -> Config:
    return Config(
        user=user,
        display_name=None,
        tenant=tenant,
        agent_hub_url="http://example.invalid/mcp",
        github_pat="ghp_test",
        gemini_api_key="key",
        gemini_model="gemini-2.5-flash",
        gemini_cli_path="gemini",
        workdir=tmp_workdir,
    )


def test_write_isolated_settings_no_user_settings(monkeypatch, tmp_path: Path) -> None:
    """user 設定が無い → minimal な agent-hub block を生成する."""
    # HOME を空 dir に向けて、user-level settings.json を存在しない状態にする
    monkeypatch.setattr(Path, "home", classmethod(lambda cls: tmp_path / "fakehome"))

    home_dir = tmp_path / "isolated"
    home_dir.mkdir()
    cfg = _make_config(user="gemini-test", tenant="kaz", tmp_workdir=tmp_path)

    _write_isolated_settings(home_dir, cfg)

    settings_path = home_dir / ".gemini" / "settings.json"
    payload = json.loads(settings_path.read_text())
    block = payload["mcpServers"]["agent-hub"]

    assert block["httpUrl"] == "http://example.invalid/mcp"
    assert block["headers"]["X-User-Id"] == "gemini-test"
    assert block["headers"]["X-Tenant-Id"] == "kaz"
    assert "Bearer" in block["headers"]["Authorization"]


def test_write_isolated_settings_inherits_and_overrides(
    monkeypatch, tmp_path: Path
) -> None:
    """user 設定の agent-hub block を継承し、identity headers だけ差し替える."""
    fake_home = tmp_path / "fakehome"
    (fake_home / ".gemini").mkdir(parents=True)
    user_settings = {
        "mcpServers": {
            "agent-hub": {
                "httpUrl": "http://hub.example/mcp",
                "headers": {
                    "Authorization": "Bearer ${GITHUB_PAT}",
                    "X-User-Id": "gemini-cli",  # ← これは上書きされるはず
                    "X-Tenant-Id": "kaz",  # ← これも override 対象
                    "X-Extra": "preserved",  # ← 関係ない header は残す
                },
            },
            "other-mcp": {"url": "http://other"},  # 関係ない server は伝播しない
        }
    }
    (fake_home / ".gemini" / "settings.json").write_text(json.dumps(user_settings))
    monkeypatch.setattr(Path, "home", classmethod(lambda cls: fake_home))

    home_dir = tmp_path / "isolated"
    home_dir.mkdir()
    cfg = _make_config(user="gemini-impl", tenant="kishi", tmp_workdir=tmp_path)
    _write_isolated_settings(home_dir, cfg)

    payload = json.loads((home_dir / ".gemini" / "settings.json").read_text())

    # agent-hub だけが入っている
    assert list(payload["mcpServers"].keys()) == ["agent-hub"]
    block = payload["mcpServers"]["agent-hub"]
    # URL は user 設定を継承
    assert block["httpUrl"] == "http://hub.example/mcp"
    # identity headers は bridge の値で上書き
    assert block["headers"]["X-User-Id"] == "gemini-impl"
    assert block["headers"]["X-Tenant-Id"] == "kishi"
    # 既存の関係ない header は残る
    assert block["headers"]["X-Extra"] == "preserved"
    # Authorization は user 設定のまま (env interpolation を保持)
    assert block["headers"]["Authorization"] == "Bearer ${GITHUB_PAT}"


def test_write_isolated_settings_strips_tenant_when_unset(
    monkeypatch, tmp_path: Path
) -> None:
    """tenant=None → X-Tenant-Id header を削除する (default tenant fallback)."""
    fake_home = tmp_path / "fakehome"
    (fake_home / ".gemini").mkdir(parents=True)
    (fake_home / ".gemini" / "settings.json").write_text(
        json.dumps(
            {
                "mcpServers": {
                    "agent-hub": {
                        "httpUrl": "http://hub.example/mcp",
                        "headers": {
                            "Authorization": "Bearer ${GITHUB_PAT}",
                            "X-User-Id": "gemini-cli",
                            "X-Tenant-Id": "kaz",
                        },
                    }
                }
            }
        )
    )
    monkeypatch.setattr(Path, "home", classmethod(lambda cls: fake_home))

    home_dir = tmp_path / "isolated"
    home_dir.mkdir()
    cfg = _make_config(user="gemini-default", tenant=None, tmp_workdir=tmp_path)
    _write_isolated_settings(home_dir, cfg)

    payload = json.loads((home_dir / ".gemini" / "settings.json").read_text())
    headers = payload["mcpServers"]["agent-hub"]["headers"]
    assert headers["X-User-Id"] == "gemini-default"
    assert "X-Tenant-Id" not in headers


@pytest.mark.parametrize("url_key", ["httpUrl", "url"])
def test_write_isolated_settings_normalizes_url_key(
    monkeypatch, tmp_path: Path, url_key: str
) -> None:
    """user 設定で `url` でも `httpUrl` でも、出力は `httpUrl` に正規化される."""
    fake_home = tmp_path / "fakehome"
    (fake_home / ".gemini").mkdir(parents=True)
    (fake_home / ".gemini" / "settings.json").write_text(
        json.dumps(
            {
                "mcpServers": {
                    "agent-hub": {
                        url_key: "http://hub.example/mcp",
                        "headers": {"Authorization": "Bearer x"},
                    }
                }
            }
        )
    )
    monkeypatch.setattr(Path, "home", classmethod(lambda cls: fake_home))

    home_dir = tmp_path / "isolated"
    home_dir.mkdir()
    cfg = _make_config(user="gemini-x", tenant=None, tmp_workdir=tmp_path)
    _write_isolated_settings(home_dir, cfg)

    block = json.loads(
        (home_dir / ".gemini" / "settings.json").read_text()
    )["mcpServers"]["agent-hub"]
    assert block["httpUrl"] == "http://hub.example/mcp"
    assert "url" not in block
