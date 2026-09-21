"""Create plain text documents with offline upload and preview fixtures."""
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
        errors, uploads = [], []
        files = {'已有.txt': b'original'}
        upload_started, upload_gate = asyncio.Event(), asyncio.Event()
        listing_started, listing_gate = asyncio.Event(), asyncio.Event()
        hold_listing = True
        page.on('pageerror', lambda error: errors.append(str(error)))

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
            elif path.endswith('/status'):
                data = {'status': 'connected'}
            elif path == '/healthz':
                data = {'version': 'offline'}
            elif path.endswith('/update'):
                data = {'state': 'idle', 'update_available': False}
            elif path.endswith('/entries'):
                folder = parse_qs(url.query)['path'][0]
                if folder != '/' and hold_listing:
                    listing_started.set()
                    await listing_gate.wait()
                data = {'entries': ([{'id': 'space:test', 'name': '测试%2F 空间', 'kind': 'folder'}] if folder == '/' else
                                    [{'id': name, 'name': name, 'kind': 'file', 'size': len(body)} for name, body in files.items()])}
            elif path.endswith('/upload'):
                assert request.request.method == 'PUT'
                query = parse_qs(url.query)
                assert 'overwrite' not in query
                target = query['path'][0]
                body = request.request.post_data_buffer or b''
                uploads.append((target, body))
                name = target.rsplit('/', 1)[-1]
                if name in files or name == '竞态.txt':
                    await request.fulfill(status=409, json={'error': 'already exists'})
                    return
                if name == '失败.txt':
                    await request.fulfill(status=503, json={'error': '测试保存失败'})
                    return
                if name == '笔记%2F #中文.md':
                    upload_started.set()
                    await upload_gate.wait()
                files[name] = body
                await request.fulfill(status=201, json={'path': target})
                return
            elif path.endswith('/preview'):
                name = parse_qs(url.query)['path'][0].rsplit('/', 1)[-1]
                await request.fulfill(body=files[name], content_type='application/octet-stream', headers={'X-Preview-Truncated': 'false'})
                return
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        await expect(page.locator('#text-file-button')).to_be_disabled()
        await page.locator('#space-list > .directory-node > .directory-row > .tree-link').click()
        await asyncio.wait_for(listing_started.wait(), 5)
        await expect(page.locator('#text-file-button')).to_be_disabled()
        hold_listing = False
        listing_gate.set()
        await expect(page.locator('#text-file-button')).to_be_enabled()
        await page.locator('#text-file-button').click()
        for name, message in [('bad.exe', '扩展名'), ('../bad.txt', '有效文件名'), ('bad\\name.txt', '有效文件名'), ('已有.txt', '同名项目')]:
            await page.locator('#text-file-name').fill(name)
            await page.locator('#text-file-save').click()
            await expect(page.locator('#text-file-error')).to_contain_text(message)
        assert not uploads

        content = '# 中文笔记\n第一行\t测试 😀\n<script>window.createdScript = true</script>\n'
        await page.locator('#text-file-name').fill('失败.txt')
        await page.locator('#text-file-content').fill(content)
        await page.locator('#text-file-save').click()
        await expect(page.locator('#text-file-error')).to_have_text('测试保存失败')
        await expect(page.locator('#text-file-content')).to_have_value(content)
        await page.locator('#text-file-name').fill('竞态.txt')
        await page.locator('#text-file-save').click()
        await expect(page.locator('#text-file-error')).to_contain_text('同名项目')
        await expect(page.locator('#text-file-content')).to_have_value(content)
        await page.keyboard.press('Escape')
        # Navigating with a closed draft must not silently change its destination.
        await page.evaluate("location.hash = encodeURIComponent('/测试%2F 空间/子目录')")
        await expect(page.locator('#breadcrumbs')).to_contain_text('子目录')
        await page.locator('#text-file-button').click()
        await expect(page.locator('#text-file-target')).to_have_text('保存到：/测试%2F 空间')
        await expect(page.locator('#text-file-content')).to_have_value(content)
        await page.locator('#text-file-close').click()
        await page.evaluate("location.hash = encodeURIComponent('/测试%2F 空间')")
        await expect(page.locator('#breadcrumbs')).not_to_contain_text('子目录')
        await page.locator('#text-file-button').click()
        await page.locator('#text-file-name').fill('笔记%2F #中文.md')
        await page.screenshot(path='/tmp/wps-text-create-desktop.png', animations='disabled')
        await page.locator('#text-file-save').click()
        await asyncio.wait_for(upload_started.wait(), 5)
        await expect(page.locator('#text-file-save')).to_be_disabled()
        await expect(page.locator('#text-file-close')).to_be_disabled()
        await page.keyboard.press('Escape')
        await expect(page.locator('#text-file-modal')).to_be_visible()
        upload_gate.set()
        await expect(page.locator('#text-file-modal')).not_to_be_visible()
        assert uploads[-1] == ('/测试%2F 空间/笔记%2F #中文.md', content.encode('utf-8'))
        assert len(uploads) == 3
        await page.get_by_role('button', name='选择文件：笔记%2F #中文.md', exact=True).dblclick()
        await expect(page.locator('#preview-content')).to_have_text(content)
        assert not await page.evaluate('Boolean(window.createdScript)')
        await page.locator('#preview-close').click()
        await page.locator('#text-file-button').click()
        await expect(page.locator('#text-file-content')).to_have_value('')
        await page.locator('#text-file-name').fill('空文件.json')
        await page.locator('#text-file-save').click()
        await expect(page.locator('#text-file-modal')).not_to_be_visible()
        await expect(page.get_by_role('button', name='选择文件：空文件.json', exact=True)).to_be_visible()
        assert files['空文件.json'] == b''
        assert files['已有.txt'] == b'original'
        await page.locator('#text-file-button').click()
        await page.locator('#text-file-content').fill('手机端草稿')
        await page.set_viewport_size({'width': 390, 'height': 844})
        await page.screenshot(path='/tmp/wps-text-create-mobile.png', animations='disabled')
        box = await page.locator('#text-file-modal').bounding_box()
        assert box['x'] >= 0 and box['x'] + box['width'] <= 390, box
        await expect(page.locator('#text-file-save')).to_be_in_viewport()
        assert not errors, errors
        await context.close()
        await browser.close()
    print('PASS: creation, UTF-8 bytes, paths, preview, empty file, collision, retained draft, failure, busy and mobile')


if __name__ == '__main__':
    asyncio.run(main())
