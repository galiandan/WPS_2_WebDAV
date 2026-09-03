"""Interactive WPS login bootstrap using a local Chrome session.

The adapter cannot read cookies from a normal web page: WPS is a different
origin and the important session cookies are HttpOnly.  This module uses the
Chrome DevTools Protocol only with a temporary, isolated browser profile.  A
person completes the login in the official WPS page, then the helper reads
the cookies Chrome itself has stored and sends the minimum useful snapshot to
the adapter host over SSH.

No WPS password is handled by this process and no cookie value is printed.
The server-side adapter continues to use its existing ``rtk`` refresh flow
after this one-time bootstrap.
"""

from __future__ import annotations

import base64
import json
import os
import secrets
import shlex
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from urllib.error import URLError
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, Request, build_opener

from .client import WpsCredentials


DEFAULT_LOGIN_URL = "https://365.kdocs.cn/space/"
DEFAULT_COOKIE_DOMAIN_SUFFIX = "kdocs.cn"
DEFAULT_REMOTE_COOKIE_PATH = "/etc/wps-adapter/secrets/wps-cookie"
DEFAULT_REMOTE_CSRF_PATH = "/etc/wps-adapter/secrets/wps-csrf"


class LoginError(RuntimeError):
    """A safe, user-facing error from the interactive login helper."""


def _host_from_url(url: str) -> str:
    parts = urlsplit(url)
    if parts.scheme != "https" or not parts.hostname or parts.username or parts.password:
        raise LoginError("登录地址必须是不带账号信息的 HTTPS 地址")
    return parts.hostname.rstrip(".").casefold()


def _domain_without_dot(value: str) -> str:
    return value.strip().lstrip(".").rstrip(".").casefold()


def _domain_matches_host(domain: str, host: str) -> bool:
    normalized = _domain_without_dot(domain)
    return bool(normalized) and (host == normalized or host.endswith("." + normalized))


def _domain_is_allowed(domain: str, suffix: str) -> bool:
    normalized = _domain_without_dot(domain)
    allowed = _domain_without_dot(suffix)
    return bool(normalized and allowed) and (
        normalized == allowed or normalized.endswith("." + allowed)
    )


def _cookie_rank(cookie: Mapping[str, object], host: str) -> tuple[int, int, int]:
    domain = _domain_without_dot(str(cookie.get("domain", "")))
    path = str(cookie.get("path", "/"))
    # Prefer a cookie set for the exact drive host, then the more specific
    # domain/path.  Chrome can contain same-name cookies at multiple scopes.
    return (1 if domain == host else 0, len(domain), len(path))


def _safe_cookie_part(value: str, *, name: bool = False) -> bool:
    if not value or any(ord(char) < 0x20 or ord(char) == 0x7F for char in value):
        return False
    if ";" in value:
        return False
    if name and any(char in value for char in "()<>@,;:\\\"/[]?={} \t"):
        return False
    return True


def _select_cookies(
    cookies: Sequence[Mapping[str, object]],
    *,
    host: str,
    domain_suffix: str,
) -> list[Mapping[str, object]]:
    selected: dict[str, Mapping[str, object]] = {}
    for cookie in cookies:
        if not isinstance(cookie, Mapping):
            continue
        domain = str(cookie.get("domain", ""))
        if not _domain_matches_host(domain, host):
            continue
        if not _domain_is_allowed(domain, domain_suffix):
            continue
        name = str(cookie.get("name", ""))
        value = str(cookie.get("value", ""))
        if not _safe_cookie_part(name, name=True) or not _safe_cookie_part(value):
            continue
        key = name.casefold()
        previous = selected.get(key)
        if previous is None or _cookie_rank(cookie, host) > _cookie_rank(previous, host):
            selected[key] = cookie
    return sorted(selected.values(), key=lambda item: str(item.get("name", "")).casefold())


