"""Local Markdown/code rendering, resource boundaries and directory README."""
import asyncio
import base64
import os
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go/web'
PNG = base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/lV8AAAAASUVORK5CYII=')
MARKDOWN = '''# 项目说明

| 类型 | 内容 |
| --- | --- |
| 本地 | **说明** |

- [x] 完成
- [ ] 待办

```javascript
const answer = 42;
const escaped = '<script>window.richAttack = true</script>';
```

[文档](docs/说明%20%252F.md) · [目录](docs/) · [定位](#项目说明)
[外链](https://example.org/docs) · [危险](javascript:alert(1))
[编码斜杠](docs%2Fsecret.md) · [越界](../../../outside.md)

![本地图片](images/合照.png)
![远程图片](https://external.invalid/pixel.png)
![危险图片](data:image/svg+xml;base64,PHN2Zz4=)

<img src="https://external.invalid/raw.png" onerror="window.richAttack=true">
<script>window.richAttack=true</script>
<svg><a xlink:href="javascript:alert(1)">危险</a></svg>
'''


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context(viewport={'width': 1366, 'height': 900})
        page = await context.new_page()
        errors, requests, metadata_requests = [], [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        fixtures = {
            '/空间/README.md': MARKDOWN.encode(),
            '/空间/示例.json': b'{"answer": 42}',
            '/空间/标签.html': b'<script>window.richAttack=true</script>',
            '/空间/中文.md': '# 中文编码\n\n正文'.encode('gb18030'),
            '/空间/大文件.md': ('# 大文件\n' + '正文' * 50000).encode(),
            '/空间/代码块.md': ('```javascript\n' + 'const long = 1;\n' * 2000 + '```').encode(),
            '/空间/元素.md': ('a\n\n---\n\n' * 6000).encode(),
            '/空间/图片.md': '\n\n'.join(f'![图片{index}](images/合照.png)' for index in range(21)).encode(),
            '/空间/docs/说明 %2F.md': '# 相对链接文档'.encode(),
            '/空间/慢目录/README.md': '# 旧目录内容'.encode(),
        }
        slow_started, slow_gate = asyncio.Event(), asyncio.Event()
        hold_metadata = False
        worker_hang = False
        metadata_started, metadata_gate = asyncio.Event(), asyncio.Event()

        def entry(name, kind='file'):
            return {'id': name, 'name': name, 'kind': kind}

        async def route(intercept):
            request = intercept.request
            url = urlparse(request.url)
            assert url.hostname == 'wps-offline.invalid', 'Unexpected external request: ' + request.url
            path = url.path
            params = parse_qs(url.query)
            if path == '/':
                await intercept.fulfill(body=(WEB / 'index.html').read_bytes(), content_type='text/html', headers={
                    'Content-Security-Policy': "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'",
                })
                return
            if path.startswith('/assets/'):
                asset = WEB / path.removeprefix('/assets/')
                if path == '/assets/rich-preview-worker.js' and worker_hang:
                    await intercept.fulfill(body='self.onmessage = () => { while (true) {} };', content_type='text/javascript')
                    return
                await intercept.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
                return
            if path.endswith('/auth/me'):
                data = {'authenticated': True, 'user': {'username': 'demo'}}
            elif path.endswith('/settings'):
                data = {'name': 'WPS Drive'}
            elif path.endswith('/transfers'):
                data = {'tasks': []}
            elif path.endswith('/tasks'):
                data = {'tasks': []}
            elif path.endswith('/status'):
                data = {'status': 'connected'}
            elif path == '/healthz':
                data = {'version': 'offline'}
            elif path.endswith('/update'):
                data = {'state': 'idle', 'update_available': False}
            elif path.endswith('/entries'):
                folder = params['path'][0]
                if folder == '/':
                    entries = [{'id': 'space:test', 'name': '空间', 'kind': 'folder'}]
                elif folder == '/空间':
                    entries = [entry(name.rsplit('/', 1)[-1]) for name in fixtures if name.count('/') == 2]
                    entries += [entry(name, 'folder') for name in ('docs', '慢目录', '空目录')]
                elif folder == '/空间/docs':
                    entries = [entry('说明 %2F.md')]
                elif folder == '/空间/慢目录':
                    entries = [entry('README.md')]
                else:
                    entries = []
                data = {'entries': entries}
            elif path.endswith('/metadata'):
                target = params['path'][0]
                metadata_requests.append(target)
                if hold_metadata:
                    metadata_started.set()
                    await metadata_gate.wait()
                assert target in ('/空间/docs/说明 %2F.md', '/空间/docs'), target
                data = {'entry': entry(target.rsplit('/', 1)[-1], 'folder' if target == '/空间/docs' else 'file')}
            elif path.endswith('/preview'):
                target = params['path'][0]
                requests.append(target)
                if target == '/空间/images/合照.png':
                    await intercept.fulfill(body=PNG, content_type='image/png')
                    return
                assert target in fixtures, target
                if target == '/空间/慢目录/README.md':
                    slow_started.set()
                    await slow_gate.wait()
                await intercept.fulfill(body=fixtures[target], content_type='application/octet-stream', headers={'X-Preview-Truncated': 'false'})
                return
            elif path == '/favicon.ico':
                await intercept.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + request.url)
            await intercept.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        await page.get_by_role('button', name='进入空间 空间', exact=True).click()
        readme = page.locator('#directory-readme-content')
        await expect(readme.locator('h1')).to_have_text('项目说明')
        await expect(readme.locator('table')).to_be_visible()
        await expect(readme.locator('.hljs-keyword').first).to_have_text('const')
        await expect(readme.locator('script,svg,iframe,input,[onclick],[onerror]')).to_have_count(0)
        assert not await page.evaluate('Boolean(window.richAttack)')
        external = readme.get_by_role('link', name='外链', exact=True)
        await expect(external).to_have_attribute('target', '_blank')
        await expect(external).to_have_attribute('rel', 'noopener noreferrer')
        for name in ('危险', '编码斜杠', '越界'):
            await expect(readme.locator('a').filter(has_text=name).first).not_to_have_attribute('href')
        await expect(readme.locator('img')).to_have_count(1)
        assert 'external.invalid' not in '\n'.join(requests)
        await readme.get_by_role('link', name='文档', exact=True).click()
        rich = page.locator('#rich-preview-content')
        await expect(rich.locator('h1')).to_have_text('相对链接文档')
        assert metadata_requests[-1] == '/空间/docs/说明 %2F.md'
        await page.locator('#preview-close').click()
        await page.locator('#directory-readme-open').click()
        await expect(rich.locator('h1')).to_have_text('项目说明')
        await page.locator('#rich-preview-toggle').click()
        await expect(page.locator('#preview-content')).to_be_visible()
        await expect(page.locator('#preview-content')).to_have_text(MARKDOWN)
        await page.locator('#rich-preview-toggle').click()
        await expect(rich).to_be_visible()
        await page.locator('#preview-close').click()

        async def open_file(name):
            await page.get_by_role('button', name='选择文件：' + name, exact=True).dblclick()
            await expect(page.locator('#preview-loading')).to_be_hidden()

        await open_file('示例.json')
        await expect(rich.locator('.hljs-attr')).to_have_text('"answer"')
        await page.locator('#preview-close').click()
        await open_file('标签.html')
        await expect(rich).to_contain_text('<script>window.richAttack=true</script>')
        await expect(rich.locator('script')).to_have_count(0)
        await expect(page.locator('#text-editor-open')).to_be_hidden()
        await page.locator('#preview-close').click()
        await open_file('中文.md')
        await expect(rich.locator('h1')).to_have_text('中文编码')
        count = requests.count('/空间/中文.md')
        await page.locator('#preview-encoding').select_option('gb18030')
        await expect(rich.locator('h1')).to_have_text('中文编码')
        assert requests.count('/空间/中文.md') == count
        await page.locator('#preview-close').click()
        await open_file('大文件.md')
        await expect(page.locator('#rich-preview-notice')).to_contain_text('256 KiB')
        await expect(page.locator('#preview-content')).to_be_visible()
        await page.locator('#preview-close').click()
        await open_file('图片.md')
        await expect(rich.locator('img')).to_have_count(20)
        await expect(rich).to_contain_text('图片20：图片未加载')
        await page.locator('#preview-close').click()
        worker_hang = True
        await open_file('示例.json')
        await expect(page.locator('#rich-preview-notice')).to_contain_text('已停止渲染')
        await expect(page.locator('#preview-content')).to_be_visible()
        await page.locator('#preview-close').click()
        worker_hang = False
        await open_file('代码块.md')
        await expect(page.locator('#rich-preview-notice')).to_contain_text('较长代码块')
        await expect(rich.locator('code')).to_contain_text('const long = 1;')
        await expect(rich.locator('.hljs-keyword')).to_have_count(0)
        await page.locator('#preview-close').click()
        await open_file('元素.md')
        await expect(page.locator('#rich-preview-notice')).to_contain_text('元素过多')
        await expect(page.locator('#preview-content')).to_be_visible()
        await page.locator('#preview-close').click()

        hold_metadata = True
        await page.locator('#directory-readme-open').click()
        await expect(rich.locator('h1')).to_have_text('项目说明')
        await rich.get_by_role('link', name='文档', exact=True).click()
        await asyncio.wait_for(metadata_started.wait(), 5)
        await page.locator('#preview-close').click()
        metadata_gate.set()
        await expect(page.locator('#preview-modal')).not_to_be_visible()
        hold_metadata = False
        await page.get_by_role('button', name='打开文件夹：慢目录', exact=True).click()
        await asyncio.wait_for(slow_started.wait(), 5)
        await page.locator('#up-button').click()
        await page.get_by_role('button', name='打开文件夹：空目录', exact=True).click()
        slow_gate.set()
        await expect(page.locator('#directory-readme')).to_be_hidden()
        await page.locator('#up-button').click()
        await expect(readme.locator('h1')).to_have_text('项目说明')
        await page.screenshot(path='/tmp/wps-rich-preview-desktop.png')
        await page.set_viewport_size({'width': 390, 'height': 844})
        await page.locator('#directory-readme-open').click()
        await expect(rich.locator('h1')).to_have_text('项目说明')
        await page.screenshot(path='/tmp/wps-rich-preview-mobile.png')
        box = await page.locator('#preview-modal').bounding_box()
        assert box['x'] >= 0 and box['x'] + box['width'] <= 390, box
        await page.locator('#preview-close').click()
        assert not errors, errors
        await context.close()
        await browser.close()
    print('PASS: Markdown/code rendering, sanitization, relative paths/images, size/node/image/highlight limits, worker timeout, README races and mobile')


if __name__ == '__main__':
    asyncio.run(main())
