"""Persistent, adapter-local settings for the browser file manager."""

from __future__ import annotations

import json
import os
import stat
import tempfile
import threading
from dataclasses import dataclass, field
from typing import Any


DEFAULT_WEB_SETTINGS_FILE = "/etc/wps-adapter/secrets/web-settings.json"
DEFAULT_ROOT_NAME = "WPS Enterprise Drive"
MAX_WEB_SETTINGS_FILE_BYTES = 16 * 1024
MAX_ROOT_NAME_CHARS = 256
MAX_ROOT_NAME_BYTES = 1024


class WebSettingsError(ValueError):
    """The local web settings are missing, malformed, or invalid."""


class WebSettingsFileError(OSError):
    """The local web settings file cannot be safely read or written."""


def validate_root_name(value: object) -> str:
    """Validate and normalize a user-visible adapter root name."""

    if not isinstance(value, str):
        raise WebSettingsError("root name must be a string")
    name = value.strip()
    if not name:
        raise WebSettingsError("root name must not be empty")
    if len(name) > MAX_ROOT_NAME_CHARS or len(name.encode("utf-8")) > MAX_ROOT_NAME_BYTES:
        raise WebSettingsError("root name is too long")
    if any(ord(char) < 0x20 or ord(char) == 0x7F for char in name):
        raise WebSettingsError("root name contains a control character")
    return name


def _validate_parent(path: str) -> None:
    if not path or not os.path.isabs(path) or any(char in path for char in "\x00\r\n"):
        raise WebSettingsError("web settings file path is invalid")
    parent = os.path.dirname(path)
    if os.path.realpath(parent) != os.path.abspath(parent):
        raise WebSettingsError("web settings file path must not use symlinks")
    try:
        metadata = os.stat(parent, follow_symlinks=False)
    except FileNotFoundError:
        return
    except OSError as exc:
        raise WebSettingsFileError("stat web settings directory failed") from exc
    if (
        not stat.S_ISDIR(metadata.st_mode)
        or metadata.st_mode & 0o077
        or metadata.st_uid not in {0, os.getuid()}
    ):
        raise WebSettingsError("web settings directory must be private")


def _validate_path(path: str) -> None:
    _validate_parent(path)
    try:
        metadata = os.lstat(path)
    except FileNotFoundError:
        return
    except OSError as exc:
        raise WebSettingsFileError("stat web settings file failed") from exc
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
        raise WebSettingsError("web settings file must be a regular file")
    if metadata.st_mode & 0o077 or metadata.st_uid not in {0, os.getuid()}:
        raise WebSettingsError("web settings file permissions are too broad")


def _read_file(path: str) -> tuple[dict[str, Any] | None, int | None]:
    _validate_path(path)
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except FileNotFoundError:
        return None, None
    except OSError as exc:
        raise WebSettingsFileError("read web settings file failed") from exc
    try:
        metadata = os.fstat(descriptor)
        if (
            not stat.S_ISREG(metadata.st_mode)
            or metadata.st_mode & 0o077
            or metadata.st_uid not in {0, os.getuid()}
            or metadata.st_size > MAX_WEB_SETTINGS_FILE_BYTES
        ):
            raise WebSettingsError("web settings file is unsafe")
        with os.fdopen(descriptor, "r", encoding="utf-8") as stream:
            descriptor = -1
            raw = stream.read(MAX_WEB_SETTINGS_FILE_BYTES + 1)
        if len(raw.encode("utf-8")) > MAX_WEB_SETTINGS_FILE_BYTES:
            raise WebSettingsError("web settings file is too large")
        if not raw.strip():
            return None, metadata.st_mtime_ns
        try:
            payload = json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise WebSettingsError("web settings file is not valid JSON") from exc
        if not isinstance(payload, dict):
            raise WebSettingsError("web settings file must contain a JSON object")
        return payload, metadata.st_mtime_ns
    except UnicodeError as exc:
        raise WebSettingsError("web settings file is not valid UTF-8") from exc
    finally:
        if descriptor != -1:
            os.close(descriptor)


@dataclass(slots=True)
class WebSettings:
    """Keep the display name in a small private file beside adapter secrets."""

    file_path: str | None = DEFAULT_WEB_SETTINGS_FILE
    fallback_name: str = DEFAULT_ROOT_NAME
    _name: str = field(default=DEFAULT_ROOT_NAME, init=False, repr=False)
    _file_mtime_ns: int | None = field(default=None, init=False, repr=False)
    _lock: threading.RLock = field(default_factory=threading.RLock, init=False, repr=False)

    def __post_init__(self) -> None:
        self.fallback_name = validate_root_name(self.fallback_name)
        if self.file_path is not None:
            self.file_path = os.fspath(self.file_path)
            _validate_path(self.file_path)
        self._name = self.fallback_name
        self._refresh_locked(force=True)

    @property
    def name(self) -> str:
        with self._lock:
            self._refresh_locked()
            return self._name

    def set_name(self, value: object) -> str:
        name = validate_root_name(value)
        with self._lock:
            if self.file_path is not None:
                self._persist_locked(name)
            self._name = name
            return name

    def _refresh_locked(self, *, force: bool = False) -> None:
        if not self.file_path:
            return
        try:
            metadata = os.stat(self.file_path, follow_symlinks=False)
            mtime_ns = metadata.st_mtime_ns
        except FileNotFoundError:
            mtime_ns = None
        except OSError as exc:
            raise WebSettingsFileError("stat web settings file failed") from exc
        if not force and mtime_ns == self._file_mtime_ns:
            return
        payload, read_mtime_ns = _read_file(self.file_path)
        if payload is None:
            name = self.fallback_name
        else:
            name = validate_root_name(payload.get("name"))
        self._name = name
        self._file_mtime_ns = read_mtime_ns

    def _persist_locked(self, name: str) -> None:
        assert self.file_path is not None
        _validate_parent(self.file_path)
        descriptor, temporary = tempfile.mkstemp(
            prefix=f".{os.path.basename(self.file_path)}.",
            dir=os.path.dirname(self.file_path),
            text=True,
        )
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                descriptor = -1
                json.dump({"name": name}, stream, ensure_ascii=True, separators=(",", ":"))
                stream.write("\n")
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.file_path)
            metadata = os.stat(self.file_path, follow_symlinks=False)
            self._file_mtime_ns = metadata.st_mtime_ns
        except OSError as exc:
            raise WebSettingsFileError("write web settings file failed") from exc
        finally:
            if descriptor != -1:
                os.close(descriptor)
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass


__all__ = [
    "DEFAULT_ROOT_NAME",
    "DEFAULT_WEB_SETTINGS_FILE",
    "MAX_ROOT_NAME_BYTES",
    "MAX_ROOT_NAME_CHARS",
    "MAX_WEB_SETTINGS_FILE_BYTES",
    "WebSettings",
    "WebSettingsError",
    "WebSettingsFileError",
    "validate_root_name",
]
