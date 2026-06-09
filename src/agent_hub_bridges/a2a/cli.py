"""CLI entry point: `agent-hub-bridge-a2a` で bridge を起動.

semantic:
  - `--participant` optional (default は env `AGENT_HUB_PARTICIPANT`、 それも無ければ
    'a2a-agent' を使う。 起動後に Agent Card 取得して 動的 register する
    流れではなく、 participant は **CLI で 固定**にする方が ops 上分かりやすい)
  - `--display-name` / `--tenant` optional (= 起動後 Agent Card の `.name`
    で 自動上書きする実装も 可能だが、 ops 視点で 明示的 control を 優先)
  - 必須 env (`A2A_AGENT_URL` / `AGENT_HUB_URL` / `GITHUB_PAT`) は
    Config 側で fail-fast 検証
"""

from __future__ import annotations

import asyncio
import os
import sys

from dotenv import load_dotenv

from agent_hub_bridges import __version__
from agent_hub_bridges._common.base_cli import build_common_parser
from agent_hub_bridges.a2a.config import Config
from agent_hub_bridges.a2a.worker import run_worker


def _resolve_participant(cli_value: str | None) -> str:
    """`--participant` の優先順位: CLI > env `AGENT_HUB_PARTICIPANT` > `'a2a-agent'` default."""
    return cli_value or os.environ.get("AGENT_HUB_PARTICIPANT") or "a2a-agent"


def main(argv: list[str] | None = None) -> int:
    """CLI entry point."""
    load_dotenv()

    parser = build_common_parser(
        prog="agent-hub-bridge-a2a",
        description="A2A client bridge: connects an external A2A agent to agent-hub as a peer.",
        version=__version__,
    )
    parser.add_argument(
        "--participant",
        default=None,
        help=(
            "agent-hub での handle (例: external-agent)。 @ 抜きで指定する。"
            " 未指定なら env AGENT_HUB_PARTICIPANT、 それも無ければ 'a2a-agent' を使う。"
        ),
    )

    args = parser.parse_args(argv)
    # a2a bridge は workdir を使わないので args.workdir は無視 (= 後方互換、
    # 共通 parser が受理する `--workdir` を 渡しても Config が None で 通す)。

    user = _resolve_participant(args.participant)

    try:
        config = Config.from_env_and_args(
            user=user,
            display_name=args.display_name,
            tenant=args.tenant,
        )
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    try:
        asyncio.run(run_worker(config))
    except KeyboardInterrupt:
        print("\nInterrupted, shutting down.", file=sys.stderr)
        return 130

    return 0


if __name__ == "__main__":
    sys.exit(main())
