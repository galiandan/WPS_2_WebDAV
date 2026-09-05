"""Parent-side harness for the language-agnostic contract tests.

Spawns the real adapter service as a subprocess against the in-process fake
upstream and talks to it exclusively over HTTP, so every scenario is black
box: no adapter internals are imported here.

The same scenarios must later run against the Go service by pointing
``Service(binary=...)`` at the Go entrypoint with the same environment
variables, secret files, and scenario JSON.

Usage:

    with Service(workspace={...}) as svc:
        status, headers, body = svc.request("GET", "/api/v1/entries?path=%2F")
"""

from __future__ import annotations

import base64
import json
import os
import queue
import socket
import subprocess
import sys
import tempfile
import threading
import time
from http.client import HTTPConnection

HERE = os.path.dirname(os.path.abspath(__file__))
PROJECT_ROOT = os.path.dirname(HERE)

COOKIE_PLACEHOLDER = "bench-session=bench-cookie-placeholder; bench-rtk=bench-rtk-placeholder"
CSRF_PLACEHOLDER = "bench-csrf-placeholder"


class StartupFailed(RuntimeError):
    """The service exited during startup; carries the exit code and stderr."""

    def __init__(self, returncode: int, stderr_tail: str) -> None:
        super().__init__(f"service exited {returncode} during startup: {stderr_tail}")
        self.returncode = returncode
        self.stderr_tail = stderr_tail


def route(
    match: str,
    *,
    method: str | None = None,
    status: int = 200,
    json_body: dict | list | None = None,
    body: str | None = None,
    headers: dict[str, str] | None = None,
    delay_ms: int = 0,
    barrier: dict | None = None,
    key: str | None = None,
) -> dict:
    entry: dict = {"match": match, "status": status}
    if method is not None:
        entry["method"] = method
    if json_body is not None:
        entry["json"] = json_body
    if body is not None:
        entry["body"] = body
    if headers:
        entry["headers"] = headers
    if delay_ms:
        entry["delay_ms"] = delay_ms
    if barrier:
        entry["barrier"] = barrier
    if key:
        entry["key"] = key
    return entry


def scenario(
    *routes: dict,
    listing: list[dict] | None = None,
    children: dict[str, list[dict]] | None = None,
    objects: dict[str, str] | None = None,
) -> dict:
    flat: list[dict] = []
    for item in routes:
        if isinstance(item, list):
            flat.extend(item)
        else:
            flat.append(item)
    payload: dict = {"routes": flat}
    if listing is not None:
        payload["listing"] = listing
    if children is not None:
        payload["children"] = children
    if objects is not None:
        payload["objects"] = objects
    return payload


def entry(
    id: str,
    name: str,
    kind: str,
    *,
    parent: str = "0",
    size: int = 11,
) -> dict:
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


def basic_header(username: str, password: str) -> dict[str, str]:
    token = base64.b64encode(f"{username}:{password}".encode()).decode()
    return {"Authorization": f"Basic {token}"}


def import_cookies() -> list[dict]:
    """Placeholder import cookies that pass the WPS domain/csrf/rtk rules."""

    return [
        {"name": "csrf", "value": "bench-import-csrf", "domain": ".kdocs.cn", "path": "/"},
        {
            "name": "rtk",
            "value": "bench-import-rtk",
            "domain": ".kdocs.cn",
            "path": "/passport/secure",
        },
        {"name": "bench-session", "value": "bench-import-session", "domain": ".kdocs.cn", "path": "/"},
    ]


