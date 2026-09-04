"""Client for the WPS operations confirmed by browser captures.

The client deliberately keeps the WPS-specific surface small.  Credentials
can be loaded from files so a long-running adapter can use a newly exported
session without putting secrets on the command line or in the process
environment.  The WPS SDK's refresh-token grant is used after a 401 when a
file-backed session includes the browser's refresh cookie; interactive login
and SSO are intentionally outside this client.
"""

from __future__ import annotations

import base64
import email.utils
import json
import os
import re
import shlex
import shutil
import stat
import subprocess
import tempfile
import threading
import time
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import datetime, timezone
from hashlib import md5, sha1, sha256
from http.client import HTTPSConnection
from http.cookies import CookieError, SimpleCookie
from math import ceil
from pathlib import Path
from typing import Any, BinaryIO, Protocol
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, Request, build_opener
from xml.etree import ElementTree

from .provider import InsufficientStorageError, RemoteEntry, UnsupportedOperationError
from .workspace import DEFAULT_WORKSPACE_FILE, WorkspaceState


def _env_bool(name: str, *, default: bool) -> bool:
    value = os.environ.get(name)
    if value is None:
        return default
    normalized = value.strip().lower()
    if normalized in {"1", "true", "yes", "on"}:
        return True
    if normalized in {"0", "false", "no", "off"}:
        return False
    raise ValueError(f"{name} must be a boolean")


def _csrf_from_cookie(cookie: str) -> str:
    for item in cookie.split(";"):
        name, separator, value = item.strip().partition("=")
        if separator and name.strip().lower() == "csrf":
            return value.strip()
    return ""


MAX_CREDENTIAL_FILE_BYTES = 4 * 1024 * 1024
MAX_JSON_RESPONSE_BYTES = 8 * 1024 * 1024
MAX_OBJECT_RESPONSE_BYTES = 1 * 1024 * 1024
MAX_XML_RESPONSE_BYTES = 4 * 1024 * 1024
DEFAULT_MAX_UPLOAD_BYTES = 1024 * 1024 * 1024
MAX_MULTIPART_PART_BUFFER = 64 * 1024 * 1024
MAX_REMOTE_NAME_BYTES = 4096
MAX_REMOTE_ETAG_BYTES = 4096


def _validate_credential_parent(path: str, *, operation: str) -> None:
    """Require a private, real parent directory before reading a secret."""

    if not path or not os.path.isabs(path) or "\x00" in path:
        raise WpsApiError(operation)
    parent = os.path.dirname(path)
    if os.path.realpath(parent) != os.path.abspath(parent):
        raise WpsApiError(operation)
    try:
        metadata = os.stat(parent, follow_symlinks=False)
    except OSError as exc:
        raise WpsApiError(operation) from exc
    if (
        not stat.S_ISDIR(metadata.st_mode)
        or metadata.st_mode & 0o077
        or metadata.st_uid not in {0, os.getuid()}
    ):
        raise WpsApiError(operation)


def _read_credential_file(path: str, *, operation: str = "read credential file") -> str:
    """Read a credential file without following symlinks or trusting broad modes."""

    _validate_credential_parent(path, operation=operation)
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as exc:
        raise WpsApiError(operation) from exc
    try:
        metadata = os.fstat(descriptor)
        if (
            not stat.S_ISREG(metadata.st_mode)
            or metadata.st_mode & 0o077
            or metadata.st_uid not in {0, os.getuid()}
        ):
            raise WpsApiError(operation)
        with os.fdopen(descriptor, "r", encoding="utf-8") as stream:
            descriptor = -1
            value = stream.read(MAX_CREDENTIAL_FILE_BYTES + 1)
        if len(value) > MAX_CREDENTIAL_FILE_BYTES:
            raise WpsApiError(operation)
        return value.strip()
    except UnicodeError as exc:
        raise WpsApiError(operation) from exc
    finally:
        if descriptor != -1:
            os.close(descriptor)


def _validate_credential_values(credentials: WpsCredentials) -> WpsCredentials:
    """Prevent malformed file values from becoming outbound HTTP headers."""

    if len(credentials.cookie) > MAX_CREDENTIAL_FILE_BYTES or len(credentials.csrf_token) > MAX_CREDENTIAL_FILE_BYTES:
        raise WpsApiError("credential value is too large")
    if any(
        ord(char) < 0x20 or ord(char) == 0x7F
        for char in credentials.cookie + credentials.csrf_token
    ):
        raise WpsApiError("credential value contains a control character")
    return credentials


DEFAULT_OBJECT_STORAGE_HOST_SUFFIX = ".ag.kdocs.cn"


class _NoRedirectHandler(HTTPRedirectHandler):
    """Do not let a signed object request escape its verified host."""

    def redirect_request(self, *_args: Any, **_kwargs: Any) -> None:
        return None


class _Response(Protocol):
    headers: Any

    def read(self, size: int = -1) -> bytes: ...

    def close(self) -> None: ...


class _Opener(Protocol):
    def open(self, request: Request, timeout: float) -> _Response: ...


class _HttpsConnection(Protocol):
    def putrequest(self, method: str, url: str) -> None: ...

    def putheader(self, header: str, value: str) -> None: ...

    def endheaders(self) -> None: ...

    def send(self, data: bytes) -> None: ...

    def getresponse(self) -> Any: ...

    def close(self) -> None: ...


@dataclass(frozen=True, slots=True)
class WpsCredentials:
    """A point-in-time credential snapshot.

    Values are intentionally excluded from ``repr`` so accidental diagnostic
    output cannot reveal the browser session.
    """

    cookie: str = field(default="", repr=False)
    csrf_token: str = field(default="", repr=False)


class CredentialSource(Protocol):
    """Source for credentials and an optional, explicitly implemented refresh."""

    def get(self) -> WpsCredentials: ...

    def refresh(self) -> bool: ...

    def store_set_cookie_headers(self, headers: Any) -> bool: ...

    def replace_credentials(self, credentials: WpsCredentials) -> bool: ...


@dataclass(slots=True)
class FileCredentialSource:
    """Read session values from local files on every request.

    ``refresh`` can optionally invoke a locally configured helper, or detect
    files replaced by the operator while the service is running.  The WPS
    SDK refresh-token grant is performed by :class:`WpsDriveClient`, which
    persists any rotated Set-Cookie headers through this source.
    """

    cookie_path: str | None = None
    csrf_token_path: str | None = None
    refresh_command: tuple[str, ...] = ()
    refresh_timeout: float = 30.0
    _last: WpsCredentials | None = field(default=None, init=False, repr=False)
    _refresh_lock: threading.Lock = field(default_factory=threading.Lock, init=False, repr=False)

    @staticmethod
    def _read(path: str | None) -> str:
        if not path:
            return ""
        return _read_credential_file(path)

    def _snapshot(self) -> WpsCredentials:
        return WpsCredentials(
            cookie=self._read(self.cookie_path),
            csrf_token=self._read(self.csrf_token_path),
        )

    def get(self) -> WpsCredentials:
        credentials = self._snapshot()
        self._last = credentials
        return credentials

    @staticmethod
    def _header_values(headers: Any, name: str) -> tuple[str, ...]:
        if headers is None:
            return ()
        get_all = getattr(headers, "get_all", None)
        if callable(get_all):
            values = get_all(name)
            if values:
                if isinstance(values, str):
                    return (values,)
                return tuple(str(value) for value in values)
        if isinstance(headers, Mapping):
            for key, value in headers.items():
                if str(key).lower() == name.lower():
                    if isinstance(value, (list, tuple)):
                        return tuple(str(item) for item in value)
                    return (str(value),)
        getter = getattr(headers, "get", None)
        if callable(getter):
            value = getter(name)
            if value:
                return (str(value),)
        return ()

    @staticmethod
    def _cookie_map(cookie_header: str) -> tuple[dict[str, str], list[str]]:
        values: dict[str, str] = {}
        order: list[str] = []
        for item in cookie_header.split(";"):
            name, separator, value = item.strip().partition("=")
            if not separator or not name or any(char in name for char in " \t\r\n"):
                continue
            existing = next(
                (key for key in order if key.casefold() == name.casefold()),
                None,
            )
            if existing is None:
                order.append(name)
                existing = name
            values[existing] = value.strip()
        return values, order

    @staticmethod
    def _cookie_expired(morsel: Any) -> bool:
        max_age = str(morsel["max-age"] or "").strip()
        if max_age:
            try:
                return int(max_age) <= 0
            except ValueError:
                pass
        expires = str(morsel["expires"] or "").strip()
        if expires:
            try:
                expiry = email.utils.parsedate_to_datetime(expires)
                if expiry.tzinfo is None:
                    expiry = expiry.replace(tzinfo=timezone.utc)
                return expiry <= datetime.now(timezone.utc)
            except (TypeError, ValueError, OverflowError):
                pass
        return False

    @staticmethod
    def _write_atomic(path: str, value: str) -> None:
        target = Path(path)
        _validate_credential_parent(str(target), operation="write credential file")
        try:
            os.chmod(target.parent, 0o700)
        except OSError as exc:
            raise WpsApiError("protect credential directory") from exc
        descriptor, temporary = tempfile.mkstemp(
            prefix=f".{target.name}.",
            dir=str(target.parent),
            text=True,
        )
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                stream.write(value)
                stream.write("\n")
            os.replace(temporary, target)
        finally:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass

    def store_set_cookie_headers(self, headers: Any) -> bool:
        """Persist WPS session-cookie rotation without exposing cookie values."""

        if not self.cookie_path:
            return False
        set_cookie_headers = self._header_values(headers, "Set-Cookie")
        if not set_cookie_headers:
            return False

        updates: dict[str, str | None] = {}
        csrf_seen = False
        csrf_value: str | None = None
        for header in set_cookie_headers:
            jar = SimpleCookie()
            try:
                jar.load(header)
            except CookieError:
                continue
            for name, morsel in jar.items():
                expired = self._cookie_expired(morsel)
                updates[name] = None if expired else morsel.coded_value
                if name.casefold() == "csrf":
                    csrf_seen = True
                    csrf_value = "" if expired else morsel.value
        if not updates:
            return False

        with self._refresh_lock:
            current = self._snapshot()
            values, order = self._cookie_map(current.cookie)
            positions = {name.casefold(): name for name in order}
            for name, value in updates.items():
                stored_name = positions.get(name.casefold())
                if stored_name is None:
                    stored_name = name
                    positions[name.casefold()] = stored_name
                    order.append(stored_name)
                if value is None:
                    values.pop(stored_name, None)
                    order = [item for item in order if item != stored_name]
                    positions.pop(name.casefold(), None)
                else:
                    values[stored_name] = value
            new_cookie = "; ".join(
                f"{name}={values[name]}" for name in order if name in values
            )
            cookie_changed = new_cookie != current.cookie
            csrf_changed = (
                csrf_seen
                and self.csrf_token_path is not None
                and (csrf_value or "") != current.csrf_token
            )
            if not cookie_changed and not csrf_changed:
                return False
            if cookie_changed:
                self._write_atomic(self.cookie_path, new_cookie)
            if csrf_changed and self.csrf_token_path is not None:
                self._write_atomic(self.csrf_token_path, csrf_value or "")
            self._last = self._snapshot()
            return True

    def replace_credentials(self, credentials: WpsCredentials) -> bool:
        """Replace the credential pair after a local interactive login."""

        if not self.cookie_path or not self.csrf_token_path:
            return False
        if not credentials.cookie or not credentials.csrf_token:
            return False
        with self._refresh_lock:
            current = self._snapshot()
            if current == credentials:
                self._last = current
                return True
            try:
                self._write_atomic(self.cookie_path, credentials.cookie)
                self._write_atomic(self.csrf_token_path, credentials.csrf_token)
            except Exception:
                # Keep a failed import from leaving one half of the pair newer
                # than the other.  The original error remains the useful one.
                try:
                    self._write_atomic(self.cookie_path, current.cookie)
                    self._write_atomic(self.csrf_token_path, current.csrf_token)
                except Exception:
                    pass
                raise
            self._last = credentials
            return True

    def refresh(self) -> bool:
        with self._refresh_lock:
            before = self._last or self._snapshot()
            if self.refresh_command:
                if self.refresh_timeout <= 0:
                    raise ValueError("refresh_timeout must be positive")
                try:
                    completed = subprocess.run(
                        list(self.refresh_command),
                        check=False,
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.DEVNULL,
                        timeout=self.refresh_timeout,
                    )
                except (OSError, subprocess.TimeoutExpired):
                    return False
                if completed.returncode != 0:
                    return False
            current = self._snapshot()
            self._last = current
            return bool(current.cookie) and current != before


