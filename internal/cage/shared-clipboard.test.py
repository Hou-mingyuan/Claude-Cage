import importlib.util
import io
import json
import multiprocessing
import os
from pathlib import Path
import tarfile
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location('shared_clipboard', Path(__file__).with_name('shared-clipboard.py'))
clipboard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(clipboard)


class DesktopSelectionTests(unittest.TestCase):
    def test_scoped_bridge_joins_mount_namespace_in_parent_and_keeps_worker_scoped(self):
        with patch.object(clipboard, 'desktop_display', return_value=(':0', '/run/test/Xauthority')), \
             patch.object(clipboard, 'enter_desktop_mount_namespace', return_value=123) as enter_mnt, \
             patch.object(clipboard.os, 'open', return_value=11) as open_ns, \
             patch.object(clipboard.os, 'close') as close, \
             patch.object(clipboard, 'bridge') as bridge:
            clipboard.bridge_desktop('a' * 24)
            enter_mnt.assert_called_once_with('a' * 24)
            open_ns.assert_called_once_with('/proc/123/ns/net', clipboard.os.O_RDONLY)
            self.assertEqual(bridge.call_args.kwargs['displays'], ((':0', 'cage'),))
            self.assertEqual(bridge.call_args.kwargs['authorities'], {':0': '/run/test/Xauthority'})
            self.assertEqual(bridge.call_args.kwargs['namespace_fds'], (11,))
            close.assert_called_once_with(11)
            display_exists = bridge.call_args.kwargs['display_exists']
            with patch.object(clipboard.Path, 'is_dir', return_value=True):
                self.assertTrue(display_exists(':0'))
            with patch.object(clipboard.Path, 'is_dir', return_value=False):
                self.assertFalse(display_exists(':0'))

    def test_enter_desktop_mount_namespace_validates_id_and_resets_tempdir(self):
        with self.assertRaises(ValueError):
            clipboard.enter_desktop_mount_namespace('../bad')
        previous_tempdir = clipboard.tempfile.tempdir
        clipboard.tempfile.tempdir = '/tmp/stale'
        try:
            with patch.object(clipboard.subprocess, 'run') as command, \
                 patch.object(clipboard.os, 'open', return_value=17) as open_ns, \
                 patch.object(clipboard.os, 'setns', create=True) as setns, \
                 patch.object(clipboard.os, 'close') as close:
                command.return_value.stdout = '456\n'
                self.assertEqual(clipboard.enter_desktop_mount_namespace('c' * 24), 456)
                open_ns.assert_called_once_with('/proc/456/ns/mnt', clipboard.os.O_RDONLY)
                setns.assert_called_once_with(17, 0)
                close.assert_called_once_with(17)
                self.assertIsNone(clipboard.tempfile.tempdir)
        finally:
            clipboard.tempfile.tempdir = previous_tempdir

    def test_import_and_export_cli_enter_desktop_mount_namespace(self):
        instance = 'd' * 24
        with patch.object(clipboard, 'enter_desktop_mount_namespace') as enter_mnt, \
             patch.object(clipboard, 'import_files', return_value=['/tmp/cage-clipboard-1/a.txt']) as do_import, \
             patch('builtins.print') as printed:
            clipboard.main(['--import', '--session', 'sess-1', '--desktop', instance])
            enter_mnt.assert_called_once_with(instance)
            do_import.assert_called_once_with(clipboard.sys.stdin.buffer, 'sess-1')
            printed.assert_called_once_with(json.dumps({'paths': ['/tmp/cage-clipboard-1/a.txt']}))

        fake_stdin = type('FakeStdin', (), {'buffer': io.BytesIO(json.dumps(['/tmp/a.txt']).encode())})()
        fake_stdout = type('FakeStdout', (), {'buffer': io.BytesIO()})()
        fake_account = type('FakePwd', (), {'pw_name': 'cage', 'pw_uid': 1000, 'pw_gid': 1000})()
        with patch.object(clipboard, 'enter_desktop_mount_namespace') as enter_mnt, \
             patch.object(clipboard.sys, 'stdin', fake_stdin), \
             patch.object(clipboard.sys, 'stdout', fake_stdout), \
             patch.object(clipboard, 'CacheRegistry') as registry_cls, \
             patch('pwd.getpwnam', return_value=fake_account), \
             patch.object(clipboard.os, 'initgroups'), \
             patch.object(clipboard.os, 'setgid'), \
             patch.object(clipboard.os, 'setuid'), \
             patch.object(clipboard, 'export_files') as do_export:
            clipboard.main(['--export-user', 'cage', '--paths-stdin', '--desktop', instance])
            enter_mnt.assert_called_once_with(instance)
            registry_cls.return_value.reading.assert_called_once_with(['/tmp/a.txt'])
            do_export.assert_called_once_with(['/tmp/a.txt'], fake_stdout.buffer)

        with self.assertRaises(ValueError):
            clipboard.main(['--import', '--unknown'])

    def test_only_requested_display_is_selected(self):
        with tempfile.TemporaryDirectory() as base:
            instance = 'a' * 24
            root = Path(base) / ('desktop-' + instance)
            root.mkdir()
            (root / 'display').write_text('0\n')
            (root / 'Xauthority').write_bytes(b'test')
            self.assertEqual(clipboard.desktop_display(instance, Path(base)), (':0', str(root / 'Xauthority')))

    def test_invalid_instance_and_display_are_rejected(self):
        with self.assertRaises(ValueError):
            clipboard.desktop_display('../cage-desktop')
        with tempfile.TemporaryDirectory() as base:
            root = Path(base) / ('desktop-' + 'b' * 24)
            root.mkdir()
            (root / 'display').write_text('remote:0')
            (root / 'Xauthority').write_bytes(b'test')
            with self.assertRaises(ValueError):
                clipboard.desktop_display('b' * 24, Path(base))


