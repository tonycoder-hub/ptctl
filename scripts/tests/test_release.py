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


class VersionTests(unittest.TestCase):
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

    def test_semver_precedence(self):
        ordered = ['v0.1.0', 'v0.2.0-alpha', 'v0.2.0-alpha.1', 'v0.2.0-alpha.2', 'v0.2.0-alpha.10',
                   'v0.2.0-alpha.beta', 'v0.2.0-beta', 'v0.2.0-beta.2', 'v0.2.0-beta.11', 'v0.2.0-rc.1',
                   'v0.2.0', 'v0.2.1', 'v0.10.0', 'v1.0.0']
        self.assertEqual(sorted(reversed(ordered), key=release.version_key), ordered)
        with self.assertRaises(ValueError):
            release.version_key('0.2.0')

    def test_release_notes_take_exactly_one_section(self):
        changelog = ('# Changelog\n\n## v0.4.0-alpha.10\n\n- later\n\n'
                     '## v0.4.0-alpha.1 - 2026-10-17\n\n- one\n\n### Known limits\n\n- two\n\n'
                     '## v0.2.0-alpha.1\n\n- old\n')
        self.assertEqual(release.release_notes(changelog, 'v0.4.0-alpha.1'), '- one\n\n### Known limits\n\n- two\n')
        self.assertEqual(release.release_notes(changelog, 'v0.2.0-alpha.1'), '- old\n')
        for text, message in (('## v0.4.0-alpha.10\n\n- later\n', 'exactly one'),
                              ('## v0.4.0-alpha.1\n- a\n## v0.4.0-alpha.1\n- b\n', 'exactly one'),
                              ('## v0.4.0-alpha.1x\n- a\n', 'exactly one'),
                              ('## v0.4.0-alpha.1\n\n\n## v0.2.0\n- a\n', 'empty')):
            with self.subTest(text=text), self.assertRaisesRegex(ValueError, message):
                release.release_notes(text, 'v0.4.0-alpha.1')


class PlanTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git('init', '--initial-branch=main')
        self.git('config', 'user.name', 'Synthetic release test')
        self.git('config', 'user.email', 'release-test@example.invalid')
        self.git('config', 'core.autocrlf', 'false')

    def git(self, *args):
        return subprocess.check_output(['git', '-c', 'commit.gpgsign=false', '-c', 'tag.gpgsign=false',
                                        '-c', 'core.hooksPath=' + str(self.root / 'empty-hooks'), *args],
                                       cwd=self.root, stderr=subprocess.DEVNULL, text=True).strip()

    def commit(self, version, notes=None, **files):
        (self.root / 'VERSION').write_bytes((version + '\n').encode())
        changelog = '# Changelog\n' + ''.join(f'\n## {v}\n\n- {text}\n' for v, text in (notes or {}).items())
        (self.root / 'CHANGELOG.md').write_bytes(changelog.encode())
        for name, content in files.items():
            path = self.root / name.replace('__', '/')
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
        self.git('add', '-A')
        self.git('commit', '-m', 'synthetic ' + version)
        commit = self.git('rev-parse', 'HEAD')
        self.git('update-ref', 'refs/remotes/origin/main', commit)
        return commit

    def plan(self, commit):
        return release.plan(commit, self.root)

    def test_only_a_new_version_releases(self):
        first = self.commit('v0.2.0-alpha.1', {'v0.2.0-alpha.1': 'first'})
        self.git('tag', '-a', 'v0.2.0-alpha.1', '-m', 'synthetic annotated tag')
        self.git('tag', 'vnext')  # Not a release version; ignored.
        same = self.commit('v0.2.0-alpha.1', {'v0.2.0-alpha.1': 'first'}, README=b'docs only\n')
        self.assertEqual(self.plan(same), {'commit': same, 'version': 'v0.2.0-alpha.1', 'channel': 'release', 'release': 'false'})
        # A rerun on the tagged commit itself finishes that release.
        self.assertEqual(self.plan(first)['release'], 'true')
        bumped = self.commit('v0.4.0-alpha.1', {'v0.2.0-alpha.1': 'first'})
        with self.assertRaisesRegex(ValueError, 'CHANGELOG.md needs exactly one section'):
            self.plan(bumped)
        noted = self.commit('v0.4.0-alpha.1', {'v0.4.0-alpha.1': 'second', 'v0.2.0-alpha.1': 'first'})
        self.assertEqual(self.plan(noted), {'commit': noted, 'version': 'v0.4.0-alpha.1', 'channel': 'release', 'release': 'true'})
        older = self.commit('v0.2.0-alpha.0', {'v0.2.0-alpha.0': 'older'})
        with self.assertRaisesRegex(ValueError, 'newer than every release tag'):
            self.plan(older)
        with self.assertRaisesRegex(ValueError, 'full source commit'):
            self.plan(noted[:12])

    def test_off_main_or_changed_workflows_fail_closed(self):
        base = self.commit('v0.4.0-alpha.1', {'v0.4.0-alpha.1': 'notes'})
        branch = self.commit('v0.4.0-alpha.2', {'v0.4.0-alpha.2': 'notes'})
        self.git('update-ref', 'refs/remotes/origin/main', base)
        with self.assertRaises(subprocess.CalledProcessError):
            self.plan(branch)
        self.git('reset', '--hard', base)
        self.commit('v0.4.0-alpha.1', {'v0.4.0-alpha.1': 'notes'}, **{'.github__workflows__change.yml': b'name: changed\n'})
        with self.assertRaises(subprocess.CalledProcessError):
            self.plan(base)


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


