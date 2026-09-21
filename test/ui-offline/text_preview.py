"""Text previews use local fixtures only; no WPS access."""
import asyncio
import os
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go' / 'web'


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context()
        page = await context.new_page()
        errors, requests = [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        fixtures = {
            '中文.txt': ('中文阅读\n第二行', 'utf-8'),
            '旧文档.LOG': ('中文编码测试', 'gb18030'),
            '繁体.csv': ('繁體中文', 'big5'),
            'UTF16.ini': ('中文配置', 'utf-16'),
            'UTF16BE.conf': ('\ufeff中文配置', 'utf-16be'),
            '空文件.md': ('', 'utf-8'),
            '标签.xml': ('<script>window.previewExecuted=true</script>', 'utf-8'),
            '截断.json': ('中', 'utf-8'),
            '截断GBK.yaml': ('中', 'gb18030'),
            '截断UTF16.yml': ('\ufeff中', 'utf-16le'),
            '二进制.toml': ('abc\x00def', 'utf-8'),
            '慢请求.txt': ('旧内容', 'utf-8'),
            '失败.txt': ('', 'utf-8'),
            '不支持.bin': ('binary', 'utf-8'),
        }
        slow_started, slow_gate, aborted = asyncio.Event(), asyncio.Event(), asyncio.Event()

        def request_failed(request):
            if 'preview?' in request.url and '慢请求.txt' in parse_qs(urlparse(request.url).query).get('path', [''])[0]:
                aborted.set()
        page.on('requestfailed', request_failed)

        async def route(request):
            url = urlparse(request.request.url)
            path = url.path
            if path == '/':
                await request.fulfill(body=(WEB / 'index.html').read_bytes(), content_type='text/html')
                return
            if path.startswith('/assets/'):
                asset = WEB / path.removeprefix('/assets/')
                await request.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
                return
            if path.endswith('/auth/me'):
                data = {'authenticated': True, 'user': {'username': 'demo'}}
            elif path.endswith('/settings'):
                data = {'name': 'WPS Drive'}
            elif path.endswith('/tasks'):
                data = {'tasks': []}
            elif path.endswith('/status'):
                data = {'status': 'connected'}
            elif path == '/healthz':
                data = {'version': 'offline'}
            elif path.endswith('/update'):
                data = {'state': 'idle', 'update_available': False}
            elif path.endswith('/entries'):
                folder = parse_qs(url.query)['path'][0]
                data = {'entries': ([{'id': 'space:test', 'name': '测试空间', 'kind': 'folder'}] if folder == '/' else
                                    [{'id': name, 'name': name, 'kind': 'file', 'size': len(text.encode(encoding))}
                                     for name, (text, encoding) in fixtures.items()])}
            elif path.endswith('/preview'):
                name = parse_qs(url.query)['path'][0].rsplit('/', 1)[-1]
                requests.append(name)
                if name == '慢请求.txt':
                    slow_started.set()
                    await slow_gate.wait()
                if name == '失败.txt':
                    await request.fulfill(status=503, json={'error': '测试读取失败'})
                    return
                text, encoding = fixtures[name]
                body = text.encode(encoding)
                truncated = name.startswith('截断')
                if truncated:
                    # Leave an incomplete second character after the complete first.
                    body += '文'.encode(encoding)[:1]
                await request.fulfill(body=body, content_type='application/octet-stream', headers={
                    'X-Preview-Truncated': str(truncated).lower(), 'X-Preview-Limit': str(len(body)),
                })
                return
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        await page.locator('#space-list > .directory-node > .directory-row > .tree-link').click()
        await expect(page.locator('#skeleton')).to_be_hidden()

        async def open_preview(name):
            await page.get_by_role('button', name='选择文件：' + name, exact=True).dblclick()
            await expect(page.locator('#preview-title')).to_have_text(name)

        async def check(name, expected):
            await open_preview(name)
            await expect(page.locator('#preview-content')).to_be_visible()
            await expect(page.locator('#preview-content')).to_have_text(expected)
            await page.locator('#preview-close').click()

        # Both layouts support double-clicking names and the surrounding item.
        for view in ('list', 'grid'):
            await page.locator('#view-' + view + '-button').click()
            name = page.get_by_role('button', name='选择文件：中文.txt', exact=True)
            await name.click()
            await expect(page.locator('#preview-modal')).not_to_be_visible()
            count = len(requests)
            await check('中文.txt', '中文阅读\n第二行')
            assert len(requests) == count + 1, 'double click must open only once'
            row = page.locator('[data-entry-path="/测试空间/中文.txt"]')
            await row.locator('.entry-glyph').dblclick()
            await expect(page.locator('#preview-content')).to_have_text('中文阅读\n第二行')
            await page.locator('#preview-close').click()
            count = len(requests)
            await page.get_by_role('button', name='选择文件：不支持.bin', exact=True).dblclick()
            await expect(page.locator('#preview-modal')).not_to_be_visible()
            assert len(requests) == count
        await page.locator('#view-list-button').click()
        # Keep the menu entry available as well.
        await page.get_by_role('button', name='选择文件：中文.txt', exact=True).click(button='right')
        await page.get_by_role('menuitem', name='在线浏览', exact=True).click()
        await expect(page.locator('#preview-content')).to_have_text('中文阅读\n第二行')
        await page.locator('#preview-close').click()

        await check('旧文档.LOG', '中文编码测试')
        await check('UTF16.ini', '中文配置')
        await check('UTF16BE.conf', '中文配置')
        await open_preview('繁体.csv')
        await expect(page.locator('#preview-loading')).to_be_hidden()
        count = len(requests)
        await page.locator('#preview-encoding').select_option('big5')
        await expect(page.locator('#preview-content')).to_have_text('繁體中文')
        assert len(requests) == count, 'encoding switch fetched bytes again'
        await page.locator('#preview-close').click()
        await open_preview('空文件.md')
        await expect(page.locator('#preview-note')).to_have_text('这是一个空文件。')
        await page.locator('#preview-close').click()
        await open_preview('标签.xml')
        await expect(page.locator('#preview-content')).to_have_text(fixtures['标签.xml'][0])
        assert not await page.evaluate('Boolean(window.previewExecuted)')
        assert await page.locator('#preview-content script').count() == 0
        await page.locator('#preview-close').click()
        for name in ('截断.json', '截断GBK.yaml', '截断UTF16.yml'):
            await open_preview(name)
            await expect(page.locator('#preview-content')).to_have_text('中')
            await expect(page.locator('#preview-note')).to_contain_text('下载完整文件')
            assert '2 MB' not in await page.locator('#preview-note').inner_text()
            await page.locator('#preview-close').click()
        await open_preview('二进制.toml')
        await expect(page.locator('#preview-error')).to_contain_text('二进制或控制字符')
        await expect(page.locator('#preview-content')).to_be_hidden()
        await page.locator('#preview-close').click()
        await open_preview('失败.txt')
        await expect(page.locator('#preview-error')).to_have_text('测试读取失败')
        await page.locator('#preview-close').click()
        await open_preview('慢请求.txt')
        await asyncio.wait_for(slow_started.wait(), 5)
        await page.keyboard.press('Escape')
        await asyncio.wait_for(aborted.wait(), 5)
        await open_preview('中文.txt')
        slow_gate.set()
        await expect(page.locator('#preview-content')).to_have_text('中文阅读\n第二行')
        await page.locator('#preview-wrap').uncheck()
        await expect(page.locator('#preview-content')).to_have_css('white-space', 'pre')
        await page.locator('#preview-wrap').check()
        await expect(page.locator('#preview-content')).to_have_css('white-space', 'pre-wrap')
        await page.locator('#preview-font').select_option('20')
        await expect(page.locator('#preview-content')).to_have_css('font-size', '20px')
        await page.locator('#preview-fullscreen').click()
        await expect(page.locator('#preview-fullscreen')).to_have_attribute('aria-pressed', 'true')
        await page.screenshot(path='/tmp/wps-text-preview-desktop.png')
        await page.set_viewport_size({'width': 390, 'height': 844})
        await page.screenshot(path='/tmp/wps-text-preview-mobile.png')
        box = await page.locator('#preview-modal').bounding_box()
        assert box['x'] >= 0 and box['x'] + box['width'] <= 390, box
        await expect(page.locator('#preview-close')).to_be_in_viewport()
        await page.locator('#preview-fullscreen').click()
        await page.locator('#preview-close').click()
        assert not errors, errors
        await context.close()
        await browser.close()
    print('PASS: formats, encodings, truncation, safe text, errors, cancellation and responsive controls')


if __name__ == '__main__':
    asyncio.run(main())
