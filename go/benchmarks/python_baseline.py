#!/usr/bin/env python3
"""B002 Python performance baseline harness (migration-only, not shipped).

Methodology (per docs/go-rewrite-plan/04-backend-migration-steps.md B002):

- The real Python adapter runs in a child process. Its WPS transport is
  replaced by an in-process fake upstream (the injection points the client
  already provides for tests), so no real WPS account is ever contacted.
- The parent process drives real loopback HTTP against the child and measures
  health/list/PROPFIND/download/upload latency and throughput, upstream
  request counts, RSS, open file descriptors, and cancel-release timing.
- All identifiers are "bench-*" placeholders; no cookie values, no real IDs,
  no signed URLs, and no file contents are recorded in the results.

Usage:
    python go/benchmarks/python_baseline.py --quick   # smoke run
    python go/benchmarks/python_baseline.py           # full baseline

The same methodology must be used for the Go comparison run.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import socket
import statistics
import subprocess
import sys
import tempfile
import threading
import time
from http.client import HTTPConnection, HTTPMessage
from io import BytesIO
from urllib.parse import parse_qsl, quote, urlsplit

PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, os.path.join(PROJECT_ROOT, "src"))

MIB = 1024 * 1024
ROOT_ENTRIES = 200
CHILD_FOLDERS = 20
CHILDREN_PER_FOLDER = 5
CACHE_TTL = 2.0

# fid -> (display name, size). Files the download scenarios use.
SIZED_FILES = {
    "bench-file-8mib": ("bench-file-8mib.bin", 8 * MIB),
    "bench-file-64mib": ("bench-file-64mib.bin", 64 * MIB),
    "bench-file-1mib": ("bench-file-1mib.bin", 1 * MIB),
    "bench-slow-64mib": ("bench-slow-64mib.bin", 64 * MIB),
}
SLOW_FILE_ID = "bench-slow-64mib"
SLOW_CHUNK_DELAY = 0.125  # ~8 MiB/s with 1 MiB chunks

OBJECT_HOST = "benchobj.ag.kdocs.cn"
COOKIE_PLACEHOLDER = "bench-session=placeholder-not-a-secret"
CSRF_PLACEHOLDER = "bench-csrf-placeholder"


def file_size(fid: str) -> int:
    if fid in SIZED_FILES:
        return SIZED_FILES[fid][1]
    return 0


def file_bytes(fid: str, start: int, length: int) -> bytes:
    """Deterministic pseudo-content, generated blockwise (never held in full)."""
    if length <= 0:
        return b""
    out = bytearray()
    index = start // 32
    skip = start - index * 32
    block = hashlib.sha256(f"bench:{fid}:{index}".encode()).digest()
    remaining = length
    if skip:
        out += block[skip:]
        remaining -= len(out)
        index += 1
    while remaining > 0:
        block = hashlib.sha256(f"bench:{fid}:{index}".encode()).digest()
        if remaining < 32:
            out += block[:remaining]
            break
        out += block
        remaining -= 32
        index += 1
    return bytes(out)


def file_sha256(fid: str, size: int) -> str:
    digest = hashlib.sha256()
    for offset in range(0, size, 32 * 65536):
        digest.update(file_bytes(fid, offset, min(32 * 65536, size - offset)))
    return digest.hexdigest()


def entry_json(id: str, name: str, kind: str, parent: str, size: int) -> dict:
    return {
        "id": id,
        "fname": name,
        "ftype": kind,
        "fsize": size,
        "mtime": 1788268272,
        "fsha": f"bench-etag-{id}",
        "parentid": parent,
        "link_id": "",
    }


class FakeObjectResponse:
    """Object-store GET response streamed blockwise from the deterministic generator."""

    def __init__(self, upstream: "FakeUpstream", fid: str, start: int, total: int, slow: bool) -> None:
        self.status = 200 if start == 0 else 206
        self._upstream = upstream
        self._fid = fid
        self._pos = start
        self._total = total
        self._slow = slow
        self._served = 0
        self.closed = False
        self.headers = HTTPMessage()
        self.headers["Content-Type"] = "application/octet-stream"
        self.headers["Content-Length"] = str(total - start)
        if start:
            self.headers["Content-Range"] = f"bytes {start}-{total - 1}/{total}"

    def read(self, size: int = -1) -> bytes:
        if self.closed:
            return b""
        if self._slow:
            time.sleep(SLOW_CHUNK_DELAY)
        length = self._total - self._pos if size in (-1, None) else min(size, self._total - self._pos)
        if length <= 0:
            return b""
        chunk = file_bytes(self._fid, self._pos, length)
        self._pos += len(chunk)
        self._served += len(chunk)
        return chunk

    def close(self) -> None:
        if not self.closed:
            self.closed = True
            self._upstream.note_object_close(self._fid, self._served)


class FakeControlResponse:
    def __init__(self, body: bytes, status: int = 200, headers: dict[str, str] | None = None) -> None:
        self.status = status
        self._body = BytesIO(body)
        self.headers = HTTPMessage()
        self.headers["Content-Type"] = "application/json"
        for key, value in (headers or {}).items():
            self.headers[key] = value

    def read(self, size: int = -1) -> bytes:
        return self._body.read(size)

    def getheaders(self) -> list[tuple[str, str]]:
        return [(name, value) for name, value in self.headers.items()]

    def close(self) -> None:
        self._body.close()


class FakeSignedConnection:
    """Stands in for an HTTPSConnection PUTting one object."""

    def __init__(self, upstream: "FakeUpstream") -> None:
        self._upstream = upstream
        self._hasher = hashlib.sha256()
        self._total = 0
        self.closed = False

    def putrequest(self, method: str, target: str) -> None:
        self._method = method

    def putheader(self, name: str, value: str) -> None:
        pass

    def endheaders(self) -> None:
        pass

    def send(self, data: bytes) -> None:
        self._hasher.update(data)
        self._total += len(data)

    def getresponse(self) -> FakeControlResponse:
        self._upstream.note_object_upload(self._total, self._hasher.hexdigest())
        headers = [("ETag", '"bench-object-etag"'), ("x-obs-save-key", "bench-save-key")]
        return FakeControlResponse(b"", status=200, headers=dict(headers))

    def close(self) -> None:
        self.closed = True


class FakeUpstream:
    """Fake WPS control API + signed object store, in-process, zero network."""

    def __init__(self, stats_path: str) -> None:
        self.stats_path = stats_path
        self._lock = threading.Lock()
        self.stats = {
            "control_requests": {},
            "object_connections_open": 0,
            "object_connections_closed": 0,
            "last_object_close_epoch": None,
            "bytes_served": 0,
            "last_upload_size": None,
            "last_upload_sha256": None,
            "listing_pages": 0,
        }
        self._children: dict[str, list[dict]] = {}

    def flush(self) -> None:
        with self._lock:
            snapshot = json.dumps(self.stats)
        with open(self.stats_path, "w", encoding="utf-8") as handle:
            handle.write(snapshot)

    def _count(self, key: str) -> None:
        with self._lock:
            self.stats["control_requests"][key] = self.stats["control_requests"].get(key, 0) + 1

    def note_object_open(self) -> None:
        with self._lock:
            self.stats["object_connections_open"] += 1
        self.flush()

    def note_object_close(self, fid: str, served: int) -> None:
        with self._lock:
            self.stats["object_connections_closed"] += 1
            self.stats["last_object_close_epoch"] = time.time()
            self.stats["bytes_served"] += served
        self.flush()

    def note_object_upload(self, size: int, sha256: str) -> None:
        with self._lock:
            self.stats["last_upload_size"] = size
            self.stats["last_upload_sha256"] = sha256
        self.flush()

    # -- listings -----------------------------------------------------------

    def _root_listing(self) -> list[dict]:
        items: list[dict] = []
        for index in range(CHILD_FOLDERS):
            items.append(
                entry_json(f"bench-dir-{index}", f"bench-folder-{index}", "folder", "0", 0)
            )
        for fid, (name, size) in SIZED_FILES.items():
            items.append(entry_json(fid, name, "file", "0", size))
        for index in range(CHILD_FOLDERS, ROOT_ENTRIES):
            items.append(
                entry_json(f"bench-file-{index}", f"bench-file-{index}.bin", "file", "0", 4096)
            )
        return items

    def _listing_for(self, parent_id: str) -> list[dict]:
        if parent_id == "0":
            return self._root_listing()
        if parent_id.startswith("bench-dir-"):
            with self._lock:
                cached = self._children.get(parent_id)
            if cached is not None:
                return cached
            index = int(parent_id.rsplit("-", 1)[1])
            items = [
                entry_json(
                    f"{parent_id}-child-{n}", f"child-{n}.txt", "file", parent_id, 1024 + n
                )
                for n in range(CHILDREN_PER_FOLDER)
            ]
            with self._lock:
                self._children[parent_id] = items
            return items
        return []

    # -- opener interface ---------------------------------------------------

    def open(self, request, timeout: float):  # noqa: ANN001 - matches urllib signature
        url = request.get_full_url()
        parts = urlsplit(url)
        path = parts.path
        query = dict(parse_qsl(parts.query))
        method = request.get_method()
        host = (parts.hostname or "").casefold()

        if host == OBJECT_HOST:
            return self._open_object(request, path, query)

        if path == "/api/v3/islogin":
            self._count("control:islogin")
            return FakeControlResponse(json.dumps({"islogin": True, "companyid": "bench-company"}).encode())

        if re.fullmatch(r"/3rd/drive/api/v5/groups/[^/]+/files", path):
            self._count("control:list")
            with self._lock:
                self.stats["listing_pages"] += 1
            parent_id = query.get("parentid", "0")
            offset = int(query.get("offset", "0"))
            count = int(query.get("count", "20"))
            items = self._listing_for(parent_id)
            page = items[offset : offset + count]
            next_offset = offset + count if offset + count < len(items) else None
            payload = {"files": page, "result": "ok"}
            if next_offset is not None:
                payload["next_offset"] = next_offset
            return FakeControlResponse(json.dumps(payload).encode())

        match = re.fullmatch(r"/api/v3/office/file/([^/]+)/download", path)
        if match:
            self._count("control:download-url")
            fid = match.group(1)
            return FakeControlResponse(
                json.dumps(
                    {
                        "download_url": f"https://{OBJECT_HOST}/objects/{quote(fid, safe='')}?sig=bench",
                        "status": "finished",
                    }
                ).encode()
            )

        if path == "/3rd/drive/api/v5/files/upload/pre_check":
            self._count("control:pre_check")
            return FakeControlResponse(json.dumps({"result": "ok"}).encode())

        if path == "/3rd/drive/api/v5/files/upload/create_update":
            self._count("control:create_update")
            return FakeControlResponse(
                json.dumps(
                    {
                        "url": f"https://{OBJECT_HOST}/upload-bench?sig=bench",
                        "response": {"expect_code": [200]},
                        "store": "bench-store",
                    }
                ).encode()
            )

        if path == "/3rd/drive/api/v5/files/file" and method == "POST":
            self._count("control:register")
            body = json.loads(request.data or b"{}")
            return FakeControlResponse(
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

    def _open_object(self, request, path: str, query: dict) -> FakeObjectResponse:
        self._count("object:GET")
        fid = path.split("/")[2]
        total = file_size(fid)
        start = 0
        range_header = request.headers.get("Range")
        if range_header:
            match = re.fullmatch(r"bytes=(\d+)-", range_header.strip())
            if match:
                start = int(match.group(1))
        self.note_object_open()
        return FakeObjectResponse(self, fid, start, total, slow=fid == SLOW_FILE_ID)

    def signed_connection(self, host: str, port: int | None, timeout: float) -> FakeSignedConnection:
        self._count("object:PUT")
        return FakeSignedConnection(self)


def serve(port: int, stats_path: str, spool_dir: str) -> None:
    from wps_adapter.client import WpsClientConfig, WpsDriveClient
    from wps_adapter.server import AdapterApplication, BasicAuth, DavLockStore, create_server
    from wps_adapter.settings import WebSettings
    from wps_adapter.storage import MultiSpaceStorage

    upstream = FakeUpstream(stats_path)
    config = WpsClientConfig(
        group_id="bench-group",
        cookie=COOKIE_PLACEHOLDER,
        csrf_token=CSRF_PLACEHOLDER,
        timeout=30.0,
        upload_min_free_bytes=0,
        upload_spool_dir=spool_dir,
    )
    client = WpsDriveClient(config, opener=upstream, https_connection_factory=upstream.signed_connection)
    storage = MultiSpaceStorage(
        client,
        (),
        root_id="0",
        root_name="Bench Drive",
        list_count=20,
        max_list_entries=10000,
        cache_ttl=CACHE_TTL,
        max_cached_folders=1024,
        max_uploads=2,
        max_downloads=4,
        transfer_wait_timeout=30.0,
        max_copy_entries=10000,
        max_copy_depth=64,
    )
    application = AdapterApplication(
        storage,
        auth=BasicAuth(username="", password=""),
        web_root_name="Bench Drive",
        web_settings=WebSettings(fallback_name="Bench Drive", file_path=None),
        locks=DavLockStore(max_locks=4096),
    )
    server = create_server(
        application,
        bind="127.0.0.1",
        port=port,
        max_connections=64,
        request_timeout=60.0,
    )
    upstream.flush()
    print(f"ready port={server.server_address[1]}", flush=True)
    server.serve_forever()


# --------------------------------------------------------------------------
# driver
# --------------------------------------------------------------------------

def percentiles(values: list[float]) -> dict:
    ordered = sorted(values)
    def pct(fraction: float) -> float:
        if not ordered:
            return 0.0
        index = min(len(ordered) - 1, max(0, round(fraction * (len(ordered) - 1))))
        return round(ordered[index], 3)
    return {
        "n": len(ordered),
        "p50_ms": pct(0.50),
        "p95_ms": pct(0.95),
        "p99_ms": pct(0.99),
        "mean_ms": round(statistics.fmean(ordered), 3) if ordered else 0.0,
        "max_ms": round(ordered[-1], 3) if ordered else 0.0,
    }


def read_proc(pid: int) -> dict:
    info: dict = {}
    try:
        with open(f"/proc/{pid}/status", encoding="utf-8") as handle:
            for line in handle:
                if line.startswith(("VmRSS:", "VmHWM:")):
                    key, value, _unit = line.split()
                    info[key.rstrip(":")] = int(value)
        info["fds"] = len(os.listdir(f"/proc/{pid}/fd"))
    except (FileNotFoundError, ProcessLookupError):
        pass
    return info


class Driver:
    def __init__(self, port: int) -> None:
        self.conn = HTTPConnection("127.0.0.1", port, timeout=60)

    def request(self, method: str, target: str, body: bytes | None = None, headers: dict | None = None):
        self.conn.request(method, target, body=body, headers=headers or {})
        response = self.conn.getresponse()
        return response

    def close(self) -> None:
        try:
            self.conn.close()
        except OSError:
            pass


def read_stats(stats_path: str) -> dict:
    for _ in range(100):
        try:
            with open(stats_path, encoding="utf-8") as handle:
                return json.load(handle)
        except (FileNotFoundError, json.JSONDecodeError):
            time.sleep(0.02)
    raise RuntimeError(f"stats file {stats_path} unreadable")


def wait_for_object_close(stats_path: str, closed_before: int, deadline: float = 10.0) -> tuple[bool, float]:
    """Wait until the object-close counter passes closed_before.

    Returns (released, elapsed_seconds_since_call)."""

    start = time.time()
    end = start + deadline
    while time.time() < end:
        stats = read_stats(stats_path)
        if stats.get("object_connections_closed", 0) > closed_before:
            return True, time.time() - start
        time.sleep(0.005)
    return False, time.time() - start


def run_scenarios(stats_path: str, port: int, quick: bool) -> dict:
    driver = Driver(port)
    results: dict = {}
    reps_health = 100 if quick else 300
    reps_cold = 2 if quick else 5
    reps_warm = 10 if quick else 20

    # -- healthz ------------------------------------------------------------
    samples = []
    for _ in range(reps_health):
        start = time.perf_counter()
        response = driver.request("GET", "/healthz")
        response.read()
        assert response.status == 200, response.status
        samples.append((time.perf_counter() - start) * 1000)
    results["healthz_keepalive"] = percentiles(samples)

    # Fresh connection per request: separates server processing from the
    # keep-alive Nagle/delayed-ACK interaction (see migration log B002).
    samples = []
    for _ in range(reps_health):
        start = time.perf_counter()
        conn = HTTPConnection("127.0.0.1", port, timeout=30)
        conn.request("GET", "/healthz")
        response = conn.getresponse()
        response.read()
        conn.close()
        assert response.status == 200
        samples.append((time.perf_counter() - start) * 1000)
    results["healthz_fresh_connection"] = percentiles(samples)

    # -- status (upstream preflight + cached) --------------------------------
    start = time.perf_counter()
    response = driver.request("GET", "/api/v1/status")
    body = response.read()
    cold_ms = (time.perf_counter() - start) * 1000
    assert response.status == 200, (response.status, body[:200])
    samples = []
    for _ in range(reps_warm):
        start = time.perf_counter()
        response = driver.request("GET", "/api/v1/status")
        response.read()
        assert response.status == 200
        samples.append((time.perf_counter() - start) * 1000)
    results["status_cold_ms"] = round(cold_ms, 3)
    results["status_cached"] = percentiles(samples)

    # -- REST list, cold vs warm ---------------------------------------------
    cold = []
    for _ in range(reps_cold):
        time.sleep(CACHE_TTL + 0.15)
        start = time.perf_counter()
        response = driver.request("GET", "/api/v1/entries?path=%2F")
        body = response.read()
        cold.append((time.perf_counter() - start) * 1000)
        assert response.status == 200, (response.status, body[:200])
        assert len(json.loads(body)["entries"]) == (
            CHILD_FOLDERS + len(SIZED_FILES) + (ROOT_ENTRIES - CHILD_FOLDERS)
        )
    results["rest_list_cold"] = percentiles(cold)
    warm = []
    for _ in range(reps_warm):
        start = time.perf_counter()
        response = driver.request("GET", "/api/v1/entries?path=%2F")
        response.read()
        warm.append((time.perf_counter() - start) * 1000)
    results["rest_list_warm"] = percentiles(warm)

    # -- PROPFIND depth 1 (warm cache) ---------------------------------------
    samples = []
    for _ in range(3 if quick else 10):
        start = time.perf_counter()
        response = driver.request("PROPFIND", "/dav/", headers={"Depth": "1"})
        body = response.read()
        samples.append((time.perf_counter() - start) * 1000)
        assert response.status == 207, response.status
        results["propfind_depth1_body_bytes"] = len(body)
    results["propfind_depth1"] = percentiles(samples)

    stats_before_downloads = read_stats(stats_path)

    # -- downloads -------------------------------------------------------------
    for scenario, fid in (("download_8mib", "bench-file-8mib"), ("download_64mib", "bench-file-64mib")):
        size = file_size(fid)
        start = time.perf_counter()
        response = driver.request("GET", f"/dav/{SIZED_FILES[fid][0]}")
        digest = hashlib.sha256()
        total = 0
        while True:
            chunk = response.read(256 * 1024)
            if not chunk:
                break
            digest.update(chunk)
            total += len(chunk)
        elapsed = time.perf_counter() - start
        assert response.status == 200, response.status
        assert total == size, (total, size)
        assert digest.hexdigest() == file_sha256(fid, size), "download content mismatch"
        results[scenario] = {
            "mib_s": round(size / MIB / elapsed, 2),
            "seconds": round(elapsed, 3),
        }

    # -- upload (spool + hashes + signed PUT + register) ----------------------
    for scenario, size in (("upload_1mib", 1 * MIB), ("upload_8mib", 8 * MIB)):
        name = f"bench-upload-{size}.bin"
        payload = file_bytes(f"upload-{size}", 0, size)
        expected = hashlib.sha256(payload).hexdigest()
        start = time.perf_counter()
        response = driver.request(
            "PUT",
            f"/api/v1/files?path=%2F{name}",
            body=BytesIO(payload),
            headers={"Content-Length": str(size), "Content-Type": "application/octet-stream"},
        )
        body = response.read()
        elapsed = time.perf_counter() - start
        assert response.status == 201, (response.status, body[:200])
        stats = read_stats(stats_path)
        assert stats["last_upload_sha256"] == expected, "upload content mismatch"
        assert stats["last_upload_size"] == size
        results[scenario] = {"mib_s": round(size / MIB / elapsed, 2), "seconds": round(elapsed, 3)}

    # -- cancel variants -------------------------------------------------------
    # The adapter checks for client disconnects between stream chunks; a
    # blocked socket write can only fail via RST or the socket timeout. Both
    # disconnect styles are measured because they exercise different paths.
    cancel: dict = {}

    # Variant A: RST disconnect (SO_LINGER 0) — write fails immediately.
    # Uses a raw socket because http.client takes ownership of the socket
    # when the server declares Connection: close.
    stats_now = read_stats(stats_path)
    closed_before = stats_now["object_connections_closed"]
    raw = socket.create_connection(("127.0.0.1", port), timeout=30)
    raw.sendall(b"GET /dav/bench-slow-64mib.bin HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
    read_total = 0
    while read_total < 2 * MIB:
        chunk = raw.recv(256 * 1024)
        if not chunk:
            break
        read_total += len(chunk)
    import struct as _struct

    raw.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, _struct.pack("ii", 1, 0))
    raw.close()
    released, elapsed = wait_for_object_close(stats_path, closed_before, deadline=10.0)
    cancel["rst_disconnect"] = {
        "bytes_read": read_total,
        "released": released,
        "release_ms": round(elapsed * 1000, 1),
    }
    assert read_total >= 2 * MIB
    assert released, "upstream object stream was not closed after RST disconnect"

    # -- post-cancel slot availability ----------------------------------------
    start = time.perf_counter()
    response = driver.request("GET", "/dav/bench-file-1mib.bin")
    body = response.read()
    cancel["post_rst_1mib_download_ms"] = round((time.perf_counter() - start) * 1000, 3)
    assert response.status == 200 and len(body) == file_size("bench-file-1mib")

    # Variant B: clean FIN disconnect with empty client receive buffer.
    # Measured once with the production default request timeout (60 s).
    stats_now = read_stats(stats_path)
    closed_before = stats_now["object_connections_closed"]
    conn = HTTPConnection("127.0.0.1", port, timeout=30)
    conn.request("GET", "/dav/bench-slow-64mib.bin")
    response = conn.getresponse()
    read_total = 0
    while read_total < 2 * MIB:
        chunk = response.read(256 * 1024)
        if not chunk:
            break
        read_total += len(chunk)
    time.sleep(0.5)  # let the server's sends drain so close() is a clean FIN
    fin_at = time.time()
    conn.close()
    released, elapsed = wait_for_object_close(stats_path, closed_before, deadline=90.0)
    cancel["fin_disconnect"] = {
        "bytes_read": read_total,
        "released": released,
        "release_seconds": round(elapsed, 1),
    }
    results["cancel_download"] = cancel

    results["upstream_requests"] = read_stats(stats_path)["control_requests"]
    results["upstream_listing_pages"] = read_stats(stats_path)["listing_pages"]
    results["object_connections"] = {
        key: read_stats(stats_path)[key]
        for key in ("object_connections_open", "object_connections_closed")
    }
    driver.close()
    conn.close()
    return results


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--quick", action="store_true", help="reduced repetitions")
    parser.add_argument("--serve", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--port", type=int, default=0)
    parser.add_argument("--stats", default="")
    args = parser.parse_args()

    if args.serve:
        with tempfile.TemporaryDirectory(prefix="wps-bench-spool-") as spool_dir:
            serve(args.port, args.stats, spool_dir)
        return 0

    port = free_port()
    stats_path = os.path.join(tempfile.mkdtemp(prefix="wps-bench-"), "stats.json")
    env = dict(os.environ)
    env["PYTHONPATH"] = os.path.join(PROJECT_ROOT, "src")
    child = subprocess.Popen(
        [sys.executable, os.path.abspath(__file__), "--serve", "--port", str(port), "--stats", stats_path],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        ready = child.stdout.readline().strip()
        if not ready.startswith("ready port="):
            child.kill()
            stderr = child.stderr.read()
            print(f"child failed to start: {ready!r}\n{stderr}", file=sys.stderr)
            return 1
        child_port = int(ready.split("port=")[1])

        rss_idle = read_proc(child.pid)
        start = time.time()
        results = run_scenarios(stats_path, child_port, args.quick)
        wall = time.time() - start
        rss_final = read_proc(child.pid)

        cpu = f"{os.cpu_count()}x {platform.processor() or 'unknown'}"
        try:
            with open("/proc/meminfo", encoding="utf-8") as handle:
                meminfo = handle.readline().strip()
        except OSError:
            meminfo = "unknown"

        report = {
            "task": "B002",
            "date": time.strftime("%Y-%m-%d"),
            "python": sys.version.split()[0],
            "platform": f"{platform.system()} {platform.release()} {platform.machine()}",
            "cpu": cpu,
            "meminfo_line": meminfo,
            "notes": [
                "child process = real adapter + in-process fake upstream (no real WPS)",
                "WPS_UPLOAD_MIN_FREE_BYTES=0 to avoid disk-free dependence",
                "basic auth disabled (loopback-only benchmark)",
                "keepalive scenarios use default client sockets; the Python server "
                "does not set TCP_NODELAY on accepted sockets, so small keep-alive "
                "responses include a ~40ms Nagle/delayed-ACK stall (see B002 record)",
            ],
            "wall_seconds": round(wall, 1),
            "rss_idle_kib": rss_idle.get("VmRSS"),
            "rss_final_kib": rss_final.get("VmRSS"),
            "rss_peak_kib": rss_final.get("VmHWM"),
            "fds_final": rss_final.get("fds"),
            "scenarios": results,
        }
        print(json.dumps(report, indent=2))
        return 0
    finally:
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:
                child.kill()
        child.stdout.close()
        child.stderr.close()


if __name__ == "__main__":
    raise SystemExit(main())
