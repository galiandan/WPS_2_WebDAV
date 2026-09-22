"""Global filename search UI, mocked index states and navigation; no WPS access."""
import asyncio
import os
from pathlib import Path
from urllib.parse import parse_qs, quote, urlparse

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
        errors, searches = [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        index_state, indexed_scope = 'idle', '/'
        fail = False
        unauthorized = False
        refresh_conflict = False
        hold_refresh = False
        refresh_started, refresh_gate = asyncio.Event(), asyncio.Event()
        slow_started, slow_gate = asyncio.Event(), asyncio.Event()
        result_path = '/个人空间/资料 %2F/报告.txt'

        async def route(request):
            nonlocal index_state, indexed_scope
            url = urlparse(request.request.url)
            path = url.path
            params = parse_qs(url.query, keep_blank_values=True)
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
            elif path.endswith('/transfers'):
                data = {'tasks': []}
            elif path.endswith('/tasks'):
                data = {'tasks': []}
            elif path.endswith('/entries'):
                folder = params['path'][0]
                data = {'entries': ([{'id': 'space:' + name, 'name': name, 'kind': 'folder'} for name in ['个人空间', '团队空间']] if folder == '/' else
                                    [{'id': 'report', 'name': '报告.txt', 'kind': 'file', 'size': 10}])}
            elif path.endswith('/search/refresh'):
                assert request.request.method == 'POST'
                indexed_scope, index_state = params['path'][0], 'indexing'
                if hold_refresh:
                    refresh_started.set()
                    await refresh_gate.wait()
                    await request.fulfill(status=503, json={'error': '旧窗口的延迟错误'})
                    return
                if refresh_conflict:
                    await request.fulfill(status=409, json={'index': {'state': index_state}})
                    return
                data = {'index': {'state': index_state}}
            elif path.endswith('/search'):
                if request.request.method == 'DELETE':
                    index_state = 'cancelled'
                    data = {'index': {'state': index_state}}
                else:
                    searches.append(params)
                    if unauthorized:
                        await request.fulfill(status=401, body='Session expired', content_type='text/plain')
                        return
                    if fail:
                        await request.fulfill(status=503, json={'error': '搜索暂时不可用'})
                        return
                    if params.get('q') == ['慢查询']:
                        slow_started.set()
                        await slow_gate.wait()
                    covered = indexed_scope == '/' or params['path'][0] == indexed_scope
                    populated = covered and index_state == 'ready' and params.get('q', [''])[0] != '没有结果'
                    offset = int(params.get('offset', ['0'])[0])
                    data = {'scope_covered': covered,
                            'index': {'state': index_state, 'path': indexed_scope, 'complete': index_state == 'ready', 'entries': 101 if populated else 0, 'updated_at': 1790000000},
                            'total': 101 if populated else 0, 'has_more': populated and offset == 0,
                            'results': [{'path': result_path if index == 0 else result_path.removesuffix('.txt') + str(index) + '.txt',
                                         'entry': {'name': '报告.txt' if index == 0 else '报告' + str(index) + '.txt', 'kind': 'file', 'size': 10}}
                                        for index in range(offset, min(offset + 100, 101))] if populated else []}
            elif path.endswith('/preview'):
                assert params['path'] == [result_path]
                await request.fulfill(body=b'preview result', content_type='application/octet-stream')
                return
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        await page.get_by_role('button', name='进入空间 个人空间', exact=True).click()
        await expect(page.locator('#skeleton')).to_be_hidden()
        await page.locator('#global-search-button').click()
        await expect(page.locator('#global-search-scope')).to_have_value('/个人空间')
        await expect(page.locator('#global-search-status')).to_contain_text('尚未建立索引')
        await page.locator('#global-search-refresh').click()
        await expect(page.locator('#global-search-status')).to_contain_text('正在扫描')
        await expect(page.locator('#global-search-cancel')).to_be_visible()
        await expect(page.locator('#global-search-refresh')).to_be_disabled()
        await page.locator('#global-search-cancel').click()
        await expect(page.locator('#global-search-status')).to_contain_text('扫描已停止')
        await expect(page.locator('#global-search-results')).to_contain_text('完整结果需要完成索引')
        index_state = 'ready'
        await page.locator('#global-search-query').fill('报告')
        await page.locator('#global-search-submit').click()
        await expect(page.locator('#global-search-results')).to_contain_text(result_path)
        await page.locator('#global-search-type').select_option('document')
        await page.locator('#global-search-match').select_option('path')
        await expect(page.locator('#global-search-results strong').first).to_have_text('报告.txt')
        await page.locator('#global-search-next').click()
        await expect(page.locator('#global-search-next')).to_be_disabled()
        await expect(page.locator('#global-search-previous')).to_be_enabled()
        assert searches[-1]['offset'] == ['100']
        assert searches[-1]['type'] == ['document'] and searches[-1]['match'] == ['path']
        await page.locator('#global-search-previous').click()
        await expect(page.locator('#global-search-previous')).to_be_disabled()
        await page.locator('#global-search-scope').select_option('/团队空间')
        await expect(page.locator('#global-search-status')).to_contain_text('当前范围未被完整索引')
        await page.locator('#global-search-scope').select_option('/个人空间')
        await page.locator('#global-search-results').get_by_role('button', name='预览', exact=True).first.click()
        await expect(page.locator('#global-search-modal')).not_to_be_visible()
        await expect(page.locator('#preview-content')).to_have_text('preview result')
        await page.locator('#preview-close').click()
        await page.locator('#global-search-button').click()
        await page.locator('#global-search-results').get_by_role('button', name='所在目录').first.click()
        await expect(page.locator('#global-search-modal')).not_to_be_visible()
        await expect(page).to_have_url('https://wps-offline.invalid/#' + quote('/个人空间/资料 %2F', safe=''))
        await page.locator('#global-search-button').click()
        await page.locator('#global-search-query').fill('慢查询')
        await page.locator('#global-search-submit').click()
        await asyncio.wait_for(slow_started.wait(), 5)
        await page.locator('#global-search-query').fill('没有结果')
        await page.locator('#global-search-submit').click()
        await expect(page.locator('#global-search-results')).to_contain_text('没有匹配')
        slow_gate.set()
        await expect(page.locator('#global-search-results strong')).to_have_count(0)
        fail = True
        await page.locator('#global-search-submit').click()
        await expect(page.locator('#global-search-error')).to_have_text('搜索暂时不可用')
        await expect(page.locator('#global-search-results strong')).to_have_count(0)
        fail = False
        await page.locator('#global-search-query').fill('报告')
        await page.locator('#global-search-submit').click()
        await expect(page.locator('#global-search-error')).to_have_text('')
        await expect(page.locator('#global-search-results strong')).to_have_count(100)
        await page.locator('#global-search-query').fill('报' * 86)
        search_count = len(searches)
        await page.locator('#global-search-submit').click()
        await expect(page.locator('#global-search-error')).to_contain_text('关键词过长')
        assert len(searches) == search_count
        await page.locator('#global-search-query').fill('报告')
        await page.locator('#global-search-submit').click()
        await expect(page.locator('#global-search-results strong')).to_have_count(100)
        refresh_conflict = True
        await page.locator('#global-search-refresh').click()
        await expect(page.locator('#global-search-status')).to_contain_text('正在扫描')
        await expect(page.locator('#global-search-error')).to_have_text('')
        await expect(page.locator('#global-search-refresh')).to_be_disabled()
        refresh_conflict = False
        await page.locator('#global-search-cancel').click()
        await expect(page.locator('#global-search-status')).to_contain_text('扫描已停止')
        hold_refresh = True
        await page.locator('#global-search-refresh').click()
        await asyncio.wait_for(refresh_started.wait(), 5)
        await page.locator('#global-search-close').click()
        await expect(page.locator('#global-search-results strong')).to_have_count(0)
        index_state = 'ready'
        await page.locator('#global-search-button').click()
        await expect(page.locator('#global-search-results strong')).to_have_count(100)
        refresh_gate.set()
        await expect(page.locator('#global-search-error')).to_have_text('')
        hold_refresh = False
        await page.screenshot(path='/tmp/wps-global-search-desktop.png')
        await page.set_viewport_size({'width': 390, 'height': 844})
        await page.screenshot(path='/tmp/wps-global-search-mobile.png')
        box = await page.locator('#global-search-modal').bounding_box()
        assert box['x'] >= 0 and box['x'] + box['width'] <= 390, box
        await page.keyboard.press('Escape')
        await expect(page.locator('#global-search-modal')).not_to_be_visible()
        await expect(page.locator('#global-search-results strong')).to_have_count(0)
        unauthorized = True
        await page.locator('#global-search-button').click()
        await expect(page.locator('#global-search-modal')).not_to_be_visible()
        await expect(page.locator('#auth-screen')).to_be_visible()
        await expect(page.locator('#global-search-results strong')).to_have_count(0)
        assert not errors, errors
        await context.close()
        await browser.close()
    print('PASS: search scope/type/path, pagination, cancellation, stale responses/actions, auth expiry, byte limits, preview/navigation and mobile')


if __name__ == '__main__':
    asyncio.run(main())
