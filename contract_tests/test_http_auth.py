"""B101: HTTP auth and framing contract tests (black box).

Scenario IDs are stable: HTTP-HEALTH-###, HTTP-AUTH-###, HTTP-FRAMING-###.
Observed Python behavior is recorded under results/ as the comparison
baseline for the Go service.
"""

from __future__ import annotations

import base64
import json
import os
import socket
import sys
import unittest
from http.client import HTTPConnection

HERE = os.path.dirname(os.path.abspath(__file__))
RESULTS_DIR = os.path.join(HERE, "results")
if HERE not in sys.path:
    sys.path.insert(0, HERE)

from harness import Service, basic_header  # noqa: E402

USER = "bench-user"
PASS = "bench-pass"


def _record(name: str, payload: dict) -> None:
    os.makedirs(RESULTS_DIR, exist_ok=True)
    with open(os.path.join(RESULTS_DIR, f"{name}.json"), "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=True, sort_keys=True)


def _raw_exchange(
    svc: Service,
    request_bytes: bytes,
    *,
    timeout: float = 5.0,
) -> bytes:
    """Send raw bytes and read the server response until the socket closes."""

    with svc.raw_connect() as sock:
        sock.settimeout(timeout)
        sock.sendall(request_bytes)
        chunks = []
        while True:
            try:
                chunk = sock.recv(65536)
            except (socket.timeout, ConnectionResetError, OSError):
                break
            if not chunk:
                break
            chunks.append(chunk)
        return b"".join(chunks)


def _status_line(response: bytes) -> str:
    first = response.split(b"\r\n", 1)[0]
    return first.decode("latin-1")


def _header_values(response: bytes, name: str) -> list[str]:
    head = response.split(b"\r\n\r\n", 1)[0]
    values = []
    for line in head.split(b"\r\n")[1:]:
        key, _sep, value = line.partition(b":")
        if key.strip().lower() == name.encode().lower():
            values.append(value.strip().decode("latin-1"))
    return values


def _body_of(response: bytes) -> bytes:
    parts = response.split(b"\r\n\r\n", 1)
    return parts[1] if len(parts) > 1 else b""


class HttpContractTests(unittest.TestCase):
    """Shared service bootstrap for the auth group."""

    def authed_service(self, **kwargs) -> Service:
        return Service(
            group_id="bench-group",
            username=USER,
            password=PASS,
            **kwargs,
        )


class TestHealth(HttpContractTests):
    def test_001_healthz_without_credentials_is_ok_and_silent(self) -> None:
        with self.authed_service() as svc:
            status, headers, body = svc.request("GET", "/healthz")
            records = svc.upstream_records()
            _record(
                "HTTP-HEALTH-001",
                {
                    "status": status,
                    "body": json.loads(body),
                    "content_type": headers.get("Content-Type"),
                    "upstream_requests": len(records),
                },
            )
            self.assertEqual(status, 200)
            payload = json.loads(body)
            self.assertEqual(payload["status"], "ok")
            self.assertEqual(payload["network_calls"], "on-demand")
            self.assertEqual(records, [], "healthz must not touch the upstream")

    def test_002_healthz_bypasses_auth_even_with_bad_credentials(self) -> None:
        with self.authed_service() as svc:
            status, _headers, body = svc.request(
                "GET", "/healthz", headers={"Authorization": "Basic d3Jvbmc6d3Jvbmc="}
            )
            _record(
                "HTTP-HEALTH-002",
                {"status": status, "body": json.loads(body)},
            )
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body)["status"], "ok")


