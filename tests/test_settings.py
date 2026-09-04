from __future__ import annotations

import json
import os
import stat
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from wps_adapter.settings import WebSettings, WebSettingsError


class WebSettingsTests(unittest.TestCase):
    def test_name_falls_back_and_survives_restart(self) -> None:
        with TemporaryDirectory() as directory:
            path = Path(directory) / "web-settings.json"
            settings = WebSettings(str(path), fallback_name="初始云盘")
            self.assertEqual(settings.name, "初始云盘")

            self.assertEqual(settings.set_name("我的学校云盘"), "我的学校云盘")
            self.assertEqual(
                json.loads(path.read_text(encoding="utf-8")),
                {"name": "我的学校云盘"},
            )
            self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
            self.assertEqual(WebSettings(str(path), fallback_name="其他名称").name, "我的学校云盘")

    def test_name_validation_allows_normal_text_and_rejects_unsafe_values(self) -> None:
        with TemporaryDirectory() as directory:
            settings = WebSettings(str(Path(directory) / "web-settings.json"))
            for value in ("  我的云盘  ", "资料 / 2026", "云盘 <测试>"):
                settings.set_name(value)

            for value in ("", "   ", "bad\nname", "bad\x00name", 123):
                with self.assertRaises(WebSettingsError):
                    settings.set_name(value)

    def test_existing_symlink_is_rejected(self) -> None:
        with TemporaryDirectory() as directory:
            target = Path(directory) / "real.json"
            target.write_text('{"name":"safe"}\n', encoding="utf-8")
            target.chmod(0o600)
            link = Path(directory) / "web-settings.json"
            link.symlink_to(target)
            with self.assertRaises(WebSettingsError):
                WebSettings(str(link))


if __name__ == "__main__":
    unittest.main()
