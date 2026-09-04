"""Command-line interface shared by the package and standalone login helper."""

from __future__ import annotations

import argparse
import getpass
import os
import sys
from dataclasses import dataclass
from urllib.parse import urlsplit, urlunsplit

from .login import (
    DEFAULT_COOKIE_DOMAIN_SUFFIX,
    DEFAULT_LOGIN_URL,
    DEFAULT_REMOTE_COOKIE_PATH,
    DEFAULT_REMOTE_CSRF_PATH,
    DEFAULT_REMOTE_WORKSPACE_PATH,
    LoginError,
    WpsWorkspaceCandidate,
    is_remote_http_url,
    login_and_sync,
)


def _env_int(name: str, default: int) -> int:
    value = os.environ.get(name)
    return default if value is None else int(value)


def add_login_arguments(parser: argparse.ArgumentParser) -> None:
    """Add login-helper arguments to a parser."""

    parser.add_argument("--login-url", default=DEFAULT_LOGIN_URL)
    parser.add_argument(
        "--workspace-url",
        default=None,
        help="指定具体 WPS 文件夹 URL；省略时使用企业云盘根目录",
    )
    parser.add_argument("--browser", default=None, help="local Chrome/Chromium executable")
    parser.add_argument("--domain-suffix", default=DEFAULT_COOKIE_DOMAIN_SUFFIX)
    parser.add_argument("--wait-timeout", type=float, default=300.0)
    parser.add_argument(
        "--adapter-url",
        default=os.environ.get("WPS_ADAPTER_URL", ""),
        help="HTTP or HTTPS adapter origin for direct credential sync",
    )
    parser.add_argument(
        "--adapter-user",
        default=os.environ.get("WPS_ADAPTER_USER", ""),
        help="adapter Basic Auth username; the password is prompted securely",
    )
    parser.add_argument(
        "--adapter-port",
        type=int,
        default=None,
        help="adapter port; use with --adapter-url when it has no port",
    )
    parser.add_argument(
        "--allow-http",
        action="store_true",
        help="allow sending the WPS session to a remote adapter over HTTP",
    )
    parser.add_argument("--adapter-timeout", type=float, default=30.0)
    parser.add_argument(
        "--ssh-target",
        default=os.environ.get("WPS_ADAPTER_SSH_TARGET", ""),
        help="remote SSH target, for example root@203.0.113.10",
    )
    parser.add_argument("--ssh-identity", default=None)
    parser.add_argument("--ssh-port", type=int, default=_env_int("WPS_ADAPTER_SSH_PORT", 22))
    parser.add_argument(
        "--ssh-password-auth",
        action="store_true",
        help="force password/keyboard-interactive SSH authentication",
    )
    parser.add_argument("--ssh-cookie-path", default=DEFAULT_REMOTE_COOKIE_PATH)
    parser.add_argument("--ssh-csrf-path", default=DEFAULT_REMOTE_CSRF_PATH)
    parser.add_argument("--ssh-workspace-path", default=DEFAULT_REMOTE_WORKSPACE_PATH)
    parser.add_argument("--ssh-timeout", type=float, default=30.0)
    parser.add_argument(
        "--output-dir",
        default=None,
        help="write credentials and wps-workspace.json to this local absolute directory",
    )


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
    print("  3) HTTP/HTTPS 适配器接口")
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
    default_url = f"http://{host_for_url}:{adapter_port}"
    entered_url = input(f"适配器 HTTP/HTTPS 地址 [{default_url}]: ").strip()
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


def _select_workspace(candidates: tuple[WpsWorkspaceCandidate, ...]) -> WpsWorkspaceCandidate:
    """Let a normal user choose a space by its name, never by its ID."""

    if len(candidates) == 1:
        print(f"已找到 WPS 空间：{candidates[0].name}，将自动使用它。", flush=True)
        return candidates[0]
    print(f"发现 {len(candidates)} 个可用 WPS 空间：", flush=True)
    for index, candidate in enumerate(candidates, 1):
        print(f"  [{index}] {candidate.name}", flush=True)
    while True:
        answer = input("请选择空间 [1]: ").strip() or "1"
        try:
            index = int(answer)
        except ValueError:
            print("请输入列表中的序号。", flush=True)
            continue
        if 1 <= index <= len(candidates):
            return candidates[index - 1]
        print("请输入列表中的序号。", flush=True)


