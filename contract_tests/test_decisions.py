"""B003 characteristic tests for compatibility decisions D-01..D-09.

Each scenario pins the CURRENT Python service behavior as observable over
HTTP (black box). The observed values are written to results/ and recorded
in go/MIGRATION-LOG.md as the evidence for the D-01..D-09 decisions.

Scenario IDs are stable: DEC-D01-A ... DEC-D09-A. Run:

    python -m unittest discover -s contract_tests -v
"""

from __future__ import annotations

import concurrent.futures
import json
import os
import socket
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS_DIR = os.path.join(HERE, "results")
if HERE not in sys.path:
    sys.path.insert(0, HERE)

from harness import (  # noqa: E402
    Service,
    StartupFailed,
    basic_header,
    entry,
    import_cookies,
    route,
    scenario,
)

DEFAULT_LISTING = [
    entry("bench-file-1", "bench-one.txt", "file"),
    entry("bench-dir-1", "bench-folder", "folder"),
]


def _record(name: str, payload: dict) -> None:
    os.makedirs(RESULTS_DIR, exist_ok=True)
    path = os.path.join(RESULTS_DIR, f"{name}.json")
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=True)


class DecisionTests(unittest.TestCase):
    maxDiff = None


class TestD01VirtualRoot(DecisionTests):
    """D-01: fixed group/root without a workspace file, and auto without one."""

    def test_a_auto_without_workspace_file_yields_empty_root(self) -> None:
        with Service(group_id="auto", scenario_data=scenario(listing=DEFAULT_LISTING)) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/entries?path=%2F")
            payload = json.loads(body)
            _record(
                "DEC-D01-A",
                {
                    "request": "GET /api/v1/entries?path=%2F with WPS_GROUP_ID=auto, no workspace file",
                    "status": status,
                    "entry_count": len(payload["entries"]),
                },
            )
            self.assertEqual(status, 200, body)
            # Current behavior: success with an empty listing instead of an
            # explicit "workspace not configured" error.
            self.assertEqual(payload["entries"], [])

    def test_b_fixed_group_without_workspace_file_serves_single_space(self) -> None:
        with Service(group_id="bench-group", scenario_data=scenario(listing=DEFAULT_LISTING)) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/entries?path=%2F")
            payload = json.loads(body)
            _record(
                "DEC-D01-B",
                {
                    "request": "GET /api/v1/entries?path=%2F with fixed WPS_GROUP_ID, no workspace file",
                    "status": status,
                    "names": [item["name"] for item in payload["entries"]],
                },
            )
            self.assertEqual(status, 200, body)
            self.assertEqual([item["name"] for item in payload["entries"]], ["bench-one.txt", "bench-folder"])


class TestD02StatusRefresh(DecisionTests):
    """D-02: does the status path trigger the credential refresh command?"""

    def test_a_status_root_list_triggers_refresh_on_401(self) -> None:
        marker_dir = tempfile.mkdtemp(prefix="wps-d02-")
        marker = os.path.join(marker_dir, "refresh-marker")
        with Service(
            group_id="bench-group",
            scenario_data=scenario(route(r"/3rd/drive/api/v5/groups/[^/]+/files", status=401, json_body={"error": "session"})),
            refresh_script=(
                f"#!/bin/sh\n"
                f"touch {marker}\n"
                f"printf 'bench-session=rotated\\n' > \"$WPS_COOKIE_FILE\"\n"
                f"printf 'bench-csrf-rotated\\n' > \"$WPS_CSRF_TOKEN_FILE\"\n"
                "exit 0\n"
            ),
        ) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/status")
            payload = json.loads(body)
            refreshed = os.path.exists(marker)
            _record(
                "DEC-D02-A",
                {
                    "request": "GET /api/v1/status while every list returns 401",
                    "status": status,
                    "wps_status": payload.get("status"),
                    "refresh_command_ran": refreshed,
                },
            )
            self.assertEqual(status, 200, body)
            self.assertEqual(payload.get("status"), "session_expired")
            # Current behavior: the status root listing retries with the
            # refresh command; the corrected contract would forbid refresh
            # on the whole status path.
            self.assertTrue(refreshed, "refresh command did not run during status")


