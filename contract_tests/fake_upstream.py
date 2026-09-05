"""Configurable fake WPS upstream + signed object store for contract tests.

Runs inside the service child process. Only the HTTP transport of the WPS
client is faked (the injection points the Python client already provides);
all adapter code, configuration files, and lifecycle are real.

All identifiers are "bench-*" placeholders. Nothing here contacts a real WPS
host and no real cookie, token, ID, or signed URL ever exists in this module.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import re
import threading
import time
from http.client import HTTPMessage
from io import BytesIO
from urllib.error import HTTPError
from urllib.parse import parse_qsl, quote, urlsplit

OBJECT_HOST = "benchobj.ag.kdocs.cn"
DEFAULT_FILE_ID = "bench-file-1"


def _default_listing() -> list[dict]:
    return [
        {
            "id": "bench-file-1",
            "fname": "bench-one.txt",
            "ftype": "file",
            "fsize": 11,
            "mtime": 1788268272,
            "fsha": "bench-etag-1",
            "parentid": "0",
            "link_id": "",
        },
        {
            "id": "bench-dir-1",
            "fname": "bench-folder",
            "ftype": "folder",
            "fsize": 0,
            "mtime": 1788268272,
            "fsha": "bench-etag-dir",
            "parentid": "0",
            "link_id": "",
        },
    ]


class _Response:
    def __init__(
        self,
        body: bytes,
        status: int = 200,
        headers: dict[str, str] | None = None,
        content_type: str = "application/json",
    ) -> None:
        self.status = status
        self._body = BytesIO(body)
        self.headers = HTTPMessage()
        self.headers["Content-Type"] = content_type
        for key, value in (headers or {}).items():
            self.headers[key] = value
        self.closed = False

    def read(self, size: int = -1) -> bytes:
        return self._body.read(size)

    def getheaders(self) -> list[tuple[str, str]]:
        return [(name, value) for name, value in self.headers.items()]

    def close(self) -> None:
        self.closed = True
        self._body.close()


class _ObjectStream(_Response):
    """GET on the signed object host; body generated blockwise, never fully held."""

    def __init__(self, content: bytes, start: int = 0) -> None:
        super().__init__(
            b"",
            status=200 if start == 0 else 206,
            headers=(
                {}
                if start == 0
                else {"Content-Range": f"bytes {start}-{len(content) - 1}/{len(content)}"}
            ),
            content_type="application/octet-stream",
        )
        self.headers["Content-Length"] = str(len(content) - start)
        self._content = content
        self._pos = start

    def read(self, size: int = -1) -> bytes:
        if self.closed:
            return b""
        chunk = self._content[self._pos :] if size in (-1, None) else self._content[self._pos : self._pos + size]
        self._pos += len(chunk)
        return chunk


class _SignedConnection:
    """Stands in for an HTTPSConnection PUTting one object."""

    def __init__(self, upstream: "FakeUpstream") -> None:
        self._upstream = upstream
        self._hasher = hashlib.sha256()
        self._size = 0

    def putrequest(self, method: str, target: str) -> None:
        self._method = method

    def putheader(self, name: str, value: str) -> None:
        pass

    def endheaders(self) -> None:
        pass

    def send(self, data: bytes) -> None:
        self._hasher.update(data)
        self._size += len(data)

    def getresponse(self) -> _Response:
        self._upstream.note_object_upload(self._size, self._hasher.hexdigest())
        return _Response(
            b"",
            status=200,
            headers={"ETag": '"bench-object-etag"', "x-obs-save-key": "bench-save-key"},
            content_type="text/plain",
        )

    def close(self) -> None:
        pass


class FakeUpstream:
    """Fake WPS control API + signed object store.

    Scenario JSON shape::

        {
          "routes": [
            {"match": "<regex on URL path>",
             "method": "GET",              # optional filter
             "status": 200,
             "json": {...},                # JSON body
             "body": "raw text",           # or raw body
             "headers": {...},
             "delay_ms": 0,
             "barrier": {"count": 4, "timeout_s": 5.0, "timeout_status": 503},
             "key": "named-route"}
          ],
          "listing": [...],                # overrides default root listing
          "objects": {"bench-file-1": "<base64>"}
        }

    Built-in handlers answer the standard observed endpoints when no route
    matches. Every request is appended to the JSONL record file and in-flight
    counters are mirrored into the stats file.
    """

    def __init__(self, scenario: dict, record_path: str, stats_path: str) -> None:
        self.routes = scenario.get("routes", [])
        self.listing = scenario.get("listing") or _default_listing()
        self.objects = {
            key: base64.b64decode(value) for key, value in scenario.get("objects", {}).items()
        }
        if DEFAULT_FILE_ID not in self.objects:
            self.objects[DEFAULT_FILE_ID] = b"bench-bytes"
        self.record_path = record_path
        self.stats_path = stats_path
        self._lock = threading.Lock()
        self._barrier = threading.Condition()
        self._inflight: dict[str, int] = {}
        self._released: dict[str, int] = {}
        self.stats: dict = {"inflight": {}, "served": {}, "object_put_size": None, "object_put_sha256": None}
        self._write_stats()

    # -- observability -------------------------------------------------------

    def _write_stats(self) -> None:
        with self._lock:
            snapshot = json.dumps(self.stats)
        # Unique per write: several request threads can finish at once.
        temporary = f"{self.stats_path}.{threading.get_ident()}.tmp"
        with open(temporary, "w", encoding="utf-8") as handle:
            handle.write(snapshot)
        os.replace(temporary, self.stats_path)

    def _record(self, request) -> None:  # noqa: ANN001 - urllib Request
        url = request.get_full_url()
        parts = urlsplit(url)
        entry = {
            "t": round(time.time(), 3),
            "method": request.get_method(),
            "path": parts.path,
            "query": parts.query,
            "host": (parts.hostname or "").casefold(),
            "cookie": request.headers.get("Cookie") is not None,
            "authorization": request.headers.get("Authorization") is not None,
            "origin": request.headers.get("Origin"),
            "referer": request.headers.get("Referer"),
        }
        with self._lock:
            with open(self.record_path, "a", encoding="utf-8") as handle:
                handle.write(json.dumps(entry) + "\n")

    def _count(self, key: str, delta: int) -> None:
        with self._lock:
            if delta > 0:
                self.stats["inflight"][key] = self.stats["inflight"].get(key, 0) + delta
                self.stats["served"][key] = self.stats["served"].get(key, 0) + 1
            else:
                remaining = self.stats["inflight"].get(key, 0) + delta
                if remaining > 0:
                    self.stats["inflight"][key] = remaining
                else:
                    self.stats["inflight"].pop(key, None)
        self._write_stats()

    def note_object_upload(self, size: int, sha256: str) -> None:
        with self._lock:
            self.stats["object_put_size"] = size
            self.stats["object_put_sha256"] = sha256
        self._write_stats()

    # -- route matching ------------------------------------------------------

    def _match_route(self, method: str, path: str) -> dict | None:
        for route in self.routes:
            if re.fullmatch(route["match"], path) is None:
                continue
            if "method" in route and route["method"] != method:
                continue
            return route
        return None

    def _route_response(self, route: dict, method: str, path: str) -> _Response:
        key = route.get("key") or f"{method} {path}"
        delay = route.get("delay_ms", 0)
        if delay:
            time.sleep(delay / 1000.0)
        barrier = route.get("barrier")
        status = int(route.get("status", 200))
        if barrier:
            timeout_s = float(barrier.get("timeout_s", 5.0))
            needed = int(barrier["count"])
            with self._barrier:
                self._inflight[key] = self._inflight.get(key, 0) + 1
                arrival = self._inflight[key]
                if arrival >= needed:
                    # Release this whole arrival wave.
                    self._released[key] = arrival
                    self._barrier.notify_all()
                    reached = True
                else:
                    deadline = time.monotonic() + timeout_s
                    while self._released.get(key, 0) < arrival and time.monotonic() < deadline:
                        self._barrier.wait(timeout=0.05)
                    reached = self._released.get(key, 0) >= arrival
                self._inflight[key] -= 1
            if not reached:
                status = int(barrier.get("timeout_status", 503))
        if "json" in route:
            return _Response(json.dumps(route["json"]).encode(), status=status, headers=route.get("headers", {}))
        if "body" in route:
            return _Response(route["body"].encode(), status=status, headers=route.get("headers", {}))
        return _Response(b"", status=status, headers=route.get("headers", {}))

    # -- urllib opener interface ----------------------------------------------

    def open(self, request, timeout: float):  # noqa: ANN001 - matches urllib signature
        self._record(request)
        url = request.get_full_url()
        parts = urlsplit(url)
        path = parts.path
        method = request.get_method()
        host = (parts.hostname or "").casefold()
        query = dict(parse_qsl(parts.query))

        route = self._match_route(method, path)
        if route is not None:
            key = route.get("key") or f"{method} {path}"
            self._count(key, +1)
            try:
                return self._as_transport_response(self._route_response(route, method, path), url)
            finally:
                self._count(key, -1)

        if host == OBJECT_HOST:
            return self._open_object(path)

        if path == "/api/v3/islogin":
            return _Response(json.dumps({"islogin": True, "companyid": "bench-company"}).encode())

        if path == "/passport/secure/api/grant_token":
            # SDK refresh grant: success means the rotated session cookie is
            # persisted by the client from the Set-Cookie header.
            return _Response(
                json.dumps({"result": "ok"}).encode(),
                headers={"Set-Cookie": "bench-session=rotated-bench-cookie; Domain=.kdocs.cn; Path=/"},
            )

        if re.fullmatch(r"/3rd/drive/api/v5/groups/[^/]+/files", path):
            parent_id = query.get("parentid", "0")
            offset = int(query.get("offset", "0"))
            count = int(query.get("count", "20"))
            payload = {"files": self.listing, "result": "ok"}
            return _Response(json.dumps(payload).encode())

        match = re.fullmatch(r"/api/v3/office/file/([^/]+)/download", path)
        if match:
            fid = match.group(1)
            return _Response(
                json.dumps(
                    {
                        "download_url": f"https://{OBJECT_HOST}/objects/{quote(fid, safe='')}?sig=bench",
                        "status": "finished",
                    }
                ).encode()
            )

        if path == "/3rd/drive/api/v5/files/upload/pre_check":
            return _Response(json.dumps({"result": "ok"}).encode())

        if path == "/3rd/drive/api/v5/files/upload/create_update":
            return _Response(
                json.dumps(
                    {
                        "url": f"https://{OBJECT_HOST}/upload-bench?sig=bench",
                        "response": {"expect_code": [200]},
                        "store": "bench-store",
                    }
                ).encode()
            )

        if path == "/3rd/drive/api/v5/files/file" and method == "POST":
            body = json.loads(request.data or b"{}")
            return _Response(
                json.dumps(
                    {
                        "result": "ok",
                        "id": "bench-file-new",
                        "fname": body.get("name", "bench-upload.bin"),
                        "ftype": "file",
                        "fsize": body.get("size", 0),
                        "mtime": 1788268272,
                        "parentid": body.get("parentid", "0"),
                    }
                ).encode()
            )

        raise AssertionError(f"fake upstream has no route for {method} {url}")

    def _as_transport_response(self, response: _Response, url: str) -> _Response:
        """Real urllib raises HTTPError for >=400; an injected opener must too."""

        if response.status >= 400:
            error = HTTPError(url, response.status, "error", response.headers, BytesIO(response.read()))
            error.headers = response.headers
            raise error
        return response

    def _open_object(self, path: str) -> _ObjectStream:
        fid = path.split("/")[2]
        content = self.objects.get(fid, b"bench-bytes")
        return _ObjectStream(content)

    def signed_connection(self, host: str, port: int | None, timeout: float) -> _SignedConnection:
        return _SignedConnection(self)
