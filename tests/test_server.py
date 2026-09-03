from __future__ import annotations

import base64
import json
import socket
import threading
import unittest
from http.client import HTTPConnection
from io import BytesIO
from types import SimpleNamespace

from wps_adapter.client import WpsCredentials
from wps_adapter.provider import (
    EntryNotFoundError,
    InvalidPathError,
    RemoteEntry,
    ServiceBusyError,
    UnsupportedOperationError,
)
from wps_adapter.server import AdapterApplication, AdapterHTTPServer, BasicAuth, DavLockStore
from wps_adapter.storage import split_remote_path


class FakeStream:
    def __init__(self, body: bytes = b"hello world") -> None:
        self.content_length = len(body)
        self.body = BytesIO(body)
        self.closed = False

    def read(self, size: int = -1) -> bytes:
        return self.body.read(size)

    def close(self) -> None:
        self.closed = True


class FakeStorage:
    def __init__(self) -> None:
        self.client = SimpleNamespace(config=SimpleNamespace(stream_chunk_size=3))
        self.root = RemoteEntry(id="root", name="Drive", kind="folder", size=0)
        self.file = RemoteEntry(
            id="file-1",
            name="hello.txt",
            kind="file",
            parent_id="root",
            size=11,
            modified_at="1788268272",
            etag="abc123",
        )
        self.folder = RemoteEntry(id="folder-1", name="docs", kind="folder", parent_id="root", size=0)
        self.uploaded: bytes | None = None
        self.upload_overwrites: list[bool] = []
        self.created_folders: list[str] = []
        self.deleted_paths: list[str] = []
        self.renamed_paths: list[tuple[str, str]] = []
        self.moved_paths: list[tuple[str, str]] = []
        self.copied_paths: list[tuple[str, str, str, bool]] = []

    def metadata(self, path: str) -> RemoteEntry:
        if path == "/":
            return self.root
        if path == "/hello.txt":
            return self.file
        if path == "/docs":
            return self.folder
        if path == "/existing.txt":
            return RemoteEntry(
                id="existing-file",
                name="existing.txt",
                kind="file",
                parent_id="root",
                size=7,
            )
        raise EntryNotFoundError(path)

    def list_path(self, path: str) -> tuple[RemoteEntry, ...]:
        if path == "/":
            return (self.file, self.folder)
        if path == "/docs":
            return ()
        raise EntryNotFoundError(path)

    def open_path(self, path: str, *, offset: int = 0, length: int | None = None) -> FakeStream:
        self.metadata(path)
        return FakeStream()

    def upload_path(self, path: str, source, *, size=None, content_type=None, csrf_token=None, overwrite=False) -> RemoteEntry:
        self.uploaded = source.read()
        self.upload_overwrites.append(overwrite)
        return RemoteEntry(id="file-2", name=path.rsplit("/", 1)[-1], kind="file", size=len(self.uploaded))

    def create_folder_path(self, path: str) -> RemoteEntry:
        self.created_folders.append(path)
        return RemoteEntry(id="folder-2", name=path.rsplit("/", 1)[-1], kind="folder", parent_id="root", size=0)

    def delete_path(self, path: str) -> None:
        self.deleted_paths.append(path)

    def rename_path(self, path: str, name: str) -> RemoteEntry:
        self.renamed_paths.append((path, name))
        return RemoteEntry(id="file-1", name=name, kind="file", parent_id="root", size=11)

    def move_path(self, path: str, destination: str) -> RemoteEntry:
        self.moved_paths.append((path, destination))
        name = destination.rstrip("/").rsplit("/", 1)[-1]
        return RemoteEntry(id="file-1", name=name, kind="file", parent_id="root", size=11)

    def copy_path(self, source: str, destination: str, *, depth="infinity", overwrite=True) -> RemoteEntry:
        self.copied_paths.append((source, destination, depth, overwrite))
        name = destination.rstrip("/").rsplit("/", 1)[-1]
        return RemoteEntry(id="copy-1", name=name, kind="file", parent_id="root", size=11)


class ImportCredentialSource:
    def __init__(self) -> None:
        self.credentials = WpsCredentials()

    def replace_credentials(self, credentials: WpsCredentials) -> bool:
        self.credentials = credentials
        return True


