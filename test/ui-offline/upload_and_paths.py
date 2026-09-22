"""Offline regressions for upload queue recovery and literal URL characters."""
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
        errors, writes, listings = [], [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        special = '报告%2F #进度100%'
        files = []
        gate = asyncio.Event()
        fail_bad = True

        async def route(request):
            url = urlparse(request.request.url)
            path = url.path
            if path == '/':
                await request.fulfill(body=(WEB / 'index.html').read_text(), content_type='text/html')
                return
            if path.startswith('/assets/'):
                asset = WEB / path.removeprefix('/assets/')
                await request.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
                return
            data = {}
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
                data = {'version': '1.0.21'}
            elif path.endswith('/update'):
                data = {'state': 'idle', 'current_version': '1.0.21', 'update_available': False}
            elif path.endswith('/entries'):
                folder = parse_qs(url.query)['path'][0]
                listings.append(folder)
                data = {'entries': ([{'id': 'space:test', 'name': special, 'kind': 'folder'}]
                                    if folder == '/' else list(files))}
            elif path.endswith('/upload'):
                target = parse_qs(url.query)['path'][0]
                writes.append(target)
                name = target.rsplit('/', 1)[-1]
                if name == 'bad.txt' and fail_bad:
                    await request.fulfill(status=503, json={'error': 'Test upload failure'})
                    return
                if name == 'good.txt':
                    await gate.wait()
                files.append({'id': name, 'name': name, 'kind': 'file', 'size': 4})
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
        assert await page.evaluate('decodeURIComponent(location.hash.slice(1))') == '/' + special
        await page.reload()
        await expect(page.locator('#auth-screen')).to_be_hidden()
        await expect(page.locator('#skeleton')).to_be_hidden()
        assert [item for item in listings if item != '/'] == ['/' + special] * 2, listings
        await page.locator('#space-root').click()
        await page.go_back()
        await expect(page.locator('#breadcrumbs')).to_contain_text(special)
        assert await page.evaluate('decodeURIComponent(location.hash.slice(1))') == '/' + special

        def file(name):
            return {'name': name, 'mimeType': 'text/plain', 'buffer': b'test'}

        await page.locator('#file-input').set_input_files([file('bad.txt'), file('good.txt')])
        failed = page.locator('#tray-list .tray-item').nth(0)
        await expect(failed).to_have_class('tray-item error')
        await expect(failed.locator('button')).to_be_disabled()
        gate.set()
        await expect(page.locator('#entries .entry-name')).to_have_text(['good.txt'])
        await expect(failed.locator('button')).to_be_enabled()
        fail_bad = False
        await failed.locator('button').click()
        await expect(failed).to_have_class('tray-item done')
        await expect(page.locator('#entries .entry-name')).to_have_text(['bad.txt', 'good.txt'])
        assert len(writes) == 3, writes
        await expect(page.locator('#upload-button')).to_be_enabled()

        # Cancelling the queue while overwrite confirmation is open must prevent PUT.
        await page.locator('#file-input').set_input_files([file('good.txt')])
        await expect(page.locator('#modal')).to_be_visible()
        # The modal makes the tray inert; invoke its action as a queued cancel event.
        await page.evaluate('document.getElementById("tray-cancel").click()')
        await page.locator('#modal-form button[type="submit"]').click()
        await expect(page.locator('#tray-list .tray-item')).to_have_class('tray-item cancelled')
        assert len(writes) == 3, writes
        assert not errors, errors
        print('PASS: special-character reload/history, retry during active queue, partial success refresh, retry recovery, cancellation before overwrite')
        await browser.close()


if __name__ == '__main__':
    asyncio.run(main())
