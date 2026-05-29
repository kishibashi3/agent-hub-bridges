"""Client-codex runtime configuration (codex-specific, stateless).

`_common.base_config.BaseConfig` に codex 固有の field を足した dataclass。
設計詳細は docs/design-bridge-codex.md §6 を参照。

必須 env: `GITHUB_PAT` / `AGENT_HUB_URL`(BaseConfig 経由)。
codex auth は `~/.codex/auth.json` の idtoken を使うため `OPENAI_API_KEY` は不要。
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

# `codex` CLI 本体の path。env `CODEX_CLI_PATH` で override 可能。
DEFAULT_CODEX_CLI_PATH = "codex"

# sandbox モードのデフォルト: workdir 書き込み許可 + ネットワーク許可。
# read-only はネットワークアクセスをブロックするため、codex が MCP 経由で
# send_message を呼べない (issue #77)。デーモン bridge 運用では workspace-write 以上が必要。
DEFAULT_SANDBOX_MODE = "workspace-write"

# 許容する sandbox_mode 値。
VALID_SANDBOX_MODES = frozenset({"read-only", "workspace-write", "danger-full-access"})


@dataclass(frozen=True)
class Config(BaseConfig):
    """codex bridge の runtime config.

    Attributes:
        workdir: 作業対象 project root(required)。
        codex_cli_path: `codex` CLI binary の path / 名前。
        model: 使用する model(None で codex デフォルト)。
        sandbox_mode: codex exec の `-s` オプション値。
        approval_bypass: True なら `--dangerously-bypass-approvals-and-sandbox` を追加。
    """

    workdir: Path  # type: ignore[assignment]  # base の Optional を required に絞る
    codex_cli_path: str = DEFAULT_CODEX_CLI_PATH
    model: str | None = None
    sandbox_mode: str = DEFAULT_SANDBOX_MODE
    approval_bypass: bool = True  # デーモン運用デフォルト: MCP tool 呼び出し承認待ちを防止

    @classmethod
    def from_env_and_args(
        cls,
        *,
        user: str,
        display_name: str | None,
        tenant: str | None,
        workdir: str | None,
        model: str | None = None,
        sandbox_mode: str | None = None,
        approval_bypass: bool | None = None,
    ) -> Config:
        """CLI 引数 + env から `Config` を組み立てる.

        必須 env: `GITHUB_PAT` / `AGENT_HUB_URL`(BaseConfig 側で fail-fast)。
        `workdir` は None で `os.getcwd()` に fallback。

        `approval_bypass` env 解決ルール (issue #77):
          CODEX_APPROVAL_BYPASS 未設定 → True  (デーモン bridge デフォルト)
          CODEX_APPROVAL_BYPASS=""    → False (明示的無効化)
          CODEX_APPROVAL_BYPASS="1"  → True  (任意の non-empty 文字列)
        """
        base = load_base_config(
            user=user,
            display_name=display_name,
            tenant=tenant,
            workdir=workdir if workdir is not None else os.getcwd(),
        )
        assert base.workdir is not None

        resolved_cli_path = load_optional_env("CODEX_CLI_PATH") or DEFAULT_CODEX_CLI_PATH
        resolved_model = model or load_optional_env("AGENT_HUB_MODEL") or None

        # sandbox_mode: CLI > env > default
        resolved_sandbox = (
            sandbox_mode
            or load_optional_env("CODEX_SANDBOX_MODE")
            or DEFAULT_SANDBOX_MODE
        )
        if resolved_sandbox not in VALID_SANDBOX_MODES:
            raise ValueError(
                f"Invalid sandbox_mode {resolved_sandbox!r}. "
                f"Must be one of: {', '.join(sorted(VALID_SANDBOX_MODES))}"
            )

        # approval_bypass: CLI > env > True (デーモン bridge デフォルト)
        # env 解決ルール:
        #   CODEX_APPROVAL_BYPASS 未設定 → True (デーモン運用に必要)
        #   CODEX_APPROVAL_BYPASS="" (空文字) → False  (明示的に無効化)
        #   CODEX_APPROVAL_BYPASS="1" 等 non-empty → True
        if approval_bypass is None:
            env_val = os.environ.get("CODEX_APPROVAL_BYPASS")
            if env_val is None:
                approval_bypass = True  # unset → daemon default
            else:
                approval_bypass = bool(env_val.strip())

        return cls(
            user=base.user,
            display_name=base.display_name,
            tenant=base.tenant,
            agent_hub_url=base.agent_hub_url,
            github_pat=base.github_pat,
            workdir=base.workdir,
            codex_cli_path=resolved_cli_path,
            model=resolved_model,
            sandbox_mode=resolved_sandbox,
            approval_bypass=approval_bypass,
        )
