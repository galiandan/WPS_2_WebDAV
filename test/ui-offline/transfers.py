"""Offline fetch/archive UI, mixed task ownership, stages and artifact lifecycle."""
import asyncio
import copy
import io
import os
import zipfile
from datetime import datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go' / 'web'
NAME = '报告%2F #.zip'
SOURCE = 'https://downloads.invalid/report%20file.zip?signature=never-show-this'


def transfer(identifier, kind='fetch'):
    return {'id': identifier, 'kind': kind, 'state': 'running', 'stage': 'downloading' if kind == 'fetch' else 'packaging',
            'name': NAME if kind == 'fetch' else 'archive.zip', 'destination': '/' + NAME if kind == 'fetch' else '',
            'bytes_done': 512, 'bytes_total': 1024, 'files_done': 0, 'files_total': 0,
            'can_cancel': True, 'can_retry': False, 'artifact_ready': False,
            'created_at': '2026-09-22T00:00:01Z', 'updated_at': '2026-09-22T00:00:01Z'}


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context(viewport={'width': 1280, 'height': 900}, accept_downloads=True)
        page = await context.new_page()
        errors, writes, downloads = [], [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        jobs = []
        batch = {'id': 'collision', 'operation': 'copy', 'state': 'running', 'created_at': '2026-09-22T00:00:00Z',
                 'total': 1, 'completed': 0, 'succeeded': 0, 'failed': 0, 'items': [{'path': '/source.txt', 'state': 'running'}]}
        batch_error = False
        unauthorized = False
        grants_upload = True
        fail_submit = False
        artifact_expired = False
        done_file = False
        stale_started, stale_gate = asyncio.Event(), asyncio.Event()
        stale = False
        packed = io.BytesIO()
        with zipfile.ZipFile(packed, 'w') as archive:
            archive.writestr('fixture.txt', 'offline')

        async def route(request):
            nonlocal stale
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
                data = {'authenticated': True, 'user': {'id': 'member', 'username': 'member', 'role': 'member', 'policy_version': 1,
                                                      'permissions': {'read': True, 'upload': grants_upload, 'delete': False}}}
            elif path.endswith('/status'):
                data = {'status': 'connected'}
            elif path.endswith('/entries'):
                data = {'entries': [{'id': 'source', 'name': 'source.txt', 'kind': 'file', 'size': 4}] + ([{'id': 'downloaded', 'name': NAME, 'kind': 'file', 'size': 1024}] if done_file else [])}
            elif path.endswith('/tasks'):
                if batch_error:
                    await request.fulfill(status=503, json={'error': '批量任务暂时不可用'})
                    return
                data = {'tasks': [batch]}
            elif path.endswith('/transfers') and req.method == 'GET':
                if unauthorized:
                    await request.fulfill(status=401, json={'error': '登录已过期'})
                    return
                snapshot = copy.deepcopy(jobs)
                if stale:
                    stale = False
                    stale_started.set()
                    await stale_gate.wait()
                data = {'tasks': snapshot, 'persistence_error': False}
            elif path.endswith('/transfers') and req.method == 'POST':
                body = req.post_data_json
                writes.append(('create', body))
                if fail_submit:
                    await request.fulfill(status=409, json={'error': 'destination already exists'})
                    return
                job = transfer('collision' if not jobs else 'job-' + str(len(jobs)), body['kind'])
                if body['kind'] == 'fetch':
                    assert grants_upload
                    job['destination'] = body['destination']
                    assert body['url'] == SOURCE
                else:
                    assert body['paths'] == ['/source.txt']
                    job.update(files_total=1, files_done=0)
                jobs.insert(0, job)
                data = {'task': copy.deepcopy(job)}
            elif '/transfers/' in path:
                parts = path.split('/')
                identifier = parts[4]
                job = next(job for job in jobs if job['id'] == identifier)
                if path.endswith('/download'):
                    downloads.append(identifier)
                    await request.fulfill(body=packed.getvalue(), content_type='application/zip', headers={'Content-Disposition': 'attachment; filename="archive.zip"'})
                    return
                if path.endswith('/cancel'):
                    writes.append(('cancel', identifier))
                    job.update(cancel_requested=True, can_cancel=False)
                elif path.endswith('/retry'):
                    writes.append(('retry', identifier))
                    job['can_retry'] = False
                    replacement = transfer('retry-' + identifier, job['kind'])
                    jobs.insert(0, replacement)
                    job = replacement
                if artifact_expired and job['kind'] == 'archive':
                    job.update(artifact_ready=False, artifact_expires_at='2020-01-01T00:00:00Z')
                data = {'task': copy.deepcopy(job)}
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('https://wps-offline.invalid/**', route)
        await page.goto('https://wps-offline.invalid/')
        await expect(page.locator('#offline-download-button')).to_be_enabled()
        await page.locator('#offline-download-button').click()
        await page.locator('#transfer-url').fill(SOURCE)
        await page.locator('#transfer-name').click()
        await expect(page.locator('#transfer-name')).to_have_value('report file.zip')
        await page.locator('#transfer-name').fill(NAME)
        await page.locator('#transfer-submit').click()
        await expect(page.locator('#tasks-modal')).to_be_visible()
        await expect(page.locator('#transfer-url')).to_have_value('')
        await expect(page.locator('.task-item')).to_have_count(2)
        await expect(page.locator('#tasks-count')).to_have_text('2')
        batch_row = page.locator('[data-task-service="batch"][data-task-id="collision"]')
        fetch_row = page.locator('[data-task-service="transfer"][data-task-id="collision"]')
        await expect(batch_row).to_contain_text('复制')
        await expect(fetch_row).to_contain_text('正在下载到服务器')
        await expect(fetch_row.locator('progress')).to_have_attribute('value', '512')
        assert writes[0][1] == {'kind': 'fetch', 'url': SOURCE, 'destination': '/' + NAME}
        assert 'never-show-this' not in await page.locator('body').text_content()
        assert not await page.evaluate('(secret) => Object.values(localStorage).some(value => value.includes(secret))', 'never-show-this')
        await page.locator('#tasks-close').click()
        await page.reload()
        await expect(page.locator('#auth-screen')).to_be_hidden()
        await page.locator('#tasks-button').click()
        await expect(fetch_row).to_be_visible()
        assert len(writes) == 1
        # Cancel routes by task service even when IDs collide with batch tasks.
        stale = True
        await page.locator('#tasks-refresh').click()
        await asyncio.wait_for(stale_started.wait(), timeout=5)
        await fetch_row.locator('[data-task-action="cancel"]').click()
        stale_gate.set()
        await expect(fetch_row).to_contain_text('正在取消')
        assert writes[-1] == ('cancel', 'collision')
        jobs[0].update(state='cancelled', stage='cancelled', can_retry=True, cancel_requested=False)
        await expect(page.locator('#tasks-refresh')).to_be_enabled()
        await page.locator('#tasks-refresh').click()
        await expect(fetch_row.locator('[data-task-action="retry"]')).to_be_enabled()
        await fetch_row.locator('[data-task-action="retry"]').click()
        await page.locator('#modal-cancel').click()
        assert len(writes) == 2
        await fetch_row.locator('[data-task-action="retry"]').click()
        await page.locator('#modal-submit').click()
        retried = page.locator('[data-task-service="transfer"][data-task-id="retry-collision"]')
        await expect(retried).to_be_visible()
        current = jobs[0]
        current.update(stage='uploading', bytes_done=1024, can_cancel=False)
        await page.locator('#tasks-refresh').click()
        await expect(retried).to_contain_text('已进入上传登记，需等待结果')
        await expect(retried.locator('[data-task-action="cancel"]')).to_have_count(0)
        current.update(state='interrupted', stage='interrupted', can_retry=False, error='upload result unknown')
        await page.locator('#tasks-refresh').click()
        await expect(retried).to_contain_text('远端结果尚未确认')
        await expect(retried.locator('[data-task-action="retry"]')).to_have_count(0)
        # Another task backend can fail while transfer progress keeps working.
        completed = transfer('successful-fetch')
        jobs.insert(0, completed)
        await page.locator('#tasks-refresh').click()
        completed_row = page.locator('[data-task-service="transfer"][data-task-id="successful-fetch"]')
        await expect(completed_row).to_have_attribute('data-state', 'running')
        batch_error = True
        completed.update(state='completed', stage='ready', bytes_done=1024, can_cancel=False)
        done_file = True
        await page.locator('#tasks-refresh').click()
        await expect(completed_row).to_contain_text('上传已完成')
        await expect(page.locator('#tasks-error')).to_contain_text('批量任务暂时不可用')
        await expect(batch_row).to_be_visible()
        batch_error = False
        await page.locator('#tasks-close').click()
        await expect(page.locator('#entries')).to_contain_text(NAME)
        fail_submit = True
        await page.locator('#offline-download-button').click()
        await page.locator('#transfer-url').fill(SOURCE)
        await page.locator('#transfer-name').fill(NAME)
        await page.locator('#transfer-submit').click()
        await expect(page.locator('#transfer-error')).to_contain_text('同名文件已存在')
        await expect(page.locator('#transfer-url')).to_have_value(SOURCE)
        await page.locator('#transfer-cancel').click()
        await expect(page.locator('#transfer-url')).to_have_value('')
        fail_submit = False
        # A reader can prepare a ZIP but cannot submit a URL upload.
        grants_upload = False
        await page.reload()
        await expect(page.locator('#offline-download-button')).to_be_disabled()
        await page.get_by_role('checkbox', name='选择：source.txt', exact=True).check()
        await expect(page.locator('#background-archive')).to_be_enabled()
        await page.locator('#background-archive').click()
        await expect(page.locator('#tasks-modal')).to_be_visible()
        archive_job = jobs[0]
        archive_row = page.locator('[data-task-service="transfer"][data-task-id="' + archive_job['id'] + '"]')
        await expect(archive_row).to_contain_text('正在生成 ZIP')
        archive_job.update(state='completed', stage='ready', files_done=1, bytes_done=1024, can_cancel=False,
                           artifact_ready=True, artifact_size=len(packed.getvalue()), artifact_expires_at=(datetime.now(timezone.utc) + timedelta(hours=1)).isoformat())
        await page.locator('#tasks-refresh').click()
        await expect(archive_row.locator('[data-task-action="download"]')).to_be_enabled()
        async with page.expect_download() as download:
            await archive_row.locator('[data-task-action="download"]').click()
        assert (await download.value).suggested_filename == 'archive.zip'
        assert downloads == [archive_job['id']]
        await page.set_viewport_size({'width': 390, 'height': 844})
        assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        await page.screenshot(path='/tmp/wps-transfers-mobile.png', animations='disabled')
        await page.set_viewport_size({'width': 1280, 'height': 900})
        await page.screenshot(path='/tmp/wps-transfers-desktop.png', animations='disabled')
        artifact_expired = True
        await archive_row.locator('[data-task-action="download"]').click()
        await expect(archive_row).to_contain_text('打包文件已过期')
        await expect(page.locator('#tasks-error')).to_contain_text('打包文件已过期')
        assert len(downloads) == 1
        unauthorized = True
        await page.locator('#tasks-refresh').click()
        await expect(page.locator('#auth-screen')).to_be_visible()
        await expect(page.locator('#tasks-list')).to_be_empty()
        await expect(page.locator('#transfer-url')).to_have_value('')
        assert not errors, errors
        print('PASS: URL task submission/path/query privacy, mixed task IDs/backends, reload/progress/cancel/retry, upload uncertainty, readonly ZIP artifacts/expiry, auth cleanup and mobile')
        await browser.close()


if __name__ == '__main__':
    asyncio.run(main())