def credentials_from_cookies(
    cookies: Sequence[Mapping[str, object]],
    *,
    base_url: str = DEFAULT_LOGIN_URL,
    domain_suffix: str = DEFAULT_COOKIE_DOMAIN_SUFFIX,
    require_refresh_cookie: bool = True,
) -> tuple[WpsCredentials, tuple[str, ...]]:
    """Build the adapter credential snapshot from Chrome cookie objects.

    Cookies are limited to the configured WPS domain suffix and cookies that
    Chrome says match the drive host.  ``rtk`` is retained even though its
    browser path is ``/passport/secure`` because the adapter sends the same
    stored snapshot to the confirmed account refresh endpoint.
    """

    host = _host_from_url(base_url)
    selected = _select_cookies(cookies, host=host, domain_suffix=domain_suffix)
    if not selected:
        raise LoginError("没有找到属于 WPS 云盘的登录 Cookie，请确认已经登录")

    pairs = [
        f"{cookie['name']}={cookie.get('value', '')}"
        for cookie in selected
    ]
    by_name = {
        str(cookie.get("name", "")).casefold(): str(cookie.get("value", ""))
        for cookie in selected
    }
    csrf = by_name.get("csrf", "")
    if not csrf:
        raise LoginError("登录 Cookie 中没有 csrf，请在 WPS 页面完成登录后重试")
    if require_refresh_cookie and not by_name.get("rtk"):
        raise LoginError(
            "登录 Cookie 中没有 rtk，无法启用自动续期；请使用此助手重新登录 WPS 后重试"
        )
    return WpsCredentials(cookie="; ".join(pairs), csrf_token=csrf), tuple(
        str(cookie.get("name", "")) for cookie in selected
    )


def _atomic_write(path: str | Path, value: str) -> None:
    target = Path(path)
    if not target.is_absolute():
        raise LoginError("凭据文件路径必须是绝对路径")
    if "\x00" in str(target) or "\n" in str(target) or "\r" in str(target):
        raise LoginError("凭据文件路径无效")
    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    try:
        os.chmod(target.parent, 0o700)
    except OSError as exc:
        raise LoginError("无法保护凭据目录") from exc
    descriptor, temporary = tempfile.mkstemp(
        prefix=f".{target.name}.",
        dir=str(target.parent),
        text=True,
    )
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            descriptor = -1
            stream.write(value)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, target)
    finally:
        if descriptor != -1:
            os.close(descriptor)
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def write_local_credentials(
    credentials: WpsCredentials,
    *,
    output_dir: str | Path,
) -> tuple[Path, Path]:
    """Write a local credential pair without putting values on argv."""

    directory = Path(output_dir)
    if not directory.is_absolute():
        raise LoginError("本地凭据目录必须是绝对路径")
    cookie_path = directory / "wps-cookie"
    csrf_path = directory / "wps-csrf"
    _atomic_write(cookie_path, credentials.cookie)
    _atomic_write(csrf_path, credentials.csrf_token)
    return cookie_path, csrf_path


_REMOTE_WRITE_SCRIPT = r'''import json, os, sys, tempfile

def atomic_write(path, value):
    target = os.path.abspath(path)
    directory = os.path.dirname(target)
    os.makedirs(directory, mode=0o700, exist_ok=True)
    os.chmod(directory, 0o700)
    fd, temporary = tempfile.mkstemp(prefix="." + os.path.basename(target) + ".", dir=directory, text=True)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            fd = -1
            stream.write(value)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, target)
    finally:
        if fd != -1:
            os.close(fd)
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass

data = json.loads(sys.stdin.buffer.read().decode("utf-8"))
cookie_path = data["cookie_path"]
csrf_path = data["csrf_path"]
cookie = data["cookie"]
csrf = data["csrf"]
if not all(isinstance(item, str) and item for item in (cookie_path, csrf_path, cookie, csrf)):
    raise ValueError("invalid credential payload")
if any(not item.startswith("/") or any(char in item for char in "\r\n\x00") for item in (cookie_path, csrf_path)):
    raise ValueError("invalid credential path")
if any(any(ord(char) < 0x20 or ord(char) == 0x7f for char in item) for item in (cookie, csrf)):
    raise ValueError("invalid credential value")
atomic_write(cookie_path, cookie)
atomic_write(csrf_path, csrf)
print("credentials-updated")
'''


