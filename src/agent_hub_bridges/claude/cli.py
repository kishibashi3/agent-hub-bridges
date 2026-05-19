"""CLI entry point: `agent-hub-bridge-claude --user <name>` で bridge を起動.

旧 repo の CLI と完全に同じ semantics:
  - `--user` required
  - `--display-name` / `--tenant` / `--workdir` optional (env で 上書き可)
  - 必須 env (`GITHUB_PAT` / `AGENT_HUB_URL`) は Config 側で fail-fast 検証
"""

from __future__ import annotations

import asyncio
import sys

from dotenv import load_dotenv

from agent_hub_bridges import __version__
from agent_hub_bridges._common.base_cli import build_common_parser
from agent_hub_bridges.claude.config import Config
from agent_hub_bridges.claude.worker import run_worker


def main(argv: list[str] | None = None) -> int:
    """CLI entry point."""
    load_dotenv()  # .env があれば読み込む (必須ではない)

    parser = build_common_parser(
        prog="agent-hub-bridge-claude",
        description="Stateful bridge worker that runs Claude as an agent-hub peer.",
        version=__version__,
    )
    parser.add_argument(
        "--user",
        required=True,
        help="agent-hub での handle (例: implementer, reviewer)。 @ 抜きで指定する。",
    )

    args = parser.parse_args(argv)

    try:
        config = Config.from_env_and_args(
            user=args.user,
            display_name=args.display_name,
            tenant=args.tenant,
            workdir=args.workdir,
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
