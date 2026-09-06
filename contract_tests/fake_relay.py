"""HTTP relay exposing the in-process FakeUpstream to the Go contract service.

The Python adapter receives the fake by patching its client constructor
(python_service.py). The Go adapter cannot be patched in-process, so its
test-only entrypoint (go/internal/contractsrv) sends every WPS-shaped
request to this relay, which replays it against the same FakeUpstream
instance. Recording, violations, and stats therefore stay byte-identical
for both services.

Wire protocol (all JSON):

    POST /open    {"method", "url", "headers": [[name, value]], "body_b64",
                   "timeout"}   -> control-plane and object GET requests
    POST /signed  {"method", "target", "host", "port", "headers",
                   "body_b64", "timeout"} -> signed object PUT/POST

    -> {"status", "headers": [[name, value]], "body_b64"}
    -> {"error": "timeout"} when the fake would time out
"""

from __future__ import annotations

import base64
import json
import socket
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError
from urllib.request import Request


class FakeRelay:
    """Replays relayed WPS requests against one FakeUpstream."""

    def __init__(self, fake, default_timeout: float = 30.0) -> None:
        self._fake = fake
        self._default_timeout = default_timeout

    def open(self, payload: dict) -> dict:
        headers: dict[str, str] = {}
        for name, value in payload.get("headers", []):
            existing = headers.get(name)
            headers[name] = f"{existing}, {value}" if existing is not None else str(value)
        body = base64.b64decode(payload.get("body_b64", ""))
        request = Request(
            payload["url"],
            data=body if body else None,
            headers=headers,
            method=payload.get("method", "GET"),
        )
        timeout = float(payload.get("timeout", self._default_timeout))
        try:
            response = self._fake.open(request, timeout)
        except HTTPError as error:
            return {
                "status": error.code,
                "headers": [[name, value] for name, value in (error.headers or {}).items()],
                "body_b64": base64.b64encode(error.read()).decode("ascii"),
            }
        except socket.timeout:
            return {"error": "timeout"}
        return {
            "status": response.status,
            "headers": [[name, value] for name, value in response.getheaders()],
            "body_b64": base64.b64encode(response.read()).decode("ascii"),
        }

    def signed(self, payload: dict) -> dict:
        connection = self._fake.signed_connection(
            payload.get("host"),
            payload.get("port"),
            float(payload.get("timeout", self._default_timeout)),
        )
        connection.putrequest(payload.get("method", "PUT"), payload.get("target", "/"))
        for name, value in payload.get("headers", []):
            connection.putheader(name, value)
        connection.endheaders()
        body = base64.b64decode(payload.get("body_b64", ""))
        if body:
            connection.send(body)
        response = connection.getresponse()
        return {
            "status": response.status,
            "headers": [[name, value] for name, value in response.getheaders()],
            "body_b64": base64.b64encode(response.read()).decode("ascii"),
        }


def serve_relay(relay: FakeRelay) -> ThreadingHTTPServer:
    """Start the relay on a random loopback port; returns the server."""

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def _dispatch(self, method_name: str, payload: dict) -> None:
            try:
                result = getattr(relay, method_name)(payload)
            except Exception as exc:  # noqa: BLE001 - the relay must answer
                result = {"error": f"{type(exc).__name__}: {exc}"}
            body = json.dumps(result).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self) -> None:  # noqa: N802 - http.server API
            # Drain the body first: keep-alive connections desync otherwise.
            length = int(self.headers.get("Content-Length", "0"))
            payload = json.loads(self.rfile.read(length)) if length else {}
            if self.path == "/open":
                self._dispatch("open", payload)
            elif self.path == "/signed":
                self._dispatch("signed", payload)
            else:
                self.send_response(404)
                self.send_header("Content-Length", "0")
                self.end_headers()

        def log_message(self, format: str, *args) -> None:  # noqa: A002
            # Relay traffic carries WPS-shaped URLs; keep it out of logs.
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server
