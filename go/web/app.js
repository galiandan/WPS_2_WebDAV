(() => {
  "use strict";

  /* ============ 基础工具 ============ */
  const $ = (id) => document.getElementById(id);
  const apiRoot = "/api/v1/";
  const DIRECTORY_CACHE_TTL_MS = 30 * 1000;
  const PREFETCH_CONCURRENCY = 2;
  const PREFETCH_MAX_FOLDERS = 24;

  const state = {
    path: "/",
    entries: [],
    busy: false,
    loading: false,
    refreshing: false,
    directoryError: null,
    search: "",
    connection: "checking",
    view: "list",
    spaces: [],
    selectedPath: "",
    pendingDeletes: new Set(),
    sort: { key: "auto", dir: "asc" },
  };

  function icon(name, cls = "") {
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    // SVG 元素的 className 是只读的 SVGAnimatedString，必须用 setAttribute。
    svg.setAttribute("class", "icon" + (cls ? " " + cls : ""));
    const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", "#i-" + name);
    svg.append(use);
    return svg;
  }

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  /* ============ 本地偏好 ============ */
  const PREF = {
    get(key, fallback) {
      try {
        const raw = localStorage.getItem("wpsdrv." + key);
        return raw === null ? fallback : raw;
      } catch (_) { return fallback; }
    },
    set(key, value) {
      try { localStorage.setItem("wpsdrv." + key, value); } catch (_) {}
    },
  };

  /* ============ 主题 ============ */
  const THEME_ORDER = ["auto", "light", "dark"];
  const THEME_META = {
    auto: { icon: "monitor", color: "#f5f7f8" },
    light: { icon: "sun", color: "#f5f7f8" },
    dark: { icon: "moon", color: "#121517" },
  };
  const darkQuery = window.matchMedia("(prefers-color-scheme: dark)");
  let theme = PREF.get("theme", "auto");
  if (!THEME_ORDER.includes(theme)) theme = "auto";

  function applyTheme(animate = false) {
    // 跟随系统时不设置 data-theme，由样式表里的 prefers-color-scheme
    // 媒体查询决定外观，切主题与首帧都不会闪烁。
    if (theme === "auto") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
    if (animate) {
      const root = document.documentElement;
      root.classList.add("theme-anim");
      setTimeout(() => root.classList.remove("theme-anim"), 380);
    }
    const meta = THEME_META[theme];
    const resolvedColor = theme === "auto"
      ? (darkQuery.matches ? THEME_META.dark.color : THEME_META.light.color)
      : meta.color;
    const button = $("theme-button");
    button.title = "打开主题设置";
    button.setAttribute("aria-label", "打开主题设置");
    const use = $("theme-icon");
    use.setAttribute("href", "#i-" + meta.icon);
    button.classList.remove("spin-icon");
    void button.offsetWidth;
    button.classList.add("spin-icon");
    setTimeout(() => button.classList.remove("spin-icon"), 480);
    document.querySelector('meta[name="theme-color"]').setAttribute("content", resolvedColor);
  }

  darkQuery.addEventListener("change", () => { if (theme === "auto") applyTheme(false); });

  let settingsDraftTheme = theme;
  let settingsInFlight = false;
  let settingsTrigger = null;
  let storageLocations = [];
  let storageCurrent = null;
  let storageLocationLoading = false;

  function renderThemeOptions() {
    document.querySelectorAll(".theme-option").forEach((button) => {
      const selected = button.dataset.theme === settingsDraftTheme;
      button.classList.toggle("selected", selected);
      button.setAttribute("aria-checked", String(selected));
    });
  }

  function setSettingsError(message) {
    $("settings-error").textContent = message || "";
  }

  function openSettingsModal() {
    if (settingsInFlight) return;
    settingsTrigger = document.activeElement;
    settingsDraftTheme = theme;
    $("settings-name").value = rootName;
    setSettingsError("");
    renderThemeOptions();
    loadStorageLocations();
    loadSecuritySettings();
    $("settings-modal").showModal();
    setTimeout(() => $("settings-name").focus(), 0);
  }

  async function loadSecuritySettings() {
    try {
      const data = await apiRequest("auth/security");
      const enabled = Boolean(data && data.totp_enabled);
      $("security-summary").textContent = enabled ? "两步验证已启用" : "密码之外的安全验证尚未启用";
      $("totp-enable-button").classList.toggle("hidden", enabled);
      $("totp-disable-button").classList.toggle("hidden", !enabled);
      const list = $("passkey-list");
      list.replaceChildren();
      const passkeys = Array.isArray(data && data.passkeys) ? data.passkeys : [];
      if (!passkeys.length) {
        list.append(el("p", "security-empty", "尚未添加 Passkey"));
        return;
      }
      passkeys.forEach((passkey) => {
        const row = el("div", "passkey-row");
        row.append(el("span", "passkey-name", passkey.name || "Passkey"));
        const remove = el("button", "icon-button small", "");
        remove.type = "button";
        remove.title = "删除 Passkey";
        remove.setAttribute("aria-label", "删除 Passkey");
        remove.append(icon("trash"));
        remove.addEventListener("click", () => deletePasskey(passkey.id));
        row.append(remove);
        list.append(row);
      });
    } catch (error) {
      if (error && error.status !== 404) setSettingsError(error.message || "无法读取安全设置");
    }
  }

  async function enableTOTP() {
    try {
      const setup = await apiRequest("auth/totp/setup", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
      const code = window.prompt(`请将此密钥添加到验证器：\n\n${setup.secret}\n\n然后输入当前 6 位验证码以启用：`);
      if (!code) return;
      const result = await apiRequest("auth/totp/enable", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ secret: setup.secret, code: code.trim() }) });
      window.alert(`两步验证已启用。请保存这些一次性恢复码：\n\n${result.recovery_codes.join("\n")}`);
      await loadSecuritySettings();
    } catch (error) {
      setSettingsError(error.message || "无法启用两步验证");
    }
  }

  async function disableTOTP() {
    const code = window.prompt("请输入当前验证码或恢复码以关闭两步验证：");
    if (!code) return;
    try {
      await apiRequest("auth/totp/disable", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ code: code.trim() }) });
      await loadSecuritySettings();
    } catch (error) {
      setSettingsError(error.message || "无法关闭两步验证");
    }
  }

  async function registerPasskey() {
    if (!window.PublicKeyCredential || !navigator.credentials) {
      setSettingsError("当前浏览器不支持 Passkey，或当前页面不是安全连接");
      return;
    }
    try {
      const data = await apiRequest("auth/passkey/register/options", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
      const credential = await navigator.credentials.create({ publicKey: publicKeyCreationOptions(data.publicKey) });
      if (!credential) throw new Error("Passkey 注册已取消");
      await apiRequest("auth/passkey/register", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ challenge: data.challenge, name: "Passkey", credential: passkeyCredentialJSON(credential, true) }),
      });
      await loadSecuritySettings();
    } catch (error) {
      setSettingsError(error.message || "Passkey 注册失败，请重试");
    }
  }

  async function deletePasskey(id) {
    if (!window.confirm("确定删除这个 Passkey 吗？")) return;
    try {
      await apiRequest("auth/passkey/delete", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id }) });
      await loadSecuritySettings();
    } catch (error) {
      setSettingsError(error.message || "无法删除 Passkey");
    }
  }

  function storageLocationLabel(location) {
    if (!location) return "未配置";
    const rootPath = location.root_path || "/";
    return rootPath === "/" ? `${location.name} /` : `${location.name} ${rootPath}`;
  }

  function renderStorageLocation() {
    const node = $("storage-location-current");
    if (!node) return;
    if (storageLocationLoading) {
      node.textContent = "正在读取存储位置...";
      return;
    }
    if (!storageCurrent) {
      node.textContent = "尚未配置 WPS 工作区";
      return;
    }
    node.textContent = storageLocationLabel(storageCurrent);
  }

  async function loadStorageLocations() {
    storageLocationLoading = true;
    renderStorageLocation();
    try {
      const data = await apiRequest("storage");
      storageLocations = data && Array.isArray(data.locations) ? data.locations : [];
      storageCurrent = data && data.current && typeof data.current === "object" ? data.current : null;
    } catch (error) {
      storageLocations = [];
      storageCurrent = null;
      setSettingsError(error.message || "存储位置读取失败");
    } finally {
      storageLocationLoading = false;
      renderStorageLocation();
    }
  }

  function closeSettingsModal() {
    const dialog = $("settings-modal");
    if (dialog.open) dialog.close();
    if (settingsTrigger && document.contains(settingsTrigger)) settingsTrigger.focus();
    settingsTrigger = null;
  }

  let storagePickerResolve = null;
  let storagePickerSpace = null;
  let storagePickerPath = "/";
  let storagePickerLoading = false;
  let storagePickerGeneration = 0;

  function storagePickerFullPath() {
    if (!storagePickerSpace) return "/";
    return storagePickerSpace.path === "/"
      ? storagePickerPath
      : joinPath(storagePickerSpace.path, storagePickerPath === "/" ? "" : storagePickerPath.slice(1));
  }

  function renderStoragePickerHeader() {
    $("storage-picker-path").textContent = storagePickerFullPath();
    $("storage-picker-up").disabled = storagePickerLoading || storagePickerPath === "/";
    $("storage-picker-select").disabled = storagePickerLoading || !storagePickerSpace;
  }

  async function renderStoragePickerFolders() {
    const generation = ++storagePickerGeneration;
    storagePickerLoading = true;
    renderStoragePickerHeader();
    const list = $("storage-picker-list");
    list.replaceChildren(el("div", "picker-loading", "正在读取文件夹..."));
    $("storage-picker-error").textContent = "";
    try {
      const data = await api("storage/entries", storagePickerFullPath());
      const entries = data && Array.isArray(data.entries) ? data.entries : [];
      if (generation !== storagePickerGeneration || !storagePickerResolve) return;
      list.replaceChildren();
      const folders = entries.filter((entry) => entry && entry.kind === "folder");
      if (!folders.length) list.append(el("div", "picker-empty", "这里没有子文件夹"));
      folders.forEach((entry) => {
        const button = el("button", "picker-item");
        button.type = "button";
        button.append(icon("folder"), el("span", "p-name", entry.name), icon("chev-right", "p-chev"));
        button.addEventListener("click", () => {
          storagePickerPath = joinPath(storagePickerPath, entry.name);
          renderStoragePickerFolders();
        });
        list.append(button);
      });
    } catch (error) {
      if (generation === storagePickerGeneration) $("storage-picker-error").textContent = error.message || "文件夹读取失败";
    } finally {
      if (generation === storagePickerGeneration) {
        storagePickerLoading = false;
        renderStoragePickerHeader();
      }
    }
  }

  function closeStoragePicker(value) {
    if (!storagePickerResolve) return;
    storagePickerGeneration += 1;
    const resolve = storagePickerResolve;
    storagePickerResolve = null;
    $("storage-picker").close();
    resolve(value);
  }

  async function openStoragePicker() {
    if (storageLocationLoading) return;
    if (!storageLocations.length) await loadStorageLocations();
    if (!storageLocations.length) return;
    storagePickerSpace = storageLocations[0];
    storagePickerPath = "/";
    $("storage-picker").showModal();
    renderStoragePickerSpaces();
    renderStoragePickerFolders();
    return new Promise((resolve) => { storagePickerResolve = resolve; });
  }

  function renderStoragePickerSpaces() {
    const list = $("storage-picker-spaces");
    list.replaceChildren();
    storageLocations.forEach((space) => {
      const button = el("button", "storage-picker-space" + (space === storagePickerSpace ? " selected" : ""));
      button.type = "button";
      button.append(icon("cloud"), el("span", "p-name", space.name));
      button.addEventListener("click", () => {
        storagePickerSpace = space;
        storagePickerPath = "/";
        renderStoragePickerSpaces();
        renderStoragePickerFolders();
      });
      list.append(button);
    });
  }

  async function chooseStorageLocation() {
    const selected = await openStoragePicker();
    if (!selected) return;
    setSettingsError("正在保存存储位置...");
    try {
      await apiRequest("storage", {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: selected }),
      });
      clearDirectoryCache();
      state.path = "/";
      history.replaceState(null, "", "#/" );
      await loadStorageLocations();
      closeSettingsModal();
      await load("/", true, true);
      toast("WebDAV 存储位置已更新", "success");
    } catch (error) {
      setSettingsError(error.message || "存储位置保存失败");
    }
  }

  async function submitSettings(event) {
    event.preventDefault();
    if (settingsInFlight) return;
    const name = $("settings-name").value.trim();
    if (!name) {
      setSettingsError("云盘名称不能为空");
      $("settings-name").focus();
      return;
    }
    if (name === rootName && settingsDraftTheme === theme) {
      closeSettingsModal();
      return;
    }
    settingsInFlight = true;
    const submit = $("settings-submit");
    submit.disabled = true;
    setSettingsError("正在保存设置...");
    try {
      if (name !== rootName) {
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
      }
      theme = settingsDraftTheme;
      PREF.set("theme", theme);
      applyTheme(true);
      applyRootName();
      closeSettingsModal();
      setStatus("设置已保存", "success");
      toast("设置已保存", "success");
    } catch (error) {
      if (error && error.status === 401) {
        closeSettingsModal();
        showError(error, { notify: false });
      } else {
        setSettingsError(error.message || "设置保存失败，请稍后重试");
      }
    } finally {
      settingsInFlight = false;
      submit.disabled = false;
    }
  }

  /* ============ REST 基础 ============ */
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

  /* ============ 网页账号会话 ============ */
  let webUser = null;
  let authInFlight = false;
  let pendingTwoFactorChallenge = "";
  let loginMethod = "password";

  function setAuthMessage(message, kind = "") {
    const node = $("auth-message");
    node.textContent = message || "";
    node.className = "auth-message" + (kind ? " " + kind : "");
  }

  function showAppForUser(user) {
    webUser = user || null;
    $("auth-screen").classList.add("hidden");
    $("app-ui").classList.remove("hidden");
    const name = webUser && webUser.username ? webUser.username : "退出登录";
    $("logout-button").title = `退出登录（${name}）`;
    $("logout-button").setAttribute("aria-label", `退出登录（${name}）`);
  }

  function showLoginScreen() {
    $("auth-screen").classList.remove("hidden");
    $("app-ui").classList.add("hidden");
  }

  function setLoginMethod(method) {
    if (pendingTwoFactorChallenge) method = "password";
    loginMethod = method === "passkey" ? "passkey" : "password";
    const passwordMethod = $("auth-method-password");
    const passkeyMethod = $("auth-method-passkey");
    const passwordPanel = $("password-login-panel");
    const passkeyPanel = $("passkey-login-panel");
    const passwordActive = loginMethod === "password";
    $("auth-methods").dataset.method = loginMethod;
    passwordMethod.classList.toggle("active", passwordActive);
    passkeyMethod.classList.toggle("active", !passwordActive);
    passwordMethod.setAttribute("aria-selected", String(passwordActive));
    passkeyMethod.setAttribute("aria-selected", String(!passwordActive));
    passwordPanel.classList.toggle("hidden", !passwordActive);
    passkeyPanel.classList.toggle("hidden", passwordActive);
    passkeyPanel.setAttribute("aria-hidden", String(passwordActive));
    if (passwordActive) $("login-username").focus();
  }

  function setTwoFactorChallenge(challenge) {
    pendingTwoFactorChallenge = challenge || "";
    setLoginMethod("password");
    const codeField = $("login-code-field");
    const username = $("login-username");
    const password = $("login-password");
    $("auth-method-password").disabled = Boolean(pendingTwoFactorChallenge);
    $("auth-method-passkey").disabled = Boolean(pendingTwoFactorChallenge);
    codeField.classList.toggle("hidden", !pendingTwoFactorChallenge);
    username.disabled = Boolean(pendingTwoFactorChallenge);
    password.disabled = Boolean(pendingTwoFactorChallenge);
    $("login-submit").querySelector(".auth-submit-label").textContent = pendingTwoFactorChallenge ? "验证并登录" : "登录";
    if (pendingTwoFactorChallenge) {
      $("login-code").value = "";
      $("login-code").focus();
    }
  }

  function arrayBufferToBase64URL(value) {
    const bytes = new Uint8Array(value);
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
  }

  function base64URLToArrayBuffer(value) {
    const padded = String(value).replace(/-/g, "+").replace(/_/g, "/") + "===".slice((String(value).length + 3) % 4);
    const binary = atob(padded);
    const bytes = new Uint8Array(binary.length);
    for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
    return bytes.buffer;
  }

  function passkeyCredentialJSON(credential, registration) {
    const response = credential.response;
    const result = {
      id: credential.id,
      rawId: arrayBufferToBase64URL(credential.rawId),
      type: credential.type,
      response: {
        clientDataJSON: arrayBufferToBase64URL(response.clientDataJSON),
      },
    };
    if (registration) {
      result.response.attestationObject = arrayBufferToBase64URL(response.attestationObject);
    } else {
      result.response.authenticatorData = arrayBufferToBase64URL(response.authenticatorData);
      result.response.signature = arrayBufferToBase64URL(response.signature);
      if (response.userHandle) result.response.userHandle = arrayBufferToBase64URL(response.userHandle);
    }
    return result;
  }

  function publicKeyRequestOptions(options) {
    const publicKey = { ...options };
    publicKey.challenge = base64URLToArrayBuffer(publicKey.challenge);
    if (Array.isArray(publicKey.allowCredentials)) {
      publicKey.allowCredentials = publicKey.allowCredentials.map((item) => ({ ...item, id: base64URLToArrayBuffer(item.id) }));
    }
    return publicKey;
  }

  function publicKeyCreationOptions(options) {
    const publicKey = { ...options };
    publicKey.challenge = base64URLToArrayBuffer(publicKey.challenge);
    publicKey.user = { ...publicKey.user, id: base64URLToArrayBuffer(publicKey.user.id) };
    if (Array.isArray(publicKey.excludeCredentials)) {
      publicKey.excludeCredentials = publicKey.excludeCredentials.map((item) => ({ ...item, id: base64URLToArrayBuffer(item.id) }));
    }
    return publicKey;
  }

  async function passkeyLogin() {
    if (authInFlight) return;
    if (!window.PublicKeyCredential || !navigator.credentials) {
      setAuthMessage("当前浏览器不支持 Passkey");
      return;
    }
    authInFlight = true;
    const passkeyButton = $("passkey-login-button");
    passkeyButton.disabled = true;
    passkeyButton.classList.add("is-loading");
    $("login-submit").disabled = true;
    setAuthMessage("正在等待 Passkey 验证…", "pending");
    try {
      const data = await apiRequest("auth/passkey/options", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
      const credential = await navigator.credentials.get({ publicKey: publicKeyRequestOptions(data.publicKey) });
      if (!credential) throw new Error("Passkey 验证已取消");
      const result = await apiRequest("auth/passkey/verify", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ challenge: data.challenge, credential: passkeyCredentialJSON(credential, false) }),
      });
      setTwoFactorChallenge("");
      showAppForUser(result && result.user);
      setAuthMessage("");
      await startDrive();
    } catch (error) {
      setAuthMessage(error.message || "Passkey 登录失败，请重试");
    } finally {
      authInFlight = false;
      passkeyButton.disabled = false;
      passkeyButton.classList.remove("is-loading");
      $("login-submit").disabled = false;
    }
  }

  async function submitAuth(event) {
    event.preventDefault();
    if (authInFlight || loginMethod !== "password") return;
    const username = $("login-username").value.trim();
    const password = $("login-password").value;
    const code = $("login-code").value.trim();
    if (pendingTwoFactorChallenge && !code) {
      setAuthMessage("请输入两步验证码");
      return;
    }
    if (!pendingTwoFactorChallenge && (!username || !password)) {
      setAuthMessage("请输入用户名和密码");
      return;
    }
    authInFlight = true;
    const submit = $("login-submit");
    submit.disabled = true;
    submit.classList.add("is-loading");
    setAuthMessage("正在登录…", "pending");
    try {
      const data = await apiRequest(pendingTwoFactorChallenge ? "auth/2fa/verify" : "auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(pendingTwoFactorChallenge
          ? { challenge: pendingTwoFactorChallenge, code }
          : { username, password }),
      });
      if (!pendingTwoFactorChallenge && data && data.status === "two_factor_required") {
        setTwoFactorChallenge(data.challenge);
        setAuthMessage("请输入验证器中的验证码，或使用一次性恢复码", "pending");
        return;
      }
      setTwoFactorChallenge("");
      showAppForUser(data && data.user);
      setAuthMessage("");
      await startDrive();
    } catch (error) {
      if (error && error.code === "auth_invalid_factor") $("login-code").focus();
      setAuthMessage(error.message || "操作失败，请稍后重试");
      if (pendingTwoFactorChallenge && error && error.code === "auth_challenge_expired") {
        setTwoFactorChallenge("");
        $("login-password").focus();
      }
      if (pendingTwoFactorChallenge && error && error.code === "auth_invalid_factor") return;
    } finally {
      authInFlight = false;
      submit.disabled = false;
      submit.classList.remove("is-loading");
    }
  }

  function togglePassword() {
    const input = $("login-password");
    const visible = input.type === "text";
    input.type = visible ? "password" : "text";
    $("password-toggle-icon").setAttribute("href", visible ? "#i-eye" : "#i-eye-off");
    $("password-toggle").title = visible ? "显示密码" : "隐藏密码";
    $("password-toggle").setAttribute("aria-label", visible ? "显示密码" : "隐藏密码");
    input.focus();
  }

  async function initWebAuth() {
    try {
      const data = await apiRequest("auth/me");
      if (!data || data.authenticated !== true) {
        showLoginScreen();
        return false;
      }
      showAppForUser(data.user);
      return true;
    } catch (error) {
      // A local-only deployment may intentionally disable Basic Auth. In
      // that mode the auth route is absent and the file manager stays open.
      if (error && error.status === 404) {
        showAppForUser(null);
        return true;
      }
      showLoginScreen();
      setAuthMessage("无法连接服务，请刷新页面重试");
      return false;
    }
  }

  async function logout() {
    if (authInFlight) return;
    try {
      await apiRequest("auth/logout", { method: "POST" });
    } catch (_) {
      // The local session is unusable even when the network is already down.
    }
    window.location.reload();
  }

  /* ============ 目录缓存与预取 ============ */
  const directoryCache = new Map();
  let directoryCacheEpoch = 0;
  let prefetchQueue = [];
  let prefetchActive = 0;
  let prefetchGeneration = 0;
  let navigationGeneration = 0;
  let rootName = "WPS Drive";

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

  /* ============ 状态行 / 通知 ============ */
  function setStatus(message, kind = "") {
    const status = $("status");
    status.textContent = message;
    status.parentElement.className = "status-row" + (kind ? " " + kind : "");
    const statusIcon = $("status-icon");
    if (statusIcon) {
      const iconName = kind === "success" ? "check" : kind === "error" ? "alert" : kind === "pending" ? "clock" : "info";
      statusIcon.querySelector("use").setAttribute("href", `#i-${iconName}`);
    }
  }

  function toast(message, kind = "info", ttl = 4200) {
    const wrap = $("toasts");
    while (wrap.children.length >= 3) wrap.firstElementChild.remove();
    const node = el("div", "toast " + kind);
    const iconName = kind === "success" ? "check" : kind === "error" ? "alert"
      : kind === "warn" ? "warn" : "info";
    const iconWrap = el("span", "toast-icon");
    iconWrap.append(icon(iconName));
    node.append(iconWrap, el("span", "toast-text", message));
    const close = el("button", "toast-close");
    close.type = "button";
    close.title = "关闭";
    close.setAttribute("aria-label", "关闭通知");
    close.append(icon("x"));
    close.addEventListener("click", () => dismiss());
    node.append(close);
    let gone = false;
    function dismiss() {
      if (gone) return;
      gone = true;
      clearTimeout(timer);
      node.classList.add("out");
      node.addEventListener("animationend", () => node.remove(), { once: true });
      setTimeout(() => node.remove(), 400);
    }
    let timer = setTimeout(dismiss, Math.max(1500, ttl));
    const pause = () => clearTimeout(timer);
    const resume = () => { if (!gone) timer = setTimeout(dismiss, Math.max(1500, ttl)); };
    node.addEventListener("mouseenter", pause);
    node.addEventListener("mouseleave", resume);
    node.addEventListener("focusin", pause);
    node.addEventListener("focusout", resume);
    wrap.append(node);
  }

  /* ============ 连接状态 ============ */
  function updateControls() {
    const unavailable = state.connection !== "connected";
    const virtualRoot = state.path === "/" && state.spaces.length > 0;
    $("settings-button").disabled = state.busy;
    $("sidebar-refresh-button").disabled = state.busy;
    $("sidebar-settings-button").disabled = state.busy;
    // Local navigation remains available when WPS is temporarily down. It
    // lets a user return to the root or another cached location.
    $("up-button").disabled = state.busy || state.path === "/";
    $("refresh-button").disabled = state.busy;
    [$("folder-button"), $("upload-button")].forEach((button) => {
      button.disabled = state.busy || unavailable || virtualRoot;
    });
    // Empty-state retry and search actions remain usable while WPS is down;
    // only remote entry operations depend on a connected upstream.
    document.querySelectorAll("#entries .action-button, #entries .action-menu-trigger").forEach((button) => {
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
      not_configured: "WPS 尚未配置",
      session_expired: "WPS 登录已过期",
      permission_denied: "无权访问当前工作区",
      upstream_unavailable: "WPS 暂时不可用",
      invalid_response: "WPS 响应异常",
      unknown: "WPS 状态未知",
    };
    const visualClass = state.connection === "connected"
      ? "connected"
      : state.connection === "checking"
        ? "checking"
        : ["not_configured", "session_expired", "permission_denied"].includes(state.connection)
          ? "disconnected"
          : "unknown";
    badge.className = "connection " + visualClass;
    $("connection-label").textContent = labels[state.connection] || labels.unknown;
    const iconNames = {
      checking: "clock",
      connected: "check",
      not_configured: "warn",
      session_expired: "warn",
      permission_denied: "warn",
      upstream_unavailable: "alert",
      invalid_response: "alert",
      unknown: "info",
    };
    $("connection-icon").querySelector("use").setAttribute("href", `#i-${iconNames[state.connection] || "info"}`);
    updateStatusPanel();
    updateControls();
  }

  function updateStatusPanel() {
    const panel = $("status-panel-wps");
    if (!panel) return;
    const labels = {
      checking: "正在检查",
      connected: "已连接，可访问当前空间",
      not_configured: "尚未配置 WPS 凭据",
      session_expired: "登录已过期",
      permission_denied: "当前空间没有访问权限",
      upstream_unavailable: "WPS 暂时不可用",
      invalid_response: "WPS 返回格式异常",
      unknown: "状态未知",
    };
    const indicator = panel.querySelector(".status-indicator");
    const indicatorClass = state.connection === "connected" ? "ok"
      : state.connection === "checking" ? "checking"
        : ["not_configured", "session_expired", "permission_denied"].includes(state.connection) ? "warn" : "bad";
    indicator.className = "status-indicator " + indicatorClass;
    $("status-panel-wps-label").textContent = "WPS";
    $("status-panel-wps-state").textContent = labels[state.connection] || labels.unknown;
    $("status-panel-message").textContent = state.connection === "connected"
      ? "当前会话可用，文件操作会按当前 WPS 权限执行。"
      : connectionMessage(state.connection);
  }

  function toggleStatusPanel(open) {
    const panel = $("status-panel");
    const shouldOpen = open === undefined ? panel.hidden : open;
    panel.hidden = !shouldOpen;
    $("connection").setAttribute("aria-expanded", String(shouldOpen));
    if (shouldOpen) updateStatusPanel();
  }

  function connectionMessage(value) {
    const messages = {
      not_configured: "WPS 尚未配置，请先在自己的电脑运行 wps_login.py 同步凭据，然后点击刷新",
      session_expired: "WPS 登录已过期，请重新运行 wps_login.py 同步凭据，然后点击刷新",
      permission_denied: "无权访问当前工作区，请检查登录账号或重新选择工作区",
      upstream_unavailable: "WPS 暂时不可用，请稍后点击刷新重试",
      invalid_response: "WPS 返回了无法识别的响应，请稍后点击刷新重试",
      unknown: "暂时无法判断 WPS 状态，请点击刷新重试",
    };
    return messages[value] || messages.unknown;
  }

  function isVirtualSpaceEntry(entry) {
    return Boolean(entry && entry.kind === "folder" && typeof entry.id === "string" && entry.id.startsWith("space:"));
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

  function showError(error, { notify = true } = {}) {
    if (error && error.status === 401) {
      showLoginScreen();
      toggleStatusPanel(false);
      setAuthMessage("登录已过期，请重新登录");
      return;
    }
    if (isWpsError(error)) {
      const connection = error.code === "wps_session_expired"
        ? "session_expired"
        : "upstream_unavailable";
      setConnection(connection);
      setStatus(connectionMessage(connection), "error");
      if (notify) toast(connectionMessage(connection), "error", 6000);
      return;
    }
    const messages = {
      403: "没有权限执行此操作",
      404: "文件或文件夹不存在，可能已被删除",
      409: "名称冲突或目标位置不允许此操作",
      413: "文件太大，超过服务限制",
      502: "WPS 暂时不可用，请稍后重试",
      503: "服务暂时繁忙，请稍后重试",
      507: "可用空间不足或已达到服务限制",
    };
    const friendlyMessage = error && messages[error.status];
    if (friendlyMessage) {
      setStatus(friendlyMessage, "error");
      if (notify) toast(friendlyMessage, "error", 6000);
      return;
    }
    if (state.connection === "checking") setConnection("unknown");
    setStatus(error.message, "error");
    if (notify) toast(error.message, "error", 6000);
  }

  /* ============ 路径 ============ */
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

  /* ============ hash 路由 ============ */
  let suppressHash = false;

  function hashPath() {
    const raw = location.hash.replace(/^#/, "");
    if (!raw) return "/";
    try { return canonicalPath(decodeURIComponent(raw)); }
    catch (_) { return "/"; }
  }

  function syncHash(path) {
    const target = "#" + path;
    if (location.hash === target) return;
    suppressHash = true;
    location.hash = target;
  }

  window.addEventListener("hashchange", () => {
    if (suppressHash) { suppressHash = false; return; }
    const target = hashPath();
    if (target !== state.path) load(target, false, true);
  });

  /* ============ 排序 ============ */
  const SORT_DEFAULT_DIR = { name: "asc", size: "desc", time: "desc" };

  function saveSort() {
    PREF.set("sort", state.sort.key + ":" + state.sort.dir);
  }

  function loadSort() {
    const raw = PREF.get("sort", "auto:asc");
    const [key, dir] = raw.split(":");
    if (["auto", "name", "size", "time"].includes(key)) {
      state.sort.key = key;
      state.sort.dir = dir === "desc" ? "desc" : "asc";
    }
  }

  function sortedEntries(entries) {
    const { key, dir } = state.sort;
    if (key === "auto") {
      return entries.slice().sort((a, b) => {
        const folderDelta = (b.kind === "folder") - (a.kind === "folder");
        if (folderDelta) return folderDelta;
        return String(a.name).localeCompare(String(b.name), "zh-Hans-CN", { numeric: true });
      });
    }
    const sign = dir === "desc" ? -1 : 1;
    return entries.slice().sort((a, b) => {
      const folderDelta = (b.kind === "folder") - (a.kind === "folder");
      if (folderDelta) return folderDelta;
      if (key === "size") return sign * ((Number(a.size) || 0) - (Number(b.size) || 0));
      if (key === "time") return sign * ((Number(a.modified_at) || 0) - (Number(b.modified_at) || 0));
      return sign * String(a.name).localeCompare(String(b.name), "zh-Hans-CN", { numeric: true });
    });
  }

  function renderSortControls() {
    const { key, dir } = state.sort;
    $("sort-select").value = key;
    const dirButton = $("sort-dir-button");
    dirButton.disabled = key === "auto";
    $("sort-dir-icon").setAttribute("href", dir === "desc" ? "#i-chev-down" : "#i-chev-up");
    dirButton.title = dir === "desc" ? "当前降序（点击切换为升序）" : "当前升序（点击切换为降序）";
    document.querySelectorAll("#list-head .sort-button").forEach((button) => {
      const cell = button.closest(".cell");
      if (button.dataset.sort === key && key !== "auto") {
        button.dataset.active = "true";
        button.dataset.dir = dir;
        cell.setAttribute("aria-sort", dir === "desc" ? "descending" : "ascending");
      } else {
        delete button.dataset.active;
        delete button.dataset.dir;
        cell.removeAttribute("aria-sort");
      }
    });
  }

  function setSort(key, dir) {
    state.sort = { key, dir };
    saveSort();
    renderSortControls();
    renderEntries({ animate: true });
  }

  /* ============ 视图切换 ============ */
  function setView(view, { animate = true } = {}) {
    state.view = view === "grid" ? "grid" : "list";
    PREF.set("view", state.view);
    $("view-toggle").dataset.view = state.view;
    $("view-list-button").setAttribute("aria-pressed", String(state.view === "list"));
    $("view-grid-button").setAttribute("aria-pressed", String(state.view === "grid"));
    $("list-head").hidden = state.view === "grid";
    if (animate) renderEntries({ animate: true });
  }

  /* ============ 文件类型 ============ */
  const EXT_KINDS = [
    [["jpg", "jpeg", "png", "gif", "webp", "svg", "bmp", "ico", "avif", "heic"], "image", "tint-cyan", "图片"],
    [["mp4", "mov", "avi", "mkv", "webm", "flv", "m4v", "wmv"], "video", "tint-red", "视频"],
    [["mp3", "wav", "flac", "aac", "ogg", "m4a", "wma"], "music", "tint-cyan", "音频"],
    [["zip", "rar", "7z", "tar", "gz", "bz2", "xz", "iso"], "archive", "tint-slate", "压缩包"],
    [["doc", "docx", "txt", "rtf", "md", "pages"], "file-text", "tint-blue", "文档"],
    [["xls", "xlsx", "csv", "numbers"], "sheet", "tint-green", "表格"],
    [["ppt", "pptx", "key"], "file-text", "tint-red", "幻灯片"],
    [["pdf"], "file-text", "tint-red", "PDF"],
    [["js", "ts", "html", "css", "json", "py", "go", "java", "c", "cpp", "h", "sh", "yml", "yaml", "xml", "sql"], "code", "tint-slate", "代码"],
  ];

  function extKind(name) {
    const dot = String(name).lastIndexOf(".");
    if (dot < 0 || dot === name.length - 1) return { icon: "file", tint: "tint-slate", label: "文件" };
    const ext = String(name).slice(dot + 1).toLowerCase();
    for (const [list, iconName, tint, label] of EXT_KINDS) {
      if (list.includes(ext)) return { icon: iconName, tint, label };
    }
    return { icon: "file", tint: "tint-blue", label: "文件" };
  }

  function glyphNode(entry, large = false) {
    const spec = isVirtualSpaceEntry(entry)
      ? { icon: "cloud", tint: "tint-blue" }
      : entry.kind === "folder"
        ? { icon: "folder", tint: "tint-amber" }
      : extKind(entry.name);
    const glyph = el("span", "entry-glyph " + spec.tint + (large ? " lg" : ""));
    glyph.setAttribute("aria-hidden", "true");
    glyph.append(icon(spec.icon));
    return glyph;
  }

  /* ============ 格式化 ============ */
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

  function formatShortTime(value) {
    const timestamp = Number(value);
    if (!Number.isFinite(timestamp) || timestamp <= 0) return "-";
    const date = new Date(timestamp * 1000);
    const now = new Date();
    const sameYear = date.getFullYear() === now.getFullYear();
    return date.toLocaleDateString("zh-CN", {
      month: "numeric", day: "numeric",
      year: sameYear ? undefined : "numeric",
    });
  }

  /* ============ 搜索高亮 ============ */
  function escapeRegExp(text) {
    return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  }

  function highlightedName(name, query) {
    if (!query) return document.createTextNode(name);
    const pattern = new RegExp(escapeRegExp(query), "ig");
    const fragment = document.createDocumentFragment();
    let last = 0;
    let match;
    while ((match = pattern.exec(String(name))) !== null) {
      if (match.index > last) fragment.append(document.createTextNode(String(name).slice(last, match.index)));
      fragment.append(el("mark", "", match[0]));
      last = match.index + match[0].length;
      if (match[0].length === 0) pattern.lastIndex += 1;
    }
    if (last < String(name).length) fragment.append(document.createTextNode(String(name).slice(last)));
    return fragment;
  }

  /* ============ 面包屑 ============ */
  function renderBreadcrumbs() {
    const nav = $("breadcrumbs");
    nav.replaceChildren();
    let path = "/";
    const parts = state.path.split("/").filter(Boolean);
    const root = el("button", "crumb" + (parts.length ? "" : " current"));
    root.type = "button";
    root.disabled = !parts.length;
    root.append(icon("home"));
    root.append(el("span", "", rootName));
    root.addEventListener("click", () => load("/"));
    nav.append(root);
    parts.forEach((part, index) => {
      const separator = el("span", "crumb-separator");
      separator.append(icon("chev-right"));
      nav.append(separator);
      path = joinPath(path, part);
      const crumb = el("button", "crumb" + (index === parts.length - 1 ? " current" : ""));
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
    $("drop-target").textContent = state.path;
    document.title = title === rootName ? rootName : `${title} · ${rootName}`;
    updateControls();
  }

  function closeMobileNav() {
    document.body.classList.remove("nav-open");
    const button = $("nav-menu-button");
    if (button) button.setAttribute("aria-expanded", "false");
    const scrim = $("mobile-scrim");
    if (scrim) scrim.hidden = true;
    if (button && document.contains(button)) button.focus();
  }

  function openMobileNav() {
    document.body.classList.add("nav-open");
    const button = $("nav-menu-button");
    if (button) button.setAttribute("aria-expanded", "true");
    const scrim = $("mobile-scrim");
    if (scrim) scrim.hidden = false;
    setTimeout(() => { if ($("sidebar-close") && document.body.classList.contains("nav-open")) $("sidebar-close").focus(); }, 0);
  }

  function renderSpaceNav(rootEntries = null) {
    if (Array.isArray(rootEntries)) {
      state.spaces = rootEntries.filter(isVirtualSpaceEntry).map((entry) => ({
        name: entry.name,
        path: joinPath("/", entry.name),
      }));
    }
    const root = $("space-root");
    const list = $("space-list");
    if (!root || !list) return;
    root.classList.toggle("active", state.path === "/");
    list.replaceChildren();
    state.spaces.forEach((space) => {
      const active = state.path === space.path || state.path.startsWith(space.path + "/");
      const button = el("button", "space-item" + (active ? " active" : ""));
      button.type = "button";
      button.title = space.name;
      button.setAttribute("aria-label", `进入空间 ${space.name}`);
      const iconWrap = el("span", "space-item-icon");
      iconWrap.append(icon("cloud"));
      button.append(iconWrap, el("span", "space-item-name", space.name));
      button.addEventListener("click", () => {
        closeMobileNav();
        load(space.path);
      });
      list.append(button);
    });
    updateControls();
  }

  /* ============ 骨架屏 ============ */
  function renderSkeleton() {
    const holder = $("skeleton");
    holder.classList.remove("hidden");
    holder.dataset.view = state.view;
    holder.replaceChildren();
    for (let index = 0; index < 8; index += 1) {
      if (state.view === "grid") {
        const card = el("div", "sk-card");
        card.append(el("div", "sk sk-glyph"));
        const long = el("div", "sk sk-line"); long.style.width = "82%";
        const mid = el("div", "sk sk-line"); mid.style.width = "55%";
        card.append(long, mid, el("div", "sk sk-line"));
        holder.append(card);
      } else {
        const row = el("div", "sk-row");
        const nameCell = el("div", "sk-cell");
        nameCell.append(el("div", "sk sk-glyph"));
        const line = el("div", "sk sk-line"); line.style.width = "46%";
        nameCell.append(line);
        row.append(nameCell, el("div", "sk sk-line"), el("div", "sk sk-line"), el("div", "sk sk-line"), el("div", "sk sk-line"));
        holder.append(row);
      }
    }
  }

  function hideSkeleton() {
    $("skeleton").classList.add("hidden");
  }

  /* ============ 条目渲染 ============ */
  function filteredEntries() {
    const query = state.search.trim().toLocaleLowerCase();
    if (!query) return state.entries;
    return state.entries.filter((entry) => entry.name.toLocaleLowerCase().includes(query));
  }

  function updateSearchControls() {
    const input = $("search-input");
    const clear = $("search-clear");
    const hasQuery = Boolean(input.value);
    clear.hidden = !hasQuery;
    clear.tabIndex = hasQuery ? 0 : -1;
  }

  function clearSearch() {
    state.search = "";
    $("search-input").value = "";
    updateSearchControls();
    renderEntries({ animate: false });
  }

  function actionButton(label, title, iconName, handler, danger = false) {
    const button = el("button", "action-button" + (danger ? " danger" : ""));
    button.type = "button";
    button.title = title;
    button.setAttribute("aria-label", title);
    button.append(icon(iconName, "action-icon"));
    button.append(el("span", "label", label));
    button.addEventListener("click", handler);
    return button;
  }

  let openActionMenu = null;

  function closeActionMenu(restoreFocus = false) {
    if (!openActionMenu) return;
    const menu = openActionMenu;
    const row = menu.closest(".list-row, .card");
    menu.classList.remove("open");
    if (row) row.classList.remove("has-open-menu");
    const trigger = menu.querySelector(".action-menu-trigger");
    if (trigger) trigger.setAttribute("aria-expanded", "false");
    openActionMenu = null;
    if (restoreFocus && trigger && document.contains(trigger)) trigger.focus();
  }

  function menuItem(label, title, iconName, handler, danger = false) {
    const item = el("button", "menu-item" + (danger ? " danger" : ""));
    item.type = "button";
    item.setAttribute("role", "menuitem");
    item.title = title;
    item.append(icon(iconName), el("span", "label", label));
    item.addEventListener("click", () => {
      closeActionMenu();
      handler();
    });
    return item;
  }

  function entryMenu(entry, entryPath) {
    const menu = el("div", "action-menu");
    const trigger = el("button", "action-menu-trigger icon-button small");
    trigger.type = "button";
    trigger.title = "更多操作";
    trigger.setAttribute("aria-label", `更多操作：${entry.name}`);
    trigger.setAttribute("aria-haspopup", "menu");
    trigger.setAttribute("aria-expanded", "false");
    trigger.append(icon("more"));

    const popover = el("div", "action-menu-popover");
    popover.setAttribute("role", "menu");
    popover.append(
      menuItem("重命名", "重命名", "pencil", () => rename(entry, entryPath)),
      menuItem("移动", "移动到其他文件夹", "move", () => move(entry, entryPath)),
      menuItem("删除", "删除", "trash", () => remove(entry, entryPath), true),
    );

    trigger.addEventListener("click", (event) => {
      event.stopPropagation();
      if (openActionMenu === menu) {
        closeActionMenu();
        return;
      }
      closeActionMenu();
      openActionMenu = menu;
      const row = menu.closest(".list-row, .card");
      if (row) row.classList.add("has-open-menu");
      menu.classList.add("open");
      trigger.setAttribute("aria-expanded", "true");
      const first = popover.querySelector(".menu-item");
      if (first) setTimeout(() => first.focus(), 0);
    });
    menu.append(trigger, popover);
    return menu;
  }

  function entryActions(entry, entryPath) {
    const actions = el("div", "actions");
    if (isVirtualSpaceEntry(entry)) return actions;
    if (state.pendingDeletes.has(entryPath)) {
      actions.append(el("span", "entry-pending", "正在删除"));
      return actions;
    }
    if (entry.kind === "file") {
      let downloadButton;
      downloadButton = actionButton("下载", "下载文件", "download", () => download(entry, entryPath, downloadButton));
      actions.append(downloadButton);
    }
    actions.append(entryMenu(entry, entryPath));
    return actions;
  }

  function selectEntry(entryPath) {
    closeActionMenu();
    state.selectedPath = entryPath;
    document.querySelectorAll("[data-entry-path]").forEach((node) => {
      const selected = node.dataset.entryPath === entryPath;
      node.classList.toggle("is-selected", selected);
      node.querySelectorAll(".entry-name, .card-name").forEach((button) => {
        button.setAttribute("aria-pressed", String(selected));
      });
    });
  }

  function openTarget(entry, entryPath) {
    if (entry.kind === "folder") load(entryPath);
    else selectEntry(entryPath);
  }

  function directoryErrorFor(error) {
    if (!error) return null;
    if (isWpsError(error) || error.status === 401) return null;
    if (error.status === 403) {
      return { mode: "permission", title: "没有权限访问此目录", note: "请返回上一级或检查当前 WPS 空间权限" };
    }
    if (error.status === 404) {
      return { mode: "missing", title: "此目录不存在或已被移动", note: "请刷新或返回上一级" };
    }
    return { mode: "read-error", title: "目录读取失败", note: "暂时无法读取此目录，请重试" };
  }

  function renderEmpty(entries) {
    const empty = $("empty");
    empty.classList.toggle("hidden", entries.length !== 0 || state.loading);
    if (entries.length || state.loading) return;
    const unavailable = state.connection !== "connected" && state.connection !== "checking";
    const mode = state.directoryError ? state.directoryError.mode : unavailable ? "offline" : state.entries.length ? "search" : "empty";
    empty.dataset.mode = mode;
    const art = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    art.setAttribute("class", "illustration");
    const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    const illustration = mode === "search" ? "#i-ill-search"
      : mode === "offline" ? "#i-ill-offline"
        : mode === "missing" ? "#i-folder" : mode === "permission" || mode === "read-error" ? "#i-alert" : "#i-ill-empty";
    use.setAttribute("href", illustration);
    art.append(use);
    const title = el("strong");
    const note = el("span");
    if (state.directoryError) {
      title.textContent = state.directoryError.title;
      note.textContent = state.directoryError.note;
    } else if (unavailable) {
      title.textContent = state.connection === "permission_denied"
        ? "无权访问当前工作区"
        : state.connection === "session_expired"
          ? "WPS 登录已过期"
          : state.connection === "not_configured"
            ? "WPS 尚未配置"
            : "暂时无法读取目录";
      note.textContent = connectionMessage(state.connection);
    } else if (state.entries.length) {
      title.textContent = "没有匹配的项目";
      note.textContent = "换一个关键词试试";
    } else {
      title.textContent = "这个文件夹还是空的";
      note.textContent = "上传文件或新建文件夹开始使用";
    }
    const actions = el("div", "empty-actions");
    if (mode === "search") {
      actions.append(actionButton("清除搜索", "清除搜索条件", "x", clearSearch));
    } else if (mode !== "empty" || state.path !== "/") {
      actions.append(actionButton("重试", "重新读取当前目录", "refresh", () => load(state.path, false, true)));
      if (state.path !== "/") {
        actions.append(actionButton("返回上一级", "返回上一级目录", "up", () => load(parentPath(state.path))));
      }
    }
    empty.replaceChildren(art, title, note, actions);
  }

  function renderEntries({ animate = false } = {}) {
    if (state.loading && !state.refreshing) {
      // 目录加载中：只展示骨架屏，避免把上一次的数据闪出来。
      renderSkeleton();
      return;
    }
    const holder = $("entries");
    hideSkeleton();
    const entries = sortedEntries(filteredEntries());
    const isGrid = state.view === "grid";
    holder.classList.toggle("is-animating", animate);
    holder.setAttribute("role", isGrid ? "list" : "table");
    holder.replaceChildren();
    renderEmpty(entries);
    const query = state.search.trim().toLocaleLowerCase();
    updateSearchControls();
    entries.forEach((entry, index) => {
      const entryPath = joinPath(state.path, entry.name);
      let node;
      if (isGrid) {
        node = el("div", "card");
        node.setAttribute("role", "listitem");
        const top = el("div", "card-top");
        top.append(glyphNode(entry, true));
        if (entry.kind === "folder") top.append(icon("chev-right", "card-chev"));
        const name = el("button", "card-name");
        name.type = "button";
        name.title = entry.name;
        name.setAttribute("aria-label", `${entry.kind === "folder" ? "打开文件夹" : "选择文件"}：${entry.name}`);
        if (entry.kind === "file") name.setAttribute("aria-pressed", String(state.selectedPath === entryPath));
        name.append(highlightedName(entry.name, query));
        name.addEventListener("click", () => openTarget(entry, entryPath));
        const typeLabel = isVirtualSpaceEntry(entry) ? "WPS 空间" : entry.kind === "folder" ? "文件夹" : extKind(entry.name).label;
        const meta = el("div", "card-meta",
          entry.kind === "folder"
            ? typeLabel
            : `${typeLabel} · ${formatBytes(entry.size)} · ${formatShortTime(entry.modified_at)}`);
        const ops = el("div", "card-ops");
        ops.append(entryActions(entry, entryPath));
        node.append(top, name, meta, ops);
      } else {
        node = el("div", "list-row");
        node.setAttribute("role", "row");
        const nameCell = el("div", "cell name name-cell");
        nameCell.setAttribute("role", "cell");
        nameCell.append(glyphNode(entry));
        const nameStack = el("div", "name-stack");
        const name = el("button", "entry-name" + (entry.kind === "folder" ? " folder" : ""));
        name.type = "button";
        name.title = entry.name;
        name.setAttribute("aria-label", `${entry.kind === "folder" ? "打开文件夹" : "选择文件"}：${entry.name}`);
        if (entry.kind === "file") name.setAttribute("aria-pressed", String(state.selectedPath === entryPath));
        name.append(highlightedName(entry.name, query));
        name.addEventListener("click", () => openTarget(entry, entryPath));
        nameStack.append(name);
        const typeLabel = isVirtualSpaceEntry(entry) ? "WPS 空间" : entry.kind === "folder" ? "文件夹" : extKind(entry.name).label;
        const mobileMeta = el("div", "entry-mobile-meta",
          entry.kind === "folder" ? `${typeLabel} · ${formatShortTime(entry.modified_at)}` :
            `${typeLabel} · ${formatBytes(entry.size)} · ${formatShortTime(entry.modified_at)}`);
        nameStack.append(mobileMeta);
        nameCell.append(nameStack);
        const typeCell = el("div", "cell type meta", typeLabel);
        typeCell.setAttribute("role", "cell");
        const sizeCell = el("div", "cell size meta",
          entry.kind === "folder" ? "—" : formatBytes(entry.size));
        sizeCell.setAttribute("role", "cell");
        const timeCell = el("div", "cell time meta", formatTime(entry.modified_at));
        timeCell.setAttribute("role", "cell");
        const opsCell = el("div", "cell ops");
        opsCell.setAttribute("role", "cell");
        opsCell.append(entryActions(entry, entryPath));
        node.append(nameCell, typeCell, sizeCell, timeCell, opsCell);
      }
      node.dataset.entryPath = entryPath;
      if (state.selectedPath === entryPath) node.classList.add("is-selected");
      if (animate) node.style.setProperty("--i", String(Math.min(index, 14)));
      if (state.pendingDeletes.has(entryPath)) node.classList.add("is-pending");
      holder.append(node);
    });
    updateControls();
    const total = state.entries.length;
    const visible = entries.length;
    const summary = el("span");
    if (state.search.trim()) {
      const strong = el("strong", "", String(visible));
      summary.append(strong, document.createTextNode(` 个匹配项目，共 ${total} 个`));
    } else {
      const strong = el("strong", "", String(total));
      summary.append(strong, document.createTextNode(" 个项目"));
    }
    const panelSummary = $("panel-summary");
    panelSummary.replaceChildren(summary);
  }

  /* ============ 连接检查与加载 ============ */
  let connectionCheck = null;

  async function checkConnection(quiet = false) {
    if (connectionCheck) return connectionCheck;
    connectionCheck = (async () => {
      const previousConnection = state.connection;
      setConnection("checking");
      try {
        const data = await apiRequest("status");
        const value = data && typeof data.status === "string" ? data.status : "invalid_response";
        if (value === "connected" && previousConnection !== "connected") {
          clearDirectoryCache();
        }
        setConnection(value);
        if (value !== "connected" && (!quiet || state.entries.length === 0)) {
          setStatus(connectionMessage(state.connection), "error");
        }
        return state.connection;
      } catch (error) {
        showError(error, { notify: !quiet });
        return state.connection;
      }
    })();
    try {
      return await connectionCheck;
    } finally {
      connectionCheck = null;
    }
  }

  async function load(path, quiet = false, force = false) {
    const targetPath = canonicalPath(path);
    const previousPath = state.path;
    const preserveCurrentList = targetPath === state.path && force && state.entries.length > 0;
    const requestGeneration = ++navigationGeneration;
    closeActionMenu();
    if (targetPath !== previousPath) state.selectedPath = "";
    state.path = targetPath;
    state.search = "";
    $("search-input").value = "";
    updateSearchControls();
    state.directoryError = null;
    syncHash(targetPath);
    renderBreadcrumbs();
    state.loading = true;
    state.refreshing = preserveCurrentList;
    $("refresh-button").classList.add("busy");
    $("refresh-progress").hidden = !state.refreshing;
    if (!quiet) setStatus(state.refreshing ? "正在刷新..." : "正在读取...");
    if (!state.refreshing) renderSkeleton();
    renderEntries({ animate: false });
    try {
      const connection = await checkConnection(quiet);
      if (connection !== "connected") {
        if (requestGeneration !== navigationGeneration) return;
        state.loading = false;
        const cached = !force ? directoryCache.get(targetPath) : null;
        const fallbackEntries = preserveCurrentList
          ? state.entries
          : cached && Array.isArray(cached.entries) ? cached.entries : [];
        state.entries = fallbackEntries;
        if (state.entries.length) {
          if (targetPath === "/") renderSpaceNav(state.entries);
          setStatus(`${connectionMessage(connection)}（显示缓存内容，尚未确认最新状态）`, "error");
        }
        renderEntries({ animate: true });
        return;
      }
      const entries = await directoryEntries(targetPath, force);
      if (requestGeneration !== navigationGeneration) return;
      state.loading = false;
      state.refreshing = false;
      $("refresh-progress").hidden = true;
      state.entries = entries;
      state.directoryError = null;
      setConnection("connected");
      if (targetPath === "/") {
        renderSpaceNav(entries);
      } else if (state.spaces.length === 0) {
        directoryEntries("/").then((rootEntries) => renderSpaceNav(rootEntries)).catch(() => {});
      } else {
        renderSpaceNav();
      }
      renderEntries({ animate: true });
      prefetchChildDirectories(targetPath, state.entries);
      setStatus(`${state.entries.length} 个项目`, "success");
    } catch (error) {
      if (requestGeneration !== navigationGeneration) return;
      state.loading = false;
      state.refreshing = false;
      $("refresh-progress").hidden = true;
      if (!preserveCurrentList) state.entries = [];
      state.directoryError = directoryErrorFor(error);
      showError(error, { notify: !quiet });
      renderEntries({ animate: true });
    } finally {
      if (requestGeneration === navigationGeneration) {
        state.loading = false;
        state.refreshing = false;
        $("refresh-progress").hidden = true;
        $("refresh-button").classList.remove("busy");
      }
    }
  }

  function download(entry, path, button = null) {
    if (button && button.disabled) return;
    if (button) {
      button.disabled = true;
      button.classList.add("busy");
    }
    const link = document.createElement("a");
    link.href = pathUrl("download", path).toString();
    // Let the server's Content-Disposition choose the filename. This
    // keeps the browser's native download lifecycle and auth handling.
    link.rel = "noopener";
    document.body.append(link);
    link.click();
    link.remove();
    toast(`已开始下载 “${entry.name}”`, "info", 2600);
    if (button) setTimeout(() => { button.disabled = false; button.classList.remove("busy"); }, 900);
  }

  /* ============ 云盘名称 ============ */
  function applyRootName() {
    document.title = rootName;
    $("brand-title").textContent = rootName;
    renderBreadcrumbs();
  }

  async function initRootName() {
    try {
      const data = await apiRequest("settings");
      if (data && typeof data.name === "string" && data.name) {
        rootName = data.name;
        applyRootName();
      }
    } catch (error) {
      setStatus("云盘名称读取失败，使用默认名称", "error");
    }
  }

  /* ============ 对话框（输入 / 确认） ============ */
  let modalResolve = null;
  let modalMode = "input";
  let modalTrigger = null;

  function closeModal(value) {
    if (!modalResolve) return;
    const resolve = modalResolve;
    modalResolve = null;
    $("modal").close();
    if (modalTrigger && document.contains(modalTrigger)) modalTrigger.focus();
    modalTrigger = null;
    resolve(value);
  }

  function openInputModal(title, label, value = "", placeholder = "", submitText = "确定") {
    modalMode = "input";
    modalTrigger = document.activeElement;
    $("modal-title").textContent = title;
    $("modal-message").classList.add("hidden");
    $("modal-label").classList.remove("hidden");
    $("modal-label-text").textContent = label;
    $("modal-input").value = value;
    $("modal-input").placeholder = placeholder;
    $("modal-submit").textContent = submitText;
    $("modal-submit").className = "primary";
    $("modal").showModal();
    setTimeout(() => $("modal-input").focus(), 0);
    return new Promise((resolve) => { modalResolve = resolve; });
  }

  function openConfirmModal(title, message, submitText = "确定", danger = false) {
    modalMode = "confirm";
    modalTrigger = document.activeElement;
    $("modal-title").textContent = title;
    $("modal-message").textContent = message;
    $("modal-message").classList.remove("hidden");
    $("modal-label").classList.add("hidden");
    $("modal-submit").textContent = submitText;
    $("modal-submit").className = danger ? "danger-solid" : "primary";
    $("modal").showModal();
    // Destructive actions and overwrite prompts both default to the
    // non-destructive choice, so Enter cannot confirm them accidentally.
    setTimeout(() => $("modal-cancel").focus(), 0);
    return new Promise((resolve) => { modalResolve = resolve; });
  }

  /* ============ 文件夹选择器 ============ */
  let pickerResolve = null;
  let pickerSourcePath = "";
  let pickerCurrent = "/";
  let pickerLoading = false;
  let pickerFirstLoad = false;
  let pickerFallback = false;
  let pickerTrigger = null;
  let pickerGeneration = 0;

  function pickerCanMoveHere() {
    if (pickerLoading) return false;
    if (pickerCurrent === parentPath(pickerSourcePath)) return false;
    if (pickerCurrent === pickerSourcePath) return false;
    if (pickerSourcePath !== "/" && pickerCurrent.startsWith(pickerSourcePath + "/")) return false;
    if (state.spaces.length > 0) {
      const sourceSpace = pickerSourcePath.split("/").filter(Boolean)[0] || "";
      const targetSpace = pickerCurrent.split("/").filter(Boolean)[0] || "";
      // The current backend does not promise cross-space moves and the
      // virtual root is not a writable destination.
      if (!sourceSpace || sourceSpace !== targetSpace) return false;
    }
    return true;
  }

  function renderPickerState() {
    $("picker-path").textContent = pickerCurrent;
    $("picker-move").disabled = !pickerCanMoveHere();
    $("picker-up").disabled = pickerLoading || pickerCurrent === "/";
  }

  function renderPickerSkeleton() {
    const list = $("picker-list");
    list.replaceChildren();
    const holder = el("div", "picker-loading");
    for (let index = 0; index < 4; index += 1) {
      const row = el("div", "sk-row");
      row.append(el("div", "sk sk-glyph"));
      const line = el("div", "sk sk-line"); line.style.width = "58%";
      row.append(line);
      holder.append(row);
    }
    list.append(holder);
  }

  async function renderPickerList() {
    const list = $("picker-list");
    const generation = ++pickerGeneration;
    const firstLoad = pickerFirstLoad;
    pickerLoading = true;
    renderPickerSkeleton();
    renderPickerState();
    try {
      const entries = await directoryEntries(pickerCurrent);
      if (pickerResolve === null || generation !== pickerGeneration) return;
      list.replaceChildren();
      const folders = entries.filter((entry) => {
        if (!entry || entry.kind !== "folder") return false;
        const full = joinPath(pickerCurrent, entry.name);
        return full !== pickerSourcePath;
      });
      if (!folders.length) {
        list.append(el("div", "picker-empty", "这里没有子文件夹"));
      }
      folders.forEach((entry) => {
        const full = joinPath(pickerCurrent, entry.name);
        const item = el("button", "picker-item");
        item.type = "button";
        item.setAttribute("role", "listitem");
        item.append(icon("folder"));
        item.append(el("span", "p-name", entry.name));
        item.append(icon("chev-right", "p-chev"));
        item.addEventListener("click", () => {
          pickerCurrent = full;
          renderPickerList();
        });
        list.append(item);
      });
      pickerLoading = false;
      pickerFirstLoad = false;
      renderPickerState();
    } catch (error) {
      if (generation !== pickerGeneration) return;
      pickerLoading = false;
      if (firstLoad && pickerResolve) {
        // 首次读取就失败说明目录接口不可用：关闭选择器，让移动操作
        // 退回手写路径的输入框，功能不至于不可用。
        pickerFirstLoad = false;
        pickerFallback = true;
        closePicker(null);
        return;
      }
      list.replaceChildren(el("div", "picker-empty", "文件夹读取失败，请点击上一级或重试"));
      renderPickerState();
    }
  }

  function closePicker(value) {
    if (!pickerResolve) return null;
    pickerGeneration += 1;
    const resolve = pickerResolve;
    pickerResolve = null;
    $("picker").close();
    if (pickerTrigger && document.contains(pickerTrigger)) pickerTrigger.focus();
    pickerTrigger = null;
    resolve(value);
  }

  function openFolderPicker(sourcePath) {
    pickerTrigger = document.activeElement;
    pickerSourcePath = sourcePath;
    pickerCurrent = parentPath(sourcePath);
    pickerLoading = true;
    pickerFirstLoad = true;
    $("picker-subtitle").textContent = `选择 “${sourcePath.split("/").filter(Boolean).pop() || "项目"}” 的目标文件夹`;
    $("picker").showModal();
    return new Promise((resolve) => {
      pickerResolve = resolve;
      renderPickerList();
    });
  }

  /* ============ 文件操作 ============ */
  async function createFolder() {
    const name = await openInputModal("新建文件夹", "文件夹名称", "", "例如：项目资料");
    if (!name) return;
    setBusy(true);
    try {
      await api("folders", joinPath(state.path, name), { method: "POST" });
      clearDirectoryCache();
      setStatus("文件夹已创建", "success");
      toast(`文件夹 “${name}” 已创建`, "success");
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
      toast(`已重命名为 “${name}”`, "success");
      await load(state.path, true, true);
    } catch (error) { showError(error); }
    finally { setBusy(false); renderBreadcrumbs(); }
  }

  async function move(entry, path) {
    pickerFallback = false;
    const picked = await openFolderPicker(path);
    let destination = picked;
    if (pickerFallback) {
      // 选择器无法读取目录：退回手写路径，保证移动功能始终可用。
      destination = await openInputModal("移动项目", "目标文件夹路径", state.path, "例如：/项目资料");
    }
    if (!destination) return;
    setBusy(true);
    try {
      await api("entries", path, { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ parent_path: canonicalPath(destination) }) });
      clearDirectoryCache();
      setStatus("项目已移动", "success");
      toast(`“${entry.name}” 已移动到 ${canonicalPath(destination)}`, "success");
      await load(state.path, true, true);
    } catch (error) { showError(error); }
    finally { setBusy(false); renderBreadcrumbs(); }
  }

  async function remove(entry, path) {
    const confirmed = await openConfirmModal("删除项目", `确定删除“${entry.name}”吗？此操作会同步到 WPS。`, "删除", true);
    if (!confirmed) return;
    const sourcePath = state.path;
    state.pendingDeletes.add(path);
    renderEntries({ animate: false });
    setStatus("正在提交删除...", "pending");
    setBusy(true);
    try {
      await api("entries", path, { method: "DELETE" });
      setStatus("删除已确认，正在刷新目录...", "pending");
      clearDirectoryCache();
      state.pendingDeletes.delete(path);
      if (state.path === sourcePath) await load(sourcePath, true, true);
      setStatus("项目已删除", "success");
      toast(`“${entry.name}” 已删除`, "success");
    } catch (error) {
      state.pendingDeletes.delete(path);
      renderEntries({ animate: false });
      showError(error);
    }
    finally { setBusy(false); renderBreadcrumbs(); }
  }

  /* ============ 上传托盘 ============ */
  const tray = {
    active: false,
    cancelled: false,
    xhr: null,
    files: [],
    states: [],
    loaded: [],
    speeds: [],
    errors: [],
    targets: [],
    existingEntries: [],
    cancelledItems: new Set(),
    done: 0,
  };

  function trayIconWrap(iconName, cls) {
    const wrap = el("span", "t-ic " + cls);
    wrap.append(icon(iconName));
    return wrap;
  }

  function traySetItem(index) {
    const item = $("tray-list").children[index];
    if (!item) return;
    const st = tray.states[index];
    item.className = "tray-item " + st;
    const iconHolder = item.querySelector(".tray-state-icon");
    iconHolder.replaceChildren();
    const iconName = st === "active" || st === "confirming" ? null
      : st === "done" ? "check"
        : st === "error" ? "alert"
          : st === "skipped" || st === "cancelled" ? "x" : "clock";
    if (st === "active" || st === "confirming" || st === "cancelling") {
      iconHolder.append(el("span", st === "cancelling" ? "tray-cancel-spinner" : "tray-spinner"));
    } else {
      iconHolder.append(trayIconWrap(iconName, st === "done" ? "ok" : st === "error" ? "bad" : ""));
    }
    const file = tray.files[index];
    const size = file ? formatBytes(file.size) : "-";
    const loaded = Math.max(0, Number(tray.loaded[index]) || 0);
    const percent = file && file.size > 0 ? Math.min(100, loaded * 100 / file.size) : 0;
    const statusText = {
      pending: "等待上传",
      active: "上传中",
      confirming: "正在确认上传结果",
      cancelling: "正在取消",
      done: "上传完成",
      error: tray.errors[index] || "上传失败",
      skipped: "已跳过",
      cancelled: "已取消",
    }[st] || "等待上传";
    item.querySelector(".tray-status").textContent = statusText;
    item.querySelector(".tray-target").textContent = `上传到 ${tray.targets[index] || "/"}`;
    item.querySelector(".tray-size").textContent = `${formatBytes(loaded)} / ${size}`;
    item.querySelector(".tray-rate").textContent = tray.speeds[index] || "计算中";
    item.querySelector(".tray-progress-bar").style.width = `${percent}%`;
    const cancel = item.querySelector(".tray-item-action");
    const canCancel = st === "pending" || st === "active" || st === "confirming";
    const canRetry = st === "error" || st === "skipped";
    cancel.hidden = !canCancel && !canRetry;
    cancel.disabled = st === "cancelling";
    cancel.title = canRetry ? "重新上传此文件" : "取消此文件上传";
    cancel.setAttribute("aria-label", cancel.title);
    cancel.replaceChildren(icon(canRetry ? "refresh" : "x"));
  }

  function trayReset(files, targetPath) {
    tray.active = true;
    tray.cancelled = false;
    tray.done = 0;
    tray.files = files.slice();
    tray.states = files.map(() => "pending");
    tray.loaded = files.map(() => 0);
    tray.speeds = files.map(() => "等待上传");
    tray.errors = files.map(() => "");
    tray.targets = files.map(() => targetPath);
    tray.existingEntries = [];
    tray.cancelledItems = new Set();
    const list = $("tray-list");
    list.replaceChildren();
    files.forEach((file, index) => {
      const item = el("li", "tray-item pending");
      const top = el("div", "tray-item-top");
      const left = el("span", "tray-state-icon");
      const name = el("span", "t-name", file.name);
      name.title = file.name;
      const status = el("span", "tray-status", "等待上传");
      top.append(left, name, status);
      const target = el("div", "tray-target", `上传到 ${targetPath}`);
      const progress = el("div", "tray-item-progress");
      progress.append(el("span", "tray-progress-bar"));
      const stats = el("div", "tray-item-stats");
      stats.append(el("span", "tray-size", `0 B / ${formatBytes(file.size)}`), el("span", "tray-rate", "等待上传"));
      const cancel = el("button", "tray-item-action");
      cancel.type = "button";
      cancel.dataset.index = String(index);
      cancel.title = "取消此文件上传";
      cancel.setAttribute("aria-label", cancel.title);
      cancel.append(icon("x"));
      item.append(top, target, progress, stats, cancel);
      cancel.addEventListener("click", () => trayCancelItem(index));
      list.append(item);
      traySetItem(index);
    });
    list.classList.toggle("has-multi", files.length > 1);
    $("tray-cancel").classList.remove("hidden");
    $("tray-close").classList.remove("hidden");
    $("tray-count").textContent = files.length > 1 ? `0 / ${files.length}` : "";
    $("tray-percent").textContent = "0%";
    setRing(0);
    $("upload-tray").classList.add("show");
    $("upload-tray").setAttribute("aria-hidden", "false");
  }

  function setRing(percent) {
    const ring = $("tray-ring");
    ring.style.strokeDashoffset = String(100 - Math.max(0, Math.min(100, percent)));
    $("tray-percent").textContent = `${Math.round(percent)}%`;
  }

  function trayCurrent(file, percent, speedText) {
    $("tray-name").textContent = file.name;
    $("tray-name").title = file.name;
    $("tray-speed").textContent = speedText;
    setRing(percent);
  }

  function trayFinishAll(ok, message) {
    tray.active = false;
    tray.xhr = null;
    $("tray-cancel").classList.add("hidden");
    $("tray-close").classList.remove("hidden");
    $("tray-speed").textContent = message || (ok ? "全部完成" : "队列已停止");
    if (ok) setTimeout(() => { if (!tray.active) trayHide(); }, 2400);
  }

  function trayHide() {
    $("upload-tray").classList.remove("show");
    $("upload-tray").setAttribute("aria-hidden", "true");
  }

  function trayCancelItem(index) {
    const st = tray.states[index];
    if (st === "pending") {
      tray.cancelledItems.add(index);
      tray.states[index] = "cancelled";
      traySetItem(index);
      return;
    }
    if (st === "error" || st === "skipped") {
      if (state.connection !== "connected") {
        toast("WPS 当前不可用，请恢复连接后重试", "warn", 5200);
        return;
      }
      tray.states[index] = "pending";
      tray.errors[index] = "";
      tray.loaded[index] = 0;
      tray.speeds[index] = "等待上传";
      traySetItem(index);
      uploadFiles([tray.files[index]], { targetPath: tray.targets[index], retryIndex: index });
      return;
    }
    if (st === "active" || st === "confirming") {
      tray.cancelledItems.add(index);
      tray.states[index] = "cancelling";
      traySetItem(index);
      if (tray.xhr) tray.xhr.abort();
    }
  }

  function trayCancel() {
    if (!tray.active) return;
    tray.cancelled = true;
    tray.states.forEach((st, index) => {
      if (st === "pending") {
        tray.cancelledItems.add(index);
        tray.states[index] = "cancelled";
        traySetItem(index);
      }
    });
    if (tray.xhr) tray.xhr.abort();
  }

  /* ============ 上传 ============ */
  function uploadOne(file, overwrite, targetPath, index) {
    return new Promise((resolve, reject) => {
      const url = pathUrl("upload", joinPath(targetPath, file.name));
      if (overwrite) url.searchParams.set("overwrite", "true");
      const startedAt = performance.now();
      const updateSpeed = (loaded) => {
        const elapsed = Math.max((performance.now() - startedAt) / 1000, 0.001);
        return `${formatRate(loaded / elapsed)} · ${formatBytes(loaded)} / ${formatBytes(file.size)}`;
      };
      trayCurrent(file, 0, "正在连接...");
      const xhr = new XMLHttpRequest();
      tray.xhr = xhr;
      xhr.open("PUT", url.toString());
      xhr.withCredentials = true;
      xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");
      xhr.upload.onprogress = (event) => {
        if (!event.lengthComputable) return;
        tray.loaded[index] = event.loaded;
        tray.speeds[index] = updateSpeed(event.loaded).split(" · ")[0];
        const percent = event.loaded * 100 / event.total;
        const speedText = updateSpeed(event.loaded);
        tray.states[index] = event.loaded >= event.total ? "confirming" : "active";
        traySetItem(index);
        setRing(percent);
        trayCurrent(file, percent, speedText);
        setStatus(`正在上传 ${file.name} · ${Math.round(percent)}%`);
      };
      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          tray.loaded[index] = file.size;
          tray.speeds[index] = updateSpeed(file.size).split(" · ")[0];
          trayCurrent(file, 100, updateSpeed(file.size));
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
      xhr.onabort = () => {
        const error = new Error("上传已取消");
        error.cancelled = true;
        reject(error);
      };
      xhr.send(file);
    });
  }

  async function uploadQueueItem(index) {
    if (tray.cancelled || tray.cancelledItems.has(index) || tray.states[index] === "cancelled") return true;
    const file = tray.files[index];
    if (!file) return true;
    tray.states[index] = "active";
    tray.errors[index] = "";
    tray.speeds[index] = "计算中";
    traySetItem(index);
    const existing = tray.existingEntries.find((entry) => entry.name === file.name);
    let overwrite = false;
    if (existing) {
      if (existing.kind !== "file") {
        tray.states[index] = "skipped";
        tray.errors[index] = "同名文件夹无法覆盖";
        traySetItem(index);
        toast(`“${file.name}” 与现有文件夹同名，已跳过`, "warn", 5200);
        return true;
      }
      const confirmed = await openConfirmModal("文件已存在", `“${file.name}”已经存在，要覆盖它吗？`, "覆盖", false);
      if (!confirmed) {
        tray.states[index] = "skipped";
        tray.errors[index] = "用户取消覆盖";
        traySetItem(index);
        return true;
      }
      overwrite = true;
    }
    try {
      await uploadOne(file, overwrite, tray.targets[index], index);
      tray.done += 1;
      tray.states[index] = "done";
      traySetItem(index);
      clearDirectoryCache();
      return true;
    } catch (error) {
      if (error.cancelled || tray.cancelledItems.has(index) || tray.cancelled) {
        tray.states[index] = "cancelled";
        traySetItem(index);
        return !tray.cancelled;
      }
      tray.states[index] = "error";
      tray.errors[index] = error.message || "上传失败";
      traySetItem(index);
      return false;
    }
  }

  async function uploadFiles(files, options = {}) {
    const retryIndex = Number.isInteger(options.retryIndex) ? options.retryIndex : null;
    const retry = retryIndex !== null && tray.files[retryIndex] === files[0];
    const targetPath = retry ? tray.targets[retryIndex] : state.path;
    if (!files.length || state.connection !== "connected" || (!retry && state.path === "/" && state.spaces.length > 0)) {
      if (files.length && !retry && state.path === "/" && state.spaces.length > 0) {
        toast("请先进入一个 WPS 空间再上传文件", "warn", 4200);
      }
      return;
    }
    if (state.busy && !retry) return;
    if (retry && tray.active) return;
    setBusy(true);
    if (retry) {
      tray.active = true;
      tray.cancelled = false;
      tray.cancelledItems.delete(retryIndex);
      tray.states[retryIndex] = "pending";
      tray.loaded[retryIndex] = 0;
      tray.errors[retryIndex] = "";
      tray.speeds[retryIndex] = "等待上传";
      traySetItem(retryIndex);
      $("tray-cancel").classList.remove("hidden");
    } else {
      trayReset(files, targetPath);
      tray.existingEntries = state.entries.slice();
    }
    let failure = null;
    try {
      if (retry) {
        if (!(await uploadQueueItem(retryIndex))) failure = new Error(tray.errors[retryIndex] || "上传失败");
      } else {
        for (let index = 0; index < tray.files.length; index += 1) {
          if (tray.cancelled) break;
          if (tray.states[index] !== "pending") continue;
          if (tray.files.length > 1) setStatus(`准备上传第 ${index + 1}/${tray.files.length} 个文件`);
          $("tray-count").textContent = tray.files.length > 1 ? `${tray.done} / ${tray.files.length}` : "";
          if (!(await uploadQueueItem(index))) {
            if (!failure) failure = new Error(tray.errors[index] || "上传失败");
          }
        }
      }
      if (tray.cancelled) {
        setStatus("上传已取消");
        toast("上传已取消", "info");
        trayFinishAll(false);
      } else if (failure) {
        showError(failure);
        trayFinishAll(false);
      } else {
        const hasSkipped = tray.states.some((item) => item === "cancelled" || item === "skipped");
        const message = hasSkipped ? `已上传 ${tray.done} 个文件，其他项目未上传` : "上传完成";
        setStatus(message, tray.done ? "success" : "");
        toast(message, tray.done ? "success" : "info");
        trayFinishAll(true, hasSkipped ? "队列已处理" : "全部完成");
        if (state.path === targetPath) await load(targetPath, true, true);
      }
    } catch (error) {
      showError(error);
      trayFinishAll(false);
    } finally {
      setBusy(false);
      renderBreadcrumbs();
    }
  }

  /* ============ 拖放 ============ */
  function hasFileTransfer(event) {
    const transfer = event.dataTransfer;
    if (!transfer) return false;
    return Array.from(transfer.types || []).includes("Files") || transfer.files.length > 0;
  }

  function showDropOverlay() {
    if (state.busy || state.connection !== "connected" || (state.path === "/" && state.spaces.length > 0)) return;
    $("drop-target").textContent = state.path;
    $("drop-overlay").classList.add("active");
    $("drop-overlay").setAttribute("aria-hidden", "false");
  }

  function hideDropOverlay() {
    $("drop-overlay").classList.remove("active");
    $("drop-overlay").setAttribute("aria-hidden", "true");
  }

  /* ============ 事件绑定 ============ */
  $("login-form").addEventListener("submit", submitAuth);
  $("auth-method-password").addEventListener("click", () => setLoginMethod("password"));
  $("auth-method-passkey").addEventListener("click", () => setLoginMethod("passkey"));
  $("password-toggle").addEventListener("click", togglePassword);
  $("passkey-login-button").addEventListener("click", passkeyLogin);
  $("logout-button").addEventListener("click", logout);
  $("connection").addEventListener("click", () => toggleStatusPanel());
  $("status-panel-close").addEventListener("click", () => toggleStatusPanel(false));
  $("status-refresh-button").addEventListener("click", async () => {
    const button = $("status-refresh-button");
    if (button.disabled) return;
    button.disabled = true;
    button.classList.add("busy");
    try {
      await checkConnection(false);
      if (state.connection === "connected" && !state.loading) await load(state.path, true, true);
    } finally {
      button.disabled = false;
      button.classList.remove("busy");
    }
  });
  $("settings-form").addEventListener("submit", submitSettings);
  $("storage-location-button").addEventListener("click", chooseStorageLocation);
  $("totp-enable-button").addEventListener("click", enableTOTP);
  $("totp-disable-button").addEventListener("click", disableTOTP);
  $("passkey-register-button").addEventListener("click", registerPasskey);
  $("settings-cancel").addEventListener("click", closeSettingsModal);
  $("settings-modal").addEventListener("cancel", (event) => {
    event.preventDefault();
    closeSettingsModal();
  });
  document.querySelectorAll(".theme-option").forEach((button) => {
    button.addEventListener("click", () => {
      settingsDraftTheme = button.dataset.theme;
      renderThemeOptions();
    });
  });
  $("nav-menu-button").addEventListener("click", () => {
    if (document.body.classList.contains("nav-open")) closeMobileNav();
    else openMobileNav();
  });
  $("sidebar-close").addEventListener("click", closeMobileNav);
  $("mobile-scrim").addEventListener("click", closeMobileNav);
  $("space-root").addEventListener("click", () => {
    closeMobileNav();
    load("/");
  });

  $("modal-form").addEventListener("submit", (event) => {
    event.preventDefault();
    if (modalMode === "input") {
      const value = $("modal-input").value.trim();
      if (!value) {
        $("modal-message").textContent = "名称不能为空";
        $("modal-message").classList.remove("hidden");
        $("modal-input").focus();
        return;
      }
      closeModal(value);
    } else closeModal(true);
  });
  $("modal-cancel").addEventListener("click", () => closeModal(null));
  $("modal").addEventListener("cancel", (event) => { event.preventDefault(); closeModal(null); });

  $("picker-cancel").addEventListener("click", () => closePicker(null));
  $("picker").addEventListener("cancel", (event) => { event.preventDefault(); closePicker(null); });
  $("picker-move").addEventListener("click", () => {
    if (pickerCanMoveHere()) closePicker(pickerCurrent);
  });
  $("picker-up").addEventListener("click", () => {
    if (pickerLoading) return;
    pickerCurrent = parentPath(pickerCurrent);
    renderPickerList();
  });

  $("storage-picker-cancel").addEventListener("click", () => closeStoragePicker(null));
  $("storage-picker").addEventListener("cancel", (event) => {
    event.preventDefault();
    closeStoragePicker(null);
  });
  $("storage-picker-up").addEventListener("click", () => {
    if (storagePickerLoading || storagePickerPath === "/") return;
    storagePickerPath = parentPath(storagePickerPath);
    renderStoragePickerFolders();
  });
  $("storage-picker-select").addEventListener("click", () => {
    if (storagePickerSpace) closeStoragePicker(storagePickerFullPath());
  });

  $("theme-button").addEventListener("click", openSettingsModal);
  $("view-list-button").addEventListener("click", () => setView("list"));
  $("view-grid-button").addEventListener("click", () => setView("grid"));
  $("settings-button").addEventListener("click", openSettingsModal);
  $("up-button").addEventListener("click", () => load(parentPath(state.path)));
  $("refresh-button").addEventListener("click", () => load(state.path, false, true));
  $("sidebar-refresh-button").addEventListener("click", () => { closeMobileNav(); load(state.path, false, true); });
  $("sidebar-settings-button").addEventListener("click", () => { closeMobileNav(); openSettingsModal(); });
  $("sidebar-theme-button").addEventListener("click", () => { closeMobileNav(); openSettingsModal(); });
  $("sidebar-logout-button").addEventListener("click", logout);
  $("folder-button").addEventListener("click", createFolder);
  $("upload-button").addEventListener("click", () => $("file-input").click());
  $("tray-cancel").addEventListener("click", trayCancel);
  $("tray-close").addEventListener("click", trayHide);

  $("file-input").addEventListener("change", (event) => {
    uploadFiles(Array.from(event.target.files || []));
    event.target.value = "";
  });

  $("search-input").addEventListener("input", (event) => {
    state.search = event.target.value;
    updateSearchControls();
    renderEntries({ animate: false });
  });
  $("search-clear").addEventListener("click", clearSearch);
  $("search-input").addEventListener("keydown", (event) => {
    if (event.key === "Escape" && $("search-input").value) {
      event.stopPropagation();
      clearSearch();
    }
  });

  $("sort-select").addEventListener("change", (event) => {
    const key = event.target.value;
    setSort(key, key === "auto" ? "asc" : SORT_DEFAULT_DIR[key] || "asc");
  });
  $("sort-dir-button").addEventListener("click", () => {
    const flipped = state.sort.dir === "desc" ? "asc" : "desc";
    setSort(state.sort.key, flipped);
  });
  document.querySelectorAll("#list-head .sort-button").forEach((button) => {
    button.addEventListener("click", () => {
      const key = button.dataset.sort;
      if (state.sort.key === key) {
        setSort(key, state.sort.dir === "desc" ? "asc" : "desc");
      } else {
        setSort(key, SORT_DEFAULT_DIR[key] || "asc");
      }
    });
  });

  document.addEventListener("click", (event) => {
    if (openActionMenu && !openActionMenu.contains(event.target)) closeActionMenu();
  });

  window.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && openActionMenu) {
      closeActionMenu(true);
      return;
    }
    const dialogOpen = document.querySelector("dialog[open]");
    const tag = document.activeElement ? document.activeElement.tagName : "";
    const typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
    if (event.key === "Escape" && !dialogOpen && !typing && !$('status-panel').hidden) {
      toggleStatusPanel(false);
      return;
    }
    if (event.key === "/" && !dialogOpen && !typing) {
      event.preventDefault();
      $("search-input").focus();
      return;
    }
    if (event.key === "Escape" && !dialogOpen && !typing && state.search) {
      clearSearch();
      return;
    }
    if (event.altKey && event.key === "ArrowUp" && !dialogOpen && !typing) {
      if (!$("up-button").disabled) {
        event.preventDefault();
        load(parentPath(state.path));
      }
    }
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

  window.addEventListener("scroll", () => {
    $("app-header").classList.toggle("scrolled", window.scrollY > 4);
  }, { passive: true });

  window.setInterval(async () => {
    if (state.busy || document.hidden) return;
    const previous = state.connection;
    const current = await checkConnection(true);
    if (current !== "connected") {
      // Keep the last confirmed directory visible during a background
      // failure. The status row and disabled controls make its freshness
      // explicit without replacing useful content with a blank state.
      if (previous === "connected" || state.entries.length === 0) {
        setStatus(`${connectionMessage(current)}${state.entries.length ? "（当前列表尚未更新）" : ""}`, "error");
      }
      if (state.entries.length === 0) renderEntries({ animate: false });
    } else if (previous !== "connected") {
      await load(state.path, true, true);
    }
  }, 30000);

  /* ============ 启动 ============ */
  async function startDrive() {
    applyTheme(false);
    loadSort();
    renderSortControls();
    setView(PREF.get("view", "list"), { animate: false });
    // Fetch the configured root name before the first render so the
    // placeholder never flips to the real name and back.
    await initRootName();
    const initial = hashPath();
    if (location.hash !== "#" + initial) {
      history.replaceState(null, "", "#" + initial);
    }
    renderBreadcrumbs();
    load(initial);
  }

  async function boot() {
    if (await initWebAuth()) await startDrive();
  }
  boot();
})();
