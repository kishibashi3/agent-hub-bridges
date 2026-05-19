"""Stub CLI entry for `agent-hub-bridge-a2a` (M0; real impl in M4)."""

from __future__ import annotations

import sys


def main(argv: list[str] | None = None) -> int:
    """Stub entry point. Real implementation lands in M4."""
    del argv
    print(
        "agent-hub-bridge-a2a: M0 stub. "
        "Real implementation lands in M4 (new bridge; spec in agent-hub#94).",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
