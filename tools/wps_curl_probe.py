#!/usr/bin/env python3
"""Replay a locally pasted Chrome cURL for one read-only WPS operation.

Run this program first, then paste Chrome's "Copy as cURL (bash)" output into
the terminal and press Ctrl-D. The cURL is parsed and used locally; it is
never printed. Only a count or byte count is printed.
"""

from __future__ import annotations

import argparse
import json
import shlex
import sys
from collections.abc import Mapping
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener


class CurlProbeError(RuntimeError):
    pass


MAX_JSON_RESPONSE_BYTES = 8 * 1024 * 1024


class _NoRedirectHandler(HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


def _validated_url(url: str, *, object_storage: bool = False) -> str:
    try:
        parts = urlsplit(url)
        port = parts.port
    except (TypeError, ValueError) as exc:
        raise CurlProbeError("captured URL is invalid") from exc
    host = parts.hostname.rstrip(".").casefold() if parts.hostname else ""
    suffix = ".ag.kdocs.cn" if object_storage else ".kdocs.cn"
    if (
        parts.scheme != "https"
        or not host
        or parts.username
        or parts.password
        or parts.fragment
        or port not in {None, 443}
        or not (host == suffix.lstrip(".") or host.endswith(suffix))
    ):
        raise CurlProbeError("captured URL is outside the WPS domain")
    return url


def _parse_curl(text: str) -> tuple[str, str, dict[str, str]]:
    normalized = text.replace("\\\r\n", " ").replace("\\\n", " ")
    try:
        tokens = shlex.split(normalized)
    except ValueError as exc:
        raise CurlProbeError("could not parse pasted cURL") from exc
    if not tokens or tokens[0].lower() not in {"curl", "curl.exe"}:
        raise CurlProbeError("pasted text does not start with curl")

    method = "GET"
    url = ""
    headers: dict[str, str] = {}
    index = 1
    while index < len(tokens):
        token = tokens[index]
        if token in {"-X", "--request"}:
            index += 1
            if index >= len(tokens):
                raise CurlProbeError("cURL request method is incomplete")
            method = tokens[index].upper()
        elif token in {"-H", "--header"}:
            index += 1
            if index >= len(tokens):
                raise CurlProbeError("cURL header is incomplete")
            name, separator, value = tokens[index].partition(":")
            if separator:
                headers[name.strip().lower()] = value.lstrip()
        elif token in {"-b", "--cookie"}:
            index += 1
            if index >= len(tokens):
                raise CurlProbeError("cURL cookie option is incomplete")
            headers["cookie"] = tokens[index]
        elif token in {"--url"}:
            index += 1
            if index >= len(tokens):
                raise CurlProbeError("cURL URL is incomplete")
            url = tokens[index]
        elif token.startswith("https://"):
            url = token
        index += 1

    _validated_url(url)
    if method != "GET":
        raise CurlProbeError("only GET cURLs are allowed by this read-only probe")
    return url, method, headers


def _request(url: str, headers: Mapping[str, str]) -> bytes:
    url = _validated_url(url)
    request = Request(url, method="GET")
    # Keep only headers useful for a same-origin read request. In particular,
    # do not forward browser pseudo-headers, content lengths, or compression.
    for name in ("accept", "accept-language", "cache-control", "origin", "pragma", "referer", "user-agent"):
        value = headers.get(name)
        if value:
            request.add_header(name, value)
    cookie = headers.get("cookie")
    if cookie:
        request.add_header("Cookie", cookie)
    try:
        response = build_opener(_NoRedirectHandler()).open(request, timeout=30)
    except HTTPError as exc:
        raise CurlProbeError(f"WPS request returned HTTP {exc.code}") from None
    except URLError as exc:
        raise CurlProbeError("WPS request could not be completed") from exc
    try:
        content_length = response.headers.get("Content-Length")
        if content_length is not None:
            try:
                if int(content_length) > MAX_JSON_RESPONSE_BYTES:
                    raise CurlProbeError("WPS JSON response is too large")
            except ValueError:
                pass
        body = response.read(MAX_JSON_RESPONSE_BYTES + 1)
        if len(body) > MAX_JSON_RESPONSE_BYTES:
            raise CurlProbeError("WPS JSON response is too large")
    finally:
        response.close()
    return body


def _json_request(url: str, headers: Mapping[str, str]) -> dict[str, object]:
    try:
        payload = json.loads(_request(url, headers).decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise CurlProbeError("WPS returned a non-JSON response") from exc
    if not isinstance(payload, dict):
        raise CurlProbeError("WPS returned an unexpected JSON shape")
    return payload


def _run_list(url: str, headers: Mapping[str, str]) -> int:
    if not urlsplit(url).path.endswith("/files"):
        raise CurlProbeError("the pasted cURL is not a files list request")
    payload = _json_request(url, headers)
    files = payload.get("files")
    result = payload.get("result") if isinstance(payload.get("result"), str) else "unknown"
    next_offset = payload.get("next_offset")
    count = len(files) if isinstance(files, list) else 0
    print(f"list_result={result} entries={count} next_offset={next_offset}")
    return 0


def _run_download(url: str, headers: Mapping[str, str], output: Path) -> int:
    if not urlsplit(url).path.endswith("/download"):
        raise CurlProbeError("the pasted cURL is not a download API request")
    payload = _json_request(url, headers)
    signed_url = payload.get("download_url") or payload.get("url")
    if not isinstance(signed_url, str):
        raise CurlProbeError("download response has no HTTPS download URL")
    signed_url = _validated_url(signed_url, object_storage=True)

    # The API URL is authenticated by Cookie. The returned object URL is
    # independently signed, so forwarding Cookie would be unnecessary.
    request = Request(signed_url, method="GET")
    request.add_header("Accept", "*/*")
    try:
        response = build_opener(_NoRedirectHandler()).open(request, timeout=30)
    except HTTPError as exc:
        raise CurlProbeError(f"object download returned HTTP {exc.code}") from None
    except URLError as exc:
        raise CurlProbeError("object download could not be completed") from exc

    total = 0
    output.parent.mkdir(parents=True, exist_ok=True)
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
    parser.add_argument("action", choices=("list", "download"))
    parser.add_argument("--output", type=Path, help="local destination for download")
    args = parser.parse_args(argv)
    if args.action == "download" and args.output is None:
        parser.error("download requires --output")

    try:
        pasted = sys.stdin.read()
        url, _method, headers = _parse_curl(pasted)
        if args.action == "list":
            return _run_list(url, headers)
        return _run_download(url, headers, args.output)
    except CurlProbeError as exc:
        print(f"probe failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
