/* Server task history is persisted by the adapter; browser uploads stay local. */
(() => {
  "use strict";
  const labels = { queued: "等待处理", running: "处理中", completed: "已完成", failed: "有项目失败", cancelled: "已取消", interrupted: "已中断，需核对结果", succeeded: "成功" };
  const operations = { copy: "复制", move: "移动", delete: "删除" };
  const isActive = (task) => task.state === "queued" || task.state === "running";
  function node(tag, className, text) {
    const value = document.createElement(tag);
    if (className) value.className = className;
    if (text !== undefined) value.textContent = text;
    return value;
  }
  function button(label, action, className = "") {
    const value = node("button", className, label);
    value.type = "button";
    value.addEventListener("click", action);
    return value;
  }

  window.WPSTaskCenter = {
    init(config) {
      const dialog = node("dialog", "tasks-dialog");
      dialog.id = "tasks-modal";
      dialog.setAttribute("aria-labelledby", "tasks-title");
      const card = node("section", "modal-card tasks-card");
      const heading = node("div", "tasks-heading");
      const title = node("h2", "modal-title", "任务中心");
      title.id = "tasks-title";
      const close = button("关闭", () => dialog.close());
      close.id = "tasks-close";
      heading.append(title, close);
      const note = node("p", "preview-meta", "复制、移动和删除由服务器处理，关闭页面仍会继续。中断项目请先检查结果，再决定是否重试。");
      const toolbar = node("div", "tasks-toolbar");
      const status = node("span", "preview-meta");
      status.id = "tasks-status";
      status.setAttribute("role", "status");
      const refreshButton = button("刷新任务", () => refresh(true));
      refreshButton.id = "tasks-refresh";
      toolbar.append(status, refreshButton);
      const errorNode = node("p", "modal-inline-error");
      errorNode.id = "tasks-error";
      errorNode.setAttribute("role", "alert");
      const list = node("div", "tasks-list");
      list.id = "tasks-list";
      const uploads = node("section", "tasks-uploads");
      uploads.id = "tasks-uploads";
      const uploadTitle = node("h3", "", "浏览器上传（本页）");
      const uploadNote = node("p", "preview-meta", "关闭或刷新此页面会中断尚未完成的上传。上传重试和取消请在上传队列中操作。");
      const uploadSummary = node("p", "preview-meta");
      uploadSummary.id = "tasks-upload-summary";
      const uploadButton = button("查看上传队列", () => { dialog.close(); config.showUploads(); });
      uploadButton.id = "tasks-show-uploads";
      uploads.append(uploadTitle, uploadNote, uploadSummary, uploadButton);
      card.append(heading, note, toolbar, errorNode, list, uploads);
      dialog.append(card);
      document.body.append(dialog);
      const openButton = document.getElementById("tasks-button");
      const badge = document.getElementById("tasks-count");
      let active = false, lifecycle = 0, snapshotVersion = 0;
      let timer = null, uploadTimer = null, controller = null, requestPromise = null;
      let tasks = [], initial = true, failures = 0, confirming = false;
      const actionControllers = new Set();
      const pendingActions = new Set();
      const settled = new Set();
      const expanded = new Set();
      const known = new Set();
      const rows = new Map();

      function schedule(delay = null) {
        clearTimeout(timer);
        timer = null;
        if (!active || document.hidden) return;
        const running = tasks.some(isActive);
        if (delay === null) {
          if (failures) delay = Math.min(30000, 2000 * Math.pow(2, failures));
          else if (running) delay = dialog.open ? 2000 : 10000;
          else if (dialog.open) delay = 10000;
          else return;
        }
        timer = setTimeout(() => refresh(), delay);
      }

      function showError(error) {
        if (error.name === "AbortError") return;
        if (error.status === 401) {
          stop();
          config.onError(error);
          return;
        }
        errorNode.textContent = error.message || "无法读取任务，请刷新重试";
      }

      function taskRow(task) {
        let record = rows.get(task.id);
        const signature = JSON.stringify(task);
        const pending = pendingActions.has(task.id);
        if (record && record.signature === signature && record.pending === pending) return record.element;
        const row = node("article", "task-item");
        row.dataset.taskId = task.id;
        row.dataset.state = task.state;
        const head = node("div", "task-item-head");
        head.append(node("strong", "", `${operations[task.operation] || task.operation} · ${task.cancel_requested && isActive(task) ? "正在取消" : labels[task.state] || task.state}`));
        const time = new Date(task.created_at);
        if (!Number.isNaN(time.getTime())) head.append(node("time", "preview-meta", time.toLocaleString("zh-CN")));
        const summary = node("p", "task-summary", `已处理 ${task.completed || 0} / ${task.total || 0} 项 · 成功 ${task.succeeded || 0} 项 · 失败 ${task.failed || 0} 项`);
        const progress = node("progress", "task-progress");
        progress.max = Math.max(1, task.total || 0);
        progress.value = task.completed || 0;
        progress.setAttribute("aria-label", "已处理项目数");
        row.append(head, summary, progress);
        if (task.destination) row.append(node("p", "task-destination preview-meta", `目标：${task.destination}`));
        if (task.cancel_requested && isActive(task)) row.append(node("p", "preview-note", "将停止后续项目，正在执行的项目需等待确认结果。"));
        if (task.state === "interrupted") row.append(node("p", "preview-note", "结果未确认的项目不会自动重试，请检查源目录和目标目录。"));
        const details = node("details", "task-details");
        details.open = expanded.has(task.id);
        details.append(node("summary", "", `查看 ${task.total || 0} 项详情`));
        details.addEventListener("toggle", () => {
          if (details.open) expanded.add(task.id);
          else expanded.delete(task.id);
        });
        const items = node("ul", "task-item-results");
        for (const item of task.items || []) {
          const result = node("li", "task-result");
          result.dataset.state = item.state;
          result.append(node("span", "task-path", item.path));
          result.append(node("span", "task-result-status", `${labels[item.state] || item.state}${item.error ? " · " + item.error : ""}`));
          if (item.state === "interrupted" && !item.retryable) result.append(node("span", "preview-note", "结果未确认，需手动检查"));
          items.append(result);
        }
        details.append(items);
        row.append(details);
        const actions = node("div", "task-actions");
        if (isActive(task)) {
          const cancel = button(task.cancel_requested ? "正在取消…" : "取消任务", () => taskAction(task, "cancel"));
          cancel.dataset.taskAction = "cancel";
          cancel.disabled = pending || Boolean(task.cancel_requested);
          actions.append(cancel);
        } else if (task.retryable_count > 0) {
          const retry = button(`重试 ${task.retryable_count} 项`, () => taskAction(task, "retry"));
          retry.dataset.taskAction = "retry";
          retry.disabled = pending || Boolean(config.canPerform && !config.canPerform(task.operation));
          actions.append(retry);
        }
        row.append(actions);
        if (record) {
          const focused = document.activeElement;
          const action = record.element.contains(focused) && focused.dataset.taskAction;
          const summaryFocused = record.element.contains(focused) && focused.tagName === "SUMMARY";
          record.element.replaceWith(row);
          const replacement = action ? row.querySelector(`[data-task-action="${action}"]`) : summaryFocused ? row.querySelector("summary") : null;
          if (replacement && !replacement.disabled) replacement.focus({ preventScroll: true });
        }
        record = { element: row, signature, pending };
        rows.set(task.id, record);
        return row;
      }

      function render() {
        const running = tasks.filter(isActive).length;
        badge.textContent = running ? String(running) : "";
        badge.hidden = !running;
        openButton.setAttribute("aria-label", running ? `任务中心，${running} 个任务正在处理` : "任务中心");
        status.textContent = tasks.length ? `${tasks.length} 个任务记录${running ? ` · ${running} 个正在处理` : ""}` : "暂无服务器任务";
        refreshButton.disabled = Boolean(requestPromise);
        const existing = new Set(tasks.map((task) => task.id));
        for (const [id, record] of rows) if (!existing.has(id)) { record.element.remove(); rows.delete(id); expanded.delete(id); }
        for (let index = 0; index < tasks.length; index += 1) {
          const row = taskRow(tasks[index]);
          if (list.children[index] !== row) list.insertBefore(row, list.children[index] || null);
        }
        renderUploads();
      }

      function applyTasks(values, announce = true) {
        const previous = new Set(known);
        tasks = Array.isArray(values) ? values : [];
        for (const task of tasks) {
          known.add(task.id);
          if (!isActive(task) && !settled.has(task.id)) {
            settled.add(task.id);
            if (announce && (!initial || previous.has(task.id))) {
              const epoch = lifecycle;
              Promise.resolve(config.onFinished(task)).catch((error) => { if (active && epoch === lifecycle) showError(error); });
              config.notify(`${operations[task.operation] || "任务"}：${labels[task.state] || task.state}，成功 ${task.succeeded || 0} 项，失败 ${task.failed || 0} 项`, task.state === "completed" ? "success" : "warn", 5000);
            }
          }
        }
        initial = false;
        render();
      }

      async function refresh(explicit = false) {
        if (!active || document.hidden || requestPromise) return requestPromise;
        const epoch = lifecycle;
        const version = snapshotVersion;
        const current = new AbortController();
        controller = current;
        let timedOut = false;
        const deadline = setTimeout(() => { timedOut = true; current.abort(); }, 20000);
        const request = (async () => {
          try {
            const response = await config.request("tasks", { signal: current.signal });
            if (!active || epoch !== lifecycle || version !== snapshotVersion) return;
            if (current.signal.aborted) return;
            errorNode.textContent = response.persistence_error ? "服务器无法保存任务记录，暂时不能提交新任务。请检查服务器存储后刷新。" : "";
            failures = 0;
            applyTasks(response.tasks);
          } catch (error) {
            if (!active || epoch !== lifecycle || version !== snapshotVersion) return;
            if (timedOut) error = new Error("任务读取超时，请稍后刷新");
            if (error.name !== "AbortError") failures += 1;
            showError(error);
          }
        })();
        requestPromise = request;
        refreshButton.disabled = true;
        if (explicit && !tasks.length) status.textContent = "正在读取任务…";
        try { await request; }
        finally {
          clearTimeout(deadline);
          if (requestPromise === request) requestPromise = null;
          if (controller === current) controller = null;
          if (active && epoch === lifecycle) { refreshButton.disabled = false; schedule(); }
        }
      }

      async function mutate(route, body) {
        if (!active) throw new Error("请登录后再操作任务");
        const epoch = lifecycle;
        const current = new AbortController();
        actionControllers.add(current);
        try {
          const response = await config.request(route, { method: "POST", signal: current.signal, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
          if (!active || epoch !== lifecycle) throw new DOMException("任务页面已关闭", "AbortError");
          snapshotVersion += 1;
          if (controller) controller.abort();
          const task = response.task;
          if (!task || !task.id) throw new Error("任务响应无效，请刷新任务中心确认结果");
          return task;
        } finally { actionControllers.delete(current); }
      }

      async function taskAction(task, action) {
        if (pendingActions.has(task.id) || !active) return;
        if (action === "retry" && config.canPerform && !config.canPerform(task.operation)) { showError(Object.assign(new Error("当前账号没有此任务的执行权限"), { status: 403 })); return; }
        const epoch = lifecycle;
        pendingActions.add(task.id);
        render();
        try {
          if (action === "retry") {
            confirming = true;
            let confirmed;
            try { confirmed = await config.confirm("重试任务", `将创建新任务，只重试 ${task.retryable_count} 个失败或尚未执行的项目。成功项目和结果未确认的项目不会重复执行。`, "创建重试任务"); }
            finally { confirming = false; }
            if (!confirmed || !active || epoch !== lifecycle) return;
          }
          const updated = await mutate(`tasks/${encodeURIComponent(task.id)}/${action}`);
          if (action === "retry") config.onQueued(updated);
          known.add(updated.id);
          applyTasks([updated, ...tasks.filter((item) => item.id !== updated.id)]);
          if (action === "retry") config.notify("已创建重试任务", "info");
          schedule(0);
        } catch (error) { if (active && epoch === lifecycle) showError(error); }
        finally {
          pendingActions.delete(task.id);
          if (active && epoch === lifecycle) render();
        }
      }

      function renderUploads() {
        const upload = config.getUploads();
        uploadSummary.textContent = upload.total ? `${upload.active ? "正在上传" : "本页上传记录"} · 已完成 ${upload.done} / ${upload.total} 项` : "本页暂无上传记录";
        uploadButton.disabled = !upload.total;
      }

      function open() {
        if (!active) return;
        render();
        if (!dialog.open) dialog.showModal();
        refresh(true);
      }

      function stop() {
        active = false;
        lifecycle += 1;
        snapshotVersion += 1;
        clearTimeout(timer);
        clearTimeout(uploadTimer);
        timer = uploadTimer = null;
        if (controller) controller.abort();
        controller = null;
        requestPromise = null;
        for (const action of actionControllers) action.abort();
        actionControllers.clear();
        tasks = [];
        known.clear();
        settled.clear();
        expanded.clear();
        rows.clear();
        pendingActions.clear();
        if (confirming && config.dismissConfirm) config.dismissConfirm();
        confirming = false;
        list.replaceChildren();
        errorNode.textContent = status.textContent = uploadSummary.textContent = "";
        badge.hidden = true;
        badge.textContent = "";
        initial = true;
        failures = 0;
        dialog.close();
        if (config.onReset) config.onReset();
      }

      openButton.addEventListener("click", open);
      dialog.addEventListener("close", () => schedule());
      document.addEventListener("visibilitychange", () => {
        if (!active) return;
        if (document.hidden) {
          clearTimeout(timer);
          timer = null;
          if (controller) controller.abort();
        } else refresh();
      });
      return {
        start() { if (active) return; active = true; refresh(); },
        stop,
        open,
        updateUploads() {
          if (!dialog.open || uploadTimer) return;
          uploadTimer = setTimeout(() => { uploadTimer = null; if (active && dialog.open) renderUploads(); }, 200);
        },
        async enqueue(body) {
          const task = await mutate("tasks", body);
          config.onQueued(task);
          known.add(task.id);
          applyTasks([task, ...tasks.filter((item) => item.id !== task.id)]);
          open();
          schedule(0);
          return task;
        },
      };
    },
  };
})();
