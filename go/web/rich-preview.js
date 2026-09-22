/* Local Markdown/code previews. Untrusted markup is sanitized before entering the live document. */
(() => {
  "use strict";
  const make = (tag, text, className) => {
    const element = document.createElement(tag);
    if (text) element.textContent = text;
    if (className) element.className = className;
    return element;
  };
  const languages = { json: "json", xml: "xml", yaml: "yaml", yml: "yaml", ini: "ini", conf: "ini", toml: "ini", js: "javascript", mjs: "javascript", cjs: "javascript", ts: "typescript", jsx: "javascript", tsx: "typescript", go: "go", py: "python", sh: "bash", bash: "bash", css: "css", html: "xml", htm: "xml", sql: "sql", rs: "rust", java: "java", c: "c", h: "c", cpp: "cpp", hpp: "cpp", diff: "diff", patch: "diff" };
  const extension = (name) => String(name).split(".").pop().toLowerCase();
  const imageName = (path) => /\.(jpe?g|png|gif|webp|avif|bmp|ico)$/i.test(path);
  let config, raw, rich, toggle, notice, readme, readmeBody, readmeNotice, readmeOpen;
  let current = null, rememberedPath = "", mode = "render", previewJob = null, readmeJob = null;
  let previewGeneration = 0, readmeGeneration = 0, readmeController = null;

  function endpoint(route, path) {
    const url = new URL("/api/v1/" + route, location.origin);
    url.searchParams.set("path", path);
    return url;
  }
  function stop(job) { if (job) { clearTimeout(job.timer); job.worker.terminate(); } }
  function clearPreview() {
    previewGeneration += 1;
    stop(previewJob); previewJob = null; current = null;
    if (!rich) return;
    for (const image of rich.querySelectorAll("img")) image.removeAttribute("src");
    rich.replaceChildren(); rich.hidden = true; toggle.hidden = true; notice.textContent = "";
  }
  function clearReadme() {
    readmeGeneration += 1;
    stop(readmeJob); readmeJob = null;
    if (readmeController) readmeController.abort();
    readmeController = null;
    if (!readme) return;
    for (const image of readmeBody.querySelectorAll("img")) image.removeAttribute("src");
    readmeBody.replaceChildren(); readme.hidden = true; readmeNotice.textContent = ""; readmeOpen.onclick = null;
  }
  function renderWorker(text, markdown, language, done) {
    let worker;
    try { worker = new Worker("/assets/rich-preview-worker.js"); }
    catch (_) { done({ error: "渲染暂时不可用，请使用原文查看。" }); return null; }
    const job = { worker, timer: null };
    let finished = false;
    const finish = (result) => {
      if (finished) return;
      finished = true; stop(job); done(result);
    };
    worker.onmessage = ({ data }) => finish(data);
    worker.onerror = (event) => { event.preventDefault(); finish({ error: "渲染暂时不可用，请使用原文查看。" }); };
    job.timer = setTimeout(() => finish({ error: "内容格式复杂，已停止渲染，请使用原文查看。" }), 2000);
    worker.postMessage({ text, markdown, language });
    return job;
  }

  // Markdown paths are URL references, whereas REST takes already-decoded
  // business paths. Decode each segment once; encoded slashes never create
  // additional path components and literal %2F names use %252F in Markdown.
  function target(href, source) {
    if (typeof href !== "string" || /[\\\u0000-\u0020\u007f]/.test(href) || href.startsWith("//")) return null;
    if (/^https?:/i.test(href)) {
      try { const url = new URL(href); return url.username || url.password ? null : { external: url.href }; } catch (_) { return null; }
    }
    if (/^[a-z][a-z0-9+.-]*:/i.test(href)) return null;
    const hashAt = href.indexOf("#"), fragment = hashAt < 0 ? "" : href.slice(hashAt + 1);
    const reference = hashAt < 0 ? href : href.slice(0, hashAt);
    if (!reference) { try { return { fragment: decodeURIComponent(fragment) }; } catch (_) { return null; } }
    if (reference.includes("?")) return null;
    const parts = reference.startsWith("/") ? [] : source.split("/").slice(1, -1);
    for (const encoded of reference.split("/")) {
      if (!encoded) continue;
      let part;
      try { part = decodeURIComponent(encoded); } catch (_) { return null; }
      if (/[\/\\\u0000-\u001f\u007f]/.test(part)) return null;
      if (part === ".") continue;
      if (part === "..") { if (!parts.length) return null; parts.pop(); }
      else parts.push(part);
    }
    return { path: "/" + parts.join("/") };
  }
  function slug(text) { return text.trim().toLowerCase().replace(/[^\p{L}\p{N}\s_-]/gu, "").replace(/\s+/g, "-"); }
  function safeFragment(html, source, destination, valid, message) {
    if (typeof html !== "string" || new TextEncoder().encode(html).length > 1024 * 1024) throw new Error("内容较大，请使用原文查看。");
    const fragment = DOMPurify.sanitize(html, {
      RETURN_DOM_FRAGMENT: true,
      ALLOWED_TAGS: ["p", "br", "hr", "strong", "em", "del", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "table", "thead", "tbody", "tr", "th", "td", "pre", "code", "a", "span"],
      ALLOWED_ATTR: ["class", "start", "data-wps-href", "data-wps-image", "data-wps-alt"],
      ALLOW_DATA_ATTR: false, ALLOW_ARIA_ATTR: false,
    });
    if (fragment.querySelectorAll("*").length > 10000) throw new Error("内容元素过多，请使用原文查看。");
    for (const element of fragment.querySelectorAll("[class]")) {
      const safe = element.className.split(/\s+/).filter((name) => /^hljs-[a-z0-9_-]+$/.test(name));
      element.className = safe.join(" ");
    }
    for (const anchor of fragment.querySelectorAll("a")) {
      const link = target(anchor.getAttribute("data-wps-href"), source);
      anchor.removeAttribute("data-wps-href");
      if (!link) { anchor.classList.add("rich-disabled-link"); continue; }
      if (link.external) { anchor.href = link.external; anchor.target = "_blank"; anchor.rel = "noopener noreferrer"; continue; }
      anchor.href = "#";
      anchor.addEventListener("click", async (event) => {
        event.preventDefault();
        if (!valid()) return;
        if ("fragment" in link) {
          const heading = [...destination.querySelectorAll("h1,h2,h3,h4,h5,h6")].find((item) => slug(item.textContent) === link.fragment);
          if (heading) heading.scrollIntoView({ block: "nearest" });
          return;
        }
        try {
          const entry = await config.metadata(link.path);
          if (!valid()) return;
          if (entry.kind === "folder") { config.closePreview(); await config.navigate(link.path); }
          else if (config.canPreview(entry)) config.preview(entry, link.path);
          else { config.closePreview(); await config.navigate(link.path.slice(0, link.path.lastIndexOf("/")) || "/"); }
        } catch (failure) { if (valid()) { message.textContent = "链接文件暂时无法打开。"; if (config.onError) config.onError(failure); } }
      });
    }
    let imageCount = 0;
    for (const placeholder of fragment.querySelectorAll("[data-wps-image]")) {
      const link = target(placeholder.getAttribute("data-wps-image"), source);
      const alt = placeholder.getAttribute("data-wps-alt") || "图片";
      placeholder.removeAttribute("data-wps-image"); placeholder.removeAttribute("data-wps-alt");
      if (!link || !link.path || !imageName(link.path) || ++imageCount > 20) {
        placeholder.textContent = `[${alt}：图片未加载]`; placeholder.className = "rich-image-placeholder"; continue;
      }
      const image = make("img");
      image.alt = alt; image.loading = "lazy"; image.decoding = "async"; image.referrerPolicy = "no-referrer";
      image.src = endpoint("preview", link.path);
      image.addEventListener("error", () => { if (valid()) { placeholder.textContent = `[${alt}：图片无法读取]`; image.replaceWith(placeholder); } });
      placeholder.replaceWith(image);
    }
    return fragment;
  }
  function showMode() {
    if (!current) return;
    const rendered = mode === "render" && rich.childNodes.length > 0;
    raw.hidden = rendered;
    rich.hidden = !rendered;
    toggle.textContent = mode === "render" ? "查看原文" : (current.markdown ? "渲染 Markdown" : "语法高亮");
    toggle.setAttribute("aria-pressed", String(rendered));
  }
  function renderText({ text, path, name, truncated }) {
    clearPreview();
    const markdown = /\.(md|markdown)$/i.test(name), language = languages[extension(name)];
    if (!markdown && !language) return;
    if (rememberedPath !== path) { rememberedPath = path; mode = "render"; }
    current = { markdown }; toggle.hidden = false;
    const generation = previewGeneration;
    if (new TextEncoder().encode(text).length > 256 * 1024) {
      notice.textContent = "内容较大，已显示原文；富文本渲染限 256 KiB。"; toggle.hidden = true; return;
    }
    notice.textContent = "正在渲染…";
    showMode();
    previewJob = renderWorker(text, markdown, language, (result) => {
      if (generation !== previewGeneration || !current) return;
      previewJob = null;
      if (result.error) { notice.textContent = result.error; toggle.hidden = true; return; }
      try {
        rich.replaceChildren(safeFragment(result.html, path, rich, () => generation === previewGeneration, notice));
        notice.textContent = [result.limited ? "较长代码块按原文显示。" : "", truncated ? "此渲染只包含已读取的文件片段。" : ""].filter(Boolean).join(" ");
        showMode();
      } catch (failure) { notice.textContent = failure.message || "渲染失败，请查看原文。"; toggle.hidden = true; }
    });
  }
  async function loadReadme(path, entries) {
    clearReadme();
    const candidates = entries.filter((entry) => entry.kind === "file" && /^readme\.md$/i.test(entry.name));
    const entry = candidates.find((item) => item.name === "README.md") || (candidates.length === 1 ? candidates[0] : null);
    if (!entry) return;
    const source = path.replace(/\/$/, "") + "/" + entry.name;
    const generation = readmeGeneration, controller = new AbortController();
    readmeController = controller; readme.hidden = false; readmeNotice.textContent = "正在读取目录说明…";
    readmeOpen.onclick = () => config.preview(entry, source);
    try {
      if (entry.size > 256 * 1024) throw new Error("目录说明较大，请打开文件查看原文。");
      const response = await fetch(endpoint("preview", source), { credentials: "same-origin", cache: "no-store", signal: controller.signal });
      if (!response.ok) { const failure = new Error("目录说明暂时无法读取。"); failure.status = response.status; throw failure; }
      const bytes = new Uint8Array(await response.arrayBuffer());
      if (generation !== readmeGeneration) return;
      if (bytes.length > 256 * 1024 || response.headers.get("X-Preview-Truncated") === "true") throw new Error("目录说明较大，请打开文件查看原文。");
      let encoding = "utf-8";
      if (bytes[0] === 0xff && bytes[1] === 0xfe) encoding = "utf-16le";
      else if (bytes[0] === 0xfe && bytes[1] === 0xff) encoding = "utf-16be";
      else { try { new TextDecoder("utf-8", { fatal: true }).decode(bytes); } catch (_) { encoding = "gb18030"; } }
      const text = new TextDecoder(encoding, { fatal: true }).decode(bytes);
      if (/[\u0000-\u0008\u000e-\u001f\u007f]/.test(text)) throw new Error("目录说明无法解码，请打开文件选择编码。");
      readmeJob = renderWorker(text, true, "", (result) => {
        if (generation !== readmeGeneration) return;
        readmeJob = null;
        if (result.error) { readmeNotice.textContent = result.error; return; }
        try { readmeBody.replaceChildren(safeFragment(result.html, source, readmeBody, () => generation === readmeGeneration, readmeNotice)); readmeNotice.textContent = result.limited ? "较长代码块按原文显示。" : ""; }
        catch (_) { readmeNotice.textContent = "目录说明无法渲染，请打开文件查看原文。"; }
      });
    } catch (failure) {
      if (generation !== readmeGeneration || failure.name === "AbortError") return;
      readmeNotice.textContent = failure.message;
      if (failure.status === 401 && config.onError) config.onError(failure);
    } finally { if (readmeController === controller) readmeController = null; }
  }
  function init(options) {
    config = options; raw = document.getElementById("preview-content");
    rich = make("div", "", "rich-document rich-preview"); rich.id = "rich-preview-content"; rich.hidden = true; rich.tabIndex = 0; raw.after(rich);
    notice = make("p", "", "preview-meta"); notice.id = "rich-preview-notice"; rich.after(notice);
    toggle = make("button", "查看原文"); toggle.id = "rich-preview-toggle"; toggle.type = "button"; toggle.hidden = true;
    toggle.onclick = () => { mode = mode === "render" ? "raw" : "render"; showMode(); };
    document.getElementById("preview-text-toolbar").append(toggle);
    readme = make("section", "", "readme-panel"); readme.id = "directory-readme"; readme.hidden = true;
    const heading = make("div", "", "readme-heading"); heading.append(make("h2", "目录说明"));
    readmeOpen = make("button", "打开 README"); readmeOpen.type = "button"; readmeOpen.id = "directory-readme-open"; heading.append(readmeOpen);
    readmeNotice = make("p", "", "preview-meta"); readmeNotice.id = "directory-readme-notice"; readmeNotice.setAttribute("role", "status");
    readmeBody = make("div", "", "rich-document"); readmeBody.id = "directory-readme-content";
    readme.append(heading, readmeNotice, readmeBody); document.querySelector("main.content section.panel").after(readme);
  }
  window.WPSRichPreview = { init, renderText, clearPreview, loadReadme, clearReadme };
})();
