"""Offline browser regressions for multiselect, batch results and directory uploads."""
import asyncio
import io
import json
import os
import tempfile
import zipfile
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go' / 'web'
SPACE = '/测试%2F #空间'
SPECIAL = '报告%2F #进度.txt'


def entry(name, kind='file'):
    return {'id': name, 'name': name, 'kind': kind, 'size': 4}


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context(accept_downloads=True)
        page = await context.new_page()
        errors, batches, writes, archives = [], [], [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        tree = {'/': [{'id': 'space:test', 'name': SPACE[1:], 'kind': 'folder'}],
                SPACE: [entry('目标', 'folder'), entry('资料', 'folder'), entry('a.txt'), entry('b.txt'), entry(SPECIAL)],
                SPACE + '/目标': [], SPACE + '/资料': []}
        folder_gate = asyncio.Event()
        failing_folder = None
        archive_error = False
        tasks = []
        zip_bytes = io.BytesIO()
        with zipfile.ZipFile(zip_bytes, 'w') as z:
            z.writestr('fixture.txt', 'offline')

        async def route(request):
            nonlocal failing_folder
            url = urlparse(request.request.url)
            path = url.path
            query = parse_qs(url.query)
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
            elif path.endswith('/status'):
                data = {'status': 'connected'}
            elif path == '/healthz':
                data = {'version': '1.0.21'}
            elif path.endswith('/update'):
                data = {'state': 'idle', 'current_version': '1.0.21', 'update_available': False}
            elif path.endswith('/entries'):
                data = {'entries': list(tree.get(query['path'][0], []))}
            elif path.endswith('/tasks') and request.request.method == 'GET':
                data = {'tasks': tasks}
            elif path.endswith('/tasks'):
                body = request.request.post_data_json
                batches.append(body)
                results = []
                for source in body['paths']:
                    name = source.rsplit('/', 1)[-1]
                    ok = not (body['operation'] == 'copy' and name == 'b.txt')
                    results.append({'path': source, 'ok': ok, **({} if ok else {'error': '同名项目已存在', 'status': 409})})
                    if ok and body['operation'] != 'copy':
                        parent = source.rsplit('/', 1)[0]
                        tree[parent] = [item for item in tree[parent] if item['name'] != name]
                task = {'id': str(len(batches)), 'operation': body['operation'], 'destination': body.get('destination', ''),
                        'state': 'failed' if any(not item['ok'] for item in results) else 'completed',
                        'created_at': '2026-09-21T01:02:03Z', 'total': len(results), 'completed': len(results),
                        'succeeded': sum(item['ok'] for item in results), 'failed': sum(not item['ok'] for item in results),
                        'retryable_count': 0,
                        'items': [{**item, 'state': 'succeeded' if item['ok'] else 'failed'} for item in results]}
                tasks.insert(0, task)
                data = {'task': task}
            elif path.endswith('/archive'):
                archives.append(json.loads(parse_qs(request.request.post_data)['paths'][0]))
                if archive_error:
                    await request.fulfill(status=409, json={'error': 'ZIP 项目超过限制'})
                    return
                await request.fulfill(body=zip_bytes.getvalue(), content_type='application/zip', headers={'Content-Disposition': 'attachment; filename="files.zip"'})
                return
            elif path.endswith('/folders'):
                target = query['path'][0]
                writes.append(('folder', target))
                if target.rsplit('/', 1)[-1] in ('取消目录', '取消单项目'):
                    await folder_gate.wait()
                if target == failing_folder:
                    await request.fulfill(status=503, json={'error': '测试文件夹创建失败'})
                    return
                if target in tree:
                    await request.fulfill(status=409, json={'error': '同名项目已存在'})
                    return
                parent, name = target.rsplit('/', 1)
                tree[target] = []
                tree.setdefault(parent, []).append(entry(name, 'folder'))
            elif path.endswith('/upload'):
                target = query['path'][0]
                writes.append(('file', target))
                parent, name = target.rsplit('/', 1)
                if parent not in tree:
                    raise AssertionError('Upload before parent creation: ' + target)
                tree[parent] = [item for item in tree[parent] if item['name'] != name] + [entry(name)]
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        await expect(page.locator('#select-all')).to_be_disabled()
        assert await page.locator('.entry-select').count() == 0
        await page.locator('#space-list > .directory-node > .directory-row > .tree-link').click()
        await expect(page.locator('#skeleton')).to_be_hidden()
        check = lambda name: page.get_by_role('checkbox', name='选择：' + name, exact=True)
        await check(SPECIAL).click()
        await check('b.txt').click(modifiers=['Shift'])
        await expect(page.locator('#selection-count')).to_have_text('已选择 3 项')
        await check('b.txt').uncheck()
        await expect(page.locator('#selection-count')).to_have_text('已选择 2 项')
        await page.locator('#search-input').fill('b.txt')
        await expect(page.locator('#entries .entry-name')).to_have_text(['b.txt'])
        await page.locator('#select-all').check()
        await expect(page.locator('#selection-count')).to_have_text('已选择 3 项（当前显示 1 项）')
        await page.locator('#search-clear').click()
        await page.locator('#view-grid-button').click()
        await expect(check(SPECIAL)).to_be_checked()
        await page.locator('#batch-copy').click()
        await page.locator('#picker-list .picker-item', has_text='目标').click()
        await page.locator('#picker-move').click()
        await page.locator('#tasks-close').click()
        await expect(page.locator('#batch-summary')).to_have_text('复制：成功 2 项，失败 1 项')
        await expect(page.locator('#batch-result-list')).to_contain_text('同名项目已存在')
        await expect(page.locator('#selection-count')).to_have_text('已选择 1 项')
        assert set(batches[0]['paths']) == {SPACE + '/a.txt', SPACE + '/b.txt', SPACE + '/' + SPECIAL}
        assert batches[0]['destination'] == SPACE + '/目标'
        await page.set_viewport_size({'width': 375, 'height': 812})
        await expect(page.locator('#batch-copy')).to_be_visible()
        assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'mobile horizontal overflow'
        if os.environ.get('WPS_UI_SCREENSHOTS'):
            shots = Path(os.environ['WPS_UI_SCREENSHOTS'])
            shots.mkdir(parents=True, exist_ok=True)
            await page.screenshot(path=str(shots / 'batch-mobile.png'), full_page=True, animations='disabled')
        await page.set_viewport_size({'width': 1280, 'height': 720})
        if os.environ.get('WPS_UI_SCREENSHOTS'):
            await page.screenshot(path=str(shots / 'batch-desktop.png'), full_page=True, animations='disabled')
        await page.locator('#view-list-button').click()
        await page.locator('#batch-move').click()
        await page.locator('#picker-list .picker-item', has_text='目标').click()
        await page.locator('#picker-move').click()
        await page.locator('#tasks-close').click()
        await expect(page.locator('#batch-summary')).to_have_text('移动：成功 1 项，失败 0 项')
        await expect(check('b.txt')).to_have_count(0)
        await check('a.txt').check()
        await page.locator('#batch-delete').click()
        await page.locator('#modal-cancel').click()
        assert len(batches) == 2
        await page.locator('#batch-delete').click()
        await page.locator('#modal-submit').click()
        await page.locator('#tasks-close').click()
        await expect(page.locator('#batch-summary')).to_have_text('删除：成功 1 项，失败 0 项')
        await check(SPECIAL).check()
        await check('资料').check()
        async with page.expect_download() as pending_download:
            await page.locator('#batch-archive').click()
        downloaded = await pending_download.value
        assert downloaded.suggested_filename == 'files.zip'
        assert set(archives[-1]) == {SPACE + '/' + SPECIAL, SPACE + '/资料'}
        archive_error = True
        await page.locator('#batch-archive').click()
        await expect(page.locator('#toasts')).to_contain_text('ZIP 项目超过限制')
        assert page.url.startswith('https://wps-offline.invalid/#')
        archive_error = False
        await page.locator('#selection-clear').click()

        # The native directory picker preserves all literal path characters.
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / '上传%2F #目录'
            (root / '子目录').mkdir(parents=True)
            (root / '子目录' / SPECIAL).write_text('offline')
            (root / 'empty.txt').write_text('')
            await page.locator('#folder-input').set_input_files(root)
            await expect(page.locator('#upload-button')).to_be_enabled()
            await expect(page.locator('#tray-list .tray-item.done')).to_have_count(4)
            assert ('file', SPACE + '/' + root.name + '/子目录/' + SPECIAL) in writes
            assert ('file', SPACE + '/' + root.name + '/empty.txt') in writes

        async def drop_directory(name, filename='child.txt', nested=True):
            await page.evaluate('''({name, filename, nested}) => {
              const f = new File(['data'], filename, {type:'text/plain'});
              const fileEntry = {isFile:true, isDirectory:false, name:filename, file:resolve => resolve(f)};
              const directory = (name, batches) => ({isFile:false, isDirectory:true, name,
                createReader:() => { let index=0; return {readEntries:resolve => resolve(batches[index++] || [])}; }});
              const children = nested ? [[directory('空文件夹', [])], [fileEntry], []] : [[fileEntry], []];
              const root = directory(name, children);
              const event = new Event('drop', {bubbles:true, cancelable:true});
              Object.defineProperty(event, 'dataTransfer', {value:{types:['Files'], files:[f], items:[{kind:'file',webkitGetAsEntry:()=>root}]}});
              window.dispatchEvent(event);
            }''', {'name': name, 'filename': filename, 'nested': nested})

        # readEntries is paginated, and empty directories must be created too.
        await drop_directory('拖入%2F #目录')
        await expect(page.locator('#upload-button')).to_be_enabled()
        await expect(page.locator('#tray-list .tray-item.done')).to_have_count(3)
        assert SPACE + '/拖入%2F #目录/空文件夹' in tree
        assert ('file', SPACE + '/拖入%2F #目录/child.txt') in writes
        write_count = len(writes)
        await drop_directory('拖入%2F #目录')
        await expect(page.locator('#modal-title')).to_have_text('文件夹已存在')
        await page.locator('#modal-cancel').click()
        await expect(page.locator('#upload-button')).to_be_enabled()
        await expect(page.locator('#tray-list .tray-item.skipped')).to_have_count(3)
        assert len(writes) == write_count
        await drop_directory('拖入%2F #目录')
        await page.locator('#modal-submit').click()
        await expect(page.locator('#modal-title')).to_have_text('文件已存在')
        await page.locator('#modal-cancel').click()
        await expect(page.locator('#upload-button')).to_be_enabled()
        assert len(writes) == write_count

        # A failed parent blocks descendants; retry keeps their original paths.
        failing_folder = SPACE + '/失败目录'
        await drop_directory('失败目录', nested=False)
        await expect(page.locator('#tray-list .tray-item.error')).to_have_count(1)
        await expect(page.locator('#upload-button')).to_be_enabled()
        await expect(page.locator('#tray-list .tray-item.skipped')).to_have_count(1)
        assert ('file', SPACE + '/失败目录/child.txt') not in writes
        failing_folder = None
        await page.locator('#tray-list .tray-item').nth(0).locator('button').click()
        await expect(page.locator('#upload-button')).to_be_enabled()
        await expect(page.locator('#tray-list .tray-item').nth(0)).to_have_class('tray-item done')
        await page.locator('#tray-list .tray-item').nth(1).locator('button').click()
        await expect(page.locator('#upload-button')).to_be_enabled()
        await expect(page.locator('#tray-list .tray-item.done')).to_have_count(2)
        assert ('file', SPACE + '/失败目录/child.txt') in writes

        # Cancelling while a mkdir is accepted stops all child uploads while
        # preserving the accurate created state of the in-flight directory.
        await drop_directory('取消目录', nested=False)
        await expect(page.locator('#tray-list .tray-item').nth(0)).to_have_class('tray-item active')
        await page.locator('#tray-cancel').click()
        folder_gate.set()
        await expect(page.locator('#upload-button')).to_be_enabled()
        await expect(page.locator('#tray-list .tray-item').nth(0)).to_have_class('tray-item done')
        await expect(page.locator('#tray-list .tray-item').nth(1)).to_have_class('tray-item cancelled')
        assert ('file', SPACE + '/取消目录/child.txt') not in writes
        folder_gate.clear()
        await drop_directory('取消单项目', nested=False)
        await expect(page.locator('#tray-list .tray-item').nth(0)).to_have_class('tray-item active')
        await page.locator('#tray-list .tray-item').nth(0).locator('button').click()
        folder_gate.set()
        await expect(page.locator('#upload-button')).to_be_enabled()
        await expect(page.locator('#tray-list .tray-item').nth(0)).to_have_class('tray-item done')
        await expect(page.locator('#tray-list .tray-item').nth(1)).to_have_class('tray-item skipped')
        assert ('file', SPACE + '/取消单项目/child.txt') not in writes
        assert not errors, errors
        print('PASS: selection/filter/grid, partial batch copy, move/delete confirmation, streamed ZIP, folder picker/drop pagination/empty folders, conflicts, retry and cancellation')
        await browser.close()


if __name__ == '__main__':
    asyncio.run(main())