def _select_workspaces(candidates: tuple[WpsWorkspaceCandidate, ...]) -> tuple[WpsWorkspaceCandidate, ...]:
    """Select one, several, or all discovered spaces by display name."""

    if len(candidates) == 1:
        return candidates
    print("可输入一个或多个序号（例如 1,3），也可以输入 all 使用全部空间。", flush=True)
    while True:
        answer = input("请选择空间 [1]: ").strip() or "1"
        if answer.casefold() == "all":
            return candidates
        try:
            indexes = [int(value.strip()) for value in answer.split(",")]
        except ValueError:
            print("请输入序号，例如 1,3，或输入 all。", flush=True)
            continue
        if not indexes or len(set(indexes)) != len(indexes) or any(
            index < 1 or index > len(candidates) for index in indexes
        ):
            print("请输入列表中的序号。", flush=True)
            continue
        return tuple(candidates[index - 1] for index in indexes)


def run_login(args: argparse.Namespace, *, interactive: bool = True) -> int:
    """Run the login flow and return a process exit code."""

    interactive_target = (
        _prompt_login_target()
        if interactive and not args.adapter_url and not args.ssh_target and args.output_dir is None
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
    allow_insecure_http = args.allow_http
    if adapter_url and is_remote_http_url(adapter_url):
        print(
            "警告：HTTP 不加密，WPS Cookie、Basic Auth 和文件请求可能被窃听。",
            flush=True,
        )
        if not allow_insecure_http:
            confirmation = input("仍然通过 HTTP 发送 Cookie？ [y/N]: ").strip().casefold()
            if confirmation not in {"y", "yes"}:
                raise LoginError("已取消明文 HTTP 凭据同步；如确认风险可使用 --allow-http")
            allow_insecure_http = True
    adapter_user = interactive_target.adapter_user
    adapter_password: str | None = None
    if adapter_url:
        if not adapter_user:
            adapter_user = input("适配器用户名: ").strip()
        adapter_password = getpass.getpass("适配器密码（不会显示）: ")
    login_and_sync(
        login_url=args.login_url,
        workspace_url=args.workspace_url,
        browser=args.browser,
        domain_suffix=args.domain_suffix,
        wait_timeout=args.wait_timeout,
        ssh_target=interactive_target.ssh_target,
        ssh_cookie_path=args.ssh_cookie_path,
        ssh_csrf_path=args.ssh_csrf_path,
        ssh_workspace_path=args.ssh_workspace_path,
        ssh_identity=interactive_target.ssh_identity,
        ssh_port=interactive_target.ssh_port,
        ssh_password_auth=interactive_target.ssh_password_auth,
        output_dir=args.output_dir,
        ssh_timeout=args.ssh_timeout,
        adapter_url=adapter_url,
        adapter_user=adapter_user,
        adapter_password=adapter_password,
        adapter_timeout=args.adapter_timeout,
        allow_insecure_http=allow_insecure_http,
        workspace_selector=_select_workspaces if interactive and not args.workspace_url else None,
    )
    return 0


def run_login_safely(args: argparse.Namespace, *, interactive: bool = True) -> int:
    """Run the login flow with user-facing error handling."""

    try:
        return run_login(args, interactive=interactive)
    except (EOFError, KeyboardInterrupt):
        print("login failed: 已取消登录同步", file=sys.stderr)
        return 1
    except (LoginError, OSError, ValueError) as exc:
        print(f"login failed: {exc}", file=sys.stderr)
        return 1


__all__ = [
    "_apply_adapter_port",
    "_prompt_login_target",
    "add_login_arguments",
    "run_login",
    "run_login_safely",
]
