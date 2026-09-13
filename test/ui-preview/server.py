#!/usr/bin/env python3
"""UI 预览 mock 服务:托管 go/web 静态文件并伪造适配器 API 响应。

用法: python3 server.py [port]
登录页预览: /?mode=login   工作台预览: /preview?theme=dark&view=grid&path=/xx
样式诊断: /debug?theme=dark(计算样式写入 <title>,配合 --dump-dom)
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

WEB = Path(__file__).resolve().parents[2] / "go" / "web"

CONTENT_TYPES = {
    ".html": "text/html; charset=utf-8",
    ".css": "text/css; charset=utf-8",
    ".js": "text/javascript; charset=utf-8",
    ".svg": "image/svg+xml",
}

PROBE_JS = """
setTimeout(() => {
  const gs = (sel, prop) => {
    const n = document.querySelector(sel);
    return n ? getComputedStyle(n)[prop] : 'MISSING';
  };
  document.title = 'PROBE:' + JSON.stringify({
    theme: document.documentElement.dataset.theme || 'auto',
    headerBg: gs('.app-header', 'backgroundColor'),
    crumbsBg: gs('.breadcrumbs', 'backgroundColor'),
    panelBg: gs('.panel', 'backgroundColor'),
    h1Image: gs('.workspace-head h1', 'backgroundImage'),
    bodyBg: gs('body', 'backgroundColor'),
    glassVar: getComputedStyle(document.documentElement)
      .getPropertyValue('--glass').trim(),
  });
}, 3000);
"""


def entry(name, kind, size, mtime, eid=None):
    e = {"name": name, "kind": kind, "size": size, "modified_at": mtime}
    if eid:
        e["id"] = eid
    return e


ROOT_ENTRIES = [
    entry("个人空间", "folder", 0, "2026-09-10T10:24:00Z", "space:personal"),
    entry("团队协作空间", "folder", 0, "2026-09-09T18:03:00Z", "space:team"),
    entry("企业文档库", "folder", 0, "2026-09-08T09:12:00Z", "space:corp"),
]

DOCS_ENTRIES = [
    entry("产品需求文档 v2.4.docx", "file", 1_284_302, "2026-09-12T08:31:00Z"),
    entry("季度经营分析.xlsx", "file", 862_144, "2026-09-11T16:45:00Z"),
    entry("发布会 keynote 终稿.pptx", "file", 48_233_112, "2026-09-11T10:02:00Z"),
    entry("2026 品牌视觉规范.pdf", "file", 12_884_556, "2026-09-10T14:20:00Z"),
    entry("会议纪要", "folder", 0, "2026-09-10T09:00:00Z"),
    entry("设计资源", "folder", 0, "2026-09-09T11:30:00Z"),
    entry("logo-gradient.png", "file", 244_120, "2026-09-09T10:11:00Z"),
    entry("产品演示-0912.mp4", "file", 421_004_224, "2026-09-12T19:40:00Z"),
    entry("背景音乐-mellow.flac", "file", 31_460_224, "2026-09-08T20:15:00Z"),
    entry("部署脚本包.tar.gz", "file", 8_912_896, "2026-09-07T08:00:00Z"),
    entry("adapter-main.go", "file", 41_236, "2026-09-12T22:51:00Z"),
    entry("README.md", "file", 15_680, "2026-09-13T21:22:00Z"),
    entry("付款凭证扫描件.pdf", "file", 2_331_112, "2026-09-05T13:37:00Z"),
    entry("系统架构图.dwg", "file", 5_552_233, "2026-09-04T17:09:00Z"),
]


def preseed_js(query):
    parts = []
    if query.get("theme", [""])[0]:
        parts.append("localStorage.setItem('wpsdrv.theme', '%s');" % query["theme"][0])
    if query.get("view", [""])[0]:
        parts.append("localStorage.setItem('wpsdrv.view', '%s');" % query["view"][0])
    return "".join(parts)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _wrap_cookie_send(self):
        handler = self

        def send_response(code, message=None):
            handler._cookie_pending = True
            BaseHTTPRequestHandler.send_response(handler, code, message)
            handler.send_header("Set-Cookie", "ui_login=1; Path=/; Max-Age=3600; SameSite=Lax")

        return send_response

    def _html(self, body):
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        url = urlparse(self.path)
        query = parse_qs(url.query)
        path = url.path
        # ?mode=login 时种 cookie,让后续 auth/me 模拟未登录(顶层导航无 Referer)。
        if not path.startswith("/api") and query.get("mode", [""])[0] == "login":
            self.send_response = self._wrap_cookie_send()
        if path == "/preview":
            # 跳板页:预置 localStorage 后跳回工作台(或登录页)。
            target = query.get("path", ["/"])[0]
            next_url = "/?mode=login" if query.get("mode", [""])[0] == "login" else "/#%s" % target
            script = preseed_js(query) + "location.replace('%s');" % next_url
            return self._html(
                ("<!doctype html><body><script>%s</script></body>" % script).encode()
            )
        if path == "/modal":
            html = (WEB / "index.html").read_text(encoding="utf-8")
            inline = ("<script>%ssetTimeout(() => { const d = document.getElementById('settings-modal');"
                      " d.showModal(); }, 2500);</script></body>" % preseed_js(query))
            return self._html(html.replace("</body>", inline).encode())
        if path == "/debug":
            html = (WEB / "index.html").read_text(encoding="utf-8")
            inline = "<script>%s%s</script></body>" % (preseed_js(query), PROBE_JS)
            return self._html(html.replace("</body>", inline).encode())
        if path.startswith("/api/"):
            return self.api(path, query)
        if path == "/healthz":
            return self._json({"status": "ok", "version": "1.0.13"})
        if path in ("/", "/web", "/web/"):
            return self.static("index.html")
        if path.startswith("/assets/"):
            return self.static(path[len("/assets/"):])
        self._json({"error": "not found"}, 404)

    def api(self, path, query):
        # 登录模式:跳板页 Referer 带 mode=login,或已种下 ui_login cookie。
        cookie = self.headers.get("Cookie", "")
        login_mode = ("mode=login" in self.headers.get("Referer", "")
                      or "ui_login=1" in cookie)
        if path == "/api/v1/auth/me":
            if login_mode:
                return self._json({"authenticated": False})
            return self._json({"authenticated": True, "user": "admin"})
        if path == "/api/v1/status":
            return self._json({"status": "connected", "mode": "personal"})
        if path == "/api/v1/settings":
            return self._json({"name": "WPS 云盘"})
        if path == "/api/v1/storage":
            return self._json({
                "locations": [
                    {"space_id": "personal", "space_name": "个人空间", "path": "/"},
                ],
                "current": {"space_id": "personal", "space_name": "个人空间", "path": "/"},
            })
        if path == "/api/v1/auth/security":
            return self._json({"totp_enabled": False, "passkeys": []})
        if path == "/api/v1/update":
            return self._json({
                "state": "idle",
                "current_version": "1.0.13",
                "message": "当前已是最新版本",
            })
        if path == "/api/v1/entries":
            target = query.get("path", ["/"])[0]
            if target == "/":
                return self._json({"entries": ROOT_ENTRIES})
            if "会议纪要" in target:
                return self._json({"entries": []})
            return self._json({"entries": DOCS_ENTRIES})
        if path == "/api/v1/storage/entries":
            return self._json({"entries": [e for e in DOCS_ENTRIES if e["kind"] == "folder"]})
        self._json({"error": "not found"}, 404)

    def static(self, name):
        file_path = (WEB / name).resolve()
        if not str(file_path).startswith(str(WEB)) or not file_path.is_file():
            return self._json({"error": "not found"}, 404)
        body = file_path.read_bytes()
        suffix = file_path.suffix.lower()
        self.send_response(200)
        self.send_header("Content-Type", CONTENT_TYPES.get(suffix, "application/octet-stream"))
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8765
    print(f"UI preview: http://127.0.0.1:{port}/  (login: /?mode=login)")
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
