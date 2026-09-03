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
from wps_adapter.login import (
    LoginError,
    _REMOTE_WRITE_SCRIPT,
    credentials_from_cookies,
    login_and_sync,
    push_credentials_over_ssh,
    push_credentials_over_https,
    wait_for_login_credentials,
    write_local_credentials,
)


PROJECT_ROOT = Path(__file__).parents[1]


class LoginHelperTests(unittest.TestCase):
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
            cookie_path.write_text("old-cookie\n", encoding="utf-8")
            csrf_path.write_text("old-csrf\n", encoding="utf-8")
            before = os.stat(cookie_path)
            payload = json.dumps(
                {
                    "cookie_path": str(cookie_path),
                    "csrf_path": str(csrf_path),
                    "cookie": "new-cookie",
                    "csrf": "new-csrf",
                }
            ).encode("utf-8")

            completed = subprocess.run(
                [sys.executable, "-c", _REMOTE_WRITE_SCRIPT],
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

    def test_https_push_uses_basic_auth_and_fixed_path(self) -> None:
        credentials = WpsCredentials(cookie="rtk=refresh; csrf=csrf", csrf_token="csrf")
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

    def test_login_target_is_validated_before_browser_starts(self) -> None:
        with patch("wps_adapter.login.ChromeLoginSession") as session:
            with self.assertRaisesRegex(LoginError, "绝对路径"):
                login_and_sync(output_dir="relative")
        session.assert_not_called()

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
