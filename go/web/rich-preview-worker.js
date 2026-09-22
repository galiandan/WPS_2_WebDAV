/* Parse and highlight untrusted text off the UI thread. No document or network URLs are accepted. */
"use strict";
importScripts("/assets/vendor-marked.js", "/assets/vendor-highlight.js");
const escapeHTML = (text) => String(text || "").replace(/[&<>"']/g, (value) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[value]));
const byteLength = (text) => new TextEncoder().encode(text).length;
self.onmessage = ({ data }) => {
  try {
    if (!data || typeof data.text !== "string" || byteLength(data.text) > 256 * 1024) throw new Error("limit");
    let highlighted = 0, limited = false;
    function highlight(text, language, maxBytes) {
      if (!language || !hljs.getLanguage(language)) return escapeHTML(text);
      const size = byteLength(text);
      if (size > maxBytes || highlighted + size > 64 * 1024) { limited = true; return escapeHTML(text); }
      highlighted += size;
      return hljs.highlight(text, { language, ignoreIllegals: true }).value;
    }
    let html;
    if (data.markdown) {
      const parser = new marked.Marked({ gfm: true, breaks: false, renderer: {
        html: ({ text }) => escapeHTML(text),
        image: ({ text, href }) => `<span data-wps-image="${escapeHTML(href)}" data-wps-alt="${escapeHTML(text)}">${escapeHTML(text)}</span>`,
        link({ href, tokens }) { return `<a data-wps-href="${escapeHTML(href)}">${this.parser.parseInline(tokens)}</a>`; },
        code: ({ text, lang }) => `<pre><code>${highlight(text, String(lang || "").trim().split(/\s+/)[0].toLowerCase(), 16 * 1024)}</code></pre>`,
        checkbox: ({ checked }) => checked ? "☑ " : "☐ ",
      } });
      html = parser.parse(data.text);
    } else {
      html = `<pre><code>${highlight(data.text, data.language, 64 * 1024)}</code></pre>`;
    }
    if (typeof html !== "string" || byteLength(html) > 1024 * 1024) throw new Error("limit");
    self.postMessage({ html, limited });
  } catch (_) { self.postMessage({ error: "内容较大或格式复杂，请使用原文查看。" }); }
};