def push_credentials_over_ssh(
    credentials: WpsCredentials,
    *,
    ssh_target: str,
    cookie_path: str = DEFAULT_REMOTE_COOKIE_PATH,
    csrf_path: str = DEFAULT_REMOTE_CSRF_PATH,
    identity_file: str | None = None,
    timeout: float = 30.0,
) -> None:
    """Atomically replace the remote secret files through an SSH stdin pipe."""

    if not ssh_target or any(char in ssh_target for char in "\r\n\x00"):
        raise LoginError("SSH 目标不能为空且不能包含换行")
    if timeout <= 0:
        raise LoginError("SSH 超时时间必须为正数")
    for path in (cookie_path, csrf_path):
        if not path.startswith("/") or any(char in path for char in "\r\n\x00"):
            raise LoginError("远程凭据路径必须是安全的绝对路径")

    payload = json.dumps(
        {
            "cookie_path": cookie_path,
            "csrf_path": csrf_path,
            "cookie": credentials.cookie,
            "csrf": credentials.csrf_token,
        },
        ensure_ascii=True,
        separators=(",", ":"),
    ).encode("utf-8")
    remote_command = (
        "python3 -c "
        + shlex.quote(_REMOTE_WRITE_SCRIPT)
    )
    command = ["ssh", "-F", "/dev/null"]
    if identity_file:
        command.extend(["-i", identity_file])
    command.extend([ssh_target, remote_command])
    try:
        completed = subprocess.run(
            command,
            input=payload,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
            check=False,
        )
    except FileNotFoundError as exc:
        raise LoginError("本机没有 ssh 命令") from exc
    except subprocess.TimeoutExpired as exc:
        raise LoginError("SSH 同步凭据超时") from exc
    if completed.returncode != 0:
        raise LoginError(
            "SSH 同步凭据失败；请先手动确认 SSH 主机指纹和登录权限，再重试"
        )


class _WebSocket:
    """Tiny client for the local CDP WebSocket, using only the stdlib."""

    def __init__(self, url: str, *, timeout: float = 10.0) -> None:
        parts = urlsplit(url)
        if parts.scheme != "ws" or not parts.hostname or parts.username or parts.password:
            raise LoginError("Chrome 调试接口地址无效")
        self._socket = socket.create_connection(
            (parts.hostname, parts.port or 80),
            timeout=timeout,
        )
        self._socket.settimeout(timeout)
        self._buffer = b""
        self._closed = False
        key = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
        path = parts.path or "/"
        if parts.query:
            path += "?" + parts.query
        host = parts.hostname
        if parts.port:
            host += ":" + str(parts.port)
        request = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: {host}\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n"
            f"Origin: http://127.0.0.1:{parts.port or 80}\r\n"
            "\r\n"
        ).encode("ascii")
        self._socket.sendall(request)
        header = self._read_until(b"\r\n\r\n", limit=64 * 1024)
        status_line = header.split(b"\r\n", 1)[0]
        if not status_line.startswith(b"HTTP/1.1 101"):
            self.close()
            raise LoginError("无法连接 Chrome 登录会话")

    def _read_until(self, marker: bytes, *, limit: int) -> bytes:
        while marker not in self._buffer:
            chunk = self._socket.recv(4096)
            if not chunk:
                raise LoginError("Chrome 调试连接提前关闭")
            self._buffer += chunk
            if len(self._buffer) > limit:
                raise LoginError("Chrome 调试响应过大")
        index = self._buffer.index(marker) + len(marker)
        result, self._buffer = self._buffer[:index], self._buffer[index:]
        return result

    def _read_exact(self, size: int) -> bytes:
        while len(self._buffer) < size:
            chunk = self._socket.recv(max(4096, size - len(self._buffer)))
            if not chunk:
                raise LoginError("Chrome 调试连接提前关闭")
            self._buffer += chunk
        result, self._buffer = self._buffer[:size], self._buffer[size:]
        return result

    def _send_frame(self, opcode: int, payload: bytes) -> None:
        if self._closed:
            raise LoginError("Chrome 调试连接已关闭")
        length = len(payload)
        if length < 126:
            header = bytes([0x80 | opcode, 0x80 | length])
        elif length <= 0xFFFF:
            header = bytes([0x80 | opcode, 0x80 | 126]) + length.to_bytes(2, "big")
        else:
            header = bytes([0x80 | opcode, 0x80 | 127]) + length.to_bytes(8, "big")
        mask = secrets.token_bytes(4)
        masked = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
        self._socket.sendall(header + mask + masked)

    def _receive_frame(self) -> tuple[bool, int, bytes]:
        first, second = self._read_exact(2)
        final = bool(first & 0x80)
        opcode = first & 0x0F
        masked = bool(second & 0x80)
        length = second & 0x7F
        if length == 126:
            length = int.from_bytes(self._read_exact(2), "big")
        elif length == 127:
            length = int.from_bytes(self._read_exact(8), "big")
        if length > 64 * 1024 * 1024:
            raise LoginError("Chrome 调试消息过大")
        mask = self._read_exact(4) if masked else b""
        payload = self._read_exact(length)
        if mask:
            payload = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
        return final, opcode, payload

    def send_json(self, payload: Mapping[str, object]) -> None:
        self._send_frame(
            0x1,
            json.dumps(payload, ensure_ascii=True, separators=(",", ":")).encode("utf-8"),
        )

    def receive_json(self) -> Mapping[str, object]:
        message = bytearray()
        message_opcode: int | None = None
        while True:
            final, opcode, payload = self._receive_frame()
            if opcode == 0x8:
                self.close()
                raise LoginError("Chrome 调试连接已关闭")
            if opcode == 0x9:
                self._send_frame(0xA, payload)
                continue
            if opcode == 0xA:
                continue
            if opcode in {0x1, 0x2}:
                message_opcode = opcode
                message = bytearray(payload)
            elif opcode == 0x0 and message_opcode is not None:
                message.extend(payload)
            else:
                continue
            if final:
                if message_opcode != 0x1:
                    raise LoginError("Chrome 调试消息格式无效")
                try:
                    decoded = json.loads(bytes(message).decode("utf-8"))
                except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                    raise LoginError("Chrome 调试消息格式无效") from exc
                if not isinstance(decoded, Mapping):
                    raise LoginError("Chrome 调试消息格式无效")
                return decoded

    def call(self, method: str, params: Mapping[str, object] | None = None) -> Mapping[str, object]:
        command_id = secrets.randbelow(2**31 - 1) + 1
        payload: dict[str, object] = {"id": command_id, "method": method}
        if params:
            payload["params"] = dict(params)
        self.send_json(payload)
        while True:
            response = self.receive_json()
            if response.get("id") != command_id:
                continue
            if "error" in response:
                raise LoginError(f"Chrome 调试命令失败: {method}")
            result = response.get("result")
            if not isinstance(result, Mapping):
                raise LoginError(f"Chrome 调试命令返回无效结果: {method}")
            return result

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            self._socket.close()
        except OSError:
            pass


