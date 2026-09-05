"""Small same-origin browser file manager for the adapter REST API."""

from __future__ import annotations

import html
import json
import re


_DEFAULT_ROOT_NAME = "WPS Enterprise Drive"
_ROOT_NAME_HTML_TOKEN = "__WPS_ROOT_NAME_HTML__"
_ROOT_NAME_JSON_TOKEN = "__WPS_ROOT_NAME_JSON__"
_ROOT_NAME_TOKEN_PATTERN = re.compile(
    rf"{re.escape(_ROOT_NAME_HTML_TOKEN)}|{re.escape(_ROOT_NAME_JSON_TOKEN)}"
)


WEB_APP_TEMPLATE = r"""<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>__WPS_ROOT_NAME_HTML__</title>
  <style>
    :root {
      color-scheme: light;
      --ink: #192532;
      --muted: #6c7884;
      --quiet: #97a3ad;
      --line: #dfe5ea;
      --line-soft: #edf1f4;
      --page: #f4f6f8;
      --panel: #ffffff;
      --panel-soft: #f8fafb;
      --blue: #1769aa;
      --blue-dark: #0f558d;
      --blue-soft: #e9f3fb;
      --amber: #a96800;
      --amber-soft: #fff3d9;
      --green: #18794e;
      --green-soft: #e7f6ee;
      --red: #b42318;
      --red-soft: #fff0ee;
      --shadow: 0 10px 30px rgba(26, 41, 54, .06);
    }

    * { box-sizing: border-box; }
    html { min-width: 320px; }
    body {
      margin: 0;
      background: var(--page);
      color: var(--ink);
      font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", "Microsoft YaHei", sans-serif;
    }
    button, input { font: inherit; }
    button, .button-link {
      align-items: center;
      border: 1px solid var(--line);
      border-radius: 7px;
      background: var(--panel);
      color: var(--ink);
      cursor: pointer;
      display: inline-flex;
      gap: 7px;
      justify-content: center;
      min-height: 36px;
      padding: 7px 12px;
      text-decoration: none;
      transition: border-color .16s ease, background .16s ease, color .16s ease, transform .16s ease;
    }
    button:hover, .button-link:hover { border-color: var(--blue); color: var(--blue); }
    button:active, .button-link:active { transform: translateY(1px); }
    button:disabled { cursor: default; opacity: .45; transform: none; }
    button:focus-visible, .button-link:focus-visible, input:focus-visible {
      outline: 3px solid #b9dbf6;
      outline-offset: 1px;
    }
    .hidden { display: none !important; }

    .app-header {
      background: var(--panel);
      border-bottom: 1px solid var(--line);
      position: sticky;
      top: 0;
      z-index: 10;
    }
    .header-inner, .content {
      margin: 0 auto;
      max-width: 1220px;
    }
    .header-inner {
      align-items: center;
      display: flex;
      gap: 22px;
      justify-content: space-between;
      min-height: 70px;
      padding: 12px 24px;
    }
    .brand { align-items: center; display: flex; gap: 11px; min-width: 0; }
    .brand-mark {
      align-items: center;
      background: var(--blue);
      border-radius: 10px;
      color: #fff;
      display: inline-flex;
      flex: 0 0 38px;
      font-size: 17px;
      font-weight: 750;
      height: 38px;
      justify-content: center;
      letter-spacing: 0;
    }
    .brand-copy { min-width: 0; }
    .brand-title { font-size: 16px; font-weight: 700; line-height: 1.2; }
    .brand-subtitle { color: var(--muted); font-size: 11px; margin-top: 2px; }
    .header-actions { align-items: center; display: flex; flex-wrap: wrap; gap: 8px; justify-content: flex-end; }
    .primary { background: var(--blue); border-color: var(--blue); color: #fff; }
    .primary:hover { background: var(--blue-dark); border-color: var(--blue-dark); color: #fff; }
    .icon-button { font-size: 20px; line-height: 1; padding: 5px 10px; width: 40px; }
    .button-icon { font-size: 16px; line-height: 1; }

    .content { padding: 30px 24px 48px; }
    .workspace-head { align-items: flex-end; display: flex; gap: 20px; justify-content: space-between; margin-bottom: 22px; }
    .eyebrow { color: var(--blue); font-size: 12px; font-weight: 700; letter-spacing: .04em; margin-bottom: 6px; }
    h1 { font-size: 26px; letter-spacing: 0; line-height: 1.2; margin: 0; }
    .workspace-note { color: var(--muted); margin: 8px 0 0; }
    .connection { align-items: center; background: var(--amber-soft); border-radius: 999px; color: var(--amber); display: inline-flex; flex: 0 0 auto; font-size: 12px; gap: 7px; padding: 7px 11px; white-space: nowrap; }
    .connection.connected { background: var(--green-soft); color: var(--green); }
    .connection.disconnected { background: var(--red-soft); color: var(--red); }
    .connection.unknown { background: var(--panel-soft); color: var(--muted); }
    .connection-dot { background: var(--amber); border-radius: 50%; height: 7px; width: 7px; }
    .connection.connected .connection-dot { background: var(--green); }
    .connection.disconnected .connection-dot { background: var(--red); }
    .connection.unknown .connection-dot { background: var(--quiet); }

    .navigation-row { align-items: center; display: flex; gap: 16px; justify-content: space-between; margin-bottom: 12px; }
    .breadcrumbs { align-items: center; display: flex; flex-wrap: wrap; gap: 3px; min-width: 0; }
    .crumb { background: transparent; border-color: transparent; color: var(--blue); min-height: 30px; padding: 4px 7px; }
    .crumb:hover { background: var(--blue-soft); border-color: transparent; }
    .crumb.current { color: var(--ink); cursor: default; font-weight: 700; }
    .crumb.current:hover { background: transparent; color: var(--ink); }
    .crumb-separator { color: var(--quiet); font-size: 16px; }
    .search { align-items: center; background: var(--panel); border: 1px solid var(--line); border-radius: 7px; display: flex; flex: 0 1 260px; min-height: 36px; padding: 0 10px; }
    .search-icon { color: var(--quiet); font-size: 18px; line-height: 1; }
    .search input { border: 0; color: var(--ink); min-width: 0; outline: 0; padding: 7px 8px; width: 100%; }
    .search input::placeholder { color: var(--quiet); }

    .panel { background: var(--panel); border: 1px solid var(--line); border-radius: 9px; box-shadow: var(--shadow); overflow: hidden; }
    .panel-toolbar { align-items: center; background: var(--panel-soft); border-bottom: 1px solid var(--line-soft); display: flex; gap: 12px; justify-content: space-between; min-height: 51px; padding: 8px 14px; }
    .panel-summary { color: var(--muted); font-size: 13px; }
    .panel-summary strong { color: var(--ink); font-weight: 700; }
    .drop-zone { align-items: center; border-bottom: 1px dashed var(--line); color: var(--muted); display: flex; gap: 9px; justify-content: center; min-height: 48px; padding: 8px 14px; transition: background .16s ease, border-color .16s ease; }
    .drop-zone strong { color: var(--ink); font-weight: 600; }
    .drop-zone .drop-icon { color: var(--blue); font-size: 18px; }
    .drop-zone button { background: transparent; border: 0; color: var(--blue); min-height: 28px; padding: 3px 5px; }
    .drop-zone button:hover { background: var(--blue-soft); border: 0; }
    .table-wrap { overflow-x: auto; }
    table { border-collapse: collapse; min-width: 760px; width: 100%; }
    th, td { border-bottom: 1px solid var(--line-soft); padding: 13px 16px; text-align: left; vertical-align: middle; }
    th { color: var(--muted); font-size: 12px; font-weight: 700; white-space: nowrap; }
    th:first-child { width: 46%; }
    th:last-child, td:last-child { text-align: right; }
    tr:last-child td { border-bottom: 0; }
    tbody tr { transition: background .16s ease; }
    tbody tr:hover { background: #fbfcfd; }
    .name-cell { align-items: center; display: flex; gap: 11px; min-width: 250px; }
    .entry-glyph { align-items: center; border-radius: 8px; display: inline-flex; flex: 0 0 34px; font-size: 15px; font-weight: 700; height: 34px; justify-content: center; }
    .entry-glyph.folder { background: var(--amber-soft); color: var(--amber); }
    .entry-glyph.file { background: var(--blue-soft); color: var(--blue); }
    .entry-glyph.folder::before { content: "▰"; }
    .entry-glyph.file::before { content: "□"; }
    .entry-name { background: transparent; border: 0; display: block; font-weight: 600; max-width: 460px; min-height: 30px; overflow: hidden; padding: 4px 0; text-align: left; text-overflow: ellipsis; white-space: nowrap; }
    .entry-name.folder { color: var(--blue); }
    .entry-name:hover { border: 0; color: var(--blue-dark); }
    .meta { color: var(--muted); white-space: nowrap; }
    .actions { display: flex; flex-wrap: wrap; gap: 5px; justify-content: flex-end; }
    .action-button { background: transparent; border-color: transparent; color: var(--muted); min-height: 30px; padding: 4px 7px; }
    .action-button:hover { background: var(--blue-soft); border-color: transparent; color: var(--blue); }
    .action-button.danger:hover { background: var(--red-soft); color: var(--red); }
    .action-icon { font-size: 16px; line-height: 1; }
    .empty { color: var(--muted); padding: 64px 20px; text-align: center; }
    .empty strong { color: var(--ink); display: block; font-size: 15px; margin-bottom: 4px; }
    .status-row { align-items: center; color: var(--muted); display: flex; flex-wrap: wrap; gap: 10px; min-height: 42px; }
    .status-row.error { color: var(--red); }
    .status-row.success { color: var(--green); }
    .status-row progress { height: 7px; max-width: 180px; width: 25vw; }
    .upload-speed { color: var(--ink); font-size: 12px; font-variant-numeric: tabular-nums; margin-left: auto; }

    .drop-overlay { align-items: center; background: rgba(23, 105, 170, .14); border: 3px dashed rgba(23, 105, 170, .62); display: flex; inset: 0; justify-content: center; opacity: 0; pointer-events: none; position: fixed; transition: opacity .16s ease, visibility .16s ease; visibility: hidden; z-index: 50; }
    .drop-overlay.active { opacity: 1; visibility: visible; }
    .drop-overlay-card { align-items: center; background: var(--panel); border: 1px solid var(--line); border-radius: 10px; box-shadow: 0 20px 60px rgba(16, 28, 38, .18); display: flex; flex-direction: column; gap: 5px; max-width: calc(100vw - 36px); padding: 24px 30px; text-align: center; }
    .drop-overlay-icon { align-items: center; background: var(--blue-soft); border-radius: 50%; color: var(--blue); display: inline-flex; font-size: 28px; height: 54px; justify-content: center; margin-bottom: 6px; width: 54px; }
    .drop-overlay-card strong { font-size: 17px; }
    .drop-overlay-card span:last-child { color: var(--muted); max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .drop-overlay-card b { color: var(--ink); font-weight: 700; }

    dialog { background: transparent; border: 0; max-width: min(430px, calc(100vw - 28px)); padding: 0; width: 100%; }
    dialog::backdrop { background: rgba(16, 28, 38, .42); }
    .modal-card { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; box-shadow: 0 24px 70px rgba(16, 28, 38, .2); padding: 22px; }
    .modal-title { font-size: 18px; font-weight: 700; margin: 0; }
    .modal-message { color: var(--muted); margin: 9px 0 0; }
    .modal-label { color: var(--muted); display: block; font-size: 13px; font-weight: 600; margin-top: 18px; }
    .modal-input { border: 1px solid var(--line); border-radius: 7px; color: var(--ink); margin-top: 7px; min-height: 40px; outline: 0; padding: 8px 10px; width: 100%; }
    .modal-input:focus { border-color: var(--blue); box-shadow: 0 0 0 3px #e2f0fb; }
    .modal-actions { display: flex; gap: 8px; justify-content: flex-end; margin-top: 22px; }
    .modal-submit.danger { background: var(--red); border-color: var(--red); color: #fff; }
    .modal-submit.danger:hover { background: #951b13; border-color: #951b13; color: #fff; }

    @media (max-width: 720px) {
      .header-inner, .content { padding-left: 14px; padding-right: 14px; }
      .header-inner { align-items: flex-start; flex-direction: column; gap: 12px; padding-bottom: 14px; padding-top: 14px; }
      .header-actions { justify-content: flex-start; width: 100%; }
      .content { padding-bottom: 32px; padding-top: 22px; }
      .workspace-head { align-items: flex-start; flex-direction: column; gap: 12px; margin-bottom: 14px; }
      h1 { font-size: 23px; }
      .navigation-row { align-items: stretch; flex-direction: column; gap: 8px; }
      .search { flex-basis: auto; max-width: none; width: 100%; }
      .panel-toolbar { align-items: flex-start; flex-direction: column; justify-content: center; }
      .drop-zone { align-items: flex-start; flex-wrap: wrap; gap: 5px 8px; justify-content: flex-start; }
      .status-row progress { width: 34vw; }
    }
  </style>
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

  <script>
    (() => {
      "use strict";
      const state = { path: "/", entries: [], busy: false, search: "", connection: "checking" };
      const $ = (id) => document.getElementById(id);
      const apiRoot = "/api/v1/";
      let rootName = __WPS_ROOT_NAME_JSON__;
      let modalResolve = null;
      let modalMode = "input";

      function apiUrl(route) {
        return new URL(apiRoot + route, window.location.origin);
      }

      function pathUrl(route, path) {
        const url = apiUrl(route);
        url.searchParams.set("path", path);
        return url;
      }

      async function responseData(response) {
        const text = await response.text();
        let data = null;
        if (text) {
          try { data = JSON.parse(text); } catch (_) { data = null; }
        }
        if (!response.ok) {
          const message = data && data.error ? data.error : `请求失败（${response.status}）`;
          const error = new Error(message);
          error.status = response.status;
          if (data && typeof data.code === "string") error.code = data.code;
          throw error;
        }
        return data;
      }

      async function api(route, path, options = {}) {
        const response = await fetch(pathUrl(route, path), {
          cache: "no-store",
          credentials: "same-origin",
          ...options,
        });
        return responseData(response);
      }

      async function apiRequest(route, options = {}) {
        const response = await fetch(apiUrl(route), {
          cache: "no-store",
          credentials: "same-origin",
          ...options,
        });
        return responseData(response);
      }

      function setStatus(message, kind = "") {
        const status = $("status");
        status.textContent = message;
        status.parentElement.className = "status-row" + (kind ? " " + kind : "");
      }

      function updateControls() {
        const unavailable = state.connection !== "connected";
        $("settings-button").disabled = state.busy;
        $("up-button").disabled = state.busy || unavailable || state.path === "/";
        $("refresh-button").disabled = state.busy;
        [$("folder-button"), $("upload-button"), $("drop-upload-button")].forEach((button) => {
          button.disabled = state.busy || unavailable;
        });
        document.querySelectorAll(".action-button").forEach((button) => {
          button.disabled = state.busy || unavailable;
        });
      }

      function setBusy(value) {
        state.busy = value;
        updateControls();
        document.body.classList.toggle("is-busy", value);
        if (value) hideDropOverlay();
      }

      function setConnection(value) {
        const known = new Set([
          "checking", "connected", "not_configured", "session_expired",
          "permission_denied", "upstream_unavailable", "invalid_response", "unknown",
        ]);
        state.connection = known.has(value) ? value : "unknown";
        const badge = $("connection");
        const labels = {
          checking: "正在检查 WPS",
          connected: "WPS 已连接",
          not_configured: "WPS 尚未连接",
          session_expired: "WPS 登录已过期",
          permission_denied: "无权访问当前工作区",
          upstream_unavailable: "WPS 暂时不可用",
          invalid_response: "WPS 响应异常",
          unknown: "WPS 状态未知",
        };
        const visualClass = state.connection === "connected"
          ? "connected"
          : ["not_configured", "session_expired", "permission_denied"].includes(state.connection)
            ? "disconnected"
            : "unknown";
        badge.className = "connection " + visualClass;
        $("connection-label").textContent = labels[state.connection] || labels.unknown;
        updateControls();
      }

      function connectionMessage(value) {
        const messages = {
          not_configured: "WPS 尚未连接，请先在自己的电脑运行 wps_login.py 同步凭据，然后点击刷新",
          session_expired: "WPS 登录已过期，请重新运行 wps_login.py 同步凭据，然后点击刷新",
          permission_denied: "无权访问当前工作区，请检查登录账号或重新选择工作区",
          upstream_unavailable: "WPS 暂时不可用，请稍后点击刷新重试",
          invalid_response: "WPS 返回了无法识别的响应，请稍后点击刷新重试",
          unknown: "暂时无法判断 WPS 状态，请点击刷新重试",
        };
        return messages[value] || messages.unknown;
      }

      function isWpsError(error) {
        return Boolean(
          error && (
            error.code === "wps_unavailable" ||
            error.code === "wps_session_expired" ||
            error.message === "upstream WPS request failed" ||
            error.message === "WPS session expired; refresh the configured credentials"
          )
        );
      }

      function showError(error) {
        if (isWpsError(error)) {
          const connection = error.code === "wps_session_expired"
            ? "session_expired"
            : "upstream_unavailable";
          setConnection(connection);
          setStatus(connectionMessage(connection), "error");
          return;
        }
        if (state.connection === "checking") setConnection("unknown");
        setStatus(error.message, "error");
      }

      function canonicalPath(path) {
        if (!path || path === "/") return "/";
        return "/" + path.split("/").filter(Boolean).join("/");
      }

      function joinPath(parent, name) {
        return canonicalPath((parent === "/" ? "" : parent) + "/" + name);
      }

      function parentPath(path) {
        const parts = path.split("/").filter(Boolean);
        parts.pop();
        return parts.length ? "/" + parts.join("/") : "/";
      }

      function renderBreadcrumbs() {
        const nav = $("breadcrumbs");
        nav.replaceChildren();
        let path = "/";
        const parts = state.path.split("/").filter(Boolean);
        const root = document.createElement("button");
        root.className = "crumb" + (parts.length ? "" : " current");
        root.type = "button";
        root.textContent = rootName;
        root.disabled = !parts.length;
        root.addEventListener("click", () => load("/"));
        nav.append(root);
        parts.forEach((part, index) => {
          const separator = document.createElement("span");
          separator.className = "crumb-separator";
          separator.textContent = "/";
          nav.append(separator);
          path = joinPath(path, part);
          const crumb = document.createElement("button");
          crumb.className = "crumb" + (index === parts.length - 1 ? " current" : "");
          crumb.type = "button";
          crumb.textContent = part;
          crumb.disabled = index === parts.length - 1;
          const target = path;
          crumb.addEventListener("click", () => load(target));
          nav.append(crumb);
        });
        const title = parts.length ? parts[parts.length - 1] : rootName;
        $("folder-title").textContent = title;
        $("folder-note").textContent = parts.length ? "当前文件夹中的文件和文件夹" : `管理 ${rootName} 中的文件和文件夹`;
        $("path-value").textContent = state.path;
        $("drop-target").textContent = state.path;
        updateControls();
      }

      function formatBytes(value) {
        if (value === 0) return "0 B";
        if (!Number.isFinite(Number(value))) return "-";
        const units = ["B", "KiB", "MiB", "GiB", "TiB"];
        let size = Number(value), unit = 0;
        while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit += 1; }
        return `${size >= 10 || unit === 0 ? size.toFixed(0) : size.toFixed(1)} ${units[unit]}`;
      }

      function formatRate(value) {
        if (!Number.isFinite(Number(value)) || Number(value) <= 0) return "0 B/s";
        const units = ["B", "KiB", "MiB", "GiB", "TiB"];
        let rate = Number(value), unit = 0;
        while (rate >= 1024 && unit < units.length - 1) { rate /= 1024; unit += 1; }
        return `${rate >= 10 || unit === 0 ? rate.toFixed(0) : rate.toFixed(1)} ${units[unit]}/s`;
      }

      function formatTime(value) {
        const timestamp = Number(value);
        if (!Number.isFinite(timestamp) || timestamp <= 0) return "-";
        return new Date(timestamp * 1000).toLocaleString("zh-CN", { hour12: false });
      }

      function actionButton(label, title, icon, handler, danger = false) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "action-button" + (danger ? " danger" : "");
        button.title = title;
        button.setAttribute("aria-label", title);
        const iconNode = document.createElement("span");
        iconNode.className = "action-icon";
        iconNode.setAttribute("aria-hidden", "true");
        iconNode.textContent = icon;
        const labelNode = document.createElement("span");
        labelNode.textContent = label;
        button.append(iconNode, labelNode);
        button.addEventListener("click", handler);
        return button;
      }

      function filteredEntries() {
        const query = state.search.trim().toLocaleLowerCase();
        if (!query) return state.entries;
        return state.entries.filter((entry) => entry.name.toLocaleLowerCase().includes(query));
      }

      function renderEntries() {
        const body = $("entries");
        body.replaceChildren();
        const entries = filteredEntries();
        const empty = $("empty");
        empty.classList.toggle("hidden", entries.length !== 0);
        if (!entries.length) {
          const title = document.createElement("strong");
          const unavailable = state.connection !== "connected" && state.connection !== "checking";
          title.textContent = unavailable
            ? state.connection === "permission_denied"
              ? "无权访问当前工作区"
              : state.connection === "session_expired"
                ? "WPS 登录已过期"
                : state.connection === "not_configured"
                  ? "WPS 尚未连接"
                  : "暂时无法读取目录"
            : state.entries.length ? "没有匹配的项目" : "这个文件夹还是空的";
          const note = document.createElement("span");
          note.textContent = unavailable
            ? connectionMessage(state.connection)
            : state.entries.length ? "换一个关键词试试" : "上传文件或新建文件夹开始使用";
          empty.replaceChildren(title, note);
        }
        entries.forEach((entry) => {
          const entryPath = joinPath(state.path, entry.name);
          const row = document.createElement("tr");
          const nameCell = document.createElement("td");
          const nameWrap = document.createElement("div");
          nameWrap.className = "name-cell";
          const glyph = document.createElement("span");
          glyph.className = "entry-glyph " + (entry.kind === "folder" ? "folder" : "file");
          glyph.setAttribute("aria-hidden", "true");
          const open = document.createElement("button");
          open.type = "button";
          open.className = "entry-name" + (entry.kind === "folder" ? " folder" : "");
          open.textContent = entry.name;
          open.title = entry.kind === "folder" ? "打开文件夹" : "下载文件";
          open.addEventListener("click", () => entry.kind === "folder" ? load(entryPath) : download(entry, entryPath));
          nameWrap.append(glyph, open);
          nameCell.append(nameWrap);

          const typeCell = document.createElement("td");
          typeCell.className = "meta";
          typeCell.textContent = entry.kind === "folder" ? "文件夹" : "文件";
          const sizeCell = document.createElement("td");
          sizeCell.className = "meta";
          sizeCell.textContent = entry.kind === "folder" ? "-" : formatBytes(entry.size);
          const timeCell = document.createElement("td");
          timeCell.className = "meta";
          timeCell.textContent = formatTime(entry.modified_at);
          const actionsCell = document.createElement("td");
          const actions = document.createElement("div");
          actions.className = "actions";
          if (entry.kind === "file") actions.append(actionButton("下载", "下载文件", "↓", () => download(entry, entryPath)));
          actions.append(actionButton("改名", "重命名", "✎", () => rename(entry, entryPath)));
          actions.append(actionButton("移动", "移动到其他文件夹", "↗", () => move(entry, entryPath)));
          actions.append(actionButton("删除", "删除", "×", () => remove(entry, entryPath), true));
          actionsCell.append(actions);
          row.append(nameCell, typeCell, sizeCell, timeCell, actionsCell);
          body.append(row);
        });
        updateControls();
        const total = state.entries.length;
        const visible = entries.length;
        $("panel-summary").innerHTML = state.search.trim()
          ? `<strong>${visible}</strong> 个匹配项目，共 ${total} 个`
          : `<strong>${total}</strong> 个项目`;
      }

      async function checkConnection(quiet = false) {
        setConnection("checking");
        try {
          const data = await apiRequest("status");
          const value = data && typeof data.status === "string" ? data.status : "invalid_response";
          setConnection(value);
          if (value !== "connected" && (!quiet || state.entries.length === 0)) {
            setStatus(connectionMessage(state.connection), "error");
          }
          return state.connection;
        } catch (error) {
          showError(error);
          return state.connection;
        }
      }

      async function load(path, quiet = false, force = false) {
        if (state.busy && !force) return;
        state.path = canonicalPath(path);
        state.search = "";
        $("search-input").value = "";
        renderBreadcrumbs();
        if (!quiet) setStatus("正在读取...");
        try {
          const connection = await checkConnection(quiet);
          if (connection !== "connected") {
            state.entries = [];
            renderEntries();
            return;
          }
          const data = await api("entries", state.path);
          state.entries = Array.isArray(data.entries) ? data.entries : [];
          setConnection("connected");
          renderEntries();
          setStatus(`${state.entries.length} 个项目`, "success");
        } catch (error) {
          state.entries = [];
          showError(error);
          renderEntries();
        }
      }

      function download(entry, path) {
        const link = document.createElement("a");
        link.href = pathUrl("download", path).toString();
        // Let the server's Content-Disposition choose the filename. This
        // keeps the browser's native download lifecycle and auth handling.
        link.rel = "noopener";
        document.body.append(link);
        link.click();
        link.remove();
      }

      async function changeRootName() {
        const name = await openInputModal("设置云盘名称", "云盘名称", rootName, "例如：我的云盘", "保存");
        if (!name || name === rootName) return;
        setBusy(true);
        try {
          const response = await fetch(apiUrl("settings"), {
            method: "PATCH",
            cache: "no-store",
            credentials: "same-origin",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ name }),
          });
          const data = await responseData(response);
          if (!data || typeof data.name !== "string") throw new Error("服务器没有返回新的云盘名称");
          rootName = data.name;
          document.title = rootName;
          renderBreadcrumbs();
          setStatus("云盘名称已更新", "success");
        } catch (error) { showError(error); }
        finally { setBusy(false); renderBreadcrumbs(); }
      }

      function hasFileTransfer(event) {
        const transfer = event.dataTransfer;
        if (!transfer) return false;
        return Array.from(transfer.types || []).includes("Files") || transfer.files.length > 0;
      }

      function showDropOverlay() {
        if (state.busy || state.connection !== "connected") return;
        $("drop-target").textContent = state.path;
        $("drop-overlay").classList.add("active");
        $("drop-overlay").setAttribute("aria-hidden", "false");
      }

      function hideDropOverlay() {
        $("drop-overlay").classList.remove("active");
        $("drop-overlay").setAttribute("aria-hidden", "true");
      }

      function closeModal(value) {
        if (!modalResolve) return;
        const resolve = modalResolve;
        modalResolve = null;
        $("modal").close();
        resolve(value);
      }

      function openInputModal(title, label, value = "", placeholder = "", submitText = "确定") {
        modalMode = "input";
        $("modal-title").textContent = title;
        $("modal-message").classList.add("hidden");
        $("modal-label").classList.remove("hidden");
        $("modal-label-text").textContent = label;
        $("modal-input").value = value;
        $("modal-input").placeholder = placeholder;
        $("modal-submit").textContent = submitText;
        $("modal-submit").className = "modal-submit primary";
        $("modal").showModal();
        setTimeout(() => $("modal-input").focus(), 0);
        return new Promise((resolve) => { modalResolve = resolve; });
      }

      function openConfirmModal(title, message, submitText = "确定", danger = false) {
        modalMode = "confirm";
        $("modal-title").textContent = title;
        $("modal-message").textContent = message;
        $("modal-message").classList.remove("hidden");
        $("modal-label").classList.add("hidden");
        $("modal-submit").textContent = submitText;
        $("modal-submit").className = "modal-submit" + (danger ? " danger" : " primary");
        $("modal").showModal();
        setTimeout(() => $("modal-submit").focus(), 0);
        return new Promise((resolve) => { modalResolve = resolve; });
      }

      async function createFolder() {
        const name = await openInputModal("新建文件夹", "文件夹名称", "", "例如：项目资料");
        if (!name) return;
        setBusy(true);
        try {
          await api("folders", joinPath(state.path, name), { method: "POST" });
          setStatus("文件夹已创建", "success");
          await load(state.path, true, true);
        } catch (error) { showError(error); }
        finally { setBusy(false); renderBreadcrumbs(); }
      }

      async function rename(entry, path) {
        const name = await openInputModal("重命名", "新名称", entry.name);
        if (!name || name === entry.name) return;
        setBusy(true);
        try {
          await api("entries", path, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name }) });
          setStatus("名称已更新", "success");
          await load(state.path, true, true);
        } catch (error) { showError(error); }
        finally { setBusy(false); renderBreadcrumbs(); }
      }

      async function move(entry, path) {
        const destination = await openInputModal("移动项目", "目标文件夹路径", state.path, "例如：/项目资料");
        if (!destination) return;
        setBusy(true);
        try {
          await api("entries", path, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ parent_path: canonicalPath(destination) }) });
          setStatus("项目已移动", "success");
          await load(state.path, true, true);
        } catch (error) { showError(error); }
        finally { setBusy(false); renderBreadcrumbs(); }
      }

      async function remove(entry, path) {
        const confirmed = await openConfirmModal("删除项目", `确定删除“${entry.name}”吗？此操作会同步到 WPS。`, "删除", true);
        if (!confirmed) return;
        setBusy(true);
        try {
          await api("entries", path, { method: "DELETE" });
          setStatus("项目已删除", "success");
          await load(state.path, true, true);
        } catch (error) { showError(error); }
        finally { setBusy(false); renderBreadcrumbs(); }
      }

      function uploadOne(file, overwrite) {
        return new Promise((resolve, reject) => {
          const url = pathUrl("upload", joinPath(state.path, file.name));
          if (overwrite) url.searchParams.set("overwrite", "true");
          const startedAt = performance.now();
          const updateSpeed = (loaded) => {
            const elapsed = Math.max((performance.now() - startedAt) / 1000, 0.001);
            $("upload-speed").textContent = `${formatRate(loaded / elapsed)} · ${formatBytes(loaded)} / ${formatBytes(file.size)}`;
            $("upload-speed").classList.remove("hidden");
          };
          $("upload-speed").textContent = "正在计算速度...";
          $("upload-speed").classList.remove("hidden");
          const xhr = new XMLHttpRequest();
          xhr.open("PUT", url.toString());
          xhr.withCredentials = true;
          xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");
          xhr.upload.onprogress = (event) => {
            if (!event.lengthComputable) return;
            const percent = Math.round(event.loaded * 100 / event.total);
            $("progress").value = percent;
            updateSpeed(event.loaded);
            setStatus(`正在上传 ${file.name} · ${percent}%`);
          };
          xhr.onload = () => {
            if (xhr.status >= 200 && xhr.status < 300) {
              updateSpeed(file.size);
              resolve();
              return;
            }
            let message = `上传失败（${xhr.status}）`;
            let payload = null;
            try {
              payload = JSON.parse(xhr.responseText);
              message = payload.error || message;
            } catch (_) {}
            const error = new Error(message);
            error.status = xhr.status;
            if (payload && typeof payload.code === "string") error.code = payload.code;
            reject(error);
          };
          xhr.onerror = () => reject(new Error("上传连接失败"));
          xhr.send(file);
        });
      }

      async function uploadFiles(files) {
        if (!files.length || state.busy || state.connection !== "connected") return;
        setBusy(true);
        $("progress").classList.remove("hidden");
        try {
          for (let index = 0; index < files.length; index += 1) {
            const file = files[index];
            const existing = state.entries.find((entry) => entry.name === file.name);
            let overwrite = false;
            if (existing) {
              if (existing.kind !== "file") continue;
              const confirmed = await openConfirmModal("文件已存在", `“${file.name}”已经存在，要覆盖它吗？`, "覆盖", false);
              if (!confirmed) continue;
              overwrite = true;
            }
            if (files.length > 1) setStatus(`准备上传第 ${index + 1}/${files.length} 个文件`);
            await uploadOne(file, overwrite);
          }
          setStatus("上传完成", "success");
          await load(state.path, true, true);
        } catch (error) { showError(error); }
        finally {
          $("progress").classList.add("hidden");
          $("progress").value = 0;
          $("upload-speed").classList.add("hidden");
          $("upload-speed").textContent = "";
          setBusy(false);
          renderBreadcrumbs();
        }
      }

      $("modal-form").addEventListener("submit", (event) => {
        event.preventDefault();
        if (modalMode === "input") closeModal($("modal-input").value.trim());
        else closeModal(true);
      });
      $("modal-cancel").addEventListener("click", () => closeModal(null));
      $("modal").addEventListener("cancel", (event) => { event.preventDefault(); closeModal(null); });
      $("settings-button").addEventListener("click", changeRootName);
      $("up-button").addEventListener("click", () => load(parentPath(state.path)));
      $("refresh-button").addEventListener("click", () => load(state.path));
      $("folder-button").addEventListener("click", createFolder);
      $("upload-button").addEventListener("click", () => $("file-input").click());
      $("drop-upload-button").addEventListener("click", () => $("file-input").click());
      $("file-input").addEventListener("change", (event) => {
        uploadFiles(Array.from(event.target.files || []));
        event.target.value = "";
      });
      $("search-input").addEventListener("input", (event) => {
        state.search = event.target.value;
        renderEntries();
      });
      let dragDepth = 0;
      window.addEventListener("dragenter", (event) => {
        if (!hasFileTransfer(event)) return;
        event.preventDefault();
        if (state.busy) return;
        dragDepth += 1;
        showDropOverlay();
      });
      window.addEventListener("dragover", (event) => {
        if (!hasFileTransfer(event)) return;
        event.preventDefault();
        if (state.busy) return;
        showDropOverlay();
      });
      window.addEventListener("dragleave", (event) => {
        if (!hasFileTransfer(event)) return;
        event.preventDefault();
        dragDepth = Math.max(0, dragDepth - 1);
        if (dragDepth === 0) hideDropOverlay();
      });
      window.addEventListener("drop", (event) => {
        if (!hasFileTransfer(event)) return;
        event.preventDefault();
        dragDepth = 0;
        hideDropOverlay();
        if (!state.busy) uploadFiles(Array.from(event.dataTransfer.files || []));
      });
      window.setInterval(async () => {
        if (state.busy) return;
        const previous = state.connection;
        const current = await checkConnection(true);
        if (current !== "connected") {
          state.entries = [];
          renderEntries();
        } else if (previous !== "connected") {
          await load(state.path, true, true);
        }
      }, 30000);
      renderBreadcrumbs();
      load("/");
    })();
  </script>
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