class FakeGitHub:
    """Hold one repository's tags and releases, and record every request."""

    def __init__(self, uploaded):
        self.refs = {}  # tag -> (object type, sha)
        self.tag_objects = {}  # tag object sha -> commit
        self.releases = []
        self.uploaded = uploaded
        self.calls = []
        self.on_upload = None

    def tag_at(self, tag, commit):
        self.tag_objects['c' * 40] = commit
        self.refs[tag] = ('tag', 'c' * 40)

    def writes(self):
        return [(method, endpoint, body) for endpoint, body, upload, method in self.calls
                if body is not None or upload is not None]

    def __call__(self, endpoint, body=None, upload=None, method='POST'):
        self.calls.append((endpoint, body, upload, method if body is not None or upload is not None else 'GET'))
        prefix = 'repos/example/repo/'
        if upload is not None:
            asset = {'name': upload.name, 'state': 'uploaded', 'digest': 'sha256:' + package.sha256(upload.read_bytes())}
            self.uploaded.append(asset)
            if self.on_upload:
                self.on_upload()
            return asset
        path = endpoint[len(prefix):]
        if path.startswith('releases?per_page='):
            return list(self.releases)
        if path.startswith('git/matching-refs/tags/'):
            name = path.rsplit('/', 1)[1]
            return [{'ref': 'refs/tags/' + tag} for tag in self.refs if tag.startswith(name)]
        if path.startswith('git/ref/tags/'):
            kind, sha = self.refs[path.rsplit('/', 1)[1]]
            return {'object': {'type': kind, 'sha': sha}}
        if path == 'git/tags':
            self.tag_objects['d' * 40] = body['object']
            return {'sha': 'd' * 40, 'tag': body['tag'], 'object': {'sha': body['object'], 'type': 'commit'}}
        if path.startswith('git/tags/'):
            return {'object': {'type': 'commit', 'sha': self.tag_objects[path.rsplit('/', 1)[1]]}}
        if path == 'git/refs':
            self.refs[body['ref'].removeprefix('refs/tags/')] = ('tag', body['sha'])
            return {'ref': body['ref'], 'object': {'type': 'tag', 'sha': body['sha']}}
        if path == 'releases':
            self.releases.append({'id': 7, 'draft': True, 'tag_name': body['tag_name'], 'prerelease': body['prerelease']})
            return dict(self.releases[-1])
        if path != 'releases/7':
            raise AssertionError('unexpected request ' + endpoint)
        current = self.releases[0]
        if method == 'PATCH' and body is not None:
            current['draft'] = body['draft']
        return dict(current, assets=list(self.uploaded), html_url='https://github.com/example/repo/releases/tag/' + VERSION)


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.assets = root / 'release'
        release.assemble(fixture(root), self.assets, VERSION, COMMIT, 'release')
        self.uploaded = []
        self.github = FakeGitHub(self.uploaded)
        main_line = patch.object(release, 'main_line')
        self.main_line = main_line.start()
        self.addCleanup(main_line.stop)

    def publish(self):
        with patch.object(release, 'gh_json', side_effect=self.github):
            return release.publish('example/repo', VERSION, COMMIT, self.assets, '- synthetic notes\n')

    def test_tags_then_drafts_and_publishes_last(self):
        # A longer tag sharing the prefix is not this release's tag.
        self.github.refs[VERSION + '.1'] = ('commit', 'b' * 40)
        result = self.publish()
        self.assertIs(result['draft'], False)
        writes = self.github.writes()
        self.assertEqual([(method, endpoint) for method, endpoint, _ in writes[:3]],
                         [('POST', 'repos/example/repo/git/tags'), ('POST', 'repos/example/repo/git/refs'),
                          ('POST', 'repos/example/repo/releases')])
        self.assertEqual(writes[0][2], {'tag': VERSION, 'message': 'pt cli ' + VERSION, 'object': COMMIT, 'type': 'commit'})
        draft = writes[2][2]
        self.assertIs(draft['draft'], True)
        self.assertIs(draft['prerelease'], True)
        self.assertEqual(draft['make_latest'], 'false')
        self.assertTrue(draft['body'].startswith('- synthetic notes\n\n---\n\n'))
        self.assertIn(COMMIT, draft['body'])
        self.assertEqual(len(writes), 9)
        self.assertEqual(len(self.uploaded), 5)
        self.assertEqual(writes[-1], ('PATCH', 'repos/example/repo/releases/7', {'draft': False, 'make_latest': 'false'}))
        self.main_line.assert_called_once()

    def test_reuses_a_tag_already_on_the_commit(self):
        self.github.tag_at(VERSION, COMMIT)
        self.assertIs(self.publish()['draft'], False)
        endpoints = [endpoint for _, endpoint, _ in self.github.writes()]
        self.assertNotIn('repos/example/repo/git/tags', endpoints)
        self.assertNotIn('repos/example/repo/git/refs', endpoints)

    def test_refuses_a_tag_on_another_commit(self):
        self.github.tag_at(VERSION, 'b' * 40)
        with self.assertRaisesRegex(ValueError, 'another commit'):
            self.publish()
        self.assertEqual(self.github.writes(), [])

    def test_existing_release_never_modified(self):
        for is_draft in (False, True):
            self.github.calls.clear()
            self.github.releases = [{'tag_name': VERSION, 'draft': is_draft}]
            with self.assertRaisesRegex(ValueError, 'already exists'):
                self.publish()
            self.assertEqual(len(self.github.calls), 1)
            self.assertIn('/releases?per_page=100&page=1', self.github.calls[0][0])

    def test_bad_assets_or_branch_precede_any_request(self):
        self.main_line.side_effect = subprocess.CalledProcessError(1, 'git')
        with self.assertRaises(subprocess.CalledProcessError):
            self.publish()
        self.main_line.side_effect = None
        (self.assets / 'SHA256SUMS').unlink()
        with self.assertRaises(ValueError):
            self.publish()
        self.assertEqual(self.github.calls, [])

    def test_partial_upload_never_publishes(self):
        def interrupt():
            if len(self.uploaded) == 2:
                raise TimeoutError('synthetic upload interruption')
        self.github.on_upload = interrupt
        with self.assertRaises(TimeoutError):
            self.publish()
        self.assertNotIn('PATCH', [method for method, _, _ in self.github.writes()])
        self.assertIs(self.github.releases[0]['draft'], True)

    def test_tag_moved_during_upload_never_publishes(self):
        def move():
            if len(self.uploaded) == 5:
                self.github.refs[VERSION] = ('commit', 'b' * 40)
        self.github.on_upload = move
        with self.assertRaisesRegex(ValueError, 'moved during upload'):
            self.publish()
        self.assertNotIn('PATCH', [method for method, _, _ in self.github.writes()])

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

    def test_gh_errors_are_not_all_treated_as_absent(self):
        for status in (401, 403, 404, 500):
            result = subprocess.CompletedProcess([], 1, '', f'gh: failure (HTTP {status})')
            with patch.object(release.subprocess, 'run', return_value=result), self.assertRaises(ValueError):
                release.gh_json('repos/example/repo/git/matching-refs/tags/' + VERSION)

    def test_gh_json_sends_the_requested_method(self):
        result = subprocess.CompletedProcess([], 0, '{}', '')
        with patch.object(release.subprocess, 'run', return_value=result) as run:
            release.gh_json('repos/example/repo/releases/7', body={'draft': False}, method='PATCH')
        command = run.call_args.args[0]
        self.assertEqual(command[command.index('--method') + 1], 'PATCH')
        self.assertEqual(json.loads(run.call_args.kwargs['input']), {'draft': False})


if __name__ == '__main__':
    unittest.main()