class ServerTests(unittest.TestCase):
    def setUp(self) -> None:
        self.storage = FakeStorage()
        self.server = AdapterHTTPServer(("127.0.0.1", 0), AdapterApplication(self.storage))
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.connection = HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)

    def tearDown(self) -> None:
        self.connection.close()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)

    def request(self, method: str, path: str, body: bytes | None = None, headers=None):
        self.connection.request(method, path, body=body, headers=headers or {})
        response = self.connection.getresponse()
        return response.status, dict(response.getheaders()), response.read()

    def test_health_does_not_touch_storage(self) -> None:
        status, headers, body = self.request("GET", "/healthz")
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Type"], "application/json; charset=utf-8")
        self.assertEqual(json.loads(body)["status"], "ok")

    def test_unauthorised_requests_close_the_connection(self) -> None:
        auth_server = AdapterHTTPServer(
            ("127.0.0.1", 0),
            AdapterApplication(self.storage, auth=BasicAuth(username="u", password="p")),
        )
        thread = threading.Thread(target=auth_server.serve_forever, daemon=True)
        thread.start()
        connection = HTTPConnection("127.0.0.1", auth_server.server_port, timeout=3)
        try:
            connection.request(
                "POST",
                "/api/v1/session/import",
                body=b"x" * 32,
                headers={"Content-Length": "32"},
            )
            response = connection.getresponse()
            self.assertEqual(response.status, 401)
            self.assertEqual(response.getheader("Connection"), "close")
            response.read()
        finally:
            connection.close()
            auth_server.shutdown()
            auth_server.server_close()
            thread.join(timeout=3)

    def test_control_body_limit_is_enforced(self) -> None:
        self.server.application.max_control_body = 8
        status, _headers, body = self.request(
            "PATCH",
            "/api/v1/entries?path=%2Fhello.txt",
            body=b'{"name":"too-long"}',
            headers={"Content-Length": "20", "Content-Type": "application/json"},
        )
        self.assertEqual(status, 413)
        self.assertIn(b"too large", body)

    def test_upload_size_limit_is_checked_before_reading_body(self) -> None:
        self.storage.client.config.max_upload_bytes = 4
        status, headers, body = self.request(
            "PUT",
            "/dav/new.txt",
            body=b"this body is not consumed",
            headers={"Content-Length": "25", "Content-Type": "text/plain"},
        )
        self.assertEqual(status, 507)
        self.assertEqual(headers["Connection"], "close")
        self.assertIn(b"size limit", body)
        self.assertIsNone(self.storage.uploaded)

    def test_invalid_request_framing_closes_the_connection(self) -> None:
        raw = socket.create_connection(("127.0.0.1", self.server.server_port), timeout=3)
        raw.settimeout(3)
        try:
            raw.sendall(
                b"PROPFIND /dav/ HTTP/1.1\r\n"
                b"Host: localhost\r\n"
                b"Transfer-Encoding: chunked\r\n"
                b"\r\n"
            )
            response = bytearray()
            while True:
                chunk = raw.recv(4096)
                if not chunk:
                    break
                response.extend(chunk)
            self.assertIn(b"400", response)
            self.assertIn(b"Connection: close", response)
        finally:
            raw.close()

    def test_move_and_copy_do_not_delete_an_existing_destination(self) -> None:
        for method in ("MOVE", "COPY"):
            status, _headers, body = self.request(
                method,
                "/dav/hello.txt",
                headers={"Destination": "/dav/existing.txt", "Overwrite": "T"},
            )
            self.assertEqual(status, 501)
            self.assertIn(b"not atomic", body)
        self.assertEqual(self.storage.deleted_paths, [])
        self.assertEqual(self.storage.moved_paths, [])
        self.assertEqual(self.storage.copied_paths, [])

    def test_destination_must_point_to_this_adapter(self) -> None:
        status, _headers, body = self.request(
            "MOVE",
            "/dav/hello.txt",
            headers={"Destination": "http://other.example/dav/renamed.txt"},
        )
        self.assertEqual(status, 400)
        self.assertIn(b"this adapter", body)

    def test_cross_origin_mutations_are_rejected(self) -> None:
        status, _headers, body = self.request(
            "PATCH",
            "/api/v1/entries?path=%2Fhello.txt",
            body=b'{"name":"renamed.txt"}',
            headers={
                "Content-Length": "22",
                "Content-Type": "application/json",
                "Origin": "https://attacker.example",
            },
        )
        self.assertEqual(status, 403)
        self.assertIn(b"cross-origin", body)
        self.assertEqual(self.storage.renamed_paths, [])

    def test_web_file_manager_is_served_without_storage_access(self) -> None:
        status, headers, body = self.request("GET", "/")
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Type"], "text/html; charset=utf-8")
        self.assertIn("Content-Security-Policy", headers)
        self.assertIn("WPS 企业云盘".encode("utf-8"), body)
        self.assertIn(b'const apiRoot = "/api/v1/";', body)
        self.assertIn(b"drop-overlay", body)
        self.assertIn(b"window.addEventListener(\"drop\"", body)
        self.assertIn(b"upload-speed", body)
        self.assertIn(b"formatRate", body)

    def test_generated_response_size_is_bounded(self) -> None:
        self.server.application.max_response_body = 64
        status, _headers, body = self.request("PROPFIND", "/dav/", headers={"Depth": "1"})
        self.assertEqual(status, 507)
        self.assertIn(b"response exceeds", body)

    def test_propfind_and_streaming_get(self) -> None:
        status, headers, body = self.request("PROPFIND", "/dav/", headers={"Depth": "1"})
        self.assertEqual(status, 207)
        self.assertEqual(headers["DAV"], "1,2")
        self.assertIn(b"hello.txt", body)
        status, headers, body = self.request("GET", "/dav/hello.txt")
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Length"], "11")
        self.assertEqual(body, b"hello world")

    def test_put_is_streamed_and_mkcol_creates_folder(self) -> None:
        status, _headers, body = self.request(
            "PUT",
            "/dav/new.txt",
            body=b"new content",
            headers={"Content-Length": "11", "Content-Type": "text/plain"},
        )
        self.assertEqual(status, 201)
        self.assertEqual(self.storage.uploaded, b"new content")
        self.assertTrue(self.storage.upload_overwrites[-1])
        self.assertEqual(json.loads(body)["name"], "new.txt")
        status, _headers, body = self.request("MKCOL", "/dav/new-folder/")
        self.assertEqual(status, 201)
        self.assertEqual(json.loads(body)["kind"], "folder")
        self.assertEqual(self.storage.created_folders, ["/new-folder/"])
        status, headers, body = self.request(
            "MOVE",
            "/dav/hello.txt",
            headers={"Destination": "/dav/renamed.txt"},
        )
        self.assertEqual(status, 201)
        self.assertEqual(headers["Location"], "/dav/renamed.txt")
        self.assertEqual(json.loads(body)["name"], "renamed.txt")
        self.assertEqual(self.storage.moved_paths, [("/hello.txt", "/renamed.txt")])
        status, headers, body = self.request(
            "MOVE",
            "/dav/renamed.txt",
            headers={"Destination": "/dav/docs/renamed.txt"},
        )
        self.assertEqual(status, 201)
        self.assertEqual(headers["Location"], "/dav/docs/renamed.txt")
        self.assertEqual(json.loads(body)["name"], "renamed.txt")
        self.assertEqual(self.storage.moved_paths[-1], ("/renamed.txt", "/docs/renamed.txt"))
        status, _headers, body = self.request("DELETE", "/dav/new-folder/")
        self.assertEqual(status, 204)
        self.assertEqual(body, b"")
        self.assertEqual(self.storage.deleted_paths, ["/new-folder/"])

    def test_copy_and_depth_infinity(self) -> None:
        status, headers, body = self.request(
            "COPY",
            "/dav/hello.txt",
            headers={"Destination": "/dav/copied.txt", "Depth": "0"},
        )
        self.assertEqual(status, 201)
        self.assertEqual(headers["Location"], "/dav/copied.txt")
        self.assertEqual(json.loads(body)["name"], "copied.txt")
        self.assertEqual(
            self.storage.copied_paths,
            [("/hello.txt", "/copied.txt", "0", True)],
        )

        status, _headers, body = self.request(
            "PROPFIND",
            "/dav/",
            headers={"Depth": "infinity"},
        )
        self.assertEqual(status, 207)
        self.assertIn(b"hello.txt", body)
        self.assertIn(b"docs", body)

    def test_lock_blocks_writes_until_token_is_supplied(self) -> None:
        lock_info = (
            b'<?xml version="1.0"?>'
            b'<D:lockinfo xmlns:D="DAV:">'
            b"<D:lockscope><D:exclusive/></D:lockscope>"
            b"<D:locktype><D:write/></D:locktype>"
            b"<D:owner><D:href>test-client</D:href></D:owner>"
            b"</D:lockinfo>"
        )
        status, headers, body = self.request(
            "LOCK",
            "/dav/hello.txt",
            body=lock_info,
            headers={"Content-Length": str(len(lock_info)), "Depth": "0"},
        )
        self.assertEqual(status, 200)
        self.assertIn(b"lockdiscovery", body)
        lock_token = headers["Lock-Token"]

        status, _headers, _body = self.request(
            "PUT",
            "/dav/hello.txt",
            body=b"blocked",
            headers={"Content-Length": "7"},
        )
        self.assertEqual(status, 423)

        status, _headers, _body = self.request(
            "PUT",
            "/dav/hello.txt",
            body=b"allowed",
            headers={"Content-Length": "7", "If": f"({lock_token})"},
        )
        self.assertEqual(status, 201)

        status, _headers, body = self.request(
            "UNLOCK",
            "/dav/hello.txt",
            headers={"Lock-Token": lock_token},
        )
        self.assertEqual(status, 204)
        self.assertEqual(body, b"")

    def test_lock_store_bounds_active_lock_count(self) -> None:
        store = DavLockStore(max_locks=1)
        store.acquire("/one", depth="0", owner="test", timeout_seconds=60)
        with self.assertRaisesRegex(ServiceBusyError, "too many active"):
            store.acquire("/two", depth="0", owner="test", timeout_seconds=60)

    def test_rest_mutations_check_destination_locks(self) -> None:
        self.server.application.locks.acquire(
            "/renamed.txt",
            depth="0",
            owner="test",
            timeout_seconds=60,
        )
        status, _headers, body = self.request(
            "PATCH",
            "/api/v1/entries?path=%2Fhello.txt",
            body=b'{"name":"renamed.txt"}',
            headers={"Content-Length": "22", "Content-Type": "application/json"},
        )
        self.assertEqual(status, 423)
        self.assertIn(b"locked", body)

    def test_lock_rejects_xml_entity_declarations(self) -> None:
        body = (
            b'<?xml version="1.0"?>'
            b'<!DOCTYPE lockinfo [<!ENTITY x "expanded">]>'
            b'<D:lockinfo xmlns:D="DAV:"></D:lockinfo>'
        )
        status, _headers, response_body = self.request(
            "LOCK",
            "/dav/hello.txt",
            body=body,
            headers={"Content-Length": str(len(body))},
        )
        self.assertEqual(status, 400)
        self.assertIn(b"XML entities", response_body)

    def test_single_byte_range_is_forwarded(self) -> None:
        class RangeStorage(FakeStorage):
            def open_path(self, path: str, *, offset: int = 0, length: int | None = None) -> FakeStream:
                self.metadata(path)
                data = b"hello world"[offset:]
                if length is not None:
                    data = data[:length]
                return FakeStream(data)

        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)
        self.connection.close()
        self.storage = RangeStorage()
        self.server = AdapterHTTPServer(("127.0.0.1", 0), AdapterApplication(self.storage))
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.connection = HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)

        status, headers, body = self.request(
            "GET",
            "/dav/hello.txt",
            headers={"Range": "bytes=6-10"},
        )
        self.assertEqual(status, 206)
        self.assertEqual(headers["Content-Range"], "bytes 6-10/11")
        self.assertEqual(headers["Content-Length"], "5")
        self.assertEqual(body, b"world")

    def test_session_import_uses_basic_auth_and_replaces_credentials(self) -> None:
        source = ImportCredentialSource()
        self.storage.client.config.credential_source = source
        self.storage.client.config.base_url = "https://365.kdocs.cn"
        self.server.application.auth = BasicAuth(username="adapter", password="secret")
        payload = json.dumps(
            {
                "cookies": [
                    {"name": "rtk", "value": "refresh", "domain": ".kdocs.cn", "path": "/passport/secure"},
                    {"name": "csrf", "value": "csrf", "domain": "365.kdocs.cn", "path": "/"},
                ]
            }
        ).encode("utf-8")

        status, _headers, _body = self.request(
            "POST",
            "/api/v1/session/import",
        )
        self.assertEqual(status, 401)

        authorization = "Basic " + base64.b64encode(b"adapter:secret").decode("ascii")
        status, headers, body = self.request(
            "POST",
            "/api/v1/session/import",
            body=payload,
            headers={
                "Authorization": authorization,
                "Content-Length": str(len(payload)),
                "Content-Type": "application/json",
            },
        )
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Type"], "application/json; charset=utf-8")
        self.assertEqual(json.loads(body)["cookie_count"], 2)
        self.assertIn("rtk=refresh", source.credentials.cookie)
        self.assertEqual(source.credentials.csrf_token, "csrf")

    def test_rest_list_and_basic_auth(self) -> None:
        status, _headers, body = self.request("GET", "/api/v1/entries?path=%2F")
        self.assertEqual(status, 200)
        self.assertEqual(len(json.loads(body)["entries"]), 2)

        status, _headers, body = self.request("POST", "/api/v1/folders?path=%2Fnew-folder")
        self.assertEqual(status, 201)
        self.assertEqual(json.loads(body)["entry"]["kind"], "folder")

        status, _headers, body = self.request("DELETE", "/api/v1/entries?path=%2Fnew-folder")
        self.assertEqual(status, 204)
        self.assertEqual(body, b"")

        status, _headers, body = self.request(
            "PATCH",
            "/api/v1/entries?path=%2Fhello.txt",
            body=b'{"name":"renamed.txt"}',
            headers={"Content-Length": "22", "Content-Type": "application/json"},
        )
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["entry"]["name"], "renamed.txt")
        self.assertEqual(self.storage.renamed_paths[-1], ("/hello.txt", "renamed.txt"))

        status, _headers, body = self.request(
            "PUT",
            "/api/v1/upload?path=%2Fhello.txt&overwrite=true",
            body=b"new content",
            headers={"Content-Length": "11", "Content-Type": "text/plain"},
        )
        self.assertEqual(status, 201)
        self.assertEqual(json.loads(body)["entry"]["name"], "hello.txt")
        self.assertTrue(self.storage.upload_overwrites[-1])

        auth_server = AdapterHTTPServer(
            ("127.0.0.1", 0),
            AdapterApplication(self.storage, auth=BasicAuth(username="u", password="p")),
        )
        thread = threading.Thread(target=auth_server.serve_forever, daemon=True)
        thread.start()
        connection = HTTPConnection("127.0.0.1", auth_server.server_port, timeout=3)
        try:
            connection.request("GET", "/dav/")
            response = connection.getresponse()
            self.assertEqual(response.status, 401)
            response.read()
            token = base64.b64encode(b"u:p").decode("ascii")
            connection.request("GET", "/healthz")
            response = connection.getresponse()
            self.assertEqual(response.status, 200)
            response.read()
            connection.request("PROPFIND", "/dav/", headers={"Authorization": f"Basic {token}"})
            response = connection.getresponse()
            self.assertEqual(response.status, 207)
            response.read()
        finally:
            connection.close()
            auth_server.shutdown()
            auth_server.server_close()
            thread.join(timeout=3)


class PathTests(unittest.TestCase):
    def test_split_path_decodes_names_and_rejects_traversal(self) -> None:
        self.assertEqual(split_remote_path("/docs/%E4%B8%AD%E6%96%87/"), ("docs", "中文"))
        with self.assertRaises(InvalidPathError):
            split_remote_path("/docs/../secret")


if __name__ == "__main__":
    unittest.main()
