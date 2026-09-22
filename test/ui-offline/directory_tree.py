"""Offline regressions for nested sidebar navigation and stale responses."""
import asyncio
import os
import re
from collections import Counter
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

WEB = Path(__file__).resolve().parents[2] / 'go' / 'web'


def folder(name, space=False):
    return {'id': ('space:' if space else 'folder:') + name, 'kind': 'folder', 'name': name}


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context(viewport={'width': 1440, 'height': 900})
        page = await context.new_page()
        errors, counts = [], Counter()
        page.on('pageerror', lambda error: errors.append(str(error)))
        base = '/个人空间'
        special = '资料%2F #中文'
        nested = base + '/' + special
        leaf = nested + '/项目'
        listings = {
            '/': [folder('个人空间', True), folder('团队空间', True)],
            base: [folder(special), folder('失败目录'), folder('慢目录'), folder('空目录'),
                   {'id': 'text', 'kind': 'file', 'name': '隐藏文件.txt', 'size': 1}],
            '/团队空间': [],
            nested: [folder('项目')],
            leaf: [folder('子目录')],
            leaf + '/子目录': [],
            base + '/失败目录': [folder('恢复目录')],
            base + '/失败目录/恢复目录': [],
            base + '/空目录': [],
            base + '/慢目录': [folder('过期子目录')],
        }
        fail = True
        slow_started, slow_gate = asyncio.Event(), asyncio.Event()

        async def route(request):
            nonlocal fail
            url = urlparse(request.request.url)
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
            elif path.endswith('/settings'):
                data = {'name': 'WPS Drive'}
            elif path.endswith('/transfers'):
                data = {'tasks': []}
            elif path.endswith('/tasks'):
                data = {'tasks': []}
            elif path.endswith('/status'):
                data = {'status': 'connected'}
            elif path == '/healthz':
                data = {'version': 'offline'}
            elif path.endswith('/update'):
                data = {'state': 'idle', 'update_available': False}
            elif path.endswith('/entries'):
                target = parse_qs(url.query)['path'][0]
                counts[target] += 1
                if target == base + '/失败目录' and fail:
                    await request.fulfill(status=503, json={'error': 'offline test failure'})
                    return
                if target == base + '/慢目录':
                    slow_started.set()
                    await slow_gate.wait()
                assert target in listings, target
                data = {'entries': listings[target]}
            elif path.endswith('/folders'):
                target = parse_qs(url.query)['path'][0]
                parent, name = target.rsplit('/', 1)
                listings[parent].append(folder(name))
                listings[target] = []
                data = {}
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('**/*', route)
        await page.goto('https://wps-offline.invalid/')
        tree = page.locator('#space-list')

        await expect(tree.get_by_role('button', name='进入空间 个人空间', exact=True)).to_be_visible()
        assert counts[nested] == 0
        await tree.get_by_role('button', name='展开目录 个人空间', exact=True).click()
        await expect(tree.get_by_role('button', name='进入目录 ' + special, exact=True)).to_be_visible()
        await expect(page.locator('#space-root')).to_have_attribute('aria-current', 'location')
        assert counts[nested] == 0, 'expansion must not walk descendants'
        assert await tree.get_by_text('隐藏文件.txt', exact=True).count() == 0
        await tree.get_by_role('button', name='展开目录 ' + special, exact=True).click()
        await expect(tree.get_by_role('button', name='进入目录 项目', exact=True)).to_be_visible()
        assert counts[nested] == 1
        project = tree.get_by_role('button', name='进入目录 项目', exact=True)
        await project.click()
        await expect(project).to_have_attribute('aria-current', 'location')
        await expect(tree.get_by_role('button', name='进入空间 个人空间', exact=True)).not_to_have_attribute('aria-current', 'location')
        await expect(page.locator('#breadcrumbs')).to_contain_text(special)
        await expect(tree.get_by_role('button', name='进入目录 子目录', exact=True)).to_be_visible()
        assert await tree.locator('[aria-current="location"]').count() == 1
        assert counts[nested] == 1
        await project.focus()
        await page.keyboard.press('ArrowLeft')
        await expect(tree.get_by_role('button', name='展开目录 项目', exact=True)).to_have_attribute('aria-expanded', 'false')
        await page.keyboard.press('ArrowRight')
        await expect(tree.get_by_role('button', name='进入目录 子目录', exact=True)).to_be_visible()
        await page.keyboard.press('ArrowRight')
        await expect(tree.get_by_role('button', name='进入目录 子目录', exact=True)).to_be_focused()

        # A write invalidates the same listing used by the tree.
        await page.locator('#folder-button').click()
        await page.locator('#modal-input').fill('新建文件夹')
        await page.locator('#modal-submit').click()
        await expect(tree.get_by_role('button', name='进入目录 新建文件夹', exact=True)).to_be_visible()

        await tree.get_by_role('button', name='展开目录 失败目录', exact=True).click()
        await expect(tree.get_by_role('button', name='重试', exact=True)).to_be_visible()
        fail = False
        await tree.get_by_role('button', name='重试', exact=True).click()
        await expect(tree.get_by_role('button', name='进入目录 恢复目录', exact=True)).to_be_visible()
        await tree.get_by_role('button', name='展开目录 空目录', exact=True).click()
        empty = tree.locator('[data-tree-path="/个人空间/空目录"]')
        await expect(empty.locator('.directory-status')).to_have_text('无子文件夹')

        # Keep an in-flight branch read, remove it from the parent, then deliver
        # the old response. It must not recreate a removed subtree.
        await tree.get_by_role('button', name='展开目录 慢目录', exact=True).click()
        await asyncio.wait_for(slow_started.wait(), 5)
        await tree.get_by_role('button', name='收起目录 慢目录', exact=True).click()
        listings[base] = [item for item in listings[base] if item['name'] != '慢目录']
        await tree.get_by_role('button', name='进入空间 个人空间', exact=True).click()
        await page.locator('#refresh-button').click()
        await expect(tree.get_by_role('button', name='进入目录 慢目录', exact=True)).to_have_count(0)
        slow_gate.set()
        await expect(tree.get_by_role('button', name='进入目录 过期子目录', exact=True)).to_have_count(0)

        # Refresh reflects folder renames/removals without retaining ghost nodes.
        listings[nested] = [folder('重命名项目')]
        listings[nested + '/重命名项目'] = []
        await tree.get_by_role('button', name='进入目录 ' + special, exact=True).click()
        await page.locator('#refresh-button').click()
        await expect(tree.get_by_role('button', name='进入目录 项目', exact=True)).to_have_count(0)
        await expect(tree.get_by_role('button', name='进入目录 重命名项目', exact=True)).to_be_visible()
        await page.screenshot(path='/tmp/wps-directory-tree-desktop.png', animations='disabled')

        # A direct deep link reconstructs its ancestry after a full reload.
        await page.evaluate("path => location.hash = encodeURIComponent(path)", nested + '/重命名项目')
        await page.reload()
        current = tree.get_by_role('button', name='进入目录 重命名项目', exact=True)
        await expect(current).to_have_attribute('aria-current', 'location')
        await expect(current).to_be_visible()
        await page.set_viewport_size({'width': 390, 'height': 844})
        await page.locator('#nav-menu-button').click()
        await expect(current).to_be_visible()
        await page.screenshot(path='/tmp/wps-directory-tree-mobile.png', animations='disabled')
        await tree.get_by_role('button', name='收起目录 ' + special, exact=True).click()
        await expect(page.locator('body')).to_have_class(re.compile(r'.*\bnav-open\b.*'))
        await tree.get_by_role('button', name='进入目录 ' + special, exact=True).click()
        await expect(page.locator('#nav-menu-button')).to_have_attribute('aria-expanded', 'false')
        await expect(page.locator('#breadcrumbs')).to_contain_text(special)
        assert not errors, errors
        await context.close()
        await browser.close()
    print('PASS: lazy nested tree, exact selection, cache reuse, keyboard, writes, retry, stale removal, deep link and mobile')


if __name__ == '__main__':
    asyncio.run(main())
