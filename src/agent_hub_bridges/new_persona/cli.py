"""CLI entry point: `agent-hub-new-persona` — persona spawn in one command.

  - `--model` required (bridge type)
  - `--from`  required (meta-persona name in $AGENT_HUB_ROLES)
  - `--name`  required (new persona handle, without @)
  - `--workdir` required (clone destination)
  - `--repos`   required (GitHub repo name to create)
  - `--tenant` / `--public` / `--display-name` optional
"""

from __future__ import annotations

import sys
from pathlib import Path

from dotenv import load_dotenv

from agent_hub_bridges import __version__
from agent_hub_bridges.new_persona.runner import _BRIDGE_BINARIES, run_new_persona


def main(argv: list[str] | None = None) -> int:
    """CLI entry point."""
    load_dotenv()

    import argparse

    parser = argparse.ArgumentParser(
        prog="agent-hub-new-persona",
        description=(
            "Persona spawn utility: create GitHub repo, copy CLAUDE.md, "
            "rewrite self-awareness, commit, and spawn bridge — in one command."
        ),
    )
    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    parser.add_argument(
        "--model",
        required=True,
        choices=sorted(_BRIDGE_BINARIES),
        help="Bridge engine type.",
    )
    parser.add_argument(
        "--from",
        dest="from_name",
        required=True,
        metavar="META_PERSONA",
        help=(
            "Meta-persona directory name under $AGENT_HUB_ROLES "
            "(e.g. agent-hub-coder)."
        ),
    )
    parser.add_argument(
        "--name",
        required=True,
        help="New persona handle without @ (e.g. hoge-coder).",
    )
    parser.add_argument(
        "--workdir",
        required=True,
        help="Absolute path where the repo will be cloned. basename must match --repos.",
    )
    parser.add_argument(
        "--repos",
        required=True,
        help="GitHub repository name to create (e.g. hoge).",
    )
    parser.add_argument(
        "--tenant",
        default=None,
        help="agent-hub tenant name.",
    )
    parser.add_argument(
        "--public",
        action="store_true",
        default=False,
        help="Create repo as public (default: private).",
    )
    parser.add_argument(
        "--display-name",
        default=None,
        help="Bridge display_name.",
    )

    args = parser.parse_args(argv)

    try:
        run_new_persona(
            model=args.model,
            from_name=args.from_name,
            name=args.name,
            workdir=Path(args.workdir).resolve(),
            repos=args.repos,
            tenant=args.tenant,
            public=args.public,
            display_name=args.display_name,
        )
    except (ValueError, FileNotFoundError, FileExistsError) as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    except Exception as e:
        print(f"fatal: {type(e).__name__}: {e}", file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    sys.exit(main())
