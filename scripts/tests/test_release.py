import io
import json
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import package
import release

VERSION = 'v0.4.0-alpha'
COMMIT = 'a' * 40


def binaries(root, goos):
    root.mkdir(parents=True)
    data = bytearray(128)
    if goos == 'windows':
        data[:2] = b'MZ'
        struct.pack_into('<I', data, 0x3c, 64)
        data[64:68] = b'PE\0\0'
        struct.pack_into('<H', data, 68, 0x8664)
    elif goos == 'linux':
        data[:6] = b'\x7fELF\x02\x01'
        struct.pack_into('<H', data, 18, 62)
    else:
        data[:4] = b'\xcf\xfa\xed\xfe'
        struct.pack_into('<I', data, 4, 0x0100000C)
    for name in package.binary_names(goos):
        (root / name).write_bytes(data)
    return root


def fixture(root):
    source = root / 'native'
    for goos, arch in package.TARGETS:
        target = source / goos
        data = package.make_package(binaries(root / 'binaries' / goos, goos), VERSION, COMMIT,
                                    goos, arch, 'release', 'go1.24.0')
        target.mkdir(parents=True)
        name = f'pt-cli-{VERSION}-{goos}-{arch}.zip'
        (target / name).write_bytes(data)
        (target / 'SHA256SUMS').write_bytes(f'{package.sha256(data)}  {name}\n'.encode())
    return source


