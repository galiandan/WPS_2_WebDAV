"""Small same-origin browser file manager for the adapter REST API."""

from __future__ import annotations

import html
import json
import os
import re
from pathlib import Path


_DEFAULT_ROOT_NAME = "WPS Enterprise Drive"
_ROOT_NAME_HTML_TOKEN = "__WPS_ROOT_NAME_HTML__"
_ROOT_NAME_JSON_TOKEN = "__WPS_ROOT_NAME_JSON__"
_ROOT_NAME_TOKEN_PATTERN = re.compile(
    rf"{re.escape(_ROOT_NAME_HTML_TOKEN)}|{re.escape(_ROOT_NAME_JSON_TOKEN)}"
)

WEB_ASSETS_DIR_ENV = "WPS_ADAPTER_WEB_ASSETS_DIR"
_DEFAULT_WEB_ASSETS_DIR = Path(__file__).resolve().parents[2] / "go" / "web"
_WEB_ASSET_CONTENT_TYPES = {
    "app.js": "text/javascript; charset=utf-8",
    "style.css": "text/css; charset=utf-8",
}


def web_assets_dir() -> Path:
    """Locate the split web assets: env override, else the repo's go/web."""

    override = os.environ.get(WEB_ASSETS_DIR_ENV)
    if override:
        return Path(override)
    return _DEFAULT_WEB_ASSETS_DIR


def web_asset_content_type(name: str) -> str | None:
    return _WEB_ASSET_CONTENT_TYPES.get(name)


def load_web_asset(name: str) -> bytes:
    """Read one whitelisted web asset; the name never reaches a path join."""

    if name not in _WEB_ASSET_CONTENT_TYPES:
        raise KeyError(name)
    return (web_assets_dir() / name).read_bytes()


def render_web_asset(name: str, root_name: str) -> bytes:
    """Serve one whitelisted asset, applying the temporary root-name token.

    Only the extracted app.js still carries ``__WPS_ROOT_NAME_JSON__``; the
    fixed index.html planned for the next stage removes this substitution.
    """

    body = load_web_asset(name).decode("utf-8")
    if name == "app.js":
        body = body.replace(_ROOT_NAME_JSON_TOKEN, _safe_root_name_json(root_name))
    return body.encode("utf-8")


