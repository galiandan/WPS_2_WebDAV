"""Hold startup requests to check first-paint and session routing offline."""
import asyncio
import os
import re
from pathlib import Path
from urllib.parse import urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go' / 'web'


async def check_case(browser, outcome):
    context = await browser.new_context()
    page = await context.new_page()
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    script_gate = asyncio.Event()
    auth_gate = asyncio.Event()
    auth_started = asyncio.Event()

    async def route(request):
        path = urlparse(request.request.url).path
        if path == '/':
            await request.fulfill(body=(WEB / 'index.html').read_bytes(), content_type='text/html')
        elif path.startswith('/assets/'):
            asset = WEB / path.removeprefix('/assets/')
            if asset.name == 'app.js':
                await script_gate.wait()
            await request.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
        elif path.endswith('/auth/me'):
            auth_started.set()
            await auth_gate.wait()
            if outcome == 'network_error':
                await request.abort('failed')
            elif outcome == 'disabled':
                await request.fulfill(status=404, json={'error': 'Not found'})
            else:
                await request.fulfill(json={'authenticated': outcome == 'authenticated', 'user': {'username': 'demo'}})
        elif path.endswith('/settings'):
            await request.fulfill(json={'name': 'WPS Drive'})
        elif path.endswith('/status'):
            await request.fulfill(json={'status': 'connected'})
        elif path.endswith('/entries'):
            await request.fulfill(json={'entries': []})
        elif path == '/healthz':
            await request.fulfill(json={'version': 'offline'})
        elif path.endswith('/update'):
            await request.fulfill(json={'state': 'idle', 'update_available': False})
        elif path == '/favicon.ico':
            await request.fulfill(status=204)
        else:
            raise AssertionError('Unexpected request: ' + path)

    await context.route('**/*', route)
    await page.goto('https://wps-offline.invalid/', wait_until='commit')
    # Even before JavaScript arrives, the login form must not be painted.
    await expect(page.locator('#auth-loading')).to_be_visible()
    await expect(page.locator('#auth-screen')).to_be_hidden()
    await expect(page.locator('#app-ui')).to_be_hidden()
    script_gate.set()
    await asyncio.wait_for(auth_started.wait(), timeout=10)
    await expect(page.locator('#auth-loading')).to_be_visible()
    await expect(page.locator('#auth-screen')).to_be_hidden()
    await expect(page.locator('#app-ui')).to_be_hidden()
    auth_gate.set()
    if outcome in ('authenticated', 'disabled'):
        await expect(page.locator('#app-ui')).to_be_visible()
        await expect(page.locator('#auth-screen')).to_be_hidden()
        await expect(page.locator('#auth-screen')).not_to_have_class(re.compile(r'.*\bleaving\b.*'))
    else:
        await expect(page.locator('#auth-screen')).to_be_visible()
        await expect(page.locator('#app-ui')).to_be_hidden()
        if outcome == 'network_error':
            await expect(page.locator('#auth-message')).to_have_text('无法连接服务，请刷新页面重试')
    await expect(page.locator('#auth-loading')).to_be_hidden()
    if outcome == 'authenticated':
        await page.screenshot(path='/tmp/wps-auth-startup.png')
    assert not errors, errors
    await context.close()


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        for outcome in ('authenticated', 'anonymous', 'disabled', 'network_error'):
            await check_case(browser, outcome)
        await browser.close()
    print('PASS: startup stays neutral until session check finishes; all four outcomes resolve')


if __name__ == '__main__':
    asyncio.run(main())
