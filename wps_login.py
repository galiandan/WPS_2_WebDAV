#!/usr/bin/env python3
"""Run the dependency-free interactive WPS login helper from a checkout."""

from __future__ import annotations

import sys
from pathlib import Path


PROJECT_ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(PROJECT_ROOT / "src"))

from wps_adapter.__main__ import main  # noqa: E402


if __name__ == "__main__":
    raise SystemExit(main(["login", *sys.argv[1:]]))
