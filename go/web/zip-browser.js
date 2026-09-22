/* Browse one ZIP through bounded server reads. No local filesystem extraction. */
(() => {
  "use strict";
  const node = (tag, text, className) => {
    const element = document.createElement(tag);
    if (text !== undefined) element.textContent = text;
    if (className) element.className = className;
    return element;
  };
  const button = (label, action) => {
    const element = node("button", label);
    element.type = "button";
    element.addEventListener("click", action);
    return element;
  };
  window.WPSZipBrowser = {
    supports: (name) => typeof name === "string" && /\.zip$/i.test(name),
    init(config) {
      const modal = node("dialog", undefined, "zip-browser-dialog");
      modal.id = "zip-browser-modal";
      modal.setAttribute("aria-labelledby", "zip-browser-title");
      const card = node("section", undefined, "modal-card zip-browser-card");
      const title = node("h2", "浏览压缩包", "modal-title");
      title.id = "zip-browser-title";
      const note = node("p", "只读浏览 ZIP，支持下载其中的单个文件。单文件下载最多 128 MiB。", "preview-meta");
      const path = node("p", "/", "preview-meta zip-browser-path");
      path.id = "zip-browser-path";
      const toolbar = node("div", undefined, "zip-browser-toolbar");
      const up = button("上一级", () => load(current.slice(0, current.lastIndexOf("/")) || "/"));
      up.id = "zip-browser-up";
      const refresh = button("刷新", () => load(current));
      refresh.id = "zip-browser-refresh";
      toolbar.append(up, refresh);
      const status = node("p", "", "preview-meta");
      status.id = "zip-browser-status";
      status.setAttribute("role", "status");
      const error = node("p", "", "modal-inline-error");
      error.id = "zip-browser-error";
      error.setAttribute("role", "alert");
      const list = node("div", undefined, "zip-browser-list");
      list.id = "zip-browser-list";
      const footer = node("div", undefined, "modal-actions");
      const close = button("关闭", () => reset());
      close.id = "zip-browser-close";
      footer.append(close);
      card.append(title, note, toolbar, path, status, error, list, footer);
      modal.append(card);
      document.body.append(modal);
      let archive = null, current = "/", controller = null, generation = 0;
      const frames = new Map();
      function endpoint(action, entry) {
        const url = new URL("/api/v1/zip/" + action, location.origin);
        url.searchParams.set("path", archive.path);
        url.searchParams.set("entry", entry);
        return url;
      }
      function reset() {
        generation += 1;
        if (controller) controller.abort();
        controller = null;
        archive = null;
        list.replaceChildren();
        status.textContent = error.textContent = "";
        for (const [frame, timer] of frames) { clearTimeout(timer); frame.remove(); }
        frames.clear();
        if (modal.open) modal.close();
      }
      function showError(failure) {
        if (failure.name === "AbortError") return;
        if (failure.status === 401) { reset(); if (config.onError) config.onError(failure); return; }
        error.textContent = failure.message || "压缩包暂时无法读取，请下载后查看。";
      }
      function download(entry) {
        if (!archive) return;
        if (frames.size >= 4) { error.textContent = "下载请求较多，请稍后再试。"; return; }
        const ticket = generation, href = endpoint("download", entry.path).toString();
        const frame = document.createElement("iframe");
        frame.hidden = true;
        frame.title = "ZIP 文件下载";
        frame.addEventListener("load", () => {
          if (ticket !== generation || !frames.has(frame)) return;
          try {
            const text = frame.contentDocument && frame.contentDocument.body && frame.contentDocument.body.textContent;
            if (text && text.trim()) {
              let message = "下载失败，请刷新压缩包后重试。";
              try { const payload = JSON.parse(text); if (payload.error) message = payload.error; } catch (_) {}
              error.textContent = message;
              clearTimeout(frames.get(frame)); frames.delete(frame); frame.remove();
            }
          } catch (_) { /* Download attachment has no accessible document. */ }
        });
        const expiry = setTimeout(() => { frames.delete(frame); frame.remove(); }, 150000);
        frames.set(frame, expiry);
        document.body.append(frame);
        frame.src = href;
        // The server caps each ZIP read at two minutes. Keep the frame beyond
        // that window, then release it even though attachments have no load event.
        status.textContent = "已请求下载；进度请查看浏览器下载列表。关闭此窗口可能中断下载。";
      }
      async function load(folder) {
        if (!archive) return;
        folder = String(folder).replace(/\/+$/, "") || "/";
        if (controller) controller.abort();
        const active = new AbortController();
        controller = active;
        const ticket = ++generation;
        current = folder;
        path.textContent = folder;
        list.replaceChildren();
        error.textContent = "";
        status.textContent = "正在读取压缩包目录…";
        up.disabled = current === "/";
        refresh.disabled = true;
        try {
          const response = await fetch(endpoint("entries", folder), { credentials: "same-origin", cache: "no-store", signal: active.signal });
          let data;
          try { data = await response.json(); } catch (_) { data = {}; }
          if (!response.ok) { const failure = new Error(data.error || `读取失败（${response.status}）`); failure.status = response.status; throw failure; }
          if (ticket !== generation || !modal.open) return;
          for (const entry of data.entries || []) {
            const row = node("div", undefined, "zip-browser-row");
            const detail = node("div", undefined, "zip-browser-detail");
            const name = node("strong", entry.name);
            const size = entry.kind === "folder" ? "文件夹" : `${Number(entry.size || 0).toLocaleString()} 字节`;
            detail.append(name, node("span", size, "preview-meta"));
            const action = button(entry.kind === "folder" ? "打开" : "下载", () => entry.kind === "folder" ? load(entry.path) : download(entry));
            action.setAttribute("aria-label", `${entry.kind === "folder" ? "打开" : "下载"}：${entry.name}`);
            row.append(detail, action);
            list.append(row);
          }
          status.textContent = data.entries && data.entries.length ? `${data.entries.length} 项 · ZIP 共 ${data.total_entries || 0} 项` : "此目录为空。";
        } catch (failure) {
          if (ticket === generation) { status.textContent = ""; showError(failure); }
        } finally {
          if (controller === active) controller = null;
          if (ticket === generation) refresh.disabled = false;
        }
      }
      modal.addEventListener("cancel", (event) => { event.preventDefault(); reset(); });
      return {
        open(entry, path) {
          reset();
          archive = { entry, path };
          title.textContent = "压缩包：" + entry.name;
          modal.showModal();
          load("/");
        },
        reset,
      };
    },
  };
})();
