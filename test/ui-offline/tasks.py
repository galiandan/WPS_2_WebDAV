"""Durable task UI lifecycle, partial results, retries and account cleanup."""
import asyncio
import copy
import os
import re
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go' / 'web'
SPACE = '/测试%2F #空间'
SPECIAL = '报告%2F #进度.txt'


def file(name, kind='file'):
    return {'id': name, 'name': name, 'kind': kind, 'size': 4}


def task(identifier, operation='copy', paths=None, state='running'):
    paths = paths or [SPACE + '/a.txt', SPACE + '/' + SPECIAL]
    return {'id': identifier, 'operation': operation, 'destination': SPACE + '/目标',
            'state': state, 'created_at': '2026-09-21T01:02:03Z', 'updated_at': '2026-09-21T01:02:03Z',
            'items': [{'path': path, 'state': 'running' if i == 0 else 'queued', 'retryable': False}
                      for i, path in enumerate(paths)],
            'cancel_requested': False, 'total': len(paths), 'completed': 0,
            'succeeded': 0, 'failed': 0, 'retryable_count': 0}


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context()
        page = await context.new_page()
        errors, mutations = [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        tasks = []
        reads = 0
        fail_read = False
        unauthorized = False
        stale_read = False
        stale_started = asyncio.Event()
        stale_gate = asyncio.Event()
        upload_gate = asyncio.Event()
        files = [file('目标', 'folder'), file('a.txt'), file(SPECIAL)]

        async def route(request):
            nonlocal reads, stale_read
            url = urlparse(request.request.url)
            path = url.path
            query = parse_qs(url.query)
            data = {}
            if path == '/':
                await request.fulfill(body=(WEB / 'index.html').read_text(), content_type='text/html')
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
                folder = query['path'][0]
                data = {'entries': [file(SPACE[1:], 'folder')] if folder == '/' else list(files) if folder == SPACE else []}
                if folder == '/':
                    data['entries'][0]['id'] = 'space:test'
            elif path.endswith('/tasks') and request.request.method == 'GET':
                reads += 1
                if unauthorized:
                    await request.fulfill(status=401, json={'error': '登录已过期'})
                    return
                if fail_read:
                    await request.fulfill(status=503, json={'error': '任务服务暂时不可用'})
                    return
                snapshot = copy.deepcopy(tasks)
                if stale_read:
                    stale_read = False
                    stale_started.set()
                    await stale_gate.wait()
                data = {'tasks': snapshot}
            elif path.endswith('/tasks') and request.request.method == 'POST':
                body = request.request.post_data_json
                mutations.append(('create', body))
                queued = task('new-' + str(len(mutations)), body['operation'], body['paths'])
                queued['destination'] = body.get('destination', '')
                tasks.insert(0, queued)
                data = {'task': copy.deepcopy(queued)}
            elif path.endswith('/cancel'):
                identifier = path.split('/')[-2]
                mutations.append(('cancel', identifier))
                original = next(item for item in tasks if item['id'] == identifier)
                original['cancel_requested'] = True
                data = {'task': copy.deepcopy(original)}
            elif path.endswith('/retry'):
                identifier = path.split('/')[-2]
                mutations.append(('retry', identifier))
                original = next(item for item in tasks if item['id'] == identifier)
                retry_paths = [item['path'] for item in original['items'] if item.get('retryable')]
                queued = task('retry-' + str(len(mutations)), original['operation'], retry_paths)
                original['retryable_count'] = 0
                for item in original['items']:
                    item['retryable'] = False
                tasks.insert(0, queued)
                data = {'task': queued}
            elif path.endswith('/upload'):
                await upload_gate.wait()
                data = {}
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
        await page.get_by_role('checkbox', name='选择：a.txt', exact=True).check()
        await page.get_by_role('checkbox', name='选择：' + SPECIAL, exact=True).check()
        await page.locator('#batch-copy').click()
        await page.locator('#picker-list .picker-item', has_text='目标').click()
        await page.locator('#picker-move').click()
        await expect(page.locator('#tasks-modal')).to_be_visible()
        await expect(page.locator('#tasks-count')).to_have_text('1')
        await expect(page.locator('.task-summary')).to_have_text('已处理 0 / 2 项 · 成功 0 项 · 失败 0 项')
        await expect(page.locator('#tasks-modal')).to_contain_text('关闭页面仍会继续')
        assert set(mutations[0][1]['paths']) == {SPACE + '/a.txt', SPACE + '/' + SPECIAL}
        await page.locator('#tasks-close').click()
        await expect(page.locator('#upload-button')).to_be_enabled()
        # Refresh/reload loads the same running server record, no new POST.
        await page.reload()
        await expect(page.locator('#auth-screen')).to_be_hidden()
        await page.locator('#tasks-button').click()
        await expect(page.locator('.task-item')).to_have_count(1)
        assert len(mutations) == 1
        await page.locator('.task-details summary').click()
        await expect(page.locator('.task-item-results')).to_contain_text(SPECIAL)
        current = tasks[0]
        current['items'][0]['state'] = 'succeeded'
        current.update(completed=1, succeeded=1)
        await page.locator('#tasks-refresh').click()
        await expect(page.locator('.task-summary')).to_have_text('已处理 1 / 2 项 · 成功 1 项 · 失败 0 项')
        assert await page.locator('.task-details').evaluate('(node) => node.open')
        current['items'][1].update(state='failed', error='同名项目已存在', retryable=True)
        current.update(state='failed', completed=2, failed=1, retryable_count=1)
        await page.locator('#tasks-refresh').click()
        await expect(page.locator('.task-item-results')).to_contain_text('同名项目已存在')
        await expect(page.locator('[data-task-action="retry"]')).to_have_text('重试 1 项')
        await expect(page.locator('#tasks-count')).to_be_hidden()
        # Explicit confirmation creates only the eligible failed item.
        await page.locator('[data-task-action="retry"]').click()
        await page.locator('#modal-cancel').click()
        assert len(mutations) == 1
        await page.locator('[data-task-action="retry"]').click()
        await page.locator('#modal-submit').click()
        await expect(page.locator('.task-item')).to_have_count(2)
        retry = tasks[0]
        assert retry['items'][0]['path'] == current['items'][1]['path']
        assert retry['total'] == 1
        # An older GET cannot undo a newer cancellation response.
        stale_read = True
        await page.locator('#tasks-refresh').click()
        await asyncio.wait_for(stale_started.wait(), timeout=5)
        await page.locator('[data-task-action="cancel"]').click()
        await expect(page.locator('[data-task-action="cancel"]')).to_have_text('正在取消…')
        stale_gate.set()
        await expect(page.locator('[data-task-action="cancel"]')).to_be_disabled()
        retry.update(state='cancelled', completed=1, retryable_count=1)
        retry['items'][0].update(state='cancelled', retryable=True)
        await expect(page.locator('#tasks-refresh')).to_be_enabled()
        await page.locator('#tasks-refresh').click()
        await expect(page.locator('.task-item').first).to_have_attribute('data-state', 'cancelled')
        # Restarted running work is uncertain; only unstarted items retry.
        restarted = task('restart', state='interrupted')
        restarted['items'][0].update(state='interrupted', retryable=False, error='结果未确认')
        restarted['items'][1].update(state='interrupted', retryable=True)
        restarted.update(completed=2, retryable_count=1)
        tasks.insert(0, restarted)
        await page.locator('#tasks-refresh').click()
        restored = page.locator('[data-task-id="restart"]')
        await restored.locator('summary').click()
        await expect(restored).to_contain_text('结果未确认，需手动检查')
        await expect(restored.locator('[data-task-action="retry"]')).to_have_text('重试 1 项')
        await page.set_viewport_size({'width': 390, 'height': 844})
        assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        await page.screenshot(path='/tmp/wps-tasks-mobile.png', animations='disabled')
        await expect(page.locator('#tasks-close')).to_be_in_viewport()
        await page.set_viewport_size({'width': 1280, 'height': 900})
        await page.screenshot(path='/tmp/wps-tasks-desktop.png', animations='disabled')
        # Transient failures keep history and offer a bounded manual retry.
        fail_read = True
        await page.locator('#tasks-refresh').click()
        await expect(page.locator('#tasks-error')).to_have_text('任务服务暂时不可用')
        await expect(page.locator('.task-item')).to_have_count(3)
        fail_read = False
        await page.locator('#tasks-refresh').click()
        await expect(page.locator('#tasks-error')).to_have_text('')
        # Browser upload lifecycle is shown separately and can return to tray.
        await page.locator('#tasks-close').click()
        await page.locator('#file-input').set_input_files({'name': 'upload.txt', 'mimeType': 'text/plain', 'buffer': b'data'})
        await expect(page.locator('#tray-list .tray-item')).to_have_class(re.compile(r'tray-item (active|confirming)'))
        await page.locator('#tasks-button').click()
        await expect(page.locator('#tasks-upload-summary')).to_have_text('正在上传 · 已完成 0 / 1 项')
        await expect(page.locator('#tasks-uploads')).to_contain_text('关闭或刷新此页面会中断')
        await page.locator('#tasks-show-uploads').click()
        await expect(page.locator('#upload-tray')).to_be_visible()
        upload_gate.set()
        await expect(page.locator('#upload-button')).to_be_enabled()
        # An expired local session closes and clears sensitive task history.
        await page.locator('#tasks-button').click()
        await page.locator('[data-task-id="restart"] [data-task-action="retry"]').click()
        await expect(page.locator('#modal')).to_be_visible()
        unauthorized = True
        # Simulate the next scheduled poll while confirmation is on top.
        await page.evaluate('document.getElementById("tasks-refresh").click()')
        await expect(page.locator('#auth-screen')).to_be_visible()
        await expect(page.locator('#tasks-modal')).to_be_hidden()
        await expect(page.locator('#modal')).to_be_hidden()
        await expect(page.locator('#tasks-list')).to_be_empty()
        reads_after_auth = reads
        await page.wait_for_timeout(2200)
        assert reads == reads_after_auth, (reads, reads_after_auth)
        assert not errors, errors
        print('PASS: task submission/reload, progress, failed-item retry, stale reads, cancellation, interrupted results, upload separation, mobile and auth cleanup')
        await browser.close()


if __name__ == '__main__':
    asyncio.run(main())