@dataclass(slots=True)
class StaticCredentialSource:
    """Small adapter useful for embedding and tests."""

    credentials: WpsCredentials

    def get(self) -> WpsCredentials:
        return self.credentials

    def refresh(self) -> bool:
        return False

    def store_set_cookie_headers(self, _headers: Any) -> bool:
        return False

    def replace_credentials(self, _credentials: WpsCredentials) -> bool:
        return False


class WpsApiError(RuntimeError):
    """An API or transport error without echoing response contents."""

    def __init__(
        self,
        operation: str,
        *,
        status: int | None = None,
        category: str = "upstream",
    ) -> None:
        self.operation = operation
        self.status = status
        self.category = category
        suffix = f" (HTTP {status})" if status is not None else ""
        super().__init__(f"WPS operation failed: {operation}{suffix}")


def _read_limited_response(
    response: Any,
    *,
    max_bytes: int,
    operation: str,
    error_category: str = "upstream",
) -> bytes:
    """Read a bounded upstream control response without trusting its length."""

    if max_bytes <= 0:
        raise ValueError("max_bytes must be positive")
    headers = getattr(response, "headers", None)
    if headers is not None:
        try:
            declared = int(headers.get("Content-Length", ""))
        except (AttributeError, TypeError, ValueError):
            declared = None
        if declared is not None and (declared < 0 or declared > max_bytes):
            raise WpsApiError(operation, category=error_category)

    body = bytearray()
    while len(body) <= max_bytes:
        try:
            chunk = response.read(min(64 * 1024, max_bytes + 1 - len(body)))
        except (OSError, ValueError) as exc:
            raise WpsApiError(operation, category=error_category) from exc
        if not chunk:
            return bytes(body)
        if not isinstance(chunk, bytes):
            raise WpsApiError(operation, category=error_category)
        body.extend(chunk)
        if len(body) > max_bytes:
            raise WpsApiError(operation, category=error_category)
    raise WpsApiError(operation, category=error_category)


@dataclass(frozen=True, slots=True)
class WpsClientConfig:
    """Connection settings; cookie values are deliberately excluded from repr."""

    group_id: str
    cookie: str = field(default="", repr=False)
    csrf_token: str = field(default="", repr=False)
    cookie_file: str | None = None
    csrf_token_file: str | None = None
    credential_source: CredentialSource | None = field(default=None, repr=False, compare=False)
    base_url: str = "https://365.kdocs.cn"
    account_base_url: str | None = None
    auto_refresh: bool = True
    referer: str | None = None
    origin: str | None = None
    cid: str | None = None
    timeout: float = 30.0
    status_probe_ttl: float = 30.0
    status_failure_backoff: float = 5.0
    upload_spool_memory: int = 8 * 1024 * 1024
    stream_chunk_size: int = 1024 * 1024
    multipart_threshold: int = 50 * 1024 * 1024
    multipart_part_size: int = 10 * 1024 * 1024
    enable_range: bool = True
    upload_spool_dir: str | None = None
    upload_resume_dir: str | None = None
    upload_min_free_bytes: int = 512 * 1024 * 1024
    max_upload_bytes: int = DEFAULT_MAX_UPLOAD_BYTES
    upload_retries: int = 2
    upload_retry_delay: float = 0.5
    credential_refresh_command: tuple[str, ...] = ()
    credential_refresh_timeout: float = 30.0
    object_storage_host_suffix: str = DEFAULT_OBJECT_STORAGE_HOST_SUFFIX
    max_json_response_bytes: int = MAX_JSON_RESPONSE_BYTES
    workspace: WorkspaceState | None = field(default=None, repr=False, compare=False)

    @classmethod
    def from_env(cls) -> "WpsClientConfig":
        refresh_command_text = os.environ.get("WPS_CREDENTIAL_REFRESH_COMMAND", "").strip()
        refresh_command = tuple(shlex.split(refresh_command_text)) if refresh_command_text else ()
        group_id = os.environ.get("WPS_GROUP_ID", "")
        root_id = os.environ.get("WPS_ROOT_ID", "0")
        workspace_path = os.environ.get("WPS_WORKSPACE_FILE") or DEFAULT_WORKSPACE_FILE
        workspace = (
            WorkspaceState.from_file(
                workspace_path,
                configured_group_id=group_id,
                configured_root_id=root_id,
            )
            if workspace_path and (group_id in {"", "auto"} or root_id == "auto" or os.path.exists(workspace_path))
            else None
        )
        cookie_file = os.environ.get("WPS_COOKIE_FILE") or None
        csrf_token_file = os.environ.get("WPS_CSRF_TOKEN_FILE") or None
        return cls(
            group_id=group_id,
            cookie=os.environ.get("WPS_COOKIE", ""),
            csrf_token=os.environ.get("WPS_CSRF_TOKEN", ""),
            cookie_file=cookie_file,
            csrf_token_file=csrf_token_file,
            credential_source=(
                FileCredentialSource(
                    cookie_path=cookie_file,
                    csrf_token_path=csrf_token_file,
                    refresh_command=refresh_command,
                    refresh_timeout=float(os.environ.get("WPS_CREDENTIAL_REFRESH_TIMEOUT", "30")),
                )
                if cookie_file or csrf_token_file or refresh_command
                else None
            ),
            base_url=os.environ.get("WPS_BASE_URL", "https://365.kdocs.cn"),
            account_base_url=os.environ.get("WPS_ACCOUNT_BASE_URL") or None,
            object_storage_host_suffix=os.environ.get(
                "WPS_OBJECT_STORAGE_HOST_SUFFIX",
                DEFAULT_OBJECT_STORAGE_HOST_SUFFIX,
            ),
            auto_refresh=_env_bool("WPS_AUTO_REFRESH", default=True),
            referer=os.environ.get("WPS_REFERER") or None,
            origin=os.environ.get("WPS_ORIGIN") or None,
            cid=os.environ.get("WPS_CID") or None,
            timeout=float(os.environ.get("WPS_TIMEOUT", "30")),
            status_probe_ttl=float(os.environ.get("WPS_STATUS_PROBE_TTL", "30")),
            status_failure_backoff=float(
                os.environ.get("WPS_STATUS_FAILURE_BACKOFF", "5")
            ),
            upload_spool_memory=int(os.environ.get("WPS_UPLOAD_SPOOL_MEMORY", str(8 * 1024 * 1024))),
            stream_chunk_size=int(os.environ.get("WPS_STREAM_CHUNK_SIZE", str(1024 * 1024))),
            multipart_threshold=int(os.environ.get("WPS_MULTIPART_THRESHOLD", str(50 * 1024 * 1024))),
            multipart_part_size=int(os.environ.get("WPS_MULTIPART_PART_SIZE", str(10 * 1024 * 1024))),
            enable_range=_env_bool("WPS_ENABLE_RANGE", default=True),
            upload_spool_dir=os.environ.get("WPS_UPLOAD_SPOOL_DIR") or None,
            upload_resume_dir=os.environ.get("WPS_UPLOAD_RESUME_DIR") or None,
            upload_min_free_bytes=int(
                os.environ.get("WPS_UPLOAD_MIN_FREE_BYTES", str(512 * 1024 * 1024))
            ),
            max_upload_bytes=int(
                os.environ.get("WPS_MAX_UPLOAD_BYTES", str(DEFAULT_MAX_UPLOAD_BYTES))
            ),
            upload_retries=int(os.environ.get("WPS_UPLOAD_RETRIES", "2")),
            upload_retry_delay=float(os.environ.get("WPS_UPLOAD_RETRY_DELAY", "0.5")),
            credential_refresh_command=refresh_command,
            credential_refresh_timeout=float(
                os.environ.get("WPS_CREDENTIAL_REFRESH_TIMEOUT", "30")
            ),
            max_json_response_bytes=int(
                os.environ.get("WPS_MAX_JSON_RESPONSE_BYTES", str(MAX_JSON_RESPONSE_BYTES))
            ),
            workspace=workspace,
        )


@dataclass(frozen=True, slots=True)
class ListPage:
    entries: tuple[RemoteEntry, ...]
    next_offset: int | None
    next_filter: str | None
    result: str | None


@dataclass(frozen=True, slots=True)
class WpsWorkspaceCandidate:
    """A workspace returned by the OpenList-compatible discovery candidate.

    Discovery only proves that WPS listed the group for the current account.
    ``verified`` becomes true only after a read of the selected group's root.
    """

    group_id: str
    name: str
    company_id: str
    source: str = "openlist-candidate"
    verified: bool = False

    @property
    def status(self) -> str:
        return "verified" if self.verified else "candidate"


