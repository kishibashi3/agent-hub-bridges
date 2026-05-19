"""Bridge runtime configuration (a2a-specific).

`_common.base_config.BaseConfig` (= 全 bridge 共通の env) に a2a 固有の
field (`a2a_agent_url` / `a2a_agent_card_path`) を 足した dataclass。
"""

from __future__ import annotations

from dataclasses import dataclass

from agent_hub_bridges._common.base_config import (
    BaseConfig,
    load_base_config,
    load_optional_env,
    load_required_env,
)

# A2A spec の Agent Card の well-known path (= a2a-sdk が default で 使う
# パス)。 endpoint 側 で 別 path に置いている場合のみ override。
DEFAULT_AGENT_CARD_PATH = "/.well-known/agent.json"


@dataclass(frozen=True)
class Config(BaseConfig):
    """a2a bridge の runtime config.

    Attributes:
        a2a_agent_url: 外部 A2A agent endpoint の base URL (必須)。
            例: `https://external-agent.example.com`。
        a2a_agent_card_path: Agent Card endpoint の relative path
            (任意、 default `/.well-known/agent.json`)。
        workdir: a2a bridge では 使わない (= relay 系、 base の Optional を
            None で 通す)。
    """

    a2a_agent_url: str
    a2a_agent_card_path: str

    @classmethod
    def from_env_and_args(
        cls,
        *,
        user: str,
        display_name: str | None,
        tenant: str | None,
    ) -> Config:
        """CLI 引数 + env から `Config` を組み立てる.

        必須 env: `A2A_AGENT_URL` / `AGENT_HUB_URL` / `GITHUB_PAT`。
        欠けてたら ValueError (fail-fast)。 a2a bridge は workdir 不要
        なので base に None を渡す。
        """
        # 共通 env は base loader へ委譲
        base = load_base_config(
            user=user,
            display_name=display_name,
            tenant=tenant,
            workdir=None,
        )

        # a2a 固有 env
        a2a_agent_url = load_required_env("A2A_AGENT_URL")
        a2a_agent_card_path = (
            load_optional_env("A2A_AGENT_CARD_PATH") or DEFAULT_AGENT_CARD_PATH
        )

        return cls(
            user=base.user,
            display_name=base.display_name,
            tenant=base.tenant,
            agent_hub_url=base.agent_hub_url,
            github_pat=base.github_pat,
            workdir=base.workdir,
            a2a_agent_url=a2a_agent_url,
            a2a_agent_card_path=a2a_agent_card_path,
        )
