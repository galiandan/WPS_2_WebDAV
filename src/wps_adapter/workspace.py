"""Persistent WPS workspace selection with safe file handling."""

from __future__ import annotations

import json
import os
import re
import stat
import tempfile
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Mapping


AUTO_VALUE = "auto"
DEFAULT_WORKSPACE_FILE = "/etc/wps-adapter/secrets/wps-workspace.json"
MAX_WORKSPACE_FILE_BYTES = 16 * 1024
_IDENTIFIER_PATTERN = re.compile(r"^[A-Za-z0-9._-]{1,256}$")


class WorkspaceConfigError(ValueError):
    """The workspace state is missing, malformed, or unsafe to use."""


def validate_workspace_identifier(value: object, *, field_name: str) -> str:
    if not isinstance(value, str) or not _IDENTIFIER_PATTERN.fullmatch(value):
        raise WorkspaceConfigError(f"{field_name} is invalid")
    return value


def _validate_parent(path: str) -> None:
    if not path or not os.path.isabs(path) or any(char in path for char in "\x00\r\n"):
        raise WorkspaceConfigError("workspace file path is invalid")
    parent = os.path.dirname(path)
    if os.path.realpath(parent) != os.path.abspath(parent):
        raise WorkspaceConfigError("workspace file path must not use symlinks")
    try:
        metadata = os.stat(parent, follow_symlinks=False)
    except FileNotFoundError:
        # Fresh installs create this directory before the first login. Keep
        # an auto-configured service startable while it is still pending that
        # first login; writes still fail closed until the directory exists.
        return
    except OSError as exc:
        raise WorkspaceConfigError("workspace file directory is unavailable") from exc
    if (
        not stat.S_ISDIR(metadata.st_mode)
        or metadata.st_mode & 0o077
        or metadata.st_uid not in {0, os.getuid()}
    ):
        raise WorkspaceConfigError("workspace file directory must be private")


def _validate_path(path: str) -> None:
    _validate_parent(path)
    try:
        metadata = os.lstat(path)
    except FileNotFoundError:
        return
    except OSError as exc:
        raise WorkspaceConfigError("workspace file is unavailable") from exc
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
        raise WorkspaceConfigError("workspace file must be a regular file")
    if metadata.st_mode & 0o077 or metadata.st_uid not in {0, os.getuid()}:
        raise WorkspaceConfigError("workspace file permissions are too broad")


def _read_file(path: str) -> tuple[dict[str, Any] | None, int | None]:
    _validate_path(path)
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except FileNotFoundError:
        return None, None
    except OSError as exc:
        raise WorkspaceConfigError("read workspace file failed") from exc
    try:
        metadata = os.fstat(descriptor)
        if (
            not stat.S_ISREG(metadata.st_mode)
            or metadata.st_mode & 0o077
            or metadata.st_uid not in {0, os.getuid()}
            or metadata.st_size > MAX_WORKSPACE_FILE_BYTES
        ):
            raise WorkspaceConfigError("workspace file is unsafe")
        with os.fdopen(descriptor, "r", encoding="utf-8") as stream:
            descriptor = -1
            raw = stream.read(MAX_WORKSPACE_FILE_BYTES + 1)
        if len(raw.encode("utf-8")) > MAX_WORKSPACE_FILE_BYTES:
            raise WorkspaceConfigError("workspace file is too large")
        if not raw.strip():
            return None, metadata.st_mtime_ns
        try:
            payload = json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise WorkspaceConfigError("workspace file is not valid JSON") from exc
        if not isinstance(payload, dict):
            raise WorkspaceConfigError("workspace file must contain a JSON object")
        return payload, metadata.st_mtime_ns
    except UnicodeError as exc:
        raise WorkspaceConfigError("workspace file is not valid UTF-8") from exc
    finally:
        if descriptor != -1:
            os.close(descriptor)


