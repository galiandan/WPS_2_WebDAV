/* Server task history is persisted by the adapter; browser uploads stay local. */
(() => {
  "use strict";
  const labels = { queued: "等待处理", running: "处理中", completed: "已完成", failed: "有项目失败", cancelled: "已取消", interrupted: "已中断，需核对结果", succeeded: "成功" };
  const operations = { copy: "复制", move: "移动", delete: "删除", fetch: "离线下载", archive: "后台打包" };
  const taskKey = (task) => `${task._service || "batch"}:${task.id}`;
  const isTransfer = (task) => task._service === "transfer";
  const taskOperation = (task) => isTransfer(task) ? task.kind : task.operation;
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
      const note = node("p", "preview-meta", "服务器任务会在关闭页面后继续处理。中断项目请先检查结果，再决定是否重试。");
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
        if (dialog.open) {
          const expiry = tasks.filter(artifactReady).map((task) => new Date(task.artifact_expires_at).getTime() - Date.now() + 20);
          if (expiry.length) delay = Math.min(delay, Math.max(100, Math.min(...expiry)));
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
        const key = taskKey(task);
        let record = rows.get(key);
        const signature = JSON.stringify(task) + String(artifactReady(task));
        const pending = pendingActions.has(key);
        if (record && record.signature === signature && record.pending === pending) return record.element;
        const row = node("article", "task-item");
        row.dataset.taskId = task.id;
        row.dataset.taskService = task._service || "batch";
        row.dataset.state = task.state;
        if (isTransfer(task)) { renderTransfer(row, task, pending); return retainRow(record, key, row, signature, pending); }
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
        details.open = expanded.has(key);
        details.append(node("summary", "", `查看 ${task.total || 0} 项详情`));
        details.addEventListener("toggle", () => {
          if (details.open) expanded.add(key);
          else expanded.delete(key);
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
        return retainRow(record, key, row, signature, pending);
      }

      function retainRow(record, key, row, signature, pending) {
        if (record) {
          const focused = document.activeElement;
          const action = record.element.contains(focused) && focused.dataset.taskAction;
          const summaryFocused = record.element.contains(focused) && focused.tagName === "SUMMARY";
          record.element.replaceWith(row);
          const replacement = action ? row.querySelector(`[data-task-action="${action}"]`) : summaryFocused ? row.querySelector("summary") : null;
          if (replacement && !replacement.disabled) replacement.focus({ preventScroll: true });
        }
        rows.set(key, { element: row, signature, pending });
        return row;
      }

      function bytes(value) {
        let amount = Math.max(0, Number(value) || 0), unit = 0;
        const units = ["B", "KiB", "MiB", "GiB", "TiB"];
        while (amount >= 1024 && unit < units.length - 1) { amount /= 1024; unit += 1; }
        return `${amount.toFixed(unit ? 1 : 0)} ${units[unit]}`;
      }

      function artifactReady(task) {
        return isTransfer(task) && task.artifact_ready === true && new Date(task.artifact_expires_at).getTime() > Date.now();
      }

      function renderTransfer(row, task, pending) {
        const head = node("div", "task-item-head");
        head.append(node("strong", "", `${operations[task.kind] || "传输任务"} · ${task.cancel_requested && isActive(task) ? "正在取消" : labels[task.state] || task.state}`));
        const time = new Date(task.created_at);
        if (!Number.isNaN(time.getTime())) head.append(node("time", "preview-meta", time.toLocaleString("zh-CN")));
        const stages = { queued: "等待处理", downloading: "正在下载到服务器", uploading: "正在上传到 WPS", packaging: "正在生成 ZIP", ready: task.kind === "archive" ? "ZIP 已准备好" : "上传已完成", completed: "已完成", validating: "正在检查文件", preparing: "正在准备" };
        row.append(head, node("p", "transfer-task-name", task.name || (task.kind === "archive" ? "文件打包" : "下载文件")));
        const count = task.kind === "archive" && task.files_total > 0 ? ` · ${task.files_done || 0} / ${task.files_total} 项` : "";
        const byteProgress = task.bytes_total > 0 ? `${bytes(task.bytes_done)} / ${bytes(task.bytes_total)}` : bytes(task.bytes_done);
        row.append(node("p", "task-summary", `${stages[task.stage] || (isActive(task) ? "正在处理" : labels[task.state] || "")} · ${byteProgress}${count}`));
        const progress = node("progress", "task-progress");
        progress.setAttribute("aria-label", "传输进度");
        if (task.bytes_total > 0) { progress.max = task.bytes_total; progress.value = Math.min(task.bytes_total, task.bytes_done || 0); }
        else if (task.kind === "archive" && task.files_total > 0) { progress.max = task.files_total; progress.value = task.files_done || 0; }
        else if (!isActive(task)) { progress.max = 1; progress.value = task.state === "completed" ? 1 : 0; }
        row.append(progress);
        if (task.destination) row.append(node("p", "task-destination preview-meta", `目标：${task.destination}`));
        if (task.error) row.append(node("p", "transfer-task-error", String(task.error).replace(/https?:\/\/[^\s]+/gi, "下载地址")));
        if (task.stage === "uploading" && isActive(task)) row.append(node("p", "preview-note", "已进入上传登记，需等待结果。字节进度达到 100% 后仍需等待 WPS 确认完成。"));
        if (task.cancel_requested && isActive(task)) row.append(node("p", "preview-note", "正在停止任务，请等待服务器确认结果。"));
        if (task.state === "interrupted" && !task.can_retry) row.append(node("p", "preview-note", "远端结果尚未确认，请先检查目标目录；此任务不能直接重试。"));
        const actions = node("div", "task-actions");
        if (isActive(task) && task.can_cancel) {
          const cancel = button(task.cancel_requested ? "正在取消…" : "取消任务", () => taskAction(task, "cancel"));
          cancel.dataset.taskAction = "cancel"; cancel.disabled = pending || Boolean(task.cancel_requested); actions.append(cancel);
        }
        if (!isActive(task) && task.can_retry) {
          const retry = button("重新创建任务", () => taskAction(task, "retry"));
          retry.dataset.taskAction = "retry"; retry.disabled = pending || Boolean(config.canPerform && !config.canPerform(task.kind)); actions.append(retry);
        }
        if (task.kind === "archive" && task.state === "completed") {
          if (artifactReady(task)) {
            const download = button("下载 ZIP", () => downloadArtifact(task));
            download.dataset.taskAction = "download"; download.disabled = pending; actions.append(download);
            row.append(node("p", "preview-meta", `${bytes(task.artifact_size)} · 可下载至 ${new Date(task.artifact_expires_at).toLocaleString("zh-CN")}`));
          } else row.append(node("p", "preview-note", "打包文件已过期或不可用，请重新打包。"));
        }
        row.append(actions);
      }

      async function downloadArtifact(task) {
        const epoch = lifecycle;
        const key = taskKey(task);
        if (!active || pendingActions.has(key)) return;
        pendingActions.add(key); render();
        try {
          const response = await config.request(`transfers/${encodeURIComponent(task.id)}`);
          if (!active || epoch !== lifecycle) return;
          if (!response || !response.task || response.task.id !== task.id || response.task.kind !== "archive") throw new Error("无法确认打包结果，请刷新任务列表。");
          const current = { ...response.task, _service: "transfer" };
          applyTasks([current, ...tasks.filter((item) => taskKey(item) !== key)], false);
          if (!artifactReady(current)) throw new Error("打包文件已过期或不可用，请重新打包。");
          const link = document.createElement("a");
          link.href = `/api/v1/transfers/${encodeURIComponent(task.id)}/download`; link.rel = "noopener";
          document.body.append(link); link.click(); link.remove();
        } catch (error) { if (active && epoch === lifecycle) showError(error); }
        finally { pendingActions.delete(key); if (active && epoch === lifecycle) render(); }
      }

      function render() {
        const running = tasks.filter(isActive).length;
        badge.textContent = running ? String(running) : "";
        badge.hidden = !running;
        openButton.setAttribute("aria-label", running ? `任务中心，${running} 个任务正在处理` : "任务中心");
        status.textContent = tasks.length ? `${tasks.length} 个任务记录${running ? ` · ${running} 个正在处理` : ""}` : "暂无服务器任务";
        refreshButton.disabled = Boolean(requestPromise);
        const existing = new Set(tasks.map(taskKey));
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
          const key = taskKey(task);
          known.add(key);
          if (!isActive(task) && !settled.has(key)) {
            settled.add(key);
            if (announce && (!initial || previous.has(key))) {
              const epoch = lifecycle;
              Promise.resolve(isTransfer(task) ? config.onTransferFinished && config.onTransferFinished(task) : config.onFinished(task)).catch((error) => { if (active && epoch === lifecycle) showError(error); });
              config.notify(isTransfer(task) ? `${operations[task.kind] || "传输任务"}：${labels[task.state] || task.state}` : `${operations[task.operation] || "任务"}：${labels[task.state] || task.state}，成功 ${task.succeeded || 0} 项，失败 ${task.failed || 0} 项`, task.state === "completed" ? "success" : "warn", 5000);
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
            const services = config.transfersEnabled && config.transfersEnabled() ? ["tasks", "transfers"] : ["tasks"];
            const reads = await Promise.allSettled(services.map(async (route) => {
              try { return await config.request(route, { signal: current.signal }); }
              catch (error) { if (error.status === 401 && active && epoch === lifecycle) showError(error); throw error; }
            }));
            if (!active || epoch !== lifecycle || version !== snapshotVersion) return;
            if (timedOut) throw new Error("任务读取超时，请稍后刷新");
            if (current.signal.aborted) return;
            const merged = [], errors = [];
            let persistenceError = false;
            reads.forEach((read, index) => {
              const service = services[index] === "transfers" ? "transfer" : "batch";
              if (read.status === "fulfilled") {
                persistenceError ||= Boolean(read.value.persistence_error);
                merged.push(...(Array.isArray(read.value.tasks) ? read.value.tasks : []).map((task) => ({ ...task, _service: service })));
              } else {
                merged.push(...tasks.filter((task) => (task._service || "batch") === service));
                errors.push(read.reason);
              }
            });
            errorNode.textContent = persistenceError ? "服务器无法保存任务记录，暂时不能提交新任务。请检查服务器存储后刷新。" : "";
            failures = errors.length ? failures + 1 : 0;
            applyTasks(merged.sort((a, b) => String(b.created_at).localeCompare(String(a.created_at))));
            if (errors.length) showError(errors[0]);
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
          return { ...task, _service: route.startsWith("transfers") ? "transfer" : "batch" };
        } finally { actionControllers.delete(current); }
      }

      async function taskAction(task, action) {
        const key = taskKey(task);
        if (pendingActions.has(key) || !active) return;
        if (action === "cancel" && isTransfer(task) && !task.can_cancel) return;
        if (action === "retry" && config.canPerform && !config.canPerform(taskOperation(task))) { showError(Object.assign(new Error("当前账号没有此任务的执行权限"), { status: 403 })); return; }
        const epoch = lifecycle;
        pendingActions.add(key);
        render();
        try {
          if (action === "retry") {
            confirming = true;
            let confirmed;
            try { confirmed = await config.confirm("重试任务", isTransfer(task) ? (task.kind === "archive" ? "将重新打包所选项目，生成新的下载文件。确认继续？" : "将重新下载并保存文件，同名文件不会覆盖。确认继续？") : `将创建新任务，只重试 ${task.retryable_count} 个失败或尚未执行的项目。成功项目和结果未确认的项目不会重复执行。`, "创建重试任务"); }
            finally { confirming = false; }
            if (!confirmed || !active || epoch !== lifecycle) return;
          }
          const updated = await mutate(`${isTransfer(task) ? "transfers" : "tasks"}/${encodeURIComponent(task.id)}/${action}`);
          if (action === "retry" && !isTransfer(updated)) config.onQueued(updated);
          known.add(taskKey(updated));
          applyTasks([updated, ...tasks.filter((item) => taskKey(item) !== taskKey(updated))]);
          if (action === "retry") config.notify("已创建重试任务", "info");
          schedule(0);
        } catch (error) { if (active && epoch === lifecycle) showError(error); }
        finally {
          pendingActions.delete(key);
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
        async enqueueTransfer(body) {
          if (!config.transfersEnabled || !config.transfersEnabled() || (config.canPerform && !config.canPerform(body.kind))) throw Object.assign(new Error("当前账号没有此传输任务的权限"), { status: 403 });
          const task = await mutate("transfers", body);
          known.add(taskKey(task));
          applyTasks([task, ...tasks.filter((item) => taskKey(item) !== taskKey(task))]);
          open(); schedule(0); return task;
        },
        async enqueue(body) {
          const task = await mutate("tasks", body);
          config.onQueued(task);
          known.add(taskKey(task));
          applyTasks([task, ...tasks.filter((item) => taskKey(item) !== taskKey(task))]);
          open();
          schedule(0);
          return task;
        },
      };
    },
  };
})();