class ContentChoiceTests(unittest.TestCase):
    def test_a_rich_text_copy_with_a_rendered_picture_is_text(self):
        self.assertTrue(clipboard.copies_rich_text(['TARGETS', 'text/rtf', 'UTF8_STRING', 'image/png']))
        self.assertTrue(clipboard.copies_rich_text(['application/rtf', 'text/plain;charset=utf-8', 'image/bmp']))

    def test_pictures_and_web_images_remain_pictures(self):
        self.assertFalse(clipboard.copies_rich_text(['TARGETS', 'image/png', 'text/html', 'UTF8_STRING']))
        self.assertFalse(clipboard.copies_rich_text(['image/png', 'text/rtf']))
        self.assertFalse(clipboard.copies_rich_text([]))

    def test_pixel_signature_ignores_row_padding_and_detects_changes(self):
        packed = clipboard.pixel_signature(2, 2, 6, 3, bytes(range(12)))
        padded = clipboard.pixel_signature(2, 2, 8, 3, bytes(range(6)) + b'\xff\xff' + bytes(range(6, 12)))
        self.assertEqual(packed, padded)
        self.assertNotEqual(packed, clipboard.pixel_signature(2, 2, 6, 3, bytes(12)))
        self.assertNotEqual(packed, clipboard.pixel_signature(1, 4, 3, 3, bytes(range(12))))


class CacheOwnershipTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.registry = clipboard.CacheRegistry(self.root / 'receipts')
        self.cache = self.root / 'created'
        self.cache.mkdir()
        self.file = self.cache / 'fixture.txt'
        self.file.write_bytes(b'complete copied bytes')
        self.paths = [str(self.file)]
        self.registry.register(self.paths, 'current-session')

    def test_current_clipboard_and_reader_references_retain_retired_files(self):
        self.registry.retire(self.paths)
        self.registry.collect(self.paths, 'current-session')
        self.assertTrue(self.file.exists())
        with self.registry.reading(self.paths):
            self.registry.collect([], 'current-session')
            self.assertEqual(self.file.read_bytes(), b'complete copied bytes')
        self.registry.collect([], 'current-session')
        self.assertFalse(self.cache.exists())
        self.assertEqual(self.registry.records(), [])

    def test_delayed_external_path_requires_recopy_but_open_reader_finishes(self):
        self.registry.retire(self.paths)
        with self.file.open('rb') as reader:
            self.registry.collect([], 'current-session')
            self.assertEqual(reader.read(), b'complete copied bytes')
        self.assertFalse(self.file.exists())

    def test_only_created_cache_is_collected(self):
        unrelated = self.root / 'cage-clipboard-qa-test-unrelated'
        unrelated.mkdir()
        (unrelated / 'keep.txt').write_text('user content')
        self.registry.collect([], 'new-session')
        self.assertFalse(self.cache.exists())
        self.assertEqual((unrelated / 'keep.txt').read_text(), 'user content')

    def test_active_and_legacy_unpublished_selections_are_retained(self):
        self.registry.collect([], 'current-session')
        self.assertTrue(self.file.exists())
        record = self.registry.records()[0]
        record['session'] = ''
        self.registry.save(record)
        self.registry.collect([], 'new-session')
        self.assertTrue(self.file.exists())
        self.registry.retire(self.paths)
        self.registry.collect([], 'new-session')
        self.assertFalse(self.cache.exists())

    def test_changed_creation_marker_blocks_deletion(self):
        record = self.registry.records()[0]
        (self.cache / record['marker']).write_text('changed owner')
        self.registry.retire(self.paths)
        with self.assertRaises(ValueError):
            self.registry.collect([], 'current-session')
        self.assertEqual(self.file.read_bytes(), b'complete copied bytes')

    def test_directory_removal_failure_retains_proof_for_retry(self):
        self.registry.retire(self.paths)
        with patch.object(Path, 'rmdir', side_effect=PermissionError('simulated busy directory')):
            with self.assertRaises(PermissionError):
                self.registry.collect([], 'current-session')
        self.registry.collect([], 'current-session')
        self.assertFalse(self.cache.exists())

    def test_malformed_receipt_and_symlink_registry_fail_without_removing_files(self):
        record = self.registry.records()[0]
        path = self.registry.directory / (record['id'] + '.json')
        path.write_text(json.dumps({'id': None}))
        with self.assertRaises(ValueError):
            self.registry.records()
        self.assertTrue(self.file.exists())
        alias = self.root / 'alias'
        alias.symlink_to(self.registry.directory, target_is_directory=True)
        with self.assertRaises(ValueError):
            clipboard.CacheRegistry(alias).records()

    def test_import_registers_exact_owned_directory_and_keeps_original_bytes(self):
        source = io.BytesIO()
        clipboard.export_files(self.paths, source)
        source.seek(0)
        with patch.object(clipboard.tempfile, 'tempdir', str(self.root)):
            registry = clipboard.CacheRegistry()
            registry.begin_session('import-session')
            paths = clipboard.import_files(source, 'import-session')
            self.assertEqual(registry.records()[0]['paths'], paths)
            registry.retire(paths)
            registry.collect([], 'import-session')
        self.assertEqual(self.file.read_bytes(), b'complete copied bytes')
        self.assertFalse(Path(paths[0]).parent.exists())

    def test_failed_archive_is_removed_before_it_can_be_published(self):
        source = io.BytesIO()
        with tarfile.open(fileobj=source, mode='w') as archive:
            entry = tarfile.TarInfo('../outside')
            entry.size = 1
            archive.addfile(entry, io.BytesIO(b'x'))
        source.seek(0)
        with patch.object(clipboard.tempfile, 'tempdir', str(self.root)):
            clipboard.CacheRegistry().ensure_directory()
            before = set(self.root.iterdir())
            with self.assertRaises(ValueError):
                clipboard.import_files(source)
        self.assertEqual(set(self.root.iterdir()), before)

    def test_an_import_finishing_after_disconnect_releases_its_created_directory(self):
        source = io.BytesIO()
        clipboard.export_files(self.paths, source)
        source.seek(0)
        with patch.object(clipboard.tempfile, 'tempdir', str(self.root)):
            registry = clipboard.CacheRegistry()
            registry.begin_session('import-session')
            original = clipboard.extract_files
            created = []
            def disconnected(stream, root):
                created.append(root)
                paths = original(stream, root)
                registry.end_session('import-session')
                return paths
            with patch.object(clipboard, 'extract_files', side_effect=disconnected):
                with self.assertRaises(ValueError):
                    clipboard.import_files(source, 'import-session')
            self.assertFalse(created[0].exists())
            registry.collect([], 'next-session')
            self.assertEqual(registry.records(), [])
        self.assertEqual(self.file.read_bytes(), b'complete copied bytes')

    def test_an_incomplete_copy_keeps_a_live_producer_and_reclaims_a_terminated_one(self):
        record = self.registry.records()[0]
        record['pending'] = True
        self.registry.save(record)
        self.registry.collect([], 'next-session')
        self.assertTrue(self.cache.exists())
        record['producer'] = 2147483647
        self.registry.save(record)
        self.registry.collect([], 'next-session')
        self.assertFalse(self.cache.exists())


