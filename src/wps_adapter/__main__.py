from __future__ import annotations

import argparse
import getpass
import os
import sys
from dataclasses import dataclass
from urllib.parse import urlsplit, urlunsplit

from . import __version__
from .client import WpsClientConfig, WpsDriveClient
from .login import (
    DEFAULT_COOKIE_DOMAIN_SUFFIX,
    DEFAULT_LOGIN_URL,
    DEFAULT_REMOTE_COOKIE_PATH,
    DEFAULT_REMOTE_CSRF_PATH,
    LoginError,
    login_and_sync,
)
from .server import AdapterApplication, BasicAuth, create_server
from .storage import WpsStorage


def _env_int(name: str, default: int) -> int:
    value = os.environ.get(name)
    return default if value is None else int(value)


def _env_float(name: str, default: float) -> float:
    value = os.environ.get(name)
    return default if value is None else float(value)


def _application() -> AdapterApplication:
    client_config = WpsClientConfig.from_env()
    if not client_config.group_id:
        raise ValueError("WPS_GROUP_ID is required")

    storage = WpsStorage(
        WpsDriveClient(client_config),
        root_id=os.environ.get("WPS_ROOT_ID", "0"),
        root_name=os.environ.get("WPS_ROOT_NAME", "WPS Enterprise Drive"),
        list_count=_env_int("WPS_LIST_COUNT", 20),
        cache_ttl=_env_float("WPS_CACHE_TTL", 2.0),
        max_uploads=_env_int("WPS_MAX_UPLOADS", 2),
        max_downloads=_env_int("WPS_MAX_DOWNLOADS", 4),
        transfer_wait_timeout=_env_float("WPS_TRANSFER_WAIT_TIMEOUT", 30.0),
        max_copy_entries=_env_int("WPS_MAX_COPY_ENTRIES", 10000),
        max_copy_depth=_env_int("WPS_MAX_COPY_DEPTH", 64),
    )
    return AdapterApplication(
        storage,
        auth=BasicAuth(
            username=os.environ.get("ADAPTER_USERNAME", ""),
            password=os.environ.get("ADAPTER_PASSWORD", ""),
            username_file=os.environ.get("ADAPTER_USERNAME_FILE") or None,
            password_file=os.environ.get("ADAPTER_PASSWORD_FILE") or None,
        ),
        dav_prefix=os.environ.get("ADAPTER_DAV_PREFIX", "/dav"),
        rest_prefix=os.environ.get("ADAPTER_REST_PREFIX", "/api/v1"),
        max_propfind_entries=_env_int("WPS_MAX_PROPFIND_ENTRIES", 10000),
        max_propfind_depth=_env_int("WPS_MAX_PROPFIND_DEPTH", 64),
    )


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="WPS enterprise cloud drive adapter (WebDAV + REST)"
    )
    parser.add_argument("--version", action="version", version=__version__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    serve = subparsers.add_parser("serve", help="start the WebDAV/REST server")
    serve.add_argument("--bind", default=os.environ.get("ADAPTER_BIND", "127.0.0.1"))
    serve.add_argument("--port", type=int, default=_env_int("ADAPTER_PORT", 54321))
    subparsers.add_parser("check-config", help="validate configuration without network calls")
    login = subparsers.add_parser(
        "login",
        help="open an isolated local WPS login window and sync its session",
    )
    login.add_argument("--login-url", default=DEFAULT_LOGIN_URL)
    login.add_argument("--browser", default=None, help="local Chrome/Chromium executable")
    login.add_argument("--domain-suffix", default=DEFAULT_COOKIE_DOMAIN_SUFFIX)
    login.add_argument("--wait-timeout", type=float, default=300.0)
    login.add_argument(
        "--adapter-url",
        default=os.environ.get("WPS_ADAPTER_URL", ""),
        help="HTTPS adapter origin for direct credential sync",
    )
    login.add_argument(
        "--adapter-user",
        default=os.environ.get("WPS_ADAPTER_USER", ""),
        help="adapter Basic Auth username; the password is prompted securely",
    )
    login.add_argument(
        "--adapter-port",
        type=int,
        default=None,
        help="HTTPS adapter port; use with --adapter-url when it has no port",
    )
    login.add_argument("--adapter-timeout", type=float, default=30.0)
    login.add_argument(
        "--ssh-target",
        default=os.environ.get("WPS_ADAPTER_SSH_TARGET", ""),
        help="remote SSH target, for example root@203.0.113.10",
    )
    login.add_argument("--ssh-identity", default=None)
    login.add_argument("--ssh-port", type=int, default=_env_int("WPS_ADAPTER_SSH_PORT", 22))
    login.add_argument(
        "--ssh-password-auth",
        action="store_true",
        help="force password/keyboard-interactive SSH authentication",
    )
    login.add_argument("--ssh-cookie-path", default=DEFAULT_REMOTE_COOKIE_PATH)
    login.add_argument("--ssh-csrf-path", default=DEFAULT_REMOTE_CSRF_PATH)
    login.add_argument("--ssh-timeout", type=float, default=30.0)
    login.add_argument(
        "--output-dir",
        default=None,
        help="write wps-cookie and wps-csrf to this local absolute directory",
    )
    return parser


@dataclass(frozen=True, slots=True)
class _LoginTarget:
    adapter_url: str = ""
    adapter_port: int | None = None
    adapter_user: str = ""
    ssh_target: str = ""
    ssh_identity: str | None = None
    ssh_port: int = 22
    ssh_password_auth: bool = False


def _prompt_port(label: str, default: int) -> int:
    while True:
        value = input(f"{label} [{default}]: ").strip() or str(default)
        try:
            port = int(value)
        except ValueError:
            print("端口必须是数字，请重新输入。")
            continue
        if 1 <= port <= 65535:
            return port
        print("端口必须在 1 到 65535 之间，请重新输入。")


def _prompt_login_target() -> _LoginTarget:
    print("提示：[] 里面的是默认选项，直接按回车即可使用。")
    host = input("VPS 地址/IP或域名: ").strip()
    if not host or any(char.isspace() for char in host):
        raise LoginError("VPS 地址不能为空且不能包含空格")
    print("选择连接方式：")
    print("  1) SSH 私钥")
    print("  2) SSH 密码")
    print("  3) HTTPS 适配器接口")
    while True:
        choice = input("连接方式 [1]: ").strip() or "1"
        if choice in {"1", "2", "3"}:
            break
        print("请输入 1、2 或 3。")

    if choice in {"1", "2"}:
        user = input("SSH 用户名 [root]: ").strip() or "root"
        if not user or any(char.isspace() or char in "@/\\" for char in user):
            raise LoginError("SSH 用户名格式不正确")
        port = _prompt_port("SSH 端口", 22)
        if choice == "2":
            print("WPS 登录完成后，系统 ssh 会在传输凭据时询问 SSH 密码。")
            return _LoginTarget(
                ssh_target=f"{user}@{host}",
                ssh_port=port,
                ssh_password_auth=True,
            )
        identity = input("SSH 私钥路径 [~/.ssh/id_ed25519]: ").strip()
        identity = identity or "~/.ssh/id_ed25519"
        return _LoginTarget(
            ssh_target=f"{user}@{host}",
            ssh_identity=os.path.expanduser(identity),
            ssh_port=port,
        )

    host_for_url = host
    if ":" in host and not host.startswith("["):
        host_for_url = f"[{host}]"
    adapter_port = _prompt_port("适配器端口", 54321)
    default_url = f"https://{host_for_url}:{adapter_port}"
    entered_url = input(f"适配器 HTTPS 地址 [{default_url}]: ").strip()
    adapter_url = entered_url or default_url
    try:
        explicit_port = urlsplit(adapter_url).port
    except ValueError:
        explicit_port = None
    if entered_url and explicit_port is not None and explicit_port != adapter_port:
        raise LoginError("适配器地址中的端口与端口输入不一致")
    return _LoginTarget(
        adapter_url=adapter_url,
        adapter_port=adapter_port,
    )


def _apply_adapter_port(adapter_url: str, port: int | None) -> str:
    if port is None:
        return adapter_url
    if not 1 <= port <= 65535:
        raise LoginError("适配器端口必须在 1 到 65535 之间")
    parts = urlsplit(adapter_url)
    try:
        existing_port = parts.port
    except ValueError as exc:
        raise LoginError("适配器地址中的端口无效") from exc
    if existing_port is not None and existing_port != port:
        raise LoginError("适配器地址中的端口与 --adapter-port 不一致")
    if existing_port is not None:
        return adapter_url
    if not parts.hostname:
        raise LoginError("适配器地址缺少主机名")
    hostname = parts.hostname
    netloc = f"[{hostname}]" if ":" in hostname else hostname
    return urlunsplit((parts.scheme, f"{netloc}:{port}", parts.path, parts.query, parts.fragment))


def _check_public_bind(bind: str, auth: BasicAuth) -> None:
    local_names = {"127.0.0.1", "localhost", "::1"}
    if bind not in local_names and not auth.enabled:
        raise ValueError(
            "refusing a non-local bind without ADAPTER_USERNAME/PASSWORD or secret files"
        )


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    if args.command == "login":
        try:
            interactive_target = (
                _prompt_login_target()
                if not args.adapter_url and not args.ssh_target and args.output_dir is None
                else _LoginTarget(
                    adapter_url=args.adapter_url,
                    adapter_port=args.adapter_port,
                    adapter_user=args.adapter_user,
                    ssh_target=args.ssh_target,
                    ssh_identity=args.ssh_identity,
                    ssh_port=args.ssh_port,
                    ssh_password_auth=args.ssh_password_auth,
                )
            )
            adapter_url = (
                _apply_adapter_port(
                    interactive_target.adapter_url,
                    interactive_target.adapter_port,
                )
                if interactive_target.adapter_url
                else ""
            )
            adapter_user = interactive_target.adapter_user
            adapter_password: str | None = None
            if adapter_url:
                if not adapter_user:
                    adapter_user = input("适配器用户名: ").strip()
                adapter_password = getpass.getpass("适配器密码（不会显示）: ")
            login_and_sync(
                login_url=args.login_url,
                browser=args.browser,
                domain_suffix=args.domain_suffix,
                wait_timeout=args.wait_timeout,
                ssh_target=interactive_target.ssh_target,
                ssh_cookie_path=args.ssh_cookie_path,
                ssh_csrf_path=args.ssh_csrf_path,
                ssh_identity=interactive_target.ssh_identity,
                ssh_port=interactive_target.ssh_port,
                ssh_password_auth=interactive_target.ssh_password_auth,
                output_dir=args.output_dir,
                ssh_timeout=args.ssh_timeout,
                adapter_url=adapter_url,
                adapter_user=adapter_user,
                adapter_password=adapter_password,
                adapter_timeout=args.adapter_timeout,
            )
            return 0
        except (EOFError, KeyboardInterrupt):
            print("login failed: 已取消登录同步", file=sys.stderr)
            return 1
        except (LoginError, OSError, ValueError) as exc:
            print(f"login failed: {exc}", file=sys.stderr)
            return 1
    try:
        application = _application()
        if args.command == "check-config":
            auth_state = "enabled" if application.auth.enabled else "disabled"
            print(
                "config=ok "
                f"group_id_configured=yes auth={auth_state} "
                f"dav={application.dav_prefix} rest={application.rest_prefix}"
            )
            return 0

        _check_public_bind(args.bind, application.auth)
        server = create_server(application, bind=args.bind, port=args.port)
        print(f"listening=http://{args.bind}:{args.port}", flush=True)
        print(
            f"webdav=http://{args.bind}:{args.port}{application.dav_prefix}/ "
            f"rest=http://{args.bind}:{args.port}{application.rest_prefix}/",
            flush=True,
        )
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            return 0
        finally:
            server.server_close()
        return 0
    except (OSError, ValueError) as exc:
        print(f"adapter failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
