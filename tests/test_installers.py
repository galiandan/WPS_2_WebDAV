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

    def test_release_manifest_matches_its_builder(self) -> None:
        completed = subprocess.run(
            [sys.executable, "tools/build_release_manifest.py", "--check"],
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

    def test_uninstaller_preserves_credentials_by_default_and_checks_ownership(self) -> None:
        script = (PROJECT_ROOT / "scripts/uninstall.sh").read_text(encoding="utf-8")
        self.assertIn("--purge", script)
        self.assertIn("--remove-image", script)
        self.assertIn("--yes", script)
        self.assertIn("/etc/wps-adapter/secrets/", script)
        self.assertIn("com.galiandan.wps-adapter.managed", script)
        self.assertIn("Description=WPS enterprise cloud drive WebDAV adapter", script)
        self.assertIn("python3(\\.[0-9]+)* -m wps_adapter serve", script)
        self.assertIn("不会删除 WPS 云盘上的远端文件", script)
        self.assertNotIn("rm -rf / ", script)

        readme = (PROJECT_ROOT / "README.md").read_text(encoding="utf-8")
        self.assertIn("scripts/uninstall.sh", readme)
        self.assertIn("--purge", readme)
        self.assertIn("--remove-image", readme)

    def test_installers_select_the_invoking_user_and_protect_secrets(self) -> None:
        native = (PROJECT_ROOT / "scripts/install-native.sh").read_text(encoding="utf-8")
        docker = (PROJECT_ROOT / "scripts/install-docker.sh").read_text(encoding="utf-8")

        for script in (native, docker):
            self.assertIn('SUDO_USER', script)
            self.assertIn('install -o "$RUN_USER" -g "$RUN_GROUP" -m 600', script)
            self.assertIn('[[ "$secret_path" == "$SECRET_DIR"/* ]]', script)
            self.assertIn('relative_path', script)
            self.assertIn('不能是符号链接', script)
            self.assertIn('SOURCE_REF="${WPS_ADAPTER_SOURCE_REF:-', script)
            self.assertIn('SOURCE_REF.tar.gz', script)
            self.assertIn('source-ref 必须是 40 位 Git 提交号', script)
            self.assertIn('SOURCE_MANIFEST_SHA256', script)
            self.assertIn('--source-manifest-sha256', script)
            self.assertIn('sha256sum -c release-manifest.txt', script)
            self.assertIn('--max-filesize 52428800', script)
            self.assertIn('GROUP_ID="${GROUP_ID_ARG:-${OLD_GROUP_ID:-auto}}"', script)
            self.assertIn('ROOT_ID="${ROOT_ID_ARG:-${OLD_ROOT_ID:-auto}}"', script)
            self.assertIn('WPS_WORKSPACE_FILE', script)
            self.assertNotIn('ask_value "WPS 企业群组 ID"', script)
            self.assertIn('--progress-bar --location --max-filesize 52428800', script)
            self.assertIn('WPS_ADAPTER_ARCHIVE_URL', script)
            self.assertIn('DOWNLOAD_CONNECT_TIMEOUT', script)
            self.assertIn('DOWNLOAD_MAX_TIME', script)
            self.assertIn('拒绝非 HTTPS 下载地址', script)
            self.assertIn('archive_members_are_safe', script)
            self.assertNotIn('printf \'尝试下载源代码：%s\\n\' "$candidate"', script)
            self.assertIn('apt', script)
            self.assertIn('dnf', script)
            self.assertIn('apk', script)
            self.assertIn('pacman', script)
            self.assertIn('zypper', script)
            self.assertIn('xbps-install', script)
            self.assertNotIn('find . -mindepth', script)
            self.assertIn('progress_step', script)

        readme = (PROJECT_ROOT / "README.md").read_text(encoding="utf-8")
        deployment = (PROJECT_ROOT / "docs/deployment.md").read_text(encoding="utf-8")
        for document in (readme, deployment):
            self.assertIn("gh-proxy.com", document)
            self.assertIn("ghfast.top", document)
            self.assertIn("--connect-timeout 10", document)
            self.assertIn("--max-time 60", document)
            self.assertNotIn("download_and_run", document)
            self.assertIn("set -o pipefail; curl -fL --progress-bar --connect-timeout 10 --max-time 60 --retry 1", document)
            self.assertIn("scripts/install-native.sh", document)
            self.assertIn("scripts/install-docker.sh", document)

        self.assertIn('awk -v run_user="$RUN_USER" -v run_group="$RUN_GROUP"', native)
        self.assertIn('systemctl is-enabled --quiet wps-adapter.service 2>/dev/null', native)
        self.assertIn('--user "$RUN_UID:$RUN_GID"', docker)
        self.assertIn('host_uses_systemd', native)
        self.assertIn('SERVICE_MODE="direct"', native)
        self.assertIn('BASE_IMAGE', docker)
        self.assertIn('docker.m.daocloud.io/library/python:3.12-slim', docker)
        self.assertIn('DOCKER_SERVICE_MODE="openrc"', docker)

        dockerfile = (PROJECT_ROOT / "deploy/Dockerfile").read_text(encoding="utf-8")
        self.assertIn('ARG BASE_IMAGE=python:3.12-slim', dockerfile)

    def test_installers_accept_default_and_ipv6_bind_addresses(self) -> None:
        bind_check = r'''set -eu
for bind in 0.0.0.0 127.0.0.1 :: '[::1]' host-name; do
    [[ "$bind" =~ ^\[?[A-Za-z0-9.:-]+\]?$ ]]
done
'''
        completed = subprocess.run(
            ["bash", "-c", bind_check],
            cwd=PROJECT_ROOT,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=False,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)

        for installer in ("scripts/install-native.sh", "scripts/install-docker.sh"):
            script = (PROJECT_ROOT / installer).read_text(encoding="utf-8")
            self.assertIn(
                r'[[ "$BIND" =~ ^\[?[A-Za-z0-9.:-]+\]?$ ]]',
                script,
            )

    def test_compose_passes_current_user_identity_to_the_container(self) -> None:
        compose = (PROJECT_ROOT / "deploy/docker-compose.yml").read_text(encoding="utf-8")

        self.assertIn('BASE_IMAGE: "${WPS_ADAPTER_DOCKER_BASE_IMAGE:-python:3.12-slim}"', compose)
        self.assertIn('APP_UID: "${WPS_ADAPTER_UID:-1000}"', compose)
        self.assertIn('APP_GID: "${WPS_ADAPTER_GID:-1000}"', compose)
        self.assertIn('user: "${WPS_ADAPTER_UID:-1000}:${WPS_ADAPTER_GID:-1000}"', compose)
        self.assertIn('/etc/wps-adapter/secrets:/etc/wps-adapter/secrets:rw', compose)
        self.assertIn('/etc/wps-adapter/secrets/adapter-username:/etc/wps-adapter/secrets/adapter-username:ro', compose)
        self.assertIn('/etc/wps-adapter/secrets/adapter-password:/etc/wps-adapter/secrets/adapter-password:ro', compose)


if __name__ == "__main__":
    unittest.main()
