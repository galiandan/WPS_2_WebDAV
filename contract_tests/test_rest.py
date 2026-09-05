"""B102: REST contract tests (black box).

Scenario IDs: REST-<AREA>-###. Observed Python behavior is recorded under
results/ as the baseline for the Go service. See docs/api.md for the public
surface; aliases (list/upload/folder/files/delete) are pinned explicitly.
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS_DIR = os.path.join(HERE, "results")
if HERE not in sys.path:
    sys.path.insert(0, HERE)

from harness import Service, entry, route, scenario  # noqa: E402

USER = "bench-user"
PASS = "bench-pass"
AUTH = {"Authorization": "Basic " + __import__("base64").b64encode(f"{USER}:{PASS}".encode()).decode()}

DEFAULT_LISTING = [
    entry("bench-file-1", "bench-one.txt", "file"),
    entry("bench-file-2", "bench-two.txt", "file"),
    entry("bench-dir-1", "bench-folder", "folder"),
]
NULL_LISTING = [
    entry("bench-null", "nulls.txt", "file"),
]

ENTRY_FIELDS = {"id", "name", "kind", "parent_id", "size", "modified_at", "etag"}


def _record(name: str, payload: dict) -> None:
    os.makedirs(RESULTS_DIR, exist_ok=True)
    with open(os.path.join(RESULTS_DIR, f"{name}.json"), "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=True, sort_keys=True)


class RestContractTests(unittest.TestCase):
    def service(self, **kwargs) -> Service:
        kwargs.setdefault("group_id", "bench-group")
        kwargs.setdefault("username", USER)
        kwargs.setdefault("password", PASS)
        return Service(**kwargs)

    def default_scenario(self, **kwargs) -> dict:
        return scenario(listing=kwargs.pop("listing", DEFAULT_LISTING), **kwargs)


class TestRestStatus(RestContractTests):
    def test_001_status_schema_and_redaction(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, headers, body = svc.request("GET", "/api/v1/status", auth=AUTH)
            payload = json.loads(body)
            _record(
                "REST-STATUS-001",
                {"status": status, "content_type": headers.get("Content-Type"), "payload": payload},
            )
            self.assertEqual(status, 200)
            self.assertEqual(
                set(payload),
                {"status", "wps", "workspace", "account_type", "last_checked_at", "retry_after"},
            )
            self.assertEqual(payload["status"], "connected")
            self.assertEqual(payload["wps"], "connected")
            self.assertEqual(payload["workspace"], "ready")
            self.assertEqual(payload["account_type"], "business")


class TestRestSettings(RestContractTests):
    def test_001_get_returns_current_name(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/settings", auth=AUTH)
            _record("REST-SETTINGS-001", {"status": status, "payload": json.loads(body)})
            self.assertEqual(status, 200)
            self.assertEqual(set(json.loads(body)), {"status", "name"})
            self.assertEqual(json.loads(body)["status"], "ok")

    def test_002_patch_trims_and_applies_without_restart(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/settings",
                body=json.dumps({"name": "  Bench Drive  "}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-SETTINGS-002", {"status": status, "payload": json.loads(body)})
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body)["name"], "Bench Drive")
            _status2, _h, body2 = svc.request("GET", "/api/v1/settings", auth=AUTH)
            self.assertEqual(json.loads(body2)["name"], "Bench Drive")

    def test_003_patch_rejects_empty_control_and_overlong_names(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            cases = {
                "empty": {"name": "   "},
                "control_char": {"name": "bad\x01name"},
                "too_long": {"name": "x" * 257},
                "non_string": {"name": 7},
            }
            observed = {}
            for label, payload in cases.items():
                status, _headers, body = svc.request(
                    "PATCH",
                    "/api/v1/settings",
                    body=json.dumps(payload).encode(),
                    headers={"Content-Type": "application/json"},
                    auth=AUTH,
                )
                observed[label] = {"status": status, "body": body.decode()}
                self.assertEqual(status, 400, (label, body))
            _record("REST-SETTINGS-003", observed)

    def test_004_patch_requires_exactly_the_name_field(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/settings",
                body=json.dumps({"name": "ok", "extra": 1}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-SETTINGS-004", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)

    def test_005_patch_rejects_invalid_json(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/settings",
                body=b"not json",
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-SETTINGS-005", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)


class TestRestList(RestContractTests):
    def test_001_entries_schema_and_null_behavior(self) -> None:
        listing = DEFAULT_LISTING + [
            {
                "id": "bench-null",
                "fname": "nulls.txt",
                "ftype": "file",
                "fsize": "not-a-number",
            }
        ]
        with self.service(scenario_data=scenario(listing=listing)) as svc:
            status, headers, body = svc.request("GET", "/api/v1/entries?path=%2F", auth=AUTH)
            payload = json.loads(body)
            _record(
                "REST-LIST-001",
                {"status": status, "content_type": headers.get("Content-Type"), "payload": payload},
            )
            self.assertEqual(status, 200)
            self.assertEqual(set(payload), {"path", "entries"})
            self.assertEqual(payload["path"], "/")
            for item in payload["entries"]:
                self.assertEqual(set(item), ENTRY_FIELDS)
            by_name = {item["name"]: item for item in payload["entries"]}
            self.assertIsNone(by_name["nulls.txt"]["size"])
            self.assertIsNone(by_name["nulls.txt"]["modified_at"])
            self.assertIsNone(by_name["nulls.txt"]["etag"])
            self.assertIsNone(by_name["nulls.txt"]["parent_id"])
            self.assertEqual(by_name["bench-folder"]["kind"], "folder")

    def test_002_list_alias_matches_entries(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            _s1, _h, body1 = svc.request("GET", "/api/v1/entries?path=%2F", auth=AUTH)
            status2, _h2, body2 = svc.request("GET", "/api/v1/list?path=%2F", auth=AUTH)
            _record(
                "REST-LIST-002",
                {"status": status2, "same_as_entries": body1 == body2},
            )
            self.assertEqual(status2, 200)
            self.assertEqual(body1, body2)

    def test_003_entries_on_file_conflicts(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "GET", "/api/v1/entries?path=%2Fbench-one.txt", auth=AUTH
            )
            _record("REST-LIST-003", {"status": status, "body": body.decode()})
            self.assertEqual(status, 409)

    def test_004_entries_missing_path_404(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/entries?path=%2Fmissing", auth=AUTH)
            _record("REST-LIST-004", {"status": status, "body": body.decode()})
            self.assertEqual(status, 404)
            self.assertIn("entry not found", body.decode())

    def test_005_path_defaults_to_root(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/entries", auth=AUTH)
            _record("REST-LIST-005", {"status": status, "path": json.loads(body)["path"]})
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body)["path"], "/")

    def test_006_empty_path_value_400(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/entries?path=", auth=AUTH)
            _record("REST-LIST-006", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)

    def test_007_multi_value_path_400(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "GET", "/api/v1/entries?path=%2F&path=%2F", auth=AUTH
            )
            _record("REST-LIST-007", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)

    def test_008_relative_path_400(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/entries?path=abc", auth=AUTH)
            _record("REST-LIST-008", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)

    def test_009_traversal_rejected(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            cases = {
                "dotdot_first": "/api/v1/entries?path=%2F..%2Fetc",
                "dotdot_middle": "/api/v1/entries?path=%2Fbench-folder%2F..%2Fbench-one.txt",
                "dot_component": "/api/v1/entries?path=%2F.%2Fbench-one.txt",
            }
            observed = {}
            for label, target in cases.items():
                status, _headers, body = svc.request("GET", target, auth=AUTH)
                observed[label] = {"status": status, "body": body.decode()}
                self.assertEqual(status, 400, (label, body))
            _record("REST-LIST-009", observed)

    def test_010_unknown_route_404(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request("GET", "/api/v1/nope", auth=AUTH)
            _record("REST-LIST-010", {"status": status, "body": body.decode()})
            self.assertEqual(status, 404)
            self.assertEqual(json.loads(body)["error"], "unknown REST route")


class TestRestMetadataDownload(RestContractTests):
    def test_001_metadata_file_and_missing(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "GET", "/api/v1/metadata?path=%2Fbench-one.txt", auth=AUTH
            )
            payload = json.loads(body)
            _record("REST-META-001", {"status": status, "payload": payload})
            self.assertEqual(status, 200)
            self.assertEqual(set(payload["entry"]), ENTRY_FIELDS)
            self.assertEqual(payload["entry"]["name"], "bench-one.txt")
            status_missing, _h, _b = svc.request(
                "GET", "/api/v1/metadata?path=%2Fmissing.txt", auth=AUTH
            )
            self.assertEqual(status_missing, 404)

    def test_002_download_streams_object_bytes(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, headers, body = svc.request(
                "GET", "/api/v1/download?path=%2Fbench-one.txt", auth=AUTH
            )
            _record(
                "REST-DOWNLOAD-001",
                {
                    "status": status,
                    "content_type": headers.get("Content-Type"),
                    "content_disposition": headers.get("Content-Disposition"),
                    "content_length": headers.get("Content-Length"),
                    "sha256": hashlib.sha256(body).hexdigest(),
                    "body": body.decode(),
                },
            )
            self.assertEqual(status, 200)
            self.assertEqual(body, b"bench-bytes")
            self.assertTrue(headers.get("Content-Disposition", "").startswith("attachment"))
            self.assertEqual(headers.get("Content-Length"), "11")


class TestRestUpload(RestContractTests):
    def test_001_upload_success_schema_and_integrity(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            payload = b"bench-upload-content"
            status, _headers, body = svc.request(
                "PUT",
                "/api/v1/upload?path=%2Fbench-uploaded.bin",
                body=payload,
                headers={"Content-Type": "application/octet-stream"},
                auth=AUTH,
            )
            result = json.loads(body)
            stats = svc.upstream_stats()
            _record(
                "REST-UPLOAD-001",
                {
                    "status": status,
                    "payload": result,
                    "object_put_sha256": stats["object_put_sha256"],
                    "object_put_size": stats["object_put_size"],
                },
            )
            self.assertEqual(status, 201)
            self.assertEqual(set(result), {"path", "entry"})
            self.assertEqual(set(result["entry"]), ENTRY_FIELDS)
            self.assertEqual(stats["object_put_sha256"], hashlib.sha256(payload).hexdigest())
            self.assertEqual(stats["object_put_size"], len(payload))

    def test_002_upload_overwrite_semantics(self) -> None:
        routes = [
            route(r"/3rd/drive/api/v5/files/upload/pre_check", status=403, json_body={"error": "duplicate"}),
        ]
        with self.service(scenario_data=scenario(routes, listing=DEFAULT_LISTING)) as svc:
            payload = b"bench-overwrite-content"
            headers = {"Content-Type": "application/octet-stream"}
            records_before = len(svc.upstream_records())
            status_existing, _h, body_existing = svc.request(
                "PUT", "/api/v1/upload?path=%2Fbench-one.txt", body=payload, headers=headers, auth=AUTH
            )
            conflict_records = svc.upstream_records()[records_before:]
            pre_checks_before = len(svc.upstream_records())
            status_overwrite, _h, _b = svc.request(
                "PUT",
                "/api/v1/upload?path=%2Fbench-one.txt&overwrite=true",
                body=payload,
                headers=headers,
                auth=AUTH,
            )
            status_new, _h2, _b2 = svc.request(
                "PUT", "/api/v1/upload?path=%2Fbench-brand-new.bin", body=payload, headers=headers, auth=AUTH
            )
            _record(
                "REST-UPLOAD-002",
                {
                    "default_existing_name_status": status_existing,
                    "default_existing_name_body": body_existing.decode(),
                    "conflict_request_upstream_paths": [rec["path"] for rec in conflict_records],
                    "overwrite_true_status": status_overwrite,
                    "new_name_with_upstream_403_status": status_new,
                },
            )
            # An existing local entry conflicts before any pre_check call.
            self.assertEqual(status_existing, 409)
            self.assertEqual([rec["path"] for rec in conflict_records].count("/3rd/drive/api/v5/files/upload/pre_check"), 0)
            # overwrite=true continues past the observed 403 pre-check.
            self.assertEqual(status_overwrite, 201)
            # A name the adapter cannot see locally reaches pre_check; the
            # observed upstream 403 then fails the default upload.
            self.assertEqual(status_new, 502)

    def test_003_upload_bool_value_variants(self) -> None:
        routes = [
            route(r"/3rd/drive/api/v5/files/upload/pre_check", status=403, json_body={"error": "duplicate"}),
        ]
        with self.service(scenario_data=scenario(routes, listing=DEFAULT_LISTING)) as svc:
            payload = b"bench-bool-content"
            headers = {"Content-Type": "application/octet-stream"}
            observed = {}
            for value in ("1", "true", "yes", "on", "TRUE"):
                status, _h, _b = svc.request(
                    "PUT",
                    f"/api/v1/upload?path=%2Fbench-bool-{value}.bin&overwrite={value}",
                    body=payload,
                    headers=headers,
                    auth=AUTH,
                )
                observed[f"overwrite={value}"] = status
                self.assertEqual(status, 201, value)
            for value in ("0", "false", "no", "off"):
                status, _h, _b = svc.request(
                    "PUT",
                    f"/api/v1/upload?path=%2Fbench-bool-{value}.bin&overwrite={value}",
                    body=payload,
                    headers=headers,
                    auth=AUTH,
                )
                observed[f"overwrite={value}"] = status
                self.assertEqual(status, 502, value)
            status_unknown, _h, body_unknown = svc.request(
                "PUT",
                "/api/v1/upload?path=%2Fbench-bool-maybe.bin&overwrite=maybe",
                body=payload,
                headers=headers,
                auth=AUTH,
            )
            observed["overwrite=maybe"] = status_unknown
            self.assertEqual(status_unknown, 400)
            status_multi, _h, _b = svc.request(
                "PUT",
                "/api/v1/upload?path=%2Fbench-bool-multi.bin&overwrite=true&overwrite=false",
                body=payload,
                headers=headers,
                auth=AUTH,
            )
            observed["overwrite=multi"] = status_multi
            self.assertEqual(status_multi, 400)
            _record("REST-UPLOAD-003", observed)


class TestRestFolders(RestContractTests):
    def test_001_create_folder_success_and_alias(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "POST", "/api/v1/folders?path=%2Fbench-created", auth=AUTH
            )
            result = json.loads(body)
            status_alias, _h, body_alias = svc.request(
                "POST", "/api/v1/folder?path=%2Fbench-created-alias", auth=AUTH
            )
            _record(
                "REST-FOLDER-001",
                {
                    "status": status,
                    "payload": result,
                    "alias_status": status_alias,
                    "alias_name": json.loads(body_alias)["entry"]["name"],
                },
            )
            self.assertEqual(status, 201)
            self.assertEqual(set(result), {"path", "entry"})
            self.assertEqual(result["entry"]["kind"], "folder")
            self.assertEqual(result["entry"]["name"], "bench-created")
            self.assertEqual(status_alias, 201)

    def test_002_create_existing_folder_conflicts(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "POST", "/api/v1/folders?path=%2Fbench-folder", auth=AUTH
            )
            _record("REST-FOLDER-002", {"status": status, "body": body.decode()})
            self.assertEqual(status, 409)


class TestRestPatch(RestContractTests):
    def test_001_rename_by_name(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=json.dumps({"name": "bench-renamed.txt"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            result = json.loads(body)
            _record("REST-PATCH-001", {"status": status, "payload": result})
            self.assertEqual(status, 200)
            self.assertEqual(result["path"], "/bench-renamed.txt")
            self.assertEqual(result["entry"]["name"], "bench-renamed.txt")

    def test_002_rename_by_fname_alias(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=json.dumps({"fname": "bench-renamed2.txt"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-002", {"status": status, "payload": json.loads(body)})
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body)["entry"]["name"], "bench-renamed2.txt")

    def test_003_move_by_destination(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=json.dumps({"destination": "/bench-folder/bench-one.txt"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-003", {"status": status, "payload": json.loads(body)})
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body)["path"], "/bench-folder/bench-one.txt")

    def test_004_move_by_parent_path(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=json.dumps({"parent_path": "/bench-folder"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-004", {"status": status, "payload": json.loads(body)})
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body)["path"], "/bench-folder/bench-one.txt")

    def test_005_conflicting_target_fields_400(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=json.dumps({"name": "x.txt", "destination": "/bench-folder/x.txt"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-005", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)

    def test_006_empty_object_400(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=b"{}",
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-006", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)

    def test_007_rename_to_existing_name_409(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=json.dumps({"name": "bench-two.txt"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-007", {"status": status, "body": body.decode()})
            self.assertEqual(status, 409)

    def test_008_patch_missing_path_404(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fmissing.txt",
                body=json.dumps({"name": "x.txt"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-008", {"status": status, "body": body.decode()})
            self.assertEqual(status, 404)

    def test_009_same_parent_move_is_noop_without_upstream(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=json.dumps({"parent_path": "/"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            moves = [
                rec for rec in svc.upstream_records() if "task/move" in rec["path"]
            ]
            _record(
                "REST-PATCH-009",
                {"status": status, "payload": json.loads(body), "upstream_move_calls": len(moves)},
            )
            self.assertEqual(status, 200)
            self.assertEqual(moves, [])

    def test_010_move_into_itself_400(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-folder",
                body=json.dumps({"parent_path": "/bench-folder"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-010", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)

    def test_011_cross_folder_rename_move_501(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request(
                "PATCH",
                "/api/v1/entries?path=%2Fbench-one.txt",
                body=json.dumps({"destination": "/bench-folder/renamed.txt"}).encode(),
                headers={"Content-Type": "application/json"},
                auth=AUTH,
            )
            _record("REST-PATCH-011", {"status": status, "body": body.decode()})
            self.assertEqual(status, 501)


class TestRestDelete(RestContractTests):
    def test_001_delete_success_204(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, headers, body = svc.request(
                "DELETE", "/api/v1/entries?path=%2Fbench-one.txt", auth=AUTH
            )
            _record(
                "REST-DELETE-001",
                {"status": status, "content_length": headers.get("Content-Length"), "body": body.decode()},
            )
            self.assertEqual(status, 204)
            self.assertEqual(body, b"")

    def test_002_delete_aliases(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status_files, _h, _b = svc.request("DELETE", "/api/v1/files?path=%2Fbench-one.txt", auth=AUTH)
            status_delete, _h2, _b2 = svc.request("DELETE", "/api/v1/delete?path=%2Fbench-two.txt", auth=AUTH)
            _record(
                "REST-DELETE-002",
                {"files_alias_status": status_files, "delete_alias_status": status_delete},
            )
            self.assertEqual(status_files, 204)
            self.assertEqual(status_delete, 204)

    def test_003_delete_root_400(self) -> None:
        with self.service(scenario_data=self.default_scenario()) as svc:
            status, _headers, body = svc.request("DELETE", "/api/v1/entries?path=%2F", auth=AUTH)
            _record("REST-DELETE-003", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)


class TestRestErrors(RestContractTests):
    def test_001_upstream_500_maps_to_502_with_redacted_body(self) -> None:
        routes = [
            route(r"/3rd/drive/api/v5/groups/[^/]+/files", status=500, json_body={"message": "secret upstream detail"}),
        ]
        with self.service(scenario_data=scenario(routes, listing=DEFAULT_LISTING)) as svc:
            status, headers, body = svc.request("GET", "/api/v1/entries?path=%2F", auth=AUTH)
            payload = json.loads(body)
            _record(
                "REST-ERROR-001",
                {
                    "status": status,
                    "payload": payload,
                    "retry_after": headers.get("Retry-After"),
                },
            )
            self.assertEqual(status, 502)
            self.assertEqual(payload["code"], "wps_unavailable")
            self.assertEqual(payload["upstream_status"], 500)
            self.assertNotIn("secret upstream detail", body.decode())

    def test_002_upstream_401_maps_to_503_with_retry_after(self) -> None:
        routes = [
            route(r"/3rd/drive/api/v5/groups/[^/]+/files", status=401, json_body={"error": "session"}),
        ]
        with self.service(
            scenario_data=scenario(routes, listing=DEFAULT_LISTING),
            auto_refresh=False,
        ) as svc:
            status, headers, body = svc.request("GET", "/api/v1/entries?path=%2F", auth=AUTH)
            payload = json.loads(body)
            _record(
                "REST-ERROR-002",
                {
                    "status": status,
                    "payload": payload,
                    "retry_after": headers.get("Retry-After"),
                },
            )
            self.assertEqual(status, 503)
            self.assertEqual(payload["code"], "wps_session_expired")
            self.assertEqual(payload["upstream_status"], 401)
            self.assertEqual(headers.get("Retry-After"), "60")


if __name__ == "__main__":
    unittest.main()
