"""Image/PDF gallery regressions using sanitized in-memory responses only."""
import asyncio
import struct
import zlib
import os
import re
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect

ROOT = Path(__file__).resolve().parents[2]
WEB = ROOT / 'go/web'
def image_fixture():
    width, height = 800, 450
    def chunk(kind, data):
        return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', zlib.crc32(kind + data))
    rows = bytearray()
    for y in range(height):
        rows.append(0)
        for x in range(width):
            if 100 < x < 700 and 100 < y < 350:
                rows.extend((45, 130 + x // 12, 190 + y // 9))
            else:
                rows.extend((230 + x // 40, 235 + y // 24, 250))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', width, height, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(rows)) + chunk(b'IEND', b'')


PNG = image_fixture()


def pdf_fixture():
    objects = [b'<< /Type /Catalog /Pages 2 0 R >>',
               b'<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
               b'<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 400] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>',
               b'<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>']
    stream = b'BT /F1 22 Tf 35 340 Td (Offline PDF preview) Tj ET'
    objects.append(b'<< /Length ' + str(len(stream)).encode() + b' >>\nstream\n' + stream + b'\nendstream')
    data = b'%PDF-1.4\n'
    offsets = [0]
    for index, obj in enumerate(objects, 1):
        offsets.append(len(data))
        data += f'{index} 0 obj\n'.encode() + obj + b'\nendobj\n'
    start = len(data)
    data += f'xref\n0 {len(offsets)}\n0000000000 65535 f \n'.encode()
    data += b''.join(f'{offset:010d} 00000 n \n'.encode() for offset in offsets[1:])
    return data + f'trailer\n<< /Size {len(offsets)} /Root 1 0 R >>\nstartxref\n{start}\n%%EOF\n'.encode()


async def main():
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context(viewport={'width': 1366, 'height': 900})
        page = await context.new_page()
        errors, requests = [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        # Match the production document policy, so images and frames cannot
        # accidentally depend on a more permissive test document.
        source = (ROOT / 'go/internal/app/application.go').read_text()
        policy_declaration = source.split('const webContentSecurityPolicy = ', 1)[1].split('\n\n', 1)[0]
        policy = ''.join(re.findall(r'"([^"]*)"', policy_declaration))
        names = ['01 中文 %2F.png', '02 图片.PNG', '03 损坏.png', '04 文档.pdf', '05 文本.txt', '06 禁止.svg']

        async def route(request):
            url = urlparse(request.request.url)
            if url.path == '/':
                await request.fulfill(body=(WEB / 'index.html').read_bytes(), content_type='text/html',
                                      headers={'Content-Security-Policy': policy})
                return
            if url.path.startswith('/assets/'):
                asset = WEB / url.path.removeprefix('/assets/')
                await request.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
                return
            if url.path.endswith('/auth/me'):
                data = {'authenticated': True, 'user': {'username': 'demo'}}
            elif url.path.endswith('/settings'):
                data = {'name': 'WPS Drive'}
            elif url.path.endswith('/tasks'):
                data = {'tasks': []}
            elif url.path.endswith('/status'):
                data = {'status': 'connected'}
            elif url.path == '/healthz':
                data = {'version': 'offline'}
            elif url.path.endswith('/update'):
                data = {'state': 'idle', 'update_available': False}
            elif url.path.endswith('/entries'):
                folder = parse_qs(url.query)['path'][0]
                data = {'entries': ([{'id': 'space:test', 'name': '测试空间', 'kind': 'folder'}] if folder == '/' else
                                    [{'id': name, 'name': name, 'kind': 'file', 'size': 100} for name in names])}
            elif url.path.endswith('/preview'):
                path = parse_qs(url.query)['path'][0]
                requests.append(path)
                if '损坏' in path:
                    await request.fulfill(status=503, json={'error': 'offline failure'})
                    return
                body, mime = (pdf_fixture(), 'application/pdf') if path.endswith('.pdf') else (b'text preview', 'application/octet-stream') if path.endswith('.txt') else (PNG, 'image/png')
                await request.fulfill(body=body, content_type=mime, headers={
                    'Content-Disposition': 'inline', 'X-Content-Type-Options': 'nosniff',
                    'Content-Security-Policy': "default-src 'none'; frame-ancestors 'self'",
                })
                return
            elif url.path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + url.path)
            await request.fulfill(json=data)

        await context.route('https://wps-offline.invalid/**', route)
        await page.goto('https://wps-offline.invalid/')
        await page.locator('#space-list > .directory-node > .directory-row > .tree-link').click()
        await expect(page.locator('#skeleton')).to_be_hidden()

        async def open_preview(name):
            await page.get_by_role('button', name='选择文件：' + name, exact=True).dblclick()
            await expect(page.locator('#preview-title')).to_have_text(name)

        await open_preview(names[0])
        img = page.locator('#preview-media img')
        await expect(img).to_be_visible()
        await page.wait_for_function('document.querySelector("#preview-media img").naturalWidth > 0')
        assert requests[-1] == '/测试空间/' + names[0]
        await expect(page.locator('#preview-previous')).to_be_disabled()
        await expect(page.locator('#preview-text-toolbar')).to_be_hidden()
        await page.locator('#preview-zoom').select_option('200')
        await expect(img).to_have_css('width', '1600px')
        await page.locator('#preview-next').click()
        await expect(page.locator('#preview-title')).to_have_text(names[1])
        await expect(page.locator('#preview-zoom')).to_have_value('fit')
        await page.locator('#preview-next').click()
        await expect(page.locator('#preview-error')).to_contain_text('文件预览失败')
        await expect(page.locator('#preview-next')).to_be_disabled()
        await page.locator('#preview-previous').click()
        await expect(page.locator('#preview-error')).to_have_text('')
        await page.wait_for_function('document.querySelector("#preview-media img").naturalWidth === 800')
        # Image dimensions become available before its load event. Capture
        # the settled layout after the loading panel has been removed.
        await expect(page.locator('#preview-loading')).to_be_hidden()
        await expect(page.locator('#preview-close')).to_be_in_viewport()
        await page.screenshot(path='/tmp/wps-media-preview-desktop.png')
        await page.set_viewport_size({'width': 390, 'height': 844})
        await page.screenshot(path='/tmp/wps-media-preview-mobile.png')
        box = await page.locator('#preview-modal').bounding_box()
        assert box['x'] >= 0 and box['x'] + box['width'] <= 390, box
        await expect(page.locator('#preview-close')).to_be_in_viewport()
        await page.locator('#preview-close').click()
        assert await page.locator('#preview-media img').count() == 0
        await page.set_viewport_size({'width': 1366, 'height': 900})
        await open_preview(names[3])
        await expect(page.locator('#preview-media iframe')).to_be_visible()
        await expect(page.locator('#preview-media iframe')).to_have_attribute('title', names[3])
        await expect(page.locator('#preview-media-toolbar')).to_be_hidden()
        await expect(page.locator('#preview-note')).to_contain_text('浏览器内置阅读器')
        await expect(page.locator('#preview-loading')).to_be_hidden()
        assert requests[-1] == '/测试空间/' + names[3]
        await page.wait_for_timeout(1500)
        await page.screenshot(path='/tmp/wps-pdf-preview-desktop.png')
        await page.keyboard.press('Escape')
        assert await page.locator('#preview-media iframe').count() == 0
        await open_preview(names[4])
        await expect(page.locator('#preview-content')).to_have_text('text preview')
        await expect(page.locator('#preview-text-toolbar')).to_be_visible()
        await expect(page.locator('#preview-media')).to_be_hidden()
        await page.locator('#preview-close').click()
        await page.get_by_role('button', name='选择文件：' + names[5], exact=True).dblclick()
        await expect(page.locator('#preview-modal')).not_to_be_visible()
        assert not errors, errors
        await context.close()
        await browser.close()
    print('PASS: image gallery, zoom, paths, failures, PDF frame, text switching, active formats and mobile layout')


if __name__ == '__main__':
    asyncio.run(main())
