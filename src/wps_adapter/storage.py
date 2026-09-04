"""Path-aware storage facade for the confirmed WPS drive operations."""

from __future__ import annotations

import posixpath
import threading
import time
from collections.abc import Iterable
from typing import Any, BinaryIO, Callable
from urllib.parse import unquote
import mimetypes

from .client import DownloadStream, WpsDriveClient
from .provider import (
    AlreadyExistsError,
    AmbiguousPathError,
    EntryNotFoundError,
    InvalidPathError,
    NotFolderError,
    RemoteEntry,
    ServiceBusyError,
    InsufficientStorageError,
    UnsupportedOperationError,
)

MAX_REMOTE_NAME_BYTES = 4096


def split_remote_path(path: str) -> tuple[str, ...]:
    """Decode and validate an absolute URL path into remote name components."""

    if not isinstance(path, str) or not path.startswith("/"):
        raise InvalidPathError("remote paths must start with '/'")
    try:
        decoded = unquote(path, encoding="utf-8", errors="strict")
    except UnicodeDecodeError as exc:
        raise InvalidPathError("remote path is not valid UTF-8") from exc
    if (
        "\x00" in decoded
        or "\\" in decoded
        or any(ord(char) < 0x20 or ord(char) == 0x7F for char in decoded)
    ):
        raise InvalidPathError("remote path contains a forbidden character")
    if decoded == "/":
        return ()

    raw_parts = decoded.split("/")[1:]
    if raw_parts and raw_parts[-1] == "":
        raw_parts.pop()
    if any(part in {"", ".", ".."} for part in raw_parts):
        raise InvalidPathError("remote path contains an empty or traversal component")
    if any(
        "\x00" in part
        or "/" in part
        or len(part.encode("utf-8")) > MAX_REMOTE_NAME_BYTES
        for part in raw_parts
    ):
        raise InvalidPathError("remote path contains a forbidden component")
    return tuple(raw_parts)


def join_remote_path(parts: Iterable[str], *, trailing_slash: bool = False) -> str:
    """Build a canonical URL path from already validated remote names."""

    validated_parts = tuple(parts)
    if any(
        not isinstance(part, str)
        or not part
        or part in {".", ".."}
        or "/" in part
        or "\\" in part
        or len(part.encode("utf-8")) > MAX_REMOTE_NAME_BYTES
        or any(ord(char) < 0x20 or ord(char) == 0x7F for char in part)
        for part in validated_parts
    ):
        raise InvalidPathError("remote path contains an invalid component")
    path = "/" + "/".join(validated_parts)
    if trailing_slash and path != "/":
        path += "/"
    return posixpath.normpath(path) if path != "/" else path


class _ManagedDownloadStream:
    """Release one storage download slot exactly when the stream is closed."""

    def __init__(self, stream: DownloadStream, release: Callable[[], None]) -> None:
        self._stream = stream
        self._release = release
        self._closed = False

    @property
    def content_length(self) -> int | None:
        return self._stream.content_length

    @property
    def content_type(self) -> str | None:
        return self._stream.content_type

    @property
    def http_status(self) -> int:
        return self._stream.http_status

    @property
    def content_range(self) -> str | None:
        return self._stream.content_range

    def read(self, size: int = -1) -> bytes:
        return self._stream.read(size)

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            self._stream.close()
        finally:
            self._release()

    def __enter__(self) -> "_ManagedDownloadStream":
        return self

    def __exit__(self, _exc_type: Any, _exc_value: Any, _traceback: Any) -> None:
        self.close()


