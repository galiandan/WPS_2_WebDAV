from __future__ import annotations

import unittest
from pathlib import Path
import subprocess
import sys


PROJECT_ROOT = Path(__file__).parents[1]


class InstallerTemplateTests(unittest.TestCase):
    def test_standalone_login_script_matches_its_builder(self) -> None:
        completed = subprocess.run(
            [sys.executable, "tools/build_login_script.py", "--check"],
            cwd=PROJECT_ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=False,
        )

        self.assertEqual(completed.returncode, 0, completed.stderr)

    def test_docker_installer_builds_the_deploy_dockerfile(self) -> None:
        script = (PROJECT_ROOT / "scripts/install-docker.sh").read_text(encoding="utf-8")

        self.assertIn('--file "$SOURCE_DIR/deploy/Dockerfile"', script)
        self.assertNotIn('docker build --tag "$IMAGE_NAME" "$APP_DIR"', script)
        self.assertLess(
            script.index("docker build "),
            script.index("systemctl stop wps-adapter.service"),
        )
        self.assertIn("systemctl start wps-adapter.service", script)

    def test_docker_installer_protects_unknown_same_name_containers(self) -> None:
        script = (PROJECT_ROOT / "scripts/install-docker.sh").read_text(encoding="utf-8")

        self.assertIn("com.galiandan.wps-adapter.managed", script)
        self.assertIn("发现同名但不属于本项目的 Docker 容器", script)

    def test_installers_select_the_invoking_user_and_protect_secrets(self) -> None:
        native = (PROJECT_ROOT / "scripts/install-native.sh").read_text(encoding="utf-8")
        docker = (PROJECT_ROOT / "scripts/install-docker.sh").read_text(encoding="utf-8")

        for script in (native, docker):
            self.assertIn('SUDO_USER', script)
            self.assertIn('chown "$RUN_USER:$RUN_GROUP"', script)
            self.assertIn('chmod 600 "$secret_path"', script)
            self.assertIn('[[ "$secret_path" == "$SECRET_DIR"/* ]]', script)

        self.assertIn('awk -v run_user="$RUN_USER" -v run_group="$RUN_GROUP"', native)
        self.assertIn('--user "$RUN_UID:$RUN_GID"', docker)

    def test_compose_passes_current_user_identity_to_the_container(self) -> None:
        compose = (PROJECT_ROOT / "deploy/docker-compose.yml").read_text(encoding="utf-8")

        self.assertIn('APP_UID: "${WPS_ADAPTER_UID:-1000}"', compose)
        self.assertIn('APP_GID: "${WPS_ADAPTER_GID:-1000}"', compose)
        self.assertIn('user: "${WPS_ADAPTER_UID:-1000}:${WPS_ADAPTER_GID:-1000}"', compose)


if __name__ == "__main__":
    unittest.main()
