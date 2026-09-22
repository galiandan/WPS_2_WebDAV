/* Creates server-side URL transfers without retaining source URLs locally. */
(() => {
  "use strict";
  const node = (tag, text = "", className = "") => { const item = document.createElement(tag); item.textContent = text; if (className) item.className = className; return item; };
  const button = (id, text, action) => { const item = node("button", text); item.type = "button"; item.id = id; item.addEventListener("click", action); return item; };
  const field = (label, input) => { const item = node("label", label, "transfer-field"); item.append(input); return item; };
  window.WPSTransfers = {
    init(config) {
      const dialog = node("dialog", "", "transfer-dialog"); dialog.id = "transfer-modal"; dialog.setAttribute("aria-labelledby", "transfer-title");
      const form = node("form", "", "modal-card transfer-form");
      const title = node("h2", "离线下载", "modal-title"); title.id = "transfer-title";
      const note = node("p", "服务器下载公开 HTTP/HTTPS 地址中的文件后上传到当前目录。关闭页面后任务仍会继续，同名文件不会覆盖。", "preview-meta");
      const destination = node("p", "", "transfer-destination"); destination.id = "transfer-destination";
      const url = node("input"); url.id = "transfer-url"; url.type = "url"; url.required = true; url.autocomplete = "off"; url.spellcheck = false; url.maxLength = 8192; url.placeholder = "https://example.com/file.zip";
      const name = node("input"); name.id = "transfer-name"; name.type = "text"; name.required = true; name.autocomplete = "off"; name.maxLength = 255; name.placeholder = "保存的文件名（含扩展名）";
      const error = node("p", "", "modal-inline-error"); error.id = "transfer-error"; error.setAttribute("role", "alert");
      const cancel = button("transfer-cancel", "取消", close);
      const submit = button("transfer-submit", "开始下载", () => {}); submit.type = "submit"; submit.className = "primary";
      const actions = node("div", "", "modal-actions"); actions.append(cancel, submit);
      form.append(title, note, destination, field("下载地址", url), field("保存文件名", name), error, actions); dialog.append(form); document.body.append(dialog);
      let target = null, generation = 0, busy = false;
      function close() { generation += 1; busy = false; target = null; url.value = name.value = error.textContent = destination.textContent = ""; dialog.close(); update(); }
      function update() { submit.disabled = cancel.disabled = url.disabled = name.disabled = busy; }
      function open(path) {
        if (!config.canFetch()) { config.onError(Object.assign(new Error("当前账号没有上传权限"), { status: 403 })); return; }
        if (dialog.open) return;
        target = path; destination.textContent = "保存到：" + path; error.textContent = ""; update(); dialog.showModal(); url.focus();
      }
      url.addEventListener("change", () => {
        if (name.value.trim()) return;
        try {
          const parsed = new URL(url.value.trim());
          const last = parsed.pathname.split("/").filter(Boolean).pop();
          const suggested = last ? decodeURIComponent(last) : "";
          if (suggested && !/[\\/\u0000-\u001f\u007f]/.test(suggested) && suggested !== "." && suggested !== "..") name.value = suggested.slice(0, 255);
        } catch (_) { /* Keep explicit filename entry available for every URL. */ }
      });
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        if (busy || target === null) return;
        if (!config.canFetch()) { error.textContent = "当前账号没有上传权限。"; return; }
        const filename = name.value.trim();
        const source = url.value.trim();
        let parsed;
        try { parsed = new URL(source); } catch (_) { error.textContent = "请输入完整的 HTTP 或 HTTPS 下载地址。"; return; }
        if (!["http:", "https:"].includes(parsed.protocol) || parsed.username || parsed.password || parsed.hash) { error.textContent = "下载地址须为 HTTP 或 HTTPS，不能包含账号密码或片段。"; return; }
        if (!filename || filename === "." || filename === ".." || /[\\/\u0000-\u001f\u007f]/.test(filename) || new TextEncoder().encode(filename).length > 255) { error.textContent = "请输入有效文件名，不能包含路径分隔符，且不超过 255 字节。"; return; }
        const path = (target === "/" ? "" : target) + "/" + filename;
        const epoch = generation;
        busy = true; update(); error.textContent = "";
        try {
          await config.enqueue({ kind: "fetch", url: source, destination: path });
          if (epoch !== generation) return;
          close();
        } catch (failure) {
          if (epoch !== generation || failure.name === "AbortError") return;
          error.textContent = ({ 400: "下载地址或文件名不符合要求，请检查后重试。", 403: "当前账号无法向此目录下载文件，或权限已改变。", 409: "同名文件已存在或目标位置已改变，请检查保存目录。", 413: "任务超过服务大小限制。", 429: "服务器任务繁忙，请稍后重试。" })[failure.status] || "任务提交未确认。请先在任务中心检查记录，再决定是否重新提交。";
          if (failure.status === 401) { close(); config.onError(failure); }
        } finally { if (epoch === generation) { busy = false; update(); } }
      });
      dialog.addEventListener("cancel", (event) => { event.preventDefault(); if (!busy) close(); });
      return { open, reset: close };
    },
  };
})();