@dataclass(frozen=True, slots=True)
class WpsStatus:
    """A deliberately redacted result of the WPS session preflight."""

    status: str
    wps: str
    workspace: str
    account_type: str
    last_checked_at: int | None
    retry_after: int = 0

    def as_dict(self) -> dict[str, object]:
        return {
            "status": self.status,
            "wps": self.wps,
            "workspace": self.workspace,
            "account_type": self.account_type,
            "last_checked_at": self.last_checked_at,
            "retry_after": self.retry_after,
        }

    def with_retry_after(self, value: int) -> "WpsStatus":
        return WpsStatus(
            status=self.status,
            wps=self.wps,
            workspace=self.workspace,
            account_type=self.account_type,
            last_checked_at=self.last_checked_at,
            retry_after=max(0, int(value)),
        )


@dataclass(frozen=True, slots=True)
class UploadOptions:
    """Captured-shape defaults for the normal upload fallback.

    The endpoint and field names are observed; defaults for optional control
    fields remain provisional until a second successful replay confirms them.
    """

    parent_path: tuple[str, ...] = ()
    req_by_internal: bool = False
    client_stores: str = ""
    startswithfilename: str = ""
    successactionstatus: int = 200
    file_id: int = 0
    with_rapid: bool = True
    tried_store: tuple[str, ...] = ()
    is_up_new_ver: bool = False


@dataclass(slots=True)
class DownloadStream:
    """A streaming object-storage response returned by the download API."""

    response: _Response
    status: str | None
    content_type: str | None
    content_length: int | None
    http_status: int = 200
    content_range: str | None = None
    _on_close: Callable[[], None] | None = field(default=None, repr=False)
    _closed: bool = field(default=False, init=False, repr=False)

    def read(self, size: int = -1) -> bytes:
        return self.response.read(size)

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            self.response.close()
        finally:
            if self._on_close is not None:
                self._on_close()

    def __enter__(self) -> "DownloadStream":
        return self

    def __exit__(self, _exc_type: Any, _exc_value: Any, _traceback: Any) -> None:
        self.close()


class _UploadSpoolReservation:
    """Reserve temporary-disk capacity across concurrent uploads."""

    def __init__(self, client: "WpsDriveClient") -> None:
        self.client = client
        self.reserved = 0

    def reserve(self, total: int) -> None:
        self.reserved = self.client._reserve_spool_bytes(total, self.reserved)

    def __enter__(self) -> "_UploadSpoolReservation":
        return self

    def __exit__(self, _exc_type: Any, _exc_value: Any, _traceback: Any) -> None:
        self.client._release_spool_bytes(self.reserved)


