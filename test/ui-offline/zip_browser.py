"""ZIP browser navigation and native downloads from local API responses."""
import asyncio
import os
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go/web'


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context(viewport={'width': 1366, 'height': 900})
        page = await context.new_page()
        errors, queries = [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        archive_name = '资料 %2F.zip'
        fail_listing = False
        fail_download = False
        expired = False
        hold = False
        started, gate = asyncio.Event(), asyncio.Event()

        async def route(route):
            req = route.request
            url = urlparse(req.url)
            params = parse_qs(url.query)
            path = url.path
            if path == '/':
                await route.fulfill(body=(WEB / 'index.html').read_bytes(), content_type='text/html')
                return
            if path.startswith('/assets/'):
                asset = WEB / path.removeprefix('/assets/')
                await route.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
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
            elif path.endswith('/transfers'):
                data = {'tasks': []}
            elif path.endswith('/tasks'):
                data = {'tasks': []}
            elif path.endswith('/zip/entries'):
                queries.append(params)
                assert params['path'] == ['/空间/' + archive_name]
                if hold:
                    started.set()
                    await gate.wait()
                if fail_listing or expired:
                    await route.fulfill(status=401 if expired else 400, json={'error': 'ZIP 已损坏或不受支持'})
                    return
                entry = params['entry'][0]
                data = {'path': params['path'][0], 'entry': entry, 'total_entries': 4, 'entries': (
                    [{'name': '目录 %2F', 'path': '/目录 %2F', 'kind': 'folder', 'size': 0},
                     {'name': '<script>.txt', 'path': '/<script>.txt', 'kind': 'file', 'size': 7}]
                    if entry == '/' else [{'name': 'file.txt', 'path': '/目录 %2F/file.txt', 'kind': 'file', 'size': 7}])}
            elif path.endswith('/zip/download'):
                assert params['path'] == ['/空间/' + archive_name]
                assert params['entry'] == ['/目录 %2F/file.txt']
                if fail_download:
                    await route.fulfill(status=503, json={'error': '压缩包下载暂时失败'})
                else:
                    await route.fulfill(body=b'payload', content_type='application/octet-stream', headers={'Content-Disposition': 'attachment; filename="file.txt"'})
                return
            elif path.endswith('/entries'):
                data = {'entries': ([{'id': 'space:test', 'name': '空间', 'kind': 'folder'}] if params['path'][0] == '/' else
                                    [{'id': 'zip-id', 'name': archive_name, 'kind': 'file', 'size': 1000}])}
            elif path == '/favicon.ico':
                await route.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request ' + path)
            await route.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        await page.get_by_role('button', name='进入空间 空间', exact=True).click()
        await expect(page.locator('#skeleton')).to_be_hidden()

        async def open_zip():
            await page.get_by_role('button', name='选择文件：' + archive_name, exact=True).dblclick()
            await expect(page.locator('#zip-browser-modal')).to_be_visible()

        await open_zip()
        await expect(page.locator('#zip-browser-list')).to_contain_text('<script>.txt')
        assert await page.locator('#zip-browser-list script').count() == 0
        await expect(page.locator('#zip-browser-up')).to_be_disabled()
        await page.get_by_role('button', name='打开：目录 %2F', exact=True).click()
        await expect(page.locator('#zip-browser-path')).to_have_text('/目录 %2F')
        await expect(page.locator('#zip-browser-list')).to_contain_text('file.txt')
        assert queries[-1]['entry'] == ['/目录 %2F']
        async with page.expect_download() as pending:
            await page.get_by_role('button', name='下载：file.txt', exact=True).click()
        download = await pending.value
        assert download.suggested_filename == 'file.txt'
        assert Path(await download.path()).read_bytes() == b'payload'
        fail_download = True
        await page.get_by_role('button', name='下载：file.txt', exact=True).click()
        await expect(page.locator('#zip-browser-error')).to_have_text('压缩包下载暂时失败')
        await page.locator('#zip-browser-up').click()
        await expect(page.locator('#zip-browser-list')).to_contain_text('<script>.txt')
        await page.screenshot(path='/tmp/wps-zip-browser-desktop.png')
        await page.set_viewport_size({'width': 390, 'height': 844})
        await page.screenshot(path='/tmp/wps-zip-browser-mobile.png')
        box = await page.locator('#zip-browser-modal').bounding_box()
        assert box['x'] >= 0 and box['x'] + box['width'] <= 390, box
        await page.locator('#zip-browser-close').click()
        assert await page.locator('iframe[title="ZIP 文件下载"]').count() == 0
        await page.set_viewport_size({'width': 1366, 'height': 900})
        fail_listing = True
        await open_zip()
        await expect(page.locator('#zip-browser-error')).to_have_text('ZIP 已损坏或不受支持')
        fail_listing = False
        await page.locator('#zip-browser-refresh').click()
        await expect(page.locator('#zip-browser-error')).to_have_text('')
        await expect(page.locator('#zip-browser-list')).to_contain_text('目录 %2F')
        await page.locator('#zip-browser-close').click()
        hold = True
        await open_zip()
        await asyncio.wait_for(started.wait(), 5)
        await page.keyboard.press('Escape')
        hold = False
        await open_zip()
        gate.set()
        await expect(page.locator('#zip-browser-list')).to_contain_text('目录 %2F')
        expired = True
        await page.locator('#zip-browser-refresh').click()
        await expect(page.locator('#auth-screen')).to_be_visible()
        await expect(page.locator('#zip-browser-modal')).not_to_be_visible()
        await expect(page.locator('#zip-browser-list')).to_be_empty()
        assert not errors, errors
        await context.close()
        await browser.close()
    print('PASS: ZIP paths, safe names, hierarchy, native download/error, cancel, auth reset and mobile')


if __name__ == '__main__':
    asyncio.run(main())
