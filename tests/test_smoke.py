from __future__ import annotations

import base64
from email.message import Message
import json
import threading
import unittest
from io import BytesIO
from hashlib import md5, sha1
from pathlib import Path
import runpy
from tempfile import TemporaryDirectory
from urllib.error import HTTPError
from urllib.parse import parse_qsl, urlsplit

from wps_adapter.client import (
    FileCredentialSource,
    WpsApiError,
    WpsClientConfig,
    WpsCredentials,
    WpsDriveClient,
    WpsWorkspaceCandidate,
)
from wps_adapter.har import REDACTED, redact_har, redact_url, safe_entry_details, safe_url_shape, summarize_har
from wps_adapter.provider import InsufficientStorageError, RemoteEntry


CURL_PROBE = runpy.run_path(str(Path(__file__).parents[1] / "tools/wps_curl_probe.py"))
HAR_PROBE = runpy.run_path(str(Path(__file__).parents[1] / "tools/wps_har_probe.py"))


class FakeResponse:
    def __init__(self, body: bytes, headers: dict[str, str] | None = None, status: int = 200) -> None:
        self._body = BytesIO(body)
        self.headers = headers or {}
        self.status = status

    def read(self, size: int = -1) -> bytes:
        return self._body.read(size)

    def close(self) -> None:
        self._body.close()


class FakeOpener:
    def __init__(self, responses: list[FakeResponse]) -> None:
        self.responses = responses
        self.requests = []

    def open(self, request, timeout: float) -> FakeResponse:
        self.requests.append((request, timeout))
        return self.responses.pop(0)


class DirectDownloadFallbackOpener:
    def __init__(self) -> None:
        self.requests = []

    def open(self, request, timeout: float) -> FakeResponse:
        self.requests.append((request, timeout))
        if len(self.requests) == 1:
            raise HTTPError(request.full_url, 403, "unsupported", {}, BytesIO())
        if len(self.requests) == 2:
            return FakeResponse(
                b'{"download_url":"https://hwc-bj.ag.kdocs.cn/signed?sig=secret",'
                b'"status":"finished"}'
            )
        return FakeResponse(b"file-content", {"Content-Length": "12"})


class OverwriteUploadOpener:
    def __init__(self, responses: list[FakeResponse]) -> None:
        self.responses = responses
        self.requests = []

    def open(self, request, timeout: float) -> FakeResponse:
        self.requests.append((request, timeout))
        if len(self.requests) == 1:
            raise HTTPError(request.full_url, 403, "duplicate pre-check", {}, BytesIO())
        return self.responses.pop(0)


class FakeObjectResponse:
    status = 200

    def __init__(
        self,
        headers: list[tuple[str, str]] | None = None,
        body: bytes = b"",
        status: int = 200,
    ) -> None:
        self.status = status
        self._body = BytesIO(body)
        self._headers = headers or [("ETag", '"etag-value"'), ("x-obs-save-key", "object-key")]

    def getheaders(self) -> list[tuple[str, str]]:
        return self._headers

    def read(self, _size: int = -1) -> bytes:
        return self._body.read()


class FakeHttpsConnection:
    def __init__(self) -> None:
        self.target = None
        self.headers = {}
        self.body = BytesIO()
        self.closed = False

    def putrequest(self, method: str, target: str) -> None:
        self.target = (method, target)

    def putheader(self, name: str, value: str) -> None:
        self.headers[name] = value

    def endheaders(self) -> None:
        pass

    def send(self, data: bytes) -> None:
        self.body.write(data)

    def getresponse(self) -> FakeObjectResponse:
        return FakeObjectResponse()

    def close(self) -> None:
        self.closed = True


class PlannedHttpsConnection(FakeHttpsConnection):
    def __init__(self, *, etag: str | None = None, body: bytes = b"") -> None:
        super().__init__()
        self.response_etag = etag
        self.response_body = body

    def getresponse(self) -> FakeObjectResponse:
        headers = [] if self.response_etag is None else [("ETag", self.response_etag)]
        return FakeObjectResponse(headers=headers, body=self.response_body)


class FailingObjectConnection(FakeHttpsConnection):
    def __init__(self) -> None:
        super().__init__()
        self.failed = False

    def getresponse(self) -> FakeObjectResponse:
        if not self.failed:
            self.failed = True
            raise OSError("simulated connection reset")
        return FakeObjectResponse()


