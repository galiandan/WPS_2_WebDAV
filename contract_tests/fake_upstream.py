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
import socket
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
    """Stands in for an HTTPSConnection PUT/POST against the object store.

    Records every header it is asked to send so the fixture can prove that
    no WPS credentials ever reach the signed host.
    """

    def __init__(self, upstream: "FakeUpstream") -> None:
        self._upstream = upstream
        self._method = "GET"
        self._target = ""
        self._hasher = hashlib.sha256()
        self._size = 0
        self._headers: dict[str, str] = {}

    def putrequest(self, method: str, target: str) -> None:
        self._method = method
        self._target = target
        # Part uploads are verified against the per-part MD5 the client
        # declared in the signed instruction.
        self._hasher = hashlib.md5() if target.startswith("/parts/") else hashlib.sha256()

    def putheader(self, name: str, value: str) -> None:
        self._headers[name.casefold()] = str(value)

    def endheaders(self) -> None:
        self._upstream.note_object_request(self._method, self._headers)

    def send(self, data: bytes) -> None:
        self._hasher.update(data)
        self._size += len(data)

    def getresponse(self) -> _Response:
        if self._method == "POST":
            # Multipart merge: the object store answers with XML.
            body = (
                b'<?xml version="1.0" encoding="UTF-8"?>'
                b'<CompleteMultipartUploadResult><ETag>"bench-merged-etag"</ETag>'
                b"</CompleteMultipartUploadResult>"
            )
            return _Response(body, status=200, content_type="application/xml")
        self._upstream.note_object_upload(self._size, self._hasher.hexdigest())
        if self._target.startswith("/parts/"):
            self._upstream.note_part_upload(self._target, self._hasher.hexdigest(), self._size)
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
        self.children: dict[str, list[dict]] = scenario.get("children", {})
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
        self.stats: dict = {
            "inflight": {},
            "served": {},
            "object_put_size": None,
            "object_put_sha256": None,
            "object_put_headers": [],
            "object_requests": [],
            "part_md5s": [],
            "part_sizes": [],
            "credential_violations": [],
            "request_contract_violations": [],
        }
        self._write_stats()

    # -- fixture contract checks ----------------------------------------------

    def _violation(self, kind: str, detail: str) -> None:
        with self._lock:
            bucket = "credential_violations" if kind == "credential" else "request_contract_violations"
            self.stats[bucket].append(detail)
        self._write_stats()

    @staticmethod
    def _expect_exact(actual: set, expected: set, where: str, what: str) -> list[str]:
        problems = []
        if actual != expected:
            problems.append(f"{where}: {what} mismatch extra={sorted(actual - expected)} missing={sorted(expected - actual)}")
        return problems

    def _check_query(self, where: str, query: dict, required: dict[str, str | None]) -> None:
        """required maps query name -> expected value (None = any non-empty)."""

        problems = self._expect_exact(set(query), set(required), where, "query names")
        for name, expected in required.items():
            if expected is not None and query.get(name) != expected:
                problems.append(f"{where}: query {name}={query.get(name)!r} expected {expected!r}")
        for problem in problems:
            self._violation("contract", problem)

    def _check_json_fields(
        self,
        where: str,
        body: bytes | None,
        expected: dict[str, set[type] | tuple[type, ...]],
    ) -> dict:
        try:
            payload = json.loads(body or b"{}")
        except json.JSONDecodeError:
            self._violation("contract", f"{where}: body is not valid JSON")
            return {}
        if not isinstance(payload, dict):
            self._violation("contract", f"{where}: body is not a JSON object")
            return {}
        problems = self._expect_exact(set(payload), set(expected), where, "JSON fields")
        for name, types in expected.items():
            if name in payload and not isinstance(payload[name], types):
                problems.append(f"{where}: field {name} has wrong type {type(payload[name]).__name__}")
        for problem in problems:
            self._violation("contract", problem)
        return payload

    def note_object_request(self, method: str, headers: dict[str, str]) -> None:
        with self._lock:
            self.stats["object_requests"].append({"method": method, "headers": sorted(headers)})
        self._write_stats()
        forbidden = {"cookie", "authorization", "csrf", "x-csrf-token", "csrfmiddlewaretoken"}
        for name in headers:
            if name in forbidden:
                self._violation("credential", f"object {method} sent {name}")

    def note_part_upload(self, target: str, md5_hex: str, size: int) -> None:
        with self._lock:
            self.stats["part_md5s"].append(md5_hex)
            self.stats["part_sizes"].append(size)
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

    def _served(self, key: str) -> None:
        with self._lock:
            self.stats["served"][key] = self.stats["served"].get(key, 0) + 1
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
        if route.get("short_read"):
            route = dict(route)
            route["headers"] = {**route.get("headers", {}), "Content-Length": "999999"}
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
                if route.get("delay_ms", 0) / 1000.0 > timeout:
                    # The real transport would abandon the read here.
                    raise socket.timeout("fake upstream timed out")
                return self._as_transport_response(self._route_response(route, method, path), url)
            finally:
                self._count(key, -1)

        if host == OBJECT_HOST:
            return self._open_object(request, path)

        if path == "/api/v3/islogin":
            self._served("control:islogin")
            return _Response(json.dumps({"islogin": True, "companyid": "bench-company"}).encode())

        if path == "/passport/secure/api/grant_token":
            # SDK refresh grant: success means the rotated session cookie is
            # persisted by the client from the Set-Cookie header.
            self._served("control:grant_token")
            if not request.headers.get("Cookie"):
                self._violation("contract", "grant_token: request sent no Cookie")
            self._check_json_fields(
                "grant_token", request.data, {"grant_type": str},
            )
            return _Response(
                json.dumps({"result": "ok"}).encode(),
                headers={"Set-Cookie": "bench-session=rotated-bench-cookie; Domain=.kdocs.cn; Path=/"},
            )

        if re.fullmatch(r"/3rd/drive/api/v5/groups/[^/]+/files", path):
            self._served("control:list")
            self._check_query(
                "list",
                query,
                {
                    "parentid": None,
                    "offset": None,
                    "count": None,
                    "orderby": "mtime",
                    "order": "desc",
                    "linkgroup": "true",
                    "include": "acl,pic_thumbnail",
                    "with_link": "true",
                    "review_pic_thumbnail": "true",
                    "with_sharefolder_type": "true",
                },
            )
            parent_id = query.get("parentid", "0")
            offset = int(query.get("offset", "0"))
            count = int(query.get("count", "20"))
            if parent_id in self.children:
                items = self.children[parent_id]
            elif parent_id == "0":
                items = self.listing
            else:
                items = []
            page = items[offset : offset + count]
            payload: dict = {"files": page, "result": "ok"}
            if offset + count < len(items):
                payload["next_offset"] = offset + count
            return _Response(json.dumps(payload).encode())

        if path == "/3rd/drive/api/v5/files/folder" and method == "POST":
            self._served("control:folder")
            self._check_json_fields(
                "folder",
                request.data,
                {
                    "groupid": (int, str),
                    "parentid": (int, str),
                    "name": str,
                    "owner": bool,
                    "parsed": bool,
                    "csrfmiddlewaretoken": str,
                },
            )
            body = json.loads(request.data or b"{}")
            return _Response(
                json.dumps(
                    {
                        "result": "ok",
                        "id": "bench-folder-new",
                        "fname": body.get("name", "bench-new-folder"),
                        "ftype": "folder",
                        "fsize": 0,
                        "mtime": 1788268272,
                        "parentid": body.get("parentid", "0"),
                    }
                ).encode()
            )

        rename_match = re.fullmatch(r"/3rd/drive/api/v3/groups/[^/]+/files/([^/]+)", path)
        if rename_match and method == "PUT":
            self._served("control:rename")
            self._check_json_fields(
                "rename", request.data, {"fname": str, "csrfmiddlewaretoken": str},
            )
            body = json.loads(request.data or b"{}")
            return _Response(
                json.dumps(
                    {
                        "result": "ok",
                        "id": rename_match.group(1),
                        "fname": body.get("fname", "bench-renamed.txt"),
                        "ftype": "file",
                        "fsize": 11,
                        "mtime": 1788268272,
                        "parentid": "0",
                    }
                ).encode()
            )

        if path == "/3rd/drive/api/v5/files/batch/task/progress":
            self._served("control:progress")
            self._check_query("progress", query, {"taskuuid": None})
            return _Response(
                json.dumps({"result": "ok", "finish": 1, "status": "success", "failed_list": []}).encode()
            )

        if path == "/3rd/drive/api/v5/files/batch/task/move" and method == "POST":
            self._served("control:task_move")
            self._check_json_fields(
                "move",
                request.data,
                {
                    "groupid": (int, str),
                    "parentid": (int, str),
                    "dst_groupid": (int, str),
                    "dst_parentid": (int, str),
                    "fileids": list,
                    "option": dict,
                    "csrfmiddlewaretoken": str,
                },
            )
            return _Response(json.dumps({"result": "ok", "taskuuid": "bench-task-move"}).encode())

        if path == "/3rd/drive/api/v5/files/batch/task/delete" and method == "POST":
            self._served("control:task_delete")
            self._check_json_fields(
                "delete",
                request.data,
                {"fileids": list, "groupid": (int, str), "csrfmiddlewaretoken": str},
            )
            return _Response(json.dumps({"result": "ok", "taskuuid": "bench-task-delete"}).encode())

        if path == "/3rd/drive/api/v3/groups/[^/]*/files/batch/copy".replace("[^/]*", "[^/]+") and method == "POST":
            self._served("control:copy")
            self._check_json_fields(
                "copy",
                request.data,
                {
                    "fileids": list,
                    "groupid": (int, str),
                    "target_groupid": (int, str),
                    "target_parentid": (int, str),
                    "duplicated_name_model": int,
                    "csrfmiddlewaretoken": str,
                },
            )
            return _Response(
                json.dumps({"result": "ok", "fileids": ["bench-file-copied"]}).encode()
            )

        match = re.fullmatch(r"/api/v3/office/file/([^/]+)/download", path)
        if match:
            self._served("control:download-url")
            self._check_query("download_url", query, {"support_checksums": "md5,sha1,sha224,sha256,sha384,sha512"})
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
            self._served("control:pre_check")
            self._check_query("pre_check", query, {"file_name": None, "group_id": None, "parent_id": None})
            return _Response(json.dumps({"result": "ok"}).encode())

        if path == "/3rd/drive/api/v5/files/upload/create_update":
            self._served("control:create_update")
            body = self._check_json_fields(
                "create_update",
                request.data,
                {
                    "groupid": (int, str),
                    "parentid": (int, str),
                    "parent_path": list,
                    "size": int,
                    "name": str,
                    "req_by_internal": bool,
                    "client_stores": str,
                    "contenttype": str,
                    "startswithfilename": str,
                    "successactionstatus": int,
                    "group_id": (int, str),
                    "parent_id": (int, str),
                    "file_id": (int, str, type(None)),
                    "with_rapid": (int, str, type(None)),
                    "tried_store": (list, tuple),
                    "sha256": str,
                    "csrfmiddlewaretoken": str,
                },
            )
            if body.get("sha256") and not re.fullmatch(r"[0-9a-f]{64}", str(body.get("sha256"))):
                self._violation("contract", "create_update: sha256 is not a 64-char hex digest")
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
            self._served("control:register")
            body = self._check_json_fields(
                "register",
                request.data,
                {
                    "key": str,
                    "groupid": (int, str),
                    "parentid": (int, str),
                    "name": str,
                    "parent_path": list,
                    "sha1": str,
                    "size": int,
                    "store": str,
                    "etag": str,
                    "isUpNewVer": (bool, int, type(None)),
                    "apiErrorInfo": type(None),
                    "csrfmiddlewaretoken": str,
                },
            )
            if body.get("sha1") and not re.fullmatch(r"[0-9a-f]{40}", str(body.get("sha1"))):
                self._violation("contract", "register: sha1 is not a 40-char hex digest")
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

        # Multipart: POST initializes a session, PUT signs one part.
        if path == "/3rd/drive/api/v5/files/upload/block" and method == "POST":
            self._served("control:multipart_init")
            body = self._check_json_fields(
                "multipart_init",
                request.data,
                {
                    "with_rapid": (int, str, type(None)),
                    "hash": str,
                    "size": int,
                    "group_id": str,
                    "name": str,
                    "parent_id": str,
                    "tried_store": (list, tuple),
                    "csrfmiddlewaretoken": str,
                },
            )
            if body.get("hash") and not re.fullmatch(r"[0-9a-f]{40}", str(body.get("hash"))):
                self._violation("contract", "multipart_init: hash is not a 40-char hex digest")
            return _Response(
                json.dumps(
                    {
                        "result": "ok",
                        "upload_id": "bench-upload-id",
                        "key": "bench-multipart-key",
                        "store": "bench-store",
                        "limit": {"min_part_size": 1, "max_part_size": 64 * 1024 * 1024, "max_parts": 10000},
                    }
                ).encode()
            )

        if path == "/3rd/drive/api/v5/files/upload/block" and method == "PUT":
            self._served("control:multipart_part")
            body = self._check_json_fields(
                "multipart_part",
                request.data,
                {
                    "key": str,
                    "md5": str,
                    "part_number": int,
                    "part_size": int,
                    "req_by_internal": bool,
                    "store": str,
                    "upload_id": str,
                    "csrfmiddlewaretoken": str,
                },
            )
            part_md5 = str(body.get("md5", ""))
            if not re.fullmatch(r"[0-9a-f]{32}", part_md5):
                self._violation("contract", "multipart_part: md5 is not a 32-char hex digest")
            part_number = body.get("part_number", 0)
            content_md5 = base64.b64encode(bytes.fromhex(part_md5)).decode("ascii") if re.fullmatch(
                r"[0-9a-f]{32}", part_md5
            ) else "invalid"
            return _Response(
                json.dumps(
                    {
                        "result": "ok",
                        "url": f"https://{OBJECT_HOST}/parts/{part_number}",
                        "method": "PUT",
                        "request": {
                            "body_type": "file",
                            "headers": {
                                "Content-MD5": content_md5,
                                "Content-Type": "application/octet-stream",
                            },
                        },
                        "response": {"expect_code": [200]},
                    }
                ).encode()
            )

        if path == "/3rd/drive/api/v5/files/upload/block/merge" and method == "POST":
            self._served("control:multipart_merge")
            body = self._check_json_fields(
                "multipart_merge",
                request.data,
                {
                    "key": str,
                    "req_by_internal": bool,
                    "store": str,
                    "part_infos": list,
                    "upload_id": str,
                    "csrfmiddlewaretoken": str,
                },
            )
            for info in body.get("part_infos", []):
                if not isinstance(info, dict) or set(info) != {"etag", "part_number"}:
                    self._violation("contract", "multipart_merge: part_infos entries are malformed")
            return _Response(
                json.dumps(
                    {
                        "result": "ok",
                        "url": f"https://{OBJECT_HOST}/merge-bench",
                        "method": "POST",
                        "request": {
                            "body_type": "data",
                            "body_data": "<CompleteMultipartUpload/>",
                            "headers": {"Content-Type": "application/xml"},
                        },
                        "response": {"expect_code": [200]},
                    }
                ).encode()
            )

        raise AssertionError(f"fake upstream has no route for {method} {url}")

    def _as_transport_response(self, response: _Response, url: str) -> _Response:
        """Real urllib raises HTTPError for >=400; an injected opener must too."""

        if response.status >= 400 or response.status in {301, 302, 303, 307, 308}:
            error = HTTPError(url, response.status, "error", response.headers, BytesIO(response.read()))
            error.headers = response.headers
            raise error
        return response

    def _open_object(self, request, path: str) -> _ObjectStream:
        self._served("object:GET")
        present = {name.casefold(): value for name, value in request.header_items()}
        self.note_object_request("GET", present)
        fid = path.split("/")[2]
        content = self.objects.get(fid, b"bench-bytes")
        return _ObjectStream(content)

    def signed_connection(self, host: str, port: int | None, timeout: float) -> _SignedConnection:
        return _SignedConnection(self)
