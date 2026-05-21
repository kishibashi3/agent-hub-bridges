"""Bridge runtime configuration (claude-specific).

`_common.base_config.BaseConfig` (= 全 bridge 共通の env) に claude 固有の
field (`anthropic_api_key`, `model`) を足した dataclass。 旧 repo
(`agent-hub-bridge-claude`) の `Config` から 1:1 移植、 共通項目は base 側
に委譲してある。
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from agent_hub_bridges._common.base_config import BaseConfig, load_base_config, load_optional_env

# Claude model default. Pin to a specific family/major.minor so the bridge
# stays on a known-good engine even if Claude CLI's own default shifts.
# Operator can override per-instance with `--model` or `AGENT_HUB_MODEL`.
#
# Switched 2026-05-21 from `claude-sonnet-4-5` → `claude-sonnet-4-6`
# (operator @ope-ultp1635 + @planner DM 79f656f6, L1 GO on legacy repo).
# Notes:
# - 4.6 is API-compatible with 4.5 (model id string change only).
# - Same pricing: $3/$15 per million.
# - 1M context window native, but @researcher PR #23 §2.2 / Cognition blog
#   flag performance degradation at 200k+ — keep context tight.
# - alias resolver accepts date-pinned form `claude-sonnet-4-6-YYYYMMDD`
#   too; we use the family alias for forward-compat with point releases.
DEFAULT_MODEL = "claude-sonnet-4-6"


@dataclass(frozen=True)
class Config(BaseConfig):
    """claude bridge の runtime config.

    Attributes:
        anthropic_api_key: Anthropic API key (任意)。 None なら Claude SDK
            は `claude` CLI auth fallback で 動く前提。
        workdir: 作業対象 project root。 LLM 系 bridge では required なので
            base の `workdir: Path | None` を `Path` に絞り直す。
        model: Claude model id (例: ``claude-sonnet-4-6``). Forwarded to
            ``ClaudeAgentOptions(model=...)``. Resolved from CLI ``--model``
            > env ``AGENT_HUB_MODEL`` > :data:`DEFAULT_MODEL`.
        add_dirs: workdir 以外に Claude がアクセスできる追加ディレクトリ。
            CLI ``--add-dir`` の複数指定を ``tuple[Path, ...]`` で保持。
            ``ClaudeAgentOptions(add_dirs=...)`` に渡す (issue #20)。
    """

    anthropic_api_key: str | None
    workdir: Path  # type: ignore[assignment]  # base の Optional を required に絞る
    model: str
    add_dirs: tuple[Path, ...] = ()  # issue #20: --add-dir で追加するディレクトリ

    @classmethod
    def from_env_and_args(
        cls,
        *,
        user: str,
        display_name: str | None,
        tenant: str | None,
        workdir: str | None,
        model: str | None = None,
        add_dirs: list[str] | None = None,
    ) -> Config:
        """CLI 引数 + env から `Config` を組み立てる.

        必須 env (`GITHUB_PAT` / `AGENT_HUB_URL`) は `load_base_config` が
        fail-fast で 検証する。 `ANTHROPIC_API_KEY` は任意 (= CLI auth
        fallback)。

        `workdir` は base では Optional だが claude bridge では required:
        None なら `os.getcwd()` を使う。

        ``model`` の解決順位は CLI ``--model`` > env ``AGENT_HUB_MODEL`` >
        :data:`DEFAULT_MODEL` (= ``claude-sonnet-4-6``)。
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

        resolved_model = model or load_optional_env("AGENT_HUB_MODEL") or DEFAULT_MODEL

        # issue #20: --add-dir を Path に変換 (resolve して絶対パス化)。
        # 呼出元が argparse の action=append を使っている場合、add_dirs は
        # list[str] または None (= 一度も指定されなかった場合)。
        resolved_add_dirs = tuple(
            Path(d).resolve() for d in (add_dirs or [])
        )

        return cls(
            user=base.user,
            display_name=base.display_name,
            tenant=base.tenant,
            agent_hub_url=base.agent_hub_url,
            github_pat=base.github_pat,
            workdir=base.workdir,
            anthropic_api_key=load_optional_env("ANTHROPIC_API_KEY"),
            model=resolved_model,
            add_dirs=resolved_add_dirs,
        )
