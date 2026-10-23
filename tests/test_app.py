import json
import os
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET
from http.cookiejar import CookieJar
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest.mock import patch

from app import Server, Store, UserError


class FakeAI(BaseHTTPRequestHandler):
    requests = []

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        self.requests.append((self.path, body, self.headers.get('Authorization')))
        context = json.loads(body['messages'][-1]['content'])
        title = context.get('filename', context.get('folder', '测试系列'))
        value = {'title': title, 'plot': '基于实际资料的简介 & <示例>', 'genres': ['教程'], 'tags': ['示例'], 'notes': ''}
        content = json.dumps(value, ensure_ascii=False)
        response = {'message': {'content': content}} if self.path == '/api/chat' else {'choices': [{'message': {'content': content}}]}
        raw = json.dumps(response).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class WorkbenchTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.base = Path(self.temp.name)
        self.media = self.base / 'media'
        self.media.mkdir()
        self.store = Store(self.media, self.base / 'data')

    def tearDown(self):
        self.store.db.close()
        self.temp.cleanup()

    def video(self, path):
        p = self.media / path
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(b'test fixture, not a real video')
        return p

    def scan(self, path='', kind='movie'):
        self.store.scan(path, kind, True)
        return self.store.list_videos()

    def draft(self, row, season=None, episode=None):
        body = {'metadata': {'title': '标题 & <测试>', 'plot': '简介', 'genres': ['教程'], 'tags': ['AI']}, 'season': season, 'episode': episode, 'show': {'title': '系列', 'plot': '系列简介'}}
        return self.store.update_video(row['id'], body)

    def wait_job(self, action, ids, overwrite=False):
        job = self.store.start_job(action, ids, overwrite)
        end = time.monotonic() + 8
        while time.monotonic() < end:
            row = self.store.rows('SELECT * FROM jobs WHERE id=?', (job['id'],))[0]
            if row['status'] not in ('queued', 'running', 'cancelling'):
                return row
            time.sleep(0.02)
        self.fail('job timed out')

    def test_scan_natural_episode_numbers_and_preserve_manual_changes(self):
        self.video('课程/Season 02/03-文件处理.mp4')
        self.video('课程/未知标题.mkv')
        self.video('课程/链接/S01E01.mp4')
        rows = self.scan('课程', 'episode')
        numbered = next(x for x in rows if '03-' in x['path'])
        self.assertEqual((numbered['season'], numbered['episode']), (2, 3))
        unknown = next(x for x in rows if '未知' in x['path'])
        self.assertIsNone(unknown['episode'])
        self.draft(numbered, 2, 8)
        self.store.scan('课程', 'movie', True)
        row = self.store.get_video(numbered['id'])
        self.assertEqual(row['episode'], 8)
        self.assertEqual(row['kind'], 'episode')
        self.assertEqual(row['metadata']['title'], '标题 & <测试>')

    def test_path_traversal_and_symlinks_rejected(self):
        outside = self.base / 'private'
        outside.mkdir()
        (outside / 'secret.mp4').write_bytes(b'private')
        (self.media / 'link').symlink_to(outside, target_is_directory=True)
        self.assertEqual(self.scan(), [])
        for p in ('../private', str(outside), 'link/secret.mp4'):
            with self.assertRaises(UserError):
                self.store.safe(p)

    def test_atomic_save_default_skip_backup_and_xml_escaping(self):
        original_video = self.video('独立视频.mp4')
        row = self.draft(self.scan()[0])
        nfo = self.media / '独立视频.nfo'
        nfo.write_text('original nfo', 'utf-8')
        job = self.wait_job('save', [row['id']])
        self.assertEqual(job['skipped'], 1)
        self.assertEqual(nfo.read_text(), 'original nfo')
        job = self.wait_job('save', [row['id']], True)
        self.assertEqual(job['failed'], 0)
        self.assertEqual(ET.parse(nfo).findtext('title'), '标题 & <测试>')
        backups = list(self.media.glob('*.bak.*'))
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].read_text(), 'original nfo')
        self.assertEqual(original_video.read_bytes(), b'test fixture, not a real video')

    def test_missing_or_duplicate_episode_blocks_writes(self):
        self.video('课/未知.mp4')
        self.video('课/02-第二集.mp4')
        rows = self.scan('课', 'episode')
        for row in rows:
            self.draft(row)
        job = self.wait_job('save', [rows[0]['id']])
        self.assertEqual(job['failed'], 1)
        self.assertFalse(list((self.media / '课').glob('*.nfo')))
        for row in rows:
            self.draft(row, 1, 2)
        with self.assertRaisesRegex(UserError, '重复'):
            self.store.preview(rows[0]['id'])

    def test_same_stem_collision_and_nfo_symlink(self):
        self.video('同名.mp4')
        self.video('同名.mkv')
        row = self.draft(self.scan()[0])
        with self.assertRaisesRegex(UserError, '同名'):
            self.store.preview(row['id'])
        outside = self.base / 'private.nfo'
        outside.write_text('private')
        (self.media / 'link.nfo').symlink_to(outside)
        with self.assertRaises(UserError):
            self.store.write_nfo('link.nfo', '<movie/>', True)
        self.assertEqual(outside.read_text(), 'private')

    def test_series_ai_http_to_drafts_to_nfo(self):
        self.video('教程/S01E01-入门.mp4')
        self.video('教程/S01E02-进阶.mp4')
        (self.media / '教程/S01E01-入门.srt').write_text('1\n00:00:01,000 --> 00:00:03,000\n今天介绍变量。', 'utf-8')
        server = ThreadingHTTPServer(('127.0.0.1', 0), FakeAI)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            FakeAI.requests.clear()
            self.store.save_settings({'provider': 'compatible', 'base_url': 'http://127.0.0.1:%s/v1' % server.server_port, 'model': 'test-model', 'api_key': 'test-secret'})
            rows = self.scan('教程', 'episode')
            ids = [x['id'] for x in rows]
            job = self.wait_job('generate', ids)
            self.assertEqual(job['failed'], 0)
            self.assertEqual(len(FakeAI.requests), 3)  # One series overview, two episodes.
            self.assertEqual(FakeAI.requests[0][0], '/v1/chat/completions')
            self.assertEqual(FakeAI.requests[0][2], 'Bearer test-secret')
            self.assertIn('今天介绍变量', json.dumps(FakeAI.requests, ensure_ascii=False))
            self.assertFalse(list(self.media.rglob('*.nfo')))
            job = self.wait_job('save', ids)
            self.assertEqual(job['failed'], 0)
            self.assertEqual(len(list(self.media.rglob('*.nfo'))), 3)
            root = ET.parse(self.media / '教程/S01E01-入门.nfo')
            self.assertEqual(root.getroot().tag, 'episodedetails')
            self.assertEqual(root.findtext('episode'), '1')
            self.assertEqual(ET.parse(self.media / '教程/tvshow.nfo').getroot().tag, 'tvshow')
            self.assertNotIn('api_key', self.store.public_settings())
            self.assertEqual(os.stat(self.store.settings_file).st_mode & 0o777, 0o600)
            self.store.save_settings({'provider': 'ollama', 'base_url': 'http://127.0.0.1:%s' % server.server_port, 'model': 'test-model', 'clear_key': True})
            self.store.ai({'filename': 'Ollama测试'})
            self.assertEqual(FakeAI.requests[-1][0], '/api/chat')
            self.assertEqual(FakeAI.requests[-1][1]['format'], 'json')
            self.assertIsNone(FakeAI.requests[-1][2])
        finally:
            server.shutdown()
            server.server_close()

    def test_failed_ai_preserves_previous_draft(self):
        self.video('视频.mp4')
        row = self.draft(self.scan()[0])
        self.store.config.update(base_url='http://example.invalid', model='test')
        with patch.object(self.store, 'ai', side_effect=UserError('模型不可用')):
            job = self.wait_job('generate', [row['id']], True)
        self.assertEqual(job['failed'], 1)
        self.assertEqual(self.store.get_video(row['id'])['metadata']['title'], '标题 & <测试>')

    def test_cancellation_and_single_job(self):
        self.video('a.mp4')
        self.video('b.mp4')
        rows = self.scan()
        self.store.config.update(base_url='http://example.invalid', model='test')
        entered, release = threading.Event(), threading.Event()
        def slow_ai(context):
            entered.set()
            release.wait(3)
            return {'title': context['filename'], 'plot': '', 'genres': [], 'tags': [], 'notes': ''}
        with patch.object(self.store, 'ai', side_effect=slow_ai):
            job = self.store.start_job('generate', [x['id'] for x in rows])
            self.assertTrue(entered.wait(2))
            with self.assertRaises(UserError):
                self.store.start_job('generate', [rows[0]['id']])
            self.store.execute("UPDATE jobs SET status='cancelling' WHERE id=?", (job['id'],))
            release.set()
            for _ in range(100):
                result = self.store.rows('SELECT * FROM jobs WHERE id=?', (job['id'],))[0]
                if result['status'] == 'cancelled':
                    break
                time.sleep(.02)
            self.assertEqual(result['status'], 'cancelled')
            self.assertEqual(result['done'], 1)
            self.assertEqual(self.store.get_video(rows[1]['id'])['status'], 'new')

    def test_restart_marks_unfinished_jobs_and_keeps_draft(self):
        self.video('视频.mp4')
        row = self.draft(self.scan()[0])
        self.store.execute("INSERT INTO jobs(id,action,status,ids,total,created) VALUES ('x','generate','running','[]',1,0)")
        self.store.execute("UPDATE videos SET status='generating' WHERE id=?", (row['id'],))
        new_store = Store(self.media, self.base / 'data')
        try:
            self.assertEqual(new_store.rows("SELECT status FROM jobs WHERE id='x'")[0]['status'], 'interrupted')
            self.assertEqual(new_store.get_video(row['id'])['status'], 'error')
            self.assertIsNotNone(new_store.get_video(row['id'])['metadata'])
        finally:
            new_store.db.close()

    def test_http_auth_origin_and_static_files(self):
        server = Server(('127.0.0.1', 0), self.store, 'test-pass-123')
        threading.Thread(target=server.serve_forever, daemon=True).start()
        base = 'http://127.0.0.1:%s' % server.server_port
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(CookieJar()))
        def request(path, data=None, origin=None):
            headers = {'Content-Type': 'application/json'}
            if origin:
                headers['Origin'] = origin
            req = urllib.request.Request(base + path, data=None if data is None else json.dumps(data).encode(), headers=headers)
            return opener.open(req)
        try:
            with request('/') as r:
                self.assertIn('AI NFO', r.read().decode())
                self.assertIn("script-src 'self'", r.headers['Content-Security-Policy'])
            with self.assertRaises(urllib.error.HTTPError) as cm:
                request('/api/state')
            self.assertEqual(cm.exception.code, 401)
            request('/api/login', {'password': 'test-pass-123'}).close()
            with request('/api/state') as r:
                self.assertNotIn('api_key', json.load(r)['settings'])
            with self.assertRaises(urllib.error.HTTPError) as cm:
                request('/api/scan', {'path': ''}, 'https://evil.example')
            self.assertEqual(cm.exception.code, 403)
            with self.assertRaises(urllib.error.HTTPError) as cm:
                request('/api/directories?path=..%2Fprivate')
            self.assertEqual(cm.exception.code, 400)
            request('/api/logout', {}).close()
            with self.assertRaises(urllib.error.HTTPError):
                request('/api/state')
        finally:
            server.shutdown()
            server.server_close()


if __name__ == '__main__':
    unittest.main()