class ClientTests(unittest.TestCase):
    def test_status_preflight_checks_login_and_workspace_once(self) -> None:
        opener = FakeOpener([
            FakeResponse(b'{"companyid":691045587,"is_company_account":true}'),
            FakeResponse(b'{"files":[],"next_offset":-1,"result":"ok"}'),
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        first = client.check_status(root_id="0")
        second = client.check_status(root_id="0")

        self.assertEqual(first.status, "connected")
        self.assertEqual(first.wps, "connected")
        self.assertEqual(first.workspace, "ready")
        self.assertEqual(first.account_type, "business")
        self.assertEqual(first.retry_after, 0)
        self.assertEqual(second, first)
        self.assertEqual(len(opener.requests), 2)
        account_request = opener.requests[0][0]
        self.assertEqual(urlsplit(account_request.full_url).hostname, "account.kdocs.cn")
        self.assertEqual(urlsplit(account_request.full_url).path, "/api/v3/islogin")
        self.assertEqual(account_request.get_header("Cookie"), "Cookie-secret")
        workspace_request = opener.requests[1][0]
        self.assertEqual(
            parse_qsl(urlsplit(workspace_request.full_url).query)[0],
            ("parentid", "0"),
        )

    def test_status_without_credentials_does_not_call_wps(self) -> None:
        opener = FakeOpener([])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1"),
            opener=opener,
        )

        result = client.check_status(root_id="0")

        self.assertEqual(result.status, "not_configured")
        self.assertEqual(result.wps, "not_configured")
        self.assertEqual(result.workspace, "not_configured")
        self.assertEqual(opener.requests, [])

    def test_status_treats_missing_credential_files_as_not_configured(self) -> None:
        with TemporaryDirectory() as directory:
            client = WpsDriveClient(
                WpsClientConfig(
                    group_id="group-1",
                    cookie_file=str(Path(directory) / "cookie"),
                    csrf_token_file=str(Path(directory) / "csrf"),
                ),
                opener=FakeOpener([]),
            )

            result = client.check_status(root_id="0")

        self.assertEqual(result.status, "not_configured")

    def test_status_marks_an_expired_session_without_refreshing(self) -> None:
        class ExpiredOpener:
            def __init__(self) -> None:
                self.requests = []

            def open(self, request, timeout: float):
                self.requests.append((request, timeout))
                raise HTTPError(request.full_url, 401, "expired", {}, BytesIO())

        opener = ExpiredOpener()
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        result = client.check_status(root_id="0")

        self.assertEqual(result.status, "session_expired")
        self.assertEqual(len(opener.requests), 1)
        self.assertEqual(urlsplit(opener.requests[0][0].full_url).path, "/api/v3/islogin")

    def test_status_root_list_401_does_not_refresh(self) -> None:
        """D-02: the root listing inside check_status must never refresh."""

        class ExpiredRootOpener:
            def __init__(self) -> None:
                self.requests = []

            def open(self, request, timeout: float):
                self.requests.append((request, timeout))
                if len(self.requests) == 1:
                    return FakeResponse(b'{"islogin":true}')
                raise HTTPError(request.full_url, 401, "expired", {}, BytesIO())

        opener = ExpiredRootOpener()
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        result = client.check_status(root_id="0")

        self.assertEqual(result.status, "session_expired")
        self.assertEqual(result.wps, "session_expired")
        self.assertEqual(len(opener.requests), 2)
        self.assertEqual(
            urlsplit(opener.requests[1][0].full_url).path,
            "/3rd/drive/api/v5/groups/group-1/files",
        )

    def test_status_distinguishes_workspace_permission_failure(self) -> None:
        class PermissionOpener:
            def __init__(self) -> None:
                self.requests = []

            def open(self, request, timeout: float):
                self.requests.append((request, timeout))
                if len(self.requests) == 1:
                    return FakeResponse(b'{"islogin":true,"is_company_account":true}')
                raise HTTPError(request.full_url, 403, "forbidden", {}, BytesIO())

        opener = PermissionOpener()
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        result = client.check_status(root_id="private-root")

        self.assertEqual(result.status, "permission_denied")
        self.assertEqual(result.wps, "connected")
        self.assertEqual(result.workspace, "permission_denied")

    def test_status_marks_malformed_login_response(self) -> None:
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=FakeOpener([FakeResponse(b"[]")]),
        )

        result = client.check_status(root_id="0")

        self.assertEqual(result.status, "invalid_response")

    def test_status_failure_backoff_reuses_the_last_failure(self) -> None:
        class ExpiredOpener:
            def __init__(self) -> None:
                self.requests = []

            def open(self, request, timeout: float):
                self.requests.append((request, timeout))
                raise HTTPError(request.full_url, 401, "expired", {}, BytesIO())

        opener = ExpiredOpener()
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="group-1",
                cookie="Cookie-secret",
                status_failure_backoff=30,
            ),
            opener=opener,
        )

        first = client.check_status(root_id="0")
        second = client.check_status(root_id="0")

        self.assertEqual(first.status, "session_expired")
        self.assertEqual(second.status, "session_expired")
        self.assertGreaterEqual(second.retry_after, 1)
        self.assertEqual(len(opener.requests), 1)

    def test_status_singleflight_merges_concurrent_checks(self) -> None:
        class SlowOpener:
            def __init__(self) -> None:
                self.requests = []
                self.started = threading.Event()
                self.release = threading.Event()

            def open(self, request, timeout: float):
                self.requests.append((request, timeout))
                if len(self.requests) == 1:
                    self.started.set()
                    self.release.wait(timeout=2)
                    return FakeResponse(b'{"islogin":true}')
                return FakeResponse(b'{"files":[],"next_offset":-1,"result":"ok"}')

        opener = SlowOpener()
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret", timeout=2),
            opener=opener,
        )
        results = []

        first_thread = threading.Thread(
            target=lambda: results.append(client.check_status(root_id="0"))
        )
        second_thread = threading.Thread(
            target=lambda: results.append(client.check_status(root_id="0"))
        )
        first_thread.start()
        self.assertTrue(opener.started.wait(timeout=2))
        second_thread.start()
        opener.release.set()
        first_thread.join(timeout=3)
        second_thread.join(timeout=3)

        self.assertFalse(first_thread.is_alive())
        self.assertFalse(second_thread.is_alive())
        self.assertEqual(len(results), 2)
        self.assertTrue(all(result.status == "connected" for result in results))
        self.assertEqual(len(opener.requests), 2)

    def test_client_config_repr_does_not_expose_cookie(self) -> None:
        config = WpsClientConfig(group_id="group-1", cookie="Cookie-secret")
        self.assertNotIn("Cookie-secret", repr(config))

    def test_credential_values_cannot_inject_http_control_characters(self) -> None:
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="sid=ok\r\nX-Leak: yes")
        )
        with self.assertRaises(WpsApiError):
            client.list_entries("folder-1")

    def test_list_maps_confirmed_file_shapes_and_query_names(self) -> None:
        opener = FakeOpener([
            FakeResponse(
                b'{"files":[{"id":7,"fname":"probe.txt","ftype":"file",'
                b'"parentid":3,"fsize":4,"mtime":123,"link_id":"download-cid"}],'
                b'"next_offset":-1,"next_filter":"file","result":"ok"}'
            )
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        page = client.list_entries(
            "folder-3",
            linkgroup=True,
            include="acl,pic_thumbnail",
            with_link=False,
        )

        self.assertEqual(page.entries[0].id, "7")
        self.assertEqual(page.entries[0].kind, "file")
        self.assertEqual(page.entries[0].link_id, "download-cid")
        self.assertEqual(page.next_offset, -1)
        request = opener.requests[0][0]
        query_names = [key for key, _ in parse_qsl(urlsplit(request.full_url).query)]
        self.assertIn("parentid", query_names)
        self.assertIn("include", query_names)
        self.assertEqual(request.get_header("Cookie"), "Cookie-secret")

    def test_workspace_discovery_is_opt_in_and_returns_candidates(self) -> None:
        opener = FakeOpener([])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        with self.assertRaises(WpsApiError) as error:
            client.discover_spaces_candidate(company_id="691045587")

        self.assertEqual(error.exception.category, "disabled")
        self.assertEqual(opener.requests, [])

    def test_workspace_discovery_strictly_parses_openlist_candidate_shape(self) -> None:
        opener = FakeOpener([
            FakeResponse(
                b'{"result":"ok","groups":['
                b'{"id":2579904987,"name":"School drive"},'
                b'{"group_id":"team-2","name":"Personal team"}]}'
            )
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        candidates = client.discover_spaces_candidate(
            company_id="691045587",
            enabled=True,
        )

        self.assertEqual([candidate.group_id for candidate in candidates], ["2579904987", "team-2"])
        self.assertEqual([candidate.status for candidate in candidates], ["candidate", "candidate"])
        self.assertTrue(all(not candidate.verified for candidate in candidates))
        request = opener.requests[0][0]
        self.assertEqual(
            urlsplit(request.full_url).path,
            "/3rd/plus/groups/v1/companies/691045587/users/self/groups/private",
        )
        self.assertEqual(request.get_header("Cookie"), "Cookie-secret")

    def test_workspace_discovery_rejects_malformed_groups_without_partial_results(self) -> None:
        for body in (
            b'{"groups":[{"id":"group-1","name":""}]}',
            b'{"groups":[{"id":"group/1","name":"Drive"}]}',
            b'{"groups":[{"id":"group-1","name":"Drive"},{"id":"group-1","name":"Duplicate"}]}',
            b'{"data":[]}',
        ):
            client = WpsDriveClient(
                WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
                opener=FakeOpener([FakeResponse(body)]),
            )
            with self.assertRaises(WpsApiError) as error:
                client.discover_spaces_candidate(company_id="company", enabled=True)
            self.assertEqual(error.exception.category, "invalid_response")

    def test_workspace_candidate_verification_is_read_only_and_marks_verified(self) -> None:
        opener = FakeOpener([
            FakeResponse(b'{"files":[],"next_offset":-1,"result":"ok"}')
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="active-group", cookie="Cookie-secret"),
            opener=opener,
        )
        candidate = WpsWorkspaceCandidate("candidate-group", "School drive", "company")

        verified = client.verify_workspace_candidate(candidate)

        self.assertTrue(verified.verified)
        self.assertEqual(verified.status, "verified")
        self.assertEqual(client.group_id, "active-group")
        request = opener.requests[0][0]
        self.assertIn("/groups/candidate-group/files", request.full_url)
        self.assertEqual(parse_qsl(urlsplit(request.full_url).query)[0], ("parentid", "0"))

    def test_json_response_size_is_bounded(self) -> None:
        opener = FakeOpener([
            FakeResponse(b"{}", {"Content-Length": str(32 * 1024 * 1024 + 1)}),
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )
        with self.assertRaises(WpsApiError):
            client.list_entries("folder-1")

    def test_remote_metadata_rejects_unsafe_names(self) -> None:
        with self.assertRaises(WpsApiError):
            WpsDriveClient._entry_from_item(
                {"id": 1, "fname": "bad\nname", "ftype": "file"}
            )
        with self.assertRaises(WpsApiError):
            WpsDriveClient._entry_from_item(
                {"id": 1, "fname": "x" * 4097, "ftype": "file"}
            )
        self.assertIsNone(
            WpsDriveClient._entry_from_item(
                {"id": 1, "fname": "ok", "ftype": "file", "fsha": "x" * 4097}
            ).etag
        )

    def test_download_stream_does_not_forward_wps_cookie(self) -> None:
        opener = FakeOpener([
            FakeResponse(b'{"download_url":"https://hwc-bj.ag.kdocs.cn/signed?sig=secret",'
                         b'"status":"finished"}'),
            FakeResponse(b"file-content", {"Content-Type": "application/octet-stream", "Content-Length": "12"}),
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        destination = BytesIO()
        written = client.download_to("file-1", destination, cid="tenant-1")

        self.assertEqual(written, 12)
        self.assertEqual(destination.getvalue(), b"file-content")
        api_request = opener.requests[0][0]
        object_request = opener.requests[1][0]
        self.assertEqual(api_request.get_header("Cookie"), "Cookie-secret")
        self.assertIsNone(object_request.get_header("Cookie"))
        query_names = [key for key, _ in parse_qsl(urlsplit(api_request.full_url).query)]
        self.assertEqual(query_names, ["support_checksums", "cid"])

    def test_download_retries_with_direct_flag_after_observed_403(self) -> None:
        opener = DirectDownloadFallbackOpener()
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        destination = BytesIO()
        written = client.download_to("file-1", destination, cid="file-link-cid")

        self.assertEqual(written, 12)
        first_query = [key for key, _ in parse_qsl(urlsplit(opener.requests[0][0].full_url).query)]
        second_query = [key for key, _ in parse_qsl(urlsplit(opener.requests[1][0].full_url).query)]
        self.assertEqual(first_query, ["support_checksums", "cid"])
        self.assertEqual(
            second_query,
            ["support_checksums", "get_direct_external_download_url", "cid"],
        )

    def test_range_download_requires_partial_object_response(self) -> None:
        opener = FakeOpener([
            FakeResponse(b'{"download_url":"https://hwc-bj.ag.kdocs.cn/signed"}'),
            FakeResponse(
                b"world",
                {"Content-Length": "5", "Content-Range": "bytes 6-10/11"},
                status=206,
            ),
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        with client.open_download("file-1", offset=6, length=5) as stream:
            self.assertEqual(stream.read(), b"world")
            self.assertEqual(stream.http_status, 206)
            self.assertEqual(stream.content_range, "bytes 6-10/11")
        self.assertEqual(opener.requests[1][0].get_header("Range"), "bytes=6-10")

    def test_range_download_rejects_mismatched_content_range(self) -> None:
        opener = FakeOpener([
            FakeResponse(b'{"download_url":"https://hwc-bj.ag.kdocs.cn/signed"}'),
            FakeResponse(
                b"wrong",
                {"Content-Length": "5", "Content-Range": "bytes 0-4/11"},
                status=206,
            ),
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        with self.assertRaises(WpsApiError):
            client.open_download("file-1", offset=6, length=5)

    def test_download_rejects_a_signed_url_outside_the_wps_object_store(self) -> None:
        opener = FakeOpener([
            FakeResponse(b'{"download_url":"https://attacker.example/signed"}'),
        ])
        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=opener,
        )

        with self.assertRaises(WpsApiError):
            client.open_download("file-1")

    def test_signed_target_rejects_http_control_characters(self) -> None:
        client = WpsDriveClient(WpsClientConfig(group_id="group-1"))
        with self.assertRaises(WpsApiError):
            client._signed_target(
                "https://hwc-bj.ag.kdocs.cn/object\r\nX-Leak: yes",
                "signed URL",
            )

    def test_object_store_configuration_cannot_escape_wps_domain(self) -> None:
        with self.assertRaisesRegex(ValueError, "within kdocs.cn"):
            WpsDriveClient(
                WpsClientConfig(
                    group_id="group-1",
                    object_storage_host_suffix="attacker.example",
                )
            )

    def test_upload_uses_confirmed_control_flow_and_streams_object_body(self) -> None:
        object_connection = FakeHttpsConnection()
        opener = FakeOpener([
            FakeResponse(b'{"result":"ok"}'),
            FakeResponse(
                b'{"method":"PUT","store":"obscn",'
                b'"url":"https://hwc-bj.ag.kdocs.cn/signed",'
                b'"response":{"expect_code":[200]}}'
            ),
            FakeResponse(b'{"id":9,"fname":"probe.txt","ftype":"file",'
                         b'"parentid":3,"fsize":5,"result":"ok"}'),
        ])
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                cookie="Cookie-secret",
                csrf_token="csrf-secret",
                stream_chunk_size=2,
            ),
            opener=opener,
            https_connection_factory=lambda host, port, timeout: object_connection,
        )

        entry = client.upload("3", "probe.txt", BytesIO(b"hello"))

        self.assertEqual(entry.id, "9")
        self.assertEqual(object_connection.body.getvalue(), b"hello")
        self.assertEqual(object_connection.headers["Content-Type"], "application/octet-stream")
        self.assertEqual(object_connection.headers["Content-Length"], "5")
        create_request = opener.requests[1][0]
        create_body = json.loads(create_request.data.decode("utf-8"))
        self.assertEqual(create_body["size"], 5)
        self.assertEqual(create_body["sha256"], "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
        self.assertNotIn("md5", create_body)
        self.assertEqual(create_body["csrfmiddlewaretoken"], "csrf-secret")
        file_request = opener.requests[2][0]
        file_body = json.loads(file_request.data.decode("utf-8"))
        self.assertEqual(file_body["key"], sha1(b"hello").hexdigest())
        self.assertEqual(file_body["etag"], '"etag-value"')

    def test_upload_retries_a_failed_signed_put_with_a_new_instruction(self) -> None:
        first_connection = FailingObjectConnection()
        second_connection = FakeHttpsConnection()
        connections = [first_connection, second_connection]
        opener = FakeOpener([
            FakeResponse(b'{"result":"ok"}'),
            FakeResponse(
                b'{"method":"PUT","store":"obscn",'
                b'"url":"https://hwc-bj.ag.kdocs.cn/signed-1",'
                b'"response":{"expect_code":[200]}}'
            ),
            FakeResponse(
                b'{"method":"PUT","store":"obscn",'
                b'"url":"https://hwc-bj.ag.kdocs.cn/signed-2",'
                b'"response":{"expect_code":[200]}}'
            ),
            FakeResponse(b'{"id":9,"fname":"probe.txt","ftype":"file",'
                         b'"parentid":3,"fsize":5,"result":"ok"}'),
        ])
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                cookie="Cookie-secret",
                csrf_token="csrf-secret",
                upload_retries=1,
                upload_retry_delay=0,
            ),
            opener=opener,
            https_connection_factory=lambda host, port, timeout: connections.pop(0),
        )

        entry = client.upload("3", "probe.txt", BytesIO(b"hello"))

        self.assertEqual(entry.id, "9")
        self.assertEqual(second_connection.body.getvalue(), b"hello")
        create_requests = [
            request for request, _timeout in opener.requests
            if request.full_url.endswith("/3rd/drive/api/v5/files/upload/create_update")
        ]
        self.assertEqual(len(create_requests), 2)

    def test_upload_rejects_when_the_spool_free_space_reserve_is_not_met(self) -> None:
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                csrf_token="csrf-secret",
                upload_spool_memory=0,
                upload_min_free_bytes=10**18,
            )
        )
        with self.assertRaises(InsufficientStorageError):
            client.upload("3", "too-large-for-test.bin", BytesIO(b"x"), size=1)

    def test_overwrite_upload_accepts_observed_precheck_403(self) -> None:
        object_connection = FakeHttpsConnection()
        opener = OverwriteUploadOpener([
            FakeResponse(
                b'{"method":"PUT","store":"obscn",'
                b'"url":"https://hwc-bj.ag.kdocs.cn/signed",'
                b'"response":{"expect_code":[200]}}'
            ),
            FakeResponse(
                b'{"id":9,"fname":"probe.txt","ftype":"file",'
                b'"parentid":3,"fsize":5,"fver":2,"result":"ok"}'
            ),
        ])
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                cookie="Cookie-secret",
                csrf_token="csrf-secret",
                stream_chunk_size=2,
            ),
            opener=opener,
            https_connection_factory=lambda host, port, timeout: object_connection,
        )

        entry = client.upload("3", "probe.txt", BytesIO(b"hello"), overwrite=True)

        self.assertEqual(entry.id, "9")
        create_body = json.loads(opener.requests[1][0].data.decode("utf-8"))
        self.assertEqual(create_body["md5"], "5d41402abc4b2a76b9719d911017c592")
        self.assertEqual(create_body["client_stores"], "ks3,ks3sh")
        self.assertEqual(create_body["startswithfilename"], "probe.txt")
        self.assertEqual(create_body["successactionstatus"], 201)
        self.assertEqual(create_body["tried_store"], ["ks3,ks3sh"])
        final_body = json.loads(opener.requests[2][0].data.decode("utf-8"))
        self.assertFalse(final_body["isUpNewVer"])

    def test_multipart_upload_uses_observed_block_merge_flow(self) -> None:
        content = b"abcdefghij"
        full_sha1 = sha1(content).hexdigest()
        part_one_md5 = md5(content[:5]).hexdigest()
        part_two_md5 = md5(content[5:]).hexdigest()
        part_one_content_md5 = base64.b64encode(md5(content[:5]).digest()).decode("ascii")
        part_two_content_md5 = base64.b64encode(md5(content[5:]).digest()).decode("ascii")
        merge_xml = (
            "<CompleteMultipartUpload>"
            '<Part><ETag>part-one-etag</ETag><PartNumber>1</PartNumber></Part>'
            '<Part><ETag>part-two-etag</ETag><PartNumber>2</PartNumber></Part>'
            "</CompleteMultipartUpload>"
        )
        opener = FakeOpener([
            FakeResponse(b'{"result":"ok"}'),
            FakeResponse(
                json.dumps(
                    {
                        "result": "ok",
                        "key": full_sha1,
                        "store": "obscn",
                        "upload_id": "upload-1",
                        "limit": {
                            "max_parts": 10000,
                            "min_part_size": 5,
                            "max_part_size": 100,
                        },
                    }
                ).encode("utf-8")
            ),
            FakeResponse(
                json.dumps(
                    {
                        "result": "ok",
                        "method": "PUT",
                        "request": {
                            "body_type": "file",
                            "headers": {
                                "Content-MD5": part_one_content_md5,
                                "Content-Type": "application/octet-stream",
                            },
                        },
                        "response": {"expect_code": [200]},
                        "url": "https://hwc-bj.ag.kdocs.cn/api/multipart/upload-1/part-1",
                    }
                ).encode("utf-8")
            ),
            FakeResponse(
                json.dumps(
                    {
                        "result": "ok",
                        "method": "PUT",
                        "request": {
                            "body_type": "file",
                            "headers": {
                                "Content-MD5": part_two_content_md5,
                                "Content-Type": "application/octet-stream",
                            },
                        },
                        "response": {"expect_code": [200]},
                        "url": "https://hwc-bj.ag.kdocs.cn/api/multipart/upload-1/part-2",
                    }
                ).encode("utf-8")
            ),
            FakeResponse(
                json.dumps(
                    {
                        "result": "ok",
                        "method": "POST",
                        "request": {
                            "body_type": "data",
                            "body_data": merge_xml,
                            "headers": {"Content-Type": "application/xml"},
                        },
                        "response": {"expect_code": [200]},
                        "url": "https://hwc-bj.ag.kdocs.cn/api/multipart/upload-1/complete",
                    }
                ).encode("utf-8")
            ),
            FakeResponse(
                b'{"id":9,"fname":"large.bin","ftype":"file",'
                b'"parentid":3,"fsize":10,"fver":1,"result":"ok"}'
            ),
        ])
        part_one_connection = PlannedHttpsConnection(etag='"part-one-etag"')
        part_two_connection = PlannedHttpsConnection(etag='"part-two-etag"')
        merge_connection = PlannedHttpsConnection(
            body=b'<CompleteMultipartUploadResult><ETag>"merged-etag"</ETag></CompleteMultipartUploadResult>'
        )
        connections = [part_one_connection, part_two_connection, merge_connection]
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                cookie="Cookie-secret",
                csrf_token="csrf-secret",
                stream_chunk_size=2,
                multipart_threshold=1,
                multipart_part_size=5,
            ),
            opener=opener,
            https_connection_factory=lambda host, port, timeout: connections.pop(0),
        )

        entry = client.upload("3", "large.bin", BytesIO(content))

        self.assertEqual(entry.id, "9")
        init_body = json.loads(opener.requests[1][0].data.decode("utf-8"))
        self.assertEqual(init_body["hash"], full_sha1)
        self.assertEqual(init_body["group_id"], "1")
        self.assertEqual(init_body["parent_id"], "3")
        self.assertEqual(init_body["tried_store"], [])
        first_block_body = json.loads(opener.requests[2][0].data.decode("utf-8"))
        self.assertEqual(first_block_body["key"], full_sha1)
        self.assertEqual(first_block_body["md5"], part_one_md5)
        self.assertEqual(first_block_body["part_number"], 1)
        self.assertEqual(first_block_body["part_size"], 5)
        self.assertEqual(part_one_connection.target[0], "PUT")
        self.assertEqual(part_one_connection.headers["Content-MD5"], part_one_content_md5)
        self.assertEqual(part_one_connection.body.getvalue(), content[:5])
        self.assertEqual(part_two_connection.body.getvalue(), content[5:])
        merge_body = json.loads(opener.requests[4][0].data.decode("utf-8"))
        self.assertEqual(
            merge_body["part_infos"],
            [
                {"etag": "part-one-etag", "part_number": 1},
                {"etag": "part-two-etag", "part_number": 2},
            ],
        )
        self.assertEqual(merge_connection.target[0], "POST")
        self.assertEqual(merge_connection.headers["Content-Type"], "application/xml")
        self.assertEqual(merge_connection.body.getvalue(), merge_xml.encode("utf-8"))
        final_body = json.loads(opener.requests[5][0].data.decode("utf-8"))
        self.assertEqual(final_body["key"], full_sha1)
        self.assertEqual(final_body["sha1"], full_sha1)
        self.assertEqual(final_body["etag"], "merged-etag")
        self.assertEqual(final_body["groupid"], "1")
        self.assertEqual(final_body["parentid"], "3")

    def test_multipart_part_buffer_has_a_memory_ceiling(self) -> None:
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="group-1",
                cookie="Cookie-secret",
                multipart_part_size=65 * 1024 * 1024,
            )
        )
        with self.assertRaises(InsufficientStorageError):
            client._multipart_part_size(
                100 * 1024 * 1024,
                {"min_part_size": 5 * 1024 * 1024,
                 "max_part_size": 5 * 1024 * 1024 * 1024,
                 "max_parts": 10000},
        )

    def test_multipart_checkpoint_is_reused_after_restart(self) -> None:
        content = b"abcdefghij"
        full_sha1 = sha1(content).hexdigest()
        md5_one = md5(content[:5]).digest()
        md5_two = md5(content[5:]).digest()

        def instruction(part: int, digest: bytes) -> FakeResponse:
            return FakeResponse(json.dumps({
                "result": "ok", "method": "PUT", "request": {
                    "body_type": "file", "headers": {
                        "Content-MD5": base64.b64encode(digest).decode(),
                        "Content-Type": "application/octet-stream",
                    },
                }, "response": {"expect_code": [200]},
                "url": f"https://hwc-bj.ag.kdocs.cn/p{part}",
            }).encode())

        class StopAfterFirstPart(FakeOpener):
            def open(self, request, timeout):
                if len(self.requests) == 3:
                    self.requests.append((request, timeout))
                    raise HTTPError(request.full_url, 503, "stop", {}, BytesIO())
                return super().open(request, timeout)

        with TemporaryDirectory() as directory:
            first_opener = StopAfterFirstPart([
                FakeResponse(b'{"result":"ok"}'),
                FakeResponse(json.dumps({"result":"ok", "key":full_sha1,
                    "store":"obscn", "upload_id":"u1", "limit":{
                        "max_parts":10000, "min_part_size":5, "max_part_size":100}}).encode()),
                instruction(1, md5_one),
            ])
            first_connection = PlannedHttpsConnection(etag='"e1"')
            config = WpsClientConfig(group_id="1", cookie="Cookie-secret",
                csrf_token="csrf-secret", multipart_threshold=1,
                multipart_part_size=5, upload_retries=0, upload_resume_dir=directory)
            first = WpsDriveClient(config, opener=first_opener,
                https_connection_factory=lambda *_: first_connection)
            with self.assertRaises(WpsApiError):
                first.upload("3", "resume.bin", BytesIO(content))
            checkpoint = list(Path(directory).glob("*.json"))
            self.assertEqual(len(checkpoint), 1)
            saved = checkpoint[0].read_text()
            self.assertEqual(json.loads(saved)["parts"], {"1": "e1"})
            self.assertNotIn("Cookie-secret", saved)

            merge_xml = "<CompleteMultipartUploadResult><ETag>merged</ETag></CompleteMultipartUploadResult>"
            second_opener = FakeOpener([
                FakeResponse(b'{"result":"ok"}'), instruction(2, md5_two),
                FakeResponse(json.dumps({"result":"ok", "method":"POST", "request":{
                    "body_type":"data", "body_data":"<merge/>",
                    "headers":{"Content-Type":"application/xml"}},
                    "response":{"expect_code":[200]},
                    "url":"https://hwc-bj.ag.kdocs.cn/complete"}).encode()),
                FakeResponse(b'{"id":9,"fname":"resume.bin","ftype":"file",'
                    b'"parentid":3,"fsize":10,"result":"ok"}'),
            ])
            part_two = PlannedHttpsConnection(etag='"e2"')
            merge = PlannedHttpsConnection(body=merge_xml.encode())
            connections = [part_two, merge]
            second = WpsDriveClient(config, opener=second_opener,
                https_connection_factory=lambda *_: connections.pop(0))
            self.assertEqual(second.upload("3", "resume.bin", BytesIO(content)).id, "9")
            self.assertEqual(part_two.body.getvalue(), content[5:])
            self.assertEqual(list(Path(directory).glob("*.json")), [])

    def test_create_folder_uses_confirmed_json_body(self) -> None:
        opener = FakeOpener([
            FakeResponse(
                b'{"id":9,"fname":"new-folder","ftype":"folder",'
                b'"parentid":3,"fsize":0,"result":"ok"}'
            )
        ])
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                cookie="Cookie-secret",
                csrf_token="csrf-secret",
            ),
            opener=opener,
        )

        entry = client.create_folder("3", "new-folder")

        self.assertEqual(entry.id, "9")
        self.assertEqual(entry.kind, "folder")
        request = opener.requests[0][0]
        self.assertEqual(request.get_header("Cookie"), "Cookie-secret")
        self.assertEqual(request.get_header("Content-type"), "application/json")
        body = json.loads(request.data.decode("utf-8"))
        self.assertEqual(
            body,
            {
                "groupid": 1,
                "parentid": 3,
                "name": "new-folder",
                "owner": True,
                "parsed": True,
                "csrfmiddlewaretoken": "csrf-secret",
            },
        )

    def test_rename_uses_confirmed_v3_json_body(self) -> None:
        opener = FakeOpener([
            FakeResponse(
                b'{"id":9,"fname":"renamed-folder","ftype":"folder",'
                b'"groupid":1,"parentid":3,"fsize":0,"mtime":123}'
            )
        ])
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                cookie="Cookie-secret",
                csrf_token="csrf-secret",
            ),
            opener=opener,
        )

        entry = client.rename("9", "renamed-folder")

        self.assertEqual(entry.id, "9")
        self.assertEqual(entry.name, "renamed-folder")
        request = opener.requests[0][0]
        self.assertEqual(request.get_method(), "PUT")
        self.assertEqual(
            urlsplit(request.full_url).path,
            "/3rd/drive/api/v3/groups/1/files/9",
        )
        self.assertEqual(request.get_header("Cookie"), "Cookie-secret")
        body = json.loads(request.data.decode("utf-8"))
        self.assertEqual(
            body,
            {
                "fname": "renamed-folder",
                "csrfmiddlewaretoken": "csrf-secret",
            },
        )

    def test_copy_uses_confirmed_same_group_endpoint(self) -> None:
        opener = FakeOpener([FakeResponse(b'{"result":"ok","fileids":[99]}')])
        client = WpsDriveClient(
            WpsClientConfig(group_id="2579904987", cookie="Cookie-secret", csrf_token="csrf-secret"),
            opener=opener,
        )

        self.assertEqual(client.copy("7", "8"), "99")
        request = opener.requests[0][0]
        self.assertEqual(
            urlsplit(request.full_url).path,
            "/3rd/drive/api/v3/groups/2579904987/files/batch/copy",
        )
        self.assertEqual(
            json.loads(request.data.decode("utf-8")),
            {
                "fileids": [7],
                "groupid": 2579904987,
                "target_groupid": 2579904987,
                "target_parentid": 8,
                "duplicated_name_model": 1,
                "csrfmiddlewaretoken": "csrf-secret",
            },
        )

    def test_delete_posts_task_and_waits_for_success(self) -> None:
        opener = FakeOpener([
            FakeResponse(
                b'{"result":"ok","taskid":12,"taskuuid":"task-uuid"}'
            ),
            FakeResponse(
                b'{"estimated_time_left":-1,"failed_list":null,"finish":1,'
                b'"result":"ok","status":"success","taskid":12,'
                b'"taskuuid":"task-uuid","total":1}'
            ),
        ])
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                cookie="Cookie-secret",
                csrf_token="csrf-secret",
            ),
            opener=opener,
        )

        client.delete("7", poll_interval=0)

        delete_request = opener.requests[0][0]
        self.assertEqual(delete_request.get_method(), "POST")
        self.assertEqual(delete_request.get_header("Cookie"), "Cookie-secret")
        body = json.loads(delete_request.data.decode("utf-8"))
        self.assertEqual(body["fileids"], [7])
        self.assertEqual(body["groupid"], 1)
        self.assertEqual(body["csrfmiddlewaretoken"], "csrf-secret")
        progress_request = opener.requests[1][0]
        self.assertEqual(progress_request.get_method(), "GET")
        self.assertEqual(
            parse_qsl(urlsplit(progress_request.full_url).query),
            [("taskuuid", "task-uuid")],
        )

    def test_move_posts_task_and_waits_for_success(self) -> None:
        opener = FakeOpener([
            FakeResponse(
                b'{"result":"ok","taskid":13,"taskuuid":"move-task"}'
            ),
            FakeResponse(
                b'{"estimated_time_left":-1,"failed_list":null,"finish":1,'
                b'"result":"ok","status":"success","taskid":13,'
                b'"taskuuid":"move-task","total":1}'
            ),
        ])
        client = WpsDriveClient(
            WpsClientConfig(
                group_id="1",
                cookie="Cookie-secret",
                csrf_token="csrf-secret",
            ),
            opener=opener,
        )

        client.move("7", "3", "8", poll_interval=0)

        move_request = opener.requests[0][0]
        self.assertEqual(move_request.get_method(), "POST")
        self.assertEqual(
            urlsplit(move_request.full_url).path,
            "/3rd/drive/api/v5/files/batch/task/move",
        )
        body = json.loads(move_request.data.decode("utf-8"))
        self.assertEqual(
            body,
            {
                "groupid": 1,
                "parentid": 3,
                "dst_groupid": 1,
                "dst_parentid": 8,
                "fileids": [7],
                "option": {},
                "csrfmiddlewaretoken": "csrf-secret",
            },
        )
        progress_request = opener.requests[1][0]
        self.assertEqual(
            parse_qsl(urlsplit(progress_request.full_url).query),
            [("taskuuid", "move-task")],
        )

    def test_iter_entries_follows_forward_pagination(self) -> None:
        opener = FakeOpener([
            FakeResponse(b'{"files":[{"id":1,"fname":"one","ftype":"file"}],"next_offset":1}'),
            FakeResponse(b'{"files":[{"id":2,"fname":"two","ftype":"file"}],"next_offset":-1}'),
        ])
        client = WpsDriveClient(WpsClientConfig(group_id="group-1"), opener=opener)
        entries = client.iter_entries("folder-1", count=1)
        self.assertEqual([entry.id for entry in entries], ["1", "2"])
        self.assertEqual(
            [parse_qsl(urlsplit(request.full_url).query)[1][1] for request, _ in opener.requests],
            ["0", "1"],
        )

    def test_iter_entries_passes_wps_next_filter_to_the_next_page(self) -> None:
        opener = FakeOpener([
            FakeResponse(b'{"files":[{"id":1,"fname":"one","ftype":"file"}],"next_offset":14,"next_filter":"file"}'),
            FakeResponse(b'{"files":[{"id":2,"fname":"two","ftype":"file"}],"next_offset":-1,"next_filter":"file"}'),
        ])
        client = WpsDriveClient(WpsClientConfig(group_id="group-1"), opener=opener)

        entries = client.iter_entries("folder-1", count=20)

        self.assertEqual([entry.id for entry in entries], ["1", "2"])
        self.assertEqual(
            parse_qsl(urlsplit(opener.requests[0][0].full_url).query),
            [
                ("parentid", "folder-1"),
                ("offset", "0"),
                ("count", "20"),
                ("orderby", "mtime"),
                ("order", "desc"),
            ],
        )
        self.assertIn(
            ("next_filter", "file"),
            parse_qsl(urlsplit(opener.requests[1][0].full_url).query),
        )

    def test_iter_entries_deduplicates_overlapping_wps_pages(self) -> None:
        opener = FakeOpener([
            FakeResponse(
                b'{"files":[{"id":1,"fname":"one","ftype":"file"},{"id":2,"fname":"two","ftype":"file"}],"next_offset":1,"next_filter":"file"}'
            ),
            FakeResponse(
                b'{"files":[{"id":2,"fname":"two","ftype":"file"},{"id":3,"fname":"three","ftype":"file"}],"next_offset":-1,"next_filter":"file"}'
            ),
        ])
        client = WpsDriveClient(WpsClientConfig(group_id="group-1"), opener=opener)

        entries = client.iter_entries("folder-1", count=2)

        self.assertEqual([entry.id for entry in entries], ["1", "2", "3"])

    def test_iter_entries_bounds_broken_pagination(self) -> None:
        class EndlessOpener:
            def __init__(self) -> None:
                self.offset = 0

            def open(self, request, timeout: float) -> FakeResponse:
                self.offset += 1
                return FakeResponse(
                    json.dumps({"files": [], "next_offset": self.offset, "result": "ok"}).encode()
                )

        client = WpsDriveClient(
            WpsClientConfig(group_id="group-1", cookie="Cookie-secret"),
            opener=EndlessOpener(),
        )
        with self.assertRaises(InsufficientStorageError):
            client.iter_entries("folder-1", count=1, max_entries=2)

    def test_file_credentials_are_read_without_repr_and_csrf_can_come_from_cookie(self) -> None:
        with TemporaryDirectory() as directory:
            cookie_path = Path(directory) / "cookie"
            cookie_path.write_text("sid=first; csrf=csrf-first", encoding="utf-8")
            cookie_path.chmod(0o600)
            source = FileCredentialSource(cookie_path=str(cookie_path))
            self.assertEqual(source.get(), WpsCredentials(cookie="sid=first; csrf=csrf-first"))
            config = WpsClientConfig(group_id="group-1", credential_source=source)
            client = WpsDriveClient(
                config,
                opener=FakeOpener([FakeResponse(b'{"files":[],"next_offset":-1}')]),
            )
            client.list_entries("folder-1")
            self.assertEqual(client._opener.requests[0][0].get_header("Cookie"), "sid=first; csrf=csrf-first")

    def test_file_credentials_refresh_detects_a_replaced_snapshot(self) -> None:
        with TemporaryDirectory() as directory:
            cookie_path = Path(directory) / "cookie"
            csrf_path = Path(directory) / "csrf"
            cookie_path.write_text("sid=first", encoding="utf-8")
            csrf_path.write_text("csrf-first", encoding="utf-8")
            cookie_path.chmod(0o600)
            csrf_path.chmod(0o600)
            source = FileCredentialSource(
                cookie_path=str(cookie_path),
                csrf_token_path=str(csrf_path),
            )
            source.get()
            cookie_path.write_text("sid=second", encoding="utf-8")
            csrf_path.write_text("csrf-second", encoding="utf-8")
            self.assertTrue(source.refresh())
            self.assertEqual(
                source.get(),
                WpsCredentials(cookie="sid=second", csrf_token="csrf-second"),
            )

    def test_file_credentials_reject_symlinks_and_broad_permissions(self) -> None:
        with TemporaryDirectory() as directory:
            target = Path(directory) / "cookie"
            target.write_text("sid=first", encoding="utf-8")
            target.chmod(0o644)
            source = FileCredentialSource(cookie_path=str(target))
            with self.assertRaises(WpsApiError):
                source.get()

            target.chmod(0o600)
            link = Path(directory) / "cookie-link"
            link.symlink_to(target)
            linked_source = FileCredentialSource(cookie_path=str(link))
            with self.assertRaises(WpsApiError):
                linked_source.get()

    def test_file_credentials_can_be_replaced_as_a_pair(self) -> None:
        with TemporaryDirectory() as directory:
            cookie_path = Path(directory) / "cookie"
            csrf_path = Path(directory) / "csrf"
            cookie_path.write_text("sid=first", encoding="utf-8")
            csrf_path.write_text("csrf-first", encoding="utf-8")
            cookie_path.chmod(0o600)
            csrf_path.chmod(0o600)
            source = FileCredentialSource(
                cookie_path=str(cookie_path),
                csrf_token_path=str(csrf_path),
            )

            self.assertTrue(
                source.replace_credentials(
                    WpsCredentials(cookie="sid=second; rtk=refresh", csrf_token="csrf-second")
                )
            )
            self.assertEqual(cookie_path.read_text(encoding="utf-8").strip(), "sid=second; rtk=refresh")
            self.assertEqual(csrf_path.read_text(encoding="utf-8").strip(), "csrf-second")

    def test_401_retry_replaces_cookie_and_json_csrf(self) -> None:
        with TemporaryDirectory() as directory:
            cookie_path = Path(directory) / "cookie"
            csrf_path = Path(directory) / "csrf"
            cookie_path.write_text("sid=first", encoding="utf-8")
            csrf_path.write_text("csrf-first", encoding="utf-8")
            cookie_path.chmod(0o600)
            csrf_path.chmod(0o600)
            source = FileCredentialSource(
                cookie_path=str(cookie_path),
                csrf_token_path=str(csrf_path),
            )

            class RefreshingOpener:
                def __init__(self) -> None:
                    self.requests = []

                def open(self, request, timeout: float) -> FakeResponse:
                    self.requests.append((request, timeout))
                    if len(self.requests) == 1:
                        cookie_path.write_text("sid=second", encoding="utf-8")
                        csrf_path.write_text("csrf-second", encoding="utf-8")
                        raise HTTPError(request.full_url, 401, "expired", {}, BytesIO())
                    return FakeResponse(
                        b'{"id":2,"fname":"new-folder","ftype":"folder",'
                        b'"parentid":1,"fsize":0,"mtime":2,"result":"ok"}'
                    )

            opener = RefreshingOpener()
            client = WpsDriveClient(
                WpsClientConfig(group_id="group-1", credential_source=source),
                opener=opener,
            )

            client.create_folder("1", "new-folder")

            retried_request = opener.requests[1][0]
            self.assertEqual(retried_request.get_header("Cookie"), "sid=second")
            self.assertEqual(
                json.loads(retried_request.data.decode("utf-8"))["csrfmiddlewaretoken"],
                "csrf-second",
            )

    def test_401_uses_wps_refresh_grant_and_persists_rotated_cookies(self) -> None:
        with TemporaryDirectory() as directory:
            cookie_path = Path(directory) / "cookie"
            csrf_path = Path(directory) / "csrf"
            cookie_path.write_text("sid=first; rtk=refresh-ticket", encoding="utf-8")
            csrf_path.write_text("csrf-first", encoding="utf-8")
            cookie_path.chmod(0o600)
            csrf_path.chmod(0o600)
            source = FileCredentialSource(
                cookie_path=str(cookie_path),
                csrf_token_path=str(csrf_path),
            )
            refresh_headers = Message()
            refresh_headers.add_header("Set-Cookie", "sid=second; Path=/")
            refresh_headers.add_header("Set-Cookie", "csrf=csrf-second; Path=/")

            class RefreshGrantOpener:
                def __init__(self) -> None:
                    self.requests = []

                def open(self, request, timeout: float) -> FakeResponse:
                    self.requests.append((request, timeout))
                    if len(self.requests) == 1:
                        raise HTTPError(request.full_url, 401, "expired", {}, BytesIO())
                    if len(self.requests) == 2:
                        return FakeResponse(
                            b'{"result":"ok"}',
                            headers=refresh_headers,
                        )
                    return FakeResponse(
                        b'{"files":[],"next_offset":-1,"result":"ok"}'
                    )

            opener = RefreshGrantOpener()
            client = WpsDriveClient(
                WpsClientConfig(
                    group_id="group-1",
                    credential_source=source,
                ),
                opener=opener,
            )

            client.list_entries("folder-1")

            refresh_request = opener.requests[1][0]
            self.assertEqual(
                urlsplit(refresh_request.full_url).path,
                "/passport/secure/api/grant_token",
            )
            self.assertEqual(
                json.loads(refresh_request.data.decode("utf-8")),
                {"grant_type": "refresh_token"},
            )
            retry_request = opener.requests[2][0]
            self.assertIn("sid=second", retry_request.get_header("Cookie"))
            self.assertIn("rtk=refresh-ticket", retry_request.get_header("Cookie"))
            self.assertEqual(
                cookie_path.read_text(encoding="utf-8").strip(),
                "sid=second; rtk=refresh-ticket; csrf=csrf-second",
            )
            self.assertEqual(csrf_path.read_text(encoding="utf-8").strip(), "csrf-second")

    def test_failed_refresh_closes_the_http_error_response(self) -> None:
        closed = {"value": False}

        class RefreshError(HTTPError):
            def close(self) -> None:
                closed["value"] = True
                super().close()

        class RefreshOpener:
            def open(self, request, timeout: float):
                raise RefreshError(request.full_url, 503, "unavailable", {}, BytesIO())

        client = WpsDriveClient(
            WpsClientConfig(
                group_id="group-1",
                cookie="rtk=refresh; csrf=csrf",
                credential_source=FileCredentialSource(),
            ),
            opener=RefreshOpener(),
        )

        self.assertFalse(client._refresh_wps_session())
        self.assertTrue(closed["value"])


