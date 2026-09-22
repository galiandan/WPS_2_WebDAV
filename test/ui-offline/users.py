"""Administrator member management and scoped member UI capabilities."""
import asyncio
import copy
import os
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go' / 'web'
ROOT_PATH = '/团队%2F #空间/成员目录'
ADMIN = {'id': 'installation', 'username': 'admin', 'role': 'admin', 'enabled': True,
         'permissions': {'read': True, 'upload': True, 'delete': True}, 'policy_version': 1}


def member(username, upload=False, delete=False):
    return {'id': username, 'username': username, 'role': 'member', 'enabled': True,
            'permissions': {'read': True, 'upload': upload, 'delete': delete}, 'policy_version': 1}


def entry(name, kind='file', identifier=None):
    return {'id': identifier or name, 'name': name, 'kind': kind, 'size': 4}


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context()
        page = await context.new_page()
        errors, calls, mutations = [], [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        principal = copy.deepcopy(ADMIN)
        users = [copy.deepcopy(ADMIN)]
        expired = False
        deny_tasks = False
        deny_users = False
        files = [entry('目标目录', 'folder'), entry('existing.txt')]

        async def route(request):
            nonlocal principal, expired
            url = urlparse(request.request.url)
            path = url.path
            query = parse_qs(url.query)
            method = request.request.method
            if path == '/':
                await request.fulfill(body=(WEB / 'index.html').read_text(), content_type='text/html')
                return
            if path.startswith('/assets/'):
                asset = WEB / path.removeprefix('/assets/')
                await request.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
                return
            calls.append((principal['username'], method, path, query))
            if method not in ('GET', 'HEAD'):
                mutations.append((method, path, request.request.post_data_json if request.request.headers.get('content-type', '').startswith('application/json') else request.request.post_data))
            if path.endswith('/auth/me'):
                data = {'authenticated': True, 'user': principal}
            elif path.endswith('/auth/login'):
                principal = member(request.request.post_data_json['username'], upload=request.request.post_data_json['username'] == 'uploader')
                expired = False
                data = {'user': principal}
            elif path.endswith('/auth/security'):
                data = {'totp_enabled': False, 'passkeys': [{'id': principal['id'] + '-key', 'name': principal['username'] + ' key'}]}
            elif path.endswith('/auth/passkey/options'):
                await request.fulfill(status=401, json={'error': '该账号尚未配置 Passkey'})
                return
            elif path.endswith('/settings'):
                assert principal['role'] == 'admin', 'Member queried global settings'
                data = {'name': '管理员云盘'}
            elif path.endswith('/storage'):
                assert principal['role'] == 'admin', 'Member queried global storage'
                data = {'locations': [], 'current': None}
            elif path.endswith('/status'):
                data = {'status': 'connected'}
            elif path == '/healthz':
                assert principal['role'] == 'admin', 'Member queried version'
                data = {'version': 'offline'}
            elif path.endswith('/update'):
                assert principal['role'] == 'admin', 'Member queried updater'
                data = {'state': 'idle', 'update_available': False}
            elif path.endswith('/entries'):
                folder = query['path'][0]
                if principal['role'] == 'admin':
                    tree = {'/': [entry('团队%2F #空间', 'folder', 'space:team')],
                            '/团队%2F #空间': [entry('成员目录', 'folder'), entry('管理员私有', 'folder')], ROOT_PATH: []}
                    data = {'entries': tree.get(folder, [])}
                else:
                    assert not folder.startswith('/团队'), 'Member requested old admin path'
                    data = {'entries': files if folder == '/' else []}
            elif path.endswith('/users') or '/users/' in path:
                if principal['role'] != 'admin' or deny_users:
                    await request.fulfill(status=403, json={'error': 'administrator required'})
                    return
                identifier = path.split('/')[-1]
                if method == 'GET':
                    data = {'users': users}
                elif method == 'POST':
                    body = request.request.post_data_json
                    if any(user['username'] == body['username'] for user in users):
                        await request.fulfill(status=409, json={'error': '用户名已经存在'})
                        return
                    created = {**member(body['username']), 'root_path': body['root_path'], 'permissions': body['permissions']}
                    users.append(created)
                    data = {'user': created}
                elif method == 'PATCH':
                    user = next(user for user in users if user['id'] == identifier)
                    assert identifier != 'installation'
                    user.update(request.request.post_data_json)
                    user.pop('password', None)
                    data = {'user': user}
                else:
                    users[:] = [user for user in users if user['id'] != identifier]
                    await request.fulfill(status=204)
                    return
            elif path.endswith('/transfers'):
                data = {'tasks': []}
            elif path.endswith('/tasks'):
                if expired:
                    await request.fulfill(status=401, json={'error': '登录过期'})
                    return
                if method == 'POST':
                    if deny_tasks:
                        await request.fulfill(status=403, json={'error': '权限已撤销'})
                        return
                    body = request.request.post_data_json
                    assert principal['permissions']['upload'] and body['operation'] == 'copy'
                    data = {'task': {'id': 'copy-task', 'operation': 'copy', 'destination': body['destination'], 'state': 'queued',
                                     'created_at': '2026-09-22T00:00:00Z', 'items': [{'path': path, 'state': 'queued'} for path in body['paths']],
                                     'total': len(body['paths']), 'completed': 0, 'succeeded': 0, 'failed': 0, 'retryable_count': 0}}
                else:
                    data = {'tasks': []}
            elif path.endswith('/search'):
                data = {'entries': [], 'total': 0, 'index': {'state': 'ready', 'entries': 0}}
            elif path.endswith('/preview'):
                await request.fulfill(body=b'text', content_type='application/octet-stream')
                return
            elif path.endswith('/upload'):
                assert principal['permissions']['upload']
                assert 'overwrite' not in query
                files.append(entry(query['path'][0].rsplit('/', 1)[-1]))
                data = {}
            elif path.endswith('/folders'):
                assert principal['permissions']['upload']
                data = {}
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request ' + path)
            await request.fulfill(json=data)

        await context.route('https://wps-offline.invalid/**', route)
        await page.goto('https://wps-offline.invalid/')
        await page.locator('#users-button').click()
        await expect(page.locator('[data-user-id="installation"]')).to_contain_text('安装管理员')
        assert await page.locator('[data-user-id="installation"] button').count() == 0
        await page.locator('#users-create').click()
        await page.locator('#users-username').fill('viewer')
        await page.locator('#users-password').fill('offline-password')
        await page.locator('#users-choose-root').click()
        await expect(page.locator('#users-root-select')).to_be_disabled()
        await page.locator('#users-root-list button', has_text='团队%2F #空间').click()
        await page.locator('#users-root-list button', has_text='成员目录').click()
        await page.locator('#users-root-select').click()
        await expect(page.locator('#users-root')).to_have_value(ROOT_PATH)
        await page.locator('#users-save').click()
        await expect(page.locator('[data-user-id="viewer"]')).to_contain_text(ROOT_PATH)
        assert mutations[-1][2] == {'username': 'viewer', 'password': 'offline-password', 'root_path': ROOT_PATH, 'permissions': {'read': True, 'upload': False, 'delete': False}}
        viewer = page.locator('[data-user-id="viewer"]')
        await viewer.get_by_role('button', name='编辑', exact=True).click()
        await page.locator('#users-upload').check()
        await page.locator('#users-save').click()
        await expect(viewer).to_contain_text('读取 · 上传')
        assert 'password' not in mutations[-1][2]
        await viewer.get_by_role('button', name='停用', exact=True).click()
        await page.locator('#modal-submit').click()
        await expect(viewer).to_contain_text('已停用')
        await viewer.get_by_role('button', name='启用', exact=True).click()
        await expect(viewer).to_contain_text('已启用')
        await page.set_viewport_size({'width': 390, 'height': 844})
        assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        await page.screenshot(path='/tmp/wps-users-mobile.png', animations='disabled')
        await page.set_viewport_size({'width': 1280, 'height': 900})
        await page.screenshot(path='/tmp/wps-users-desktop.png', animations='disabled')
        await viewer.get_by_role('button', name='删除账号', exact=True).click()
        await page.locator('#modal-cancel').click()
        await expect(viewer).to_have_count(1)
        await viewer.get_by_role('button', name='删除账号', exact=True).click()
        await page.locator('#modal-submit').click()
        await expect(viewer).to_have_count(0)
        deny_users = True
        await page.locator('#users-refresh').click()
        await expect(page.locator('#users-error')).to_contain_text('需要管理员权限')
        deny_users = False
        await page.locator('#users-close').click()
        assert not any('/storage' in path for method, path, body in mutations), mutations
        # Expired admin session followed by another account must clear all old
        # paths and caches without relying on a whole-page reload.
        await page.locator('#space-list > .directory-node > .directory-row > .tree-link').click()
        await expect(page.locator('#entries')).to_contain_text('管理员私有')
        expired = True
        await page.locator('#tasks-button').click()
        await expect(page.locator('#auth-screen')).to_be_visible()
        await expect(page.locator('#users-list')).to_be_empty()
        await expect(page.locator('#users-root-list')).to_be_empty()
        await page.locator('#login-username').fill('reader')
        await page.locator('#login-password').fill('offline-password')
        await page.locator('#login-submit').click()
        await expect(page.locator('#auth-screen')).to_be_hidden()
        await expect(page.locator('#entries')).to_contain_text('existing.txt')
        await expect(page.locator('#entries')).not_to_contain_text('管理员私有')
        await expect(page.locator('#space-root')).to_have_text('我的文件')
        for identifier in ['users-button', 'version-button']:
            await expect(page.locator('#' + identifier)).to_be_hidden()
        for identifier in ['upload-button', 'upload-folder-button', 'folder-button', 'text-file-button', 'offline-download-button']:
            await expect(page.locator('#' + identifier)).to_be_disabled()
        before = len(mutations)
        await page.evaluate('document.getElementById("folder-button").disabled=false; document.getElementById("folder-button").click(); document.getElementById("users-button").click();')
        await expect(page.locator('#toasts')).to_contain_text('没有权限')
        assert len(mutations) == before
        await page.get_by_role('checkbox', name='选择：existing.txt', exact=True).check()
        for identifier in ['batch-copy', 'batch-move', 'batch-delete']:
            await expect(page.locator('#' + identifier)).to_be_disabled()
        await page.get_by_role('button', name='选择文件：existing.txt', exact=True).dblclick()
        await expect(page.locator('#preview-content')).to_have_text('text')
        await expect(page.locator('#text-editor-open')).to_be_hidden()
        await page.locator('#preview-close').click()
        await page.locator('#settings-button').click()
        await expect(page.locator('[data-admin-only]').first).to_be_hidden()
        await expect(page.locator('#passkey-list')).to_contain_text('reader key')
        await page.locator('#settings-cancel').click()
        await page.locator('#global-search-button').click()
        await expect(page.locator('#global-search-scope option')).to_have_text(['我的文件'])
        await page.locator('#global-search-close').click()
        # Upload-only members can create files and copy but cannot overwrite.
        principal = member('uploader', upload=True)
        await page.reload()
        await expect(page.locator('#upload-button')).to_be_enabled()
        await page.locator('#file-input').set_input_files({'name': 'existing.txt', 'mimeType': 'text/plain', 'buffer': b'test'})
        await expect(page.locator('#tray-list .tray-item')).to_have_class('tray-item skipped')
        await expect(page.locator('#tray-list')).to_contain_text('没有覆盖')
        await expect(page.locator('#modal')).to_be_hidden()
        assert not any(path.endswith('/upload') for method, path, body in mutations)
        await page.locator('#file-input').set_input_files({'name': 'new.txt', 'mimeType': 'text/plain', 'buffer': b'test'})
        await expect(page.locator('#tray-list .tray-item')).to_have_class('tray-item done')
        await page.get_by_role('checkbox', name='选择：existing.txt', exact=True).check()
        await expect(page.locator('#batch-copy')).to_be_enabled()
        await expect(page.locator('#batch-move')).to_be_disabled()
        await expect(page.locator('#batch-delete')).to_be_disabled()
        deny_tasks = True
        await page.locator('#batch-copy').click()
        await page.locator('#picker-list .picker-item', has_text='目标目录').click()
        await page.locator('#picker-move').click()
        await expect(page.locator('#toasts')).to_contain_text('没有权限')
        deny_tasks = False
        await page.locator('#batch-copy').click()
        await page.locator('#picker-list .picker-item', has_text='目标目录').click()
        await page.locator('#picker-move').click()
        await expect(page.locator('#tasks-modal')).to_be_visible()
        await page.locator('#tasks-close').click()
        assert not any(actor != 'admin' and (path.endswith('/settings') or path.endswith('/storage') or path.endswith('/update') or path.endswith('/users')) for actor, method, path, query in calls)
        # Username-specific Passkey discovery uses the visible Passkey field.
        expired = True
        await page.locator('#tasks-button').click()
        await expect(page.locator('#auth-screen')).to_be_visible()
        await page.locator('#auth-method-passkey').click()
        await page.locator('#passkey-username').fill('passkey-member')
        await page.locator('#passkey-login-button').click()
        await expect(page.locator('#auth-message')).to_have_text('该账号尚未配置 Passkey')
        assert mutations[-1][1:] == ('/api/v1/auth/passkey/options', {'username': 'passkey-member'})
        assert not errors, errors
        print('PASS: admin CRUD/immutable admin/root picker, disable/delete confirmation, 403 handling, member scope/cache reset, personal security, read/upload-only controls and overwrite refusal')
        await browser.close()


if __name__ == '__main__':
    asyncio.run(main())