class Service:
    """One adapter service under test (Python today, Go later)."""

    def __init__(
        self,
        *,
        scenario_data: dict | None = None,
        workspace: dict | None = None,
        group_id: str = "bench-group",
        root_id: str = "0",
        cookie_value: str = COOKIE_PLACEHOLDER,
        csrf_value: str = CSRF_PLACEHOLDER,
        auto_refresh: bool = True,
        refresh_script: str | None = None,
        username: str | None = None,
        password: str | None = None,
        max_connections: int = 64,
        extra_env: dict[str, str] | None = None,
        start_timeout: float = 20.0,
    ) -> None:
        self._dir = tempfile.mkdtemp(prefix="wps-contract-")
        os.chmod(self._dir, 0o700)
        self._process: subprocess.Popen | None = None
        self.port = self._free_port()

        self.cookie_path = self._write_secret("wps-cookie", cookie_value)
        self.csrf_path = self._write_secret("wps-csrf", csrf_value)
        settings_dir = os.path.join(self._dir, "web-settings")
        os.mkdir(settings_dir)
        os.chmod(settings_dir, 0o700)
        self.web_settings_path = os.path.join(settings_dir, "web-settings.json")
        env = {
            "WPS_GROUP_ID": group_id,
            "WPS_ROOT_ID": root_id,
            "WPS_COOKIE_FILE": self.cookie_path,
            "WPS_CSRF_TOKEN_FILE": self.csrf_path,
            "WPS_AUTO_REFRESH": "true" if auto_refresh else "false",
            "WPS_UPLOAD_MIN_FREE_BYTES": "0",
            "ADAPTER_BIND": "127.0.0.1",
            "ADAPTER_PORT": str(self.port),
            "ADAPTER_MAX_CONNECTIONS": str(max_connections),
            "CONTRACT_WEB_SETTINGS_FILE": self.web_settings_path,
            "PYTHONPATH": os.path.join(PROJECT_ROOT, "src"),
            "PYTHONDONTWRITEBYTECODE": "1",
        }
        if workspace is not None:
            self.workspace_path = self._write_secret(
                "wps-workspace.json", json.dumps(workspace), text=True
            )
            env["WPS_WORKSPACE_FILE"] = self.workspace_path
        if refresh_script is not None:
            script_path = os.path.join(self._dir, "refresh.sh")
            with open(script_path, "w", encoding="utf-8") as handle:
                handle.write(refresh_script)
            os.chmod(script_path, 0o700)
            env["WPS_CREDENTIAL_REFRESH_COMMAND"] = script_path
            env["WPS_CREDENTIAL_REFRESH_TIMEOUT"] = "10"
        if username is not None:
            env["ADAPTER_USERNAME_FILE"] = self._write_secret("adapter-username", username)
        if password is not None:
            env["ADAPTER_PASSWORD_FILE"] = self._write_secret("adapter-password", password)
        if extra_env:
            env.update(extra_env)

        self._scenario_path = os.path.join(self._dir, "scenario.json")
        with open(self._scenario_path, "w", encoding="utf-8") as handle:
            json.dump(scenario_data or {}, handle)
        self._record_path = os.path.join(self._dir, "upstream-requests.jsonl")
        self._stats_path = os.path.join(self._dir, "upstream-stats.json")

        child_env = os.environ.copy()
        child_env.update(env)
        self._process = subprocess.Popen(
            [
                sys.executable,
                os.path.join(HERE, "python_service.py"),
                "--port",
                str(self.port),
                "--scenario",
                self._scenario_path,
                "--record",
                self._record_path,
                "--stats",
                self._stats_path,
            ],
            env=child_env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        # The child prints its listening line only after the server socket is
        # bound, so stdout replaces port probing (a probe connection would
        # consume a connection slot and break max-connections scenarios).
        self._stdout_lines: queue.Queue[str] = queue.Queue()
        threading.Thread(target=self._pump_stdout, daemon=True).start()
        deadline = time.monotonic() + start_timeout
        try:
            first_line = self._stdout_lines.get(timeout=max(0.1, deadline - time.monotonic()))
        except queue.Empty:
            first_line = ""
        if self._process.poll() is not None:
            _, stderr = self._process.communicate()
            raise StartupFailed(self._process.returncode, (stderr or "")[-2000:])
        if "listening=" not in first_line:
            self.stop()
            raise StartupFailed(-1, f"unexpected first output: {first_line!r}")

    def _pump_stdout(self) -> None:
        assert self._process is not None and self._process.stdout is not None
        for line in self._process.stdout:
            self._stdout_lines.put(line.strip())

    # -- lifecycle -------------------------------------------------------------

    @staticmethod
    def _free_port() -> int:
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            return sock.getsockname()[1]

    def _write_secret(self, name: str, content: str, *, text: bool = False) -> str:
        path = os.path.join(self._dir, name)
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(content)
            if not content.endswith("\n") and text:
                handle.write("\n")
        os.chmod(path, 0o600)
        return path

    def stop(self) -> None:
        if self._process is None:
            return
        if self._process.poll() is None:
            self._process.terminate()
            try:
                self._process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self._process.kill()
                self._process.wait(timeout=5)
        self._stderr_tail = (self._process.stderr.read() or "")[-4000:]
        self._process.stdout.close()
        self._process.stderr.close()
        self._process = None

    @property
    def stderr_tail(self) -> str:
        return getattr(self, "_stderr_tail", "")

    def __enter__(self) -> "Service":
        return self

    def __exit__(self, *exc: object) -> None:
        self.stop()

    # -- HTTP helpers ------------------------------------------------------------

    @property
    def host(self) -> str:
        return "127.0.0.1"

    def request(
        self,
        method: str,
        target: str,
        *,
        body: bytes | None = None,
        headers: dict[str, str] | None = None,
        auth: dict[str, str] | None = None,
    ) -> tuple[int, dict[str, str], bytes]:
        conn = HTTPConnection(self.host, self.port, timeout=30)
        try:
            merged = dict(headers or {})
            if auth:
                merged.update(auth)
            conn.request(method, target, body=body, headers=merged)
            response = conn.getresponse()
            payload = response.read()
            return response.status, {k: v for k, v in response.getheaders()}, payload
        finally:
            conn.close()

    def raw_connect(self) -> socket.socket:
        return socket.create_connection((self.host, self.port), timeout=10)

    # -- fake upstream observability ----------------------------------------------

    def upstream_records(self) -> list[dict]:
        try:
            with open(self._record_path, encoding="utf-8") as handle:
                return [json.loads(line) for line in handle if line.strip()]
        except FileNotFoundError:
            return []

    def upstream_stats(self) -> dict:
        with open(self._stats_path, encoding="utf-8") as handle:
            return json.load(handle)