WEB_APP_TEMPLATE = r"""<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>__WPS_ROOT_NAME_HTML__</title>
  <link rel="stylesheet" href="/assets/style.css">
</head>
<body>
  <div id="drop-overlay" class="drop-overlay" aria-hidden="true">
    <div class="drop-overlay-card" role="status" aria-live="polite">
      <span class="drop-overlay-icon" aria-hidden="true">↑</span>
      <strong>松开即可上传</strong>
      <span>将保存到当前目录：<b id="drop-target">/</b></span>
    </div>
  </div>
  <header class="app-header">
    <div class="header-inner">
      <div class="brand">
        <span class="brand-mark" aria-hidden="true">W</span>
        <div class="brand-copy">
          <div class="brand-title">__WPS_ROOT_NAME_HTML__</div>
          <div class="brand-subtitle">WebDAV Adapter</div>
        </div>
      </div>
      <div class="header-actions">
        <button id="settings-button" class="icon-button" type="button" title="设置云盘名称" aria-label="设置云盘名称">⚙</button>
        <button id="up-button" class="icon-button" type="button" title="返回上一级" aria-label="返回上一级">←</button>
        <button id="refresh-button" class="icon-button" type="button" title="刷新目录" aria-label="刷新目录">↻</button>
        <button id="folder-button" type="button"><span class="button-icon" aria-hidden="true">＋</span>新建文件夹</button>
        <button id="upload-button" class="primary" type="button"><span class="button-icon" aria-hidden="true">↑</span>上传文件</button>
        <input id="file-input" class="hidden" type="file" multiple>
      </div>
    </div>
  </header>

  <main class="content">
    <section class="workspace-head" aria-labelledby="folder-title">
      <div>
        <div class="eyebrow">企业空间</div>
        <h1 id="folder-title">__WPS_ROOT_NAME_HTML__</h1>
        <p id="folder-note" class="workspace-note">管理 __WPS_ROOT_NAME_HTML__ 中的文件和文件夹</p>
      </div>
      <div id="connection" class="connection checking" role="status" aria-live="polite">
        <span class="connection-dot" aria-hidden="true"></span><span id="connection-label">正在检查 WPS</span>
      </div>
    </section>

    <div class="navigation-row">
      <nav id="breadcrumbs" class="breadcrumbs" aria-label="当前位置"></nav>
      <label class="search" for="search-input">
        <span class="search-icon" aria-hidden="true">⌕</span>
        <input id="search-input" type="search" placeholder="搜索当前目录" autocomplete="off">
      </label>
    </div>

    <section class="panel" aria-label="文件列表">
      <div class="panel-toolbar">
        <div id="panel-summary" class="panel-summary">正在读取...</div>
        <div id="path-value" class="meta">/</div>
      </div>
      <div id="drop-zone" class="drop-zone">
        <span class="drop-icon" aria-hidden="true">↑</span>
        <span><strong>拖动文件到页面任意位置</strong>，或点击选择文件</span>
        <button id="drop-upload-button" type="button">选择文件</button>
      </div>
      <div class="table-wrap">
        <table>
          <thead>
            <tr><th>名称</th><th>类型</th><th>大小</th><th>修改时间</th><th>操作</th></tr>
          </thead>
          <tbody id="entries"></tbody>
        </table>
        <div id="empty" class="empty hidden"></div>
      </div>
    </section>

    <div class="status-row">
      <span id="status" role="status" aria-live="polite">正在读取...</span>
      <span id="upload-speed" class="upload-speed hidden" aria-live="off"></span>
      <progress id="progress" class="hidden" max="100" value="0"></progress>
    </div>
  </main>

  <dialog id="modal">
    <form id="modal-form" class="modal-card" method="dialog">
      <h2 id="modal-title" class="modal-title"></h2>
      <p id="modal-message" class="modal-message hidden"></p>
      <label id="modal-label" class="modal-label">
        <span id="modal-label-text"></span>
        <input id="modal-input" class="modal-input" type="text" autocomplete="off">
      </label>
      <div class="modal-actions">
        <button id="modal-cancel" type="button">取消</button>
        <button id="modal-submit" class="modal-submit primary" type="submit">确定</button>
      </div>
    </form>
  </dialog>

  <script src="/assets/app.js" defer></script>
</body>
</html>
"""


def _safe_root_name_json(root_name: str) -> str:
    """Encode a configured name for an inline script without ending it."""

    return (
        json.dumps(root_name, ensure_ascii=False)
        .replace("<", "\\u003c")
        .replace(">", "\\u003e")
        .replace("&", "\\u0026")
        .replace("\u2028", "\\u2028")
        .replace("\u2029", "\\u2029")
    )


def render_web_app(root_name: str = _DEFAULT_ROOT_NAME) -> str:
    """Render the same-origin file manager with the configured root name."""

    if not isinstance(root_name, str):
        raise TypeError("root_name must be a string")
    display_name = root_name or _DEFAULT_ROOT_NAME
    replacements = {
        _ROOT_NAME_HTML_TOKEN: html.escape(display_name, quote=True),
        _ROOT_NAME_JSON_TOKEN: _safe_root_name_json(display_name),
    }
    return _ROOT_NAME_TOKEN_PATTERN.sub(
        lambda match: replacements[match.group(0)],
        WEB_APP_TEMPLATE,
    )


WEB_APP_HTML = render_web_app()


__all__ = ["WEB_APP_HTML", "WEB_APP_TEMPLATE", "render_web_app"]
