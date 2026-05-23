"""Bridge runtime configuration (claude_p-specific).

`_common.base_config.BaseConfig` に claude_p 固有の field を足した dataclass。
設計詳細は docs/design-bridge-claude-p.md §7 を参照。

必須 env: `GITHUB_PAT` / `AGENT_HUB_URL` (BaseConfig 経由)。
`ANTHROPIC_API_KEY` は意図的に渡さない (subscription auth を使うため)。
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

from agent_hub_bridges._common.base_config import (
    BaseConfig,
    load_base_config,
    load_optional_env,
)

# `claude` CLI 本体の path。env `CLAUDE_CLI_PATH` で override 可能。
DEFAULT_CLAUDEP_CLI_PATH = "claude"


@dataclass(frozen=True)
class Config(BaseConfig):
    """claude_p bridge の runtime config.

    Attributes:
        workdir: 作業対象 project root (required)。
        claudep_cli_path: `claude` CLI binary の path / 名前。
        model: 使用する model (None で claude デフォルト)。
        permission_bypass: True なら `--dangerously-skip-permissions` を追加。
            daemon として自動実行するためデフォルト True。
    """

    workdir: Path  # type: ignore[assignment]  # base の Optional を required に絞る
    claudep_cli_path: str = DEFAULT_CLAUDEP_CLI_PATH
    model: str | None = None
    permission_bypass: bool = True

    @classmethod
    def from_env_and_args(
        cls,
        *,
        user: str,
        display_name: str | None,
        tenant: str | None,
        workdir: str | None,
        model: str | None = None,
        permission_bypass: bool | None = None,
    ) -> Config:
        """CLI 引数 + env から `Config` を組み立てる.

        必須 env: `GITHUB_PAT` / `AGENT_HUB_URL` (BaseConfig 側で fail-fast)。
        `workdir` は None で `os.getcwd()` に fallback。
        `permission_bypass` は None でデフォルト True (daemon 用途)。
        """
        base = load_base_config(
            user=user,
            display_name=display_name,
            tenant=tenant,
            workdir=workdir if workdir is not None else os.getcwd(),
        )
        assert base.workdir is not None

        resolved_cli_path = load_optional_env("CLAUDE_CLI_PATH") or DEFAULT_CLAUDEP_CLI_PATH
        resolved_model = model or load_optional_env("AGENT_HUB_MODEL") or None

        # permission_bypass: CLI 指定があればそれ、なければデフォルト True
        if permission_bypass is None:
            permission_bypass = True

        return cls(
            user=base.user,
            display_name=base.display_name,
            tenant=base.tenant,
            agent_hub_url=base.agent_hub_url,
            github_pat=base.github_pat,
            workdir=base.workdir,
            claudep_cli_path=resolved_cli_path,
            model=resolved_model,
            permission_bypass=permission_bypass,
        )
