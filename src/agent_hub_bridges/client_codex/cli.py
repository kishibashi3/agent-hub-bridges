"""CLI entry point: `agent-hub-client-codex --participant <name>` で client を起動.

  - `--participant` required
  - `--display-name` / `--tenant` / `--workdir` optional (env で上書き可)
  - `--model` / `--sandbox` / `--bypass-approvals` optional (env で上書き可)
  - 必須 env (`GITHUB_PAT` / `AGENT_HUB_URL`) は Config 側で fail-fast 検証
"""

from __future__ import annotations

import asyncio
import sys

from dotenv import load_dotenv

from agent_hub_bridges import __version__
from agent_hub_bridges._common.base_cli import build_common_parser
from agent_hub_bridges.client_codex.config import DEFAULT_SANDBOX_MODE, VALID_SANDBOX_MODES, Config
from agent_hub_bridges.client_codex.worker import run_worker


def main(argv: list[str] | None = None) -> int:
    """CLI entry point."""
    load_dotenv()

    parser = build_common_parser(
        prog="agent-hub-client-codex",
        description="Stateless client worker that runs OpenAI Codex CLI as an agent-hub peer.",
        version=__version__,
    )
    parser.add_argument(
        "--participant",
        required=True,
        help="agent-hub での handle (例: codex-impl)。@ 抜きで指定する。",
    )
    parser.add_argument(
        "--model",
        default=None,
        help="Codex model 名。未指定なら env AGENT_HUB_MODEL → codex デフォルト。",
    )
    parser.add_argument(
        "--sandbox",
        dest="sandbox_mode",
        default=None,
        choices=sorted(VALID_SANDBOX_MODES),
        help=(
            f"sandbox モード (default: {DEFAULT_SANDBOX_MODE})。"
            "env CODEX_SANDBOX_MODE でも設定可能。"
        ),
    )
    parser.add_argument(
        "--bypass-approvals",
        dest="approval_bypass",
        action="store_true",
        default=False,
        help=(
            "codex exec に --dangerously-bypass-approvals-and-sandbox を渡す。"
            "env CODEX_APPROVAL_BYPASS (non-empty) でも有効化可能。"
        ),
    )

    args = parser.parse_args(argv)

    try:
        config = Config.from_env_and_args(
            user=args.participant,
            display_name=args.display_name,
            tenant=args.tenant,
            workdir=args.workdir,
            model=args.model,
            sandbox_mode=args.sandbox_mode,
            approval_bypass=args.approval_bypass or None,
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
