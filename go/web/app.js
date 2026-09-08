(() => {
  "use strict";

  /* ============ 基础工具 ============ */
  const $ = (id) => document.getElementById(id);
  const apiRoot = "/api/v1/";
  const DIRECTORY_CACHE_TTL_MS = 30 * 1000;
  const PREFETCH_CONCURRENCY = 2;
  const PREFETCH_MAX_FOLDERS = 24;

  const state = {
    path: "/",
    entries: [],
    busy: false,
    loading: false,
    search: "",
    connection: "checking",
    view: "list",
    sort: { key: "auto", dir: "asc" },
  };

  function icon(name, cls = "") {
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    // SVG 元素的 className 是只读的 SVGAnimatedString，必须用 setAttribute。
    svg.setAttribute("class", "icon" + (cls ? " " + cls : ""));
    const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", "#i-" + name);
    svg.append(use);
    return svg;
  }

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  /* ============ 本地偏好 ============ */
  const PREF = {
    get(key, fallback) {
      try {
        const raw = localStorage.getItem("wpsdrv." + key);
        return raw === null ? fallback : raw;
      } catch (_) { return fallback; }
    },
    set(key, value) {
      try { localStorage.setItem("wpsdrv." + key, value); } catch (_) {}
    },
  };

  /* ============ 主题 ============ */
  const THEME_ORDER = ["auto", "light", "dark"];
  const THEME_META = {
    auto: { icon: "monitor", label: "主题：跟随系统（点击切换为浅色）", color: "#f5f7fc" },
    light: { icon: "sun", label: "主题：浅色（点击切换为深色）", color: "#f5f7fc" },
    dark: { icon: "moon", label: "主题：深色（点击切换为跟随系统）", color: "#0e1424" },
  };
  const darkQuery = window.matchMedia("(prefers-color-scheme: dark)");
  let theme = PREF.get("theme", "auto");
  if (!THEME_ORDER.includes(theme)) theme = "auto";

  function applyTheme(animate = false) {
    // 跟随系统时不设置 data-theme，由样式表里的 prefers-color-scheme
    // 媒体查询决定外观，切主题与首帧都不会闪烁。
    if (theme === "auto") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
    if (animate) {
      const root = document.documentElement;
      root.classList.add("theme-anim");
      setTimeout(() => root.classList.remove("theme-anim"), 380);
    }
    const meta = THEME_META[theme];
    const resolvedColor = theme === "auto"
      ? (darkQuery.matches ? THEME_META.dark.color : THEME_META.light.color)
      : meta.color;
    const button = $("theme-button");
    button.title = meta.label;
    button.setAttribute("aria-label", meta.label);
    const use = $("theme-icon");
    use.setAttribute("href", "#i-" + meta.icon);
    button.classList.remove("spin-icon");
    void button.offsetWidth;
    button.classList.add("spin-icon");
    setTimeout(() => button.classList.remove("spin-icon"), 480);
    document.querySelector('meta[name="theme-color"]').setAttribute("content", resolvedColor);
  }

  function cycleTheme() {
    theme = THEME_ORDER[(THEME_ORDER.indexOf(theme) + 1) % THEME_ORDER.length];
    PREF.set("theme", theme);
    applyTheme(true);
  }
  darkQuery.addEventListener("change", () => { if (theme === "auto") applyTheme(false); });

  /* ============ REST 基础 ============ */
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

  /* ============ 网页账号会话 ============ */
  let webUser = null;
  let authInFlight = false;

  function setAuthMessage(message, kind = "") {
    const node = $("auth-message");
    node.textContent = message || "";
    node.className = "auth-message" + (kind ? " " + kind : "");
  }

  function showAppForUser(user) {
    webUser = user || null;
    $("auth-screen").classList.add("hidden");
    $("app-ui").classList.remove("hidden");
    const name = webUser && webUser.username ? webUser.username : "退出登录";
    $("logout-button").title = `退出登录（${name}）`;
    $("logout-button").setAttribute("aria-label", `退出登录（${name}）`);
  }

  function showLoginScreen() {
    $("auth-screen").classList.remove("hidden");
    $("app-ui").classList.add("hidden");
  }

  function switchAuthMode(mode) {
    const register = mode === "register";
    $("login-tab").classList.toggle("active", !register);
    $("login-tab").setAttribute("aria-selected", String(!register));
    $("register-tab").classList.toggle("active", register);
    $("register-tab").setAttribute("aria-selected", String(register));
    $("login-form").classList.toggle("hidden", register);
    $("register-form").classList.toggle("hidden", !register);
    $("auth-title").textContent = register ? "创建账号" : "欢迎回来";
    $("auth-subtitle").textContent = register ? "创建一个账号来保护你的文件" : "登录后管理你的 WPS 文件";
    setAuthMessage("");
    const first = register ? $("register-username") : $("login-username");
    setTimeout(() => first.focus(), 0);
  }

  async function submitAuth(mode, event) {
    event.preventDefault();
    if (authInFlight) return;
    const register = mode === "register";
    const username = $(`${mode}-username`).value.trim();
    const password = $(`${mode}-password`).value;
    if (register && password !== $("register-confirm").value) {
      setAuthMessage("两次输入的密码不一致");
      return;
    }
    if (!username || !password) {
      setAuthMessage("请输入用户名和密码");
      return;
    }
    authInFlight = true;
    const submit = $(`${mode}-submit`);
    submit.disabled = true;
    setAuthMessage(register ? "正在创建账号…" : "正在登录…", "pending");
    try {
      const data = await apiRequest(`auth/${register ? "register" : "login"}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      showAppForUser(data && data.user);
      setAuthMessage("");
      await startDrive();
    } catch (error) {
      setAuthMessage(error.message || "操作失败，请稍后重试");
    } finally {
      authInFlight = false;
      submit.disabled = false;
    }
  }

  async function initWebAuth() {
    try {
      const data = await apiRequest("auth/me");
      if (data && data.registration_enabled === false) $("register-tab").classList.add("hidden");
      if (!data || data.authenticated !== true) {
        showLoginScreen();
        return false;
      }
      showAppForUser(data.user);
      return true;
    } catch (error) {
      showLoginScreen();
      setAuthMessage("无法连接服务，请刷新页面重试");
      return false;
    }
  }

  async function logout() {
    if (authInFlight) return;
    try {
      await apiRequest("auth/logout", { method: "POST" });
    } catch (_) {
      // The local session is unusable even when the network is already down.
    }
    window.location.reload();
  }

  /* ============ 目录缓存与预取 ============ */
  const directoryCache = new Map();
  let directoryCacheEpoch = 0;
  let prefetchQueue = [];
  let prefetchActive = 0;
  let prefetchGeneration = 0;
  let navigationGeneration = 0;
  let rootName = "WPS Enterprise Drive";

  function clearDirectoryCache() {
    directoryCacheEpoch += 1;
    directoryCache.clear();
    prefetchQueue = [];
    prefetchGeneration += 1;
  }

  function directoryEntries(path, force = false) {
    const key = canonicalPath(path);
    const now = Date.now();
    const existing = directoryCache.get(key);
    if (!force && existing && existing.entries && existing.expiresAt > now) {
      return Promise.resolve(existing.entries);
    }
    if (existing && existing.pending) return existing.pending;
    if (force && existing) directoryCache.delete(key);

    const epoch = directoryCacheEpoch;
    const pending = api("entries", key).then((data) => {
      const entries = Array.isArray(data.entries) ? data.entries : [];
      if (epoch === directoryCacheEpoch) {
        directoryCache.set(key, {
          entries,
          expiresAt: Date.now() + DIRECTORY_CACHE_TTL_MS,
        });
      }
      return entries;
    }).catch((error) => {
      const current = directoryCache.get(key);
      if (current && current.pending === pending) directoryCache.delete(key);
      throw error;
    });
    directoryCache.set(key, {
      entries: existing && existing.entries ? existing.entries : null,
      expiresAt: 0,
      pending,
    });
    return pending;
  }

  function pumpPrefetch(generation) {
    if (generation !== prefetchGeneration) return;
    while (prefetchActive < PREFETCH_CONCURRENCY && prefetchQueue.length) {
      const path = prefetchQueue.shift();
      prefetchActive += 1;
      directoryEntries(path).catch(() => {}).finally(() => {
        prefetchActive -= 1;
        pumpPrefetch(prefetchGeneration);
      });
    }
  }

  function prefetchChildDirectories(parentPath, entries) {
    prefetchGeneration += 1;
    const generation = prefetchGeneration;
    prefetchQueue = entries
      .filter((entry) => entry && entry.kind === "folder" && typeof entry.name === "string")
      .slice(0, PREFETCH_MAX_FOLDERS)
      .map((entry) => joinPath(parentPath, entry.name));
    pumpPrefetch(generation);
  }

  /* ============ 状态行 / 通知 ============ */
  function setStatus(message, kind = "") {
    const status = $("status");
    status.textContent = message;
    status.parentElement.className = "status-row" + (kind ? " " + kind : "");
  }

  function toast(message, kind = "info", ttl = 4200) {
    const wrap = $("toasts");
    while (wrap.children.length >= 4) wrap.firstElementChild.remove();
    const node = el("div", "toast " + kind);
    const iconName = kind === "success" ? "check" : kind === "error" ? "alert"
      : kind === "warn" ? "warn" : "info";
    const iconWrap = el("span", "toast-icon");
    iconWrap.append(icon(iconName));
    node.append(iconWrap, el("span", "toast-text", message));
    const close = el("button", "toast-close");
    close.type = "button";
    close.title = "关闭";
    close.setAttribute("aria-label", "关闭通知");
    close.append(icon("x"));
    close.addEventListener("click", () => dismiss());
    node.append(close);
    let gone = false;
    function dismiss() {
      if (gone) return;
      gone = true;
      clearTimeout(timer);
      node.classList.add("out");
      node.addEventListener("animationend", () => node.remove(), { once: true });
      setTimeout(() => node.remove(), 400);
    }
    const timer = setTimeout(dismiss, Math.max(1500, ttl));
    wrap.append(node);
  }

  /* ============ 连接状态 ============ */
  function updateControls() {
    const unavailable = state.connection !== "connected";
    $("settings-button").disabled = state.busy;
    $("up-button").disabled = state.busy || unavailable || state.path === "/";
    $("refresh-button").disabled = state.busy;
    [$("folder-button"), $("upload-button")].forEach((button) => {
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

  function showError(error, { notify = true } = {}) {
    if (isWpsError(error)) {
      const connection = error.code === "wps_session_expired"
        ? "session_expired"
        : "upstream_unavailable";
      setConnection(connection);
      setStatus(connectionMessage(connection), "error");
      if (notify) toast(connectionMessage(connection), "error", 6000);
      return;
    }
    if (state.connection === "checking") setConnection("unknown");
    setStatus(error.message, "error");
    if (notify) toast(error.message, "error", 6000);
  }

  /* ============ 路径 ============ */
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

  /* ============ hash 路由 ============ */
  let suppressHash = false;

  function hashPath() {
    const raw = location.hash.replace(/^#/, "");
    if (!raw) return "/";
    try { return canonicalPath(decodeURIComponent(raw)); }
    catch (_) { return "/"; }
  }

  function syncHash(path) {
    const target = "#" + path;
    if (location.hash === target) return;
    suppressHash = true;
    location.hash = target;
  }

  window.addEventListener("hashchange", () => {
    if (suppressHash) { suppressHash = false; return; }
    const target = hashPath();
    if (target !== state.path) load(target, false, true);
  });

  /* ============ 排序 ============ */
  const SORT_DEFAULT_DIR = { name: "asc", size: "desc", time: "desc" };

  function saveSort() {
    PREF.set("sort", state.sort.key + ":" + state.sort.dir);
  }

  function loadSort() {
    const raw = PREF.get("sort", "auto:asc");
    const [key, dir] = raw.split(":");
    if (["auto", "name", "size", "time"].includes(key)) {
      state.sort.key = key;
      state.sort.dir = dir === "desc" ? "desc" : "asc";
    }
  }

  function sortedEntries(entries) {
    const { key, dir } = state.sort;
    if (key === "auto") return entries;
    const sign = dir === "desc" ? -1 : 1;
    return entries.slice().sort((a, b) => {
      const folderDelta = (b.kind === "folder") - (a.kind === "folder");
      if (folderDelta) return folderDelta;
      if (key === "size") return sign * ((Number(a.size) || 0) - (Number(b.size) || 0));
      if (key === "time") return sign * ((Number(a.modified_at) || 0) - (Number(b.modified_at) || 0));
      return sign * String(a.name).localeCompare(String(b.name), "zh-Hans-CN", { numeric: true });
    });
  }

  function renderSortControls() {
    const { key, dir } = state.sort;
    $("sort-select").value = key;
    const dirButton = $("sort-dir-button");
    dirButton.disabled = key === "auto";
    $("sort-dir-icon").setAttribute("href", dir === "desc" ? "#i-chev-down" : "#i-chev-up");
    dirButton.title = dir === "desc" ? "当前降序（点击切换为升序）" : "当前升序（点击切换为降序）";
    document.querySelectorAll("#list-head .sort-button").forEach((button) => {
      const cell = button.closest(".cell");
      if (button.dataset.sort === key && key !== "auto") {
        button.dataset.active = "true";
        button.dataset.dir = dir;
        cell.setAttribute("aria-sort", dir === "desc" ? "descending" : "ascending");
      } else {
        delete button.dataset.active;
        delete button.dataset.dir;
        cell.removeAttribute("aria-sort");
      }
    });
  }

  function setSort(key, dir) {
    state.sort = { key, dir };
    saveSort();
    renderSortControls();
    renderEntries({ animate: true });
  }

  /* ============ 视图切换 ============ */
  function setView(view, { animate = true } = {}) {
    state.view = view === "grid" ? "grid" : "list";
    PREF.set("view", state.view);
    $("view-toggle").dataset.view = state.view;
    $("view-list-button").setAttribute("aria-pressed", String(state.view === "list"));
    $("view-grid-button").setAttribute("aria-pressed", String(state.view === "grid"));
    $("list-head").hidden = state.view === "grid";
    if (animate) renderEntries({ animate: true });
  }

  /* ============ 文件类型 ============ */
  const EXT_KINDS = [
    [["jpg", "jpeg", "png", "gif", "webp", "svg", "bmp", "ico", "avif", "heic"], "image", "tint-violet", "图片"],
    [["mp4", "mov", "avi", "mkv", "webm", "flv", "m4v", "wmv"], "video", "tint-red", "视频"],
    [["mp3", "wav", "flac", "aac", "ogg", "m4a", "wma"], "music", "tint-cyan", "音频"],
    [["zip", "rar", "7z", "tar", "gz", "bz2", "xz", "iso"], "archive", "tint-amber", "压缩包"],
    [["doc", "docx", "txt", "rtf", "md", "pages"], "file-text", "tint-blue", "文档"],
    [["xls", "xlsx", "csv", "numbers"], "sheet", "tint-green", "表格"],
    [["ppt", "pptx", "key"], "image", "tint-amber", "幻灯片"],
    [["pdf"], "file-text", "tint-red", "PDF"],
    [["js", "ts", "html", "css", "json", "py", "go", "java", "c", "cpp", "h", "sh", "yml", "yaml", "xml", "sql"], "code", "tint-slate", "代码"],
  ];

  function extKind(name) {
    const dot = String(name).lastIndexOf(".");
    if (dot < 0 || dot === name.length - 1) return { icon: "file", tint: "tint-slate", label: "文件" };
    const ext = String(name).slice(dot + 1).toLowerCase();
    for (const [list, iconName, tint, label] of EXT_KINDS) {
      if (list.includes(ext)) return { icon: iconName, tint, label };
    }
    return { icon: "file", tint: "tint-blue", label: "文件" };
  }

  function glyphNode(entry, large = false) {
    const spec = entry.kind === "folder"
      ? { icon: "folder", tint: "tint-amber" }
      : extKind(entry.name);
    const glyph = el("span", "entry-glyph " + spec.tint + (large ? " lg" : ""));
    glyph.setAttribute("aria-hidden", "true");
    glyph.append(icon(spec.icon));
    return glyph;
  }

  /* ============ 格式化 ============ */
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

  function formatShortTime(value) {
    const timestamp = Number(value);
    if (!Number.isFinite(timestamp) || timestamp <= 0) return "-";
    const date = new Date(timestamp * 1000);
    const now = new Date();
    const sameYear = date.getFullYear() === now.getFullYear();
    return date.toLocaleDateString("zh-CN", {
      month: "numeric", day: "numeric",
      year: sameYear ? undefined : "numeric",
    });
  }

  /* ============ 搜索高亮 ============ */
  function escapeRegExp(text) {
    return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  }

  function highlightedName(name, query) {
    if (!query) return document.createTextNode(name);
    const pattern = new RegExp(escapeRegExp(query), "ig");
    const fragment = document.createDocumentFragment();
    let last = 0;
    let match;
    while ((match = pattern.exec(String(name))) !== null) {
      if (match.index > last) fragment.append(document.createTextNode(String(name).slice(last, match.index)));
      fragment.append(el("mark", "", match[0]));
      last = match.index + match[0].length;
      if (match[0].length === 0) pattern.lastIndex += 1;
    }
    if (last < String(name).length) fragment.append(document.createTextNode(String(name).slice(last)));
    return fragment;
  }

  /* ============ 面包屑 ============ */
  function renderBreadcrumbs() {
    const nav = $("breadcrumbs");
    nav.replaceChildren();
    let path = "/";
    const parts = state.path.split("/").filter(Boolean);
    const root = el("button", "crumb" + (parts.length ? "" : " current"));
    root.type = "button";
    root.disabled = !parts.length;
    root.append(icon("home"));
    root.append(el("span", "", rootName));
    root.addEventListener("click", () => load("/"));
    nav.append(root);
    parts.forEach((part, index) => {
      const separator = el("span", "crumb-separator");
      separator.append(icon("chev-right"));
      nav.append(separator);
      path = joinPath(path, part);
      const crumb = el("button", "crumb" + (index === parts.length - 1 ? " current" : ""));
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
    document.title = title === rootName ? rootName : `${title} · ${rootName}`;
    updateControls();
  }

  /* ============ 骨架屏 ============ */
  function renderSkeleton() {
    const holder = $("skeleton");
    holder.classList.remove("hidden");
    holder.dataset.view = state.view;
    holder.replaceChildren();
    for (let index = 0; index < 8; index += 1) {
      if (state.view === "grid") {
        const card = el("div", "sk-card");
        card.append(el("div", "sk sk-glyph"));
        const long = el("div", "sk sk-line"); long.style.width = "82%";
        const mid = el("div", "sk sk-line"); mid.style.width = "55%";
        card.append(long, mid, el("div", "sk sk-line"));
        holder.append(card);
      } else {
        const row = el("div", "sk-row");
        const nameCell = el("div", "sk-cell");
        nameCell.append(el("div", "sk sk-glyph"));
        const line = el("div", "sk sk-line"); line.style.width = "46%";
        nameCell.append(line);
        row.append(nameCell, el("div", "sk sk-line"), el("div", "sk sk-line"), el("div", "sk sk-line"), el("div", "sk sk-line"));
        holder.append(row);
      }
    }
  }

  function hideSkeleton() {
    $("skeleton").classList.add("hidden");
  }

  /* ============ 条目渲染 ============ */
  function filteredEntries() {
    const query = state.search.trim().toLocaleLowerCase();
    if (!query) return state.entries;
    return state.entries.filter((entry) => entry.name.toLocaleLowerCase().includes(query));
  }

  function actionButton(label, title, iconName, handler, danger = false) {
    const button = el("button", "action-button" + (danger ? " danger" : ""));
    button.type = "button";
    button.title = title;
    button.setAttribute("aria-label", title);
    button.append(icon(iconName, "action-icon"));
    button.append(el("span", "label", label));
    button.addEventListener("click", handler);
    return button;
  }

  function entryActions(entry, entryPath) {
    const actions = el("div", "actions");
    if (entry.kind === "file") {
      actions.append(actionButton("下载", "下载文件", "download", () => download(entry, entryPath)));
    }
    actions.append(actionButton("改名", "重命名", "pencil", () => rename(entry, entryPath)));
    actions.append(actionButton("移动", "移动到其他文件夹", "move", () => move(entry, entryPath)));
    actions.append(actionButton("删除", "删除", "trash", () => remove(entry, entryPath), true));
    return actions;
  }

  function openTarget(entry, entryPath) {
    if (entry.kind === "folder") load(entryPath);
    else download(entry, entryPath);
  }

  function renderEmpty(entries) {
    const empty = $("empty");
    empty.classList.toggle("hidden", entries.length !== 0 || state.loading);
    if (entries.length || state.loading) return;
    const unavailable = state.connection !== "connected" && state.connection !== "checking";
    const mode = unavailable ? "offline" : state.entries.length ? "search" : "empty";
    empty.dataset.mode = mode;
    const art = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    art.setAttribute("class", "illustration");
    const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", mode === "search" ? "#i-ill-search" : mode === "offline" ? "#i-ill-offline" : "#i-ill-empty");
    art.append(use);
    const title = el("strong");
    const note = el("span");
    if (unavailable) {
      title.textContent = state.connection === "permission_denied"
        ? "无权访问当前工作区"
        : state.connection === "session_expired"
          ? "WPS 登录已过期"
          : state.connection === "not_configured"
            ? "WPS 尚未连接"
            : "暂时无法读取目录";
      note.textContent = connectionMessage(state.connection);
    } else if (state.entries.length) {
      title.textContent = "没有匹配的项目";
      note.textContent = "换一个关键词试试";
    } else {
      title.textContent = "这个文件夹还是空的";
      note.textContent = "上传文件或新建文件夹开始使用";
    }
    empty.replaceChildren(art, title, note);
  }

  function renderEntries({ animate = false } = {}) {
    if (state.loading) {
      // 目录加载中：只展示骨架屏，避免把上一次的数据闪出来。
      renderSkeleton();
      return;
    }
    const holder = $("entries");
    hideSkeleton();
    const entries = sortedEntries(filteredEntries());
    const isGrid = state.view === "grid";
    holder.setAttribute("role", isGrid ? "list" : "table");
    holder.replaceChildren();
    renderEmpty(entries);
    const query = state.search.trim().toLocaleLowerCase();
    entries.forEach((entry, index) => {
      const entryPath = joinPath(state.path, entry.name);
      let node;
      if (isGrid) {
        node = el("div", "card");
        node.setAttribute("role", "listitem");
        const top = el("div", "card-top");
        top.append(glyphNode(entry, true));
        if (entry.kind === "folder") top.append(icon("chev-right", "card-chev"));
        const name = el("button", "card-name");
        name.type = "button";
        name.title = entry.kind === "folder" ? "打开文件夹" : "下载文件";
        name.append(highlightedName(entry.name, query));
        name.addEventListener("click", () => openTarget(entry, entryPath));
        const typeLabel = entry.kind === "folder" ? "文件夹" : extKind(entry.name).label;
        const meta = el("div", "card-meta",
          entry.kind === "folder"
            ? typeLabel
            : `${typeLabel} · ${formatBytes(entry.size)} · ${formatShortTime(entry.modified_at)}`);
        const ops = el("div", "card-ops");
        ops.append(entryActions(entry, entryPath));
        node.append(top, name, meta, ops);
      } else {
        node = el("div", "list-row");
        node.setAttribute("role", "row");
        const nameCell = el("div", "cell name name-cell");
        nameCell.setAttribute("role", "cell");
        nameCell.append(glyphNode(entry));
        const name = el("button", "entry-name" + (entry.kind === "folder" ? " folder" : ""));
        name.type = "button";
        name.title = entry.kind === "folder" ? "打开文件夹" : "下载文件";
        name.append(highlightedName(entry.name, query));
        name.addEventListener("click", () => openTarget(entry, entryPath));
        nameCell.append(name);
        const typeCell = el("div", "cell type meta",
          entry.kind === "folder" ? "文件夹" : extKind(entry.name).label);
        typeCell.setAttribute("role", "cell");
        const sizeCell = el("div", "cell size meta",
          entry.kind === "folder" ? "—" : formatBytes(entry.size));
        sizeCell.setAttribute("role", "cell");
        const timeCell = el("div", "cell time meta", formatTime(entry.modified_at));
        timeCell.setAttribute("role", "cell");
        const opsCell = el("div", "cell ops");
        opsCell.setAttribute("role", "cell");
        opsCell.append(entryActions(entry, entryPath));
        node.append(nameCell, typeCell, sizeCell, timeCell, opsCell);
      }
      if (animate) node.style.setProperty("--i", String(Math.min(index, 14)));
      holder.append(node);
    });
    updateControls();
    const total = state.entries.length;
    const visible = entries.length;
    const summary = el("span");
    if (state.search.trim()) {
      const strong = el("strong", "", String(visible));
      summary.append(strong, document.createTextNode(` 个匹配项目，共 ${total} 个`));
    } else {
      const strong = el("strong", "", String(total));
      summary.append(strong, document.createTextNode(" 个项目"));
    }
    const panelSummary = $("panel-summary");
    panelSummary.replaceChildren(summary);
  }

  /* ============ 连接检查与加载 ============ */
  async function checkConnection(quiet = false) {
    const previousConnection = state.connection;
    setConnection("checking");
    try {
      const data = await apiRequest("status");
      const value = data && typeof data.status === "string" ? data.status : "invalid_response";
      if (value === "connected" && previousConnection !== "connected") {
        clearDirectoryCache();
      }
      setConnection(value);
      if (value !== "connected" && (!quiet || state.entries.length === 0)) {
        setStatus(connectionMessage(state.connection), "error");
      }
      return state.connection;
    } catch (error) {
      showError(error, { notify: !quiet });
      return state.connection;
    }
  }

  async function load(path, quiet = false, force = false) {
    if (state.busy && !force) return;
    const targetPath = canonicalPath(path);
    const requestGeneration = ++navigationGeneration;
    state.path = targetPath;
    state.search = "";
    $("search-input").value = "";
    syncHash(targetPath);
    renderBreadcrumbs();
    state.loading = true;
    $("refresh-button").classList.add("busy");
    if (!quiet) setStatus("正在读取...");
    renderSkeleton();
    renderEntries({ animate: false });
    try {
      const connection = await checkConnection(quiet);
      if (connection !== "connected") {
        if (requestGeneration !== navigationGeneration) return;
        state.loading = false;
        state.entries = [];
        renderEntries({ animate: true });
        return;
      }
      const entries = await directoryEntries(targetPath, force);
      if (requestGeneration !== navigationGeneration) return;
      state.loading = false;
      state.entries = entries;
      setConnection("connected");
      renderEntries({ animate: true });
      prefetchChildDirectories(targetPath, state.entries);
      setStatus(`${state.entries.length} 个项目`, "success");
    } catch (error) {
      if (requestGeneration !== navigationGeneration) return;
      state.loading = false;
      state.entries = [];
      showError(error, { notify: !quiet });
      renderEntries({ animate: true });
    } finally {
      if (requestGeneration === navigationGeneration) {
        $("refresh-button").classList.remove("busy");
      }
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
    toast(`已开始下载 “${entry.name}”`, "info", 2600);
  }

  /* ============ 云盘名称 ============ */
  function applyRootName() {
    document.title = rootName;
    $("brand-title").textContent = rootName;
    renderBreadcrumbs();
  }

  async function initRootName() {
    try {
      const data = await apiRequest("settings");
      if (data && typeof data.name === "string" && data.name) {
        rootName = data.name;
        applyRootName();
      }
    } catch (error) {
      setStatus("云盘名称读取失败，使用默认名称", "error");
    }
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
      applyRootName();
      setStatus("云盘名称已更新", "success");
      toast("云盘名称已更新", "success");
    } catch (error) { showError(error); }
    finally { setBusy(false); renderBreadcrumbs(); }
  }

  /* ============ 对话框（输入 / 确认） ============ */
  let modalResolve = null;
  let modalMode = "input";

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
    $("modal-submit").className = "primary";
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
    $("modal-submit").className = danger ? "danger-solid" : "primary";
    $("modal").showModal();
    setTimeout(() => $("modal-submit").focus(), 0);
    return new Promise((resolve) => { modalResolve = resolve; });
  }

  /* ============ 文件夹选择器 ============ */
  let pickerResolve = null;
  let pickerSourcePath = "";
  let pickerCurrent = "/";
  let pickerLoading = false;
  let pickerFirstLoad = false;
  let pickerFallback = false;

  function pickerCanMoveHere() {
    if (pickerLoading) return false;
    if (pickerCurrent === parentPath(pickerSourcePath)) return false;
    if (pickerCurrent === pickerSourcePath) return false;
    if (pickerSourcePath !== "/" && pickerCurrent.startsWith(pickerSourcePath + "/")) return false;
    return true;
  }

  function renderPickerState() {
    $("picker-path").textContent = pickerCurrent;
    $("picker-move").disabled = !pickerCanMoveHere();
    $("picker-up").disabled = pickerLoading || pickerCurrent === "/";
  }

  function renderPickerSkeleton() {
    const list = $("picker-list");
    list.replaceChildren();
    const holder = el("div", "picker-loading");
    for (let index = 0; index < 4; index += 1) {
      const row = el("div", "sk-row");
      row.append(el("div", "sk sk-glyph"));
      const line = el("div", "sk sk-line"); line.style.width = "58%";
      row.append(line);
      holder.append(row);
    }
    list.append(holder);
  }

  async function renderPickerList() {
    const list = $("picker-list");
    const firstLoad = pickerFirstLoad;
    renderPickerSkeleton();
    renderPickerState();
    try {
      const entries = await directoryEntries(pickerCurrent);
      if (pickerResolve === null) return;
      list.replaceChildren();
      const folders = entries.filter((entry) => {
        if (!entry || entry.kind !== "folder") return false;
        const full = joinPath(pickerCurrent, entry.name);
        return full !== pickerSourcePath;
      });
      if (!folders.length) {
        list.append(el("div", "picker-empty", "这里没有子文件夹"));
      }
      folders.forEach((entry) => {
        const full = joinPath(pickerCurrent, entry.name);
        const item = el("button", "picker-item");
        item.type = "button";
        item.setAttribute("role", "listitem");
        item.append(icon("folder"));
        item.append(el("span", "p-name", entry.name));
        item.append(icon("chev-right", "p-chev"));
        item.addEventListener("click", () => {
          pickerCurrent = full;
          renderPickerList();
        });
        list.append(item);
      });
      pickerLoading = false;
      pickerFirstLoad = false;
      renderPickerState();
    } catch (error) {
      pickerLoading = false;
      if (firstLoad && pickerResolve) {
        // 首次读取就失败说明目录接口不可用：关闭选择器，让移动操作
        // 退回手写路径的输入框，功能不至于不可用。
        pickerFirstLoad = false;
        pickerFallback = true;
        closePicker(null);
        return;
      }
      list.replaceChildren(el("div", "picker-empty", "文件夹读取失败，请点击上一级或重试"));
      renderPickerState();
    }
  }

  function closePicker(value) {
    if (!pickerResolve) return null;
    const resolve = pickerResolve;
    pickerResolve = null;
    $("picker").close();
    resolve(value);
  }

  function openFolderPicker(sourcePath) {
    pickerSourcePath = sourcePath;
    pickerCurrent = parentPath(sourcePath);
    pickerLoading = true;
    pickerFirstLoad = true;
    $("picker-subtitle").textContent = `选择 “${sourcePath.split("/").filter(Boolean).pop() || "项目"}” 的目标文件夹`;
    $("picker").showModal();
    renderPickerList();
    return new Promise((resolve) => { pickerResolve = resolve; });
  }

  /* ============ 文件操作 ============ */
  async function createFolder() {
    const name = await openInputModal("新建文件夹", "文件夹名称", "", "例如：项目资料");
    if (!name) return;
    setBusy(true);
    try {
      await api("folders", joinPath(state.path, name), { method: "POST" });
      clearDirectoryCache();
      setStatus("文件夹已创建", "success");
      toast(`文件夹 “${name}” 已创建`, "success");
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
      clearDirectoryCache();
      setStatus("名称已更新", "success");
      toast(`已重命名为 “${name}”`, "success");
      await load(state.path, true, true);
    } catch (error) { showError(error); }
    finally { setBusy(false); renderBreadcrumbs(); }
  }

  async function move(entry, path) {
    pickerFallback = false;
    const picked = await openFolderPicker(path);
    let destination = picked;
    if (pickerFallback) {
      // 选择器无法读取目录：退回手写路径，保证移动功能始终可用。
      destination = await openInputModal("移动项目", "目标文件夹路径", state.path, "例如：/项目资料");
    }
    if (!destination) return;
    setBusy(true);
    try {
      await api("entries", path, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ parent_path: canonicalPath(destination) }) });
      clearDirectoryCache();
      setStatus("项目已移动", "success");
      toast(`“${entry.name}” 已移动到 ${canonicalPath(destination)}`, "success");
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
      clearDirectoryCache();
      setStatus("项目已删除", "success");
      toast(`“${entry.name}” 已删除`, "success");
      await load(state.path, true, true);
    } catch (error) { showError(error); }
    finally { setBusy(false); renderBreadcrumbs(); }
  }

  /* ============ 上传托盘 ============ */
  const tray = { active: false, cancelled: false, xhr: null, files: [], states: [], done: 0 };

  function trayIconWrap(iconName, cls) {
    const wrap = el("span", "t-ic " + cls);
    wrap.append(icon(iconName));
    return wrap;
  }

  function traySetItem(index) {
    const item = $("tray-list").children[index];
    if (!item) return;
    const st = tray.states[index];
    item.className = "tray-item " + st;
    const left = item.children[0];
    const right = item.children[2];
    left.replaceChildren();
    right.replaceChildren();
    if (st === "active") {
      left.append(el("span", "tray-spinner"));
    } else if (st === "pending") {
      left.append(el("span", "tray-dot"));
    } else if (st === "done") {
      left.append(trayIconWrap("check", "ok"));
      right.append(el("span", "", "完成"));
    } else if (st === "error") {
      left.append(trayIconWrap("alert", "bad"));
      right.append(el("span", "", "失败"));
    } else if (st === "skipped") {
      left.append(trayIconWrap("x", ""));
      right.append(el("span", "", "跳过"));
    } else {
      left.append(trayIconWrap("x", ""));
      right.append(el("span", "", "已取消"));
    }
  }

  function trayReset(files) {
    tray.active = true;
    tray.cancelled = false;
    tray.done = 0;
    tray.files = files.slice();
    tray.states = files.map(() => "pending");
    const list = $("tray-list");
    list.replaceChildren();
    files.forEach((file) => {
      const item = el("li", "tray-item pending");
      const left = el("span", "t-state");
      left.append(el("span", "tray-dot"));
      const name = el("span", "t-name", file.name);
      name.title = file.name;
      const right = el("span", "t-state");
      item.append(left, name, right);
      list.append(item);
    });
    list.classList.toggle("has-multi", files.length > 1);
    $("tray-cancel").classList.remove("hidden");
    $("tray-close").classList.add("hidden");
    $("tray-count").textContent = files.length > 1 ? `0 / ${files.length}` : "";
    $("tray-percent").textContent = "0%";
    setRing(0);
    $("upload-tray").classList.add("show");
    $("upload-tray").setAttribute("aria-hidden", "false");
  }

  function setRing(percent) {
    const ring = $("tray-ring");
    ring.style.strokeDashoffset = String(100 - Math.max(0, Math.min(100, percent)));
    $("tray-percent").textContent = `${Math.round(percent)}%`;
  }

  function trayCurrent(file, percent, speedText) {
    $("tray-name").textContent = file.name;
    $("tray-name").title = file.name;
    $("tray-speed").textContent = speedText;
    setRing(percent);
  }

  function trayFinishAll(ok, message) {
    tray.active = false;
    $("tray-cancel").classList.add("hidden");
    $("tray-close").classList.remove("hidden");
    if (ok) {
      $("tray-speed").textContent = message || "全部完成";
      setTimeout(() => {
        if (!tray.active) trayHide();
      }, 2400);
    }
  }

  function trayHide() {
    $("upload-tray").classList.remove("show");
    $("upload-tray").setAttribute("aria-hidden", "true");
  }

  function trayCancel() {
    if (!tray.active) return;
    tray.cancelled = true;
    if (tray.xhr) tray.xhr.abort();
  }

  /* ============ 上传 ============ */
  function uploadOne(file, overwrite) {
    return new Promise((resolve, reject) => {
      const url = pathUrl("upload", joinPath(state.path, file.name));
      if (overwrite) url.searchParams.set("overwrite", "true");
      const startedAt = performance.now();
      const updateSpeed = (loaded) => {
        const elapsed = Math.max((performance.now() - startedAt) / 1000, 0.001);
        return `${formatRate(loaded / elapsed)} · ${formatBytes(loaded)} / ${formatBytes(file.size)}`;
      };
      trayCurrent(file, 0, "正在连接...");
      const xhr = new XMLHttpRequest();
      tray.xhr = xhr;
      xhr.open("PUT", url.toString());
      xhr.withCredentials = true;
      xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");
      xhr.upload.onprogress = (event) => {
        if (!event.lengthComputable) return;
        const percent = event.loaded * 100 / event.total;
        const speedText = updateSpeed(event.loaded);
        setRing(percent);
        trayCurrent(file, percent, speedText);
        setStatus(`正在上传 ${file.name} · ${Math.round(percent)}%`);
      };
      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          trayCurrent(file, 100, updateSpeed(file.size));
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
      xhr.onabort = () => reject(new Error("上传已取消"));
      xhr.send(file);
    });
  }

  async function uploadFiles(files) {
    if (!files.length || state.busy || state.connection !== "connected") return;
    setBusy(true);
    trayReset(files);
    let failure = null;
    try {
      for (let index = 0; index < files.length; index += 1) {
        if (tray.cancelled) {
          for (let rest = index; rest < files.length; rest += 1) {
            tray.states[rest] = "cancelled";
            traySetItem(rest);
          }
          break;
        }
        const file = files[index];
        if (files.length > 1) setStatus(`准备上传第 ${index + 1}/${files.length} 个文件`);
        tray.states[index] = "active";
        traySetItem(index);
        $("tray-count").textContent = files.length > 1 ? `${index} / ${files.length}` : "";
        const existing = state.entries.find((entry) => entry.name === file.name);
        let overwrite = false;
        if (existing) {
          if (existing.kind !== "file") {
            // 同名文件夹无法被文件覆盖：显式跳过而不是静默丢弃。
            tray.states[index] = "skipped";
            traySetItem(index);
            toast(`“${file.name}” 与现有文件夹同名，已跳过`, "warn", 5200);
            continue;
          }
          const confirmed = await openConfirmModal("文件已存在", `“${file.name}”已经存在，要覆盖它吗？`, "覆盖", false);
          if (!confirmed) {
            tray.states[index] = "skipped";
            traySetItem(index);
            continue;
          }
          overwrite = true;
        }
        try {
          await uploadOne(file, overwrite);
          tray.done += 1;
          tray.states[index] = "done";
          traySetItem(index);
          $("tray-count").textContent = files.length > 1 ? `${tray.done} / ${files.length}` : "";
          clearDirectoryCache();
        } catch (error) {
          if (tray.cancelled) {
            tray.states[index] = "cancelled";
          } else {
            tray.states[index] = "error";
            failure = error;
          }
          traySetItem(index);
          break;
        }
      }
      if (tray.cancelled) {
        setStatus("上传已取消");
        toast("上传已取消", "info");
        trayFinishAll(false);
      } else if (failure) {
        showError(failure);
        trayFinishAll(false);
      } else {
        setStatus("上传完成", "success");
        toast(tray.done > 1 ? `已上传 ${tray.done} 个文件` : "上传完成", "success");
        trayFinishAll(true);
        await load(state.path, true, true);
      }
    } catch (error) {
      showError(error);
      trayFinishAll(false);
    } finally {
      setBusy(false);
      renderBreadcrumbs();
    }
  }

  /* ============ 拖放 ============ */
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

  /* ============ 事件绑定 ============ */
  $("login-tab").addEventListener("click", () => switchAuthMode("login"));
  $("register-tab").addEventListener("click", () => switchAuthMode("register"));
  $("login-form").addEventListener("submit", (event) => submitAuth("login", event));
  $("register-form").addEventListener("submit", (event) => submitAuth("register", event));
  $("logout-button").addEventListener("click", logout);

  $("modal-form").addEventListener("submit", (event) => {
    event.preventDefault();
    if (modalMode === "input") closeModal($("modal-input").value.trim());
    else closeModal(true);
  });
  $("modal-cancel").addEventListener("click", () => closeModal(null));
  $("modal").addEventListener("cancel", (event) => { event.preventDefault(); closeModal(null); });

  $("picker-cancel").addEventListener("click", () => closePicker(null));
  $("picker").addEventListener("cancel", (event) => { event.preventDefault(); closePicker(null); });
  $("picker-move").addEventListener("click", () => {
    if (pickerCanMoveHere()) closePicker(pickerCurrent);
  });
  $("picker-up").addEventListener("click", () => {
    if (pickerLoading) return;
    pickerCurrent = parentPath(pickerCurrent);
    renderPickerList();
  });

  $("theme-button").addEventListener("click", cycleTheme);
  $("view-list-button").addEventListener("click", () => setView("list"));
  $("view-grid-button").addEventListener("click", () => setView("grid"));
  $("settings-button").addEventListener("click", changeRootName);
  $("up-button").addEventListener("click", () => load(parentPath(state.path)));
  $("refresh-button").addEventListener("click", () => load(state.path, false, true));
  $("folder-button").addEventListener("click", createFolder);
  $("upload-button").addEventListener("click", () => $("file-input").click());
  $("tray-cancel").addEventListener("click", trayCancel);
  $("tray-close").addEventListener("click", trayHide);

  $("file-input").addEventListener("change", (event) => {
    uploadFiles(Array.from(event.target.files || []));
    event.target.value = "";
  });

  $("search-input").addEventListener("input", (event) => {
    state.search = event.target.value;
    renderEntries({ animate: false });
  });
  $("search-input").addEventListener("keydown", (event) => {
    if (event.key === "Escape" && $("search-input").value) {
      event.stopPropagation();
      state.search = "";
      $("search-input").value = "";
      renderEntries({ animate: false });
    }
  });

  $("sort-select").addEventListener("change", (event) => {
    const key = event.target.value;
    setSort(key, key === "auto" ? "asc" : SORT_DEFAULT_DIR[key] || "asc");
  });
  $("sort-dir-button").addEventListener("click", () => {
    const flipped = state.sort.dir === "desc" ? "asc" : "desc";
    setSort(state.sort.key, flipped);
  });
  document.querySelectorAll("#list-head .sort-button").forEach((button) => {
    button.addEventListener("click", () => {
      const key = button.dataset.sort;
      if (state.sort.key === key) {
        setSort(key, state.sort.dir === "desc" ? "asc" : "desc");
      } else {
        setSort(key, SORT_DEFAULT_DIR[key] || "asc");
      }
    });
  });

  window.addEventListener("keydown", (event) => {
    const dialogOpen = document.querySelector("dialog[open]");
    const tag = document.activeElement ? document.activeElement.tagName : "";
    const typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
    if (event.key === "/" && !dialogOpen && !typing) {
      event.preventDefault();
      $("search-input").focus();
      return;
    }
    if (event.key === "Escape" && !dialogOpen && !typing && state.search) {
      state.search = "";
      $("search-input").value = "";
      renderEntries({ animate: false });
      return;
    }
    if (event.altKey && event.key === "ArrowUp" && !dialogOpen && !typing) {
      if (!$("up-button").disabled) {
        event.preventDefault();
        load(parentPath(state.path));
      }
    }
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

  window.addEventListener("scroll", () => {
    $("app-header").classList.toggle("scrolled", window.scrollY > 4);
  }, { passive: true });

  window.setInterval(async () => {
    if (state.busy || document.hidden) return;
    const previous = state.connection;
    const current = await checkConnection(true);
    if (current !== "connected") {
      state.entries = [];
      renderEntries({ animate: true });
    } else if (previous !== "connected") {
      await load(state.path, true, true);
    }
  }, 30000);

  /* ============ 启动 ============ */
  async function startDrive() {
    applyTheme(false);
    loadSort();
    renderSortControls();
    setView(PREF.get("view", "list"), { animate: false });
    // Fetch the configured root name before the first render so the
    // placeholder never flips to the real name and back.
    await initRootName();
    const initial = hashPath();
    if (location.hash !== "#" + initial) {
      history.replaceState(null, "", "#" + initial);
    }
    renderBreadcrumbs();
    load(initial);
  }

  async function boot() {
    if (await initWebAuth()) await startDrive();
  }
  boot();
})();
