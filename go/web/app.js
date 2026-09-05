    (() => {
      "use strict";
      const state = { path: "/", entries: [], busy: false, search: "", connection: "checking" };
      const $ = (id) => document.getElementById(id);
      const apiRoot = "/api/v1/";
      const DIRECTORY_CACHE_TTL_MS = 30 * 1000;
      const PREFETCH_CONCURRENCY = 2;
      const PREFETCH_MAX_FOLDERS = 24;
      const directoryCache = new Map();
      let directoryCacheEpoch = 0;
      let prefetchQueue = [];
      let prefetchActive = 0;
      let prefetchGeneration = 0;
      let navigationGeneration = 0;
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
        const previousConnection = state.connection;
        setConnection("checking");
        try {
          const data = await apiRequest("status");
          const value = data && typeof data.status === "string" ? data.status : "invalid_response";
          if (value === "connected" && previousConnection !== "connected") {
            // A reconnect may point to a different WPS workspace.
            clearDirectoryCache();
          }
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
        const targetPath = canonicalPath(path);
        const requestGeneration = ++navigationGeneration;
        state.path = targetPath;
        state.search = "";
        $("search-input").value = "";
        renderBreadcrumbs();
        if (!quiet) setStatus("正在读取...");
        try {
          const connection = await checkConnection(quiet);
          if (connection !== "connected") {
            if (requestGeneration !== navigationGeneration) return;
            state.entries = [];
            renderEntries();
            return;
          }
          const entries = await directoryEntries(targetPath, force);
          if (requestGeneration !== navigationGeneration) return;
          state.entries = entries;
          setConnection("connected");
          renderEntries();
          prefetchChildDirectories(targetPath, state.entries);
          setStatus(`${state.entries.length} 个项目`, "success");
        } catch (error) {
          if (requestGeneration !== navigationGeneration) return;
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
          clearDirectoryCache();
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
          clearDirectoryCache();
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
          clearDirectoryCache();
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
          clearDirectoryCache();
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
            clearDirectoryCache();
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
      $("refresh-button").addEventListener("click", () => load(state.path, false, true));
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
