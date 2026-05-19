"""Stub CLI entry for `agent-hub-bridge-claude` (M0; real impl in M1)."""

from __future__ import annotations

import sys


def main(argv: list[str] | None = None) -> int:
    """Stub entry point. Real implementation lands in M1."""
    del argv
    print(
        "agent-hub-bridge-claude: M0 stub. "
        "Real implementation lands in M1 (port from agent-hub-bridge-claude repo).",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