class CurlProbeTests(unittest.TestCase):
    def test_parse_curl_extracts_cookie_without_printing_it(self) -> None:
        parse_curl = CURL_PROBE["_parse_curl"]
        url, method, headers = parse_curl(
            "curl 'https://365.kdocs.cn/3rd/drive/api/v5/groups/g/files?parentid=f' "
            "-H 'accept: */*' -H 'cookie: session-secret'"
        )
        self.assertEqual(method, "GET")
        self.assertIn("/files?parentid=f", url)
        self.assertEqual(headers["cookie"], "session-secret")

    def test_probe_urls_cannot_send_cookies_outside_wps(self) -> None:
        with self.assertRaisesRegex(CURL_PROBE["CurlProbeError"], "WPS domain"):
            CURL_PROBE["_validated_url"]("https://attacker.example/files")
        with self.assertRaisesRegex(HAR_PROBE["ProbeError"], "WPS domain"):
            HAR_PROBE["_validated_url"]("https://attacker.example/download", object_storage=True)


class HarTests(unittest.TestCase):
    def test_redacts_headers_query_and_json_values_without_mutating_input(self) -> None:
        document = {
            "log": {
                "entries": [
                    {
                        "request": {
                            "method": "POST",
                            "url": "https://drive.example.test/list?parent_id=folder-1&token=secret",
                            "headers": [
                                {"name": "Authorization", "value": "Bearer secret"},
                                {"name": "Content-Type", "value": "application/json"},
                            ],
                            "postData": {
                                "mimeType": "application/json",
                                "text": '{"parent_id":"folder-1","access_token":"secret"}',
                            },
                        },
                        "response": {
                            "status": 200,
                            "content": {
                                "mimeType": "application/json",
                                "text": '{"items":[{"id":"file-1"}],"refresh_token":"secret"}',
                            },
                        },
                    }
                ]
            }
        }

        redacted = redact_har(document)
        entry = redacted["log"]["entries"][0]
        self.assertIn("token=%3Credacted%3E", entry["request"]["url"])
        self.assertEqual(entry["request"]["headers"][0]["value"], REDACTED)
        self.assertNotIn("secret", entry["request"]["postData"]["text"])
        self.assertNotIn("secret", entry["response"]["content"]["text"])
        self.assertEqual(document["log"]["entries"][0]["request"]["headers"][0]["value"], "Bearer secret")

    def test_redacts_signed_urls_and_object_ids_inside_json(self) -> None:
        document = {
            "log": {
                "entries": [
                    {
                        "request": {"url": "https://365.kdocs.cn/list?parentid=test-parent-id"},
                        "response": {
                            "content": {
                                "mimeType": "application/json",
                                "text": (
                                    '{"download_url":"https://hwc-bj.ag.kdocs.cn/object/secret?'
                                    'AccessKeyId=secret&Signature=secret",'
                                    '"link_url":"https://www.kdocs.cn/l/private",'
                                    '"id":"test-file-id","key":"object-secret"}'
                                ),
                            }
                        },
                    }
                ]
            }
        }

        redacted = redact_har(document)
        text = redacted["log"]["entries"][0]["response"]["content"]["text"]
        self.assertNotIn("hwc-bj", text)
        self.assertNotIn("AccessKeyId", text)
        self.assertNotIn("private", text)
        self.assertNotIn("test-file-id", text)
        self.assertNotIn("object-secret", text)
        self.assertIn(REDACTED, text)

    def test_redacts_signed_url_query_credentials(self) -> None:
        value = (
            "https://hwc-bj.ag.kdocs.cn/object/private?"
            "AccessKeyId=access-secret&Policy=policy-secret&Expires=123&Signature=signature-secret"
        )

        redacted = redact_url(value)

        self.assertNotIn("access-secret", redacted)
        self.assertNotIn("policy-secret", redacted)
        self.assertNotIn("signature-secret", redacted)
        self.assertIn("AccessKeyId=%3Credacted%3E", redacted)
        self.assertIn("Policy=%3Credacted%3E", redacted)

    def test_summary_contains_shape_not_header_values(self) -> None:
        document = {
            "log": {
                "entries": [
                    {
                        "request": {
                            "method": "GET",
                            "url": "https://drive.example.test/root",
                            "headers": [{"name": "Content-Type", "value": "application/json"}],
                            "bodySize": 0,
                        },
                        "response": {
                            "status": 200,
                            "bodySize": 42,
                            "content": {"mimeType": "application/json"},
                        },
                    }
                ]
            }
        }
        summary = summarize_har(document)
        self.assertEqual(len(summary), 1)
        self.assertIn("GET", summary[0])
        self.assertIn("200", summary[0])
        self.assertIn("application/json", summary[0])

    def test_safe_details_never_contains_request_or_response_values(self) -> None:
        entry = {
            "request": {
                "method": "POST",
                "url": "https://drive.example.test/3rd/groups/123456/files?token=secret&count=20",
                "headers": [{"name": "Cookie", "value": "session-secret"}],
                "postData": {
                    "mimeType": "application/json",
                    "text": '{"parentid":123456,"csrfmiddlewaretoken":"secret"}',
                },
            },
            "response": {
                "status": 200,
                "headers": [{"name": "Set-Cookie", "value": "response-secret"}],
                "content": {
                    "mimeType": "application/json",
                    "text": '{"url":"https://hwc-bj.ag.kdocs.cn/signed-secret","result":"ok"}',
                },
            },
        }
        details = safe_entry_details(entry)
        rendered = __import__("json").dumps(details)
        self.assertNotIn("secret", rendered)
        self.assertNotIn("session-secret", rendered)
        self.assertIn("token", details["request"]["url_shape"])
        self.assertIn("Cookie", details["request"]["header_names"])
        self.assertIn("parentid", details["request"]["body"]["shape"]["keys"])
        self.assertIn("url", details["response"]["content"]["shape"]["keys"])

    def test_safe_url_shape_masks_opaque_path_ids(self) -> None:
        shaped = safe_url_shape(
            "https://drive.example.test/api/v7/drives/123456/files/ctxbAmD2KVvO/rapid_upload"
        )
        self.assertNotIn("123456", shaped)
        self.assertNotIn("ctxbAmD2KVvO", shaped)
        self.assertIn("rapid_upload", shaped)


class ProviderContractTests(unittest.TestCase):
    def test_remote_entry_is_metadata_only(self) -> None:
        entry = RemoteEntry(id="file-1", name="probe.txt", kind="file", size=4)
        self.assertEqual(entry.name, "probe.txt")
        self.assertEqual(entry.size, 4)


if __name__ == "__main__":
    unittest.main()
