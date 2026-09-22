"""Share creation/revocation plus independent bearer-to-grant public viewer."""
import asyncio
import copy
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect
from media_preview import PNG, pdf_fixture

ROOT = Path(__file__).resolve().parents[2]
WEB = ROOT / 'go' / 'web'
ID = 'a' * 32
SECRET = 'b' * 64
FOLDER = '分享%2F #资料'
FILE = '文档%2F #<img>.html'
CHILD = '子目录%2F #'


def expiry(seconds=86400):
    return (datetime.now(timezone.utc) + timedelta(seconds=seconds)).isoformat()


def share(kind='folder'):
    return {'id': ID, 'name': FOLDER if kind == 'folder' else FILE, 'kind': kind, 'path': '/' + FOLDER,
            'owner_id': 'reader', 'created_at': datetime.now(timezone.utc).isoformat(), 'expires_at': expiry(),
            'password_required': True, 'revoked': False}


async def owner_tests(browser):
    context = await browser.new_context(viewport={'width': 1280, 'height': 900})
    page = await context.new_page()
    errors, calls, records = [], [], []
    page.on('pageerror', lambda error: errors.append(str(error)))
    deny = False
    files = [{'id': 'folder', 'name': FOLDER, 'kind': 'folder'}, {'id': 'file', 'name': FILE, 'kind': 'file', 'size': 4}]

    async def route(request):
        req = request.request
        url = urlparse(req.url)
        path = url.path
        if path == '/':
            await request.fulfill(body=(WEB / 'index.html').read_bytes(), content_type='text/html')
            return
        if path.startswith('/assets/'):
            asset = WEB / path.removeprefix('/assets/')
            await request.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
            return
        data = {}
        if path.endswith('/auth/me'):
            data = {'authenticated': True, 'user': {'id': 'reader', 'username': 'reader', 'role': 'member', 'policy_version': 1, 'permissions': {'read': True, 'upload': False, 'delete': False}}}
        elif path.endswith('/status'):
            data = {'status': 'connected'}
        elif path.endswith('/transfers'):
            data = {'tasks': []}
        elif path.endswith('/tasks'):
            data = {'tasks': []}
        elif path.endswith('/entries'):
            data = {'entries': files}
        elif path.endswith('/shares'):
            if req.method == 'GET':
                data = {'shares': records}
            else:
                calls.append(req.post_data_json)
                if deny:
                    await request.fulfill(status=403, json={'error': 'permission changed'})
                    return
                created = share()
                created['expires_at'] = req.post_data_json['expires_at']
                records.append(created)
                data = {'share': created, 'url': '/share/' + ID + '#' + SECRET}
        elif path.endswith('/shares/' + ID) and req.method == 'DELETE':
            calls.append('revoke')
            records[0]['revoked'] = True
            await request.fulfill(status=204)
            return
        elif path == '/favicon.ico':
            await request.fulfill(status=204)
            return
        else:
            raise AssertionError('Unexpected owner request ' + path)
        await request.fulfill(json=data)

    await context.route('https://wps-offline.invalid/**', route)
    await page.goto('https://wps-offline.invalid/')
    await expect(page.locator('#skeleton')).to_be_hidden()
    await page.get_by_role('checkbox', name='选择：' + FOLDER, exact=True).check()
    await expect(page.locator('#selection-share')).to_be_enabled()
    await page.get_by_role('checkbox', name='选择：' + FILE, exact=True).check()
    await expect(page.locator('#selection-share')).to_be_disabled()
    await page.get_by_role('checkbox', name='选择：' + FILE, exact=True).uncheck()
    await page.locator('#selection-share').click()
    await expect(page.locator('#shares-days')).to_have_value('7')
    await expect(page.locator('#shares-source')).to_contain_text('/' + FOLDER)
    await page.locator('#shares-password').fill('offline-code')
    await page.locator('#shares-create').click()
    await expect(page.locator('#shares-created-url')).to_have_value('https://wps-offline.invalid/share/' + ID + '#' + SECRET)
    assert calls[0]['path'] == '/' + FOLDER and calls[0]['password'] == 'offline-code'
    duration = datetime.fromisoformat(calls[0]['expires_at'].replace('Z', '+00:00')) - datetime.now(timezone.utc)
    assert 6.99 < duration.total_seconds() / 86400 <= 7
    await expect(page.locator('#shares-password')).to_have_value('')
    await page.locator('#shares-copy').click()
    await expect(page.locator('#shares-copy-status')).not_to_have_text('')
    assert not await page.evaluate('(secret) => Object.values(localStorage).some(value => value.includes(secret))', SECRET)
    await page.set_viewport_size({'width': 390, 'height': 844})
    assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth')
    # Hide the synthetic bearer URL in screenshots, even though it is a fixture.
    await page.screenshot(path='/tmp/wps-shares-management-mobile.png', mask=[page.locator('#shares-created-url')], animations='disabled')
    await page.set_viewport_size({'width': 1280, 'height': 900})
    await page.locator('#shares-close').click()
    await expect(page.locator('#shares-created-url')).to_have_value('')
    await page.locator('#shares-button').click()
    await expect(page.locator('#shares-form')).to_be_hidden()
    await expect(page.locator('#shares-list')).to_contain_text(FOLDER)
    await expect(page.locator('#shares-created')).to_be_hidden()
    assert SECRET not in await page.locator('#shares-modal').text_content()
    await page.locator('#shares-list button', has_text='撤销分享').click()
    await page.locator('#modal-cancel').click()
    assert calls.count('revoke') == 0
    await page.locator('#shares-list button', has_text='撤销分享').click()
    await page.locator('#modal-submit').click()
    await expect(page.locator('#shares-list')).to_contain_text('已撤销')
    await page.locator('#shares-close').click()
    deny = True
    await page.locator('#selection-share').click()
    await page.locator('#shares-create').click()
    await expect(page.locator('#shares-error')).to_contain_text('当前账号不能分享')
    assert not errors, errors
    await context.close()


