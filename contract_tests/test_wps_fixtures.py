"""B104: WPS fixture contract tests (client level).

These fixtures pin the OBSERVED WPS control-plane request shapes and the
signed-object credential isolation. The fake upstream validates every
request (method, path, query names/values, JSON field sets and types) and
records violations; a clean run must report zero violations.

This file drives the Python client as the protocol oracle. The same fixture
module must later validate the Go client against identical expectations.
"""

from __future__ import annotations

import hashlib
import io
import json
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS_DIR = os.path.join(HERE, "results")
SRC = os.path.join(os.path.dirname(HERE), "src")
if HERE not in sys.path:
    sys.path.insert(0, HERE)
if SRC not in sys.path:
    sys.path.insert(0, SRC)

from fake_upstream import FakeUpstream  # noqa: E402
from harness import route, scenario  # noqa: E402
from wps_adapter.client import (  # noqa: E402
    FileCredentialSource,
    WpsApiError,
    WpsClientConfig,
    WpsDriveClient,
)

COOKIE = "bench-session=fixture-cookie; bench-rtk=fixture-rtk"
CSRF = "fixture-csrf-placeholder"

STORAGE_LIST_KWARGS = dict(
    linkgroup=True,
    include="acl,pic_thumbnail",
    with_link=True,
    review_pic_thumbnail=True,
    with_sharefolder_type=True,
)