class TestD03BudgetMultiplication(DecisionTests):
    """D-03: with two mounted spaces, do 4 uploads proceed concurrently?"""

    def test_a_two_spaces_double_the_upload_slots(self) -> None:
        workspace = {
            "group_id": "bench-group-a",
            "root_id": "0",
            "spaces": [
                {"group_id": "bench-group-a", "root_id": "0", "name": "space-a"},
                {"group_id": "bench-group-b", "root_id": "0", "name": "space-b"},
            ],
        }
        pre_check_route = route(
            r"/3rd/drive/api/v5/files/upload/pre_check",
            barrier={"count": 4, "timeout_s": 6.0, "timeout_status": 503},
            json_body={"result": "ok"},
            key="pre_check",
        )
        with Service(
            group_id="auto",
            workspace=workspace,
            scenario_data=scenario(pre_check_route, listing=DEFAULT_LISTING),
        ) as svc:
            payload = json.dumps({"content": "bench"}).encode()

            def upload(space: str, index: int):
                return svc.request(
                    "PUT",
                    f"/api/v1/files?path=%2F{space}%2Fbench-{index}.bin",
                    body=payload,
                    headers={"Content-Type": "application/octet-stream"},
                )

            with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                futures = [pool.submit(upload, space, i) for space in ("space-a", "space-b") for i in range(2)]
                results = [future.result(timeout=30) for future in futures]
            statuses = [status for status, _headers, _body in results]
            stats = svc.upstream_stats()
            _record(
                "DEC-D03-A",
                {
                    "request": "4 concurrent REST uploads across 2 mounted spaces (default WPS_MAX_UPLOADS=2)",
                    "statuses": statuses,
                    "upstream_pre_check_arrivals": stats["served"].get("pre_check", 0),
                },
            )
            # Current behavior: each space has its own upload slots, so all
            # four uploads pass the fake pre_check barrier at the same time.
            self.assertEqual(statuses, [201, 201, 201, 201])
            self.assertEqual(stats["served"].get("pre_check", 0), 4)


class TestD04PathDecoding(DecisionTests):
    """D-04: how many times is the REST business path URL-decoded?"""

    def test_a_query_path_decoding_cases(self) -> None:
        listing = [
            entry("bench-dir-space", "a b", "folder"),
            entry("bench-file-weird", "weird%2Fname.txt", "file"),
        ]
        with Service(
            group_id="bench-group",
            scenario_data=scenario(listing=listing),
        ) as svc:
            # '+' in the encoded query value.
            status_plus, _h, body_plus = svc.request("GET", "/api/v1/metadata?path=%2Fa+b")
            # '%252F': one query decode leaves '%2F' in the business path.
            status_single, _h, body_single = svc.request("GET", "/api/v1/metadata?path=%2Fweird%252Fname.txt")
            # '%25252F': one query decode leaves '%252F'.
            status_double, _h, body_double = svc.request("GET", "/api/v1/metadata?path=%2Fweird%25252Fname.txt")
            _record(
                "DEC-D04-A",
                {
                    "case_plus_a_b": {"status": status_plus, "body": body_plus.decode()},
                    "case_weird_percent_2f": {"status": status_single, "body": body_single.decode()},
                    "case_weird_percent_252f": {"status": status_double, "body": body_double.decode()},
                },
            )
            self.assertEqual(status_plus, 200, body_plus)
            self.assertEqual(json.loads(body_plus)["entry"]["name"], "a b")
            # Current behavior: the business path is decoded a second time
            # (query decode + split_remote_path decode). '%252F' therefore
            # resolves as '/' and misses, while '%25252F' resolves to the
            # literal '%2F' entry. Both facts are recorded as D-04 evidence.
            self.assertEqual(status_single, 404, body_single)
            self.assertIn("entry not found: weird", body_single.decode())
            self.assertEqual(status_double, 200, body_double)
            self.assertEqual(json.loads(body_double)["entry"]["name"], "weird%2Fname.txt")