async def public_tests(browser):
    context = await browser.new_context(viewport={'width': 1280, 'height': 900}, accept_downloads=True)
    page = await context.new_page()
    errors, urls, actions, paths = [], [], [], []
    page.on('pageerror', lambda error: errors.append(str(error)))
    info = share()
    grant_expiry = expiry(3600)
    revoked = False
    hold_entries = False
    entries_gate = asyncio.Event()
    entries_started = asyncio.Event()
    html_source = '<script>window.compromised=true</script><img src=x onerror="window.compromised=true">'
    # Same origin controls as the production app; no external script or asset.
    source = (ROOT / 'go/internal/app/application.go').read_text()
    declaration = source.split('const webContentSecurityPolicy = ', 1)[1].split('\n\n', 1)[0]
    import re
    policy = ''.join(re.findall(r'"([^"]*)"', declaration))

    async def route(request):
        req = request.request
        url = urlparse(req.url)
        urls.append(req.url)
        if url.path == '/share/' + ID:
            await request.fulfill(body=(WEB / 'share.html').read_bytes(), content_type='text/html', headers={'Content-Security-Policy': policy, 'Referrer-Policy': 'no-referrer'})
            return
        if url.path in ('/assets/share-page.js', '/assets/share-page.css'):
            asset = WEB / url.path.removeprefix('/assets/')
            await request.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
            return
        if url.path == '/favicon.ico':
            await request.fulfill(status=204)
            return
        assert url.path.startswith('/api/share/' + ID + '/'), 'Public page reached non-share API: ' + url.path
        operation = url.path.rsplit('/', 1)[-1]
        query = parse_qs(url.query)
        target = query.get('path', ['/'])[0]
        paths.append(target)
        if operation == 'unlock':
            actions.append(req.post_data_json)
            body = req.post_data_json
            if revoked or body.get('token') != SECRET or (info['password_required'] and body.get('password') != 'offline-code'):
                await request.fulfill(status=401, json={'error': 'share unavailable'})
                return
            await request.fulfill(json={'share': info, 'path': '/', 'grant_expires_at': grant_expiry}, headers={'Set-Cookie': f'wps_share={ID}; Path=/api/share/{ID}/; Secure; HttpOnly; SameSite=Lax'})
            return
        if revoked or 'wps_share=' + ID not in req.headers.get('cookie', ''):
            await request.fulfill(status=401, json={'error': 'share unavailable'})
            return
        if operation == 'info':
            await request.fulfill(json={'share': info, 'path': '/', 'grant_expires_at': grant_expiry})
        elif operation == 'entries':
            if hold_entries:
                entries_started.set()
                await entries_gate.wait()
            rows = [{'name': CHILD, 'kind': 'folder', 'path': '/ignored'}, {'name': FILE, 'kind': 'file', 'size': 12},
                    {'name': 'photo.png', 'kind': 'file', 'size': len(PNG)}, {'name': 'doc.pdf', 'kind': 'file', 'size': len(pdf_fixture())}]
            await request.fulfill(json={'path': target, 'entries': rows if target == '/' else [{'name': '内部.txt', 'kind': 'file', 'size': 4}]})
        elif operation == 'preview':
            if target.endswith('.png'):
                await request.fulfill(body=PNG, content_type='image/png')
            elif target.endswith('.pdf'):
                await request.fulfill(body=pdf_fixture(), content_type='application/pdf')
            else:
                await request.fulfill(body=html_source.encode(), content_type='application/octet-stream')
        elif operation == 'download':
            await request.fulfill(body=b'data', content_type='application/octet-stream', headers={'Content-Disposition': 'attachment; filename="shared.txt"'})
        else:
            raise AssertionError('Unexpected public route ' + operation)

    await context.route('https://wps-offline.invalid/**', route)
    original = 'https://wps-offline.invalid/share/' + ID + '#' + SECRET
    await page.goto(original)
    await expect(page.locator('#share-unlock')).to_be_visible()
    await expect(page.locator('#share-content')).to_be_hidden()
    await page.locator('#share-password').fill('wrong')
    await page.locator('#share-unlock-submit').click()
    await expect(page.locator('#share-status')).to_contain_text('暂时无法访问')
    await page.locator('#share-password').fill('offline-code')
    await page.locator('#share-unlock-submit').click()
    await expect(page.locator('#share-list')).to_contain_text(FILE)
    assert not urlparse(page.url).fragment
    assert all(SECRET not in url for url in urls), urls
    assert await page.evaluate('Object.keys(localStorage).length === 0 && Object.keys(sessionStorage).length === 0')
    assert actions[-1] == {'token': SECRET, 'password': 'offline-code'}
    await page.get_by_role('button', name=CHILD, exact=True).click()
    await expect(page.locator('#share-list')).to_contain_text('内部.txt')
    assert paths[-1] == '/' + CHILD, paths
    await page.locator('#share-breadcrumbs button').first.click()
    await expect(page.locator('#share-list')).to_contain_text(FILE)
    row = page.locator('.share-row').filter(has=page.locator('.share-name', has_text=FILE))
    await row.get_by_role('button', name='在线预览').click()
    await expect(page.locator('#share-preview-content pre')).to_have_text(html_source)
    assert await page.evaluate('window.compromised') is None
    assert await page.locator('#share-preview-content img').count() == 0
    await page.locator('#share-preview-close').click()
    image_row = page.locator('.share-row').filter(has=page.locator('.share-name', has_text='photo.png'))
    await image_row.get_by_role('button', name='在线预览').click()
    await page.wait_for_function('() => document.querySelector("#share-preview-content img")?.naturalWidth > 0')
    await page.locator('#share-preview-close').click()
    pdf_row = page.locator('.share-row').filter(has=page.locator('.share-name', has_text='doc.pdf'))
    await pdf_row.get_by_role('button', name='在线预览').click()
    await expect(page.locator('#share-preview-content iframe')).to_have_attribute('title', 'doc.pdf')
    await page.locator('#share-preview-close').click()
    async with page.expect_download() as download:
        await row.get_by_role('button', name='下载', exact=True).click()
    assert (await download.value).suggested_filename == 'shared.txt'
    await page.reload()
    await expect(page.locator('#share-list')).to_contain_text(FILE)
    assert len(actions) == 3, 'Grant reload attempted another unlock'
    await page.set_viewport_size({'width': 390, 'height': 844})
    assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth')
    await page.screenshot(path='/tmp/wps-public-share-mobile.png', animations='disabled')
    await page.set_viewport_size({'width': 1280, 'height': 900})
    await page.screenshot(path='/tmp/wps-public-share-desktop.png', animations='disabled')
    # Revocation during an active preview clears every filename and source.
    await row.get_by_role('button', name='在线预览').click()
    await expect(page.locator('#share-preview')).to_be_visible()
    revoked = True
    await page.evaluate('document.getElementById("share-refresh").click()')
    await expect(page.locator('#share-content')).to_be_hidden()
    await expect(page.locator('#share-preview')).to_be_hidden()
    await expect(page.locator('#share-list')).to_be_empty()
    await expect(page.locator('#share-preview-content')).to_be_empty()
    # Single-file shares use only '/' and honor grant expiry without refresh.
    revoked = False
    info = share('file')
    info['password_required'] = False
    grant_expiry = expiry(3)
    await context.clear_cookies()
    await page.goto(original)
    await expect(page.locator('#share-list')).to_contain_text(FILE)
    async with page.expect_download() as download:
        await page.get_by_role('button', name='下载', exact=True).click()
    await download.value
    assert paths[-1] == '/', paths
    await expect(page.locator('#share-content')).to_be_hidden(timeout=6000)
    await expect(page.locator('#share-status')).to_contain_text('访问已到期')
    info = share('folder')
    info['password_required'] = False
    grant_expiry = expiry(1.5)
    hold_entries = True
    await page.goto(original)
    await asyncio.wait_for(entries_started.wait(), timeout=5)
    await expect(page.locator('#share-content')).to_be_hidden(timeout=5000)
    entries_gate.set()
    await expect(page.locator('#share-list')).to_be_empty()
    await expect(page.locator('#share-title')).to_have_text('文件分享')
    assert not errors, errors
    await context.close()


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        await owner_tests(browser)
        await public_tests(browser)
        await browser.close()
    print('PASS: scoped creation/expiry/code/copy/revoke, bearer cleanup, grant reload, safe public previews/paths/download, revocation/expiry cleanup and mobile')


if __name__ == '__main__':
    asyncio.run(main())