class VersionAndTagTests(unittest.TestCase):
    def test_versions_fail_closed(self):
        for value in ('v1.2.3', VERSION, 'v1.2.3-rc.1'):
            self.assertEqual(release.release_version(value), value)
        for value in ('1.2.3', 'v01.2.3', 'v1.2.3-01', 'v1.2.3+meta', 'v1.2.3\n', 'v1.2.3;echo x'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                release.release_version(value)
        for content in ('v1.2.3', 'v1.2.3\r\n', 'v1.2.3\n\n'):
            with self.assertRaises(ValueError):
                release.version_file(content)
        with self.assertRaises(ValueError):
            release.identity(VERSION + '+g' + 'b' * 12, COMMIT, 'ci-preview')

    def test_real_git_tag_binding(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            def git(*args):
                return subprocess.check_output(['git', '-c', 'commit.gpgsign=false', '-c', 'tag.gpgsign=false',
                                                '-c', 'core.hooksPath=' + str(root / 'empty-hooks'), *args],
                                               cwd=root, stderr=subprocess.DEVNULL, text=True).strip()
            git('init', '--initial-branch=main')
            git('config', 'user.name', 'Synthetic release test')
            git('config', 'user.email', 'release-test@example.invalid')
            git('config', 'core.autocrlf', 'false')
            (root / 'VERSION').write_bytes((VERSION + '\n').encode())
            git('add', 'VERSION')
            git('commit', '-m', 'synthetic release version')
            commit = git('rev-parse', 'HEAD')
            git('update-ref', 'refs/remotes/origin/main', commit)
            git('tag', '-a', VERSION, '-m', 'synthetic annotated tag')
            self.assertEqual(release.check_tag(VERSION, commit, root)['commit'], commit)
            with self.assertRaises(ValueError):
                release.check_tag(VERSION, 'b' * 40, root)
            git('tag', 'v9.0.0')
            with self.assertRaises(ValueError):
                release.check_tag('v9.0.0', cwd=root)
            with self.assertRaises(subprocess.CalledProcessError):
                release.check_tag('v9.0.1', cwd=root)
            (root / 'VERSION').write_bytes(b'v1.0.0\n')
            git('commit', '-am', 'unmerged version')
            git('tag', 'v1.0.0')
            with self.assertRaises(subprocess.CalledProcessError):
                release.check_tag('v1.0.0', cwd=root)
            (root / '.github/workflows').mkdir(parents=True)
            (root / '.github/workflows/change.yml').write_bytes(b'name: changed\n')
            git('add', '.github')
            git('commit', '-m', 'workflow changes on main')
            git('update-ref', 'refs/remotes/origin/main', 'HEAD')
            with self.assertRaises(subprocess.CalledProcessError):
                release.check_tag(VERSION, cwd=root)


class PackageTests(unittest.TestCase):
    def test_deterministic_zip_and_modes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            a = binaries(root / 'first', 'linux')
            b = binaries(root / 'second', 'linux')
            for p in b.iterdir():
                os.utime(p, (1700000000, 1700000000))
            options = (VERSION, COMMIT, 'linux', 'amd64', 'release', 'go1.24.0')
            first = package.make_package(a, *options)
            self.assertEqual(first, package.make_package(b, *options))
            with zipfile.ZipFile(io.BytesIO(first)) as archive:
                self.assertEqual(archive.namelist(), sorted(archive.namelist()))
                self.assertTrue(all(x.date_time == (1980, 1, 1, 0, 0, 0) for x in archive.infolist()))
                self.assertEqual(archive.getinfo('pt').external_attr >> 16 & 0o777, 0o755)
                entries = {n: archive.read(n) for n in archive.namelist()}
            package.inspect_package(first, *options[:5])
            entries['examples/readonly/demo.txt'] = b'corrupted'
            with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
                package.inspect_package(package.canonical_zip(entries, ('pt', 'ptctl')), *options[:5])
            (a / 'pt').write_bytes(b'wrong architecture')
            with self.assertRaisesRegex(ValueError, 'architecture'):
                package.make_package(a, *options)

    def test_complete_assembly_and_corruption(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = fixture(root)
            output = root / 'release'
            release.assemble(source, output, VERSION, COMMIT, 'release')
            self.assertEqual(len(release.verify_assets(output, VERSION, COMMIT)), 5)
            with self.assertRaises(ValueError):
                release.verify_assets(output, VERSION, 'b' * 40)
            (output / 'SHA256SUMS').write_bytes(b'corrupt')
            with self.assertRaisesRegex(ValueError, 'SHA256SUMS'):
                release.verify_assets(output, VERSION, COMMIT)
            next(source.rglob('*.zip')).unlink()
            with self.assertRaisesRegex(ValueError, 'missing'):
                release.assemble(source, root / 'incomplete', VERSION, COMMIT, 'release')
            self.assertFalse((root / 'incomplete').exists())


class DraftTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.assets = root / 'release'
        release.assemble(fixture(root), self.assets, VERSION, COMMIT, 'release')
        self.calls = []
        self.uploaded = []

    def api(self, endpoint, body=None, upload=None):
        self.calls.append((endpoint, body, upload))
        if '?per_page=' in endpoint:
            return []
        if body is not None:
            return {'id': 7, 'draft': True}
        if upload is not None:
            asset = {'name': upload.name, 'state': 'uploaded', 'digest': 'sha256:' + package.sha256(upload.read_bytes())}
            self.uploaded.append(asset)
            return asset
        return {'draft': True, 'tag_name': VERSION, 'assets': self.uploaded, 'html_url': 'https://github.com/example/repo/releases/tag/' + VERSION}

    def create(self):
        return release.create_draft('example/repo', VERSION, COMMIT, self.assets)

    def test_draft_only_and_exact_uploaded_digests(self):
        with patch.object(release, 'remote_tag', return_value=COMMIT), patch.object(release, 'gh_json', side_effect=self.api):
            self.assertTrue(self.create()['draft'])
        writes = [body for _, body, _ in self.calls if body is not None]
        self.assertEqual(len(writes), 1)
        self.assertIs(writes[0]['draft'], True)
        self.assertIs(writes[0]['prerelease'], True)
        self.assertEqual(writes[0]['make_latest'], 'false')
        self.assertEqual(len(self.uploaded), 5)

    def test_existing_release_never_modified(self):
        for is_draft in (False, True):
            with patch.object(release, 'remote_tag', return_value=COMMIT), patch.object(release, 'gh_json', return_value=[{'tag_name': VERSION, 'draft': is_draft}]) as api:
                with self.assertRaisesRegex(ValueError, 'already exists'):
                    self.create()
                self.assertEqual(api.call_count, 1)
                self.assertIn('/releases?per_page=100&page=1', api.call_args.args[0])

    def test_existing_draft_on_later_page(self):
        old = [{'tag_name': 'v0.0.' + str(i)} for i in range(100)]
        draft = {'tag_name': VERSION, 'draft': True}
        with patch.object(release, 'gh_json', side_effect=[old, [draft]]) as api:
            self.assertEqual(release.existing_release('example/repo', VERSION), draft)
            self.assertEqual(api.call_count, 2)

    def test_annotated_remote_tag(self):
        responses = [{'object': {'type': 'tag', 'sha': 'b' * 40}},
                     {'object': {'type': 'commit', 'sha': COMMIT}}]
        with patch.object(release, 'gh_json', side_effect=responses):
            self.assertEqual(release.remote_tag('example/repo', VERSION), COMMIT)

    def test_bad_assets_and_moved_tag_precede_writes(self):
        with patch.object(release, 'remote_tag', return_value='b' * 40), patch.object(release, 'gh_json') as api:
            with self.assertRaisesRegex(ValueError, 'moved'):
                self.create()
            api.assert_not_called()
        (self.assets / 'SHA256SUMS').unlink()
        with patch.object(release, 'gh_json') as api, patch.object(release, 'remote_tag') as tag:
            with self.assertRaises(ValueError):
                self.create()
            api.assert_not_called()
            tag.assert_not_called()

    def test_partial_upload_does_not_publish_or_retry(self):
        def fail(endpoint, **kwargs):
            if kwargs.get('upload') and self.uploaded:
                raise TimeoutError('synthetic upload interruption')
            return self.api(endpoint, **kwargs)
        with patch.object(release, 'remote_tag', return_value=COMMIT), patch.object(release, 'gh_json', side_effect=fail):
            with self.assertRaises(TimeoutError):
                self.create()
        self.assertEqual(len(self.uploaded), 1)
        self.assertEqual([body['draft'] for _, body, _ in self.calls if body], [True])

    def test_gh_errors_are_not_all_treated_as_absent(self):
        for status in (401, 403, 404, 500):
            result = subprocess.CompletedProcess([], 1, '', f'gh: failure (HTTP {status})')
            with patch.object(release.subprocess, 'run', return_value=result), self.assertRaises(ValueError):
                release.gh_json('repos/example/repo/releases?per_page=100&page=1')

    def test_tag_moves_after_upload_remains_draft(self):
        with patch.object(release, 'remote_tag', side_effect=[COMMIT, 'b' * 40]), patch.object(release, 'gh_json', side_effect=self.api):
            with self.assertRaisesRegex(ValueError, 'moved during upload'):
                self.create()
        self.assertEqual([body['draft'] for _, body, _ in self.calls if body], [True])


if __name__ == '__main__':
    unittest.main()
