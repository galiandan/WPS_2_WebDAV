from __future__ import annotations

import unittest
from io import BytesIO
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace

from wps_adapter.client import DownloadStream
from wps_adapter.provider import AlreadyExistsError, InvalidPathError, RemoteEntry
from wps_adapter.storage import MultiSpaceStorage, WpsStorage
from wps_adapter.workspace import WorkspaceState


class FakeClient:
    def __init__(self) -> None:
        self.config = SimpleNamespace(stream_chunk_size=4)
        self.children = {
            "root": (
                RemoteEntry(id="docs", name="docs", kind="folder", parent_id="root", size=0),
                RemoteEntry(id="top", name="top.txt", kind="file", parent_id="root", size=3),
            ),
            "docs": (RemoteEntry(id="readme", name="readme.txt", kind="file", parent_id="docs", size=4),),
        }
        self.upload_calls = []
        self.download_calls = []
        self.folder_calls = []
        self.delete_calls = []
        self.rename_calls = []
        self.move_calls = []

    def iter_entries(self, parent_id: str, **_kwargs):
        return self.children.get(parent_id, ())

    def upload(self, parent_id: str, name: str, source, **kwargs):
        body = source.read()
        self.upload_calls.append((parent_id, name, body, kwargs))
        return RemoteEntry(id="new", name=name, kind="file", parent_id=parent_id, size=len(body))

    def open_download(self, entry_id: str, **kwargs):
        self.download_calls.append((entry_id, kwargs))
        raise AssertionError("download stream should not be opened in this test")

    def create_folder(self, parent_id: str, name: str):
        self.folder_calls.append((parent_id, name))
        return RemoteEntry(id="folder-new", name=name, kind="folder", parent_id=parent_id, size=0)

    def delete(self, entry_id: str):
        self.delete_calls.append(entry_id)

    def rename(self, entry_id: str, name: str):
        self.rename_calls.append((entry_id, name))
        return RemoteEntry(id=entry_id, name=name, kind="file", parent_id="root", size=3)

    def move(self, entry_id: str, source_parent_id: str, destination_parent_id: str):
        self.move_calls.append((entry_id, source_parent_id, destination_parent_id))


class CopyClient(FakeClient):
    def __init__(self) -> None:
        super().__init__()
        self.children["root"] = (
            RemoteEntry(id="source", name="source.txt", kind="file", parent_id="root", size=11),
            RemoteEntry(id="docs", name="docs", kind="folder", parent_id="root", size=0),
        )
        self.source_bytes = b"copy source"

    def open_download(self, entry_id: str, **kwargs):
        self.download_calls.append((entry_id, kwargs))

        class Response:
            headers = {"Content-Length": "11"}

            def __init__(self, body: bytes) -> None:
                self.body = BytesIO(body)

            def read(self, size: int = -1) -> bytes:
                return self.body.read(size)

            def close(self) -> None:
                self.body.close()

        return DownloadStream(Response(self.source_bytes), "finished", "text/plain", 11)


class NativeCopyClient(CopyClient):
    def __init__(self) -> None:
        super().__init__()
        self.copy_calls = []

    def copy(self, file_id: str, target_parent_id: str):
        self.copy_calls.append((file_id, target_parent_id))
        return "native-copy"


class FailingFolderCopyClient(FakeClient):
    def __init__(self) -> None:
        super().__init__()
        self.children["root"] = (
            RemoteEntry(id="source-folder", name="source", kind="folder", parent_id="root", size=0),
        )
        self.children["source-folder"] = (
            RemoteEntry(id="child", name="child.txt", kind="file", parent_id="source-folder", size=4),
        )

    def open_download(self, entry_id: str, **kwargs):
        self.download_calls.append((entry_id, kwargs))
        raise RuntimeError("simulated copy failure")


