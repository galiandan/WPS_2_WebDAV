#!/usr/bin/env python3
"""Build the file manifest verified by the one-command installers."""

from __future__ import annotations

import hashlib
import subprocess
import sys
from pathlib import Path


PROJECT_ROOT = Path(__file__).resolve().parents[1]
MANIFEST_PATH = PROJECT_ROOT / "release-manifest.txt"
EXCLUDED_PATHS = {
    "release-manifest.txt",
    "scripts/install-native.sh",
    "scripts/install-docker.sh",
}


def _tracked_paths() -> list[str]:
    completed = subprocess.run(
        ["git", "-C", str(PROJECT_ROOT), "ls-files", "-z"],
        check=True,
        stdout=subprocess.PIPE,
    )
    paths = set(completed.stdout.decode("utf-8").split("\0"))
    paths.discard("")
    paths.add(str(Path(__file__).relative_to(PROJECT_ROOT)))
    return sorted(path for path in paths if path not in EXCLUDED_PATHS)


def build() -> str:
    lines: list[str] = []
    for relative in _tracked_paths():
        path = PROJECT_ROOT / relative
        if not path.is_file() or path.is_symlink():
            raise RuntimeError(f"manifest path is not a regular file: {relative}")
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        lines.append(f"{digest}  {relative}")
    return "\n".join(lines) + "\n"


def main() -> int:
    generated = build()
    if "--check" in sys.argv[1:]:
        current = MANIFEST_PATH.read_text(encoding="utf-8") if MANIFEST_PATH.exists() else ""
        if current != generated:
            print(f"{MANIFEST_PATH} is out of date", file=sys.stderr)
            return 1
        return 0
    MANIFEST_PATH.write_text(generated, encoding="utf-8")
    print(f"wrote {MANIFEST_PATH}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
