"""Bridge-codex CLI エントリポイント.

`agent-hub-bridge-codex` コンソールスクリプト から呼ばれる。
client_codex.cli と同構造。
"""

from __future__ import annotations

import argparse
import sys

import anyio

from agent_hub_bridges.codex.config import Config
from agent_hub_bridges.codex.worker import run_worker


def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="agent-hub-bridge-codex",
        description="agent-hub bridge backed by OpenAI Codex CLI (resident-process, MCP history)",
    )
    parser.add_argument("--user", required=True, help="agent-hub handle (without @)")
    parser.add_argument("--display-name", default=None, help="Display name for register()")
    parser.add_argument("--tenant", default=None, help="Tenant ID (optional)")
    parser.add_argument("--workdir", default=None, help="Working directory for codex exec")
    parser.add_argument("--model", default=None, help="Codex model override (-m)")
    parser.add_argument(
        "--sandbox",
        dest="sandbox_mode",
        default=None,
        choices=["read-only", "workspace-write", "danger-full-access"],
        help="Codex sandbox mode (default: danger-full-access)",
    )
    parser.add_argument(
        "--bypass-approvals",
        dest="approval_bypass",
        action="store_true",
        default=None,
        help="Add --dangerously-bypass-approvals-and-sandbox (default: True for daemon)",
    )
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> None:
    args = _parse_args(argv)
    config = Config.from_env_and_args(
        user=args.user,
        display_name=args.display_name,
        tenant=args.tenant,
        workdir=args.workdir,
        model=args.model,
        sandbox_mode=args.sandbox_mode,
        approval_bypass=args.approval_bypass,
    )
    try:
        anyio.run(run_worker, config)
    except KeyboardInterrupt:
        sys.exit(0)