class StorageTests(unittest.TestCase):
    def test_workspace_without_named_spaces_does_not_create_an_id_named_folder(self) -> None:
        client = FakeClient()
        with TemporaryDirectory() as directory:
            path = Path(directory) / "workspace.json"
            path.write_text('{"group_id":"group-1","root_id":"root"}\n', encoding="utf-8")
            path.chmod(0o600)
            workspace = WorkspaceState.from_file(
                str(path),
                configured_group_id="auto",
                configured_root_id="auto",
            )
            client.config.workspace = workspace
            client.config.group_id = "auto"
            storage = MultiSpaceStorage(client, workspace.spaces, root_name="Drive", root_id="0")

            self.assertEqual(workspace.spaces, ())
            self.assertEqual([item.name for item in storage.list_path("/")], ["docs", "top.txt"])

    def test_auto_workspace_root_is_reloaded_after_login_selection(self) -> None:
        client = FakeClient()
        with TemporaryDirectory() as directory:
            workspace = WorkspaceState.from_file(
                str(Path(directory) / "workspace.json"),
                configured_group_id="auto",
                configured_root_id="auto",
            )
            client.config.workspace = workspace
            storage = WpsStorage(client, root_id="0")

            workspace.update("group", "selected-root")

            self.assertEqual(storage.root.id, "selected-root")

    def test_workspace_group_change_invalidates_metadata_cache(self) -> None:
        client = FakeClient()
        with TemporaryDirectory() as directory:
            workspace = WorkspaceState.from_file(
                str(Path(directory) / "workspace.json"),
                configured_group_id="auto",
                configured_root_id="auto",
            )
            client.config.workspace = workspace
            storage = WpsStorage(client, root_id="root", cache_ttl=60)
            workspace.update("group-1", "root-1")
            storage.list_path("/")
            self.assertTrue(storage._cache)

            workspace.update("group-2", "root-1")
            storage.root

            self.assertFalse(storage._cache)

    def test_resolves_nested_paths_using_parent_ids(self) -> None:
        storage = WpsStorage(FakeClient(), root_id="root", cache_ttl=60)
        entry = storage.metadata("/docs/readme.txt")
        self.assertEqual(entry.id, "readme")
        self.assertEqual([item.name for item in storage.list_path("/")], ["docs", "top.txt"])

    def test_metadata_cache_has_a_folder_count_bound(self) -> None:
        client = FakeClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60, max_cached_folders=1)

        storage.list_path("/")
        storage.list_path("/docs")

        self.assertEqual(len(storage._cache), 1)
        self.assertIn("docs", storage._cache)

    def test_uploads_new_path_and_rejects_collision(self) -> None:
        client = FakeClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)
        result = storage.upload_path("/docs/new.txt", BytesIO(b"hello"), size=5)
        self.assertEqual(result.id, "new")
        self.assertEqual(client.upload_calls[0][0:3], ("docs", "new.txt", b"hello"))
        with self.assertRaises(AlreadyExistsError):
            storage.upload_path("/docs/readme.txt", BytesIO(b"overwrite"), size=9)

    def test_overwrites_existing_file_only_when_enabled(self) -> None:
        client = FakeClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        storage.upload_path("/top.txt", BytesIO(b"new"), size=3, overwrite=True)

        self.assertTrue(client.upload_calls[0][3]["overwrite"])
        with self.assertRaises(AlreadyExistsError):
            storage.upload_path("/docs", BytesIO(b"new"), size=3, overwrite=True)

    def test_renames_path_and_rejects_collision(self) -> None:
        client = FakeClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        result = storage.rename_path("/top.txt", "renamed.txt")

        self.assertEqual(result.name, "renamed.txt")
        self.assertEqual(client.rename_calls, [("top", "renamed.txt")])
        with self.assertRaises(AlreadyExistsError):
            storage.rename_path("/top.txt", "docs")

    def test_renames_by_id(self) -> None:
        client = FakeClient()
        storage = WpsStorage(client, root_id="root")

        result = storage.rename("top", "renamed.txt")

        self.assertEqual(result.name, "renamed.txt")
        self.assertEqual(client.rename_calls, [("top", "renamed.txt")])

    def test_deletes_path_and_rejects_root(self) -> None:
        client = FakeClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        storage.delete_path("/top.txt")

        self.assertEqual(client.delete_calls, ["top"])
        with self.assertRaises(InvalidPathError):
            storage.delete_path("/")

    def test_moves_path_to_destination_parent(self) -> None:
        client = FakeClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        result = storage.move_to_parent_path("/top.txt", "/docs")

        self.assertEqual(result.parent_id, "docs")
        self.assertEqual(client.move_calls, [("top", "root", "docs")])

    def test_download_uses_file_link_id_as_cid(self) -> None:
        client = FakeClient()
        client.children["root"] = (
            RemoteEntry(
                id="top",
                name="top.txt",
                kind="file",
                parent_id="root",
                size=3,
                link_id="file-link-cid",
            ),
        )
        storage = WpsStorage(client, root_id="root", cache_ttl=60)
        with self.assertRaises(AssertionError):
            storage.open_path("/top.txt")
        self.assertEqual(
            client.download_calls,
            [("top", {"offset": 0, "length": None, "cid": "file-link-cid"})],
        )

    def test_creates_folder_path_and_rejects_collision(self) -> None:
        client = FakeClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        result = storage.create_folder_path("/new-folder")

        self.assertEqual(result.kind, "folder")
        self.assertEqual(client.folder_calls, [("root", "new-folder")])
        with self.assertRaises(AlreadyExistsError):
            storage.create_folder_path("/docs")

    def test_copies_file_through_a_stream_without_reading_it_as_a_whole_source(self) -> None:
        client = CopyClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        result = storage.copy_path("/source.txt", "/copied.txt", depth="0")

        self.assertEqual(result.name, "copied.txt")
        self.assertEqual(client.download_calls[0][0], "source")
        self.assertEqual(client.upload_calls[0][0:3], ("root", "copied.txt", b"copy source"))

    def test_native_copy_is_used_only_when_destination_preserves_name(self) -> None:
        client = NativeCopyClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        result = storage.copy_path("/source.txt", "/docs/source.txt", depth="0")

        self.assertEqual(result.id, "native-copy")
        self.assertEqual(client.copy_calls, [("source", "docs")])
        self.assertEqual(client.download_calls, [])
        self.assertEqual(client.upload_calls, [])

    def test_renamed_file_copy_uses_relay_instead_of_native_copy(self) -> None:
        client = NativeCopyClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        result = storage.copy_path("/source.txt", "/renamed.txt", depth="0")

        self.assertEqual(result.name, "renamed.txt")
        self.assertEqual(client.copy_calls, [])
        self.assertEqual(client.upload_calls[0][0:3], ("root", "renamed.txt", b"copy source"))

    def test_recursive_copy_attempts_to_clean_a_new_root_after_failure(self) -> None:
        client = FailingFolderCopyClient()
        storage = WpsStorage(client, root_id="root", cache_ttl=60)

        with self.assertRaisesRegex(RuntimeError, "simulated copy failure"):
            storage.copy_path("/source", "/copied", depth="infinity")

        self.assertIn("folder-new", client.delete_calls)


if __name__ == "__main__":
    unittest.main()
