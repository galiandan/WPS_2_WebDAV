"""Dependency-free WebDAV and REST facade for :class:`WpsStorage`."""

from __future__ import annotations

import base64
import binascii
import email.utils
import hmac
import json
import logging
import mimetypes
import re
import threading
import time
import uuid
from dataclasses import dataclass, field
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, BinaryIO
from urllib.parse import parse_qs, quote, urlsplit
from xml.etree import ElementTree

from . import __version__
from .client import WpsApiError
from .client import _read_credential_file
from .login import credentials_from_cookies
from .provider import (
    AlreadyExistsError,
    AmbiguousPathError,
    EntryNotFoundError,
    InsufficientStorageError,
    InvalidPathError,
    NotFolderError,
    RemoteEntry,
    ServiceBusyError,
    UnsupportedOperationError,
)
from .storage import WpsStorage, join_remote_path, split_remote_path
from .settings import WebSettings, validate_root_name
from .web import render_web_app
from .workspace import WorkspaceConfigError, validate_workspace_identifier


LOG = logging.getLogger("wps_adapter.http")


@dataclass(slots=True)
class BasicAuth:
    """Optional adapter-side Basic authentication.

    The credentials are read from files for each request when file paths are
    configured.  WPS Cookie values are never used as adapter credentials.
    """

    username: str = ""
    password: str = field(default="", repr=False)
    username_file: str | None = None
    password_file: str | None = None

    @staticmethod
    def _read(path: str | None) -> str:
        if not path:
            return ""
        try:
            return _read_credential_file(path, operation="read adapter credential file")
        except WpsApiError:
            return ""

    def values(self) -> tuple[str, str]:
        username = self._read(self.username_file) if self.username_file else self.username
        password = self._read(self.password_file) if self.password_file else self.password
        return username, password

    @property
    def enabled(self) -> bool:
        return bool(
            self.username
            or self.password
            or self.username_file
            or self.password_file
        )

    def accepts(self, header: str | None) -> bool:
        username, password = self.values()
        if not username or not password or not header:
            return False
        scheme, separator, encoded = header.partition(" ")
        if separator == "" or scheme.lower() != "basic":
            return False
        try:
            decoded = base64.b64decode(encoded.strip(), validate=True).decode("utf-8")
        except (ValueError, UnicodeDecodeError, binascii.Error):
            return False
        supplied_user, separator, supplied_password = decoded.partition(":")
        if not separator:
            return False
        return hmac.compare_digest(supplied_user, username) and hmac.compare_digest(
            supplied_password, password
        )


@dataclass(frozen=True, slots=True)
class ActiveLock:
    token: str
    path: str
    depth: str
    owner: str
    timeout_seconds: int
    expires_at: float


class DavLockStore:
    """Process-local WebDAV write locks.

    WPS locking was not present in the confirmed captures, so locks protect
    this adapter's concurrent clients only. They expire automatically and do
    not survive a service restart.
    """

    _token_pattern = re.compile(r"<((?:opaquelocktoken:)[^>]+)>", re.IGNORECASE)

    def __init__(self, *, max_timeout: int = 86400, max_locks: int = 4096) -> None:
        if max_timeout <= 0:
            raise ValueError("max_timeout must be positive")
        if max_locks <= 0:
            raise ValueError("max_locks must be positive")
        self.max_timeout = max_timeout
        self.max_locks = max_locks
        self._locks: dict[str, ActiveLock] = {}
        self._lock = threading.RLock()

    @staticmethod
    def _applies(lock: ActiveLock, path: str) -> bool:
        if lock.path == path:
            return True
        if lock.depth != "infinity":
            return False
        return path.startswith("/") and (
            lock.path == "/" or path.startswith(lock.path.rstrip("/") + "/")
        )

    def _purge(self) -> None:
        now = time.monotonic()
        expired = [token for token, lock in self._locks.items() if lock.expires_at <= now]
        for token in expired:
            del self._locks[token]

    @classmethod
    def tokens_from_headers(cls, *headers: str | None) -> set[str]:
        tokens: set[str] = set()
        for header in headers:
            if not header:
                continue
            tokens.update(match.group(1) for match in cls._token_pattern.finditer(header))
            stripped = header.strip().strip("<>")
            if stripped.lower().startswith("opaquelocktoken:"):
                tokens.add(stripped)
        return tokens

    def allows(self, path: str, tokens: set[str]) -> bool:
        with self._lock:
            self._purge()
            return all(
                lock.token in tokens
                for lock in self._locks.values()
                if self._applies(lock, path)
            )

    def acquire(
        self,
        path: str,
        *,
        depth: str,
        owner: str,
        timeout_seconds: int,
        refresh_token: str | None = None,
    ) -> ActiveLock:
        with self._lock:
            self._purge()
            timeout_seconds = max(1, min(timeout_seconds, self.max_timeout))
            if refresh_token is not None:
                current = self._locks.get(refresh_token)
                if current is None or current.path != path:
                    raise KeyError("lock token is invalid")
                refreshed = ActiveLock(
                    token=current.token,
                    path=current.path,
                    depth=current.depth,
                    owner=current.owner,
                    timeout_seconds=timeout_seconds,
                    expires_at=time.monotonic() + timeout_seconds,
                )
                self._locks[refresh_token] = refreshed
                return refreshed

            for current in self._locks.values():
                if self._applies(current, path) or (
                    depth == "infinity" and self._applies(
                        ActiveLock("", path, depth, "", 1, 0), current.path
                    )
                ):
                    raise RuntimeError("resource is already locked")
            if len(self._locks) >= self.max_locks:
                raise ServiceBusyError("too many active WebDAV locks")
            token = "opaquelocktoken:" + str(uuid.uuid4())
            active = ActiveLock(
                token=token,
                path=path,
                depth=depth,
                owner=owner,
                timeout_seconds=timeout_seconds,
                expires_at=time.monotonic() + timeout_seconds,
            )
            self._locks[token] = active
            return active

    def unlock(self, path: str, token: str) -> None:
        with self._lock:
            self._purge()
            current = self._locks.get(token)
            if current is None or current.path != path:
                raise KeyError("lock token is invalid")
            del self._locks[token]