class WpsDriveClient:
    """Client for observed list, download, and normal-upload endpoints."""

    def __init__(
        self,
        config: WpsClientConfig,
        *,
        opener: _Opener | None = None,
        https_connection_factory: Callable[[str, int | None, float], _HttpsConnection] | None = None,
    ) -> None:
        if not config.group_id and config.workspace is None:
            raise ValueError("group_id or workspace state is required")
        if config.max_json_response_bytes <= 0:
            raise ValueError("max_json_response_bytes must be positive")
        base_parts = urlsplit(config.base_url)
        if (
            base_parts.scheme != "https"
            or not base_parts.hostname
            or base_parts.username
            or base_parts.password
            or base_parts.query
            or base_parts.fragment
            or base_parts.path not in {"", "/"}
            or not self._is_wps_host(base_parts.hostname)
        ):
            raise ValueError("base_url must be an HTTPS WPS host without a path or credentials")
        object_suffix = config.object_storage_host_suffix.strip().lstrip(".").rstrip(".").casefold()
        if not object_suffix or not (
            object_suffix == "kdocs.cn" or object_suffix.endswith(".kdocs.cn")
        ):
            raise ValueError("object_storage_host_suffix must be within kdocs.cn")
        if config.status_probe_ttl < 0:
            raise ValueError("status_probe_ttl must not be negative")
        if config.status_failure_backoff < 0:
            raise ValueError("status_failure_backoff must not be negative")
        self.config = config
        self._opener = opener or build_opener(_NoRedirectHandler())
        self._signed_opener = self._opener
        self._credential_refresh_lock = threading.Lock()
        self._status_condition = threading.Condition()
        self._status_inflight = False
        self._status_cache: WpsStatus | None = None
        self._status_cache_until = 0.0
        self._status_cache_marker: tuple[str, str, str, str] | None = None
        self._spool_reservation_lock = threading.Lock()
        self._reserved_spool_bytes = 0
        self._https_connection_factory = https_connection_factory or (
            lambda host, port, timeout: HTTPSConnection(host, port=port, timeout=timeout)
        )

    @property
    def group_id(self) -> str:
        configured = self.config.group_id
        if configured in {"", "auto"}:
            value = self.config.workspace.group_id if self.config.workspace is not None else ""
        else:
            value = configured
        if not value:
            raise WpsApiError("WPS workspace is not configured", status=503)
        return value

    def _credentials(self) -> WpsCredentials:
        if self.config.credential_source is not None:
            credentials = self.config.credential_source.get()
            if not credentials.cookie and self.config.cookie:
                credentials = WpsCredentials(
                    cookie=self.config.cookie,
                    csrf_token=credentials.csrf_token,
                )
            if not credentials.csrf_token and self.config.csrf_token:
                credentials = WpsCredentials(
                    cookie=credentials.cookie,
                    csrf_token=self.config.csrf_token,
                )
        else:
            credentials = WpsCredentials(
                cookie=(
                    _read_credential_file(self.config.cookie_file)
                    if self.config.cookie_file
                    else self.config.cookie
                ),
                csrf_token=(
                    _read_credential_file(self.config.csrf_token_file)
                    if self.config.csrf_token_file
                    else self.config.csrf_token
                ),
            )
        if not credentials.csrf_token:
            credentials = WpsCredentials(
                cookie=credentials.cookie,
                csrf_token=_csrf_from_cookie(credentials.cookie),
            )
        return _validate_credential_values(credentials)

    def _status_credentials_are_missing(self) -> bool:
        source = self.config.credential_source
        paths: list[str] = []
        if isinstance(source, FileCredentialSource):
            paths.extend(
                path
                for path in (source.cookie_path, source.csrf_token_path)
                if path
            )
        else:
            paths.extend(
                path
                for path in (self.config.cookie_file, self.config.csrf_token_file)
                if path
            )
        if paths:
            return any(not os.path.exists(path) for path in paths)
        return not bool(self.config.cookie)

    @staticmethod
    def _status_truth(value: object) -> bool | None:
        if isinstance(value, bool):
            return value
        if isinstance(value, int) and not isinstance(value, bool):
            return value != 0
        if isinstance(value, str):
            normalized = value.strip().casefold()
            if normalized in {"1", "true", "yes", "on", "ok", "success", "logged_in"}:
                return True
            if normalized in {"0", "false", "no", "off", "logout", "logged_out"}:
                return False
        return None

    @classmethod
    def _status_account_type(cls, payload: Mapping[str, Any]) -> str:
        for key in ("is_company_account", "is_business_account"):
            if key not in payload:
                continue
            marker = cls._status_truth(payload[key])
            if marker is True:
                return "business"
            if marker is False:
                return "personal"
        for key in ("companyid", "current_companyid", "company_id"):
            value = payload.get(key)
            if value is None or value == 0:
                continue
            if isinstance(value, str) and value.strip() in {"", "0"}:
                continue
            return "business"
        return "unknown"

    def _login_preflight(self) -> str:
        """Check the account session without starting a refresh grant."""

        payload = self._request_json(
            "/api/v3/islogin",
            base_url=self._account_base_url(),
            retry_on_401=False,
        )
        # The observed/OpenList-compatible response does not consistently
        # include a boolean marker. Treat a successful JSON object as logged
        # in, while honoring the marker when a deployment provides it.
        if "islogin" in payload:
            logged_in = self._status_truth(payload["islogin"])
            if logged_in is None:
                raise WpsApiError("parse WPS login status", category="invalid_response")
            if not logged_in:
                raise WpsApiError(
                    "WPS login preflight",
                    status=401,
                    category="session_expired",
                )
        return self._status_account_type(payload)

    def check_login(self) -> str:
        """Run the read-only WPS account login check.

        The return value is the coarse account type only; response identifiers
        remain inside the client for later workspace discovery.
        """

        return self._login_preflight()

    @staticmethod
    def _discovery_id(value: object, *, operation: str) -> str:
        """Accept only bounded, path-safe identifiers from discovery JSON."""

        if not isinstance(value, bool) and isinstance(value, (int, str)):
            text = str(value).strip()
        else:
            raise WpsApiError(operation, category="invalid_response")
        if (
            not text
            or len(text) > 256
            or "/" in text
            or "\\" in text
            or any(ord(char) < 0x20 or ord(char) == 0x7F for char in text)
        ):
            raise WpsApiError(operation, category="invalid_response")
        return text

    @staticmethod
    def _discovery_name(value: object, *, operation: str) -> str:
        if (
            not isinstance(value, str)
            or not value.strip()
            or len(value.encode("utf-8")) > MAX_REMOTE_NAME_BYTES
            or any(ord(char) < 0x20 or ord(char) == 0x7F for char in value)
        ):
            raise WpsApiError(operation, category="invalid_response")
        return value.strip()

    def discover_spaces_candidate(
        self,
        *,
        company_id: str,
        enabled: bool = False,
    ) -> tuple[WpsWorkspaceCandidate, ...]:
        """List account-visible enterprise groups using an opt-in candidate API.

        This endpoint is borrowed from OpenList and is not treated as a
        verified WPS contract.  It is therefore disabled unless the caller
        explicitly opts in.  Results are never persisted or used as the
        active workspace by this method.
        """

        operation = "discover WPS workspaces"
        if not enabled:
            raise WpsApiError(operation, category="disabled")
        company_text = self._discovery_id(company_id, operation=operation)
        payload = self._request_json(
            "/3rd/plus/groups/v1/companies/"
            f"{quote(company_text, safe='')}/users/self/groups/private",
            retry_on_401=False,
        )
        result = payload.get("result")
        if result is not None and result != "ok":
            raise WpsApiError(operation, category="invalid_response")
        raw_groups = payload.get("groups")
        if not isinstance(raw_groups, list):
            raise WpsApiError(operation, category="invalid_response")

        candidates: list[WpsWorkspaceCandidate] = []
        seen: set[str] = set()
        for item in raw_groups:
            if not isinstance(item, Mapping):
                raise WpsApiError(operation, category="invalid_response")
            group_value = item.get("id", item.get("group_id"))
            group_text = self._discovery_id(group_value, operation=operation)
            if group_text in seen:
                raise WpsApiError(operation, category="invalid_response")
            seen.add(group_text)
            name = self._discovery_name(item.get("name"), operation=operation)
            candidates.append(
                WpsWorkspaceCandidate(
                    group_id=group_text,
                    name=name,
                    company_id=company_text,
                )
            )
        return tuple(candidates)

    def verify_workspace_candidate(
        self,
        candidate: WpsWorkspaceCandidate,
        *,
        root_id: str = "0",
    ) -> WpsWorkspaceCandidate:
        """Verify a discovered group without changing the active workspace."""

        if not isinstance(candidate, WpsWorkspaceCandidate):
            raise TypeError("candidate must be a WpsWorkspaceCandidate")
        operation = "verify WPS workspace candidate"
        group_id = self._discovery_id(candidate.group_id, operation=operation)
        company_id = self._discovery_id(candidate.company_id, operation=operation)
        name = self._discovery_name(candidate.name, operation=operation)
        self.list_entries(root_id, count=1, group_id=group_id)
        return WpsWorkspaceCandidate(
            group_id=group_id,
            name=name,
            company_id=company_id,
            source=candidate.source,
            verified=True,
        )

    @staticmethod
    def _status_from_error(
        exc: WpsApiError,
        *,
        workspace_phase: bool,
        account_type: str,
        checked_at: int,
    ) -> WpsStatus:
        if exc.status == 401 or exc.category == "session_expired":
            return WpsStatus(
                status="session_expired",
                wps="session_expired",
                workspace="unknown",
                account_type=account_type,
                last_checked_at=checked_at,
            )
        if workspace_phase and exc.status in {403, 404}:
            return WpsStatus(
                status="permission_denied",
                wps="connected",
                workspace="permission_denied",
                account_type=account_type,
                last_checked_at=checked_at,
            )
        if exc.category == "invalid_response":
            return WpsStatus(
                status="invalid_response",
                wps="unknown",
                workspace="unknown",
                account_type=account_type,
                last_checked_at=checked_at,
            )
        return WpsStatus(
            status="upstream_unavailable",
            wps="unknown",
            workspace="unknown",
            account_type=account_type,
            last_checked_at=checked_at,
        )

    def _probe_status(self, *, root_id: str, checked_at: int) -> WpsStatus:
        try:
            account_type = self.check_login()
        except WpsApiError as exc:
            return self._status_from_error(
                exc,
                workspace_phase=False,
                account_type="unknown",
                checked_at=checked_at,
            )

        try:
            # A single root listing proves that the selected group/root is
            # readable without fetching a complete directory.
            self.list_entries(root_id, count=1)
        except WpsApiError as exc:
            return self._status_from_error(
                exc,
                workspace_phase=True,
                account_type=account_type,
                checked_at=checked_at,
            )
        return WpsStatus(
            status="connected",
            wps="connected",
            workspace="ready",
            account_type=account_type,
            last_checked_at=checked_at,
        )

    def check_status(self, *, root_id: str = "0") -> WpsStatus:
        """Return a cached, redacted WPS login and workspace status.

        The account preflight is read-only and deliberately does not trigger
        the refresh-token grant. Normal file requests retain the existing
        refresh-on-401 behavior.
        """

        if not isinstance(root_id, str) or not root_id:
            raise ValueError("root_id is required")
        checked_at = int(time.time())
        try:
            credentials = self._credentials()
        except WpsApiError:
            if self._status_credentials_are_missing():
                return WpsStatus(
                    status="not_configured",
                    wps="not_configured",
                    workspace="not_configured",
                    account_type="unknown",
                    last_checked_at=checked_at,
                )
            return WpsStatus(
                status="invalid_response",
                wps="unknown",
                workspace="unknown",
                account_type="unknown",
                last_checked_at=checked_at,
            )
        if not credentials.cookie:
            return WpsStatus(
                status="not_configured",
                wps="not_configured",
                workspace="not_configured",
                account_type="unknown",
                last_checked_at=checked_at,
            )
        try:
            group_id = self.group_id
        except WpsApiError:
            return WpsStatus(
                status="not_configured",
                wps="not_configured",
                workspace="not_configured",
                account_type="unknown",
                last_checked_at=checked_at,
            )
        marker = (credentials.cookie, credentials.csrf_token, group_id, root_id)

        with self._status_condition:
            if marker != self._status_cache_marker:
                self._status_cache = None
                self._status_cache_until = 0.0
                self._status_cache_marker = marker
            now = time.monotonic()
            if self._status_cache is not None and self._status_cache_until > now:
                remaining = ceil(self._status_cache_until - now)
                return self._status_cache.with_retry_after(
                    0 if self._status_cache.status == "connected" else remaining
                )
            if self._status_inflight:
                deadline = now + max(self.config.timeout, 1.0) + 1.0
                while self._status_inflight:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        return WpsStatus(
                            status="upstream_unavailable",
                            wps="unknown",
                            workspace="unknown",
                            account_type="unknown",
                            last_checked_at=checked_at,
                            retry_after=1,
                        )
                    self._status_condition.wait(timeout=remaining)
                now = time.monotonic()
                if self._status_cache is not None and self._status_cache_until > now:
                    remaining = ceil(self._status_cache_until - now)
                    return self._status_cache.with_retry_after(
                        0 if self._status_cache.status == "connected" else remaining
                    )
            self._status_inflight = True

        try:
            result = self._probe_status(root_id=root_id, checked_at=checked_at)
        except Exception:
            result = WpsStatus(
                status="upstream_unavailable",
                wps="unknown",
                workspace="unknown",
                account_type="unknown",
                last_checked_at=checked_at,
            )
        with self._status_condition:
            self._status_cache = result
            cache_duration = (
                self.config.status_probe_ttl
                if result.status == "connected"
                else self.config.status_failure_backoff
            )
            self._status_cache_until = time.monotonic() + cache_duration
            self._status_cache_marker = marker
            self._status_inflight = False
            self._status_condition.notify_all()
        return result

    def _refresh_credentials(self) -> bool:
        # Several request threads can observe the same expired session. Keep
        # refresh grants serial so a rotated rtk cookie cannot be overwritten
        # by a concurrent grant response.
        with self._credential_refresh_lock:
            source = self.config.credential_source
            if source is not None and source.refresh():
                return True
            if not self.config.auto_refresh:
                return False
            return self._refresh_wps_session()

    def _persist_set_cookie_headers(self, headers: Any) -> bool:
        source = self.config.credential_source
        if source is None:
            return False
        store = getattr(source, "store_set_cookie_headers", None)
        if not callable(store):
            return False
        try:
            return bool(store(headers))
        except (OSError, WpsApiError):
            return False

    def _account_base_url(self) -> str:
        base_url = self.config.account_base_url
        if not base_url:
            hostname = urlsplit(self.config.base_url).hostname
            labels = hostname.split(".") if hostname else []
            if len(labels) < 2:
                raise WpsApiError("resolve account refresh URL")
            base_url = "https://account." + ".".join(labels[-2:])
        parts = urlsplit(base_url)
        if (
            parts.scheme != "https"
            or not parts.hostname
            or parts.username
            or parts.password
            or parts.query
            or parts.fragment
            or parts.path not in {"", "/"}
            or not self._is_wps_host(parts.hostname)
        ):
            raise WpsApiError("resolve account refresh URL")
        return base_url.rstrip("/")

    @staticmethod
    def _is_wps_host(host: str) -> bool:
        normalized = host.rstrip(".").casefold()
        return normalized == "kdocs.cn" or normalized.endswith(".kdocs.cn")

    def _refresh_wps_session(self) -> bool:
        """Use the WPS SDK refresh-token grant and persist rotated cookies."""

        credentials = self._credentials()
        if not credentials.cookie:
            return False
        request = Request(
            self._account_base_url() + "/passport/secure/api/grant_token",
            data=b'{"grant_type":"refresh_token"}',
            method="POST",
        )
        request.add_header("Accept", "application/json")
        request.add_header("Content-Type", "application/json")
        request.add_header("Cookie", credentials.cookie)
        if self.config.referer:
            request.add_header("Referer", self.config.referer)
        if self.config.origin:
            request.add_header("Origin", self.config.origin)
        try:
            response = self._opener.open(request, timeout=self.config.timeout)
        except HTTPError as exc:
            exc.close()
            return False
        except URLError:
            return False
        try:
            response_status = getattr(response, "status", 200)
            if not isinstance(response_status, int) or response_status != 200:
                return False
            headers = response.headers
            _read_limited_response(
                response,
                max_bytes=self.config.max_json_response_bytes,
                operation="refresh WPS session",
            )
        finally:
            response.close()
        return self._persist_set_cookie_headers(headers)

    @staticmethod
    def _refresh_json_body(body: bytes | None, csrf_token: str) -> bytes | None:
        """Replace a stale CSRF field when a request is retried after 401."""

        if not body or not csrf_token:
            return body
        try:
            payload = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            return body
        if not isinstance(payload, dict) or not isinstance(
            payload.get("csrfmiddlewaretoken"), str
        ):
            return body
        payload["csrfmiddlewaretoken"] = csrf_token
        return json.dumps(payload, ensure_ascii=True, separators=(",", ":")).encode("utf-8")

    def _url(
        self,
        path: str,
        query: Sequence[tuple[str, str]] = (),
        *,
        base_url: str | None = None,
    ) -> str:
        url = (base_url or self.config.base_url).rstrip("/") + "/" + path.lstrip("/")
        encoded_query = urlencode(query)
        return f"{url}?{encoded_query}" if encoded_query else url

    def _request_json(
        self,
        path: str,
        *,
        method: str = "GET",
        query: Sequence[tuple[str, str]] = (),
        body: bytes | None = None,
        base_url: str | None = None,
        retry_on_401: bool = True,
    ) -> dict[str, Any]:
        url = self._url(path, query, base_url=base_url)
        credentials = self._credentials()
        response = None
        for attempt in range(2):
            request = Request(url, data=body, method=method)
            request.add_header("Accept", "*/*")
            if body is not None:
                request.add_header("Content-Type", "application/json")
            if credentials.cookie:
                request.add_header("Cookie", credentials.cookie)
            if self.config.referer:
                request.add_header("Referer", self.config.referer)
            if self.config.origin:
                request.add_header("Origin", self.config.origin)

            try:
                response = self._opener.open(request, timeout=self.config.timeout)
                self._persist_set_cookie_headers(response.headers)
                break
            except HTTPError as exc:
                rotated = self._persist_set_cookie_headers(exc.headers)
                exc.close()
                if exc.code == 401 and retry_on_401 and attempt == 0 and (
                    rotated or self._refresh_credentials()
                ):
                    credentials = self._credentials()
                    body = self._refresh_json_body(body, credentials.csrf_token)
                    continue
                raise WpsApiError(path, status=exc.code, category="http") from None
            except URLError as exc:
                raise WpsApiError(path, category="unavailable") from exc
            except OSError as exc:
                raise WpsApiError(path, category="unavailable") from exc

        if response is None:
            raise WpsApiError(path, status=401)

        try:
            payload = _read_limited_response(
                response,
                max_bytes=self.config.max_json_response_bytes,
                operation=path,
                error_category="invalid_response",
            )
        finally:
            response.close()

        try:
            decoded = json.loads(payload.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise WpsApiError(path, category="invalid_response") from exc
        if not isinstance(decoded, dict):
            raise WpsApiError(path, category="invalid_response")
        return decoded

    @staticmethod
    def _bool(value: bool) -> str:
        return "true" if value else "false"

    @staticmethod
    def _json_id(value: str) -> int | str:
        return int(value) if value.isdecimal() else value

    @staticmethod
    def _entry_from_item(item: Mapping[str, Any]) -> RemoteEntry:
        if item.get("id") is None:
            raise WpsApiError("normalize file metadata")
        name = item.get("fname", "")
        if (
            not isinstance(name, str)
            or not name
            or name in {".", ".."}
            or "/" in name
            or "\\" in name
            or "\x00" in name
            or any(ord(char) < 0x20 or ord(char) == 0x7F for char in name)
            or len(name.encode("utf-8")) > MAX_REMOTE_NAME_BYTES
        ):
            raise WpsApiError("normalize file metadata")
        kind = item.get("ftype")
        normalized_kind = kind if isinstance(kind, str) and kind in {"file", "folder"} else "unknown"
        raw_size = item.get("fsize")
        size = raw_size if isinstance(raw_size, int) and not isinstance(raw_size, bool) and raw_size >= 0 else None
        raw_etag = item.get("fsha")
        etag = (
            raw_etag
            if isinstance(raw_etag, str)
            and len(raw_etag.encode("utf-8")) <= MAX_REMOTE_ETAG_BYTES
            and not any(ord(char) < 0x20 or ord(char) == 0x7F for char in raw_etag)
            else None
        )
        return RemoteEntry(
            id=str(item["id"]),
            name=name,
            kind=normalized_kind,
            parent_id=str(item["parentid"]) if item.get("parentid") is not None else None,
            size=size,
            modified_at=str(item["mtime"]) if item.get("mtime") is not None else None,
            etag=etag,
            link_id=str(item["link_id"]) if item.get("link_id") else None,
        )

    def list_entries(
        self,
        parent_id: str,
        *,
        group_id: str | None = None,
        offset: int = 0,
        count: int = 20,
        orderby: str = "mtime",
        order: str = "desc",
        linkgroup: bool | None = None,
        include: str | None = None,
        with_link: bool | None = None,
        review_pic_thumbnail: bool | None = None,
        with_sharefolder_type: bool | None = None,
    ) -> ListPage:
        """List a remote folder using the captured v5 endpoint shape."""

        query: list[tuple[str, str]] = [
            ("parentid", parent_id),
            ("offset", str(offset)),
            ("count", str(count)),
            ("orderby", orderby),
            ("order", order),
        ]
        optional = (
            ("linkgroup", linkgroup),
            ("include", include),
            ("with_link", with_link),
            ("review_pic_thumbnail", review_pic_thumbnail),
            ("with_sharefolder_type", with_sharefolder_type),
        )
        for name, value in optional:
            if value is None:
                continue
            query.append((name, self._bool(value) if isinstance(value, bool) else str(value)))

        selected_group_id = self.group_id if group_id is None else group_id
        payload = self._request_json(
            f"/3rd/drive/api/v5/groups/{quote(selected_group_id, safe='')}/files",
            query=query,
        )
        raw_entries = payload.get("files", [])
        if not isinstance(raw_entries, list):
            raise WpsApiError("list files")

        entries = []
        for item in raw_entries:
            if not isinstance(item, Mapping) or item.get("id") is None:
                continue
            entries.append(self._entry_from_item(item))

        next_offset = payload.get("next_offset")
        if not isinstance(next_offset, int):
            next_offset = None
        next_filter = payload.get("next_filter")
        if not isinstance(next_filter, str):
            next_filter = None
        result = payload.get("result")
        if not isinstance(result, str):
            result = None
        if result not in {None, "ok"}:
            raise WpsApiError("list files")
        return ListPage(tuple(entries), next_offset, next_filter, result)

    def iter_entries(
        self,
        parent_id: str,
        *,
        count: int = 100,
        max_entries: int | None = None,
        orderby: str = "mtime",
        order: str = "desc",
        linkgroup: bool | None = None,
        include: str | None = None,
        with_link: bool | None = None,
        review_pic_thumbnail: bool | None = None,
        with_sharefolder_type: bool | None = None,
    ) -> Sequence[RemoteEntry]:
        """Fetch all pages while guarding against a broken repeated cursor."""

        if count <= 0:
            raise ValueError("count must be positive")
        if max_entries is not None and max_entries <= 0:
            raise ValueError("max_entries must be positive")
        entries: list[RemoteEntry] = []
        offset = 0
        seen_offsets: set[int] = set()
        page_count = 0
        page_limit = max_entries + 1 if max_entries is not None else 10000
        while True:
            page_count += 1
            if page_count > page_limit:
                raise InsufficientStorageError("remote folder pagination exceeds the configured limit")
            page = self.list_entries(
                parent_id,
                offset=offset,
                count=count,
                orderby=orderby,
                order=order,
                linkgroup=linkgroup,
                include=include,
                with_link=with_link,
                review_pic_thumbnail=review_pic_thumbnail,
                with_sharefolder_type=with_sharefolder_type,
            )
            if max_entries is not None and len(entries) + len(page.entries) > max_entries:
                raise InsufficientStorageError("remote folder exceeds the configured entry limit")
            entries.extend(page.entries)
            next_offset = page.next_offset
            if next_offset is None or next_offset < 0 or next_offset in seen_offsets:
                return tuple(entries)
            seen_offsets.add(next_offset)
            if next_offset <= offset:
                return tuple(entries)
            offset = next_offset

    def _csrf(self, csrf_token: str | None) -> str:
        token = csrf_token or self._credentials().csrf_token
        if not token:
            raise ValueError("csrf_token is required for write operation")
        return token

    @staticmethod
    def _normalise_etag(value: str) -> str:
        return value.strip().strip('"')

    def _check_upload_budget(self, total: int) -> None:
        """Reject an upload before its temporary spool can exhaust the host."""

        if total < 0:
            raise ValueError("upload size must not be negative")
        if self.config.max_upload_bytes < 0:
            raise ValueError("max_upload_bytes must not be negative")
        if self.config.upload_min_free_bytes < 0:
            raise ValueError("upload_min_free_bytes must not be negative")
        if self.config.max_upload_bytes and total > self.config.max_upload_bytes:
            raise InsufficientStorageError("upload exceeds the configured size limit")

        # SpooledTemporaryFile rolls the complete buffer onto disk once it
        # crosses max_size, so reserve the complete upload rather than only
        # the bytes beyond the in-memory threshold.
        if total <= self.config.upload_spool_memory:
            return
        spool_dir = self.config.upload_spool_dir or tempfile.gettempdir()
        try:
            free_bytes = shutil.disk_usage(spool_dir).free
        except OSError as exc:
            raise InsufficientStorageError("upload spool directory is unavailable") from exc
        required = total + self.config.upload_min_free_bytes
        if free_bytes < required:
            raise InsufficientStorageError("not enough free space for the upload spool")

    def _reserve_spool_bytes(self, total: int, current: int) -> int:
        """Reserve this upload's complete spool plus the configured free-space reserve."""

        if total <= self.config.upload_spool_memory:
            return current
        spool_dir = self.config.upload_spool_dir or tempfile.gettempdir()
        required = total + self.config.upload_min_free_bytes
        with self._spool_reservation_lock:
            try:
                free_bytes = shutil.disk_usage(spool_dir).free
            except OSError as exc:
                raise InsufficientStorageError("upload spool directory is unavailable") from exc
            other_reservations = self._reserved_spool_bytes - current
            if free_bytes - other_reservations < required:
                raise InsufficientStorageError("not enough free space for concurrent upload spools")
            self._reserved_spool_bytes += required - current
        return required

    def _release_spool_bytes(self, reserved: int) -> None:
        if reserved <= 0:
            return
        with self._spool_reservation_lock:
            self._reserved_spool_bytes = max(0, self._reserved_spool_bytes - reserved)

    def _retry_delay(self, attempt: int) -> None:
        if attempt and self.config.upload_retry_delay:
            time.sleep(self.config.upload_retry_delay * (2 ** (attempt - 1)))

    def _signed_target(self, signed_url: str, operation: str) -> tuple[str, int | None, str]:
        if any(ord(char) < 0x20 or ord(char) == 0x7F for char in signed_url):
            raise WpsApiError(operation)
        parts = urlsplit(signed_url)
        try:
            port = parts.port
        except ValueError:
            raise WpsApiError(operation) from None
        host = parts.hostname
        suffix = (
            self.config.object_storage_host_suffix.strip()
            .lstrip(".")
            .rstrip(".")
            .casefold()
        )
        host_allowed = bool(
            suffix
            and host
            and (
                host.rstrip(".").casefold() == suffix
                or host.rstrip(".").casefold().endswith("." + suffix)
            )
        )
        if (
            parts.scheme != "https"
            or not host_allowed
            or parts.username
            or parts.password
            or parts.fragment
            or port not in {None, 443}
        ):
            raise WpsApiError(operation)
        target = parts.path or "/"
        if parts.query:
            target += "?" + parts.query
        return host, port, target

    @staticmethod
    def _range_response_matches(
        value: str | None,
        *,
        offset: int,
        length: int | None,
        content_length: int | None,
    ) -> bool:
        if not value or content_length is None or content_length < 0:
            return False
        match = re.fullmatch(r"bytes (\d+)-(\d+)/(\d+|\*)", value.strip())
        if match is None:
            return False
        start, end = int(match.group(1)), int(match.group(2))
        total = match.group(3)
        if start != offset or end < start or end - start + 1 != content_length:
            return False
        if length is not None and content_length != length:
            return False
        return total == "*" or int(total) > end

    def _put_signed_object(self, signed_url: str, source: BinaryIO, size: int) -> tuple[str | None, str | None]:
        host, port, target = self._signed_target(signed_url, "resolve object upload URL")
        connection = self._https_connection_factory(host, port, self.config.timeout)
        try:
            connection.putrequest("PUT", target)
            connection.putheader("Content-Type", "application/octet-stream")
            connection.putheader("Content-Length", str(size))
            connection.endheaders()
            while True:
                chunk = source.read(self.config.stream_chunk_size)
                if not chunk:
                    break
                connection.send(chunk)
            response = connection.getresponse()
            response_headers = {
                str(name).lower(): str(value)
                for name, value in response.getheaders()
            }
            _read_limited_response(
                response,
                max_bytes=MAX_OBJECT_RESPONSE_BYTES,
                operation="object upload",
            )
            if response.status != 200:
                raise WpsApiError("object upload", status=response.status)
            return response_headers.get("etag"), response_headers.get("x-obs-save-key")
        finally:
            connection.close()

    def _put_signed_part(
        self,
        signed_url: str,
        data: bytes,
        *,
        content_md5: str,
    ) -> str:
        host, port, target = self._signed_target(signed_url, "resolve multipart part URL")
        connection = self._https_connection_factory(host, port, self.config.timeout)
        try:
            connection.putrequest("PUT", target)
            connection.putheader("Content-MD5", content_md5)
            connection.putheader("Content-Type", "application/octet-stream")
            connection.putheader("Content-Length", str(len(data)))
            connection.endheaders()
            for offset in range(0, len(data), self.config.stream_chunk_size):
                connection.send(data[offset : offset + self.config.stream_chunk_size])
            response = connection.getresponse()
            response_headers = {
                str(name).lower(): str(value)
                for name, value in response.getheaders()
            }
            _read_limited_response(
                response,
                max_bytes=MAX_OBJECT_RESPONSE_BYTES,
                operation="multipart part upload",
            )
            if response.status != 200:
                raise WpsApiError("multipart part upload", status=response.status)
            etag = response_headers.get("etag")
            if not etag:
                raise WpsApiError("multipart part response missing ETag")
            return self._normalise_etag(etag)
        finally:
            connection.close()

    def _post_signed_data(
        self,
        signed_url: str,
        body: bytes,
        *,
        content_type: str,
    ) -> bytes:
        host, port, target = self._signed_target(signed_url, "resolve multipart merge URL")
        connection = self._https_connection_factory(host, port, self.config.timeout)
        try:
            connection.putrequest("POST", target)
            connection.putheader("Content-Type", content_type)
            connection.putheader("Content-Length", str(len(body)))
            connection.endheaders()
            for offset in range(0, len(body), self.config.stream_chunk_size):
                connection.send(body[offset : offset + self.config.stream_chunk_size])
            response = connection.getresponse()
            response_body = _read_limited_response(
                response,
                max_bytes=MAX_XML_RESPONSE_BYTES,
                operation="multipart merge",
            )
            if response.status != 200:
                raise WpsApiError("multipart merge", status=response.status)
            return response_body
        finally:
            connection.close()

    @staticmethod
    def _multipart_etag(response_body: bytes) -> str:
        if b"<!doctype" in response_body.lower() or b"<!entity" in response_body.lower():
            raise WpsApiError("parse multipart merge response")
        try:
            root = ElementTree.fromstring(response_body)
        except (ElementTree.ParseError, UnicodeDecodeError) as exc:
            raise WpsApiError("parse multipart merge response") from exc
        for element in root.iter():
            if element.tag.rsplit("}", 1)[-1] == "ETag" and element.text:
                return WpsDriveClient._normalise_etag(element.text)
        raise WpsApiError("multipart merge response missing ETag")

    def create_folder(
        self,
        parent_id: str,
        name: str,
        *,
        csrf_token: str | None = None,
    ) -> RemoteEntry:
        """Create a folder using the request captured from the WPS UI."""

        if not name or "/" in name or "\\" in name:
            raise ValueError("name must be one remote folder name")
        csrf = self._csrf(csrf_token)
        body = {
            "groupid": self._json_id(self.group_id),
            "parentid": self._json_id(parent_id),
            "name": name,
            "owner": True,
            "parsed": True,
            "csrfmiddlewaretoken": csrf,
        }
        payload = self._request_json(
            "/3rd/drive/api/v5/files/folder",
            method="POST",
            body=json.dumps(body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
        )
        if payload.get("result") not in {None, "ok"}:
            raise WpsApiError("create folder")
        return self._entry_from_item(payload)

    def rename(
        self,
        file_id: str,
        name: str,
        *,
        csrf_token: str | None = None,
    ) -> RemoteEntry:
        """Rename one file or folder using the confirmed v3 endpoint."""

        if not file_id:
            raise ValueError("file_id is required")
        if not name or "/" in name or "\\" in name:
            raise ValueError("name must be one remote entry name")
        csrf = self._csrf(csrf_token)
        body = {
            "fname": name,
            "csrfmiddlewaretoken": csrf,
        }
        payload = self._request_json(
            f"/3rd/drive/api/v3/groups/{quote(self.group_id, safe='')}/files/"
            f"{quote(str(file_id), safe='')}",
            method="PUT",
            body=json.dumps(body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
        )
        if payload.get("result") not in {None, "ok"}:
            raise WpsApiError("rename file")
        return self._entry_from_item(payload)

    def _wait_for_task(
        self,
        task_uuid: str,
        *,
        operation: str,
        poll_interval: float,
        poll_timeout: float,
    ) -> None:
        deadline = time.monotonic() + poll_timeout
        while True:
            progress = self._request_json(
                "/3rd/drive/api/v5/files/batch/task/progress",
                query=(("taskuuid", task_uuid),),
            )
            if progress.get("result") not in {None, "ok"}:
                raise WpsApiError(f"{operation} progress")
            status = progress.get("status")
            failed_list = progress.get("failed_list")
            if progress.get("finish") == 1 or status == "success":
                if failed_list not in (None, []):
                    raise WpsApiError(operation, status=409)
                return
            if status in {"failed", "error"}:
                raise WpsApiError(f"{operation} task")
            if time.monotonic() >= deadline:
                raise WpsApiError(f"{operation} task timeout")
            if poll_interval:
                time.sleep(poll_interval)

    def move(
        self,
        file_id: str,
        source_parent_id: str,
        destination_parent_id: str,
        *,
        destination_group_id: str | None = None,
        csrf_token: str | None = None,
        option: Mapping[str, Any] | None = None,
        poll_interval: float = 0.5,
        poll_timeout: float = 60.0,
    ) -> None:
        """Move one entry and wait for the observed task to finish."""

        if not file_id:
            raise ValueError("file_id is required")
        if not source_parent_id or not destination_parent_id:
            raise ValueError("source and destination parent IDs are required")
        if poll_interval < 0:
            raise ValueError("poll_interval must not be negative")
        if poll_timeout <= 0:
            raise ValueError("poll_timeout must be positive")
        csrf = self._csrf(csrf_token)
        body = {
            "groupid": self._json_id(self.group_id),
            "parentid": self._json_id(source_parent_id),
            "dst_groupid": self._json_id(destination_group_id or self.group_id),
            "dst_parentid": self._json_id(destination_parent_id),
            "fileids": [self._json_id(file_id)],
            "option": dict(option or {}),
            "csrfmiddlewaretoken": csrf,
        }
        task = self._request_json(
            "/3rd/drive/api/v5/files/batch/task/move",
            method="POST",
            body=json.dumps(body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
        )
        if task.get("result") not in {None, "ok"}:
            raise WpsApiError("move file")
        task_uuid = task.get("taskuuid")
        if not isinstance(task_uuid, str) or not task_uuid:
            raise WpsApiError("move file task")
        self._wait_for_task(
            task_uuid,
            operation="move file",
            poll_interval=poll_interval,
            poll_timeout=poll_timeout,
        )

    def delete(
        self,
        file_id: str,
        *,
        csrf_token: str | None = None,
        poll_interval: float = 0.5,
        poll_timeout: float = 60.0,
    ) -> None:
        """Delete one file or folder and wait for the observed task to finish."""

        if not file_id:
            raise ValueError("file_id is required")
        if poll_interval < 0:
            raise ValueError("poll_interval must not be negative")
        if poll_timeout <= 0:
            raise ValueError("poll_timeout must be positive")
        csrf = self._csrf(csrf_token)
        body = {
            "fileids": [self._json_id(file_id)],
            "groupid": self._json_id(self.group_id),
            "csrfmiddlewaretoken": csrf,
        }
        task = self._request_json(
            "/3rd/drive/api/v5/files/batch/task/delete",
            method="POST",
            body=json.dumps(body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
        )
        if task.get("result") not in {None, "ok"}:
            raise WpsApiError("delete file")
        task_uuid = task.get("taskuuid")
        if not isinstance(task_uuid, str) or not task_uuid:
            raise WpsApiError("delete file task")

        self._wait_for_task(
            task_uuid,
            operation="delete file",
            poll_interval=poll_interval,
            poll_timeout=poll_timeout,
        )

    def _multipart_part_size(self, total: int, limit: Mapping[str, Any]) -> int:
        try:
            min_size = int(limit["min_part_size"])
            max_size = int(limit["max_part_size"])
            max_parts = int(limit["max_parts"])
        except (KeyError, TypeError, ValueError) as exc:
            raise WpsApiError("parse multipart limits") from exc
        if min_size <= 0 or max_size < min_size or max_parts <= 0:
            raise WpsApiError("invalid multipart limits")
        if self.config.multipart_part_size <= 0:
            raise ValueError("multipart_part_size must be positive")
        part_size = max(self.config.multipart_part_size, min_size)
        part_size = max(part_size, (total + max_parts - 1) // max_parts)
        if part_size > max_size:
            raise WpsApiError("file exceeds multipart size limits")
        if part_size > MAX_MULTIPART_PART_BUFFER:
            raise InsufficientStorageError("multipart part exceeds the memory safety limit")
        return part_size

    def _multipart_upload(
        self,
        spool: BinaryIO,
        *,
        total: int,
        name: str,
        parent_id: str,
        sha1_hex: str,
        options: UploadOptions,
        csrf: str,
        resume_identity: str,
    ) -> RemoteEntry:
        """Upload a large file through the captured block/multipart flow."""

        group_text = str(self.group_id)
        parent_text = str(parent_id)
        resume_path: Path | None = None
        if self.config.upload_resume_dir:
            resume_root = Path(self.config.upload_resume_dir)
            if not resume_root.is_absolute():
                raise ValueError("upload_resume_dir must be absolute")
            resume_path = resume_root / (sha256(resume_identity.encode()).hexdigest() + ".json")

        state: dict[str, Any] | None = None
        if resume_path is not None:
            try:
                metadata = resume_path.stat()
                if metadata.st_mode & 0o077 or metadata.st_uid not in {0, os.getuid()}:
                    raise OSError("insecure resume checkpoint permissions")
                candidate = json.loads(resume_path.read_text(encoding="utf-8"))
                if (
                    isinstance(candidate, dict)
                    and candidate.get("version") == 1
                    and candidate.get("identity") == resume_identity
                    and isinstance(candidate.get("parts"), dict)
                    and all(isinstance(k, str) and k.isdigit() and isinstance(v, str)
                            for k, v in candidate["parts"].items())
                ):
                    state = candidate
            except (OSError, UnicodeError, json.JSONDecodeError):
                state = None

        def save_state() -> None:
            if resume_path is None or state is None:
                return
            resume_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            temporary = resume_path.with_name("." + resume_path.name + ".tmp")
            temporary.write_text(json.dumps(state, ensure_ascii=True, sort_keys=True), encoding="utf-8")
            temporary.chmod(0o600)
            temporary.replace(resume_path)

        if state is None or not all(
            isinstance(state.get(k), str) and state[k]
            for k in ("upload_id", "key", "store")
        ):
            init_body = {
                "with_rapid": options.with_rapid, "hash": sha1_hex, "size": total,
                "group_id": group_text, "name": name, "parent_id": parent_text,
                "tried_store": list(options.tried_store), "csrfmiddlewaretoken": csrf,
            }
            init_payload = self._request_json(
                "/3rd/drive/api/v5/files/upload/block", method="POST",
                body=json.dumps(init_body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
            )
            if init_payload.get("result") not in {None, "ok"}:
                raise WpsApiError("initialize multipart upload")
            upload_id, key, store, limit = (init_payload.get(k) for k in ("upload_id", "key", "store", "limit"))
            if not all(isinstance(v, str) and v for v in (upload_id, key, store)) or not isinstance(limit, Mapping):
                raise WpsApiError("multipart initialization response is incomplete")
            part_size = self._multipart_part_size(total, limit)
            state = {"version": 1, "identity": resume_identity, "upload_id": upload_id,
                     "key": key, "store": store, "part_size": part_size, "parts": {}}
            save_state()
        else:
            upload_id, key, store = state["upload_id"], state["key"], state["store"]
            try:
                part_size = int(state["part_size"])
            except (KeyError, TypeError, ValueError):
                raise WpsApiError("invalid multipart resume checkpoint")
        part_infos: list[dict[str, int | str]] = []
        completed = state["parts"]

        part_number = 1
        while True:
            known_etag = completed.get(str(part_number))
            if known_etag:
                part_infos.append({"etag": known_etag, "part_number": part_number})
                part_number += 1
                if (part_number - 1) * part_size >= total:
                    break
                continue
            spool.seek((part_number - 1) * part_size)
            data = spool.read(part_size)
            if not data:
                break
            if not isinstance(data, bytes):
                raise TypeError("upload spool must return bytes")
            part_md5_hex = md5(data).hexdigest()
            last_error: Exception | None = None
            session_reset = False
            for attempt in range(self.config.upload_retries + 1):
                try:
                    block_body = {
                        "key": key,
                        "md5": part_md5_hex,
                        "part_number": part_number,
                        "part_size": len(data),
                        "req_by_internal": options.req_by_internal,
                        "store": store,
                        "upload_id": upload_id,
                        "csrfmiddlewaretoken": csrf,
                    }
                    block_payload = self._request_json(
                        "/3rd/drive/api/v5/files/upload/block",
                        method="PUT",
                        body=json.dumps(
                            block_body, ensure_ascii=True, separators=(",", ":")
                        ).encode("utf-8"),
                    )
                    if block_payload.get("result") not in {None, "ok"}:
                        raise WpsApiError("get multipart part URL")
                    part_url = block_payload.get("url")
                    method = block_payload.get("method")
                    request_info = block_payload.get("request")
                    response_info = block_payload.get("response")
                    if method != "PUT" or not isinstance(part_url, str):
                        raise WpsApiError("invalid multipart part instruction")
                    if not isinstance(request_info, Mapping) or request_info.get("body_type") != "file":
                        raise WpsApiError("invalid multipart part request instruction")
                    if not isinstance(response_info, Mapping):
                        raise WpsApiError("invalid multipart part response instruction")
                    expected_codes = response_info.get("expect_code", [200])
                    if not isinstance(expected_codes, list) or not expected_codes or expected_codes[0] != 200:
                        raise WpsApiError("unsupported multipart part status")
                    part_headers = request_info.get("headers")
                    if not isinstance(part_headers, Mapping):
                        raise WpsApiError("multipart part headers missing")
                    content_md5 = part_headers.get("Content-MD5") or part_headers.get("content-md5")
                    content_type = part_headers.get("Content-Type") or part_headers.get("content-type")
                    expected_md5 = base64.b64encode(bytes.fromhex(part_md5_hex)).decode("ascii")
                    if content_md5 != expected_md5 or content_type != "application/octet-stream":
                        raise WpsApiError("multipart part headers do not match content")
                    etag = self._put_signed_part(part_url, data, content_md5=content_md5)
                    break
                except (OSError, WpsApiError) as exc:
                    last_error = exc
                    if (
                        isinstance(exc, WpsApiError)
                        and exc.status in {400, 404, 410}
                        and resume_path is not None
                        and not session_reset
                    ):
                        init_body = {
                            "with_rapid": options.with_rapid, "hash": sha1_hex,
                            "size": total, "group_id": group_text, "name": name,
                            "parent_id": parent_text,
                            "tried_store": list(options.tried_store),
                            "csrfmiddlewaretoken": csrf,
                        }
                        fresh = self._request_json(
                            "/3rd/drive/api/v5/files/upload/block", method="POST",
                            body=json.dumps(init_body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
                        )
                        if fresh.get("result") not in {None, "ok"}:
                            raise WpsApiError("reinitialize multipart upload")
                        new_upload, new_key, new_store = (fresh.get(k) for k in ("upload_id", "key", "store"))
                        limit = fresh.get("limit")
                        if not all(isinstance(v, str) and v for v in (new_upload, new_key, new_store)) or not isinstance(limit, Mapping):
                            raise WpsApiError("reinitialize multipart response is incomplete")
                        upload_id, key, store = new_upload, new_key, new_store
                        part_size = self._multipart_part_size(total, limit)
                        completed.clear()
                        part_infos.clear()
                        state.update({"upload_id": upload_id, "key": key, "store": store,
                                      "part_size": part_size})
                        save_state()
                        session_reset = True
                        break
                    if attempt >= self.config.upload_retries:
                        raise
                    self._retry_delay(attempt + 1)
            else:
                raise last_error or WpsApiError("multipart part upload")
            if session_reset:
                part_number = 1
                continue
            part_infos.append({"etag": etag, "part_number": part_number})
            completed[str(part_number)] = etag
            save_state()
            part_number += 1

        merge_body = {
            "key": key,
            "req_by_internal": options.req_by_internal,
            "store": store,
            "part_infos": part_infos,
            "upload_id": upload_id,
            "csrfmiddlewaretoken": csrf,
        }
        merge_payload = self._request_json(
            "/3rd/drive/api/v5/files/upload/block/merge",
            method="POST",
            body=json.dumps(merge_body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
        )
        if merge_payload.get("result") not in {None, "ok"}:
            raise WpsApiError("prepare multipart merge")
        merge_url = merge_payload.get("url")
        merge_method = merge_payload.get("method")
        merge_request = merge_payload.get("request")
        merge_response = merge_payload.get("response")
        merge_body_data = merge_request.get("body_data") if isinstance(merge_request, Mapping) else None
        merge_headers = merge_request.get("headers") if isinstance(merge_request, Mapping) else None
        if merge_method != "POST" or not isinstance(merge_url, str):
            raise WpsApiError("invalid multipart merge instruction")
        if not isinstance(merge_request, Mapping) or merge_request.get("body_type") != "data":
            raise WpsApiError("invalid multipart merge request instruction")
        if not isinstance(merge_body_data, str) or not isinstance(merge_headers, Mapping):
            raise WpsApiError("multipart merge body is missing")
        merge_content_type = merge_headers.get("Content-Type") or merge_headers.get("content-type")
        if merge_content_type != "application/xml":
            raise WpsApiError("unsupported multipart merge content type")
        if not isinstance(merge_response, Mapping):
            raise WpsApiError("invalid multipart merge response instruction")
        expected_codes = merge_response.get("expect_code", [200])
        if not isinstance(expected_codes, list) or not expected_codes or expected_codes[0] != 200:
            raise WpsApiError("unsupported multipart merge status")
        merged_body = self._post_signed_data(
            merge_url,
            merge_body_data.encode("utf-8"),
            content_type=merge_content_type,
        )
        merged_etag = self._multipart_etag(merged_body)

        file_body = {
            "key": key,
            "groupid": group_text,
            "parentid": parent_text,
            "name": name,
            "parent_path": list(options.parent_path),
            "sha1": sha1_hex,
            "size": total,
            "store": store,
            "etag": merged_etag,
            "isUpNewVer": options.is_up_new_ver,
            "apiErrorInfo": None,
            "csrfmiddlewaretoken": csrf,
        }
        final_payload = self._request_json(
            "/3rd/drive/api/v5/files/file",
            method="POST",
            body=json.dumps(file_body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
        )
        if final_payload.get("result") not in {None, "ok"}:
            raise WpsApiError("register multipart file")
        entry = self._entry_from_item(final_payload)
        if resume_path is not None:
            try:
                resume_path.unlink()
            except FileNotFoundError:
                pass
        return entry

    def upload(
        self,
        parent_id: str,
        name: str,
        source: BinaryIO,
        *,
        size: int | None = None,
        content_type: str | None = None,
        csrf_token: str | None = None,
        options: UploadOptions | None = None,
        overwrite: bool = False,
    ) -> RemoteEntry:
        """Upload through the observed normal-upload fallback.

        The source is spooled temporarily because WPS asks for checksums before
        returning the signed object-storage URL. The spool is closed and
        removed when this method returns or raises.
        """

        if not name or "/" in name or "\\" in name:
            raise ValueError("name must be one remote file name")
        if self.config.stream_chunk_size <= 0:
            raise ValueError("stream_chunk_size must be positive")
        if self.config.upload_spool_memory < 0:
            raise ValueError("upload_spool_memory must not be negative")
        if self.config.upload_min_free_bytes < 0:
            raise ValueError("upload_min_free_bytes must not be negative")
        if self.config.max_upload_bytes < 0:
            raise ValueError("max_upload_bytes must not be negative")
        if self.config.upload_retries < 0:
            raise ValueError("upload_retries must not be negative")
        if self.config.upload_retry_delay < 0:
            raise ValueError("upload_retry_delay must not be negative")
        if self.config.multipart_threshold <= 0:
            raise ValueError("multipart_threshold must be positive")
        if self.config.multipart_part_size <= 0:
            raise ValueError("multipart_part_size must be positive")
        if size is not None:
            self._check_upload_budget(size)
        options = options or UploadOptions()
        if overwrite:
            options = UploadOptions(
                parent_path=options.parent_path,
                req_by_internal=options.req_by_internal,
                client_stores=options.client_stores or "ks3,ks3sh",
                startswithfilename=options.startswithfilename or name,
                successactionstatus=201,
                file_id=options.file_id,
                with_rapid=options.with_rapid,
                tried_store=options.tried_store or ("ks3,ks3sh",),
                is_up_new_ver=options.is_up_new_ver,
            )
        csrf = self._csrf(csrf_token)
        content_type = content_type or "application/octet-stream"

        hasher_md5 = md5()
        hasher_sha1 = sha1()
        hasher_sha256 = sha256()
        total = 0
        spool_dir = self.config.upload_spool_dir or tempfile.gettempdir()
        with _UploadSpoolReservation(self) as reservation, tempfile.SpooledTemporaryFile(
            max_size=self.config.upload_spool_memory,
            mode="w+b",
            dir=spool_dir,
        ) as spool:
            while True:
                chunk = source.read(self.config.stream_chunk_size)
                if not chunk:
                    break
                if not isinstance(chunk, bytes):
                    raise TypeError("source must return bytes")
                self._check_upload_budget(total + len(chunk))
                reservation.reserve(total + len(chunk))
                spool.write(chunk)
                hasher_md5.update(chunk)
                hasher_sha1.update(chunk)
                hasher_sha256.update(chunk)
                total += len(chunk)
            if size is not None and size != total:
                raise ValueError(f"source size mismatch: expected {size}, read {total}")

            md5_hex = hasher_md5.hexdigest()
            sha1_hex = hasher_sha1.hexdigest()
            sha256_hex = hasher_sha256.hexdigest()
            group_value = self._json_id(self.group_id)
            parent_value = self._json_id(parent_id)

            try:
                pre_check = self._request_json(
                    "/3rd/drive/api/v5/files/upload/pre_check",
                    query=(
                        ("file_name", name),
                        ("group_id", str(group_value)),
                        ("parent_id", str(parent_value)),
                    ),
                )
            except WpsApiError as exc:
                if not (overwrite and exc.status == 403):
                    raise
                pre_check = {"result": "ok"}
            if pre_check.get("result") not in {None, "ok"}:
                raise WpsApiError("upload pre-check")

            if total >= self.config.multipart_threshold:
                if overwrite:
                    raise UnsupportedOperationError(
                        "multipart overwrite is disabled until independently verified"
                    )
                return self._multipart_upload(
                    spool,
                    total=total,
                    name=name,
                    parent_id=parent_id,
                    sha1_hex=sha1_hex,
                    options=options,
                    csrf=csrf,
                    resume_identity=f"{group_value}:{parent_value}:{name}:{total}:{sha1_hex}",
                )

            create_body = {
                "groupid": group_value,
                "parentid": parent_value,
                "parent_path": list(options.parent_path),
                "size": total,
                "name": name,
                "req_by_internal": options.req_by_internal,
                "client_stores": options.client_stores,
                "contenttype": content_type,
                "startswithfilename": options.startswithfilename,
                "successactionstatus": options.successactionstatus,
                "group_id": group_value,
                "parent_id": parent_value,
                "file_id": options.file_id,
                "with_rapid": options.with_rapid,
                "tried_store": list(options.tried_store),
                "sha256": sha256_hex,
                "csrfmiddlewaretoken": csrf,
            }
            if overwrite:
                create_body["md5"] = md5_hex
            def create_upload_instruction() -> tuple[dict[str, Any], str]:
                result = self._request_json(
                    "/3rd/drive/api/v5/files/upload/create_update",
                    method="PUT",
                    body=json.dumps(
                        create_body, ensure_ascii=True, separators=(",", ":")
                    ).encode("utf-8"),
                )
                url = result.get("url")
                if not isinstance(url, str):
                    raise WpsApiError("create upload URL")
                response_meta = result.get("response")
                expected_codes = (
                    response_meta.get("expect_code", [200])
                    if isinstance(response_meta, Mapping)
                    else [200]
                )
                expected_code = (
                    expected_codes[0]
                    if isinstance(expected_codes, list) and expected_codes
                    else 200
                )
                if expected_code != 200:
                    raise WpsApiError("unsupported object upload status")
                return result, url

            create_result, signed_url = create_upload_instruction()
            last_error: Exception | None = None
            etag: str | None = None
            for attempt in range(self.config.upload_retries + 1):
                try:
                    spool.seek(0)
                    etag, save_key = self._put_signed_object(signed_url, spool, total)
                    break
                except (OSError, WpsApiError) as exc:
                    last_error = exc
                    if attempt >= self.config.upload_retries:
                        raise
                    self._retry_delay(attempt + 1)
                    create_result, signed_url = create_upload_instruction()
            else:
                raise last_error or WpsApiError("object upload")
            if etag is None:
                raise WpsApiError("object upload response missing ETag")

            file_body = {
                "key": sha1_hex,
                "groupid": group_value,
                "parentid": parent_value,
                "name": name,
                "parent_path": list(options.parent_path),
                "sha1": sha1_hex,
                "size": total,
                "store": create_result.get("store", ""),
                "etag": etag,
                "isUpNewVer": options.is_up_new_ver,
                "apiErrorInfo": None,
                "csrfmiddlewaretoken": csrf,
            }
            final_payload = self._request_json(
                "/3rd/drive/api/v5/files/file",
                method="POST",
                body=json.dumps(file_body, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
            )
            if final_payload.get("result") not in {None, "ok"}:
                raise WpsApiError("register uploaded file")
            return self._entry_from_item(final_payload)

    def open_download(
        self,
        file_id: str,
        *,
        offset: int = 0,
        length: int | None = None,
        checksums: Sequence[str] = ("md5", "sha1", "sha224", "sha256", "sha384", "sha512"),
        get_direct_external_download_url: bool | None = None,
        cid: str | None = None,
    ) -> DownloadStream:
        """Resolve and open a download stream without forwarding WPS cookies.

        Browser captures show two behaviors: some files return a signed URL
        when the direct-download flag is omitted, while others require the
        flag to be ``true``. The default follows the browser request and
        retries the observed 403 variant with the flag enabled.
        """

        if offset < 0:
            raise ValueError("offset must not be negative")
        if length is not None and length <= 0:
            raise ValueError("length must be positive")
        if (offset or length is not None) and not self.config.enable_range:
            raise WpsApiError("range download is disabled until independently verified")

        if cid is None:
            cid = self.config.cid
        path = f"/api/v3/office/file/{quote(str(file_id), safe='')}/download"

        def resolve(direct: bool | None) -> dict[str, Any]:
            query: list[tuple[str, str]] = [("support_checksums", ",".join(checksums))]
            if direct is not None:
                query.append(("get_direct_external_download_url", self._bool(direct)))
            if cid is not None:
                query.append(("cid", cid))
            return self._request_json(path, query=query)

        try:
            payload = resolve(get_direct_external_download_url)
        except WpsApiError as exc:
            if get_direct_external_download_url is None and exc.status == 403:
                payload = resolve(True)
            else:
                raise
        download_url = payload.get("download_url") or payload.get("url")
        if not isinstance(download_url, str) or not download_url.startswith("https://"):
            raise WpsApiError("resolve download URL")
        self._signed_target(download_url, "resolve download URL")

        # The returned URL is already signed. Sending the WPS Cookie to the
        # object-storage host is unnecessary and increases credential exposure.
        range_requested = bool(offset or length is not None)
        request = Request(download_url, method="GET")
        request.add_header("Accept", "*/*")
        if range_requested:
            end = "" if length is None else str(offset + length - 1)
            request.add_header("Range", f"bytes={offset}-{end}")
        try:
            response = self._signed_opener.open(request, timeout=self.config.timeout)
        except HTTPError as exc:
            raise WpsApiError("object download", status=exc.code) from None
        except URLError as exc:
            raise WpsApiError("object download") from exc

        response_status = getattr(response, "status", 200)
        if not isinstance(response_status, int):
            response_status = 200
        if range_requested and response_status != 206:
            response.close()
            raise WpsApiError("range download was not honored", status=response_status)
        headers = response.headers
        content_type = headers.get("Content-Type") if headers is not None else None
        content_length = None
        if headers is not None:
            try:
                content_length = int(headers.get("Content-Length", ""))
            except (TypeError, ValueError):
                pass
        content_range = headers.get("Content-Range") if headers is not None else None
        if range_requested and not self._range_response_matches(
            content_range,
            offset=offset,
            length=length,
            content_length=content_length,
        ):
            response.close()
            raise WpsApiError("range response metadata was not honored", status=response_status)
        status = payload.get("status") if isinstance(payload.get("status"), str) else None
        return DownloadStream(
            response,
            status,
            content_type,
            content_length,
            http_status=response_status,
            content_range=content_range,
        )

    def download_to(
        self,
        file_id: str,
        destination: BinaryIO,
        *,
        chunk_size: int = 1024 * 1024,
        checksums: Sequence[str] = ("md5", "sha1", "sha224", "sha256", "sha384", "sha512"),
        get_direct_external_download_url: bool | None = None,
        cid: str | None = None,
    ) -> int:
        """Stream a file into an existing binary destination and return bytes written."""

        if chunk_size <= 0:
            raise ValueError("chunk_size must be positive")
        with self.open_download(
            file_id,
            checksums=checksums,
            get_direct_external_download_url=get_direct_external_download_url,
            cid=cid,
        ) as stream:
            total = 0
            while True:
                chunk = stream.read(chunk_size)
                if not chunk:
                    return total
                destination.write(chunk)
                total += len(chunk)
