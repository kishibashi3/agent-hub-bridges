"""Stub CLI entry for `agent-hub-bridge-slack` (M0; real impl in M2)."""

from __future__ import annotations

import sys


def main(argv: list[str] | None = None) -> int:
    """Stub entry point. Real implementation lands in M2."""
    del argv
    print(
        "agent-hub-bridge-slack: M0 stub. "
        "Real implementation lands in M2 (port from agent-hub-bridge-slack repo).",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
