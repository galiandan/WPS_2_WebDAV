/* Bounded text editing with server-checked content revisions. */
(() => {
  "use strict";
  const make = (tag, text, className) => {
    const element = document.createElement(tag);
    if (text) element.textContent = text;
    if (className) element.className = className;
    return element;
  };
  const button = (id, label, action) => {
    const element = make("button", label);
    element.type = "button";
    element.id = id;
    element.addEventListener("click", action);
    return element;
  };
  window.WPSTextEditor = {
    init(config) {
      const dialog = make("dialog", "", "text-editor-dialog");
      dialog.id = "text-editor-modal";
      dialog.setAttribute("aria-labelledby", "text-editor-title");
      const card = make("section", "", "modal-card text-editor-card");
      const title = make("h2", "编辑文本文件", "modal-title");
      title.id = "text-editor-title";
      const pathLabel = make("p", "", "preview-meta");
      pathLabel.id = "text-editor-path";
      const info = make("p", "最大 2 MiB；保存为 UTF-8。关闭后草稿留在当前页面，刷新或离开页面后丢失。", "preview-meta");
      const controls = make("div", "", "text-editor-controls");
      const encodingLabel = make("label", "读取编码 ");
      const encoding = make("select");
      encoding.id = "text-editor-encoding";
      for (const [key, label] of [["auto", "自动识别"], ["utf-8", "UTF-8"], ["gb18030", "GB18030 / GBK"], ["big5", "Big5"], ["utf-16le", "UTF-16 LE"], ["utf-16be", "UTF-16 BE"]]) {
        const option = make("option", label);
        option.value = key;
        encoding.append(option);
      }
      encodingLabel.append(encoding);
      const reload = button("text-editor-reload", "重新读取", () => load(true));
      controls.append(encodingLabel, reload);
      const textarea = make("textarea");
      textarea.id = "text-editor-content";
      textarea.setAttribute("aria-label", "文件内容");
      textarea.spellcheck = false;
      const status = make("p", "", "preview-meta");
      status.id = "text-editor-status";
      status.setAttribute("role", "status");
      const error = make("p", "", "modal-inline-error");
      error.id = "text-editor-error";
      error.setAttribute("role", "alert");
      const actions = make("div", "", "modal-actions");
      const close = button("text-editor-close", "关闭并保留草稿", () => closeEditor());
      const save = button("text-editor-save", "保存", () => saveFile());
      save.className = "primary";
      actions.append(close, save);
      card.append(title, pathLabel, info, controls, textarea, status, error, actions);
      dialog.append(card);
      document.body.append(dialog);
      const open = button("text-editor-open", "编辑文件", () => openEditor());
      document.querySelector("#preview-modal .modal-actions").prepend(open);
      let draft = null, controller = null, generation = 0, busy = false;

      const dirty = () => draft && draft.text !== draft.original;
      const url = (path) => {
        const endpoint = new URL("/api/v1/text", window.location.origin);
        endpoint.searchParams.set("path", path);
        return endpoint;
      };
      function update() {
        const changed = dirty();
        save.disabled = busy || !draft || !draft.revision || !changed || new TextEncoder().encode(draft.text).length > 2 * 1024 * 1024;
        reload.disabled = busy;
        encoding.disabled = busy || !draft || !draft.bytes;
        textarea.disabled = busy || !draft || !draft.revision;
        close.disabled = busy && controller && controller.saving;
        status.textContent = busy ? "正在处理文件，请稍候…" : changed ? `有未保存的修改 · ${new TextEncoder().encode(draft.text).length.toLocaleString()} 字节` : draft && draft.revision ? "内容与当前读取版本一致" : "";
      }
      function guess(bytes) {
        if (bytes[0] === 0xff && bytes[1] === 0xfe) return "utf-16le";
        if (bytes[0] === 0xfe && bytes[1] === 0xff) return "utf-16be";
        try { new TextDecoder("utf-8", { fatal: true }).decode(bytes); return "utf-8"; }
        catch (_) { return "gb18030"; }
      }
      function decode(bytes) {
        const actual = encoding.value === "auto" ? guess(bytes) : encoding.value;
        const text = new TextDecoder(actual, { fatal: true }).decode(bytes);
        if (/[\u0000-\u0008\u000e-\u001f\u007f]/.test(text)) throw new Error("文件包含二进制或控制字符，无法编辑。请下载查看。");
        return text;
      }
      async function responseError(response) {
        let message = `请求失败（${response.status}）`, code = "";
        try { const data = await response.json(); if (data.error) message = data.error; code = data.code || ""; } catch (_) {}
        if (code === "text_save_uncertain" || code === "text_save_unverified") message = "保存结果未确认，远端可能已改变。草稿已保留，请先复制需要的内容，再重新读取核对。";
        if (response.status === 403) message = "当前账号没有编辑此文件的权限。";
        if (response.status === 412) message = "文件已被修改、替换或删除，未覆盖远端内容。草稿已保留；请复制需要的内容，再重新读取最新版本。";
        if (response.status === 413) message = "文件超过 2 MiB 编辑上限，请下载后编辑。";
        if (response.status === 423) message = "文件正被 WebDAV 客户端锁定，请稍后保存。";
        const failure = new Error(message);
        failure.status = response.status;
        failure.code = code;
        return failure;
      }
      function showError(failure) {
        if (failure.name === "AbortError") return;
        error.textContent = failure.message || "文件操作失败，草稿已保留";
        if (failure.status === 401) {
          if (dialog.open) dialog.close();
          document.getElementById("preview-modal").close();
          if (config.onError) config.onError(failure);
        }
      }
      function closeEditor() {
        if (busy && controller && controller.saving) return;
        generation += 1;
        if (controller) controller.abort();
        controller = null;
        busy = false;
        dialog.close();
      }
      function openEditor() {
        const target = config.getTarget();
        if (!target || !config.canEdit(target.entry)) return;
        if (draft && draft.path !== target.path && dirty() && !window.confirm("另一个文件有未保存的草稿。丢弃该草稿并编辑此文件？")) return;
        const reuse = draft && draft.path === target.path && dirty();
        if (!reuse) draft = { path: target.path, name: target.entry.name, text: "", original: "", revision: null, bytes: null, encoding: "auto" };
        pathLabel.textContent = draft.path;
        title.textContent = "编辑：" + draft.name;
        textarea.value = draft.text;
        encoding.value = draft.encoding;
        error.textContent = "";
        dialog.showModal();
        update();
        if (!reuse) load(false);
        else textarea.focus();
      }
      async function load(confirmDiscard) {
        if (busy || !draft) return;
        if (confirmDiscard && dirty() && !window.confirm("重新读取将丢弃当前草稿，继续？")) return;
        const id = ++generation;
        const active = new AbortController();
        controller = active;
        busy = true;
        error.textContent = "";
        update();
        try {
          const response = await fetch(url(draft.path), { credentials: "same-origin", cache: "no-store", signal: active.signal });
          if (!response.ok) throw await responseError(response);
          const bytes = new Uint8Array(await response.arrayBuffer());
          if (id !== generation) return;
          if (bytes.length > 2 * 1024 * 1024) throw new Error("文件超过编辑大小上限");
          const revision = response.headers.get("ETag");
          if (!revision) throw new Error("无法取得文件版本，请更新服务后重试");
          draft.bytes = bytes;
          draft.readRevision = revision;
          draft.revision = null;
          draft.text = draft.original = "";
          textarea.value = "";
          const text = decode(bytes);
          draft.text = draft.original = text;
          draft.revision = revision;
          draft.encoding = encoding.value;
          textarea.value = text;
        } catch (failure) { if (id === generation) showError(failure); }
        finally { if (id === generation) { busy = false; controller = null; update(); } }
      }
      async function saveFile() {
        if (save.disabled || !draft) return;
        if (!config.canEdit({ name: draft.name })) { error.textContent = "当前账号没有编辑文件的权限"; return; }
        const id = ++generation;
        const active = new AbortController();
        active.saving = true;
        controller = active;
        const bytes = new TextEncoder().encode(draft.text);
        busy = true;
        error.textContent = "";
        update();
        try {
          const response = await fetch(url(draft.path), { method: "PUT", credentials: "same-origin", cache: "no-store", signal: active.signal,
            headers: { "Content-Type": "text/plain; charset=utf-8", "If-Match": draft.revision }, body: bytes });
          if (!response.ok) throw await responseError(response);
          const result = await response.json();
          if (id !== generation) return;
          const revision = response.headers.get("ETag") || result.revision;
          if (!revision) throw new Error("文件可能已保存，但响应缺少新版本。请保留草稿并重新读取确认。");
          draft.original = draft.text;
          draft.revision = draft.readRevision = revision;
          draft.bytes = bytes;
          draft.encoding = encoding.value = "utf-8";
          if (config.onSaved) {
            try { await config.onSaved(draft.path, result.entry, draft.text); }
            catch (_) { error.textContent = "文件已保存，但目录刷新失败，请手动刷新。"; }
          }
        } catch (failure) {
          if (id === generation) {
            if (!failure.status && failure.name !== "AbortError") failure.message = "保存结果未确认，远端可能已改变。草稿已保留，请重新读取核对。";
            showError(failure);
          }
        }
        finally { if (id === generation) { busy = false; controller = null; update(); if (!dirty() && draft.revision) status.textContent = "已保存为 UTF-8"; } }
      }
      textarea.addEventListener("input", () => { if (draft) { draft.text = textarea.value; update(); } });
      encoding.addEventListener("change", () => {
        if (!draft || !draft.bytes) return;
        if (dirty() && !window.confirm("切换编码将重新解码原文件并丢弃草稿，继续？")) { encoding.value = draft.encoding; return; }
        try {
          const text = decode(draft.bytes);
          draft.text = draft.original = text;
          draft.revision = draft.readRevision;
          draft.encoding = encoding.value;
          textarea.value = text;
          error.textContent = "";
        } catch (failure) { encoding.value = draft.encoding; showError(failure); }
        update();
      });
      dialog.addEventListener("cancel", (event) => { event.preventDefault(); closeEditor(); });
      dialog.addEventListener("keydown", (event) => {
        if ((event.ctrlKey || event.metaKey) && event.key === "s") { event.preventDefault(); saveFile(); }
      });
      window.addEventListener("beforeunload", (event) => { if (dirty()) { event.preventDefault(); event.returnValue = ""; } });
      const preview = document.getElementById("preview-modal");
      new MutationObserver(() => {
        const target = config.getTarget();
        open.hidden = !preview.open || !target || !config.canEdit(target.entry);
      }).observe(preview, { attributes: true, attributeFilter: ["open"] });
      open.hidden = true;
      function suspend() {
        generation += 1;
        if (controller) controller.abort();
        controller = null; busy = false;
        textarea.value = pathLabel.textContent = error.textContent = status.textContent = "";
        title.textContent = "编辑文本文件";
        dialog.close();
        open.hidden = true;
      }
      return {
        suspend,
        reset() { suspend(); draft = null; update(); },
      };
    },
  };
})();
