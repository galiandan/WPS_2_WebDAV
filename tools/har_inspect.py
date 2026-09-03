#!/usr/bin/env python3
"""Print a safe-first summary of a local browser HAR and optionally redact it."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from wps_adapter.har import redact_har, summarize_har  # noqa: E402


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("har", type=Path, help="local HAR file; it is read but never uploaded")
    parser.add_argument(
        "--redacted-out",
        type=Path,
        help="write a first-pass redacted HAR to this explicit path",
    )
    parser.add_argument(
        "--details",
        type=int,
        metavar="ENTRY",
        help="print value-free request/response structure for a 1-based entry",
    )
    args = parser.parse_args(argv)

    try:
        document = json.loads(args.har.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        parser.error(f"cannot read HAR: {exc}")

    if not isinstance(document, dict):
        parser.error("HAR root must be a JSON object")

    lines = summarize_har(document)
    print(f"entries: {len(lines)}")
    for line in lines:
        print(line)

    if args.details is not None:
        entries = document.get("log", {}).get("entries", [])
        if not isinstance(entries, list) or not 1 <= args.details <= len(entries):
            parser.error(f"ENTRY must be between 1 and {len(entries) if isinstance(entries, list) else 0}")
        entry = entries[args.details - 1]
        if not isinstance(entry, dict):
            parser.error("selected HAR entry is not an object")
        from wps_adapter.har import safe_entry_details

        print(f"details_entry: {args.details}")
        print(json.dumps(safe_entry_details(entry), ensure_ascii=True, indent=2))

    if args.redacted_out is not None:
        args.redacted_out.parent.mkdir(parents=True, exist_ok=True)
        args.redacted_out.write_text(
            json.dumps(redact_har(document), ensure_ascii=True, indent=2) + "\n",
            encoding="utf-8",
        )
        print(f"redacted_har: {args.redacted_out}")
        print("warning: review the redacted HAR manually before sharing it")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
