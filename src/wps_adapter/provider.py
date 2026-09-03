"""Remote-storage contracts and errors shared by protocol adapters."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, BinaryIO, Iterable, Literal, Mapping, Protocol


EntryKind = Literal["file", "folder", "unknown"]


class StorageError(RuntimeError):
    """Base class for errors that can be translated by an HTTP adapter."""


class InvalidPathError(StorageError):
    """A client supplied a path that is not safe or not absolute."""


class EntryNotFoundError(StorageError):
    """The requested remote entry does not exist."""


class NotFolderError(StorageError):
    """A folder operation was requested for a regular file."""


class AlreadyExistsError(StorageError):
    """A create operation would collide with an existing entry."""


class InsufficientStorageError(StorageError):
    """The adapter cannot safely buffer or expand an operation."""


class ServiceBusyError(StorageError):
    """The adapter has reached its configured transfer concurrency limit."""


class AmbiguousPathError(StorageError):
    """The remote provider returned duplicate names in one folder."""


class UnsupportedOperationError(StorageError):
    """The provider operation has not been confirmed by a safe capture."""


@dataclass(frozen=True, slots=True)
class RemoteEntry:
    """Normalized metadata shared by the WPS and protocol layers."""

    id: str
    name: str
    kind: EntryKind
    parent_id: str | None = None
    size: int | None = None
    modified_at: str | None = None
    etag: str | None = None
    link_id: str | None = None
    raw: Mapping[str, Any] = field(default_factory=dict)


class RemoteStorage(Protocol):
    """Small contract consumed by the WebDAV and REST layers."""

    def list(self, parent_id: str | None = None) -> Iterable[RemoteEntry]:
        """List entries directly below a remote folder."""

    def create_folder(self, parent_id: str | None, name: str) -> RemoteEntry:
        """Create a folder below the selected parent."""

    def upload(
        self,
        parent_id: str | None,
        name: str,
        source: BinaryIO,
        *,
        size: int | None = None,
        content_type: str | None = None,
        overwrite: bool = False,
    ) -> RemoteEntry:
        """Upload from a file-like object without requiring a local full copy."""

    def open_download(self, entry_id: str, *, offset: int = 0) -> BinaryIO:
        """Open a remote download stream, optionally starting at an offset."""

    def delete(self, entry_id: str) -> None:
        """Delete an entry after the caller has checked its scope."""

    def rename(self, entry_id: str, name: str) -> RemoteEntry:
        """Rename one remote entry."""

    def move(self, entry_id: str, parent_id: str | None) -> RemoteEntry:
        """Move one remote entry below another remote folder."""

    def copy(
        self,
        source_path: str,
        destination_path: str,
        *,
        depth: str = "infinity",
        overwrite: bool = True,
    ) -> RemoteEntry:
        """Copy a remote entry to a path, optionally including descendants."""
