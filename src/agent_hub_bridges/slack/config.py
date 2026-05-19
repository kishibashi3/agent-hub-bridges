"""Bridge runtime configuration (slack-specific).

`_common.base_config.BaseConfig` (= 全 bridge 共通の env) に slack 固有の
field (`slack_bot_token` / `slack_app_token` / `slack_default_channel`) を
足した dataclass。 旧 repo の `Config` から 1:1 移植、 共通項目は base 側
に委譲してある。

slack bridge は relay 系 (= LLM engine 無し) なので `workdir` は使わない:
base の `workdir` を None で 通す。
"""

from __future__ import annotations

from dataclasses import dataclass

from agent_hub_bridges._common.base_config import (
    BaseConfig,
    load_base_config,
    load_optional_env,
    load_required_env,
)


@dataclass(frozen=True)
class Config(BaseConfig):
    """slack bridge の runtime config.

    Attributes:
        slack_bot_token: Socket Mode `xoxb-...` token (必須)。
        slack_app_token: Socket Mode `xapp-...` token (必須)。
        slack_default_channel: 未 bind peer の hub→Slack 投稿先 (任意)。
            None なら thread 未 bind の peer 宛 message は drop される。
        workdir: slack bridge では使わない (= base の Optional をそのまま継承)。
    """

    slack_bot_token: str
    slack_app_token: str
    slack_default_channel: str | None

    @classmethod
    def from_env_and_args(
        cls,
        *,
        user: str,
        display_name: str | None,
        tenant: str | None,
    ) -> Config:
        """CLI 引数 + env から `Config` を組み立てる.

        必須 env: `SLACK_BOT_TOKEN` / `SLACK_APP_TOKEN` / `AGENT_HUB_URL` /
        `GITHUB_PAT`。 欠けてたら ValueError (fail-fast)。 slack bridge は
        `workdir` を使わないので base に None を渡す。
        """
        # 共通 env (USER/PAT/URL/TENANT/DISPLAY_NAME) は base loader に委譲。
        # workdir は slack relay では不要なので 明示的に None。
        base = load_base_config(
            user=user,
            display_name=display_name,
            tenant=tenant,
            workdir=None,
        )

        # slack 固有 env: 2 token 必須 + default_channel optional
        slack_bot_token = load_required_env("SLACK_BOT_TOKEN")
        slack_app_token = load_required_env("SLACK_APP_TOKEN")
        slack_default_channel = load_optional_env("SLACK_DEFAULT_CHANNEL")

        return cls(
            user=base.user,
            display_name=base.display_name,
            tenant=base.tenant,
            agent_hub_url=base.agent_hub_url,
            github_pat=base.github_pat,
            workdir=base.workdir,
            slack_bot_token=slack_bot_token,
            slack_app_token=slack_app_token,
            slack_default_channel=slack_default_channel,
        )