@dataclass(slots=True)
class AdapterApplication:
    storage: WpsStorage
    auth: BasicAuth = field(default_factory=BasicAuth)
    dav_prefix: str = "/dav"
    rest_prefix: str = "/api/v1"
    locks: DavLockStore = field(default_factory=DavLockStore)
    max_propfind_entries: int = 10000
    max_propfind_depth: int = 64
    max_control_body: int = 1024 * 1024
    max_response_body: int = 16 * 1024 * 1024
    web_root_name: str = "WPS Enterprise Drive"
    web_settings: WebSettings | None = None

    def __post_init__(self) -> None:
        self.dav_prefix = self._normalise_prefix(self.dav_prefix)
        self.rest_prefix = self._normalise_prefix(self.rest_prefix)
        if not self.web_root_name:
            self.web_root_name = "WPS Enterprise Drive"
        if self.web_settings is not None:
            self._apply_web_root_name(self.web_settings.name)
        if self.max_propfind_entries <= 0:
            raise ValueError("max_propfind_entries must be positive")
        if self.max_propfind_depth <= 0:
            raise ValueError("max_propfind_depth must be positive")
        if self.max_control_body <= 0:
            raise ValueError("max_control_body must be positive")
        if self.max_response_body <= 0:
            raise ValueError("max_response_body must be positive")

    @staticmethod
    def _normalise_prefix(value: str) -> str:
        if not value.startswith("/"):
            value = "/" + value
        return value.rstrip("/") or "/"

    def _apply_web_root_name(self, name: str) -> None:
        self.web_root_name = name
        set_root_name = getattr(self.storage, "set_root_name", None)
        if callable(set_root_name):
            set_root_name(name)

    def current_web_root_name(self) -> str:
        if self.web_settings is not None:
            name = self.web_settings.name
            if name != self.web_root_name:
                self._apply_web_root_name(name)
        return self.web_root_name

    def set_web_root_name(self, value: object) -> str:
        name = validate_root_name(value)
        if self.web_settings is not None:
            self.web_settings.set_name(name)
        self._apply_web_root_name(name)
        return name


class _LimitedReader:
    """Expose exactly a Content-Length-sized view of an HTTP request body."""

    def __init__(self, source: BinaryIO, remaining: int) -> None:
        self.source = source
        self.remaining = remaining

    def read(self, size: int = -1) -> bytes:
        if self.remaining <= 0:
            return b""
        if size is None or size < 0:
            size = self.remaining
        size = min(size, self.remaining)
        chunk = self.source.read(size)
        if not chunk:
            self.remaining = 0
            return b""
        self.remaining -= len(chunk)
        return chunk

    def drain(self) -> None:
        while self.remaining:
            chunk = self.read(min(64 * 1024, self.remaining))
            if not chunk:
                return


class _RangeNotSatisfiable(ValueError):
    def __init__(self, size: int | None) -> None:
        self.size = size
        super().__init__("requested byte range cannot be satisfied")


class _RequestBodyTooLarge(ValueError):
    """A control request exceeded the adapter's bounded parser budget."""

    def __init__(self) -> None:
        super().__init__("request body is too large")


class AdapterHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 64

    def __init__(
        self,
        address: tuple[str, int],
        application: AdapterApplication,
        *,
        max_connections: int = 64,
        request_timeout: float = 60.0,
    ) -> None:
        if max_connections <= 0:
            raise ValueError("max_connections must be positive")
        if request_timeout <= 0:
            raise ValueError("request_timeout must be positive")
        self.application = application
        self.max_connections = max_connections
        self.request_timeout = request_timeout
        self._connection_slots = threading.BoundedSemaphore(max_connections)
        super().__init__(address, AdapterRequestHandler)

    def process_request(self, request: Any, client_address: Any) -> None:
        if not self._connection_slots.acquire(blocking=False):
            request.close()
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self._connection_slots.release()
            raise

    def process_request_thread(self, request: Any, client_address: Any) -> None:
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._connection_slots.release()


