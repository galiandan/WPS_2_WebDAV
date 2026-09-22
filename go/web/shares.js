/* Share management keeps newly created bearer URLs in this dialog only. */
(() => {
  "use strict";
  const node = (tag, text = "", className = "") => {
    const item = document.createElement(tag);
    item.textContent = text;
    if (className) item.className = className;
    return item;
  };
  const button = (id, text, action, className = "") => {
    const item = node("button", text, className);
    item.id = id;
    item.type = "button";
    item.addEventListener("click", action);
    return item;
  };
  const field = (text, input) => { const label = node("label", text, "shares-field"); label.append(input); return label; };
  const date = (value) => { const parsed = new Date(value); return Number.isNaN(parsed.getTime()) ? "未知时间" : parsed.toLocaleString("zh-CN"); };
  window.WPSShares = {
    init(config) {
      const dialog = node("dialog", "", "shares-dialog"); dialog.id = "shares-modal";
      dialog.setAttribute("aria-labelledby", "shares-title");
      const card = node("section", "", "modal-card shares-card");
      const heading = node("div", "", "shares-heading");
      const title = node("h2", "我的分享", "modal-title"); title.id = "shares-title";
      heading.append(title, button("shares-close", "关闭", close));
      const note = node("p", "分享文件或文件夹供他人读取。创建后请复制链接；以后可以撤销或重新创建。", "preview-meta");
      const error = node("p", "", "modal-inline-error"); error.id = "shares-error"; error.setAttribute("role", "alert");
      const form = node("form", "", "shares-form"); form.id = "shares-form"; form.hidden = true;
      const source = node("p", "", "shares-source"); source.id = "shares-source";
      const days = node("input"); days.id = "shares-days"; days.type = "number"; days.min = "1"; days.max = "30"; days.step = "1"; days.value = "7"; days.required = true;
      const password = node("input"); password.id = "shares-password"; password.type = "password"; password.autocomplete = "new-password"; password.maxLength = 128;
      const inputs = node("div", "", "shares-inputs"); inputs.append(field("有效天数（1–30 天）", days), field("提取码（可选）", password));
      const create = button("shares-create", "创建分享链接", () => {}, "primary"); create.type = "submit";
      form.append(source, inputs, create);
      const result = node("section", "", "shares-created"); result.id = "shares-created"; result.hidden = true;
      const urlField = node("input"); urlField.id = "shares-created-url"; urlField.readOnly = true; urlField.setAttribute("aria-label", "新创建的分享链接");
      const copied = node("p", "", "preview-meta"); copied.id = "shares-copy-status"; copied.setAttribute("role", "status");
      const copy = button("shares-copy", "复制链接", () => copyURL(), "primary");
      const openLink = button("shares-open", "打开分享", () => { if (createdURL) window.open(createdURL, "_blank", "noopener,noreferrer"); });
      const resultActions = node("div", "", "shares-actions"); resultActions.append(copy, openLink);
      result.append(node("strong", "分享已创建，请保存链接"), urlField, resultActions, copied);
      const tools = node("div", "", "shares-toolbar");
      const refreshButton = button("shares-refresh", "刷新分享", () => refresh());
      const count = node("span", "", "preview-meta"); count.id = "shares-count";
      tools.append(count, refreshButton);
      const list = node("div", "", "shares-list"); list.id = "shares-list";
      card.append(heading, note, error, form, result, tools, list); dialog.append(card); document.body.append(dialog);
      let target = null, records = [], createdURL = "", busy = false, generation = 0, controller = null, confirming = false;
      function clearURL() { createdURL = ""; urlField.value = copied.textContent = ""; result.hidden = true; }
      function update() {
        create.disabled = refreshButton.disabled = days.disabled = password.disabled = busy;
        list.querySelectorAll("button").forEach((item) => { item.disabled = busy; });
      }
      function fail(failure) {
        if (failure.name === "AbortError") return;
        error.textContent = failure.status === 403 ? "当前账号不能分享此项目，或权限已改变。" : failure.message || "分享操作失败，请重试";
        if (failure.status === 401) { reset(); config.onError(failure); }
      }
      function render() {
        count.textContent = records.length ? `${records.length} 个分享记录` : "尚未创建分享";
        list.replaceChildren(...records.map((share) => {
          const item = node("article", "", "share-record"); item.dataset.shareId = share.id;
          const expired = new Date(share.expires_at).getTime() <= Date.now();
          const heading = node("div", "", "share-record-heading");
          heading.append(node("strong", share.name), node("span", share.revoked ? "已撤销" : expired ? "已过期" : "有效", "preview-meta"));
          item.append(heading, node("p", share.path, "share-record-path"), node("p", `到期：${date(share.expires_at)}${share.password_required ? " · 需要提取码" : ""}`, "preview-meta"));
          if (!share.revoked && !expired) item.append(button("", "撤销分享", () => revoke(share), "danger"));
          return item;
        }));
        update();
      }
      async function request(route, options = {}) {
        const epoch = generation;
        const active = new AbortController(); controller = active;
        const data = await config.request(route, { ...options, signal: active.signal });
        if (epoch !== generation) throw new DOMException("分享窗口已关闭", "AbortError");
        return data;
      }
      async function refresh() {
        if (!dialog.open || busy) return;
        const epoch = generation;
        busy = true; update(); error.textContent = "";
        try { const data = await request("shares"); records = Array.isArray(data.shares) ? data.shares : []; render(); }
        catch (failure) { if (epoch === generation) fail(failure); }
        finally { if (epoch === generation) { busy = false; controller = null; update(); } }
      }
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        if (!target || busy || !config.canShare()) return;
        const duration = Number(days.value);
        const length = new TextEncoder().encode(password.value).length;
        if (!Number.isInteger(duration) || duration < 1 || duration > 30) { error.textContent = "有效期须为 1–30 天。"; return; }
        if (password.value && (length < 4 || length > 128)) { error.textContent = "提取码长度须为 4–128 字节。"; return; }
        const body = { path: target.path, expires_at: new Date(Date.now() + duration * 86400000).toISOString(), ...(password.value ? { password: password.value } : {}) };
        const epoch = generation;
        busy = true; update(); clearURL(); error.textContent = "";
        try {
          const data = await request("shares", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
          if (!data || !/^\/share\/[a-f0-9]{32}#[a-f0-9]{64}$/.test(data.url)) throw new Error("链接响应无效，请刷新列表确认分享结果。");
          createdURL = new URL(data.url, location.origin).toString();
          urlField.value = createdURL; result.hidden = false;
          password.value = "";
          records = [data.share, ...records.filter((item) => item.id !== data.share.id)]; render();
          urlField.focus(); urlField.select();
        } catch (failure) { if (epoch === generation) fail(failure); }
        finally { if (epoch === generation) { busy = false; controller = null; update(); } }
      });
      async function copyURL() {
        if (!createdURL) return;
        const epoch = generation;
        try {
          if (!navigator.clipboard || !window.isSecureContext) throw new Error("clipboard unavailable");
          await navigator.clipboard.writeText(createdURL);
          if (epoch === generation) copied.textContent = "链接已复制。";
        } catch (_) {
          if (epoch !== generation) return;
          urlField.focus(); urlField.select();
          let success = false;
          try { success = document.execCommand("copy"); } catch (_) {}
          copied.textContent = success ? "链接已复制。" : "请复制上方已选中的链接。";
        }
      }
      async function revoke(share) {
        if (busy || !config.canShare()) return;
        const epoch = generation;
        confirming = true;
        let approved;
        try { approved = await config.confirm("撤销分享", `撤销“${share.name}”后，其他人将无法再通过此链接访问。`, "撤销", true); }
        finally { confirming = false; }
        if (!approved || epoch !== generation) return;
        busy = true; update(); error.textContent = "";
        try {
          await request("shares/" + encodeURIComponent(share.id), { method: "DELETE" });
          if (createdURL && new URL(createdURL).pathname.endsWith("/" + share.id)) clearURL();
          share.revoked = true; render(); config.notify("分享已撤销", "success");
        } catch (failure) { if (epoch === generation) fail(failure); }
        finally { if (epoch === generation) { busy = false; controller = null; update(); } }
      }
      function close() {
        generation += 1;
        if (controller) controller.abort();
        controller = null; busy = false;
        clearURL(); password.value = ""; target = null; form.hidden = true; source.textContent = "";
        if (confirming) config.dismissConfirm();
        dialog.close();
      }
      function reset() { close(); records = []; list.replaceChildren(); count.textContent = error.textContent = ""; }
      function open(selection = null) {
        if (!config.canShare()) { config.onError(Object.assign(new Error("当前账号没有分享权限"), { status: 403 })); return; }
        if (dialog.open) return;
        target = selection;
        form.hidden = !target;
        source.textContent = target ? `分享${target.entry.kind === "folder" ? "文件夹" : "文件"}：${target.path}` : "";
        days.value = "7"; password.value = ""; error.textContent = ""; clearURL();
        dialog.showModal(); refresh();
      }
      dialog.addEventListener("cancel", (event) => { event.preventDefault(); close(); });
      return { open, reset };
    },
  };
})();
