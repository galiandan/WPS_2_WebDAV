"""Actual native audio/video, progress, local subtitles and lazy thumbnails."""
import asyncio
import io
import json
import math
import os
import re
import struct
import wave
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect
from media_preview import PNG

ROOT = Path(__file__).resolve().parents[2]
WEB = ROOT / 'go' / 'web'
SPACE = '/媒体%2F #空间'


def audio_fixture():
    buffer = io.BytesIO()
    with wave.open(buffer, 'wb') as audio:
        audio.setnchannels(1)
        audio.setsampwidth(2)
        audio.setframerate(8000)
        audio.writeframes(b''.join(struct.pack('<h', round(500 * math.sin(i * 2 * math.pi * 220 / 8000))) for i in range(12 * 8000)))
    return buffer.getvalue()


async def main():
    # Repository-authored solid-color VP8 fixture; no external service or
    # multimedia tooling is needed to run this browser regression.
    video_bytes = (Path(__file__).parent / 'fixtures' / 'native-video.webm').read_bytes()
    audio_bytes = audio_fixture()
    async with async_playwright() as playwright:
        options = {'headless': True}
        if os.environ.get('WPS_UI_BROWSER'):
            options['executable_path'] = os.environ['WPS_UI_BROWSER']
        browser = await playwright.chromium.launch(**options)
        context = await browser.new_context(viewport={'width': 1280, 'height': 900})
        page = await context.new_page()
        errors, requests, thumbnails, writes = [], [], [], []
        page.on('pageerror', lambda error: errors.append(str(error)))
        username = 'alice'
        account_id = None
        policy_version = 1
        thumbnail_gate = asyncio.Event()
        hold_thumbnails = False
        active_thumbnails = 0
        maximum_thumbnails = 0
        names = ['01 声音%2F #.wav', '02 视频.webm', '03 不支持.mp4']
        files = [{'id': name, 'name': name, 'kind': 'file', 'size': len(audio_bytes) if name.endswith('.wav') else len(video_bytes), 'modified_at': 123} for name in names]
        images = [{'id': str(index), 'name': f'图片{index:02}.png', 'kind': 'file', 'size': len(PNG)} for index in range(40)]
        source = (ROOT / 'go/internal/app/application.go').read_text()
        policy_declaration = source.split('const webContentSecurityPolicy = ', 1)[1].split('\n\n', 1)[0]
        policy = ''.join(re.findall(r'"([^"]*)"', policy_declaration))
        await page.add_init_script('''window.revokedSubtitleURLs = []; const originalRevoke = URL.revokeObjectURL.bind(URL); URL.revokeObjectURL = (url) => {window.revokedSubtitleURLs.push(url); originalRevoke(url);};''')

        async def route(request):
            nonlocal active_thumbnails, maximum_thumbnails
            url = urlparse(request.request.url)
            path = url.path
            query = parse_qs(url.query)
            if path == '/':
                await request.fulfill(body=(WEB / 'index.html').read_bytes(), content_type='text/html', headers={'Content-Security-Policy': policy})
                return
            if path.startswith('/assets/'):
                asset = WEB / path.removeprefix('/assets/')
                await request.fulfill(body=asset.read_bytes(), content_type='text/css' if asset.suffix == '.css' else 'text/javascript')
                return
            if request.request.method not in ('GET', 'HEAD'):
                writes.append(path)
            if path.endswith('/auth/me'):
                data = {'authenticated': True, 'user': {'username': username, **({'id': account_id, 'policy_version': policy_version} if account_id else {})}}
            elif path.endswith('/auth/logout'):
                data = {}
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
                folder = query['path'][0]
                data = {'entries': [{'id': 'space:media', 'name': SPACE[1:], 'kind': 'folder'}] if folder == '/' else files + images if folder == SPACE else []}
            elif path.endswith('/preview'):
                requested = query['path'][0]
                requests.append((requested, request.request.headers.get('range', '')))
                if requested.endswith('.wav'):
                    body, mime = audio_bytes, 'audio/wav'
                elif requested.endswith('.webm'):
                    body, mime = video_bytes, 'video/webm'
                else:
                    body, mime = b'unsupported media codec', 'video/mp4'
                headers = {'Accept-Ranges': 'bytes', 'Content-Disposition': 'inline', 'X-Content-Type-Options': 'nosniff'}
                range_header = request.request.headers.get('range', '')
                match = re.fullmatch(r'bytes=(\d+)-(\d*)', range_header)
                status = 200
                if match:
                    start = int(match[1])
                    end = min(int(match[2]) if match[2] else len(body) - 1, len(body) - 1)
                    headers['Content-Range'] = f'bytes {start}-{end}/{len(body)}'
                    body = body[start:end + 1]
                    status = 206
                await request.fulfill(status=status, body=body, content_type=mime, headers=headers)
                return
            elif path.endswith('/thumbnail'):
                requested = query['path'][0]
                thumbnails.append(requested)
                active_thumbnails += 1
                maximum_thumbnails = max(maximum_thumbnails, active_thumbnails)
                try:
                    if hold_thumbnails:
                        await thumbnail_gate.wait()
                    if requested.endswith('图片00.png'):
                        await request.fulfill(status=415, json={'error': 'bad image'})
                    else:
                        await request.fulfill(body=PNG, content_type='image/png')
                finally:
                    active_thumbnails -= 1
                return
            elif path == '/favicon.ico':
                await request.fulfill(status=204)
                return
            else:
                raise AssertionError('Unexpected request: ' + path)
            await request.fulfill(json=data)

        await context.route('https://wps-offline.invalid/**', route)
        await page.goto('https://wps-offline.invalid/')
        await page.locator('#space-list > .directory-node > .directory-row > .tree-link').click()
        await expect(page.locator('#skeleton')).to_be_hidden()
        assert not thumbnails, thumbnails

        async def open_file(name):
            await page.get_by_role('button', name='选择文件：' + name, exact=True).dblclick()
            await expect(page.locator('#preview-title')).to_have_text(name)

        await open_file(names[0])
        player = page.locator('#preview-player')
        await page.wait_for_function('() => document.getElementById("preview-player").readyState >= 1')
        assert await player.evaluate('(media) => media.tagName') == 'AUDIO'
        assert await player.evaluate('(media) => media.paused && !media.autoplay && media.preload === "metadata"')
        assert requests[-1][0] == SPACE + '/' + names[0]
        await expect(page.locator('#player-previous')).to_be_disabled()
        assert await page.locator('#player-playlist option').count() == 3
        await player.evaluate('(media) => media.play()')
        await page.wait_for_function('() => document.getElementById("preview-player").currentTime > 0.25')
        await player.evaluate('(media) => {media.pause(); media.currentTime=5; media.dispatchEvent(new Event("timeupdate"));}')
        await page.locator('#player-rate').select_option('1.5')
        assert await player.evaluate('(media) => media.playbackRate') == 1.5
        await page.locator('#preview-close').click()
        assert await page.locator('#preview-player').count() == 0
        saved = json.loads(await page.evaluate('localStorage.getItem("wpsdrv.media-progress.alice")'))
        assert 4.9 <= saved[-1]['position'] <= 5.1, saved
        await open_file(names[0])
        await expect(page.locator('#player-progress-note')).to_contain_text('已恢复到 0:05')
        assert await player.evaluate('(media) => media.paused')
        # Capture old element; stale ended events must not navigate a new item.
        await player.evaluate('(media) => {window.oldPlayer=media;}')
        await page.locator('#player-next').click()
        await expect(page.locator('#preview-title')).to_have_text(names[1])
        await page.wait_for_function('() => document.getElementById("preview-player").readyState >= 1')
        await page.evaluate('window.oldPlayer.dispatchEvent(new Event("ended"))')
        await expect(page.locator('#preview-title')).to_have_text(names[1])
        assert await page.evaluate('window.oldPlayer.paused && !window.oldPlayer.hasAttribute("src")')
        assert await player.evaluate('(media) => media.tagName === "VIDEO" && media.paused && media.playsInline')
        await player.evaluate('(media) => media.play()')
        await page.wait_for_function('() => document.getElementById("preview-player").currentTime > 0.25')
        await player.evaluate('(media) => media.pause()')
        # WebVTT stays local; SRT becomes local WebVTT. Native tracks parse cues.
        await page.locator('#player-subtitle-input').set_input_files({'name': '字幕.vtt', 'mimeType': 'text/vtt', 'buffer': b'WEBVTT\n\n00:00:00.000 --> 00:00:10.000\nLocal subtitle\n'})
        await page.wait_for_function('() => document.getElementById("preview-player").textTracks[0]?.cues?.length === 1')
        first_url = await page.locator('#preview-player track').get_attribute('src')
        assert first_url.startswith('blob:')
        await page.locator('#player-subtitle-input').set_input_files({'name': '字幕.srt', 'mimeType': 'application/x-subrip', 'buffer': b'1\n00:00:00,000 --> 00:00:10,000\nConverted subtitle\n'})
        await expect(page.locator('#player-subtitle-note')).to_have_text('本地字幕：字幕.srt')
        assert first_url in await page.evaluate('window.revokedSubtitleURLs')
        await page.wait_for_function('() => document.querySelector("#preview-player track").track.cues?.[0]?.text === "Converted subtitle"')
        converted = await page.locator('#preview-player track').evaluate('(track) => ({start:track.track.cues[0].startTime,end:track.track.cues[0].endTime})')
        assert converted == {'start': 0, 'end': 10}, converted
        second_url = await page.locator('#preview-player track').get_attribute('src')
        await page.locator('#player-subtitle-input').set_input_files({'name': 'bad.srt', 'mimeType': 'application/x-subrip', 'buffer': b'invalid'})
        await expect(page.locator('#player-subtitle-note')).to_contain_text('时间轴无效')
        await page.set_viewport_size({'width': 390, 'height': 844})
        await expect(page.locator('#preview-close')).to_be_in_viewport()
        assert await page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        await page.screenshot(path='/tmp/wps-media-player-mobile.png', animations='disabled')
        await page.set_viewport_size({'width': 1280, 'height': 900})
        await page.screenshot(path='/tmp/wps-media-player-desktop.png', animations='disabled')
        await page.locator('#player-next').click()
        await expect(page.locator('#preview-error')).to_contain_text('浏览器无法播放此文件')
        await expect(page.locator('#preview-download')).to_be_enabled()
        assert second_url in await page.evaluate('window.revokedSubtitleURLs')
        await page.locator('#preview-close').click()
        assert not writes, writes
        # Progress is per account and bounded even if many old files exist.
        username = 'bob'
        await page.reload()
        await expect(page.locator('#skeleton')).to_be_hidden()
        await open_file(names[0])
        await page.wait_for_function('() => document.getElementById("preview-player").readyState >= 1')
        assert await player.evaluate('(media) => media.currentTime') == 0
        await page.locator('#preview-close').click()
        username = 'alice'
        # Reusing a username after account deletion, or assigning a new root
        # policy, must not restore another namespace's saved playback paths.
        for account_id, policy_version in [('first-alice', 1), ('recreated-alice', 1), ('recreated-alice', 2)]:
            await page.reload()
            await expect(page.locator('#skeleton')).to_be_hidden()
            await open_file(names[0])
            await page.wait_for_function('() => document.getElementById("preview-player").readyState >= 1')
            assert await player.evaluate('(media) => media.currentTime') == 0
            await player.evaluate('(media) => { media.currentTime = 6; }')
            await page.locator('#preview-close').click()
        account_id = None
        policy_version = 1
        await page.evaluate('''localStorage.setItem('wpsdrv.media-progress.alice', JSON.stringify(Array.from({length:200}, (_,i)=>({path:'/old/'+i,identity:'[]',position:5,updated:i}))));''')
        await page.reload()
        await expect(page.locator('#skeleton')).to_be_hidden()
        await open_file(names[0])
        await page.wait_for_function('() => document.getElementById("preview-player").readyState >= 1')
        await player.evaluate('(media) => {media.currentTime=4;}')
        await page.locator('#preview-close').click()
        assert await page.evaluate('JSON.parse(localStorage.getItem("wpsdrv.media-progress.alice")).length') == 200
        # Native playback reaching the end advances once and respects playlist.
        await open_file(names[0])
        await page.wait_for_function('() => document.getElementById("preview-player").readyState >= 1')
        await player.evaluate('(media) => {media.currentTime=media.duration-0.2; return media.play();}')
        await expect(page.locator('#preview-title')).to_have_text(names[1])
        await page.locator('#preview-close').click()
        # Grid thumbnails request only visible images with two active requests.
        hold_thumbnails = True
        await page.locator('#view-grid-button').click()
        await page.wait_for_function('() => document.querySelectorAll(".entry-thumbnail[src]").length === 2')
        assert len(thumbnails) == 2 and maximum_thumbnails <= 2, (len(thumbnails), maximum_thumbnails)
        thumbnail_gate.set()
        await expect(page.locator('.thumbnail-ready').first).to_be_visible()
        assert len(thumbnails) < len(images), len(thumbnails)
        first_image = page.locator('[data-entry-path]').filter(has=page.get_by_role('button', name='选择文件：图片00.png', exact=True))
        await expect(first_image.locator('.entry-glyph > .icon')).to_be_visible()
        assert await first_image.locator('.entry-thumbnail').count() == 0
        await page.locator('#view-list-button').click()
        assert await page.locator('.entry-thumbnail').count() == 0
        assert any(value for _, value in requests), requests
        # Signing out releases media source and local subtitle URLs immediately.
        await open_file(names[1])
        await page.wait_for_function('() => document.getElementById("preview-player").readyState >= 1')
        await page.evaluate('window.logoutPlayer=document.getElementById("preview-player")')
        async with page.expect_navigation():
            await page.evaluate('document.getElementById("logout-button").click()')
        await expect(page.locator('#preview-modal')).to_be_hidden()
        assert not errors, errors
        print('PASS: native audio/video playback, metadata-only open, Range, progress/account/bounds, playlist/stale events, local VTT/SRT cleanup, codecs, mobile and lazy thumbnails')
        await browser.close()


if __name__ == '__main__':
    asyncio.run(main())