def _record(name: str, payload: dict) -> None:
    os.makedirs(RESULTS_DIR, exist_ok=True)
    with open(os.path.join(RESULTS_DIR, f"{name}.json"), "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=True, sort_keys=True)


class ClientFixtureTests(unittest.TestCase):
    def make_client(self, scenario_data: dict, *, with_resume_dir: bool = False, **overrides):
        self._tmp = tempfile.TemporaryDirectory(prefix="wps-fixture-")
        self.addCleanup(self._tmp.cleanup)
        self._record_path = os.path.join(self._tmp.name, "record.jsonl")
        self._stats_path = os.path.join(self._tmp.name, "stats.json")
        self.fake = FakeUpstream(scenario_data, self._record_path, self._stats_path)
        self.cookie_path = os.path.join(self._tmp.name, "cookie")
        self.csrf_path = os.path.join(self._tmp.name, "csrf")
        for path, value in ((self.cookie_path, COOKIE), (self.csrf_path, CSRF)):
            descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                handle.write(value)
        resume_dir = None
        if with_resume_dir:
            resume_dir = os.path.join(self._tmp.name, "resume")
            os.mkdir(resume_dir, 0o700)
        refresh_command = ()
        if overrides.pop("with_refresh_command", False):
            marker = os.path.join(self._tmp.name, "refresh-marker")
            script = os.path.join(self._tmp.name, "refresh.sh")
            with open(script, "w", encoding="utf-8") as handle:
                handle.write(
                    f"#!/bin/sh\ntouch {marker}\n"
                    f"printf 'bench-session=cmd-rotated\\n' > {self.cookie_path}\n"
                    f"printf 'cmd-rotated\\n' > {self.csrf_path}\n"
                    "exit 0\n"
                )
            os.chmod(script, 0o700)
            self.refresh_marker = marker
            refresh_command = (script,)
        timeout = overrides.pop("timeout", 5.0)
        config = WpsClientConfig(
            group_id="bench-group",
            cookie_file=self.cookie_path,
            csrf_token_file=self.csrf_path,
            timeout=timeout,
            upload_resume_dir=resume_dir,
            credential_source=FileCredentialSource(
                cookie_path=self.cookie_path,
                csrf_token_path=self.csrf_path,
                refresh_command=refresh_command,
                refresh_timeout=10.0,
            ),
            **overrides,
        )
        return WpsDriveClient(config, opener=self.fake, https_connection_factory=self.fake.signed_connection)

    def assert_clean(self, case: str) -> dict:
        stats = self.fake.stats
        _record(
            case,
            {
                "credential_violations": stats["credential_violations"],
                "request_contract_violations": stats["request_contract_violations"],
                "served": stats["served"],
                "object_put_sha256": stats["object_put_sha256"],
                "part_md5s": stats["part_md5s"],
            },
        )
        self.assertEqual(stats["credential_violations"], [], "credentials leaked to the object store")
        self.assertEqual(stats["request_contract_violations"], [], "request shape drifted from the fixture")
        return stats

    def storage_list(self, client):
        """List with the exact argument shape the storage layer uses."""

        return client.list_entries("0", count=1, **STORAGE_LIST_KWARGS)


class TestNormalUploadFixture(ClientFixtureTests):
    def test_001_upload_request_shapes_and_object_isolation(self) -> None:
        payload = os.urandom(1024 * 1024)
        client = self.make_client(scenario(), upload_retries=0)
        entry = client.upload("0", "bench-fixture.bin", io.BytesIO(payload), size=len(payload))
        self.assertEqual(entry.name, "bench-fixture.bin")
        stats = self.assert_clean("WPS-FIXTURE-001")
        self.assertEqual(stats["served"].get("control:pre_check"), 1)
        self.assertEqual(stats["served"].get("control:create_update"), 1)
        self.assertEqual(stats["served"].get("control:register"), 1)
        self.assertEqual(stats["object_put_size"], len(payload))
        self.assertEqual(stats["object_put_sha256"], hashlib.sha256(payload).hexdigest())
        # The object PUT carried Content-Type/Length but never credentials.
        puts = [req for req in stats["object_requests"] if req["method"] == "PUT"]
        self.assertEqual(puts, [{"method": "PUT", "headers": ["content-length", "content-type"]}])


class TestMultipartFixture(ClientFixtureTests):
    def test_002_multipart_parts_merge_and_register(self) -> None:
        part_size = 5 * 1024 * 1024
        payload = os.urandom(part_size * 2 + 1234)
        client = self.make_client(
            scenario(),
            with_resume_dir=True,
            multipart_threshold=4 * 1024 * 1024,
            multipart_part_size=part_size,
            upload_retries=0,
        )
        entry = client.upload("0", "bench-multipart.bin", io.BytesIO(payload), size=len(payload))
        self.assertEqual(entry.name, "bench-multipart.bin")
        stats = self.assert_clean("WPS-FIXTURE-002")
        self.assertEqual(stats["served"].get("control:multipart_init"), 1)
        self.assertEqual(stats["served"].get("control:multipart_part"), 3)
        self.assertEqual(stats["served"].get("control:multipart_merge"), 1)
        self.assertEqual(stats["served"].get("control:register"), 1)
        expected_md5s = [
            hashlib.md5(payload[i : i + part_size]).hexdigest()
            for i in range(0, len(payload), part_size)
        ]
        self.assertEqual(stats["part_md5s"], expected_md5s)
        self.assertEqual(stats["part_sizes"], [part_size, part_size, len(payload) - 2 * part_size])
        # The durable checkpoint is removed after a successful registration.
        self.assertEqual(os.listdir(os.path.join(self._tmp.name, "resume")), [])


class TestDownloadFixture(ClientFixtureTests):
    def test_003_download_resolves_signed_url_without_credentials(self) -> None:
        client = self.make_client(scenario())
        sink = io.BytesIO()
        written = client.download_to("bench-file-1", sink, chunk_size=1024)
        stats = self.assert_clean("WPS-FIXTURE-003")
        self.assertEqual(written, len(b"bench-bytes"))
        gets = [req for req in stats["object_requests"] if req["method"] == "GET"]
        self.assertEqual(len(gets), 1)
        # The signed object request must not carry any WPS credentials.
        self.assertNotIn("cookie", gets[0]["headers"])
        self.assertNotIn("authorization", gets[0]["headers"])


class TestRefreshFixture(ClientFixtureTests):
    def test_004_401_rotates_via_grant_token_and_retries(self) -> None:
        routes = [
            route(r"/3rd/drive/api/v5/groups/[^/]+/files", status=401, json_body={"error": "session"}),
        ]
        client = self.make_client(scenario(*routes))
        with self.assertRaises(WpsApiError) as caught:
            self.storage_list(client)
        stats = self.assert_clean("WPS-FIXTURE-004")
        self.assertEqual(caught.exception.status, 401)
        self.assertEqual(stats["served"].get("control:grant_token"), 1)
        with open(self.cookie_path, encoding="utf-8") as handle:
            self.assertIn("rotated-bench-cookie", handle.read())

    def test_005_401_prefers_external_refresh_command(self) -> None:
        routes = [
            route(r"/3rd/drive/api/v5/groups/[^/]+/files", status=401, json_body={"error": "session"}),
        ]
        client = self.make_client(scenario(*routes), with_refresh_command=True)
        with self.assertRaises(WpsApiError) as caught:
            self.storage_list(client)
        stats = self.assert_clean("WPS-FIXTURE-005")
        self.assertEqual(caught.exception.status, 401)
        self.assertTrue(os.path.exists(self.refresh_marker), "external refresh command did not run")
        self.assertIsNone(stats["served"].get("control:grant_token"))
        with open(self.cookie_path, encoding="utf-8") as handle:
            self.assertIn("cmd-rotated", handle.read())


class TestUpstreamErrorFixture(ClientFixtureTests):
    def test_006_status_injections_map_to_redacted_errors(self) -> None:
        observed = {}
        for status in (301, 403, 404, 410, 500):
            routes = [
                route(r"/3rd/drive/api/v5/groups/[^/]+/files", status=status, json_body={"error": "x"}),
            ]
            client = self.make_client(scenario(*routes))
            with self.assertRaises(WpsApiError) as caught:
                self.storage_list(client)
            observed[str(status)] = {
                "exception_status": caught.exception.status,
                "category": caught.exception.category,
            }
        _record("WPS-FIXTURE-006", observed)
        self.assertEqual(observed["301"]["exception_status"], 301)
        self.assertEqual(observed["403"]["exception_status"], 403)
        self.assertEqual(observed["404"]["exception_status"], 404)
        self.assertEqual(observed["410"]["exception_status"], 410)
        self.assertEqual(observed["500"]["exception_status"], 500)

    def test_007_malformed_oversize_and_timeout_injections(self) -> None:
        observed = {}

        client = self.make_client(
            scenario(route(r"/3rd/drive/api/v5/groups/[^/]+/files", body="not-json"))
        )
        with self.assertRaises(WpsApiError) as caught:
            self.storage_list(client)
        observed["malformed_json"] = {"category": caught.exception.category}

        client = self.make_client(
            scenario(route(r"/3rd/drive/api/v5/groups/[^/]+/files", body="x" * 4096)),
            max_json_response_bytes=1024,
        )
        with self.assertRaises(WpsApiError) as caught:
            self.storage_list(client)
        observed["oversize_response"] = {"category": caught.exception.category}

        client = self.make_client(
            scenario(
                route(
                    r"/3rd/drive/api/v5/groups/[^/]+/files",
                    delay_ms=2000,
                    json_body={"files": [], "result": "ok"},
                )
            ),
            timeout=0.5,
        )
        with self.assertRaises(WpsApiError) as caught:
            self.storage_list(client)
        observed["timeout"] = {"category": caught.exception.category}

        _record("WPS-FIXTURE-007", observed)
        self.assertEqual(observed["malformed_json"]["category"], "invalid_response")
        self.assertEqual(observed["timeout"]["category"], "unavailable")


if __name__ == "__main__":
    unittest.main()