class BridgeEventLoopTests(unittest.TestCase):
    def test_wait_timeout_avoids_50ms_polling_when_idle(self):
        self.assertEqual(clipboard.bridge_wait_timeout(2, 2, True, False, 0.0, 10.0), 0.0)
        self.assertEqual(clipboard.bridge_wait_timeout(2, 2, False, False, 0.0, 10.0), 10.0)
        self.assertEqual(clipboard.bridge_wait_timeout(1, 2, False, False, 0.0, 10.0), 1.0)
        self.assertAlmostEqual(clipboard.bridge_wait_timeout(2, 2, False, True, 10.4, 10.0), 0.4, places=3)

    def test_event_driven_bridge_roundtrips_text_png_files_and_worker_eof(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            registry = clipboard.CacheRegistry(root / 'receipts')
            sample_dir = root / 'sample'
            sample_dir.mkdir()
            sample_file = sample_dir / 'hello.txt'
            sample_file.write_bytes(b'clipboard file bytes')
            sample_file_2 = sample_dir / 'guest.txt'
            sample_file_2.write_bytes(b'guest clipboard file bytes')
            registry.register([str(sample_file)], 'initial')

            stdin_r, stdin_w = os.pipe()
            stdout_r, stdout_w = os.pipe()
            stdout_file = os.fdopen(stdout_w, 'w', buffering=1)
            stdout_reader = os.fdopen(stdout_r, 'r', buffering=1)
            worker_ends = {}
            alive = {':77': True}
            display_up = {':77': True}

            class FakeProcess:
                def __init__(self, name):
                    self.name = name
                def is_alive(self):
                    return alive[self.name]
                def join(self, timeout=None):
                    pass
                def terminate(self):
                    alive[self.name] = False
                def kill(self):
                    alive[self.name] = False

            def spawn(name, _user):
                parent, child = multiprocessing.Pipe()
                worker_ends[name] = child
                return FakeProcess(name), parent

            def next_output(timeout=1.0):
                result = []
                def _read():
                    line = stdout_reader.readline()
                    if line:
                        result.append(json.loads(line))
                t = threading.Thread(target=_read, daemon=True)
                t.start()
                t.join(timeout)
                self.assertTrue(result, 'timed out waiting for bridge output')
                return result[0]

            thread = threading.Thread(
                target=clipboard.bridge,
                kwargs={
                    'displays': ((':77', 'cage-browser'),),
                    'stdin_fd': stdin_r,
                    'stdout': stdout_file,
                    'registry': registry,
                    'spawn_worker': spawn,
                    'display_exists': lambda name: display_up.get(name, False),
                    'materialize': lambda paths, _user, _session: paths,
                },
                daemon=True,
            )
            thread.start()
            try:
                hello = next_output()
                self.assertTrue(hello['hello']['available'])
                child = worker_ends[':77']

                # Text roundtrip: host -> worker -> ack, then worker -> host
                t0 = time.monotonic()
                os.write(stdin_w, (json.dumps({'text': 'host-text', 'serial': 1}) + '\n').encode())
                msg = child.recv()
                self.assertEqual(msg, {'text': 'host-text', 'serial': 1})
                child.send({'ack': 1})
                self.assertEqual(next_output(), {'ack': 1})
                child.send({'text': 'guest-text'})
                self.assertEqual(next_output(), {'text': 'guest-text'})
                self.assertLess(time.monotonic() - t0, 0.5)

                # Image (PNG) roundtrip
                t0 = time.monotonic()
                os.write(stdin_w, (json.dumps({'png': 'iVBORw0KGgo=', 'serial': 2}) + '\n').encode())
                self.assertEqual(child.recv(), {'png': 'iVBORw0KGgo=', 'serial': 2})
                child.send({'ack': 2})
                self.assertEqual(next_output(), {'ack': 2})
                child.send({'png': 'R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7'})
                self.assertEqual(next_output()['png'], 'R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7')
                self.assertLess(time.monotonic() - t0, 0.5)

                # File selection roundtrip
                t0 = time.monotonic()
                os.write(stdin_w, (json.dumps({'files': [str(sample_file)], 'serial': 3}) + '\n').encode())
                self.assertTrue(child.poll(1.0))
                self.assertEqual(child.recv(), {'files': [str(sample_file)], 'serial': 3})
                child.send({'ack': 3})
                self.assertEqual(next_output(), {'ack': 3})
                child.send({'clipboard_files': []})
                child.send({'files': [str(sample_file_2)], 'source': 'cage-browser'})
                self.assertTrue(child.poll(1.0))
                reseed = child.recv()
                self.assertEqual(reseed['files'], [str(sample_file_2)])
                child.send({'ack': reseed['serial']})
                published = next_output()
                self.assertEqual(published['files'], [str(sample_file_2)])
                self.assertEqual(published['source'], 'cage')
                self.assertLess(time.monotonic() - t0, 0.5)

                # Worker pipe EOF wakes bridge immediately and announces unavailable
                t0 = time.monotonic()
                display_up[':77'] = False
                alive[':77'] = False
                child.close()
                down = next_output()
                self.assertFalse(down['hello']['available'])
                self.assertLess(time.monotonic() - t0, 0.5)
            finally:
                os.close(stdin_w)
                thread.join(timeout=2)
                self.assertFalse(thread.is_alive())
                os.close(stdin_r)
                stdout_file.close()
                stdout_reader.close()


if __name__ == '__main__':
    unittest.main()
