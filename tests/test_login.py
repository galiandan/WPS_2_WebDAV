from __future__ import annotations

import json
import os
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest.mock import patch

from wps_adapter.client import WpsCredentials
from wps_adapter.login import (
    LoginError,
    credentials_from_cookies,
    login_and_sync,
    push_credentials_over_ssh,
    write_local_credentials,
)


class LoginHelperTests(unittest.TestCase):
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

    def test_login_target_is_validated_before_browser_starts(self) -> None:
        with patch("wps_adapter.login.ChromeLoginSession") as session:
            with self.assertRaisesRegex(LoginError, "绝对路径"):
                login_and_sync(output_dir="relative")
        session.assert_not_called()


if __name__ == "__main__":
    unittest.main()
