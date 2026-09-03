#!/usr/bin/env python3
"""Use a local HAR to replay one captured, read-only WPS operation.

Cookies are read locally from the HAR and are never printed. The tool also
never forwards WPS cookies to the object-storage download host.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections.abc import Mapping
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import Request, build_opener


class ProbeError(RuntimeError):
    pass


def _header_value(headers: object, wanted: str) -> str | None:
    if not isinstance(headers, list):
        return None
    for item in headers:
        if isinstance(item, Mapping) and str(item.get("name", "")).lower() == wanted.lower():
            value = item.get("value")
            return str(value) if value is not None else ""
    return None


def _entry_cookie(entry: Mapping[str, object]) -> str:
    request = entry.get("request")
    if not isinstance(request, Mapping):
        return ""

    cookies = request.get("cookies")
    if isinstance(cookies, list):
        pairs = []
        for cookie in cookies:
            if isinstance(cookie, Mapping) and cookie.get("name") is not None:
                pairs.append(f"{cookie['name']}={cookie.get('value', '')}")
        if pairs:
            return "; ".join(pairs)

    return _header_value(request.get("headers"), "cookie") or ""


def _find_cookie(entries: list[object], preferred: Mapping[str, object]) -> str:
    cookie = _entry_cookie(preferred)
    if cookie:
        return cookie
    for item in reversed(entries):
        if isinstance(item, Mapping):
            cookie = _entry_cookie(item)
            if cookie:
                return cookie
    raise ProbeError("the HAR has no Cookie value; export a fresh HAR while logged in")


def _load_entries(path: Path) -> list[object]:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProbeError("cannot read HAR") from exc
    log = document.get("log") if isinstance(document, Mapping) else None
    entries = log.get("entries") if isinstance(log, Mapping) else None
    if not isinstance(entries, list):
        raise ProbeError("HAR has no entries")
    return entries


def _request_url(entry: Mapping[str, object]) -> str:
    request = entry.get("request")
    url = request.get("url") if isinstance(request, Mapping) else None
    if not isinstance(url, str) or not url.startswith("https://"):
        raise ProbeError("selected HAR request has no HTTPS URL")
    return url


def _find_entry(entries: list[object], path_suffix: str) -> Mapping[str, object]:
    for item in reversed(entries):
        if not isinstance(item, Mapping):
            continue
        request = item.get("request")
        if not isinstance(request, Mapping) or str(request.get("method", "")).upper() != "GET":
            continue
        url = request.get("url")
        if not isinstance(url, str):
            continue
        if urlsplit(url).path.endswith(path_suffix):
            return item
    raise ProbeError(f"could not find a captured GET request ending in {path_suffix}")


def _fetch_json(url: str, cookie: str) -> dict[str, object]:
    request = Request(url, method="GET")
    request.add_header("Accept", "*/*")
    if cookie:
        request.add_header("Cookie", cookie)
    opener = build_opener()
    try:
        response = opener.open(request, timeout=30)
    except HTTPError as exc:
        raise ProbeError(f"WPS request returned HTTP {exc.code}") from None
    except URLError as exc:
        raise ProbeError("WPS request could not be completed") from exc
    try:
        raw = response.read()
    finally:
        response.close()
    try:
        payload = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProbeError("WPS returned a non-JSON response") from exc
    if not isinstance(payload, dict):
        raise ProbeError("WPS returned an unexpected JSON shape")
    return payload


def _list(entries: list[object]) -> int:
    selected = _find_entry(entries, "/files")
    cookie = _find_cookie(entries, selected)
    payload = _fetch_json(_request_url(selected), cookie)
    files = payload.get("files")
    count = len(files) if isinstance(files, list) else 0
    result = payload.get("result") if isinstance(payload.get("result"), str) else "unknown"
    next_offset = payload.get("next_offset")
    print(f"list_result={result} entries={count} next_offset={next_offset}")
    return 0


def _download(entries: list[object], output: Path) -> int:
    selected = _find_entry(entries, "/download")
    cookie = _find_cookie(entries, selected)
    payload = _fetch_json(_request_url(selected), cookie)
    signed_url = payload.get("download_url") or payload.get("url")
    if not isinstance(signed_url, str) or not signed_url.startswith("https://"):
        raise ProbeError("download response has no HTTPS download URL")

    # This request intentionally has no WPS Cookie; the URL itself is signed.
    request = Request(signed_url, method="GET")
    request.add_header("Accept", "*/*")
    try:
        response = build_opener().open(request, timeout=30)
    except HTTPError as exc:
        raise ProbeError(f"object download returned HTTP {exc.code}") from None
    except URLError as exc:
        raise ProbeError("object download could not be completed") from exc

    output.parent.mkdir(parents=True, exist_ok=True)
    total = 0
    try:
        with output.open("wb") as destination:
            while True:
                chunk = response.read(1024 * 1024)
                if not chunk:
                    break
                destination.write(chunk)
                total += len(chunk)
    finally:
        response.close()
    print(f"downloaded_bytes={total}")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("har", type=Path, help="local HAR exported while logged in")
    parser.add_argument("action", choices=("list", "download"))
    parser.add_argument("--output", type=Path, help="download destination for the download action")
    args = parser.parse_args(argv)

    if args.action == "download" and args.output is None:
        parser.error("download requires --output")

    try:
        entries = _load_entries(args.har)
        return _list(entries) if args.action == "list" else _download(entries, args.output)
    except ProbeError as exc:
        print(f"probe failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
