/* Administrator-managed adapter accounts. Roots are chosen through /entries. */
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
  const field = (label, input) => {
    const wrapper = node("label", label, "users-field");
    wrapper.append(input);
    return wrapper;
  };
  window.WPSUsers = {
    init(config) {
      const dialog = node("dialog", "", "users-dialog");
      dialog.id = "users-modal";
      dialog.setAttribute("aria-labelledby", "users-title");
      const card = node("section", "", "modal-card users-card");
      const heading = node("div", "", "users-heading");
      const title = node("h2", "用户与目录权限", "modal-title");
      title.id = "users-title";
      const close = button("users-close", "关闭", () => closeDialog());
      heading.append(title, close);
      const note = node("p", "成员通过各自的账号访问分配目录。修改密码、目录、权限或停用账号会使该成员重新登录。", "preview-meta");
      const tools = node("div", "", "users-toolbar");
      const refreshButton = button("users-refresh", "刷新用户", () => refresh());
      const createButton = button("users-create", "添加用户", () => edit(null), "primary");
      const count = node("span", "", "preview-meta");
      count.id = "users-count";
      tools.append(createButton, refreshButton, count);
      const error = node("p", "", "modal-inline-error");
      error.id = "users-error";
      error.setAttribute("role", "alert");
      const list = node("div", "", "users-list");
      list.id = "users-list";
      const form = node("form", "", "users-form");
      form.id = "users-form";
      form.hidden = true;
      const formTitle = node("h3", "添加用户");
      formTitle.id = "users-form-title";
      const username = node("input");
      username.id = "users-username";
      username.autocomplete = "off";
      username.required = true;
      username.maxLength = 64;
      username.pattern = "[A-Za-z0-9_.-]{1,64}";
      const password = node("input");
      password.id = "users-password";
      password.type = "password";
      password.autocomplete = "new-password";
      password.maxLength = 256;
      const passwordLabel = field("密码（8–256 字节）", password);
      const root = node("input");
      root.id = "users-root";
      root.readOnly = true;
      root.placeholder = "请选择成员可以访问的目录";
      const chooseRoot = button("users-choose-root", "选择目录", () => openPicker());
      const rootRow = node("div", "", "users-root-row");
      rootRow.append(root, chooseRoot);
      const rootField = field("可访问的根目录", rootRow);
      const permissions = node("fieldset", "", "users-permissions");
      permissions.append(node("legend", "文件权限"));
      const read = node("input"); read.type = "checkbox"; read.checked = true; read.disabled = true;
      const upload = node("input"); upload.type = "checkbox"; upload.id = "users-upload";
      const remove = node("input"); remove.type = "checkbox"; remove.id = "users-delete";
      for (const [label, input] of [["读取和下载", read], ["新建、上传和复制", upload], ["删除", remove]]) permissions.append(field(label, input));
      permissions.append(node("p", "同时允许上传和删除时，也允许覆盖、编辑、重命名和移动。", "preview-meta"));
      const enabled = node("input"); enabled.type = "checkbox"; enabled.id = "users-enabled"; enabled.checked = true;
      const enabledField = field("启用此账号", enabled);
      const save = button("users-save", "创建用户", () => {} , "primary"); save.type = "submit";
      const cancel = button("users-cancel", "取消编辑", () => clearForm());
      const actions = node("div", "", "modal-actions"); actions.append(cancel, save);
      form.append(formTitle, field("用户名", username), passwordLabel, rootField, permissions, enabledField, actions);
      card.append(heading, note, tools, error, list, form);
      dialog.append(card);
      const picker = node("dialog", "", "users-root-dialog"); picker.id = "users-root-modal";
      picker.setAttribute("aria-labelledby", "users-root-title");
      const pickerCard = node("section", "", "modal-card");
      const pickerTitle = node("h2", "选择成员根目录", "modal-title"); pickerTitle.id = "users-root-title";
      const pickerNav = node("div", "", "picker-nav");
      const up = button("users-root-up", "上一级", () => readFolder(parent(pickerPath)));
      const pathLabel = node("span", "/", "picker-path"); pathLabel.id = "users-root-path";
      pickerNav.append(up, pathLabel);
      const pickerList = node("div", "", "picker-list"); pickerList.id = "users-root-list";
      const pickerError = node("p", "", "modal-inline-error"); pickerError.id = "users-root-error";
      const pickerCancel = button("users-root-cancel", "取消", () => closePicker());
      const pickerChoose = button("users-root-select", "使用此目录", () => { root.value = pickerPath; closePicker(); }, "primary");
      const pickerActions = node("div", "", "modal-actions"); pickerActions.append(pickerCancel, pickerChoose);
      pickerCard.append(pickerTitle, pickerNav, pickerList, pickerError, pickerActions); picker.append(pickerCard);
      document.body.append(dialog, picker);
      const openButton = document.getElementById("users-button");
      let users = [], editing = null, busy = false, lifecycle = 0;
      let controller = null, pickerController = null, pickerGeneration = 0, pickerPath = "/", confirming = false;
      const parent = (path) => path.slice(0, path.lastIndexOf("/")) || "/";
      const join = (path, name) => (path === "/" ? "" : path) + "/" + name;
      function allowed() {
        if (config.isAdmin()) return true;
        config.onError(Object.assign(new Error("此操作需要管理员权限"), { status: 403 }));
        return false;
      }
      function update() {
        createButton.disabled = busy || users.filter((user) => user.role !== "admin").length >= 32;
        refreshButton.disabled = busy;
        save.disabled = busy;
        cancel.disabled = busy;
        username.disabled = password.disabled = chooseRoot.disabled = upload.disabled = remove.disabled = enabled.disabled = busy;
        list.querySelectorAll("button").forEach((item) => { item.disabled = busy; });
      }
      function clearForm() {
        editing = null;
        form.hidden = true;
        username.value = password.value = root.value = "";
      }
      function edit(user) {
        if (!allowed() || busy || (user && user.role === "admin")) return;
        editing = user;
        username.value = user ? user.username : "";
        password.value = "";
        password.required = !user;
        password.placeholder = user ? "留空则保持原密码" : "设置初始密码";
        root.value = user ? user.root_path : "";
        upload.checked = Boolean(user && user.permissions.upload);
        remove.checked = Boolean(user && user.permissions.delete);
        enabled.checked = user ? user.enabled : true;
        enabledField.hidden = !user;
        formTitle.textContent = user ? `编辑：${user.username}` : "添加用户";
        save.textContent = user ? "保存用户" : "创建用户";
        error.textContent = "";
        form.hidden = false;
        username.focus();
      }
      function render() {
        count.textContent = `${users.filter((user) => user.role !== "admin").length} / 32 个成员`;
        list.replaceChildren(...users.map((user) => {
          const item = node("article", "", "user-item"); item.dataset.userId = user.id;
          const summary = node("div", "", "user-summary");
          summary.append(node("strong", user.username));
          summary.append(node("span", user.role === "admin" ? "安装管理员" : user.enabled ? "已启用" : "已停用", "user-state"));
          item.append(summary);
          if (user.role === "admin") item.append(node("p", "管理员由服务配置管理，可访问全部文件。", "preview-meta"));
          else {
            item.append(node("p", `根目录：${user.root_path}`, "user-root"));
            const grants = ["读取", ...(user.permissions.upload ? ["上传"] : []), ...(user.permissions.delete ? ["删除"] : [])];
            item.append(node("p", grants.join(" · "), "preview-meta"));
            const actions = node("div", "", "user-actions");
            actions.append(button("", "编辑", () => edit(user)), button("", user.enabled ? "停用" : "启用", () => changeEnabled(user)), button("", "删除账号", () => deleteUser(user), "danger"));
            item.append(actions);
          }
          return item;
        }));
        update();
      }
      function failure(err) {
        if (err.name === "AbortError") return;
        error.textContent = err.status === 403 ? "需要管理员权限，或当前登录的权限已改变。请重新登录。" : err.message || "用户操作失败，请重试";
        if (err.status === 401) { reset(); config.onError(err); }
      }
      async function refresh() {
        if (!allowed() || busy) return;
        busy = true; update(); error.textContent = "";
        const epoch = lifecycle, request = new AbortController(); controller = request;
        try {
          const response = await config.request("users", { signal: request.signal });
          if (epoch !== lifecycle) return;
          users = Array.isArray(response.users) ? response.users : [];
          render();
        } catch (err) { if (epoch === lifecycle) failure(err); }
        finally { if (epoch === lifecycle) { busy = false; controller = null; update(); } }
      }
      async function mutate(route, method, body) {
        if (!allowed() || busy) return false;
        busy = true; update(); error.textContent = "";
        const epoch = lifecycle, request = new AbortController(); controller = request;
        try {
          await config.request(route, { method, signal: request.signal, headers: { "Content-Type": "application/json" }, ...(body ? { body: JSON.stringify(body) } : {}) });
          if (epoch !== lifecycle) return false;
          clearForm();
          config.notify("用户设置已更新", "success");
          return true;
        } catch (err) { if (epoch === lifecycle) failure(err); return false; }
        finally { if (epoch === lifecycle) { busy = false; controller = null; update(); } }
      }
      async function confirmation(title, message, action, danger = false) {
        const epoch = lifecycle;
        confirming = true;
        try { return await config.confirm(title, message, action, danger) && epoch === lifecycle && config.isAdmin(); }
        finally { confirming = false; }
      }
      async function changeEnabled(user) {
        if (!allowed() || busy) return;
        if (user.enabled && !(await confirmation("停用账号", `停用“${user.username}”后，其会话将失效。`, "停用", true))) return;
        if (await mutate("users/" + encodeURIComponent(user.id), "PATCH", { enabled: !user.enabled })) await refresh();
      }
      async function deleteUser(user) {
        if (!allowed() || busy || user.role === "admin") return;
        if (!(await confirmation("删除账号", `删除“${user.username}”的登录账号和个人验证设置？目录中的文件不会删除。`, "删除账号", true))) return;
        if (await mutate("users/" + encodeURIComponent(user.id), "DELETE")) await refresh();
      }
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        if (!allowed() || busy) return;
        const name = username.value.trim();
        const bytes = new TextEncoder().encode(password.value).length;
        if (!name || !root.value) { error.textContent = "请填写用户名并选择根目录。"; return; }
        if ((!editing || password.value) && (bytes < 8 || bytes > 256 || /[\u0000-\u001f\u007f]/.test(password.value))) { error.textContent = "密码需要 8–256 个 UTF-8 字节。"; return; }
        const body = { username: name, root_path: root.value, permissions: { read: true, upload: upload.checked, delete: remove.checked } };
        if (password.value) body.password = password.value;
        if (editing) body.enabled = enabled.checked;
        if (await mutate(editing ? "users/" + encodeURIComponent(editing.id) : "users", editing ? "PATCH" : "POST", body)) await refresh();
      });
      function closePicker() {
        pickerGeneration += 1;
        if (pickerController) pickerController.abort();
        pickerController = null;
        picker.close();
      }
      async function readFolder(path) {
        if (!allowed()) return;
        const generation = ++pickerGeneration;
        if (pickerController) pickerController.abort();
        const request = new AbortController(); pickerController = request;
        pickerPath = path; pathLabel.textContent = path;
        up.disabled = path === "/";
        pickerChoose.disabled = true;
        pickerError.textContent = "";
        pickerList.replaceChildren(node("p", "正在读取目录…", "preview-meta"));
        try {
          const response = await config.entries(path, request.signal);
          if (generation !== pickerGeneration) return;
          const entries = response.entries || [];
          const folders = entries.filter((entry) => entry.kind === "folder");
          pickerList.replaceChildren(...folders.map((entry) => button("", entry.name, () => readFolder(join(path, entry.name)), "picker-item")));
          if (!folders.length) pickerList.append(node("p", "这里没有子文件夹", "preview-meta"));
          pickerChoose.disabled = path === "/" && folders.some((entry) => String(entry.id).startsWith("space:"));
        } catch (err) {
          if (generation !== pickerGeneration || err.name === "AbortError") return;
          pickerError.textContent = err.message || "目录读取失败，请返回上一级重试";
          if (err.status === 401) { reset(); config.onError(err); }
        }
      }
      function openPicker() {
        if (!allowed() || busy) return;
        picker.showModal();
        readFolder(root.value || "/");
      }
      function closeDialog() {
        lifecycle += 1;
        if (controller) controller.abort();
        controller = null; busy = false;
        closePicker();
        clearForm();
        dialog.close();
      }
      function reset() {
        closeDialog();
        if (confirming) config.dismissConfirm();
        users = [];
        list.replaceChildren(); pickerList.replaceChildren();
        pickerPath = "/"; pathLabel.textContent = "/"; pickerError.textContent = "";
        count.textContent = error.textContent = "";
      }
      openButton.addEventListener("click", () => {
        if (!allowed()) return;
        if (!dialog.open) dialog.showModal();
        refresh();
      });
      dialog.addEventListener("cancel", (event) => { event.preventDefault(); closeDialog(); });
      picker.addEventListener("cancel", (event) => { event.preventDefault(); closePicker(); });
      return { reset };
    },
  };
})();
