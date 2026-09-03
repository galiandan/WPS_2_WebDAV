from __future__ import annotations

import argparse
import os
import sys

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
        "--ssh-target",
        default=os.environ.get("WPS_ADAPTER_SSH_TARGET", ""),
        help="remote SSH target, for example root@203.0.113.10",
    )
    login.add_argument("--ssh-identity", default=None)
    login.add_argument("--ssh-cookie-path", default=DEFAULT_REMOTE_COOKIE_PATH)
    login.add_argument("--ssh-csrf-path", default=DEFAULT_REMOTE_CSRF_PATH)
    login.add_argument("--ssh-timeout", type=float, default=30.0)
    login.add_argument(
        "--output-dir",
        default=None,
        help="write wps-cookie and wps-csrf to this local absolute directory",
    )
    return parser


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
            login_and_sync(
                login_url=args.login_url,
                browser=args.browser,
                domain_suffix=args.domain_suffix,
                wait_timeout=args.wait_timeout,
                ssh_target=args.ssh_target,
                ssh_cookie_path=args.ssh_cookie_path,
                ssh_csrf_path=args.ssh_csrf_path,
                ssh_identity=args.ssh_identity,
                output_dir=args.output_dir,
                ssh_timeout=args.ssh_timeout,
            )
            return 0
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
