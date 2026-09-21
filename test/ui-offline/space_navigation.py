"""Exercise space navigation with intercepted requests; never start an HTTP server."""
import asyncio
import os
from collections import Counter
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
        context = await browser.new_context(viewport={'width': 1440, 'height': 900})
        page = await context.new_page()
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        counts = Counter()
        gates = {path: asyncio.Event() for path in ['/个人空间', '/团队空间']}
        fail_team = False
        connection_state = 'connected'

        async def route(request):
            nonlocal fail_team
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
            elif path.endswith('/tasks'):
                data = {'tasks': []}
            elif path.endswith('/status'):
                data = {'status': connection_state}
            elif path == '/healthz':
                data = {'version': '1.0.19'}
            elif path.endswith('/update'):
                data = {'state': 'idle', 'current_version': '1.0.19', 'update_available': False}
            elif path.endswith('/entries'):
                folder = parse_qs(url.query)['path'][0]
                counts[folder] += 1
                if folder in gates:
                    await gates[folder].wait()
                if folder == '/':
                    data = {'entries': [{'id': 'space:' + name, 'name': name, 'kind': 'folder'} for name in ['个人空间', '团队空间']]}
                elif folder == '/团队空间' and fail_team:
                    await request.fulfill(status=503, json={'error': 'Temporary offline test failure'})
                    return
                else:
                    data = {'entries': [{'id': folder, 'name': folder[1:] + '.txt', 'kind': 'file', 'size': 100}]}
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        await expect(page.locator('#auth-screen')).to_be_hidden()
        await expect(page.locator('#space-list > .directory-node > .directory-row > .tree-link')).to_have_count(2)
        await page.evaluate('window.originalSpaceButtons = [...document.querySelectorAll("#space-list > .directory-node > .directory-row > .tree-link")]')
        personal = page.get_by_role('button', name='进入空间 个人空间', exact=True)
        team = page.get_by_role('button', name='进入空间 团队空间', exact=True)

        # The upstream request is held: selection must change immediately.
        await personal.click()
        await expect(personal).to_have_attribute('aria-current', 'location')
        await expect(personal).to_have_attribute('aria-busy', 'true')
        await expect(personal).to_be_focused()
        await expect(page.locator('#entries')).to_be_hidden()
        await expect(page.locator('#skeleton')).to_be_visible()
        await expect(page.locator('#upload-button')).to_be_disabled()
        await expect(page.locator('#folder-button')).to_be_disabled()
        await team.click()
        await expect(team).to_have_attribute('aria-current', 'location')
        await expect(personal).not_to_have_attribute('aria-current', 'location')
        gates['/个人空间'].set()
        await expect(team).to_have_attribute('aria-busy', 'true')
        gates['/团队空间'].set()
        await expect(page.locator('#entries .entry-name')).to_have_text(['团队空间.txt'])
        await expect(team).to_have_attribute('aria-busy', 'false')
        await expect(team).to_be_focused()
        await expect(page.locator('#upload-button')).to_be_enabled()
        await expect(page.locator('#folder-button')).to_be_enabled()
        assert await page.evaluate('originalSpaceButtons.every((node, i) => node === document.querySelectorAll("#space-list > .directory-node > .directory-row > .tree-link")[i])')

        # Returning to a fresh directory reuses its response, including root.
        await page.locator('#space-root').click()
        await expect(page.locator('#entries .entry-name')).to_have_count(2)
        await team.click()
        await expect(page.locator('#entries .entry-name')).to_have_text(['团队空间.txt'])
        await expect(team).to_have_attribute('aria-busy', 'false')
        assert counts['/'] == 1, counts
        assert counts['/团队空间'] == 1, counts

        # A cache hit must not turn an expired WPS session back into connected.
        connection_state = 'session_expired'
        await page.locator('#connection').click()
        await page.locator('#status-refresh-button').click()
        await expect(page.locator('#connection-label')).to_have_text('WPS 登录已过期')
        await page.locator('#status-panel-close').click()
        await team.click()
        await expect(team).to_have_attribute('aria-busy', 'false')
        await expect(page.locator('#connection-label')).to_have_text('WPS 登录已过期')
        assert counts['/团队空间'] == 1, counts
        connection_state = 'connected'

        # Explicit refresh bypasses the cache; expiration also triggers a fetch.
        await page.locator('#refresh-button').click()
        await expect(team).to_have_attribute('aria-busy', 'false')
        assert counts['/团队空间'] == 2, counts
        await page.evaluate('Date.now = ((now) => () => now() + 31000)(Date.now)')
        await team.click()
        await expect(team).to_have_attribute('aria-busy', 'false')
        assert counts['/团队空间'] == 3, counts

        # Failure settles loading feedback without reverting the selected space.
        fail_team = True
        await page.locator('#refresh-button').click()
        await expect(page.locator('#status')).to_contain_text('服务暂时繁忙')
        await expect(team).to_have_attribute('aria-busy', 'false')
        await expect(team).to_have_attribute('aria-current', 'location')
        await expect(page.locator('#entries .entry-name')).to_have_text(['团队空间.txt'])
        assert not errors, errors
        print('PASS: immediate selection, latest navigation, focus/DOM stability, cache TTL, forced refresh, failure recovery')
        await browser.close()


if __name__ == '__main__':
    asyncio.run(main())