class TestD05BasicAuthHalfConfigured(DecisionTests):
    """D-05: half-configured Basic Auth on a public bind."""

    def test_a_username_only_file_passes_bind_check_but_all_requests_401(self) -> None:
        with Service(
            group_id="bench-group",
            username="bench-user",
            scenario_data=scenario(listing=DEFAULT_LISTING),
            extra_env={"ADAPTER_BIND": "0.0.0.0"},
        ) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/entries?path=%2F")
            _record(
                "DEC-D05-A",
                {
                    "request": "GET /api/v1/entries with only ADAPTER_USERNAME_FILE set, bind 0.0.0.0",
                    "startup": "succeeded",
                    "status_without_credentials": status,
                    "body": body.decode(),
                },
            )
            # Current behavior: the public-bind check passes because auth is
            # considered enabled, then every request is rejected with 401.
            self.assertEqual(status, 401, body)

    def test_b_complete_credentials_allow_access(self) -> None:
        with Service(
            group_id="bench-group",
            username="bench-user",
            password="bench-pass",
            scenario_data=scenario(listing=DEFAULT_LISTING),
            extra_env={"ADAPTER_BIND": "0.0.0.0"},
        ) as svc:
            status, _headers, body = svc.request(
                "GET",
                "/api/v1/entries?path=%2F",
                auth=basic_header("bench-user", "bench-pass"),
            )
            status_anon, _h, _b = svc.request("GET", "/api/v1/entries?path=%2F")
            _record(
                "DEC-D05-B",
                {
                    "status_with_credentials": status,
                    "status_without_credentials": status_anon,
                },
            )
            self.assertEqual(status, 200, body)
            self.assertEqual(status_anon, 401)

    def test_c_empty_auth_refused_on_public_bind(self) -> None:
        with self.assertRaises(StartupFailed):
            with Service(
                group_id="bench-group",
                scenario_data=scenario(listing=DEFAULT_LISTING),
                extra_env={"ADAPTER_BIND": "0.0.0.0"},
            ):
                pass
        _record(
            "DEC-D05-C",
            {"startup": "refused for 0.0.0.0 without any Basic Auth configuration"},
        )


class TestD06SessionImportAuto(DecisionTests):
    """D-06: session import workspace remap under a fixed WPS_GROUP_ID."""

    def test_a_fixed_group_without_workspace_file_is_rejected(self) -> None:
        payload = {
            "cookies": import_cookies(),
            "workspace": {"group_id": "bench-imported", "root_id": "0"},
        }
        with Service(
            group_id="bench-fixed",
            scenario_data=scenario(listing=DEFAULT_LISTING),
        ) as svc:
            status, _headers, body = svc.request(
                "POST",
                "/api/v1/session/import",
                body=json.dumps(payload).encode(),
                headers={"Content-Type": "application/json"},
            )
            _record(
                "DEC-D06-A",
                {
                    "request": "POST /api/v1/session/import with fixed WPS_GROUP_ID and no workspace file",
                    "status": status,
                    "body": body.decode(),
                },
            )
            self.assertEqual(status, 400, body)

    def test_b_fixed_group_with_existing_workspace_file_is_accepted(self) -> None:
        workspace = {"group_id": "bench-original", "root_id": "0"}
        payload = {
            "cookies": import_cookies(),
            "workspace": {"group_id": "bench-imported", "root_id": "0"},
        }
        with Service(
            group_id="bench-fixed",
            workspace=workspace,
            scenario_data=scenario(listing=DEFAULT_LISTING),
        ) as svc:
            status, _headers, body = svc.request(
                "POST",
                "/api/v1/session/import",
                body=json.dumps(payload).encode(),
                headers={"Content-Type": "application/json"},
            )
            _record(
                "DEC-D06-B",
                {
                    "request": "POST /api/v1/session/import with fixed WPS_GROUP_ID but an existing workspace file",
                    "status": status,
                    "body": body.decode(),
                },
            )
            # Current behavior: the import succeeds because the check only
            # asks whether a workspace state exists, not whether the static
            # configuration allows a remap.
            self.assertEqual(status, 200, body)
            self.assertEqual(json.loads(body).get("workspace"), "updated")


