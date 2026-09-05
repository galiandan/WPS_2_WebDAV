"""B103: WebDAV contract tests (black box).

Scenario IDs: DAV-<METHOD>-###. Observed Python behavior is recorded under
results/ as the baseline for the Go service. Range/If-Range download details
belong to the download stage (B802) and are only pinned superficially here.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import sys
import unittest
import xml.etree.ElementTree as ElementTree

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS_DIR = os.path.join(HERE, "results")
if HERE not in sys.path:
    sys.path.insert(0, HERE)

from harness import Service, entry, route, scenario  # noqa: E402

USER = "bench-user"
PASS = "bench-pass"
AUTH = {
    "Authorization": "Basic "
    + base64.b64encode(f"{USER}:{PASS}".encode()).decode()
}

DEFAULT_LISTING = [
    entry("bench-file-1", "bench-one.txt", "file"),
    entry("bench-file-2", "bench-two.txt", "file"),
    entry("bench-dir-1", "bench-folder", "folder"),
]


def _record(name: str, payload: dict) -> None:
    os.makedirs(RESULTS_DIR, exist_ok=True)
    with open(os.path.join(RESULTS_DIR, f"{name}.json"), "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=True, sort_keys=True)


def _localname(tag: str) -> str:
    return tag.rsplit("}", 1)[-1]


def _propfind_props(xml: bytes) -> list[dict]:
    """Flatten a multistatus body into [{href, props:{...}, collection: bool}]."""

    root = ElementTree.fromstring(xml)
    parsed = []
    for response in root.iter():
        if _localname(response.tag) != "response":
            continue
        item: dict = {"href": None, "props": {}, "collection": False}
        for element in response.iter():
            name = _localname(element.tag)
            if name == "href" and item["href"] is None:
                item["href"] = (element.text or "").strip()
            elif name == "resourcetype":
                item["collection"] = any(_localname(c.tag) == "collection" for c in element)
            elif name in {
                "displayname",
                "getcontentlength",
                "getcontenttype",
                "getetag",
                "getlastmodified",
            }:
                item["props"][name] = element.text
        parsed.append(item)
    return parsed


def _lock_token_of(body: bytes) -> str:
    root = ElementTree.fromstring(body)
    for element in root.iter():
        if _localname(element.tag) == "href" and (element.text or "").startswith("opaquelocktoken:"):
            return element.text.strip()
    raise AssertionError("no lock token in response")


class DavContractTests(unittest.TestCase):
    def service(self, **kwargs) -> Service:
        kwargs.setdefault("group_id", "bench-group")
        kwargs.setdefault("username", USER)
        kwargs.setdefault("password", PASS)
        kwargs.setdefault("scenario_data", scenario(listing=DEFAULT_LISTING))
        return Service(**kwargs)


class TestDavOptions(DavContractTests):
    def test_001_options_capabilities_are_fixed(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request("OPTIONS", "/dav/", auth=AUTH)
            _record(
                "DAV-OPTIONS-001",
                {
                    "status": status,
                    "dav": headers.get("DAV"),
                    "allow": headers.get("Allow"),
                    "body": body.decode(),
                },
            )
            self.assertEqual(status, 200)
            self.assertEqual(headers.get("DAV"), "1,2")
            self.assertEqual(
                headers.get("Allow"),
                "OPTIONS, PROPFIND, GET, HEAD, PUT, MKCOL, DELETE, MOVE, "
                "COPY, LOCK, UNLOCK",
            )
            self.assertEqual(body, b"")

    def test_002_options_answers_outside_dav_prefix(self) -> None:
        with self.service() as svc:
            status, headers, _body = svc.request("OPTIONS", "/not-a-route", auth=AUTH)
            _record(
                "DAV-OPTIONS-002",
                {"status": status, "dav": headers.get("DAV"), "allow": bool(headers.get("Allow"))},
            )
            self.assertEqual(status, 200)
            self.assertEqual(headers.get("DAV"), "1,2")


class TestDavPropfind(DavContractTests):
    def test_001_depth0_on_file_returns_one_response(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request(
                "PROPFIND", "/dav/bench-one.txt", headers={"Depth": "0"}, auth=AUTH
            )
            entries = _propfind_props(body)
            _record(
                "DAV-PROPFIND-001",
                {"status": status, "content_type": headers.get("Content-Type"), "entries": entries},
            )
            self.assertEqual(status, 207)
            self.assertEqual(len(entries), 1)
            self.assertEqual(entries[0]["href"], "/dav/bench-one.txt")
            self.assertFalse(entries[0]["collection"])
            self.assertEqual(entries[0]["props"]["displayname"], "bench-one.txt")
            self.assertEqual(entries[0]["props"]["getcontentlength"], "11")
            self.assertEqual(entries[0]["props"]["getcontenttype"], "text/plain")
            self.assertEqual(entries[0]["props"]["getetag"], '"bench-etag-bench-file-1"')
            self.assertIn("GMT", entries[0]["props"]["getlastmodified"] or "")

    def test_002_depth1_on_root_lists_children(self) -> None:
        with self.service() as svc:
            status, _headers, body = svc.request("PROPFIND", "/dav/", headers={"Depth": "1"}, auth=AUTH)
            entries = _propfind_props(body)
            _record("DAV-PROPFIND-002", {"status": status, "hrefs": [e["href"] for e in entries]})
            self.assertEqual(status, 207)
            hrefs = {e["href"] for e in entries}
            self.assertEqual(
                hrefs,
                {"/dav/", "/dav/bench-one.txt", "/dav/bench-two.txt", "/dav/bench-folder/"},
            )
            root = next(e for e in entries if e["href"] == "/dav/")
            self.assertTrue(root["collection"])

    def test_003_depth1_on_folder_lists_one_level(self) -> None:
        children = [entry("bench-dir-1-c1", "child.txt", "file", parent="bench-dir-1")]
        with self.service(scenario_data=scenario(listing=DEFAULT_LISTING, children={"bench-dir-1": children})) as svc:
            status, _headers, body = svc.request(
                "PROPFIND", "/dav/bench-folder/", headers={"Depth": "1"}, auth=AUTH
            )
            entries = _propfind_props(body)
            _record("DAV-PROPFIND-003", {"status": status, "hrefs": [e["href"] for e in entries]})
            self.assertEqual(status, 207)
            self.assertEqual(
                {e["href"] for e in entries},
                {"/dav/bench-folder/", "/dav/bench-folder/child.txt"},
            )

    def test_004_depth_infinity_walks_the_tree(self) -> None:
        children = {
            "bench-dir-1": [entry("bench-dir-1-c1", "child.txt", "file", parent="bench-dir-1")],
        }
        with self.service(scenario_data=scenario(listing=DEFAULT_LISTING, children=children)) as svc:
            status, _headers, body = svc.request(
                "PROPFIND", "/dav/", headers={"Depth": "infinity"}, auth=AUTH
            )
            entries = _propfind_props(body)
            _record("DAV-PROPFIND-004", {"status": status, "hrefs": [e["href"] for e in entries]})
            self.assertEqual(status, 207)
            self.assertEqual(len(entries), 5)
            self.assertIn("/dav/bench-folder/child.txt", {e["href"] for e in entries})

    def test_005_missing_depth_defaults_to_1(self) -> None:
        with self.service() as svc:
            status, _headers, body = svc.request("PROPFIND", "/dav/", auth=AUTH)
            entries = _propfind_props(body)
            _record("DAV-PROPFIND-005", {"status": status, "href_count": len(entries)})
            self.assertEqual(status, 207)
            self.assertEqual(len(entries), 4)

    def test_006_depth_values_and_case(self) -> None:
        with self.service() as svc:
            status_bad, _h, body_bad = svc.request("PROPFIND", "/dav/", headers={"Depth": "2"}, auth=AUTH)
            status_upper, _h2, body_upper = svc.request(
                "PROPFIND", "/dav/bench-one.txt", headers={"Depth": "INFINITY"}, auth=AUTH
            )
            _record(
                "DAV-PROPFIND-006",
                {
                    "depth_2_status": status_bad,
                    "depth_2_body": body_bad.decode(),
                    "depth_infinity_uppercase_status": status_upper,
                },
            )
            self.assertEqual(status_bad, 400)
            self.assertIn("Depth must be 0, 1 or infinity", body_bad.decode())
            # Valid Depth values are matched case-insensitively.
            self.assertEqual(status_upper, 207)

    def test_007_propfind_outside_prefix_404(self) -> None:
        with self.service() as svc:
            status, _headers, body = svc.request("PROPFIND", "/", auth=AUTH)
            _record("DAV-PROPFIND-007", {"status": status, "body": body.decode()})
            self.assertEqual(status, 404)

    def test_008_propfind_missing_entry_404(self) -> None:
        with self.service() as svc:
            status, _headers, body = svc.request(
                "PROPFIND", "/dav/missing.txt", headers={"Depth": "0"}, auth=AUTH
            )
            _record("DAV-PROPFIND-008", {"status": status, "body": body.decode()})
            self.assertEqual(status, 404)

    def test_009_xml_escaping_and_encoding(self) -> None:
        tricky = 'a&b<c>"d\'.txt'
        listing = [entry("bench-tricky", tricky, "file")]
        with self.service(scenario_data=scenario(listing=listing)) as svc:
            status, _headers, body = svc.request("PROPFIND", "/dav/", headers={"Depth": "1"}, auth=AUTH)
            entries = _propfind_props(body)
            _record(
                "DAV-PROPFIND-009",
                {"status": status, "entries": entries, "raw_has_amp_entity": b"&amp;" in body},
            )
            self.assertEqual(status, 207)
            child = next(e for e in entries if e["href"] != "/dav/")
            self.assertEqual(child["props"]["displayname"], tricky)
            self.assertTrue(child["href"].startswith("/dav/a"))
            self.assertNotIn(b'{"', body)


class TestDavGetHead(DavContractTests):
    def test_001_get_file_headers_and_body(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request("GET", "/dav/bench-one.txt", auth=AUTH)
            _record(
                "DAV-GET-001",
                {
                    "status": status,
                    "content_type": headers.get("Content-Type"),
                    "content_length": headers.get("Content-Length"),
                    "etag": headers.get("ETag"),
                    "accept_ranges": headers.get("Accept-Ranges"),
                    "cache_control": headers.get("Cache-Control"),
                    "connection": headers.get("Connection"),
                    "body": body.decode(),
                },
            )
            self.assertEqual(status, 200)
            self.assertEqual(body, b"bench-bytes")
            self.assertEqual(headers.get("Content-Type"), "text/plain")
            self.assertEqual(headers.get("Content-Length"), "11")
            self.assertEqual(headers.get("ETag"), '"bench-etag-bench-file-1"')
            self.assertEqual(headers.get("Accept-Ranges"), "bytes")

    def test_002_get_directory_conflicts(self) -> None:
        with self.service() as svc:
            status, _headers, body = svc.request("GET", "/dav/", auth=AUTH)
            _record("DAV-GET-002", {"status": status, "body": body.decode()})
            self.assertEqual(status, 409)

    def test_003_get_missing_404(self) -> None:
        with self.service() as svc:
            status, _headers, _body = svc.request("GET", "/dav/missing.txt", auth=AUTH)
            _record("DAV-GET-003", {"status": status})
            self.assertEqual(status, 404)

    def test_004_head_file_has_length_without_body(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request("HEAD", "/dav/bench-one.txt", auth=AUTH)
            _record(
                "DAV-HEAD-001",
                {
                    "status": status,
                    "content_length": headers.get("Content-Length"),
                    "etag": headers.get("ETag"),
                    "body_bytes": len(body),
                },
            )
            self.assertEqual(status, 200)
            self.assertEqual(headers.get("Content-Length"), "11")
            self.assertEqual(body, b"")

    def test_005_head_folder_is_zero_length_directory(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request("HEAD", "/dav/", auth=AUTH)
            _record(
                "DAV-HEAD-002",
                {
                    "status": status,
                    "content_type": headers.get("Content-Type"),
                    "content_length": headers.get("Content-Length"),
                    "body_bytes": len(body),
                },
            )
            self.assertEqual(status, 200)
            self.assertEqual(headers.get("Content-Type"), "httpd/unix-directory")
            self.assertEqual(headers.get("Content-Length"), "0")
            self.assertEqual(body, b"")


class TestDavWrite(DavContractTests):
    def test_001_put_creates_with_location(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request(
                "PUT",
                "/dav/bench-dav-upload.bin",
                body=b"dav-content",
                headers={"Content-Type": "application/octet-stream"},
                auth=AUTH,
            )
            payload = json.loads(body)
            _record(
                "DAV-PUT-001",
                {"status": status, "location": headers.get("Location"), "payload": payload},
            )
            self.assertEqual(status, 201)
            self.assertEqual(headers.get("Location"), "/dav/bench-dav-upload.bin")
            self.assertEqual(set(payload), {"id", "name", "kind", "parent_id", "size", "modified_at", "etag"})

    def test_002_put_defaults_to_overwrite(self) -> None:
        routes = [
            route(r"/3rd/drive/api/v5/files/upload/pre_check", status=403, json_body={"error": "duplicate"}),
        ]
        with self.service(scenario_data=scenario(routes, listing=DEFAULT_LISTING)) as svc:
            status, _headers, _body = svc.request(
                "PUT",
                "/dav/bench-one.txt",
                body=b"dav-overwrite",
                headers={"Content-Type": "application/octet-stream"},
                auth=AUTH,
            )
            _record("DAV-PUT-002", {"status": status})
            # DAV PUT defaults to overwrite and continues past the observed
            # upstream 403 pre-check (REST would need overwrite=true).
            self.assertEqual(status, 201)

    def test_003_mkcol_creates_and_conflicts(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request("MKCOL", "/dav/created-dav/", auth=AUTH)
            status_conflict, _h2, body_conflict = svc.request("MKCOL", "/dav/bench-folder/", auth=AUTH)
            _record(
                "DAV-MKCOL-001",
                {
                    "status": status,
                    "location": headers.get("Location"),
                    "payload": json.loads(body),
                    "conflict_status": status_conflict,
                    "conflict_body": body_conflict.decode(),
                },
            )
            self.assertEqual(status, 201)
            self.assertEqual(headers.get("Location"), "/dav/created-dav/")
            self.assertEqual(status_conflict, 409)

    def test_004_delete_file_and_root(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request("DELETE", "/dav/bench-one.txt", auth=AUTH)
            status_root, _h2, body_root = svc.request("DELETE", "/dav/", auth=AUTH)
            _record(
                "DAV-DELETE-001",
                {
                    "status": status,
                    "content_length": headers.get("Content-Length"),
                    "body": body.decode(),
                    "root_status": status_root,
                    "root_body": body_root.decode(),
                },
            )
            self.assertEqual(status, 204)
            self.assertEqual(body, b"")
            self.assertEqual(status_root, 400)
            self.assertIn("the root cannot be deleted", body_root.decode())

    def test_005_move_to_destination_folder(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request(
                "MOVE",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-folder/bench-one.txt"},
                auth=AUTH,
            )
            _record(
                "DAV-MOVE-001",
                {"status": status, "location": headers.get("Location"), "body": body.decode()},
            )
            self.assertEqual(status, 201)
            self.assertEqual(headers.get("Location"), "/dav/bench-folder/bench-one.txt")

    def test_006_move_rename_same_folder(self) -> None:
        with self.service() as svc:
            status, headers, _body = svc.request(
                "MOVE",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-renamed.txt"},
                auth=AUTH,
            )
            _record("DAV-MOVE-002", {"status": status, "location": headers.get("Location")})
            self.assertEqual(status, 201)
            self.assertEqual(headers.get("Location"), "/dav/bench-renamed.txt")

    def test_007_destination_header_validation(self) -> None:
        with self.service() as svc:
            cases = {
                "missing": {},
                "query": {"Destination": "/dav/x?query=1"},
                "fragment": {"Destination": "/dav/x#frag"},
                "userinfo": {"Destination": "https://user:pw@127.0.0.1/dav/x"},
                "other_host": {"Destination": "https://evil.example/dav/x"},
                "other_port": {"Destination": f"http://127.0.0.1:{svc.port + 1}/dav/x"},
                "outside_prefix": {"Destination": "/api/v1/x"},
            }
            observed = {}
            for label, headers in cases.items():
                status, _h, body = svc.request("MOVE", "/dav/bench-one.txt", headers=headers, auth=AUTH)
                observed[label] = {"status": status, "body": body.decode()}
                self.assertEqual(status, 400, (label, body))
            # Same host and explicit port is accepted (absolute URL form).
            status_ok, _h2, _b2 = svc.request(
                "MOVE",
                "/dav/bench-one.txt",
                headers={"Destination": f"http://127.0.0.1:{svc.port}/dav/bench-renamed.txt"},
                auth=AUTH,
            )
            observed["absolute_same_host"] = {"status": status_ok}
            self.assertEqual(status_ok, 201)
            _record("DAV-MOVE-003", observed)

    def test_008_move_overwrite_semantics(self) -> None:
        with self.service() as svc:
            status_default, _h, body_default = svc.request(
                "MOVE",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-two.txt"},
                auth=AUTH,
            )
            status_f, _h2, body_f = svc.request(
                "MOVE",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-two.txt", "Overwrite": "F"},
                auth=AUTH,
            )
            status_bad, _h3, body_bad = svc.request(
                "MOVE",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-renamed.txt", "Overwrite": "X"},
                auth=AUTH,
            )
            _record(
                "DAV-MOVE-004",
                {
                    "default_overwrite_status": status_default,
                    "default_body": body_default.decode(),
                    "overwrite_f_status": status_f,
                    "overwrite_f_body": body_f.decode(),
                    "overwrite_x_status": status_bad,
                },
            )
            # Destination exists: default T is 501 (non-atomic), F is 412.
            self.assertEqual(status_default, 501)
            self.assertEqual(status_f, 412)
            self.assertEqual(status_bad, 400)

    def test_009_move_to_same_path_is_allowed(self) -> None:
        with self.service() as svc:
            status, headers, _body = svc.request(
                "MOVE",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-one.txt"},
                auth=AUTH,
            )
            _record("DAV-MOVE-005", {"status": status, "location": headers.get("Location")})
            self.assertEqual(status, 201)

    def test_010_copy_file_via_relay(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request(
                "COPY",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-copy.txt"},
                auth=AUTH,
            )
            stats = svc.upstream_stats()
            _record(
                "DAV-COPY-001",
                {
                    "status": status,
                    "location": headers.get("Location"),
                    "body": body.decode(),
                    "object_put_sha256": stats["object_put_sha256"],
                    "object_get_bytes": stats["served"],
                },
            )
            self.assertEqual(status, 201)
            self.assertEqual(headers.get("Location"), "/dav/bench-copy.txt")
            # Relay copy: the copied bytes equal the served object bytes.
            self.assertEqual(
                stats["object_put_sha256"],
                hashlib.sha256(b"bench-bytes").hexdigest(),
            )

    def test_011_copy_overwrite_semantics(self) -> None:
        with self.service() as svc:
            status_default, _h, body_default = svc.request(
                "COPY",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-two.txt"},
                auth=AUTH,
            )
            status_f, _h2, body_f = svc.request(
                "COPY",
                "/dav/bench-one.txt",
                headers={"Destination": "/dav/bench-two.txt", "Overwrite": "F"},
                auth=AUTH,
            )
            _record(
                "DAV-COPY-002",
                {
                    "default_status": status_default,
                    "default_body": body_default.decode(),
                    "overwrite_f_status": status_f,
                    "overwrite_f_body": body_f.decode(),
                },
            )
            self.assertEqual(status_default, 501)
            self.assertEqual(status_f, 412)

    def test_012_copy_folder_depth0_and_depth1(self) -> None:
        children = {
            "bench-dir-1": [
                entry("bench-dir-1-c1", "child.txt", "file", parent="bench-dir-1"),
            ],
        }
        with self.service(scenario_data=scenario(listing=DEFAULT_LISTING, children=children)) as svc:
            status_d0, _h, _b = svc.request(
                "COPY",
                "/dav/bench-folder/",
                headers={"Destination": "/dav/copy-d0/", "Depth": "0"},
                auth=AUTH,
            )
            status_d1, _h2, _b2 = svc.request(
                "COPY",
                "/dav/bench-folder/",
                headers={"Destination": "/dav/copy-d1/", "Depth": "1"},
                auth=AUTH,
            )
            object_puts = svc.upstream_stats()["served"].get("object:GET", 0)
            _record(
                "DAV-COPY-003",
                {"depth0_status": status_d0, "depth1_status": status_d1, "object_gets": object_puts},
            )
            self.assertEqual(status_d0, 201)
            self.assertEqual(status_d1, 201)
            self.assertEqual(object_puts, 1)


class TestDavLock(DavContractTests):
    def test_001_new_lock_on_existing_resource_200(self) -> None:
        with self.service() as svc:
            status, headers, body = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:owner>bench owner</D:owner></D:lockinfo>',
                headers={"Content-Type": "application/xml"},
                auth=AUTH,
            )
            token = _lock_token_of(body)
            _record(
                "DAV-LOCK-001",
                {"status": status, "lock_token_header": headers.get("Lock-Token"), "body": body.decode()},
            )
            self.assertEqual(status, 200)
            self.assertEqual(headers.get("Lock-Token"), f"<{token}>")
            self.assertIn("exclusive", body.decode())
            self.assertTrue(token.startswith("opaquelocktoken:"))

    def test_002_lock_on_missing_resource_201(self) -> None:
        with self.service() as svc:
            status, _headers, _body = svc.request(
                "LOCK",
                "/dav/lock-null.txt",
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            _record("DAV-LOCK-002", {"status": status})
            self.assertEqual(status, 201)

    def test_003_refresh_with_if_token_keeps_token(self) -> None:
        with self.service() as svc:
            status1, _h1, body1 = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            token = _lock_token_of(body1)
            status2, headers2, body2 = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                headers={"If": f"<{token}>"},
                body=b"",
                auth=AUTH,
            )
            _record(
                "DAV-LOCK-003",
                {
                    "new_status": status1,
                    "refresh_status": status2,
                    "same_token": _lock_token_of(body2) == token,
                },
            )
            self.assertEqual(status2, 200)
            self.assertEqual(_lock_token_of(body2), token)

    def test_004_root_infinity_lock_blocks_descendants(self) -> None:
        """Depth-infinity locks cover descendants; siblings do not conflict."""

        with self.service() as svc:
            status_file1, _h0, body1 = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            token1 = _lock_token_of(body1)
            status_sibling, _h1, _b1 = svc.request(
                "LOCK",
                "/dav/bench-two.txt",
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            # Locking an ancestor while a descendant lock exists conflicts.
            status_root, _h2, body_root = svc.request(
                "LOCK",
                "/dav/",
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            svc.request("UNLOCK", "/dav/bench-one.txt", headers={"Lock-Token": f"<{token1}>"}, auth=AUTH)
            # A folder lock covers its descendants' writes.
            status_folder, _h3, body3 = svc.request(
                "LOCK",
                "/dav/bench-folder/",
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            token3 = _lock_token_of(body3)
            status_put, _h4, _b4 = svc.request(
                "PUT",
                "/dav/bench-folder/inner.bin",
                body=b"x",
                headers={"Content-Type": "application/octet-stream"},
                auth=AUTH,
            )
            status_put_token, _h5, _b5 = svc.request(
                "PUT",
                "/dav/bench-folder/inner.bin",
                body=b"x",
                headers={"Content-Type": "application/octet-stream", "If": f"<{token3}>"},
                auth=AUTH,
            )
            _record(
                "DAV-LOCK-004",
                {
                    "first_file_lock": status_file1,
                    "sibling_lock_status": status_sibling,
                    "ancestor_lock_status": status_root,
                    "ancestor_lock_body": body_root.decode(),
                    "folder_lock_status": status_folder,
                    "put_without_token": status_put,
                    "put_with_folder_token": status_put_token,
                },
            )
            # File-level siblings are independent: no conflict (200 = locked
            # existing resource).
            self.assertEqual(status_file1, 200)
            self.assertEqual(status_sibling, 200)
            # A lock covering an already-locked descendant conflicts.
            self.assertEqual(status_root, 423)
            self.assertEqual(status_folder, 200)
            self.assertEqual(status_put, 423)
            self.assertEqual(status_put_token, 201)

    def test_005_unlock_requires_matching_token(self) -> None:
        with self.service() as svc:
            status1, _h1, body1 = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            token = _lock_token_of(body1)
            status_bad, _h2, body_bad = svc.request(
                "UNLOCK",
                "/dav/bench-one.txt",
                headers={"Lock-Token": "<opaquelocktoken:wrong>"},
                auth=AUTH,
            )
            status_none, _h3, body_none = svc.request("UNLOCK", "/dav/bench-one.txt", auth=AUTH)
            status_ok, _h4, body_ok = svc.request(
                "UNLOCK",
                "/dav/bench-one.txt",
                headers={"Lock-Token": f"<{token}>"},
                auth=AUTH,
            )
            status_after, _h5, _b5 = svc.request(
                "PUT",
                "/dav/bench-one.txt",
                body=b"after-unlock",
                headers={"Content-Type": "application/octet-stream"},
                auth=AUTH,
            )
            _record(
                "DAV-LOCK-005",
                {
                    "lock_status": status1,
                    "unlock_wrong_token": status_bad,
                    "unlock_wrong_body": body_bad.decode(),
                    "unlock_missing_header": status_none,
                    "unlock_ok": status_ok,
                    "put_after_unlock": status_after,
                },
            )
            self.assertEqual(status_bad, 409)
            self.assertEqual(status_none, 400)
            self.assertEqual(status_ok, 204)
            self.assertEqual(status_after, 201)

    def test_006_lock_depth_1_is_rejected(self) -> None:
        with self.service() as svc:
            status, _headers, body = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                headers={"Depth": "1"},
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            _record("DAV-LOCK-006", {"status": status, "body": body.decode()})
            self.assertEqual(status, 400)

    def test_007_lock_body_with_xml_entities_rejected(self) -> None:
        with self.service() as svc:
            doctype = (
                b'<?xml version="1.0"?><!DOCTYPE lockinfo [<!ENTITY x "y">]>'
                b'<D:lockinfo xmlns:D="DAV:"/>'
            )
            status_doctype, _h, body_doctype = svc.request("LOCK", "/dav/bench-one.txt", body=doctype, auth=AUTH)
            status_invalid, _h2, body_invalid = svc.request(
                "LOCK", "/dav/bench-one.txt", body=b"not xml", auth=AUTH
            )
            _record(
                "DAV-LOCK-007",
                {
                    "doctype_status": status_doctype,
                    "doctype_body": body_doctype.decode(),
                    "invalid_status": status_invalid,
                },
            )
            self.assertEqual(status_doctype, 400)
            self.assertEqual(status_invalid, 400)

    def test_008_timeout_header_clamped_and_validated(self) -> None:
        with self.service() as svc:
            status_infinite, _h1, body1 = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                headers={"Timeout": "Infinite"},
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            token1 = _lock_token_of(body1)
            timeout1 = int(
                next(
                    e.text
                    for e in ElementTree.fromstring(body1).iter()
                    if _localname(e.tag) == "timeout"
                ).split("-")[1]
            )
            svc.request("UNLOCK", "/dav/bench-one.txt", headers={"Lock-Token": f"<{token1}>"}, auth=AUTH)
            status_huge, _h2, body2 = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                headers={"Timeout": "Second-999999999"},
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            token2 = _lock_token_of(body2)
            timeout2 = int(
                next(
                    e.text
                    for e in ElementTree.fromstring(body2).iter()
                    if _localname(e.tag) == "timeout"
                ).split("-")[1]
            )
            svc.request("UNLOCK", "/dav/bench-one.txt", headers={"Lock-Token": f"<{token2}>"}, auth=AUTH)
            status_garbage, _h3, body3 = svc.request(
                "LOCK",
                "/dav/bench-one.txt",
                headers={"Timeout": "Second-abc"},
                body=b'<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"/>',
                auth=AUTH,
            )
            _record(
                "DAV-LOCK-008",
                {
                    "infinite_status": status_infinite,
                    "infinite_timeout": timeout1,
                    "huge_status": status_huge,
                    "huge_timeout": timeout2,
                    "garbage_status": status_garbage,
                    "garbage_body": body3.decode(),
                },
            )
            self.assertEqual(status_infinite, 200)
            self.assertIn(timeout1, (86400, 86399))
            self.assertEqual(status_huge, 200)
            self.assertIn(timeout2, (86400, 86399))
            self.assertEqual(status_garbage, 400)


class TestDavOrigin(DavContractTests):
    def test_001_cross_origin_mutation_403(self) -> None:
        with self.service() as svc:
            status_evil, _h, body = svc.request(
                "PUT",
                "/dav/bench-one.txt",
                body=b"x",
                headers={"Content-Type": "application/octet-stream", "Origin": "https://evil.example"},
                auth=AUTH,
            )
            status_ok, _h2, _b2 = svc.request(
                "PUT",
                "/dav/bench-one.txt",
                body=b"x",
                headers={"Content-Type": "application/octet-stream", "Origin": f"http://127.0.0.1:{svc.port}"},
                auth=AUTH,
            )
            status_read, _h3, _b3 = svc.request(
                "GET", "/dav/bench-one.txt", headers={"Origin": "https://evil.example"}, auth=AUTH
            )
            _record(
                "DAV-ORIGIN-001",
                {
                    "evil_origin_put": status_evil,
                    "evil_origin_body": body.decode(),
                    "same_origin_put": status_ok,
                    "cross_origin_read": status_read,
                },
            )
            self.assertEqual(status_evil, 403)
            self.assertIn("cross-origin mutation is not allowed", body.decode())
            self.assertEqual(status_ok, 201)
            self.assertEqual(status_read, 200)


if __name__ == "__main__":
    unittest.main()
