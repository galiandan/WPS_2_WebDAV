#!/usr/bin/env python3
"""Run a local, read-only probe against confirmed WPS endpoints.

The Cookie is entered with hidden input and is never printed. This tool is
intended for the account owner to run locally; do not send its Cookie or URL
to another person.
"""

from __future__ import annotations

import argparse
import getpass
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from wps_adapter.client import WpsApiError, WpsClientConfig, WpsDriveClient  # noqa: E402


def _add_common(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--group-id", required=True, help="your enterprise group ID")
    parser.add_argument("--base-url", default="https://365.kdocs.cn")
    parser.add_argument("--referer", help="optional WPS page URL; do not include credentials")


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    list_parser = subparsers.add_parser("list", help="list one remote folder")
    _add_common(list_parser)
    list_parser.add_argument("--parent-id", required=True, help="folder ID from your own capture")
    list_parser.add_argument("--count", type=int, default=20)
    list_parser.add_argument(
        "--observed-options",
        action="store_true",
        help="include the optional query parameters seen in the browser capture",
    )

    download_parser = subparsers.add_parser("download", help="stream one remote file to a local path")
    _add_common(download_parser)
    download_parser.add_argument("--file-id", required=True, help="file ID from your own listing")
    download_parser.add_argument("--output", type=Path, required=True)
    download_parser.add_argument("--cid", help="optional cid query value from your own session")
    download_parser.add_argument(
        "--direct-external",
        action="store_true",
        help="send the observed get_direct_external_download_url=true option",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    cookie = getpass.getpass("WPS Cookie (input hidden): ")
    client = WpsDriveClient(
        WpsClientConfig(
            group_id=args.group_id,
            cookie=cookie,
            base_url=args.base_url,
            referer=args.referer,
        )
    )

    try:
        if args.command == "list":
            options = {}
            if args.observed_options:
                options = {
                    "linkgroup": True,
                    "include": "acl,pic_thumbnail",
                    "with_link": True,
                    "review_pic_thumbnail": True,
                    "with_sharefolder_type": True,
                }
            page = client.list_entries(args.parent_id, count=args.count, **options)
            for entry in page.entries:
                size = "-" if entry.size is None else str(entry.size)
                print(f"{entry.id}\t{entry.kind}\t{size}\t{entry.name}")
            print(f"result={page.result or 'unknown'} next_offset={page.next_offset}")
            return 0

        written = 0
        args.output.parent.mkdir(parents=True, exist_ok=True)
        with args.output.open("wb") as destination:
            written = client.download_to(
                args.file_id,
                destination,
                cid=args.cid,
                get_direct_external_download_url=True if args.direct_external else None,
            )
        print(f"downloaded_bytes={written}")
        return 0
    except (OSError, ValueError, WpsApiError) as exc:
        print(f"probe failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