def _find_free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def find_browser(explicit: str | None = None) -> str:
    """Find a locally installed Chrome/Chromium executable."""

    if explicit:
        return explicit
    configured = os.environ.get("WPS_BROWSER", "").strip()
    if configured:
        return configured
    candidates = [
        "google-chrome-stable",
        "google-chrome",
        "chromium",
        "chromium-browser",
        "chrome",
    ]
    if sys.platform == "darwin":
        candidates.append("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
    elif sys.platform == "win32":
        local_app_data = os.environ.get("LOCALAPPDATA", "")
        program_files = os.environ.get("PROGRAMFILES", "")
        candidates.extend(
            str(Path(path) / "Google/Chrome/Application/chrome.exe")
            for path in (local_app_data, program_files)
            if path
        )
    for candidate in candidates:
        path = shutil.which(candidate) or candidate
        if Path(path).is_file() and os.access(path, os.X_OK):
            return path
    raise LoginError(
        "本机没有找到 Chrome/Chromium；请安装并从自己的电脑运行此登录助手"
    )


def _cdp_page_url(port: int, *, timeout: float) -> str:
    # The endpoint is local; do not let an HTTP proxy intercept it.
    opener = build_opener(ProxyHandler({}))
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            request = Request(f"http://127.0.0.1:{port}/json/list")
            with opener.open(request, timeout=min(2.0, max(0.1, deadline - time.monotonic()))) as response:
                payload = json.loads(response.read().decode("utf-8"))
            if isinstance(payload, list):
                for target in payload:
                    if (
                        isinstance(target, Mapping)
                        and target.get("type") == "page"
                        and isinstance(target.get("webSocketDebuggerUrl"), str)
                    ):
                        return str(target["webSocketDebuggerUrl"])
        except (OSError, URLError, TimeoutError, ValueError, json.JSONDecodeError) as exc:
            last_error = exc
        time.sleep(0.2)
    suffix = "" if last_error is None else "，请确认 Chrome 已正常启动"
    raise LoginError("等待 Chrome 登录窗口超时" + suffix)


def _wait_for_enter(timeout: float) -> None:
    """Wait for Enter without leaving a headless helper blocked forever."""

    if not sys.stdin.isatty():
        try:
            input()
        except EOFError as exc:
            raise LoginError("登录助手需要在终端中运行") from exc
        return
    if sys.platform == "win32":
        import msvcrt

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if msvcrt.kbhit():
                character = msvcrt.getwch()
                if character in {"\r", "\n"}:
                    return
            time.sleep(0.1)
        raise LoginError("等待登录完成超时")
    import select

    ready, _, _ = select.select([sys.stdin], [], [], timeout)
    if not ready:
        raise LoginError("等待登录完成超时")
    sys.stdin.readline()


@dataclass
class ChromeLoginSession:
    """A temporary visible Chrome profile controlled through local CDP."""

    login_url: str = DEFAULT_LOGIN_URL
    browser: str | None = None
    startup_timeout: float = 20.0
    _process: subprocess.Popen[bytes] | None = None
    _profile: tempfile.TemporaryDirectory[str] | None = None
    _connection: _WebSocket | None = None

    def __enter__(self) -> "ChromeLoginSession":
        _host_from_url(self.login_url)
        if self.startup_timeout <= 0:
            raise LoginError("Chrome 启动超时时间必须为正数")
        browser = find_browser(self.browser)
        self._profile = tempfile.TemporaryDirectory(prefix="wps-login-")
        port = _find_free_port()
        command = [
            browser,
            f"--user-data-dir={self._profile.name}",
            "--no-first-run",
            "--no-default-browser-check",
            "--disable-sync",
            "--remote-debugging-address=127.0.0.1",
            f"--remote-allow-origins=http://127.0.0.1:{port}",
            f"--remote-debugging-port={port}",
            self.login_url,
        ]
        try:
            self._process = subprocess.Popen(
                command,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            page_url = _cdp_page_url(port, timeout=self.startup_timeout)
            self._connection = _WebSocket(page_url, timeout=10.0)
            return self
        except Exception:
            self.close()
            raise

    def cookies(self) -> list[Mapping[str, object]]:
        if self._connection is None:
            raise LoginError("Chrome 登录会话未启动")
        result = self._connection.call("Network.getAllCookies")
        cookies = result.get("cookies")
        if not isinstance(cookies, list):
            raise LoginError("Chrome 没有返回 Cookie")
        return [item for item in cookies if isinstance(item, Mapping)]

    def close(self) -> None:
        if self._connection is not None:
            self._connection.close()
            self._connection = None
        process = self._process
        self._process = None
        if process is not None:
            try:
                process.terminate()
                process.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired):
                try:
                    process.kill()
                except OSError:
                    pass
        profile = self._profile
        self._profile = None
        if profile is not None:
            profile.cleanup()

    def __exit__(self, _exc_type: object, _exc_value: object, _traceback: object) -> None:
        self.close()


def login_and_sync(
    *,
    login_url: str = DEFAULT_LOGIN_URL,
    browser: str | None = None,
    domain_suffix: str = DEFAULT_COOKIE_DOMAIN_SUFFIX,
    wait_timeout: float = 300.0,
    ssh_target: str = "",
    ssh_cookie_path: str = DEFAULT_REMOTE_COOKIE_PATH,
    ssh_csrf_path: str = DEFAULT_REMOTE_CSRF_PATH,
    ssh_identity: str | None = None,
    output_dir: str | None = None,
    ssh_timeout: float = 30.0,
) -> tuple[str, ...]:
    """Open WPS, wait for a human login, then sync a safe credential snapshot."""

    if bool(ssh_target) == bool(output_dir):
        raise LoginError("请在 --ssh-target 和 --output-dir 中选择一个同步目标")
    if wait_timeout <= 0:
        raise LoginError("登录等待时间必须为正数")
    if output_dir is not None and not Path(output_dir).is_absolute():
        raise LoginError("本地凭据目录必须是绝对路径")
    with ChromeLoginSession(login_url=login_url, browser=browser) as session:
        print("WPS 登录窗口已打开。请只在这个官方 WPS 窗口中完成登录。", flush=True)
        print("登录成功并看到云盘页面后，回到此终端按回车继续。", flush=True)
        try:
            _wait_for_enter(wait_timeout)
        except (EOFError, KeyboardInterrupt) as exc:
            raise LoginError("已取消登录同步") from exc
        credentials, names = credentials_from_cookies(
            session.cookies(),
            base_url=login_url,
            domain_suffix=domain_suffix,
        )

    print(
        f"已获取 {len(names)} 个 WPS Cookie（包含 rtk 和 csrf）；Cookie 值不会显示。",
        flush=True,
    )
    if ssh_target:
        push_credentials_over_ssh(
            credentials,
            ssh_target=ssh_target,
            cookie_path=ssh_cookie_path,
            csrf_path=ssh_csrf_path,
            identity_file=ssh_identity,
            timeout=ssh_timeout,
        )
        print("已通过 SSH 更新 VPS 凭据，适配器下次请求会读取新会话。", flush=True)
    else:
        cookie_path, csrf_path = write_local_credentials(credentials, output_dir=output_dir or "")
        print(f"已写入本地凭据文件：{cookie_path}、{csrf_path}", flush=True)
    return names