class AdapterRequestHandler(BaseHTTPRequestHandler):
    """HTTP/1.1 handler implementing a deliberately small protocol surface."""

    protocol_version = "HTTP/1.1"
    server_version = "wps-enterprise-adapter/" + __version__

    @property
    def application(self) -> AdapterApplication:
        return self.server.application  # type: ignore[attr-defined]

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(self.server.request_timeout)  # type: ignore[attr-defined]

    def parse_request(self) -> bool:
        if not super().parse_request():
            return False
        transfer_encoding = ",".join(self.headers.get_all("Transfer-Encoding", [])).strip()
        content_lengths = self.headers.get_all("Content-Length", [])
        if transfer_encoding:
            self.close_connection = True
            self._send_error(HTTPStatus.BAD_REQUEST, "Transfer-Encoding is not supported")
            return False
        if len(content_lengths) > 1:
            self.close_connection = True
            self._send_error(HTTPStatus.BAD_REQUEST, "multiple Content-Length headers are not supported")
            return False
        if self.command in {"GET", "HEAD", "OPTIONS"} and content_lengths:
            try:
                declared = int(content_lengths[0])
            except ValueError:
                declared = -1
            if declared != 0:
                self.close_connection = True
                self._send_error(HTTPStatus.BAD_REQUEST, "request body is not supported for this method")
                return False
        return True

    def log_message(self, format: str, *args: Any) -> None:
        # Keep query values and headers out of logs.  Signed WPS URLs should
        # never reach this service, but avoiding query logging is still safer.
        path = urlsplit(self.path).path
        path = "".join(char if ord(char) >= 0x20 and char != "\x7f" else "?" for char in path)
        LOG.info("%s %s", self.command, path)

    def _is_health(self) -> bool:
        return urlsplit(self.path).path == "/healthz"

    def _is_web_app(self) -> bool:
        return urlsplit(self.path).path in {"/", "/web", "/web/"}

    def _authorise(self) -> bool:
        if self._is_health() or not self.application.auth.enabled:
            return True
        if self.application.auth.accepts(self.headers.get("Authorization")):
            return True
        self.send_response(HTTPStatus.UNAUTHORIZED)
        self.send_header("WWW-Authenticate", 'Basic realm="wps-adapter"')
        self.send_header("Connection", "close")
        self.send_header("Content-Length", "0")
        self.end_headers()
        self.close_connection = True
        return False

    def _origin_matches_request_host(self, value: str, *, referer: bool) -> bool:
        if not value or any(ord(char) < 0x20 or ord(char) == 0x7F for char in value):
            return False
        try:
            parts = urlsplit(value)
            if (
                parts.scheme not in {"http", "https"}
                or not parts.hostname
                or parts.username
                or parts.password
                or parts.query
                or parts.fragment
                or (not referer and parts.path not in {"", "/"})
            ):
                return False
            origin_port = parts.port
            request_host = self.headers.get("Host", "")
            request_parts = urlsplit("//" + request_host)
            if not request_parts.hostname:
                return False
            request_port = request_parts.port
            origin_hostname = parts.hostname
            request_hostname = request_parts.hostname
        except ValueError:
            return False
        if origin_hostname.rstrip(".").casefold() != request_hostname.rstrip(".").casefold():
            return False
        # A browser normally sends the same explicit adapter port in both
        # headers. Reject a mismatch while allowing reverse proxies to omit
        # the public default port from Host.
        return origin_port is None or request_port is None or origin_port == request_port

    def _allow_mutation_origin(self) -> bool:
        origin = self.headers.get("Origin")
        if origin is not None:
            matches = self._origin_matches_request_host(origin, referer=False)
        else:
            referer = self.headers.get("Referer")
            matches = referer is None or self._origin_matches_request_host(referer, referer=True)
        if matches:
            return True
        self.close_connection = True
        self._send_error(
            HTTPStatus.FORBIDDEN,
            "cross-origin mutation is not allowed",
            headers={"Connection": "close"},
        )
        return False

    def _send_bytes(
        self,
        status: int,
        body: bytes = b"",
        *,
        content_type: str = "text/plain; charset=utf-8",
        headers: dict[str, str] | None = None,
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        if headers:
            for name, value in headers.items():
                self.send_header(name, value)
        if self.close_connection and (
            not headers or not any(name.casefold() == "connection" for name in headers)
        ):
            self.send_header("Connection", "close")
        self.end_headers()
        if self.command != "HEAD" and body:
            self.wfile.write(body)

    def _send_json(self, status: int, payload: object, *, headers: dict[str, str] | None = None) -> None:
        body = json.dumps(payload, ensure_ascii=True, separators=(",", ":")).encode("utf-8")
        if len(body) > self.application.max_response_body:
            raise InsufficientStorageError("response exceeds the configured size limit")
        self._send_bytes(status, body, content_type="application/json; charset=utf-8", headers=headers)

    def _send_error(
        self,
        status: int,
        message: str,
        *,
        rest: bool = False,
        headers: dict[str, str] | None = None,
    ) -> None:
        if rest:
            self._send_json(status, {"error": message}, headers=headers)
        else:
            self._send_bytes(status, (message + "\n").encode("utf-8"), headers=headers)

    def _handle_exception(self, exc: Exception, *, rest: bool = False) -> None:
        if isinstance(exc, _RequestBodyTooLarge):
            self._send_error(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, str(exc), rest=rest)
        elif isinstance(exc, InvalidPathError):
            self._send_error(HTTPStatus.BAD_REQUEST, str(exc), rest=rest)
        elif isinstance(exc, (EntryNotFoundError,)):
            self._send_error(HTTPStatus.NOT_FOUND, str(exc), rest=rest)
        elif isinstance(exc, (NotFolderError, AlreadyExistsError, AmbiguousPathError)):
            self._send_error(HTTPStatus.CONFLICT, str(exc), rest=rest)
        elif isinstance(exc, InsufficientStorageError):
            self._send_error(HTTPStatus.INSUFFICIENT_STORAGE, str(exc), rest=rest)
        elif isinstance(exc, ServiceBusyError):
            self._send_error(
                HTTPStatus.SERVICE_UNAVAILABLE,
                str(exc),
                rest=rest,
                headers={"Retry-After": "5"},
            )
        elif isinstance(exc, UnsupportedOperationError):
            self._send_error(HTTPStatus.NOT_IMPLEMENTED, str(exc), rest=rest)
        elif isinstance(exc, WpsApiError):
            # Do not relay upstream response bodies or signed URLs.
            status = HTTPStatus.BAD_GATEWAY
            message = "upstream WPS request failed"
            headers: dict[str, str] = {}
            if exc.status == 401:
                status = HTTPStatus.SERVICE_UNAVAILABLE
                message = "WPS session expired; refresh the configured credentials"
                headers["Retry-After"] = "60"
            payload: dict[str, object] = {
                "error": message,
                "code": (
                    "wps_session_expired"
                    if exc.status == 401
                    else "wps_unavailable"
                ),
            }
            if rest:
                if exc.status is not None:
                    payload["upstream_status"] = exc.status
                self._send_json(status, payload, headers=headers)
            else:
                self._send_error(status, message, rest=False, headers=headers)
        elif isinstance(exc, (ValueError, TypeError)):
            self._send_error(HTTPStatus.BAD_REQUEST, str(exc), rest=rest)
        elif isinstance(exc, OSError):
            self._send_error(HTTPStatus.BAD_GATEWAY, "local or upstream I/O failed", rest=rest)
        else:
            LOG.exception("request failed")
            self._send_error(HTTPStatus.INTERNAL_SERVER_ERROR, "internal server error", rest=rest)

    def _dav_path(self) -> str | None:
        path = urlsplit(self.path).path
        prefix = self.application.dav_prefix
        if path == prefix:
            return "/"
        if path.startswith(prefix + "/"):
            return path[len(prefix) :] or "/"
        return None

    def _rest_route(self) -> tuple[str, dict[str, list[str]]] | None:
        parsed = urlsplit(self.path)
        prefix = self.application.rest_prefix
        if parsed.path != prefix and not parsed.path.startswith(prefix + "/"):
            return None
        suffix = parsed.path[len(prefix) :].strip("/")
        return suffix, parse_qs(parsed.query, keep_blank_values=True)

    @staticmethod
    def _query_path(query: dict[str, list[str]]) -> str:
        values = query.get("path", ["/"])
        if len(values) != 1 or not values[0]:
            raise InvalidPathError("query parameter 'path' must contain one non-empty path")
        return values[0]

    @staticmethod
    def _query_bool(
        query: dict[str, list[str]],
        name: str,
        *,
        default: bool = False,
    ) -> bool:
        values = query.get(name)
        if values is None:
            return default
        if len(values) != 1:
            raise InvalidPathError(f"query parameter '{name}' must contain one value")
        value = values[0].strip().lower()
        if value in {"1", "true", "yes", "on"}:
            return True
        if value in {"0", "false", "no", "off"}:
            return False
        raise InvalidPathError(f"query parameter '{name}' must be boolean")

    def _content_length(self, *, required: bool = False) -> int | None:
        transfer_encoding = ",".join(self.headers.get_all("Transfer-Encoding", [])).strip()
        if transfer_encoding:
            self.close_connection = True
            raise ValueError("Transfer-Encoding is not supported; send Content-Length")
        content_lengths = self.headers.get_all("Content-Length", [])
        if len(content_lengths) > 1:
            self.close_connection = True
            raise ValueError("multiple Content-Length headers are not supported")
        value = content_lengths[0] if content_lengths else None
        if value is None:
            if required:
                self.close_connection = True
                self._send_error(HTTPStatus.LENGTH_REQUIRED, "Content-Length is required")
            return None
        try:
            length = int(value)
        except ValueError as exc:
            self.close_connection = True
            raise ValueError("invalid Content-Length") from exc
        if length < 0:
            self.close_connection = True
            raise ValueError("invalid Content-Length")
        return length

    def _check_declared_upload_length(self, length: int) -> None:
        config = getattr(getattr(self.application.storage, "client", None), "config", None)
        maximum = getattr(config, "max_upload_bytes", 0)
        if isinstance(maximum, int) and maximum > 0 and length > maximum:
            self.close_connection = True
            raise InsufficientStorageError("upload exceeds the configured size limit")

    def _discard_body(self) -> None:
        length = self._content_length(required=False)
        if length is None:
            return
        if length > self.application.max_control_body:
            self.close_connection = True
            raise _RequestBodyTooLarge()
        remaining = length
        while remaining:
            chunk = self.rfile.read(min(64 * 1024, remaining))
            if not chunk:
                break
            remaining -= len(chunk)

    def _json_body(self, *, max_length: int | None = None) -> dict[str, Any] | None:
        length = self._content_length(required=True)
        if length is None:
            return None
        if max_length is None:
            max_length = self.application.max_control_body
        if max_length is not None and length > max_length:
            self.close_connection = True
            raise _RequestBodyTooLarge()
        body = self.rfile.read(length)
        if len(body) != length:
            self.close_connection = True
            raise ValueError("request body is shorter than Content-Length")
        try:
            payload = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ValueError("request body must be valid JSON") from exc
        if not isinstance(payload, dict):
            raise ValueError("request body must be a JSON object")
        return payload

    @staticmethod
    def _entry_json(entry: RemoteEntry) -> dict[str, object]:
        return {
            "id": entry.id,
            "name": entry.name,
            "kind": entry.kind,
            "parent_id": entry.parent_id,
            "size": entry.size,
            "modified_at": entry.modified_at,
            "etag": entry.etag,
        }

    def _handle_health(self) -> None:
        self._send_json(
            HTTPStatus.OK,
            {
                "status": "ok",
                "service": "wps-enterprise-adapter",
                "version": __version__,
                "network_calls": "on-demand",
            },
        )

    def _handle_web_app(self) -> None:
        self._send_bytes(
            HTTPStatus.OK,
            render_web_app(self.application.current_web_root_name()).encode("utf-8"),
            content_type="text/html; charset=utf-8",
            headers={
                "Content-Security-Policy": (
                    "default-src 'self'; script-src 'self' 'unsafe-inline'; "
                    "style-src 'self' 'unsafe-inline'; img-src 'self' data:; "
                    "object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
                )
            },
        )

    def _send_download(self, path: str, *, rest: bool = False, head: bool = False) -> None:
        entry = self.application.storage.metadata(path)
        if entry.kind != "file":
            raise NotFolderError("the requested path is not a file")
        content_type = mimetypes.guess_type(entry.name)[0] or "application/octet-stream"
        offset = 0
        length: int | None = None
        range_header = self.headers.get("Range")
        if range_header and self._if_range_matches(entry):
            try:
                offset, length = self._parse_range(range_header, entry.size)
            except _RangeNotSatisfiable as exc:
                size_text = "*" if exc.size is None else str(exc.size)
                self._send_error(
                    HTTPStatus.REQUESTED_RANGE_NOT_SATISFIABLE,
                    "requested byte range cannot be satisfied",
                    rest=rest,
                    headers={"Content-Range": f"bytes */{size_text}"},
                )
                return
        range_requested = range_header is not None and length is not None
        headers: dict[str, str] = {"Accept-Ranges": "bytes"}
        if entry.etag:
            headers["ETag"] = f'"{entry.etag.strip(chr(34))}"'
        if range_requested:
            assert entry.size is not None
            headers["Content-Range"] = f"bytes {offset}-{offset + length - 1}/{entry.size}"
            headers["Content-Length"] = str(length)
        elif entry.size is not None and entry.size >= 0:
            headers["Content-Length"] = str(entry.size)

        if head:
            self.send_response(
                HTTPStatus.PARTIAL_CONTENT if range_requested else HTTPStatus.OK
            )
            self.send_header("Content-Type", content_type)
            for name, value in headers.items():
                self.send_header(name, value)
            if "Content-Length" not in headers and entry.size is not None and entry.size >= 0:
                self.send_header("Content-Length", str(entry.size))
            self.end_headers()
            return

        stream = self.application.storage.open_path(path, offset=offset, length=length)
        if "Content-Length" not in headers and stream.content_length is not None:
            headers["Content-Length"] = str(stream.content_length)
        self.send_response(HTTPStatus.PARTIAL_CONTENT if range_requested else HTTPStatus.OK)
        self.send_header("Content-Type", content_type)
        for name, value in headers.items():
            self.send_header(name, value)
        if "Content-Length" not in headers:
            self.send_header("Connection", "close")
            self.close_connection = True
        self.end_headers()
        try:
            while True:
                chunk = stream.read(self.application.storage.client.config.stream_chunk_size)
                if not chunk:
                    break
                self.wfile.write(chunk)
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True
        finally:
            stream.close()

    def _if_range_matches(self, entry: RemoteEntry) -> bool:
        value = self.headers.get("If-Range")
        if not value:
            return True
        if not entry.etag:
            return False
        expected = f'"{entry.etag.strip(chr(34))}"'
        return value.strip() in {expected, entry.etag.strip(chr(34))}

    @staticmethod
    def _parse_range(value: str, size: int | None) -> tuple[int, int]:
        if size is None or size < 0:
            raise _RangeNotSatisfiable(size)
        unit, separator, spec = value.partition("=")
        if separator == "" or unit.strip().lower() != "bytes" or "," in spec:
            raise _RangeNotSatisfiable(size)
        start_text, dash, end_text = spec.strip().partition("-")
        if dash == "":
            raise _RangeNotSatisfiable(size)
        try:
            if not start_text:
                suffix = int(end_text)
                if suffix <= 0:
                    raise _RangeNotSatisfiable(size)
                start = max(size - suffix, 0)
                end = size - 1
            else:
                start = int(start_text)
                if start < 0 or start >= size:
                    raise _RangeNotSatisfiable(size)
                end = size - 1 if not end_text else min(int(end_text), size - 1)
                if end < start:
                    raise _RangeNotSatisfiable(size)
        except (TypeError, ValueError):
            raise _RangeNotSatisfiable(size) from None
        return start, end - start + 1

    def _webdav_entries(self, path: str, depth: str) -> list[tuple[str, RemoteEntry]]:
        parts = split_remote_path(path)
        entry = self.application.storage.metadata(path)
        result: list[tuple[str, RemoteEntry]] = []

        def visit(current_path: str, current_parts: tuple[str, ...], current_entry: RemoteEntry, level: int) -> None:
            if len(result) >= self.application.max_propfind_entries:
                raise InsufficientStorageError("PROPFIND exceeds the configured entry limit")
            result.append((self._href(current_parts, current_entry), current_entry))
            should_recurse = current_entry.kind == "folder" and (
                depth == "infinity" or (depth == "1" and level == 0)
            )
            if not should_recurse:
                return
            if level >= self.application.max_propfind_depth:
                raise InsufficientStorageError("PROPFIND exceeds the configured depth limit")
            for child in self.application.storage.list_path(current_path):
                child_parts = current_parts + (child.name,)
                child_path = join_remote_path(
                    child_parts,
                    trailing_slash=child.kind == "folder",
                )
                visit(child_path, child_parts, child, level + 1)

        visit(path, parts, entry, 0)
        return result

    def _href(self, parts: tuple[str, ...], entry: RemoteEntry) -> str:
        prefix = self.application.dav_prefix.rstrip("/")
        encoded_parts = "/".join(quote(part, safe="") for part in parts)
        href = prefix + ("/" + encoded_parts if encoded_parts else "/")
        if entry.kind == "folder" and not href.endswith("/"):
            href += "/"
        return href

    def _href_path(self, path: str) -> str:
        parts = split_remote_path(path)
        prefix = self.application.dav_prefix.rstrip("/")
        encoded_parts = "/".join(quote(part, safe="") for part in parts)
        return prefix + ("/" + encoded_parts if encoded_parts else "/")

    @staticmethod
    def _canonical_path(path: str) -> str:
        return join_remote_path(split_remote_path(path))

    def _lock_tokens(self) -> set[str]:
        return DavLockStore.tokens_from_headers(
            self.headers.get("If"),
            self.headers.get("Lock-Token"),
        )

    def _check_locks(self, *paths: str, rest: bool = False) -> bool:
        tokens = self._lock_tokens()
        for path in paths:
            if not self.application.locks.allows(self._canonical_path(path), tokens):
                self._send_error(HTTPStatus.LOCKED, "resource is locked", rest=rest)
                return False
        return True

    def _overwrite_header(self) -> bool:
        value = self.headers.get("Overwrite", "T").strip().upper()
        if value not in {"T", "F"}:
            raise ValueError("Overwrite must be T or F")
        return value == "T"

    def _lock_timeout(self) -> int:
        value = self.headers.get("Timeout", "Second-3600")
        if value.strip().lower() == "infinite":
            return self.application.locks.max_timeout
        match = re.search(r"second-(\d+)", value, flags=re.IGNORECASE)
        if not match:
            raise ValueError("Timeout must be Second-N or Infinite")
        return max(1, min(int(match.group(1)), self.application.locks.max_timeout))

    def _lock_depth(self) -> str:
        value = self.headers.get("Depth", "infinity").strip().lower()
        if value not in {"0", "infinity"}:
            raise ValueError("LOCK Depth must be 0 or infinity")
        return value

    def _read_lock_owner(self) -> str:
        length = self._content_length(required=False)
        if length is None or length == 0:
            return ""
        if length > 64 * 1024:
            self.close_connection = True
            raise _RequestBodyTooLarge()
        reader = _LimitedReader(self.rfile, length)
        body = reader.read(length)
        if len(body) != length:
            raise ValueError("request body is shorter than Content-Length")
        if b"<!doctype" in body.lower() or b"<!entity" in body.lower():
            raise ValueError("LOCK request body must not declare XML entities")
        try:
            root = ElementTree.fromstring(body)
        except (ElementTree.ParseError, UnicodeDecodeError) as exc:
            raise ValueError("LOCK request body must be valid XML") from exc
        owner = next(
            (element for element in root.iter() if element.tag.rsplit("}", 1)[-1] == "owner"),
            None,
        )
        if owner is None:
            return ""
        return " ".join("".join(owner.itertext()).split())[:512]

    def _lock_body(self, active: ActiveLock) -> bytes:
        ElementTree.register_namespace("D", "DAV:")
        prop = ElementTree.Element("{DAV:}prop")
        discovery = ElementTree.SubElement(prop, "{DAV:}lockdiscovery")
        lock = ElementTree.SubElement(discovery, "{DAV:}activelock")
        locktype = ElementTree.SubElement(lock, "{DAV:}locktype")
        ElementTree.SubElement(locktype, "{DAV:}write")
        lockscope = ElementTree.SubElement(lock, "{DAV:}lockscope")
        ElementTree.SubElement(lockscope, "{DAV:}exclusive")
        ElementTree.SubElement(lock, "{DAV:}depth").text = (
            "Infinity" if active.depth == "infinity" else "0"
        )
        owner = ElementTree.SubElement(lock, "{DAV:}owner")
        if active.owner:
            owner.text = active.owner
        ElementTree.SubElement(lock, "{DAV:}timeout").text = (
            f"Second-{max(1, int(active.expires_at - time.monotonic()))}"
        )
        locktoken = ElementTree.SubElement(lock, "{DAV:}locktoken")
        ElementTree.SubElement(locktoken, "{DAV:}href").text = active.token
        lockroot = ElementTree.SubElement(lock, "{DAV:}lockroot")
        ElementTree.SubElement(lockroot, "{DAV:}href").text = self._href_path(active.path)
        return ElementTree.tostring(prop, encoding="utf-8", xml_declaration=True)

    def _send_lock_response(self, status: int, active: ActiveLock) -> None:
        self._send_bytes(
            status,
            self._lock_body(active),
            content_type="application/xml; charset=utf-8",
            headers={"DAV": "1,2", "Lock-Token": f"<{active.token}>"},
        )

    @staticmethod
    def _http_date(value: str | None) -> str | None:
        if value is None:
            return None
        try:
            return email.utils.formatdate(float(value), usegmt=True)
        except (TypeError, ValueError, OverflowError):
            return None

    def _propfind_body(self, entries: list[tuple[str, RemoteEntry]]) -> bytes:
        ElementTree.register_namespace("D", "DAV:")
        prefix = b'<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">'
        suffix = b"</D:multistatus>"
        chunks = [prefix]
        response_size = len(prefix) + len(suffix)
        for href, entry in entries:
            response = ElementTree.Element("{DAV:}response")
            ElementTree.SubElement(response, "{DAV:}href").text = href
            propstat = ElementTree.SubElement(response, "{DAV:}propstat")
            prop = ElementTree.SubElement(propstat, "{DAV:}prop")
            resource_type = ElementTree.SubElement(prop, "{DAV:}resourcetype")
            if entry.kind == "folder":
                ElementTree.SubElement(resource_type, "{DAV:}collection")
            ElementTree.SubElement(prop, "{DAV:}displayname").text = entry.name
            ElementTree.SubElement(prop, "{DAV:}getcontentlength").text = str(entry.size or 0)
            content_type = (
                "httpd/unix-directory"
                if entry.kind == "folder"
                else mimetypes.guess_type(entry.name)[0] or "application/octet-stream"
            )
            ElementTree.SubElement(prop, "{DAV:}getcontenttype").text = content_type
            if entry.etag:
                ElementTree.SubElement(prop, "{DAV:}getetag").text = f'"{entry.etag.strip(chr(34))}"'
            modified = self._http_date(entry.modified_at)
            if modified:
                ElementTree.SubElement(prop, "{DAV:}getlastmodified").text = modified
            ElementTree.SubElement(propstat, "{DAV:}status").text = "HTTP/1.1 200 OK"
            chunk = ElementTree.tostring(response, encoding="utf-8")
            response_size += len(chunk)
            if response_size > self.application.max_response_body:
                raise InsufficientStorageError("PROPFIND response exceeds the configured size limit")
            chunks.append(chunk)
        chunks.append(suffix)
        return b"".join(chunks)

    def _do_propfind(self, path: str) -> None:
        self._discard_body()
        depth = self.headers.get("Depth", "1").strip().lower()
        if depth not in {"0", "1", "infinity"}:
            self._send_error(HTTPStatus.BAD_REQUEST, "Depth must be 0, 1 or infinity")
            return
        body = self._propfind_body(self._webdav_entries(path, depth))
        self._send_bytes(
            HTTPStatus.MULTI_STATUS,
            body,
            content_type="application/xml; charset=utf-8",
            headers={"DAV": "1,2"},
        )

    def _do_webdav_put(self, path: str) -> None:
        if not self._check_locks(path):
            self._discard_body()
            return
        length = self._content_length(required=True)
        if length is None:
            return
        self._check_declared_upload_length(length)
        source = _LimitedReader(self.rfile, length)
        try:
            entry = self.application.storage.upload_path(
                path,
                source,
                size=length,
                content_type=self.headers.get("Content-Type"),
                overwrite=True,
            )
        except Exception:
            source.drain()
            raise
        self._send_json(HTTPStatus.CREATED, self._entry_json(entry), headers={"Location": self._href(split_remote_path(path), entry)})

    def _do_webdav_mkcol(self, path: str) -> None:
        if not self._check_locks(path):
            self._discard_body()
            return
        self._discard_body()
        entry = self.application.storage.create_folder_path(path)
        self._send_json(
            HTTPStatus.CREATED,
            self._entry_json(entry),
            headers={"Location": self._href(split_remote_path(path), entry)},
        )

    def _destination_dav_path(self) -> str:
        destination = self.headers.get("Destination")
        if not destination:
            raise InvalidPathError("Destination header is required")
        parsed = urlsplit(destination)
        if parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise InvalidPathError("Destination must not contain credentials, query, or fragment")
        if parsed.netloc:
            destination_host = parsed.hostname
            request_host = self.headers.get("Host", "")
            request_parts = urlsplit("//" + request_host)
            try:
                destination_port = parsed.port
                request_port = request_parts.port
            except ValueError as exc:
                raise InvalidPathError("Destination host or port is invalid") from exc
            if (
                not destination_host
                or not request_parts.hostname
                or destination_host.casefold() != request_parts.hostname.casefold()
                or destination_port != request_port
            ):
                raise InvalidPathError("Destination must point to this adapter")
        destination_path = parsed.path
        prefix = self.application.dav_prefix
        if destination_path == prefix:
            return "/"
        if not destination_path.startswith(prefix + "/"):
            raise InvalidPathError("Destination must point inside the WebDAV path")
        return destination_path[len(prefix) :] or "/"

    def _do_webdav_move(self, path: str) -> None:
        destination = self._destination_dav_path()
        if not self._check_locks(path, destination):
            self._discard_body()
            return
        self._discard_body()
        overwrite = self._overwrite_header()
        same_path = self._canonical_path(path) == self._canonical_path(destination)
        destination_exists = False
        if not same_path:
            try:
                self.application.storage.metadata(destination)
                destination_exists = True
            except EntryNotFoundError:
                pass
        if destination_exists:
            if not overwrite:
                self._send_error(HTTPStatus.PRECONDITION_FAILED, "destination already exists")
                return
            self._send_error(
                HTTPStatus.NOT_IMPLEMENTED,
                "MOVE overwrite is disabled because WPS move is not atomic",
            )
            return
        destination_parts = split_remote_path(destination)
        entry = self.application.storage.move_path(path, destination)
        headers = {"Location": self._href(destination_parts, entry)}
        if destination_exists:
            self._send_bytes(HTTPStatus.NO_CONTENT, headers=headers)
        else:
            self._send_json(HTTPStatus.CREATED, self._entry_json(entry), headers=headers)

    def _do_webdav_copy(self, path: str) -> None:
        destination = self._destination_dav_path()
        depth = self.headers.get("Depth", "infinity").strip().lower()
        if depth not in {"0", "1", "infinity"}:
            self._discard_body()
            self._send_error(HTTPStatus.BAD_REQUEST, "Depth must be 0, 1 or infinity")
            return
        overwrite = self._overwrite_header()
        if not self._check_locks(path, destination):
            self._discard_body()
            return
        self._discard_body()

        destination_exists = True
        try:
            self.application.storage.metadata(destination)
        except EntryNotFoundError:
            destination_exists = False
        if destination_exists and not overwrite:
            self._send_error(HTTPStatus.PRECONDITION_FAILED, "destination already exists")
            return
        if destination_exists:
            self._send_error(
                HTTPStatus.NOT_IMPLEMENTED,
                "COPY overwrite is disabled because the relay is not atomic",
            )
            return
        try:
            entry = self.application.storage.copy_path(
                path,
                destination,
                depth=depth,
                overwrite=overwrite,
            )
        except AlreadyExistsError:
            if not overwrite:
                self._send_error(HTTPStatus.PRECONDITION_FAILED, "destination already exists")
                return
            raise
        destination_parts = split_remote_path(destination)
        headers = {"Location": self._href(destination_parts, entry)}
        if destination_exists:
            self._send_bytes(HTTPStatus.NO_CONTENT, headers=headers)
        else:
            self._send_json(HTTPStatus.CREATED, self._entry_json(entry), headers=headers)

    def _do_lock(self, path: str) -> None:
        canonical = self._canonical_path(path)
        tokens = self._lock_tokens()
        if len(tokens) > 1:
            raise ValueError("LOCK request contains multiple lock tokens")
        refresh_token = next(iter(tokens), None)
        depth = self._lock_depth()
        timeout = self._lock_timeout()
        owner = self._read_lock_owner()
        if refresh_token is not None:
            if not self.application.locks.allows(canonical, tokens):
                self._send_error(HTTPStatus.LOCKED, "resource is locked")
                return
            try:
                active = self.application.locks.acquire(
                    canonical,
                    depth=depth,
                    owner=owner,
                    timeout_seconds=timeout,
                    refresh_token=refresh_token,
                )
            except KeyError:
                self._send_error(HTTPStatus.CONFLICT, "lock token is invalid")
                return
            self._send_lock_response(HTTPStatus.OK, active)
            return

        if not self.application.locks.allows(canonical, tokens):
            self._send_error(HTTPStatus.LOCKED, "resource is locked")
            return
        existed = True
        try:
            self.application.storage.metadata(path)
        except EntryNotFoundError:
            existed = False
        try:
            active = self.application.locks.acquire(
                canonical,
                depth=depth,
                owner=owner,
                timeout_seconds=timeout,
            )
        except RuntimeError:
            self._send_error(HTTPStatus.LOCKED, "resource is locked")
            return
        self._send_lock_response(HTTPStatus.OK if existed else HTTPStatus.CREATED, active)

    def _do_unlock(self, path: str) -> None:
        self._discard_body()
        tokens = DavLockStore.tokens_from_headers(self.headers.get("Lock-Token"))
        if len(tokens) != 1:
            raise ValueError("Lock-Token header is required")
        try:
            self.application.locks.unlock(self._canonical_path(path), next(iter(tokens)))
        except KeyError:
            self._send_error(HTTPStatus.CONFLICT, "lock token is invalid")
            return
        self._send_bytes(HTTPStatus.NO_CONTENT)

    def _do_rest_get(self, route: str, query: dict[str, list[str]]) -> None:
        if route == "settings":
            self._discard_body()
            self._send_json(
                HTTPStatus.OK,
                {"status": "ok", "name": self.application.current_web_root_name()},
            )
            return
        path = self._query_path(query)
        if route in {"entries", "list"}:
            entry = self.application.storage.metadata(path)
            if entry.kind != "folder":
                raise NotFolderError("the requested path is not a folder")
            entries = self.application.storage.list_path(path)
            self._send_json(
                HTTPStatus.OK,
                {"path": path, "entries": [self._entry_json(item) for item in entries]},
            )
        elif route == "metadata":
            self._send_json(HTTPStatus.OK, {"path": path, "entry": self._entry_json(self.application.storage.metadata(path))})
        elif route == "download":
            self._send_download(path, rest=True)
        else:
            self._send_error(HTTPStatus.NOT_FOUND, "unknown REST route", rest=True)

    def _do_rest_session_import(self) -> None:
        payload = self._json_body(max_length=512 * 1024)
        if payload is None:
            return
        raw_cookies = payload.get("cookies")
        if not isinstance(raw_cookies, list) or not raw_cookies:
            raise ValueError("JSON field 'cookies' must be a non-empty array")
        if len(raw_cookies) > 256:
            raise ValueError("too many cookies")
        raw_workspace = payload.get("workspace")
        workspace_state = getattr(self.application.storage.client.config, "workspace", None)
        workspace_data: dict[str, str] | None = None
        if raw_workspace is not None:
            if not isinstance(raw_workspace, dict):
                raise ValueError("JSON field 'workspace' must be an object")
            if workspace_state is None:
                raise ValueError(
                    "workspace import requires WPS_GROUP_ID=auto or WPS_ROOT_ID=auto"
                )
            try:
                workspace_data = {
                    "group_id": validate_workspace_identifier(
                        raw_workspace.get("group_id"),
                        field_name="workspace.group_id",
                    ),
                    "root_id": validate_workspace_identifier(
                        raw_workspace.get("root_id"),
                        field_name="workspace.root_id",
                    ),
                }
            except WorkspaceConfigError as exc:
                raise ValueError(str(exc)) from exc
        credentials, names = credentials_from_cookies(
            raw_cookies,
            base_url=getattr(
                self.application.storage.client.config,
                "base_url",
                "https://365.kdocs.cn",
            ),
        )
        source = getattr(self.application.storage.client.config, "credential_source", None)
        replace_credentials = getattr(source, "replace_credentials", None)
        if not callable(replace_credentials) or not replace_credentials(credentials):
            raise WpsApiError("store imported credentials")
        if workspace_data is not None:
            try:
                workspace_state.update(**workspace_data)
            except WorkspaceConfigError as exc:
                raise WpsApiError("store imported workspace") from exc
            self.application.storage.set_root_id(workspace_state.root_id)
        self._send_json(
            HTTPStatus.OK,
            {
                "status": "ok",
                "cookie_count": len(names),
                **({"workspace": "updated"} if workspace_data is not None else {}),
            },
        )

    def _do_rest_put(self, route: str, query: dict[str, list[str]]) -> None:
        if route not in {"upload", "files"}:
            self._discard_body()
            self._send_error(HTTPStatus.NOT_FOUND, "unknown REST route", rest=True)
            return
        length = self._content_length(required=True)
        if length is None:
            return
        self._check_declared_upload_length(length)
        path = self._query_path(query)
        overwrite = self._query_bool(query, "overwrite")
        if not self._check_locks(path, rest=True):
            self._discard_body()
            return
        source = _LimitedReader(self.rfile, length)
        try:
            entry = self.application.storage.upload_path(
                path,
                source,
                size=length,
                content_type=self.headers.get("Content-Type"),
                overwrite=overwrite,
            )
        except Exception:
            source.drain()
            raise
        self._send_json(HTTPStatus.CREATED, {"path": path, "entry": self._entry_json(entry)})

    def _do_rest_post(self, route: str, query: dict[str, list[str]]) -> None:
        if route == "session/import":
            self._do_rest_session_import()
            return
        if route not in {"folders", "folder"}:
            self._discard_body()
            self._send_error(HTTPStatus.NOT_FOUND, "unknown REST route", rest=True)
            return
        self._discard_body()
        path = self._query_path(query)
        if not self._check_locks(path, rest=True):
            return
        entry = self.application.storage.create_folder_path(path)
        self._send_json(HTTPStatus.CREATED, {"path": path, "entry": self._entry_json(entry)})

    def _do_rest_delete(self, route: str, query: dict[str, list[str]]) -> None:
        if route not in {"entries", "files", "delete"}:
            self._discard_body()
            self._send_error(HTTPStatus.NOT_FOUND, "unknown REST route", rest=True)
            return
        self._discard_body()
        path = self._query_path(query)
        if not self._check_locks(path, rest=True):
            return
        self.application.storage.delete_path(path)
        self._send_bytes(HTTPStatus.NO_CONTENT)

    def _do_rest_patch(self, route: str, query: dict[str, list[str]]) -> None:
        if route == "settings":
            payload = self._json_body()
            if payload is None:
                return
            if set(payload) != {"name"}:
                raise ValueError("JSON field 'name' is required")
            name = self.application.set_web_root_name(payload["name"])
            self._send_json(HTTPStatus.OK, {"status": "ok", "name": name})
            return
        if route not in {"entries", "files"}:
            self._discard_body()
            self._send_error(HTTPStatus.NOT_FOUND, "unknown REST route", rest=True)
            return
        path = self._query_path(query)
        payload = self._json_body()
        if payload is None:
            return
        name_keys = [key for key in ("name", "fname") if key in payload]
        move_keys = [key for key in ("destination", "parent_path") if key in payload]
        if name_keys and move_keys:
            raise ValueError("choose either a new name or a move destination")
        if len(name_keys) > 1 or len(move_keys) > 1:
            raise ValueError("request contains multiple mutation targets")

        # A destination can be protected by a lock independently of the
        # source. Check both sides before making any WPS mutation, including
        # the exact child path for a parent-directory move.
        lock_paths = [path]
        source_parts = split_remote_path(path)
        if name_keys:
            name = payload[name_keys[0]]
            if not isinstance(name, str):
                raise ValueError("JSON field 'name' is required")
            lock_paths.append(join_remote_path(source_parts[:-1] + (name,)))
        elif "destination" in payload:
            destination = payload["destination"]
            if not isinstance(destination, str):
                raise ValueError("JSON field 'destination' must be a path")
            split_remote_path(destination)
            lock_paths.append(destination)
        elif "parent_path" in payload:
            parent_path = payload["parent_path"]
            if not isinstance(parent_path, str):
                raise ValueError("JSON field 'parent_path' must be a path")
            parent_parts = split_remote_path(parent_path)
            source_entry = self.application.storage.metadata(path)
            lock_paths.extend(
                (parent_path, join_remote_path(parent_parts + (source_entry.name,)))
            )
        else:
            raise ValueError("JSON field 'name', 'destination' or 'parent_path' is required")
        if not self._check_locks(*lock_paths, rest=True):
            return

        if name_keys:
            name = payload[name_keys[0]]
            if not isinstance(name, str):
                raise ValueError("JSON field 'name' is required")
            entry = self.application.storage.rename_path(path, name)
            parts = split_remote_path(path)
            new_path = join_remote_path(
                parts[:-1] + (entry.name,),
                trailing_slash=entry.kind == "folder",
            )
        elif "destination" in payload:
            destination = payload["destination"]
            if not isinstance(destination, str):
                raise ValueError("JSON field 'destination' must be a path")
            entry = self.application.storage.move_path(path, destination)
            new_path = join_remote_path(
                split_remote_path(destination),
                trailing_slash=entry.kind == "folder",
            )
        elif "parent_path" in payload:
            parent_path = payload["parent_path"]
            if not isinstance(parent_path, str):
                raise ValueError("JSON field 'parent_path' must be a path")
            entry = self.application.storage.move_to_parent_path(path, parent_path)
            new_path = join_remote_path(
                split_remote_path(parent_path) + (entry.name,),
                trailing_slash=entry.kind == "folder",
            )
        else:
            raise ValueError("JSON field 'name', 'destination' or 'parent_path' is required")
        self._send_json(
            HTTPStatus.OK,
            {"path": new_path, "entry": self._entry_json(entry)},
        )

    def do_OPTIONS(self) -> None:
        if not self._authorise():
            return
        self._send_bytes(
            HTTPStatus.OK,
            headers={
                "DAV": "1,2",
                "Allow": (
                    "OPTIONS, PROPFIND, GET, HEAD, PUT, MKCOL, DELETE, MOVE, "
                    "COPY, LOCK, UNLOCK"
                ),
            },
        )

    def do_PROPFIND(self) -> None:
        if not self._authorise():
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._do_propfind(path)
        except Exception as exc:
            self._handle_exception(exc)

    def do_GET(self) -> None:
        if self._is_health():
            self._handle_health()
            return
        if not self._authorise():
            return
        if self._is_web_app():
            self._handle_web_app()
            return
        rest = self._rest_route()
        if rest is not None:
            route, query = rest
            try:
                self._do_rest_get(route, query)
            except Exception as exc:
                self._handle_exception(exc, rest=True)
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._send_download(path)
        except Exception as exc:
            self._handle_exception(exc)

    def do_HEAD(self) -> None:
        if not self._authorise():
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            entry = self.application.storage.metadata(path)
            if entry.kind == "folder":
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", "httpd/unix-directory")
                self.send_header("Content-Length", "0")
                self.end_headers()
            else:
                self._send_download(path, head=True)
        except Exception as exc:
            self._handle_exception(exc)

    def do_PUT(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        rest = self._rest_route()
        try:
            if rest is not None:
                self._do_rest_put(*rest)
                return
            path = self._dav_path()
            if path is None:
                self.close_connection = True
                self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
                return
            self._do_webdav_put(path)
        except Exception as exc:
            self._handle_exception(exc, rest=rest is not None)

    def do_POST(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        rest = self._rest_route()
        if rest is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._do_rest_post(*rest)
        except Exception as exc:
            self._handle_exception(exc, rest=True)

    def do_DELETE(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        rest = self._rest_route()
        if rest is not None:
            try:
                self._do_rest_delete(*rest)
            except Exception as exc:
                self._handle_exception(exc, rest=True)
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._discard_body()
            if not self._check_locks(path):
                return
            self.application.storage.delete_path(path)
            self._send_bytes(HTTPStatus.NO_CONTENT)
        except Exception as exc:
            self._handle_exception(exc)

    def do_PATCH(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        rest = self._rest_route()
        if rest is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_IMPLEMENTED, "WPS rename/move is not available")
            return
        try:
            self._do_rest_patch(*rest)
        except Exception as exc:
            self._handle_exception(exc, rest=True)

    def do_MKCOL(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._do_webdav_mkcol(path)
        except Exception as exc:
            self._handle_exception(exc)

    def do_MOVE(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._do_webdav_move(path)
        except Exception as exc:
            self._handle_exception(exc)

    def do_COPY(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._do_webdav_copy(path)
        except Exception as exc:
            self._handle_exception(exc)

    def do_LOCK(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._do_lock(path)
        except Exception as exc:
            self._handle_exception(exc)

    def do_UNLOCK(self) -> None:
        if not self._authorise():
            return
        if not self._allow_mutation_origin():
            return
        path = self._dav_path()
        if path is None:
            self.close_connection = True
            self._send_error(HTTPStatus.NOT_FOUND, "unknown route")
            return
        try:
            self._do_unlock(path)
        except Exception as exc:
            self._handle_exception(exc)


def create_server(
    application: AdapterApplication,
    *,
    bind: str = "127.0.0.1",
    port: int = 54321,
    max_connections: int = 64,
    request_timeout: float = 60.0,
) -> AdapterHTTPServer:
    """Create a server without starting threads or making network calls."""

    if not 1 <= port <= 65535:
        raise ValueError("port must be between 1 and 65535")
    return AdapterHTTPServer(
        (bind, port),
        application,
        max_connections=max_connections,
        request_timeout=request_timeout,
    )


__all__ = ["AdapterApplication", "AdapterHTTPServer", "BasicAuth", "create_server"]
