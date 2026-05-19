"""Bridge runtime configuration (gemini-specific).

`_common.base_config.BaseConfig` (= 全 bridge 共通の env) に gemini 固有の
field (`gemini_api_key` / `gemini_model` / `gemini_cli_path`) を 足した
dataclass。 旧 repo の `Config` から 1:1 移植、 共通項目は base 側に
委譲してある。
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from agent_hub_bridges._common.base_config import (
    BaseConfig,
    load_base_config,
    load_optional_env,
    load_required_env,
)

# 未指定時に使う Gemini model。 flash 系は安価で peer 用途に十分。
DEFAULT_GEMINI_MODEL = "gemini-2.5-flash"

# `gemini` CLI 本体の path。 env `GEMINI_CLI_PATH` で override 可能。
# PATH 上に `gemini` がある前提なので、 binary 名だけで十分。
DEFAULT_GEMINI_CLI_PATH = "gemini"


@dataclass(frozen=True)
class Config(BaseConfig):
    """gemini bridge の runtime config.

    Attributes:
        gemini_api_key: Gemini API key (必須、 gemini CLI が読む)。
        gemini_model: 使用する Gemini model (default `gemini-2.5-flash`)。
        gemini_cli_path: `gemini` CLI binary の path / 名前。
        workdir: 作業対象 project root。 LLM 系 bridge では required
            (= base の `workdir: Optional[Path]` を `Path` に narrowing)。
    """

    gemini_api_key: str
    gemini_model: str
    gemini_cli_path: str
    workdir: Path  # type: ignore[assignment]  # base の Optional を required に絞る

    @classmethod
    def from_env_and_args(
        cls,
        *,
        user: str,
        display_name: str | None,
        tenant: str | None,
        workdir: str | None,
        model: str | None,
    ) -> Config:
        """CLI 引数 + env から `Config` を組み立てる.

        必須 env: `GEMINI_API_KEY` / `GITHUB_PAT` / `AGENT_HUB_URL`。
        `GEMINI_MODEL` と `GEMINI_CLI_PATH` は optional。 `workdir` は
        None で `os.getcwd()` に fallback (= claude と同様、 gemini bridge
        では required field)。
        """
        import os

        # 共通 env (USER/PAT/URL/TENANT/DISPLAY_NAME) は base loader に委譲。
        # workdir は claude と同じく cwd を default にして必ず Path にする。
        base = load_base_config(
            user=user,
            display_name=display_name,
            tenant=tenant,
            workdir=workdir if workdir is not None else os.getcwd(),
        )
        assert base.workdir is not None  # workdir をデフォルト cwd で渡したため

        # gemini 固有 env
        gemini_api_key = load_required_env("GEMINI_API_KEY")
        resolved_model = model or load_optional_env("GEMINI_MODEL") or DEFAULT_GEMINI_MODEL
        resolved_cli_path = (
            load_optional_env("GEMINI_CLI_PATH") or DEFAULT_GEMINI_CLI_PATH
        )

        return cls(
            user=base.user,
            display_name=base.display_name,
            tenant=base.tenant,
            agent_hub_url=base.agent_hub_url,
            github_pat=base.github_pat,
            workdir=base.workdir,
            gemini_api_key=gemini_api_key,
            gemini_model=resolved_model,
            gemini_cli_path=resolved_cli_path,
        )