class WpsStorage:
    """Resolve human paths against WPS IDs and expose confirmed operations.

    The cache only contains metadata and expires quickly. It is cleared after
    successful mutations. Deletion and move are confirmed asynchronous WPS
    tasks. COPY is implemented as a bounded download/upload relay because no
    WPS server-side copy request has been confirmed.
    """

    def __init__(
        self,
        client: WpsDriveClient,
        *,
        root_id: str = "0",
        root_name: str = "WPS Enterprise Drive",
        list_count: int = 20,
        max_list_entries: int = 10000,
        cache_ttl: float = 2.0,
        max_cached_folders: int = 1024,
        max_uploads: int = 2,
        max_downloads: int = 4,
        transfer_wait_timeout: float = 30.0,
        max_copy_entries: int = 10000,
        max_copy_depth: int = 64,
    ) -> None:
        if not root_id:
            raise ValueError("root_id is required")
        if list_count <= 0:
            raise ValueError("list_count must be positive")
        if max_list_entries <= 0:
            raise ValueError("max_list_entries must be positive")
        if list_count > max_list_entries:
            raise ValueError("list_count must not exceed max_list_entries")
        if cache_ttl < 0:
            raise ValueError("cache_ttl must not be negative")
        if max_cached_folders <= 0:
            raise ValueError("max_cached_folders must be positive")
        if max_uploads <= 0:
            raise ValueError("max_uploads must be positive")
        if max_downloads <= 0:
            raise ValueError("max_downloads must be positive")
        if transfer_wait_timeout <= 0:
            raise ValueError("transfer_wait_timeout must be positive")
        if max_copy_entries <= 0:
            raise ValueError("max_copy_entries must be positive")
        if max_copy_depth <= 0:
            raise ValueError("max_copy_depth must be positive")
        self.client = client
        self.root_id = str(root_id)
        self.root_name = root_name or "WPS Enterprise Drive"
        self.list_count = list_count
        self.max_list_entries = max_list_entries
        self.cache_ttl = cache_ttl
        self.max_cached_folders = max_cached_folders
        self.max_copy_entries = max_copy_entries
        self.max_copy_depth = max_copy_depth
        self._cache: dict[str, tuple[float, tuple[RemoteEntry, ...]]] = {}
        self._cache_group_id = ""
        self._lock = threading.RLock()
        self._upload_slots = threading.BoundedSemaphore(max_uploads)
        self._download_slots = threading.BoundedSemaphore(max_downloads)
        self._transfer_wait_timeout = transfer_wait_timeout

    def _sync_workspace_root(self) -> None:
        workspace = getattr(self.client.config, "workspace", None)
        if workspace is None or getattr(workspace, "configured_root_id", "") != "auto":
            return
        selected_root = workspace.root_id
        selected_group = workspace.group_id
        with self._lock:
            if selected_group != self._cache_group_id:
                self._cache.clear()
                self._cache_group_id = selected_group
            if selected_root != self.root_id:
                self.root_id = str(selected_root)
                self._cache.clear()

    @property
    def root(self) -> RemoteEntry:
        self._sync_workspace_root()
        return RemoteEntry(
            id=self.root_id,
            name=self.root_name,
            kind="folder",
            parent_id=None,
            size=0,
        )

    def invalidate(self) -> None:
        with self._lock:
            self._cache.clear()

    def set_root_id(self, root_id: str) -> None:
        """Switch the mapped WPS root after a login-selected workspace update."""

        if not root_id:
            raise ValueError("root_id is required")
        with self._lock:
            self.root_id = str(root_id)
            self._cache.clear()
            self._cache_group_id = ""

    def set_root_name(self, root_name: str) -> None:
        """Update the adapter-side display name for the virtual root."""

        if not isinstance(root_name, str) or not root_name:
            raise ValueError("root_name is required")
        with self._lock:
            self.root_name = root_name

    def _children(self, parent_id: str) -> tuple[RemoteEntry, ...]:
        self._sync_workspace_root()
        now = time.monotonic()
        with self._lock:
            cached = self._cache.get(parent_id)
            if cached is not None and cached[0] >= now:
                return cached[1]

        entries = tuple(
            self.client.iter_entries(
                parent_id,
                count=self.list_count,
                max_entries=self.max_list_entries,
                linkgroup=True,
                include="acl,pic_thumbnail",
                with_link=True,
                review_pic_thumbnail=True,
                with_sharefolder_type=True,
            )
        )
        with self._lock:
            if parent_id not in self._cache and len(self._cache) >= self.max_cached_folders:
                oldest_parent = min(self._cache, key=lambda key: self._cache[key][0])
                del self._cache[oldest_parent]
            self._cache[parent_id] = (time.monotonic() + self.cache_ttl, entries)
        return entries

    @staticmethod
    def _child(parent_id: str, name: str, entries: Iterable[RemoteEntry]) -> RemoteEntry:
        matches = [entry for entry in entries if entry.name == name and entry.kind != "unknown"]
        if not matches:
            raise EntryNotFoundError(f"entry not found: {name}")
        if len(matches) > 1:
            raise AmbiguousPathError(f"multiple entries have the name: {name}")
        entry = matches[0]
        if entry.parent_id is not None and entry.parent_id != parent_id:
            raise EntryNotFoundError(f"entry not found: {name}")
        return entry

    def _resolve_parts(self, parts: tuple[str, ...]) -> RemoteEntry:
        current = self.root
        for name in parts:
            if current.kind != "folder":
                raise NotFolderError(f"not a folder: {current.name}")
            current = self._child(current.id, name, self._children(current.id))
        return current

    def resolve(self, path: str) -> RemoteEntry:
        return self._resolve_parts(split_remote_path(path))

    def list_path(self, path: str) -> tuple[RemoteEntry, ...]:
        entry = self.resolve(path)
        if entry.kind != "folder":
            raise NotFolderError(f"not a folder: {path}")
        return self._children(entry.id)

    def list(self, parent_id: str | None = None) -> Iterable[RemoteEntry]:
        self._sync_workspace_root()
        return self._children(str(parent_id) if parent_id is not None else self.root_id)

    def _parent_and_name(self, path: str) -> tuple[RemoteEntry, str, tuple[str, ...]]:
        parts = split_remote_path(path)
        if not parts:
            raise InvalidPathError("the root cannot be used as a file name")
        parent_parts = parts[:-1]
        parent = self._resolve_parts(parent_parts)
        if parent.kind != "folder":
            raise NotFolderError(f"not a folder: {join_remote_path(parent_parts)}")
        return parent, parts[-1], parts

    @staticmethod
    def _validate_entry_name(name: str) -> str:
        if (
            not isinstance(name, str)
            or not name
            or name in {".", ".."}
            or "/" in name
            or "\\" in name
            or "\x00" in name
            or len(name.encode("utf-8")) > MAX_REMOTE_NAME_BYTES
            or any(ord(char) < 0x20 or ord(char) == 0x7F for char in name)
        ):
            raise InvalidPathError("name must be one remote path component")
        return name

    def _upload_stream(
        self,
        parent_id: str,
        name: str,
        source: BinaryIO,
        *,
        size: int | None,
        content_type: str | None,
        csrf_token: str | None = None,
        overwrite: bool,
    ) -> RemoteEntry:
        if not self._upload_slots.acquire(timeout=self._transfer_wait_timeout):
            raise ServiceBusyError("too many uploads are active")
        try:
            return self.client.upload(
                parent_id,
                name,
                source,
                size=size,
                content_type=content_type,
                csrf_token=csrf_token,
                overwrite=overwrite,
            )
        finally:
            self._upload_slots.release()

    def upload_path(
        self,
        path: str,
        source: BinaryIO,
        *,
        size: int | None = None,
        content_type: str | None = None,
        csrf_token: str | None = None,
        overwrite: bool = False,
    ) -> RemoteEntry:
        parent, name, _parts = self._parent_and_name(path)
        existing = [entry for entry in self._children(parent.id) if entry.name == name]
        if existing:
            if not overwrite or len(existing) > 1 or existing[0].kind != "file":
                raise AlreadyExistsError(f"overwrite is not enabled for: {path}")
        result = self._upload_stream(
            parent.id,
            name,
            source,
            size=size,
            content_type=content_type,
            csrf_token=csrf_token,
            overwrite=overwrite,
        )
        self.invalidate()
        return result

    def open_path(
        self,
        path: str,
        *,
        offset: int = 0,
        length: int | None = None,
    ) -> DownloadStream:
        entry = self.resolve(path)
        if entry.kind != "file":
            raise NotFolderError(f"not a downloadable file: {path}")
        if not self._download_slots.acquire(timeout=self._transfer_wait_timeout):
            raise ServiceBusyError("too many downloads are active")
        try:
            stream = self.client.open_download(
                entry.id,
                offset=offset,
                length=length,
                cid=entry.link_id,
            )
        except Exception:
            self._download_slots.release()
            raise
        return _ManagedDownloadStream(stream, self._download_slots.release)

    def metadata(self, path: str) -> RemoteEntry:
        return self.resolve(path)

    def create_folder(self, parent_id: str | None, name: str) -> RemoteEntry:
        self._sync_workspace_root()
        parent = self.root_id if parent_id is None else str(parent_id)
        if not name or "/" in name or "\\" in name:
            raise InvalidPathError("folder name must be one remote path component")
        existing = [entry for entry in self._children(parent) if entry.name == name]
        if existing:
            raise AlreadyExistsError(f"entry already exists: {name}")
        result = self.client.create_folder(parent, name)
        self.invalidate()
        return result

    def create_folder_path(self, path: str) -> RemoteEntry:
        parent, name, _parts = self._parent_and_name(path)
        existing = [entry for entry in self._children(parent.id) if entry.name == name]
        if existing:
            raise AlreadyExistsError(f"entry already exists: {path}")
        result = self.client.create_folder(parent.id, name)
        self.invalidate()
        return result

    def open_download(self, entry_id: str, *, offset: int = 0) -> DownloadStream:
        return self.client.open_download(entry_id, offset=offset)

    def delete(self, entry_id: str) -> None:
        self.client.delete(entry_id)
        self.invalidate()

    def delete_path(self, path: str) -> None:
        parts = split_remote_path(path)
        if not parts:
            raise InvalidPathError("the root cannot be deleted")
        entry = self.resolve(path)
        self.client.delete(entry.id)
        self.invalidate()

    def rename(self, entry_id: str, name: str) -> RemoteEntry:
        name = self._validate_entry_name(name)
        result = self.client.rename(entry_id, name)
        self.invalidate()
        return result

    def rename_path(self, path: str, name: str) -> RemoteEntry:
        parts = split_remote_path(path)
        if not parts:
            raise InvalidPathError("the root cannot be renamed")
        name = self._validate_entry_name(name)
        entry = self.resolve(path)
        parent = self._resolve_parts(parts[:-1])
        if name == entry.name:
            return entry
        existing = [child for child in self._children(parent.id) if child.name == name and child.id != entry.id]
        if existing:
            raise AlreadyExistsError(f"entry already exists: {name}")
        result = self.client.rename(entry.id, name)
        self.invalidate()
        return result

    def move_to_parent_path(self, path: str, parent_path: str) -> RemoteEntry:
        source_parts = split_remote_path(path)
        if not source_parts:
            raise InvalidPathError("the root cannot be moved")
        destination_parent_parts = split_remote_path(parent_path)
        if destination_parent_parts[: len(source_parts)] == source_parts:
            raise InvalidPathError("an entry cannot be moved into itself")

        entry = self.resolve(path)
        source_parent = self._resolve_parts(source_parts[:-1])
        destination_parent = self.resolve(parent_path)
        if destination_parent.kind != "folder":
            raise NotFolderError(f"not a destination folder: {parent_path}")
        if source_parent.id == destination_parent.id:
            return entry
        existing = [
            child
            for child in self._children(destination_parent.id)
            if child.name == entry.name
        ]
        if existing:
            raise AlreadyExistsError(f"entry already exists: {entry.name}")
        self.client.move(entry.id, source_parent.id, destination_parent.id)
        self.invalidate()
        return RemoteEntry(
            id=entry.id,
            name=entry.name,
            kind=entry.kind,
            parent_id=destination_parent.id,
            size=entry.size,
            modified_at=entry.modified_at,
            etag=entry.etag,
            link_id=entry.link_id,
        )

    def move_path(self, path: str, destination_path: str) -> RemoteEntry:
        """Move to a full destination path, preserving the entry name."""

        source_parts = split_remote_path(path)
        destination_parts = split_remote_path(destination_path)
        if not source_parts or not destination_parts:
            raise InvalidPathError("the root cannot be moved")
        entry = self.resolve(path)
        if destination_parts[-1] != entry.name:
            if source_parts[:-1] == destination_parts[:-1]:
                return self.rename_path(path, destination_parts[-1])
            raise UnsupportedOperationError(
                "cross-folder move with rename is not supported"
            )
        destination_parent_path = join_remote_path(destination_parts[:-1])
        return self.move_to_parent_path(path, destination_parent_path)

    def copy_path(
        self,
        source_path: str,
        destination_path: str,
        *,
        depth: str = "infinity",
        overwrite: bool = True,
    ) -> RemoteEntry:
        """Copy through a bounded streaming download/upload relay.

        WPS has no confirmed server-side COPY request. A folder copy creates
        the destination tree one object at a time; file bytes are never kept
        in a Python bytes object and the upload client removes its temporary
        spool after each file.
        """

        depth = depth.strip().lower()
        if depth not in {"0", "1", "infinity"}:
            raise InvalidPathError("COPY Depth must be 0, 1 or infinity")
        source_parts = split_remote_path(source_path)
        destination_parts = split_remote_path(destination_path)
        if not source_parts or not destination_parts:
            raise InvalidPathError("the root cannot be copied")
        if source_parts == destination_parts:
            raise InvalidPathError("an entry cannot be copied onto itself")

        source = self.resolve(source_path)
        if source.kind == "folder" and destination_parts[: len(source_parts)] == source_parts:
            raise InvalidPathError("a folder cannot be copied into itself")
        destination_parent_parts = destination_parts[:-1]
        destination_parent = self._resolve_parts(destination_parent_parts)
        if destination_parent.kind != "folder":
            raise NotFolderError("the COPY destination parent is not a folder")
        destination_name = self._validate_entry_name(destination_parts[-1])

        try:
            existing = self.resolve(destination_path)
        except EntryNotFoundError:
            existing = None
        if existing is not None:
            if not overwrite:
                raise AlreadyExistsError(f"entry already exists: {destination_path}")
            raise UnsupportedOperationError(
                "COPY overwrite is disabled because the relay is not atomic"
            )

        native_copy = getattr(self.client, "copy", None)
        # The captured WPS COPY API accepts only a destination parent.  It
        # always preserves the source name, so using it for a destination
        # with a different basename would report a path that does not exist.
        if (
            source.kind == "file"
            and destination_name == source.name
            and callable(native_copy)
        ):
            copied_id = native_copy(source.id, destination_parent.id)
            self.invalidate()
            return RemoteEntry(
                id=copied_id,
                name=destination_name,
                kind="file",
                parent_id=destination_parent.id,
                size=source.size,
                modified_at=source.modified_at,
                etag=source.etag,
                link_id=source.link_id,
            )

        copied = 0

        def copy_entry(
            source_entry: RemoteEntry,
            source_item_path: str,
            destination_parent_entry: RemoteEntry,
            destination_item_name: str,
            level: int,
            existing_item: RemoteEntry | None = None,
        ) -> RemoteEntry:
            nonlocal copied
            copied += 1
            if copied > self.max_copy_entries:
                raise InsufficientStorageError("COPY exceeds the configured entry limit")
            if level > self.max_copy_depth:
                raise InsufficientStorageError("COPY exceeds the configured depth limit")

            if source_entry.kind == "file":
                target_exists = existing_item is not None
                if target_exists and existing_item.kind != "file":
                    self.client.delete(existing_item.id)
                    self.invalidate()
                    target_exists = False
                with self.open_path(source_item_path) as stream:
                    result = self._upload_stream(
                        destination_parent_entry.id,
                        destination_item_name,
                        stream,
                        size=source_entry.size,
                        content_type=mimetypes.guess_type(source_entry.name)[0]
                        or "application/octet-stream",
                        overwrite=target_exists,
                    )
                self.invalidate()
                return result

            if existing_item is not None:
                self.client.delete(existing_item.id)
                self.invalidate()
            result = self.client.create_folder(destination_parent_entry.id, destination_item_name)
            self.invalidate()
            try:
                if depth == "0" or (depth == "1" and level >= 1):
                    return result
                if depth == "1":
                    for child in self.list_path(source_item_path):
                        child_path = join_remote_path(
                            split_remote_path(source_item_path) + (child.name,),
                            trailing_slash=child.kind == "folder",
                        )
                        copy_entry(
                            child,
                            child_path,
                            result,
                            child.name,
                            level + 1,
                        )
                    return result

                for child in self.list_path(source_item_path):
                    child_path = join_remote_path(
                        split_remote_path(source_item_path) + (child.name,),
                        trailing_slash=child.kind == "folder",
                    )
                    copy_entry(child, child_path, result, child.name, level + 1)
                return result
            except Exception:
                # The destination did not exist before this request. Remove the
                # newly-created root when a recursive relay fails part-way through.
                try:
                    self.client.delete(result.id)
                except Exception:
                    pass
                self.invalidate()
                raise

        return copy_entry(
            source,
            join_remote_path(source_parts, trailing_slash=source.kind == "folder"),
            destination_parent,
            destination_name,
            0,
            existing,
        )

    def move(self, entry_id: str, parent_id: str | None) -> RemoteEntry:
        raise UnsupportedOperationError(
            "move by ID is unavailable without the source parent path"
        )
