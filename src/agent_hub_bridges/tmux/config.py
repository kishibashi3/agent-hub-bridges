"""bridge-tmux runtime configuration.

`_common.base_config.BaseConfig` に bridge-tmux 固有フィールドを追加した dataclass。

必須 env: `GITHUB_PAT` / `AGENT_HUB_URL` (BaseConfig 経由)。
`ANTHROPIC_API_KEY` は Tier2 (tmux セッション) に渡さない (subscription auth 優先)。

Issue: #110
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

# Tier2 (claude CLI) の default path
DEFAULT_CLAUDE_CLI_PATH = "claude"

# idle タイムアウト: warm セッションを N 秒無通信で kill する
DEFAULT_IDLE_TIMEOUT_S = 600.0  # 10 分 (standard)

# spawn タイムアウト: tmux セッション + claude 起動を待つ上限
DEFAULT_SPAWN_TIMEOUT_S = 60.0

# activity idle 閾値: N 秒間 pane 変化なし → 応答完了と判断
DEFAULT_ACTIVITY_IDLE_S = 8.0

# 1 メッセージあたりの応答タイムアウト
DEFAULT_RESPONSE_TIMEOUT_S = 300.0


@dataclass(frozen=True)
class Config(BaseConfig):
    """bridge-tmux の runtime config.

    Attributes:
        workdir: peer の作業 root (CLAUDE.md が置かれた dir)。
        claude_cli_path: `claude` CLI の path / 名前。
        model: Claude model 名 (None = デフォルト)。
        permission_bypass: True なら `--dangerously-skip-permissions`。
        idle_timeout_s: warm セッション idle kill タイムアウト (秒)。
        spawn_timeout_s: Tier2 起動待ちタイムアウト (秒)。
        activity_idle_s: 応答完了判定 — pane 変化なし続く秒数。
        response_timeout_s: 1 メッセージあたりの最大処理時間 (秒)。
    """

    workdir: Path  # type: ignore[assignment]  # BaseConfig の Optional を required に絞る
    claude_cli_path: str = DEFAULT_CLAUDE_CLI_PATH
    model: str | None = None
    permission_bypass: bool = True
    idle_timeout_s: float = DEFAULT_IDLE_TIMEOUT_S
    spawn_timeout_s: float = DEFAULT_SPAWN_TIMEOUT_S
    activity_idle_s: float = DEFAULT_ACTIVITY_IDLE_S
    response_timeout_s: float = DEFAULT_RESPONSE_TIMEOUT_S

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
        idle_timeout_s: float | None = None,
    ) -> Config:
        """CLI 引数 + env から Config を組み立てる."""
        base = load_base_config(
            user=user,
            display_name=display_name,
            tenant=tenant,
            workdir=workdir if workdir is not None else os.getcwd(),
        )
        assert base.workdir is not None

        resolved_cli_path = load_optional_env("CLAUDE_CLI_PATH") or DEFAULT_CLAUDE_CLI_PATH
        resolved_model = model or load_optional_env("AGENT_HUB_MODEL") or None

        if permission_bypass is None:
            permission_bypass = True

        resolved_idle = idle_timeout_s
        if resolved_idle is None:
            env_idle = load_optional_env("BRIDGE_TMUX_IDLE_TIMEOUT_S")
            resolved_idle = float(env_idle) if env_idle else DEFAULT_IDLE_TIMEOUT_S

        resolved_activity_idle = float(
            load_optional_env("BRIDGE_TMUX_ACTIVITY_IDLE_S") or DEFAULT_ACTIVITY_IDLE_S
        )
        resolved_response_timeout = float(
            load_optional_env("BRIDGE_TMUX_RESPONSE_TIMEOUT_S") or DEFAULT_RESPONSE_TIMEOUT_S
        )
        resolved_spawn_timeout = float(
            load_optional_env("BRIDGE_TMUX_SPAWN_TIMEOUT_S") or DEFAULT_SPAWN_TIMEOUT_S
        )

        return cls(
            user=base.user,
            display_name=base.display_name,
            tenant=base.tenant,
            agent_hub_url=base.agent_hub_url,
            github_pat=base.github_pat,
            workdir=base.workdir,
            claude_cli_path=resolved_cli_path,
            model=resolved_model,
            permission_bypass=permission_bypass,
            idle_timeout_s=resolved_idle,
            spawn_timeout_s=resolved_spawn_timeout,
            activity_idle_s=resolved_activity_idle,
            response_timeout_s=resolved_response_timeout,
        )
