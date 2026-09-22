/* Global filename search. Uses the adapter's bounded directory index only. */
(() => {
  "use strict";
  function node(tag, text, className) {
    const value = document.createElement(tag);
    if (text) value.textContent = text;
    if (className) value.className = className;
    return value;
  }
  function button(text, id, action) {
    const value = node("button", text);
    value.type = "button";
    if (id) value.id = id;
    if (action) value.addEventListener("click", action);
    return value;
  }
  function select(id, choices) {
    const value = node("select");
    value.id = id;
    for (const [key, label] of choices) {
      const option = node("option", label);
      option.value = key;
      value.append(option);
    }
    return value;
  }
  function field(text, input) {
    const label = node("label", text);
    label.append(input);
    return label;
  }
  window.WPSGlobalSearch = {
    init(config) {
      const dialog = node("dialog", "", "global-search-dialog");
      dialog.id = "global-search-modal";
      dialog.setAttribute("aria-labelledby", "global-search-title");
      const card = node("section", "", "modal-card global-search-card");
      const title = node("h2", "全局文件搜索", "modal-title");
      title.id = "global-search-title";
      const form = node("form", "", "global-search-form");
      const query = node("input");
      query.id = "global-search-query";
      query.type = "search";
      query.placeholder = "输入文件名或路径关键词";
      query.maxLength = 256;
      query.autocomplete = "off";
      const scope = select("global-search-scope", [["/", "所有已选空间"]]);
      const type = select("global-search-type", [["all", "所有类型"], ["file", "文件"], ["folder", "文件夹"], ["image", "图片"], ["document", "文档"], ["video", "视频"], ["audio", "音频"]]);
      const match = select("global-search-match", [["name", "文件名"], ["path", "完整路径"]]);
      const submit = button("搜索", "global-search-submit");
      submit.type = "submit";
      submit.className = "primary";
      form.append(field("关键词", query), field("范围", scope), field("类型", type), field("匹配", match), submit);
      const tools = node("div", "", "global-search-tools");
      const refresh = button("建立 / 更新索引", "global-search-refresh", () => refreshIndex());
      const cancel = button("停止扫描", "global-search-cancel", () => cancelIndex());
      cancel.hidden = true;
      tools.append(refresh, cancel);
      const status = node("p", "", "preview-meta");
      status.id = "global-search-status";
      status.setAttribute("role", "status");
      const error = node("p", "", "modal-inline-error");
      error.id = "global-search-error";
      error.setAttribute("role", "alert");
      const results = node("div", "", "global-search-results");
      results.id = "global-search-results";
      const pageControls = node("div", "", "modal-actions global-search-pages");
      const previous = button("上一页", "global-search-previous", () => { offset = Math.max(0, offset - 100); search(); });
      const next = button("下一页", "global-search-next", () => { offset += 100; search(); });
      const count = node("span", "", "preview-meta");
      const close = button("关闭", "global-search-close", () => closeDialog());
      pageControls.append(previous, count, next, close);
      card.append(title, form, tools, status, error, results, pageControls);
      dialog.append(card);
      document.body.append(dialog);
      const openButton = button("全局搜索", "global-search-button", () => openDialog());
      openButton.title = "搜索所选空间的文件和文件夹";
      document.querySelector(".header-actions .search").after(openButton);
      let offset = 0, timer = null, controller = null, generation = 0;
      let actionController = null, lifecycle = 0;
      let pending = false, lastIndex = null;

      function stopRequest() {
        clearTimeout(timer);
        if (controller) controller.abort();
        controller = null;
        generation += 1;
      }
      function closeDialog() {
        stopRequest();
        lifecycle += 1;
        if (actionController) actionController.abort();
        actionController = null;
        pending = false;
        resetDisplay();
        dialog.close();
      }
      function clearResults() {
        results.replaceChildren();
        previous.disabled = next.disabled = true;
        count.textContent = "";
      }
      function resetDisplay() {
        clearResults();
        error.textContent = status.textContent = "";
        cancel.hidden = true;
        cancel.disabled = false;
        refresh.disabled = false;
        lastIndex = null;
      }
      function openDialog() {
        if (dialog.open) return;
        lifecycle += 1;
        resetDisplay();
        scope.replaceChildren();
        const all = node("option", config.allScopeLabel ? config.allScopeLabel() : "所有已选空间");
        all.value = "/";
        scope.append(all);
        const current = config.getPath();
        for (const space of config.getSpaces()) {
          const option = node("option", space.name);
          option.value = space.path;
          scope.append(option);
          if (current === space.path || current.startsWith(space.path + "/")) scope.value = space.path;
        }
        offset = 0;
        dialog.showModal();
        query.focus();
        search();
      }
      async function request(method, suffix, params, signal) {
        const url = new URL("/api/v1/" + suffix, window.location.origin);
        for (const [key, value] of Object.entries(params || {})) url.searchParams.set(key, value);
        const response = await fetch(url, { method, credentials: "same-origin", cache: "no-store", signal });
        let payload = null;
        try { payload = JSON.parse(await response.text()); } catch (_) { /* Preserve the HTTP status for proxy errors. */ }
        if (!response.ok) {
          const failure = new Error(payload && payload.error || `请求失败（${response.status}）`);
          failure.status = response.status;
          throw failure;
        }
        if (!payload || typeof payload !== "object" || !payload.index) throw new Error("搜索返回格式异常，请重试");
        return payload;
      }
      function displayError(failure) {
        if (failure.name === "AbortError") return;
        if (failure.status === 401) {
          closeDialog();
          if (config.onError) config.onError(failure);
          return;
        }
        error.textContent = failure.message || "搜索失败，请稍后重试";
      }
      function render(data) {
        const index = data.index || {};
        lastIndex = index;
        const state = index.state;
        const scanning = (state === "indexing" || state === "cancelling");
        cancel.hidden = !scanning;
        refresh.disabled = scanning || pending;
        const descriptions = { idle: "尚未建立索引，请点击建立 / 更新索引", indexing: "正在扫描目录，可先查看已找到的结果", cancelling: "正在停止扫描，请等待当前目录请求结束", ready: "扫描完成", canceled: "扫描已停止，结果可能不完整", cancelled: "扫描已停止，结果可能不完整", stale: "文件或账号已变化，请更新索引", failed: "扫描未完成，请重试", partial: "扫描未完成，结果可能不完整" };
        const note = descriptions[state] || (index.complete ? "扫描完成" : "索引不完整，请更新索引");
        const updated = index.updated_at ? new Date(typeof index.updated_at === "number" ? index.updated_at * 1000 : index.updated_at).toLocaleString() : "";
        const reasons = { root_unavailable: "无法读取索引根目录", listing_failed: "部分目录读取失败", invalid_entry: "部分目录条目无效", ambiguous_path: "发现同名路径", entry_limit: "已达到条目数量上限", folder_limit: "已达到目录数量上限", depth_limit: "已达到目录深度上限", metadata_limit: "已达到索引内存上限", repeated_folder: "发现重复目录", time_limit: "已达到扫描时长上限", cancelled: "用户已停止扫描" };
        status.textContent = `${note} · 已收录 ${index.entries || 0} 项${index.path ? " · 索引范围：" + index.path : ""}${updated ? " · 更新：" + updated : ""}${index.reason ? " · " + (reasons[index.reason] || "需要重新扫描") : ""}`;
        if (data.scope_covered === false && state !== "idle") status.textContent += " · 当前范围未被完整索引，请更新索引";
        results.replaceChildren();
        for (const result of data.results || []) {
          const entry = result.entry;
          if (!entry || typeof result.path !== "string") continue;
          const row = node("div", "", "global-search-result");
          const detail = node("div", "", "global-search-detail");
          const name = node("strong", entry.name);
          const path = node("span", result.path, "preview-meta");
          detail.append(name, path);
          const actions = node("div", "", "global-search-result-actions");
          actions.append(button(entry.kind === "folder" ? "打开" : "所在目录", "", () => {
            closeDialog();
            config.navigate(entry.kind === "folder" ? result.path : result.path.slice(0, result.path.lastIndexOf("/")) || "/");
          }));
          if (config.canPreview(entry)) actions.append(button("预览", "", () => { closeDialog(); config.preview(entry, result.path); }));
          if (entry.kind === "file") actions.append(button("下载", "", () => config.download(entry, result.path)));
          row.append(detail, actions);
          results.append(row);
        }
        if (!results.childElementCount) results.append(node("p", index.complete && data.scope_covered !== false ? "没有匹配的文件或文件夹" : "当前已扫描部分没有匹配结果；完整结果需要完成索引。", "preview-meta"));
        previous.disabled = offset === 0;
        next.disabled = !data.has_more;
        count.textContent = `共 ${data.total || 0} 项 · 第 ${Math.floor(offset / 100) + 1} 页`;
        if (scanning && dialog.open) timer = setTimeout(() => search(true), 1500);
      }
      async function search(poll = false) {
        stopRequest();
        const current = generation;
        const currentController = new AbortController();
        controller = currentController;
        error.textContent = "";
        if (!poll) {
          clearResults();
          status.textContent = "正在查询索引…";
        }
        try {
          if (new TextEncoder().encode(query.value.trim()).length > 256) throw new Error("关键词过长，请缩短后重试");
          const data = await request("GET", "search", { q: query.value.trim(), path: scope.value, type: type.value, match: match.value, offset, limit: 100 }, currentController.signal);
          if (generation !== current || !dialog.open) return;
          render(data);
        } catch (failure) {
          if (generation === current && failure.name !== "AbortError") {
            clearResults();
            status.textContent = "查询未完成，请重试";
            displayError(failure);
          }
        } finally {
          if (controller === currentController) controller = null;
        }
      }
      async function refreshIndex() {
        if (pending) return;
        stopRequest();
        const currentLifecycle = lifecycle;
        const currentController = new AbortController();
        actionController = currentController;
        pending = true;
        refresh.disabled = true;
        error.textContent = "";
        try {
          await request("POST", "search/refresh", { path: scope.value }, currentController.signal);
          if (lifecycle !== currentLifecycle || !dialog.open) return;
          offset = 0;
          pending = false;
          await search();
        } catch (failure) {
          if (lifecycle !== currentLifecycle || !dialog.open) return;
          pending = false;
          if (failure.status === 409) await search();
          else displayError(failure);
        } finally {
          if (lifecycle === currentLifecycle) {
            pending = false;
            refresh.disabled = Boolean(lastIndex && (lastIndex.state === "indexing" || lastIndex.state === "cancelling"));
          }
          if (actionController === currentController) actionController = null;
        }
      }
      async function cancelIndex() {
        if (pending) return;
        const currentLifecycle = lifecycle;
        const currentController = new AbortController();
        actionController = currentController;
        pending = true;
        cancel.disabled = true;
        try {
          await request("DELETE", "search", null, currentController.signal);
          if (lifecycle !== currentLifecycle || !dialog.open) return;
          pending = false;
          await search();
        } catch (failure) {
          if (lifecycle === currentLifecycle && dialog.open) displayError(failure);
        } finally {
          if (lifecycle === currentLifecycle) {
            pending = false;
            cancel.disabled = false;
          }
          if (actionController === currentController) actionController = null;
        }
      }
      form.addEventListener("submit", (event) => { event.preventDefault(); offset = 0; search(); });
      for (const control of [scope, type, match]) control.addEventListener("change", () => { offset = 0; search(); });
      dialog.addEventListener("keydown", (event) => {
        if (event.key !== "Escape") return;
        event.preventDefault();
        event.stopPropagation();
        closeDialog();
      });
      dialog.addEventListener("cancel", (event) => { event.preventDefault(); closeDialog(); });
      return { reset() { closeDialog(); query.value = ""; scope.replaceChildren(); } };
    },
  };
})();
