from __future__ import annotations

import base64
import json
import os
import subprocess
import sys
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest.mock import patch

from wps_adapter.client import WpsCredentials
from wps_adapter.__main__ import _apply_adapter_port, _prompt_login_target
from wps_adapter.login_command import _select_workspace, _select_workspaces
from wps_adapter.login import (
    ChromeLoginSession,
    LoginError,
    _REMOTE_WRITE_SCRIPT,
    _WebSocket,
    WpsWorkspaceSelection,
    WpsWorkspaceCandidate,
    credentials_from_cookies,
    login_and_sync,
    push_credentials_over_ssh,
    push_credentials_over_https,
    verify_workspace_access,
    wait_for_login_credentials,
    wait_for_login_snapshot,
    write_local_credentials,
    write_local_workspace,
    workspace_root_from_page_url,
    workspace_from_page_url,
)


PROJECT_ROOT = Path(__file__).parents[1]


class LoginHelperTests(unittest.TestCase):
    def test_workspace_access_verification_uses_observed_list_request(self) -> None:
        class Response:
            def read(self, _size: int = -1) -> bytes:
                return b'{"files":[],"result":"ok"}'

            def close(self) -> None:
                pass

        class Opener:
            def __init__(self) -> None:
                self.request = None

            def open(self, request, timeout: float) -> Response:
                self.request = request
                self.timeout = timeout
                return Response()

        opener = Opener()
        verify_workspace_access(
            WpsCredentials(cookie="sid=secret", csrf_token="csrf"),
            WpsWorkspaceSelection("tenant", "group-1", "0"),
            base_url="https://365.kdocs.cn/space/",
            opener=opener,
        )
        self.assertIsNotNone(opener.request)
        self.assertIn("/3rd/drive/api/v5/groups/group-1/files", opener.request.full_url)
        self.assertIn("parentid=0", opener.request.full_url)
        self.assertEqual(opener.request.get_header("Cookie"), "sid=secret")

    def test_workspace_access_verification_reports_permission_failure(self) -> None:
        from urllib.error import HTTPError
        from io import BytesIO

        class Opener:
            def open(self, request, timeout: float):
                raise HTTPError(request.full_url, 403, "forbidden", {}, BytesIO())

        with self.assertRaisesRegex(LoginError, "无权访问所选 WPS 工作区"):
            verify_workspace_access(
                WpsCredentials(cookie="sid=secret", csrf_token="csrf"),
                WpsWorkspaceSelection("tenant", "group-1", "0"),
                opener=Opener(),
            )

    def test_workspace_access_verification_rejects_malformed_response(self) -> None:
        class Response:
            def read(self, _size: int = -1) -> bytes:
                return b'{"result":"ok"}'

            def close(self) -> None:
                pass

        class Opener:
            def open(self, request, timeout: float) -> Response:
                return Response()

        with self.assertRaisesRegex(LoginError, "返回格式异常"):
            verify_workspace_access(
                WpsCredentials(cookie="sid=secret", csrf_token="csrf"),
                WpsWorkspaceSelection("tenant", "group-1", "0"),
                opener=Opener(),
            )

    def test_workspace_discovery_parses_names_and_uses_candidate_endpoint(self) -> None:
        class Response:
            def read(self, _size: int = -1) -> bytes:
                return b'{"groups":[{"id":123,"name":"\\u5b66\\u6821\\u4e91\\u76d8"},{"group_id":"456","group_name":"\\u4e2a\\u4eba\\u56e2\\u961f"}]}'

            def close(self) -> None:
                pass

        class Opener:
            def open(self, request, timeout: float) -> Response:
                self.request = request
                return Response()

        opener = Opener()
        from wps_adapter.login import discover_workspaces

        candidates = discover_workspaces(
            WpsCredentials(cookie="sid=secret", csrf_token="csrf"),
            tenant_id="tenant",
            opener=opener,
        )
        self.assertEqual(
            candidates,
            (
                WpsWorkspaceCandidate("tenant", "123", "学校云盘"),
                WpsWorkspaceCandidate("tenant", "456", "个人团队"),
            ),
        )
        self.assertIn("/3rd/plus/groups/v1/companies/tenant/users/self/groups/private", opener.request.full_url)
        self.assertEqual(opener.request.get_header("Cookie"), "sid=secret")

    def test_workspace_selection_uses_human_readable_name(self) -> None:
        candidates = (
            WpsWorkspaceCandidate("tenant", "123", "学校云盘"),
            WpsWorkspaceCandidate("tenant", "456", "个人团队"),
        )
        with patch("builtins.input", return_value="2"):
            selected = _select_workspace(candidates)
        self.assertEqual(selected, candidates[1])

    def test_single_workspace_is_selected_without_an_extra_prompt(self) -> None:
        candidate = WpsWorkspaceCandidate("tenant", "123", "学校云盘")
        with patch("builtins.input") as prompt:
            selected = _select_workspace((candidate,))
        self.assertEqual(selected, candidate)
        prompt.assert_not_called()

    def test_workspace_selection_supports_multiple_and_all(self) -> None:
        candidates = (
            WpsWorkspaceCandidate("tenant", "123", "学校云盘"),
            WpsWorkspaceCandidate("tenant", "456", "个人团队"),
            WpsWorkspaceCandidate("tenant", "789", "自动备份"),
        )
        with patch("builtins.input", return_value="1,3"):
            self.assertEqual(_select_workspaces(candidates), (candidates[0], candidates[2]))
        with patch("builtins.input", return_value="all"):
            self.assertEqual(_select_workspaces(candidates), candidates)

    def test_workspace_selection_happens_after_browser_closes(self) -> None:
        class Session:
            closed = False

            def __enter__(self):
                return self

            def __exit__(self, _type, _value, _traceback):
                self.closed = True

            def current_url(self):
                return "https://365.kdocs.cn/space/tenant/group/"

        session = Session()
        candidates = (
            WpsWorkspaceCandidate("tenant", "123", "学校云盘"),
            WpsWorkspaceCandidate("tenant", "456", "自动备份"),
        )

        def choose(items):
            self.assertTrue(session.closed)
            return items[1]

        with TemporaryDirectory() as directory, patch(
            "wps_adapter.login.ChromeLoginSession", return_value=session
        ), patch(
            "wps_adapter.login.wait_for_login_credentials",
            return_value=(
                WpsCredentials(cookie="sid=secret", csrf_token="csrf"),
                ("csrf", "rtk"),
                [{"name": "cid", "value": "tenant"}],
            ),
        ), patch("wps_adapter.login.discover_workspaces", return_value=candidates), patch(
            "wps_adapter.login.verify_workspace_access"
        ):
            login_and_sync(
                output_dir=directory,
                workspace_selector=choose,
            )

    def test_workspace_discovery_rejects_invalid_items_without_exposing_secrets(self) -> None:
        class Response:
            def read(self, _size: int = -1) -> bytes:
                return b'{"groups":[{"id":"bad/id","name":"bad"},{"id":"ok","name":"good"}]}'

            def close(self) -> None:
                pass

        class Opener:
            def open(self, request, timeout: float) -> Response:
                return Response()

        from wps_adapter.login import discover_workspaces

        candidates = discover_workspaces(
            WpsCredentials(cookie="sid=secret", csrf_token="csrf"),
            tenant_id="tenant",
            opener=Opener(),
        )
        self.assertEqual(candidates, (WpsWorkspaceCandidate("tenant", "ok", "good"),))

    def test_standalone_helper_runs_without_the_project_checkout(self) -> None:
        environment = os.environ.copy()
        environment.pop("PYTHONPATH", None)
        with TemporaryDirectory() as directory:
            completed = subprocess.run(
                [sys.executable, str(PROJECT_ROOT / "wps_login.py"), "--help"],
                cwd=directory,
                env=environment,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                check=False,
            )

        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("--adapter-url", completed.stdout)
        self.assertIn("--allow-http", completed.stdout)
        self.assertIn("--workspace-url", completed.stdout)

    def test_cookie_snapshot_keeps_wps_refresh_cookie_and_filters_other_domains(self) -> None:
        credentials, names = credentials_from_cookies(
            [
                {"name": "rtk", "value": "refresh-ticket", "domain": ".kdocs.cn", "path": "/passport/secure"},
                {"name": "csrf", "value": "csrf-value", "domain": "365.kdocs.cn", "path": "/"},
                {"name": "kso_sid", "value": "drive-session", "domain": "365.kdocs.cn", "path": "/"},
                {"name": "same", "value": "parent", "domain": ".kdocs.cn", "path": "/"},
                {"name": "same", "value": "drive", "domain": "365.kdocs.cn", "path": "/"},
                {"name": "other", "value": "do-not-copy", "domain": ".example.com", "path": "/"},
                {"name": "subdomain", "value": "do-not-copy", "domain": "login.kdocs.cn", "path": "/"},
            ]
        )

        self.assertEqual(names, ("csrf", "kso_sid", "rtk", "same"))
        self.assertIn("rtk=refresh-ticket", credentials.cookie)
        self.assertIn("csrf=csrf-value", credentials.cookie)
        self.assertIn("same=drive", credentials.cookie)
        self.assertNotIn("do-not-copy", credentials.cookie)
        self.assertEqual(credentials.csrf_token, "csrf-value")

    def test_missing_refresh_cookie_is_rejected(self) -> None:
        with self.assertRaisesRegex(LoginError, "没有 rtk"):
            credentials_from_cookies(
                [
                    {"name": "csrf", "value": "csrf-value", "domain": "365.kdocs.cn", "path": "/"},
                ]
            )

    def test_local_credentials_are_written_with_restricted_modes(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh; csrf=csrf", csrf_token="csrf")
        with TemporaryDirectory() as directory:
            cookie_path, csrf_path = write_local_credentials(
                credentials,
                output_dir=Path(directory) / "secrets",
            )
            self.assertEqual(cookie_path.read_text(encoding="utf-8").strip(), credentials.cookie)
            self.assertEqual(csrf_path.read_text(encoding="utf-8").strip(), credentials.csrf_token)
            self.assertEqual(os.stat(cookie_path).st_mode & 0o777, 0o600)
            self.assertEqual(os.stat(csrf_path).st_mode & 0o777, 0o600)
            self.assertEqual(os.stat(cookie_path.parent).st_mode & 0o777, 0o700)

    def test_workspace_page_url_is_parsed_without_network_calls(self) -> None:
        selection = workspace_from_page_url(
            "https://365.kdocs.cn/space/tenant-1/group-2/root-3/?view=list"
        )

        self.assertEqual(
            selection,
            WpsWorkspaceSelection(
                tenant_id="tenant-1",
                group_id="group-2",
                root_id="root-3",
            ),
        )
        self.assertIsNone(workspace_from_page_url("https://365.kdocs.cn/space/"))
        with self.assertRaisesRegex(LoginError, "WPS"):
            workspace_from_page_url("https://example.com/space/a/b/c")

    def test_workspace_root_page_url_ignores_restored_folder(self) -> None:
        self.assertEqual(
            workspace_root_from_page_url("https://365.kdocs.cn/space/tenant-1/group-2/root-3"),
            WpsWorkspaceSelection(tenant_id="tenant-1", group_id="group-2", root_id="0"),
        )
        self.assertEqual(
            workspace_root_from_page_url("https://365.kdocs.cn/space/tenant-1/group-2/"),
            WpsWorkspaceSelection(tenant_id="tenant-1", group_id="group-2", root_id="0"),
        )
        self.assertIsNone(workspace_root_from_page_url("https://365.kdocs.cn/space/"))

    def test_local_workspace_is_written_with_restricted_mode(self) -> None:
        workspace = WpsWorkspaceSelection("tenant", "group", "root")
        with TemporaryDirectory() as directory:
            path = write_local_workspace(workspace, output_dir=Path(directory) / "secrets")

            self.assertEqual(
                json.loads(path.read_text(encoding="utf-8")),
                {"group_id": "group", "root_id": "root"},
            )
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)

    def test_ssh_payload_keeps_secret_values_out_of_command_arguments(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh-secret; csrf=csrf-secret", csrf_token="csrf-secret")
        with patch("wps_adapter.login.subprocess.run") as run:
            run.return_value = SimpleNamespace(returncode=0)
            push_credentials_over_ssh(
                credentials,
                ssh_target="root@vps-host",
                identity_file="/tmp/id_ed25519",
            )

        command = run.call_args.args[0]
        command_text = " ".join(command)
        self.assertNotIn("refresh-secret", command_text)
        self.assertNotIn("csrf-secret", command_text)
        payload = json.loads(run.call_args.kwargs["input"])
        self.assertEqual(payload["cookie"], credentials.cookie)
        self.assertEqual(payload["csrf"], credentials.csrf_token)
        self.assertEqual(command[:4], ["ssh", "-F", "/dev/null", "-i"])

    def test_ssh_payload_includes_selected_workspace_without_exposing_secrets(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh-secret", csrf_token="csrf-secret")
        workspace = WpsWorkspaceSelection("tenant", "group", "root")
        with patch("wps_adapter.login.subprocess.run") as run:
            run.return_value = SimpleNamespace(returncode=0)
            push_credentials_over_ssh(
                credentials,
                ssh_target="root@vps-host",
                workspace=workspace,
            )

        payload = json.loads(run.call_args.kwargs["input"])
        self.assertEqual(payload["workspace_path"], "/etc/wps-adapter/secrets/wps-workspace.json")
        self.assertEqual(payload["workspace"], {"group_id": "group", "root_id": "root"})
        self.assertNotIn("refresh-secret", " ".join(run.call_args.args[0]))

    def test_ssh_rejects_paths_outside_the_adapter_secret_directory(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh", csrf_token="csrf")
        with self.assertRaisesRegex(LoginError, "secret"):
            push_credentials_over_ssh(
                credentials,
                ssh_target="root@vps-host",
                cookie_path="/tmp/wps-cookie",
            )

    def test_ssh_password_auth_uses_native_ssh_prompt(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh-secret", csrf_token="csrf-secret")
        with patch("wps_adapter.login.subprocess.run") as run:
            run.return_value = SimpleNamespace(returncode=0)
            push_credentials_over_ssh(
                credentials,
                ssh_target="root@vps-host",
                password_auth=True,
                port=2222,
            )

        command = run.call_args.args[0]
        self.assertIn("-p", command)
        self.assertEqual(command[command.index("-p") + 1], "2222")
        self.assertIn("PubkeyAuthentication=no", command)
        self.assertIn("PreferredAuthentications=password,keyboard-interactive", command)
        self.assertIsNone(run.call_args.kwargs["stderr"])

    def test_remote_ssh_writer_preserves_existing_secret_owner(self) -> None:
        with TemporaryDirectory() as directory:
            cookie_path = Path(directory) / "wps-cookie"
            csrf_path = Path(directory) / "wps-csrf"
            workspace_path = Path(directory) / "wps-workspace.json"
            cookie_path.write_text("old-cookie\n", encoding="utf-8")
            csrf_path.write_text("old-csrf\n", encoding="utf-8")
            before = os.stat(cookie_path)
            payload = json.dumps(
                {
                    "cookie_path": str(cookie_path),
                    "csrf_path": str(csrf_path),
                    "cookie": "new-cookie",
                    "csrf": "new-csrf",
                    "workspace_path": str(workspace_path),
                    "workspace": {"group_id": "new-group", "root_id": "new-root"},
                }
            ).encode("utf-8")
            script = _REMOTE_WRITE_SCRIPT.replace(
                'SECRET_DIR = "/etc/wps-adapter/secrets"',
                f"SECRET_DIR = {str(directory)!r}",
                1,
            )

            completed = subprocess.run(
                [sys.executable, "-c", script],
                input=payload,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )

            self.assertEqual(completed.returncode, 0, completed.stderr.decode())
            after = os.stat(cookie_path)
            self.assertEqual(after.st_uid, before.st_uid)
            self.assertEqual(after.st_gid, before.st_gid)
            self.assertEqual(cookie_path.read_text(encoding="utf-8").strip(), "new-cookie")
            self.assertEqual(csrf_path.read_text(encoding="utf-8").strip(), "new-csrf")
            self.assertEqual(
                json.loads(workspace_path.read_text(encoding="utf-8")),
                {"group_id": "new-group", "root_id": "new-root"},
            )

    def test_https_push_uses_basic_auth_and_fixed_path(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh; csrf=csrf", csrf_token="csrf")
        workspace = WpsWorkspaceSelection("tenant", "group", "root")
        captured: dict[str, object] = {}

        class Response:
            status = 200

            def read(self) -> bytes:
                return b'{"status":"ok","cookie_count":2}'

        class Connection:
            def request(self, method, path, *, body, headers) -> None:
                captured.update(method=method, path=path, body=body, headers=headers)

            def getresponse(self) -> Response:
                return Response()

            def close(self) -> None:
                captured["closed"] = True

        def factory(host, port, timeout):
            captured.update(host=host, port=port, timeout=timeout)
            return Connection()

        push_credentials_over_https(
            credentials,
            cookies=[
                {"name": "rtk", "value": "refresh", "domain": ".kdocs.cn", "path": "/passport/secure"},
                {"name": "csrf", "value": "csrf", "domain": "365.kdocs.cn", "path": "/"},
            ],
            workspace=workspace,
            adapter_url="https://adapter.example:18080",
            username="adapter",
            password="secret",
            connection_factory=factory,
        )

        self.assertEqual(captured["method"], "POST")
        self.assertEqual(captured["path"], "/api/v1/session/import")
        self.assertEqual(captured["port"], 18080)
        self.assertNotIn("secret", str(captured["path"]))
        self.assertEqual(
            captured["headers"]["Authorization"],  # type: ignore[index]
            "Basic " + base64.b64encode(b"adapter:secret").decode("ascii"),
        )
        body = json.loads(captured["body"].decode("utf-8"))  # type: ignore[union-attr]
        self.assertEqual(body["cookies"][0]["value"], "refresh")
        self.assertEqual(body["workspace"], {"group_id": "group", "root_id": "root"})
        self.assertTrue(captured["closed"])

    def test_adapter_port_is_added_when_url_omits_one(self) -> None:
        self.assertEqual(
            _apply_adapter_port("https://adapter.example", 18080),
            "https://adapter.example:18080",
        )
        self.assertEqual(
            _apply_adapter_port("https://adapter.example:9443", None),
            "https://adapter.example:9443",
        )

    def test_https_push_rejects_remote_plain_http(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh; csrf=csrf", csrf_token="csrf")
        with self.assertRaisesRegex(LoginError, "allow-http"):
            push_credentials_over_https(
                credentials,
                cookies=[{"name": "rtk", "value": "refresh", "domain": ".kdocs.cn", "path": "/"}],
                adapter_url="http://adapter.example",
                username="adapter",
                password="secret",
            )

    def test_http_push_works_when_explicitly_allowed(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh; csrf=csrf", csrf_token="csrf")
        captured: dict[str, object] = {}

        class Response:
            status = 200

            def read(self) -> bytes:
                return b'{"status":"ok"}'

        class Connection:
            def request(self, method, path, *, body, headers) -> None:
                captured.update(method=method, path=path, body=body, headers=headers)

            def getresponse(self) -> Response:
                return Response()

            def close(self) -> None:
                captured["closed"] = True

        def factory(host, port, timeout):
            captured.update(host=host, port=port, timeout=timeout)
            return Connection()

        push_credentials_over_https(
            credentials,
            cookies=[
                {"name": "rtk", "value": "refresh", "domain": ".kdocs.cn", "path": "/"},
                {"name": "csrf", "value": "csrf", "domain": "365.kdocs.cn", "path": "/"},
            ],
            adapter_url="http://adapter.example:18080",
            username="adapter",
            password="secret",
            allow_insecure_http=True,
            connection_factory=factory,
        )

        self.assertEqual(captured["host"], "adapter.example")
        self.assertEqual(captured["port"], 18080)
        self.assertTrue(captured["closed"])

    def test_adapter_sync_rejects_oversized_response(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh; csrf=csrf", csrf_token="csrf")

        class Response:
            status = 200

            def read(self, _size=None) -> bytes:
                return b"x" * (1024 * 1024 + 1)

        class Connection:
            def request(self, method, path, *, body, headers) -> None:
                pass

            def getresponse(self) -> Response:
                return Response()

            def close(self) -> None:
                pass

        with self.assertRaisesRegex(LoginError, "响应过大"):
            push_credentials_over_https(
                credentials,
                cookies=[{"name": "rtk", "value": "refresh", "domain": ".kdocs.cn"}],
                adapter_url="https://adapter.example",
                username="adapter",
                password="secret",
                connection_factory=lambda _host, _port, _timeout: Connection(),
            )

    def test_login_cookie_detection_does_not_require_enter(self) -> None:
        class Session:
            def cookies(self):
                return [
                    {"name": "rtk", "value": "refresh", "domain": ".kdocs.cn", "path": "/passport/secure"},
                    {"name": "csrf", "value": "csrf", "domain": "365.kdocs.cn", "path": "/"},
                ]

        credentials, names, selected = wait_for_login_credentials(
            Session(),
            login_url="https://365.kdocs.cn/space/",
            domain_suffix="kdocs.cn",
            timeout=1,
        )
        self.assertEqual(names, ("csrf", "rtk"))
        self.assertEqual(credentials.csrf_token, "csrf")
        self.assertEqual(len(selected), 2)

    def test_login_snapshot_defaults_to_enterprise_root(self) -> None:
        class Session:
            def current_url(self):
                return "https://365.kdocs.cn/space/tenant/group/root"

            def cookies(self):
                return [
                    {"name": "rtk", "value": "refresh", "domain": ".kdocs.cn", "path": "/"},
                    {"name": "csrf", "value": "csrf", "domain": "365.kdocs.cn", "path": "/"},
                ]

        credentials, names, selected, workspace = wait_for_login_snapshot(
            Session(),
            login_url="https://365.kdocs.cn/space/",
            domain_suffix="kdocs.cn",
            timeout=1,
        )
        self.assertEqual(credentials.csrf_token, "csrf")
        self.assertEqual(names, ("csrf", "rtk"))
        self.assertEqual(len(selected), 2)
        self.assertEqual(workspace.group_id, "group")
        self.assertEqual(workspace.root_id, "0")

    def test_login_snapshot_accepts_an_explicit_folder_url(self) -> None:
        class Session:
            def current_url(self):
                return "https://365.kdocs.cn/space/tenant/group/root"

            def cookies(self):
                return [
                    {"name": "rtk", "value": "refresh", "domain": ".kdocs.cn", "path": "/"},
                    {"name": "csrf", "value": "csrf", "domain": "365.kdocs.cn", "path": "/"},
                ]

        _credentials, _names, _selected, workspace = wait_for_login_snapshot(
            Session(),
            login_url="https://365.kdocs.cn/space/",
            domain_suffix="kdocs.cn",
            timeout=1,
            workspace_url="https://365.kdocs.cn/space/tenant/group/root?view=list",
        )
        self.assertEqual(
            workspace,
            WpsWorkspaceSelection(tenant_id="tenant", group_id="group", root_id="root"),
        )

    def test_login_snapshot_rejects_a_different_explicit_folder(self) -> None:
        class Session:
            def current_url(self):
                return "https://365.kdocs.cn/space/tenant/group/other-root"

            def cookies(self):
                return [
                    {"name": "rtk", "value": "refresh", "domain": ".kdocs.cn", "path": "/"},
                    {"name": "csrf", "value": "csrf", "domain": "365.kdocs.cn", "path": "/"},
                ]

        with self.assertRaisesRegex(LoginError, "workspace-url"):
            wait_for_login_snapshot(
                Session(),
                login_url="https://365.kdocs.cn/space/",
                domain_suffix="kdocs.cn",
                timeout=0.01,
                workspace_url="https://365.kdocs.cn/space/tenant/group/root",
            )

    def test_chrome_session_reads_current_url_via_runtime_evaluate(self) -> None:
        calls = []

        class Connection:
            def call(self, method, params):
                calls.append((method, params))
                return {"result": {"type": "string", "value": "https://365.kdocs.cn/space/t/g/r"}}

        session = ChromeLoginSession()
        session._connection = Connection()  # type: ignore[assignment]
        self.assertEqual(session.current_url(), "https://365.kdocs.cn/space/t/g/r")
        self.assertEqual(calls[0][0], "Runtime.evaluate")
        self.assertEqual(calls[0][1]["expression"], "location.href")

    def test_chrome_session_can_navigate_to_selected_wps_root(self) -> None:
        calls = []

        class Connection:
            def call(self, method, params):
                calls.append((method, params))
                return {}

        session = ChromeLoginSession()
        session._connection = Connection()  # type: ignore[assignment]
        session.navigate("https://365.kdocs.cn/space/tenant/group/")
        self.assertEqual(calls, [("Page.navigate", {"url": "https://365.kdocs.cn/space/tenant/group/"})])

    def test_chrome_session_close_ignores_cache_cleanup_race(self) -> None:
        class Profile:
            def cleanup(self) -> None:
                raise OSError("directory not empty")

        session = ChromeLoginSession()
        session._profile = Profile()  # type: ignore[assignment]
        session.close()

    def test_login_target_is_validated_before_browser_starts(self) -> None:
        with patch("wps_adapter.login.ChromeLoginSession") as session:
            with self.assertRaisesRegex(LoginError, "绝对路径"):
                login_and_sync(output_dir="relative")
        session.assert_not_called()

    def test_login_url_must_be_a_wps_host(self) -> None:
        with self.assertRaisesRegex(LoginError, "WPS"):
            credentials_from_cookies(
                [
                    {"name": "rtk", "value": "refresh", "domain": ".example.com", "path": "/"},
                    {"name": "csrf", "value": "csrf", "domain": ".example.com", "path": "/"},
                ],
                base_url="https://login.example.com/",
                domain_suffix="example.com",
            )

    def test_cdp_websocket_must_stay_on_loopback(self) -> None:
        with self.assertRaisesRegex(LoginError, "调试接口地址无效"):
            _WebSocket("ws://attacker.example:9222/devtools/page/1")

    def test_interactive_target_supports_ssh_password_login(self) -> None:
        with patch(
            "builtins.input",
            side_effect=["vps.example", "2", "deploy", "2222"],
        ):
            target = _prompt_login_target()

        self.assertEqual(target.ssh_target, "deploy@vps.example")
        self.assertEqual(target.ssh_port, 2222)
        self.assertTrue(target.ssh_password_auth)
        self.assertIsNone(target.ssh_identity)

    def test_interactive_target_includes_custom_adapter_port(self) -> None:
        with patch(
            "builtins.input",
            side_effect=["vps.example", "3", "18080", ""],
        ):
            target = _prompt_login_target()

        self.assertEqual(target.adapter_url, "http://vps.example:18080")
        self.assertEqual(target.adapter_port, 18080)

    def test_interactive_target_rejects_conflicting_url_port(self) -> None:
        with patch(
            "builtins.input",
            side_effect=["vps.example", "3", "18080", "https://vps.example:9443"],
        ):
            with self.assertRaisesRegex(LoginError, "端口输入不一致"):
                _prompt_login_target()


if __name__ == "__main__":
    unittest.main()
