/* Anonymous share viewer: uses only the narrow public grant endpoints. */
(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const match = location.pathname.match(/^\/share\/([a-f0-9]{32})\/?$/);
  const shareID = match && match[1];
  let bearer = /^[a-f0-9]{64}$/.test(location.hash.slice(1)) ? location.hash.slice(1) : "";
  let share = null, currentPath = "/", grantExpires = 0, generation = 0, previewGeneration = 0;
  let controller = null, previewController = null, expiryTimer = null, checkTimer = null;
  let unlocking = false, grantCheck = null, previewTarget = null;
  const unavailable = "分享暂时无法访问，请检查链接和提取码，或联系分享者。";
  const node = (tag, text = "", className = "") => { const item = document.createElement(tag); item.textContent = text; if (className) item.className = className; return item; };
  const button = (text, action, className = "") => { const item = node("button", text, className); item.type = "button"; item.addEventListener("click", action); return item; };
  const textName = (name) => /\.(txt|log|md|markdown|csv|json|xml|ya?ml|ini|conf|toml|[cm]?js|jsx|ts|tsx|go|py|sh|bash|css|html?|sql|rs|java|c|h|cpp|hpp|diff|patch)$/i.test(name);
  const imageName = (name) => /\.(jpe?g|png|gif|webp|avif|bmp|ico)$/i.test(name);
  const videoName = (name) => /\.(mp4|m4v|webm|ogv)$/i.test(name);
  const audioName = (name) => /\.(mp3|m4a|aac|ogg|oga|wav|flac)$/i.test(name);
  const previewable = (name) => textName(name) || imageName(name) || videoName(name) || audioName(name) || /\.pdf$/i.test(name);
  function safePath(path) {
    return typeof path === "string" && path.startsWith("/") && path.length <= 4096 && !/[\\\u0000-\u001f\u007f]/.test(path) && (path === "/" || path.slice(1).split("/").every((part) => part && part !== "." && part !== ".."));
  }
  function endpoint(route, path = null) {
    const url = new URL(`/api/share/${shareID}/${route}`, location.origin);
    if (path !== null) {
      if (!safePath(path)) throw new Error(unavailable);
      url.searchParams.set("path", path);
    }
    return url;
  }
  function status(message, error = false) { $("share-status").textContent = message; $("share-status").classList.toggle("error", error); }
  function closePreview() {
    previewGeneration += 1;
    if (previewController) previewController.abort();
    previewController = null; previewTarget = null;
    for (const media of $("share-preview-content").querySelectorAll("img,iframe,audio,video")) {
      media.onload = media.onerror = media.onloadedmetadata = null;
      if (media.pause) media.pause();
      media.removeAttribute("src");
      if (media.load) media.load();
    }
    $("share-preview-content").replaceChildren(); $("share-preview-title").textContent = $("share-preview-state").textContent = "";
    $("share-preview").close();
  }
  function clearContent(message = unavailable) {
    generation += 1;
    if (controller) controller.abort();
    controller = null;
    clearTimeout(expiryTimer); clearTimeout(checkTimer);
    expiryTimer = checkTimer = null;
    closePreview(); share = null;
    $("share-list").replaceChildren(); $("share-breadcrumbs").replaceChildren();
    $("share-content").hidden = true; $("share-empty").hidden = true;
    $("share-title").textContent = "文件分享"; document.title = "文件分享 · WPS Drive";
    status(message, true);
  }
  async function request(route, options = {}) {
    const response = await fetch(endpoint(route), { credentials: "same-origin", cache: "no-store", ...options });
    if (!response.ok) throw Object.assign(new Error(unavailable), { status: response.status });
    return response.json();
  }
  function scheduleCheck() {
    clearTimeout(checkTimer);
    if (share && !document.hidden) checkTimer = setTimeout(() => checkGrant(), 30000);
  }
  function acceptInfo(data) {
    if (!data || !data.share || data.share.id !== shareID || !["file", "folder"].includes(data.share.kind) || typeof data.share.name !== "string") throw new Error(unavailable);
    const expiry = new Date(data.share.expires_at).getTime();
    const grant = data.grant_expires_at ? new Date(data.grant_expires_at).getTime() : expiry;
    if (!Number.isFinite(expiry) || !Number.isFinite(grant) || Math.min(expiry, grant) <= Date.now()) throw new Error(unavailable);
    share = data.share; grantExpires = Math.min(expiry, grant);
    $("share-title").textContent = share.name; document.title = share.name + " · 文件分享";
    status(`有效至 ${new Date(expiry).toLocaleString("zh-CN")}`);
    clearTimeout(expiryTimer);
    expiryTimer = setTimeout(() => clearContent("本次访问已到期，请重新打开原分享链接。"), Math.min(2147483647, Math.max(0, grantExpires - Date.now())));
    scheduleCheck();
  }
  async function checkGrant() {
    if (!share) return false;
    if (grantCheck) return grantCheck;
    if (Date.now() >= grantExpires) { clearContent("本次访问已到期，请重新打开原分享链接。"); return false; }
    const epoch = generation;
    const pending = (async () => {
      try {
        const info = await request("info");
        if (epoch !== generation) return false;
        acceptInfo(info); return true;
      } catch (error) {
        if (epoch === generation) clearContent();
        return false;
      }
    })();
    grantCheck = pending;
    try { return await pending; }
    finally { if (grantCheck === pending) grantCheck = null; scheduleCheck(); }
  }
  function breadcrumbs(path) {
    const parts = path === "/" ? [] : path.slice(1).split("/");
    const nav = $("share-breadcrumbs"); nav.replaceChildren(button(share.name, () => loadDirectory("/")));
    parts.forEach((part, index) => { nav.append(node("span", "/"), button(part, () => loadDirectory("/" + parts.slice(0, index + 1).join("/")))); });
  }
  function fileRow(entry, path) {
    const row = node("article", "", "share-row");
    const description = node("div", "", "share-file");
    if (entry.kind === "folder") description.append(button(entry.name, () => loadDirectory(path), "share-name"));
    else description.append(node("p", entry.name, "share-name"));
    const bytes = Number(entry.size);
    description.append(node("p", entry.kind === "folder" ? "文件夹" : Number.isFinite(bytes) ? `${bytes.toLocaleString()} 字节` : "文件", "share-meta"));
    const actions = node("div", "", "share-actions");
    if (entry.kind === "file") {
      if (previewable(entry.name)) actions.append(button("在线预览", () => openPreview(entry, path)));
      actions.append(button("下载", () => download(entry, path), "primary"));
    }
    row.append(description, actions); return row;
  }
  async function loadDirectory(path) {
    if (!share || share.kind !== "folder" || !safePath(path)) return;
    const epoch = ++generation;
    closePreview();
    if (controller) controller.abort();
    const active = new AbortController(); controller = active;
    $("share-refresh").disabled = true;
    $("share-list").replaceChildren(node("p", "正在读取目录…")); $("share-empty").hidden = true;
    try {
      const response = await fetch(endpoint("entries", path), { credentials: "same-origin", cache: "no-store", signal: active.signal });
      if (!response.ok) throw Object.assign(new Error(unavailable), { status: response.status });
      const data = await response.json();
      if (epoch !== generation) return;
      if (!Array.isArray(data.entries)) throw new Error(unavailable);
      const entries = data.entries.filter((entry) => entry && typeof entry.name === "string" && !/[\/\\\u0000-\u001f\u007f]/.test(entry.name) && entry.name !== "." && entry.name !== ".." && ["file", "folder"].includes(entry.kind));
      const rows = entries.map((entry) => {
        const fullPath = (path === "/" ? "" : path) + "/" + entry.name;
        return fileRow(entry, fullPath);
      });
      currentPath = path; breadcrumbs(path);
      $("share-content").hidden = false;
      $("share-list").replaceChildren(...rows); $("share-empty").hidden = entries.length > 0;
    } catch (error) { if (epoch === generation && error.name !== "AbortError") clearContent(); }
    finally { if (epoch === generation) { controller = null; $("share-refresh").disabled = false; } }
  }
  async function download(entry, path) {
    if (!(await checkGrant())) return;
    const link = document.createElement("a"); link.href = endpoint("download", path).toString(); link.rel = "noreferrer";
    document.body.append(link); link.click(); link.remove();
  }
  async function openPreview(entry, path) {
    if (!(await checkGrant())) return;
    closePreview();
    const epoch = previewGeneration;
    const active = new AbortController(); previewController = active;
    previewTarget = { entry, path };
    $("share-preview-title").textContent = entry.name;
    $("share-preview-state").textContent = "正在读取预览…";
    $("share-preview").showModal();
    const holder = $("share-preview-content");
    if (textName(entry.name)) {
      try {
        const response = await fetch(endpoint("preview", path), { credentials: "same-origin", cache: "no-store", signal: active.signal });
        if (!response.ok) throw Object.assign(new Error(unavailable), { status: response.status });
        const bytes = new Uint8Array(await response.arrayBuffer());
        if (epoch !== previewGeneration) return;
        if (bytes.length > 2 * 1024 * 1024) throw new Error("文件较大，请下载查看。");
        let encoding = "utf-8";
        if (bytes[0] === 255 && bytes[1] === 254) encoding = "utf-16le";
        else if (bytes[0] === 254 && bytes[1] === 255) encoding = "utf-16be";
        else { try { new TextDecoder("utf-8", { fatal: true }).decode(bytes); } catch (_) { encoding = "gb18030"; } }
        const text = new TextDecoder(encoding).decode(bytes);
        if (/[\u0000-\u0008\u000e-\u001f\u007f]/.test(text)) throw new Error("此文件无法作为文本预览，请下载查看。");
        holder.append(node("pre", text));
        $("share-preview-state").textContent = response.headers.get("X-Preview-Truncated") === "true" ? "仅显示文件开头部分，完整内容请下载查看。" : "";
      } catch (error) {
        if (epoch !== previewGeneration || error.name === "AbortError") return;
        if (error.status === 401 || error.status === 403 || error.status === 404) clearContent();
        else $("share-preview-state").textContent = "预览不可用，请下载查看。";
      }
      return;
    }
    const media = document.createElement(imageName(entry.name) ? "img" : videoName(entry.name) ? "video" : audioName(entry.name) ? "audio" : "iframe");
    media.setAttribute(media.tagName === "IMG" ? "alt" : "title", entry.name);
    if (media.tagName === "VIDEO" || media.tagName === "AUDIO") { media.controls = true; media.preload = "metadata"; media.playsInline = true; }
    media.onload = media.onloadedmetadata = () => {
      if (epoch === previewGeneration) $("share-preview-state").textContent = media.tagName === "IFRAME" ? "使用浏览器 PDF 阅读器；若未显示内容，请下载查看。" : "";
    };
    media.onerror = () => {
      if (epoch !== previewGeneration) return;
      $("share-preview-state").textContent = "浏览器无法预览此文件，请下载查看。";
      checkGrant();
    };
    holder.append(media); media.src = endpoint("preview", path).toString();
  }
  function showContent() {
    $("share-unlock").hidden = true; $("share-password").value = "";
    $("share-content").hidden = false;
    if (share.kind === "folder") loadDirectory("/");
    else { currentPath = "/"; breadcrumbs("/"); $("share-list").replaceChildren(fileRow(share, "/")); $("share-empty").hidden = true; }
  }
  async function unlock() {
    if (unlocking || !shareID || !bearer) return;
    unlocking = true; $("share-unlock-submit").disabled = true; status("正在打开分享…");
    const epoch = generation;
    try {
      const info = await request("unlock", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ token: bearer, ...($("share-password").value ? { password: $("share-password").value } : {}) }) });
      if (epoch !== generation) return;
      acceptInfo(info);
      history.replaceState(null, "", location.pathname + location.search);
      bearer = ""; showContent();
    } catch (error) {
      if (epoch !== generation) return;
      status(unavailable, true); $("share-unlock").hidden = false; $("share-password").value = ""; $("share-password").focus();
    } finally { if (epoch === generation) { unlocking = false; $("share-unlock-submit").disabled = false; } }
  }
  $("share-unlock").addEventListener("submit", (event) => { event.preventDefault(); unlock(); });
  $("share-refresh").addEventListener("click", async () => { if (await checkGrant()) showContentAtCurrentPath(); });
  function showContentAtCurrentPath() { if (share.kind === "folder") loadDirectory(currentPath); else showContent(); }
  $("share-preview-close").addEventListener("click", closePreview);
  $("share-preview").addEventListener("cancel", (event) => { event.preventDefault(); closePreview(); });
  $("share-preview-download").addEventListener("click", () => { if (previewTarget) download(previewTarget.entry, previewTarget.path); });
  document.addEventListener("visibilitychange", () => { if (document.hidden) { clearTimeout(checkTimer); } else if (share) checkGrant(); });
  window.addEventListener("pagehide", () => { bearer = ""; clearContent(); });
  function restoreGrant() {
    const epoch = generation;
    request("info").then((info) => { if (epoch === generation) { acceptInfo(info); showContent(); } })
      .catch(() => { if (epoch === generation) clearContent("请重新打开完整的分享链接；链接也可能已过期或撤销。"); });
  }
  window.addEventListener("pageshow", (event) => { if (event.persisted && shareID) restoreGrant(); });
  window.addEventListener("hashchange", () => {
    const token = location.hash.slice(1);
    if (!shareID || !/^[a-f0-9]{64}$/.test(token)) return;
    clearContent();
    bearer = token; unlocking = false;
    unlock();
  });
  if (!shareID) { clearContent(); return; }
  if (bearer) unlock();
  else restoreGrant();
})();