class TestD07MountNameControlChars(DecisionTests):
    """D-07: mount names with control characters during session import."""

    def test_a_import_accepts_mount_name_with_newline(self) -> None:
        workspace = {"group_id": "bench-original", "root_id": "0"}
        payload = {
            "cookies": import_cookies(),
            "workspace": {
                "group_id": "bench-imported",
                "root_id": "0",
                "spaces": [
                    {"group_id": "bench-group-c", "root_id": "0", "name": "bench-a\nb"}
                ],
            },
        }
        with Service(
            group_id="auto",
            workspace=workspace,
            scenario_data=scenario(listing=DEFAULT_LISTING),
        ) as svc:
            status, _headers, body = svc.request(
                "POST",
                "/api/v1/session/import",
                body=json.dumps(payload).encode(),
                headers={"Content-Type": "application/json"},
            )
            names = None
            if status == 200:
                status_list, _h, body_list = svc.request("GET", "/api/v1/entries?path=%2F")
                names = [item["name"] for item in json.loads(body_list)["entries"]]
            _record(
                "DEC-D07-A",
                {
                    "request": "POST /api/v1/session/import with mount name 'bench-a\\nb'",
                    "import_status": status,
                    "import_body": body.decode(),
                    "root_names_after_import": names,
                },
            )
            # Current behavior: the control character is accepted and the
            # name is served back verbatim in listings.
            self.assertEqual(status, 200, body)
            self.assertEqual(names, ["bench-a\nb"])


class TestD08PropfindFixedAttributes(DecisionTests):
    """D-08: PROPFIND ignores the request body and returns fixed attributes."""

    def test_a_requested_prop_selection_is_ignored(self) -> None:
        body = (
            '<?xml version="1.0" encoding="utf-8"?>'
            '<D:propfind xmlns:D="DAV:"><D:prop><D:displayname/></D:prop></D:propfind>'
        )
        with Service(
            group_id="bench-group",
            scenario_data=scenario(listing=DEFAULT_LISTING),
        ) as svc:
            status, headers, payload = svc.request(
                "PROPFIND",
                "/dav/bench-one.txt",
                body=body.encode(),
                headers={"Depth": "0", "Content-Type": "application/xml"},
            )
            text = payload.decode()
            _record(
                "DEC-D08-A",
                {
                    "request": "PROPFIND Depth 0 requesting only displayname",
                    "status": status,
                    "content_type": headers.get("Content-Type"),
                    "response_contains_getlastmodified": "getlastmodified" in text,
                    "response_contains_getetag": "getetag" in text,
                    "response_contains_displayname": "displayname" in text,
                },
            )
            self.assertEqual(status, 207, payload)
            # Current behavior: the full fixed attribute set is returned even
            # though only displayname was selected.
            self.assertIn("getlastmodified", text)
            self.assertIn("getetag", text)


class TestD09ConnectionLimit(DecisionTests):
    """D-09: connections above the limit are closed without an HTTP response."""

    def test_a_extra_connection_is_closed_without_503(self) -> None:
        list_route = route(
            r"/3rd/drive/api/v5/groups/[^/]+/files",
            delay_ms=2500,
            json_body={"files": DEFAULT_LISTING, "result": "ok"},
        )
        with Service(
            group_id="bench-group",
            max_connections=2,
            scenario_data=scenario(list_route, listing=DEFAULT_LISTING),
        ) as svc:
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                held = [
                    pool.submit(svc.request, "GET", "/api/v1/entries?path=%2F")
                    for _ in range(2)
                ]
                # Wait until both requests reached the fake upstream (their
                # connection slots are therefore held by the server).
                import time as _time

                deadline = _time.monotonic() + 5.0
                while _time.monotonic() < deadline:
                    _recorded = len(svc.upstream_records())
                    if _recorded >= 2:
                        break
                    _time.sleep(0.05)

                probe = svc.raw_connect()
                try:
                    probe.sendall(b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
                    try:
                        received = probe.recv(1024)
                    except ConnectionResetError:
                        received = b""
                finally:
                    probe.close()
                responses = [future.result(timeout=30) for future in held]
            _record(
                "DEC-D09-A",
                {
                    "request": "third TCP connection while max_connections=2 requests are held",
                    "probe_received": repr(received),
                    "held_request_statuses": [status for status, _h, _b in responses],
                },
            )
            # Current behavior: the extra connection is closed at accept time
            # without any HTTP status (no 503).
            self.assertNotIn(b"HTTP/", received)
            self.assertEqual([status for status, _h, _b in responses], [200, 200])


if __name__ == "__main__":
    unittest.main()
