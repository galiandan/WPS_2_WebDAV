from __future__ import annotations

import argparse
import os
import sys

from . import __version__
from .client import WpsClientConfig, WpsDriveClient
from .login_command import (
    _apply_adapter_port,
    _prompt_login_target,
    add_login_arguments,
    run_login_safely,
)
from .server import AdapterApplication, BasicAuth, DavLockStore, create_server
from .settings import DEFAULT_ROOT_NAME, WebSettings
from .storage import WpsStorage


def _env_int(name: str, default: int) -> int:
    value = os.environ.get(name)
    return default if value is None else int(value)


def _env_float(name: str, default: float) -> float:
    value = os.environ.get(name)
    return default if value is None else float(value)


def _application() -> AdapterApplication:
    client_config = WpsClientConfig.from_env()
    workspace = client_config.workspace
    root_name = os.environ.get("WPS_ROOT_NAME") or DEFAULT_ROOT_NAME
    web_settings = WebSettings(fallback_name=root_name)
    root_name = web_settings.name
    root_id = (
        workspace.root_id
        if workspace is not None
        else os.environ.get("WPS_ROOT_ID", "0")
    )

    storage = WpsStorage(
        WpsDriveClient(client_config),
        root_id=root_id,
        root_name=root_name,
        list_count=_env_int("WPS_LIST_COUNT", 20),
        max_list_entries=_env_int("WPS_MAX_LIST_ENTRIES", 10000),
        cache_ttl=_env_float("WPS_CACHE_TTL", 2.0),
        max_uploads=_env_int("WPS_MAX_UPLOADS", 2),
        max_downloads=_env_int("WPS_MAX_DOWNLOADS", 4),
        transfer_wait_timeout=_env_float("WPS_TRANSFER_WAIT_TIMEOUT", 30.0),
        max_copy_entries=_env_int("WPS_MAX_COPY_ENTRIES", 10000),
        max_copy_depth=_env_int("WPS_MAX_COPY_DEPTH", 64),
    )
    return AdapterApplication(
        storage,
        web_root_name=root_name,
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
        max_control_body=_env_int("WPS_MAX_CONTROL_BODY", 1024 * 1024),
        max_response_body=_env_int("WPS_MAX_RESPONSE_BODY_BYTES", 16 * 1024 * 1024),
        locks=DavLockStore(max_locks=_env_int("WPS_MAX_LOCKS", 4096)),
        web_settings=web_settings,
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
    add_login_arguments(login)
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
        return run_login_safely(args)
    try:
        application = _application()
        if args.command == "check-config":
            auth_state = "enabled" if application.auth.enabled else "disabled"
            workspace = application.storage.client.config.workspace
            group_id = (
                workspace.group_id
                if workspace is not None
                else (
                    ""
                    if application.storage.client.config.group_id in {"", "auto"}
                    else application.storage.client.config.group_id
                )
            )
            group_state = (
                "ready"
                if group_id
                else "pending-login"
            )
            print(
                "config=ok "
                f"group_id={group_state} auth={auth_state} "
                f"dav={application.dav_prefix} rest={application.rest_prefix}"
            )
            return 0

        _check_public_bind(args.bind, application.auth)
        server = create_server(
            application,
            bind=args.bind,
            port=args.port,
            max_connections=_env_int("ADAPTER_MAX_CONNECTIONS", 64),
            request_timeout=_env_float("ADAPTER_REQUEST_TIMEOUT", 60.0),
        )
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
