"""Text editor revisions, drafts and UTF-8 saving; local responses only."""
import asyncio
import hashlib
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
        errors, writes = [], []
        page.on('pageerror', lambda err: errors.append(str(err)))
        fixtures = {
            '中文 %2F.txt': '初始中文'.encode(),
            '旧文档.txt': '旧编码中文'.encode('gb18030'),
            '繁体.csv': '繁體中文'.encode('big5'),
            '空文件.md': b'',
            '二进制.txt': b'abc\0def',
            '大文件.txt': b'large',
            '慢请求.txt': b'slow',
        }
        revisions = {name: '"' + hashlib.sha256(name.encode() + body).hexdigest() + '"' for name, body in fixtures.items()}
        put_status = 200
        put_code = ''
        unauthorized = False
        slow_started, slow_gate = asyncio.Event(), asyncio.Event()
        save_started, save_gate = asyncio.Event(), asyncio.Event()
        hold_save = False

        async def route(request):
            nonlocal revisions
            req = request.request
            url = urlparse(req.url)
            params = parse_qs(url.query)
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
            elif path.endswith('/auth/login'):
                data = {'user': {'username': req.post_data_json['username']}}
            elif path.endswith('/settings'):
                data = {'name': 'WPS Drive'}
            elif path.endswith('/status'):
                data = {'status': 'connected'}
            elif path == '/healthz':
                data = {'version': 'offline'}
            elif path.endswith('/update'):
                data = {'state': 'idle', 'update_available': False}
            elif path.endswith('/tasks'):
                data = {'tasks': []}
            elif path.endswith('/entries'):
                data = {'entries': ([{'id': 'space:test', 'name': '空间', 'kind': 'folder'}] if params['path'][0] == '/' else
                                    [{'id': name, 'name': name, 'kind': 'file', 'size': len(body)} for name, body in fixtures.items()])}
            elif path.endswith('/preview'):
                name = params['path'][0].rsplit('/', 1)[-1]
                if unauthorized:
                    await request.fulfill(status=401, body='Session expired', content_type='text/plain')
                    return
                await request.fulfill(body=fixtures[name], content_type='application/octet-stream')
                return
            elif path.endswith('/text'):
                name = params['path'][0].rsplit('/', 1)[-1]
                if unauthorized:
                    await request.fulfill(status=401, body='Session expired', content_type='text/plain')
                    return
                if req.method == 'GET':
                    if name == '慢请求.txt':
                        slow_started.set()
                        await slow_gate.wait()
                    if name == '大文件.txt':
                        await request.fulfill(status=413, json={'error': 'too large'})
                    else:
                        await request.fulfill(body=fixtures[name], content_type='application/octet-stream', headers={'ETag': revisions[name]})
                    return
                assert req.method == 'PUT'
                assert req.headers['if-match'] == revisions[name]
                assert req.headers['content-type'] == 'text/plain; charset=utf-8'
                writes.append((params['path'][0], req.post_data_buffer))
                if hold_save:
                    save_started.set()
                    await save_gate.wait()
                if put_status != 200:
                    await request.fulfill(status=put_status, json={'error': 'offline save failure', 'code': put_code})
                    return
                fixtures[name] = req.post_data_buffer
                revisions[name] = '"' + hashlib.sha256(name.encode() + fixtures[name]).hexdigest() + '"'
                data = {'path': params['path'][0], 'entry': {'id': name, 'name': name, 'kind': 'file', 'size': len(fixtures[name])}, 'revision': revisions[name]}
                await request.fulfill(json=data, headers={'ETag': revisions[name]})
                return
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        await page.get_by_role('button', name='进入空间 空间', exact=True).click()
        await expect(page.locator('#skeleton')).to_be_hidden()

        async def open_editor(name):
            await page.get_by_role('button', name='选择文件：' + name, exact=True).dblclick()
            await expect(page.locator('#preview-loading')).to_be_hidden()
            await page.locator('#text-editor-open').click()
            await expect(page.locator('#text-editor-path')).to_have_text('/空间/' + name)

        async def close_both():
            await page.locator('#text-editor-close').click()
            await page.locator('#preview-close').click()

        content = page.locator('#text-editor-content')
        await open_editor('中文 %2F.txt')
        await expect(content).to_have_value('初始中文')
        await expect(page.locator('#text-editor-save')).to_be_disabled()
        await content.fill('修改中文\n第二行')
        await page.locator('#text-editor-save').click()
        await expect(page.locator('#text-editor-status')).to_have_text('已保存为 UTF-8')
        assert writes[-1] == ('/空间/中文 %2F.txt', '修改中文\n第二行'.encode())
        await content.fill('第二次修改')
        await page.keyboard.press('Control+s')
        await expect(page.locator('#text-editor-status')).to_have_text('已保存为 UTF-8')
        await content.fill('保留的草稿')
        put_status = 412
        await page.locator('#text-editor-save').click()
        await expect(page.locator('#text-editor-error')).to_contain_text('未覆盖远端内容')
        await expect(content).to_have_value('保留的草稿')
        await close_both()
        await open_editor('中文 %2F.txt')
        await expect(content).to_have_value('保留的草稿')
        page.once('dialog', lambda d: d.dismiss())
        await page.locator('#text-editor-reload').click()
        await expect(content).to_have_value('保留的草稿')
        page.once('dialog', lambda d: d.accept())
        await page.locator('#text-editor-reload').click()
        await expect(content).to_have_value('第二次修改')
        put_status = 503
        await content.fill('服务器失败草稿')
        await page.locator('#text-editor-save').click()
        await expect(page.locator('#text-editor-error')).to_have_text('offline save failure')
        await expect(content).to_have_value('服务器失败草稿')
        for put_status, put_code in [(502, 'text_save_uncertain'), (409, 'text_save_unverified')]:
            await page.locator('#text-editor-save').click()
            await expect(page.locator('#text-editor-error')).to_contain_text('远端可能已改变')
            await expect(content).to_have_value('服务器失败草稿')
        put_status = 200
        put_code = ''
        hold_save = True
        await page.locator('#text-editor-save').click()
        await asyncio.wait_for(save_started.wait(), 5)
        await expect(page.locator('#text-editor-save')).to_be_disabled()
        await expect(page.locator('#text-editor-close')).to_be_disabled()
        await page.keyboard.press('Escape')
        await expect(page.locator('#text-editor-modal')).to_be_visible()
        save_gate.set()
        await expect(page.locator('#text-editor-status')).to_have_text('已保存为 UTF-8')
        hold_save = False
        await close_both()
        await open_editor('旧文档.txt')
        await expect(content).to_have_value('旧编码中文')
        await content.fill('转换为 UTF-8')
        await page.locator('#text-editor-save').click()
        await expect(page.locator('#text-editor-status')).to_have_text('已保存为 UTF-8')
        assert writes[-1][1] == '转换为 UTF-8'.encode()
        await close_both()
        await open_editor('繁体.csv')
        await expect(page.locator('#text-editor-encoding')).to_be_enabled()
        await page.locator('#text-editor-encoding').select_option('big5')
        await expect(content).to_have_value('繁體中文')
        await close_both()
        await open_editor('空文件.md')
        await expect(content).to_have_value('')
        await expect(content).to_be_enabled()
        await content.fill('新的内容')
        await page.locator('#text-editor-save').click()
        await expect(page.locator('#text-editor-status')).to_have_text('已保存为 UTF-8')
        await page.screenshot(path='/tmp/wps-text-editor-desktop.png')
        await page.emulate_media(color_scheme='dark')
        await page.screenshot(path='/tmp/wps-text-editor-dark.png')
        await page.emulate_media(color_scheme='light')
        await page.set_viewport_size({'width': 390, 'height': 844})
        await page.screenshot(path='/tmp/wps-text-editor-mobile.png')
        box = await page.locator('#text-editor-modal').bounding_box()
        assert box['x'] >= 0 and box['x'] + box['width'] <= 390, box
        await close_both()
        await page.set_viewport_size({'width': 1366, 'height': 900})
        await open_editor('二进制.txt')
        await expect(page.locator('#text-editor-error')).to_contain_text('二进制')
        await expect(page.locator('#text-editor-save')).to_be_disabled()
        await close_both()
        await open_editor('大文件.txt')
        await expect(page.locator('#text-editor-error')).to_contain_text('2 MiB')
        await close_both()
        await open_editor('慢请求.txt')
        await asyncio.wait_for(slow_started.wait(), 5)
        await close_both()
        await open_editor('空文件.md')
        slow_gate.set()
        await expect(content).to_have_value('新的内容')
        await close_both()
        await open_editor('空文件.md')
        await expect(content).to_have_value('新的内容')
        await content.fill('登录过期后的草稿')
        unauthorized = True
        await page.locator('#text-editor-save').click()
        await expect(page.locator('#text-editor-modal')).not_to_be_visible()
        await expect(page.locator('#preview-modal')).not_to_be_visible()
        await expect(page.locator('#auth-screen')).to_be_visible()
        await expect(content).to_have_value('')
        unauthorized = False
        await page.locator('#login-username').fill('demo')
        await page.locator('#login-password').fill('offline-password')
        await page.locator('#login-submit').click()
        await expect(page.locator('#auth-screen')).to_be_hidden()
        await open_editor('空文件.md')
        await expect(content).to_have_value('登录过期后的草稿')
        unauthorized = True
        await page.locator('#text-editor-save').click()
        await expect(page.locator('#auth-screen')).to_be_visible()
        await expect(content).to_have_value('')
        unauthorized = False
        await page.locator('#login-username').fill('different-user')
        await page.locator('#login-password').fill('offline-password')
        await page.locator('#login-submit').click()
        await expect(page.locator('#auth-screen')).to_be_hidden()
        await page.locator('#space-list > .directory-node > .directory-row > .tree-link').click()
        await open_editor('空文件.md')
        await expect(content).to_have_value('新的内容')
        assert not errors, errors
        await context.close()
        await browser.close()
    print('PASS: revisions, UTF-8, legacy decoding, empty files, conflict/error drafts, cancellation, save lifecycle and mobile')


if __name__ == '__main__':
    asyncio.run(main())