class TestBasicAuth(HttpContractTests):
    def test_003_missing_credentials_401_shape(self) -> None:
        with self.authed_service() as svc:
            status, headers, body = svc.request("GET", "/")
            _record(
                "HTTP-AUTH-003",
                {
                    "status": status,
                    "www_authenticate": headers.get("WWW-Authenticate"),
                    "connection": headers.get("Connection"),
                    "content_length": headers.get("Content-Length"),
                    "body": body.decode(),
                },
            )
            self.assertEqual(status, 401)
            self.assertEqual(headers.get("WWW-Authenticate"), 'Basic realm="wps-adapter"')
            self.assertEqual(headers.get("Connection"), "close")
            self.assertEqual(headers.get("Content-Length"), "0")
            self.assertEqual(body, b"")

    def test_004_wrong_password_401(self) -> None:
        with self.authed_service() as svc:
            status, _headers, _body = svc.request(
                "GET", "/", auth=basic_header(USER, "not-the-password")
            )
            _record("HTTP-AUTH-004", {"status": status})
            self.assertEqual(status, 401)

    def test_005_illegal_base64_401(self) -> None:
        with self.authed_service() as svc:
            status, _headers, _body = svc.request(
                "GET", "/", headers={"Authorization": "Basic !!!not-base64!!!"}
            )
            _record("HTTP-AUTH-005", {"status": status})
            self.assertEqual(status, 401)

    def test_006_non_utf8_credential_bytes_401(self) -> None:
        token = base64.b64encode(b"bench-user:\xff\xfe").decode()
        with self.authed_service() as svc:
            status, _headers, _body = svc.request(
                "GET", "/", headers={"Authorization": f"Basic {token}"}
            )
            _record("HTTP-AUTH-006", {"status": status})
            self.assertEqual(status, 401)

    def test_007_missing_colon_401(self) -> None:
        token = base64.b64encode(b"bench-user-no-colon").decode()
        with self.authed_service() as svc:
            status, _headers, _body = svc.request(
                "GET", "/", headers={"Authorization": f"Basic {token}"}
            )
            _record("HTTP-AUTH-007", {"status": status})
            self.assertEqual(status, 401)

    def test_008_correct_credentials_accepted(self) -> None:
        with self.authed_service() as svc:
            status, headers, body = svc.request(
                "GET", "/", auth=basic_header(USER, PASS)
            )
            _record(
                "HTTP-AUTH-008",
                {"status": status, "content_type": headers.get("Content-Type"), "body_bytes": len(body)},
            )
            self.assertEqual(status, 200)

    def test_009_basic_scheme_is_case_insensitive(self) -> None:
        token = base64.b64encode(f"{USER}:{PASS}".encode()).decode()
        with self.authed_service() as svc:
            status, _headers, _body = svc.request(
                "GET", "/", headers={"Authorization": f"basic {token}"}
            )
            _record("HTTP-AUTH-009", {"status": status})
            self.assertEqual(status, 200)

    def test_010_unknown_scheme_401(self) -> None:
        token = base64.b64encode(f"{USER}:{PASS}".encode()).decode()
        with self.authed_service() as svc:
            status, _headers, _body = svc.request(
                "GET", "/", headers={"Authorization": f"Bearer {token}"}
            )
            _record("HTTP-AUTH-010", {"status": status})
            self.assertEqual(status, 401)


