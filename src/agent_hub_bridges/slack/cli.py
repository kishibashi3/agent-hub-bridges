"""CLI entry point: `agent-hub-bridge-slack` で bridge を起動.

旧 repo の CLI と semantic 同等:
  - `--user` optional (default `slack-bot`、 env `AGENT_HUB_USER` fallback)
  - `--display-name` / `--tenant` optional
  - 必須 env (`SLACK_BOT_TOKEN` / `SLACK_APP_TOKEN` / `GITHUB_PAT` /
    `AGENT_HUB_URL`) は Config 側で fail-fast 検証
"""

from __future__ import annotations

import asyncio
import os
import sys

from dotenv import load_dotenv

from agent_hub_bridges import __version__
from agent_hub_bridges._common.base_cli import build_common_parser
from agent_hub_bridges.slack.config import Config
from agent_hub_bridges.slack.worker import run_worker


def _resolve_user(cli_value: str | None) -> str:
    """`--user` の優先順位: CLI > env `AGENT_HUB_USER` > `'slack-bot'` default."""
    return cli_value or os.environ.get("AGENT_HUB_USER") or "slack-bot"


def main(argv: list[str] | None = None) -> int:
    """CLI entry point."""
    load_dotenv()  # .env があれば読み込む (必須ではない)

    parser = build_common_parser(
        prog="agent-hub-bridge-slack",
        description="Stateful bridge worker that connects a Slack workspace to agent-hub.",
        version=__version__,
    )
    # slack では `--user` を optional に (default は env or 'slack-bot')。
    # bridge-claude / bridge-gemini の required と挙動が違う唯一の理由は、
    # slack bridge は workspace 単位の relay で peer 名が 1 つに固定される
    # ユースケースが多いから。
    parser.add_argument(
        "--user",
        default=None,
        help=(
            "agent-hub での handle (例: slack-bot)。 @ 抜きで指定する。"
            " 未指定なら env AGENT_HUB_USER、 それも無ければ 'slack-bot' を使う。"
        ),
    )

    args = parser.parse_args(argv)
    # slack bridge は workdir を 使わないので args.workdir は無視。 argparse の
    # 共通 parser で `--workdir` が 受理されるが Config に渡さない。

    user = _resolve_user(args.user)

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
