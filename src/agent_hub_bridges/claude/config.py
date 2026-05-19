"""Bridge runtime configuration (claude-specific).

`_common.base_config.BaseConfig` (= 全 bridge 共通の env) に claude 固有の
field (`anthropic_api_key`) を 1 つだけ足した dataclass。 旧 repo
(`agent-hub-bridge-claude`) の `Config` から 1:1 移植、 共通項目は base 側
に委譲してある。
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from agent_hub_bridges._common.base_config import BaseConfig, load_base_config, load_optional_env


@dataclass(frozen=True)
class Config(BaseConfig):
    """claude bridge の runtime config.

    Attributes:
        anthropic_api_key: Anthropic API key (任意)。 None なら Claude SDK
            は `claude` CLI auth fallback で 動く前提。
        workdir: 作業対象 project root。 LLM 系 bridge では required なので
            base の `workdir: Path | None` を `Path` に絞り直す。
    """

    anthropic_api_key: str | None
    workdir: Path  # type: ignore[assignment]  # base の Optional を required に絞る

    @classmethod
    def from_env_and_args(
        cls,
        *,
        user: str,
        display_name: str | None,
        tenant: str | None,
        workdir: str | None,
    ) -> Config:
        """CLI 引数 + env から `Config` を組み立てる.

        必須 env (`GITHUB_PAT` / `AGENT_HUB_URL`) は `load_base_config` が
        fail-fast で 検証する。 `ANTHROPIC_API_KEY` は任意 (= CLI auth
        fallback)。

        `workdir` は base では Optional だが claude bridge では required:
        None なら `os.getcwd()` を使う。
        """
        import os

        # 共通 env (USER/PAT/URL/TENANT/DISPLAY_NAME) は base loader に委譲
        base = load_base_config(
            user=user,
            display_name=display_name,
            tenant=tenant,
            workdir=workdir if workdir is not None else os.getcwd(),
        )
        assert base.workdir is not None  # workdir をデフォルト cwd で渡したため

        return cls(
            user=base.user,
            display_name=base.display_name,
            tenant=base.tenant,
            agent_hub_url=base.agent_hub_url,
            github_pat=base.github_pat,
            workdir=base.workdir,
            anthropic_api_key=load_optional_env("ANTHROPIC_API_KEY"),
        )