class TestFraming(HttpContractTests):
    """Framing checks run before authentication, so raw sockets suffice."""

    def request_line(self, method: str = "GET", target: str = "/healthz") -> bytes:
        return f"{method} {target} HTTP/1.1\r\nHost: 127.0.0.1\r\n".encode()

    def test_001_transfer_encoding_rejected(self) -> None:
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                self.request_line() + b"Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
            )
            _record(
                "HTTP-FRAMING-001",
                {
                    "status_line": _status_line(response),
                    "body": _body_of(response).decode(),
                    "connection": _header_values(response, "Connection"),
                },
            )
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 400"), response)
            self.assertIn(b"Transfer-Encoding is not supported", response)
            self.assertEqual(_header_values(response, "Connection"), ["close"])

    def test_002_multiple_content_length_rejected(self) -> None:
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                self.request_line()
                + b"Content-Length: 0\r\nContent-Length: 0\r\n\r\n",
            )
            _record(
                "HTTP-FRAMING-002",
                {"status_line": _status_line(response), "body": _body_of(response).decode()},
            )
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 400"), response)
            self.assertIn(b"multiple Content-Length", response)

    def test_003_get_with_nonzero_body_rejected(self) -> None:
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                self.request_line() + b"Content-Length: 5\r\n\r\nhello",
            )
            _record(
                "HTTP-FRAMING-003",
                {"status_line": _status_line(response), "body": _body_of(response).decode()},
            )
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 400"), response)
            self.assertIn(b"request body is not supported for this method", response)

    def test_004_get_with_negative_content_length_rejected(self) -> None:
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                self.request_line() + b"Content-Length: -1\r\n\r\n",
            )
            _record("HTTP-FRAMING-004", {"status_line": _status_line(response)})
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 400"), response)

    def test_005_get_with_illegal_content_length_rejected(self) -> None:
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                self.request_line() + b"Content-Length: abc\r\n\r\n",
            )
            _record("HTTP-FRAMING-005", {"status_line": _status_line(response)})
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 400"), response)

    def test_006_get_with_zero_content_length_allowed(self) -> None:
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                self.request_line() + b"Content-Length: 0\r\n\r\n",
            )
            _record("HTTP-FRAMING-006", {"status_line": _status_line(response)})
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 200"), response)

    def test_011_put_without_content_length_411(self) -> None:
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                b"PUT /api/v1/files?path=%2Fbench-411.bin HTTP/1.1\r\n"
                b"Host: 127.0.0.1\r\n"
                b"Authorization: Basic "
                + base64.b64encode(f"{USER}:{PASS}".encode())
                + b"\r\n\r\n",
            )
            _record(
                "HTTP-FRAMING-011",
                {"status_line": _status_line(response), "body": _body_of(response).decode()},
            )
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 411"), response)

    def test_012_control_body_over_limit_413(self) -> None:
        limit = 1024 * 1024
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                b"POST /api/v1/folders?path=%2Fbench-413 HTTP/1.1\r\n"
                b"Host: 127.0.0.1\r\n"
                b"Authorization: Basic "
                + base64.b64encode(f"{USER}:{PASS}".encode())
                + b"\r\n"
                + f"Content-Length: {limit + 1}\r\n\r\n".encode()
                + b"x",
            )
            _record(
                "HTTP-FRAMING-012",
                {"status_line": _status_line(response), "body": _body_of(response).decode()},
            )
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 413"), response)

    def test_013_session_import_body_over_512kib_413(self) -> None:
        limit = 512 * 1024
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                b"POST /api/v1/session/import HTTP/1.1\r\n"
                b"Host: 127.0.0.1\r\n"
                b"Authorization: Basic "
                + base64.b64encode(f"{USER}:{PASS}".encode())
                + b"\r\n"
                + f"Content-Length: {limit + 1}\r\n\r\n".encode()
                + b"{}",
            )
            _record("HTTP-FRAMING-013", {"status_line": _status_line(response)})
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 413"), response)

    def test_014_lock_body_over_64kib_413(self) -> None:
        limit = 64 * 1024
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                b"LOCK /dav/bench-one.txt HTTP/1.1\r\n"
                b"Host: 127.0.0.1\r\n"
                b"Authorization: Basic "
                + base64.b64encode(f"{USER}:{PASS}".encode())
                + b"\r\n"
                + f"Content-Length: {limit + 1}\r\n\r\n".encode()
                + b"<x/>",
            )
            _record("HTTP-FRAMING-014", {"status_line": _status_line(response)})
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 413"), response)

    def test_015_short_body_detected(self) -> None:
        """Declared 100 bytes, send 10, close: the response is an error."""

        with self.authed_service() as svc:
            with svc.raw_connect() as sock:
                sock.settimeout(10)
                sock.sendall(
                    b"PUT /api/v1/files?path=%2Fbench-short.bin HTTP/1.1\r\n"
                    b"Host: 127.0.0.1\r\n"
                    b"Authorization: Basic "
                    + base64.b64encode(f"{USER}:{PASS}".encode())
                    + b"\r\n"
                    b"Content-Type: application/octet-stream\r\n"
                    b"Content-Length: 100\r\n\r\n"
                    + b"0123456789"
                )
                sock.shutdown(socket.SHUT_WR)
                chunks = []
                while True:
                    try:
                        chunk = sock.recv(65536)
                    except (socket.timeout, ConnectionResetError, OSError):
                        break
                    if not chunk:
                        break
                    chunks.append(chunk)
                response = b"".join(chunks)
            _record(
                "HTTP-FRAMING-015",
                {
                    "status_line": _status_line(response),
                    "body": _body_of(response).decode(),
                },
            )
            self.assertTrue(response.startswith(b"HTTP/1.1 4"), response)
            self.assertNotIn(b"201", response.split(b"\r\n", 1)[0])

    def test_016_keepalive_serves_two_requests(self) -> None:
        with self.authed_service() as svc:
            conn = HTTPConnection(svc.host, svc.port, timeout=10)
            statuses = []
            try:
                for _ in range(2):
                    conn.request("GET", "/healthz")
                    response = conn.getresponse()
                    response.read()
                    statuses.append(response.status)
            finally:
                conn.close()
            _record("HTTP-FRAMING-016", {"statuses": statuses})
            self.assertEqual(statuses, [200, 200])

    def test_017_401_closes_connection(self) -> None:
        with self.authed_service() as svc:
            response = _raw_exchange(
                svc,
                b"GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n",
            )
            _record(
                "HTTP-FRAMING-017",
                {
                    "status_line": _status_line(response),
                    "connection": _header_values(response, "Connection"),
                },
            )
            self.assertTrue(_status_line(response).startswith("HTTP/1.1 401"), response)
            self.assertEqual(_header_values(response, "Connection"), ["close"])


if __name__ == "__main__":
    unittest.main()
