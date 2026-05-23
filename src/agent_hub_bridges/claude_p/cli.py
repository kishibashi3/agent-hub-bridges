"""CLI entry point: `agent-hub-bridge-claude-p --user <name>` で bridge を起動.

  - `--user` required
  - `--display-name` / `--tenant` / `--workdir` optional (env で上書き可)
  - `--model` optional (env AGENT_HUB_MODEL で上書き可)
  - `--no-bypass-permissions` で permission bypass を無効化 (デフォルト: 有効)
"""

from __future__ import annotations

import asyncio
import sys

from dotenv import load_dotenv

from agent_hub_bridges import __version__
from agent_hub_bridges._common.base_cli import build_common_parser
from agent_hub_bridges.claude_p.config import Config
from agent_hub_bridges.claude_p.worker import run_worker


def main(argv: list[str] | None = None) -> int:
    """CLI entry point."""
    load_dotenv()

    parser = build_common_parser(
        prog="agent-hub-bridge-claude-p",
        description=(
            "On-demand bridge worker that runs Claude Code (claude -p) as an agent-hub peer."
        ),
        version=__version__,
    )
    parser.add_argument(
        "--user",
        required=True,
        help="agent-hub での handle (例: claude-p-impl)。@ 抜きで指定する。",
    )
    parser.add_argument(
        "--model",
        default=None,
        help="Claude model 名。未指定なら env AGENT_HUB_MODEL → claude デフォルト。",
    )
    parser.add_argument(
        "--no-bypass-permissions",
        dest="no_bypass_permissions",
        action="store_true",
        default=False,
        help=(
            "--dangerously-skip-permissions を付けない (デフォルト: 付ける)。"
            "permission 確認が必要な環境で使用する。"
        ),
    )

    args = parser.parse_args(argv)

    try:
        config = Config.from_env_and_args(
            user=args.user,
            display_name=args.display_name,
            tenant=args.tenant,
            workdir=args.workdir,
            model=args.model,
            permission_bypass=not args.no_bypass_permissions,
        )
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2

    try:
        asyncio.run(run_worker(config))
    except KeyboardInterrupt:
        print("\nInterrupted, shutting down.", file=sys.stderr)
        return 130
    except asyncio.CancelledError:
        # issue #58: SIGTERM → run_worker の add_signal_handler が task.cancel() を
        # 注入し、CancelledError が asyncio.run() の外に伝播する。
        # 標準 SIGTERM 終了コード 143 (= 128 + signal.SIGTERM) を返す。
        print("\nTerminated, shutting down.", file=sys.stderr)
        return 143

    return 0


if __name__ == "__main__":
    sys.exit(main())
