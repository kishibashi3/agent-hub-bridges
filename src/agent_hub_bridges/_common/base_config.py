"""全 bridge で 共通する env-based config の loader.

旧 bridge-claude / bridge-slack / bridge-gemini の `Config.from_env_and_args`
で 同じ env (USER / PAT / URL / TENANT / DISPLAY_NAME / WORKDIR) を
何度も読み直していたので、 ここに 1 度だけ書く。 bridge 固有 env は
各 bridge 側で `BaseConfig` を継承 (or 同梱) して 追加する。

M0 では shared dataclass のみ提供。 各 bridge への組み込みは M1 以降。
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class BaseConfig:
    """全 bridge が共通で必要とする runtime config.

    Attributes:
        user: agent-hub での自分の `@handle` (= `--participant` / env AGENT_HUB_PARTICIPANT)。
        display_name: 表示名 (任意)。 None なら server 側 default。
        tenant: tenant 名 (任意)。 None なら default tenant (雑談室)。
        agent_hub_url: agent-hub MCP endpoint (必須)。
        github_pat: agent-hub auth 用 GitHub PAT (必須)。
        workdir: 作業 root path (LLM 系 bridge のみ意味あり; relay 系は無視)。
    """

    user: str
    display_name: str | None
    tenant: str | None
    agent_hub_url: str
    github_pat: str
    workdir: Path | None


def load_required_env(name: str) -> str:
    """必須 env を読む; 無ければ `ValueError` を投げる.

    fail-fast 原則 (agent-hub-sdk の `ConfigurationError` 思想と同じ): 設定
    不足は起動時に明示的に落とす。 silent default は禁止。
    """
    value = os.environ.get(name)
    if not value:
        raise ValueError(f"required env var is missing: {name}")
    return value


def load_optional_env(name: str, default: str | None = None) -> str | None:
    """optional env を読む; 無ければ `default` (= None)."""
    return os.environ.get(name) or default


def load_base_config(
    *,
    user: str,
    display_name: str | None = None,
    tenant: str | None = None,
    workdir: str | os.PathLike[str] | None = None,
) -> BaseConfig:
    """CLI 引数 + env から `BaseConfig` を組み立てる.

    CLI 引数が None の場合は env を fallback (`AGENT_HUB_DISPLAY_NAME` /
    `AGENT_HUB_TENANT`)。 `workdir` は None で `os.getcwd()` を使う。

    Raises:
        ValueError: 必須 env (`AGENT_HUB_URL` / `GITHUB_PAT`) が無いか、
            workdir が 存在しないディレクトリの場合。
    """
    url = load_required_env("AGENT_HUB_URL")
    pat = load_required_env("GITHUB_PAT")

    resolved_display = display_name or load_optional_env("AGENT_HUB_DISPLAY_NAME")
    resolved_tenant = tenant or load_optional_env("AGENT_HUB_TENANT")

    resolved_workdir: Path | None
    if workdir is None and load_optional_env("AGENT_HUB_WORKDIR") is None:
        resolved_workdir = None
    else:
        chosen = workdir if workdir is not None else load_optional_env("AGENT_HUB_WORKDIR")
        assert chosen is not None  # guarded by branch above
        resolved_workdir = Path(chosen).resolve()
        if not resolved_workdir.is_dir():
            raise ValueError(
                f"workdir does not exist or is not a directory: {resolved_workdir}"
            )

    return BaseConfig(
        user=user,
        display_name=resolved_display,
        tenant=resolved_tenant,
        agent_hub_url=url,
        github_pat=pat,
        workdir=resolved_workdir,
    )