@dataclass(slots=True)
class WorkspaceState:
    """Resolve configured or login-selected group/root IDs at request time."""

    file_path: str | None = None
    configured_group_id: str = ""
    configured_root_id: str = "0"
    _group_id: str = field(default="", init=False, repr=False)
    _root_id: str = field(default="0", init=False, repr=False)
    _file_mtime_ns: int | None = field(default=None, init=False, repr=False)
    _lock: threading.RLock = field(default_factory=threading.RLock, init=False, repr=False)

    def __post_init__(self) -> None:
        if self.file_path is not None:
            _validate_path(self.file_path)
        self.configured_group_id = self._validate_configured(
            self.configured_group_id, field_name="WPS_GROUP_ID", allow_empty=True
        )
        self.configured_root_id = self._validate_configured(
            self.configured_root_id or "0", field_name="WPS_ROOT_ID", allow_empty=False
        )
        self._group_id = "" if self.configured_group_id in {"", AUTO_VALUE} else self.configured_group_id
        self._root_id = "0" if self.configured_root_id == AUTO_VALUE else self.configured_root_id
        self._refresh_locked(force=True)

    @staticmethod
    def _validate_configured(value: str, *, field_name: str, allow_empty: bool) -> str:
        if allow_empty and value == "":
            return value
        if value == AUTO_VALUE:
            return value
        return validate_workspace_identifier(value, field_name=field_name)

    @classmethod
    def from_file(
        cls,
        file_path: str | None,
        *,
        configured_group_id: str = "",
        configured_root_id: str = "0",
    ) -> "WorkspaceState":
        return cls(
            file_path=file_path,
            configured_group_id=configured_group_id,
            configured_root_id=configured_root_id,
        )

    def _apply_file_payload_locked(self, payload: Mapping[str, Any] | None) -> None:
        if payload is None:
            file_group = ""
            file_root = "0"
        else:
            raw_group = payload.get("group_id", "")
            raw_root = payload.get("root_id", "0")
            file_group = (
                ""
                if raw_group == ""
                else validate_workspace_identifier(raw_group, field_name="workspace.group_id")
            )
            file_root = validate_workspace_identifier(raw_root, field_name="workspace.root_id")
        if self.configured_group_id in {"", AUTO_VALUE}:
            self._group_id = file_group
        if self.configured_root_id == AUTO_VALUE:
            self._root_id = file_root

    def _refresh_locked(self, *, force: bool = False) -> None:
        if not self.file_path:
            return
        try:
            metadata = os.stat(self.file_path, follow_symlinks=False)
            mtime_ns = metadata.st_mtime_ns
        except FileNotFoundError:
            mtime_ns = None
        except OSError as exc:
            raise WorkspaceConfigError("stat workspace file failed") from exc
        if not force and mtime_ns == self._file_mtime_ns:
            return
        payload, read_mtime_ns = _read_file(self.file_path)
        self._apply_file_payload_locked(payload)
        self._file_mtime_ns = read_mtime_ns

    @property
    def group_id(self) -> str:
        with self._lock:
            self._refresh_locked()
            return self._group_id

    @property
    def root_id(self) -> str:
        with self._lock:
            self._refresh_locked()
            return self._root_id

    @property
    def configured(self) -> bool:
        return bool(self.group_id)

    def update(self, group_id: str, root_id: str = "0") -> None:
        group_id = validate_workspace_identifier(group_id, field_name="workspace.group_id")
        root_id = validate_workspace_identifier(root_id, field_name="workspace.root_id")
        with self._lock:
            if self.file_path:
                self._persist_locked(group_id, root_id)
            if self.configured_group_id in {"", AUTO_VALUE}:
                self._group_id = group_id
            if self.configured_root_id == AUTO_VALUE:
                self._root_id = root_id

    def _persist_locked(self, group_id: str, root_id: str) -> None:
        assert self.file_path is not None
        _validate_parent(self.file_path)
        descriptor, temporary = tempfile.mkstemp(
            prefix=f".{Path(self.file_path).name}.",
            dir=os.path.dirname(self.file_path),
            text=True,
        )
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                descriptor = -1
                json.dump(
                    {"group_id": group_id, "root_id": root_id},
                    stream,
                    ensure_ascii=True,
                    separators=(",", ":"),
                )
                stream.write("\n")
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.file_path)
            metadata = os.stat(self.file_path, follow_symlinks=False)
            self._file_mtime_ns = metadata.st_mtime_ns
        except OSError as exc:
            raise WorkspaceConfigError("write workspace file failed") from exc
        finally:
            if descriptor != -1:
                os.close(descriptor)
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass


__all__ = [
    "AUTO_VALUE",
    "DEFAULT_WORKSPACE_FILE",
    "WorkspaceConfigError",
    "WorkspaceState",
    "validate_workspace_identifier",
]
