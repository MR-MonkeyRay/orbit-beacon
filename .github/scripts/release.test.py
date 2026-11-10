#!/usr/bin/env python3
"""Offline publication transitions: real git refs, local Release/OCI state, no network."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().with_name("release.sh")
ASSETS = [f"orbitbeacon-{kind}-linux-{arch}" for arch in ("amd64", "arm64") for kind in ("server", "agent", "mcp")]
DATE = "2026-11-09T00:00:00Z"

# These commands implement persistent external state, including interrupted writes.
STUB = r'''#!/usr/bin/env python3
import hashlib, json, os, pathlib, shutil, subprocess, sys
s = pathlib.Path(os.environ['SANDBOX'])
a = sys.argv[1:]
cmd = pathlib.Path(sys.argv[0]).name
state = json.loads((s/'state.json').read_text())
def save(): (s/'state.json').write_text(json.dumps(state, sort_keys=True))
def stop(message): print(message, file=sys.stderr); sys.exit(1)
def value(flag): return a[a.index(flag)+1]
def binaries(): return [f'orbitbeacon-{k}-linux-{p}' for p in ('amd64','arm64') for k in ('server','agent','mcp')]
def metadata(path): return json.loads(pathlib.Path(path).read_text().splitlines()[1].removeprefix('# sandbox build '))
if cmd == 'go':
    if a[:2] == ['version','-m']:
        m = metadata(a[2]); print('\tbuild\tvcs.revision='+m['commit'])
    elif a[0] == 'build':
        flags = value('-ldflags').split(); m = {'arch': os.environ['GOARCH'], 'kind': a[-1].split('-')[-1]}
        for key, field in [('Version','version'),('Commit','commit'),('BuildDate','date')]: m[field] = next(x.split('=',1)[1] for x in flags if x.startswith('orbitbeacon/internal/buildinfo.'+key+'='))
        content = '# sandbox build\n# sandbox build '+json.dumps(m, sort_keys=True)+'\n'+m['commit']+'\n'+m['version']+'\n'+m['date']+'\n'
        path = pathlib.Path(value('-o')); path.write_text(content); path.chmod(0o755); state['builds'] += 1; save()
    else: stop('unsupported go '+repr(a))
elif cmd == 'gh':
    if a[0] == 'api':
        endpoint = next(x for x in a if x.startswith('repos/'))
        if '--method' in a:
            ref = next(x[4:] for x in a if x.startswith('ref=')); sha = next(x[4:] for x in a if x.startswith('sha='))
            subprocess.run(['git','--git-dir',str(s/'origin.git'),'update-ref',ref,sha],check=True)
        elif '/releases/tags/' in endpoint:
            if state['release'] is None or state['release']['draft']: stop('gh: Not Found (HTTP 404)')
            print(json.dumps(state['release']))
        elif endpoint.endswith('/releases?per_page=100'):
            if os.environ.get('FAIL_LOOKUP'): stop('gh: API unavailable (HTTP 503)')
            pages = [[{'tag_name':'v0.0.0','draft':False,'assets':[]}]]
            if state['release'] is not None: pages.append([state['release']])
            print(json.dumps(pages if '--paginate' in a and '--slurp' in a else pages[0]))
        else: stop('unsupported API '+repr(a))
    elif a[:2] == ['release','download']:
        dest = pathlib.Path(value('--dir')); dest.mkdir(parents=True,exist_ok=True)
        for p in (s/'assets').iterdir(): shutil.copyfile(p,dest/p.name)
    elif a[:2] == ['release','create']:
        if state['release'] is not None: stop('release for tag already exists')
        state['release'] = {'draft':True,'tag_name':'v'+os.environ['VERSION'],'assets':[]}; save()
    elif a[:2] == ['release','upload']:
        for path in a[3:]:
            if path.startswith('--'): stop('overwriting assets is forbidden')
            p = pathlib.Path(path)
            if (s/'assets'/p.name).exists(): stop('asset already exists')
            shutil.copyfile(p,s/'assets'/p.name)
            state['release']['assets'].append({'name':p.name}); save()
            if os.environ.get('FAIL_UPLOAD') == p.name: stop('interrupted upload')
    elif a[:2] == ['release','edit']:
        if os.environ.get('FAIL_RELEASE'): stop('injected Release publish failure')
        state['release']['draft'] = False; save()
    else: stop('unsupported gh '+repr(a))
elif cmd == 'docker':
    if a[:3] == ['buildx','imagetools','inspect']:
        ref = a[3]; digest = ref.split('@',1)[1] if '@' in ref else state['tags'].get(ref.rsplit(':',1)[1])
        if not digest: stop('manifest unknown')
        if digest in state['images']:
            image = state['images'][digest]
            print(json.dumps({'digest':digest,'manifests':[{'digest':digest+'-'+arch,'platform':{'os':'linux','architecture':arch}} for arch in ('amd64','arm64')]}))
        else:
            parent, arch = digest.rsplit('-',1); image = state['images'][parent]; revision = image.get('revisions',{}).get(arch,image['commit'])
            print(json.dumps({'config':{'Labels':{'org.opencontainers.image.revision':revision,'org.opencontainers.image.version':image['version'],'org.opencontainers.image.created':image['date']}}}))
    elif a[:2] == ['buildx','build']:
        context = value('--build-context').split('=',1)[1]; digest = 'sha256:'+hashlib.sha256((pathlib.Path(context)/'SHA256SUMS').read_bytes()).hexdigest()
        image = {'commit':os.environ['GITHUB_SHA'],'version':os.environ['VERSION'],'date':value('--build-arg').split('=',1)[1]}
        image['date'] = next(x.split('=',1)[1] for x in a if x.startswith('BUILD_DATE='))
        image['servers'] = {arch:(pathlib.Path(context)/('orbitbeacon-server-linux-'+arch)).read_text() for arch in ('amd64','arm64')}
        state['images'][digest] = image
        for i,x in enumerate(a):
            if x == '--tag': state['tags'][a[i+1].rsplit(':',1)[1]] = digest
        save()
    elif a[:3] == ['buildx','imagetools','create']:
        digest = a[-1].split('@')[1]
        for i,x in enumerate(a):
            if x == '--tag':
                tag = a[i+1].rsplit(':',1)[1]
                if not state['release'] or state['release']['draft']: stop('promotion before public Release')
                if {x['name'] for x in state['release']['assets']} != set(binaries()+['SHA256SUMS']): stop('promotion before complete asset set')
                for line in (s/'assets'/'SHA256SUMS').read_text().splitlines():
                    checksum,name = line.split()
                    if checksum != hashlib.sha256((s/'assets'/name).read_bytes()).hexdigest(): stop('promotion with inconsistent assets')
                for arch,data in state['images'][digest]['servers'].items():
                    if data != (s/'assets'/('orbitbeacon-server-linux-'+arch)).read_text(): stop('promotion with different Server binary')
                if tag == os.environ['VERSION'] and os.environ.get('FAIL_VERSION'): stop('interrupted promotion')
                state['tags'][tag] = digest; save()
    elif a[0] == 'pull': pass
    elif a[0] == 'create':
        (s/'container').write_text(a[-1].split('@')[1]); print('sandbox-container')
    elif a[0] == 'cp':
        parent,arch = (s/'container').read_text().rsplit('-',1)
        pathlib.Path(a[-1]).write_text(state['images'][parent]['servers'][arch])
    elif a[0] == 'rm': pass
    else: stop('unsupported docker '+repr(a))
else: stop('unsupported command')
'''


class PublicationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="orbitbeacon-release-test-")
        self.root = Path(self.tmp.name)
        self.addCleanup(self.tmp.cleanup)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.git("init", "-q")
        self.git("config", "user.email", "test@example.invalid")
        self.git("config", "user.name", "Release sandbox")
        (self.repo / "VERSION").write_text("0.3.4\n")
        self.git("add", "VERSION")
        self.git("commit", "-qm", "original")
        self.old = self.git("rev-parse", "HEAD").strip()
        (self.repo / "change").write_text("new main")
        self.git("add", "change")
        self.git("commit", "-qm", "current")
        self.sha = self.git("rev-parse", "HEAD").strip()
        subprocess.run(["git", "clone", "-q", "--bare", str(self.repo), str(self.root / "origin.git")], check=True)
        self.git("remote", "add", "origin", str(self.root / "origin.git"))
        (self.root / "assets").mkdir()
        (self.root / "bin").mkdir()
        for name in ("gh", "docker", "go"):
            path = self.root / "bin" / name
            path.write_text(STUB)
            path.chmod(0o755)
        self.state = {"release": None, "tags": {}, "images": {}, "builds": 0}
        self.save()
        self.env = dict(os.environ, SANDBOX=str(self.root), RUNNER_TEMP=str(self.root), GITHUB_SHA=self.sha,
                        GITHUB_REPOSITORY="example/orbit-beacon", VERSION="0.3.4",
                        PATH=str(self.root / "bin") + os.pathsep + os.environ["PATH"])

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repo, text=True)

    def save(self):
        (self.root / "state.json").write_text(json.dumps(self.state, sort_keys=True))

    def load(self):
        self.state = json.loads((self.root / "state.json").read_text())

    def run_release(self, success=True, **env):
        result = subprocess.run(["bash", str(SCRIPT)], cwd=self.repo, env=dict(self.env, **env), text=True, capture_output=True)
        self.load()
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def seed(self, commit=None, complete=False, image=True, annotated=False):
        commit = commit or self.sha
        args = ["tag"] + (["-a", "-m", "release"] if annotated else []) + ["v0.3.4", commit]
        self.git(*args)
        self.git("push", "-q", "origin", "refs/tags/v0.3.4")
        self.state['release'] = {'draft': not complete, 'tag_name': 'v0.3.4', 'assets': []}
        for name in ASSETS:
            kind, arch = name.split('-')[1], name.split('-')[-1]
            m = {'commit':commit,'version':'0.3.4','date':DATE,'kind':kind,'arch':arch}
            content = '# sandbox build\n# sandbox build '+json.dumps(m, sort_keys=True)+'\n'+commit+'\n0.3.4\n'+DATE+'\n'
            path = self.root/'assets'/name; path.write_text(content); path.chmod(0o755)
        sums = ''.join(hashlib.sha256((self.root/'assets'/n).read_bytes()).hexdigest()+'  '+n+'\n' for n in sorted(ASSETS))
        (self.root/'assets'/'SHA256SUMS').write_text(sums)
        self.state['release']['assets'] = [{'name':n} for n in ASSETS+['SHA256SUMS']]
        digest = 'sha256:'+hashlib.sha256(sums.encode()).hexdigest()
        if image:
            self.state['images'][digest] = {'commit':commit,'version':'0.3.4','date':DATE,'servers':{a:(self.root/'assets'/('orbitbeacon-server-linux-'+a)).read_text() for a in ('amd64','arm64')}}
            self.state['tags'][commit] = digest
            if complete: self.state['tags']['0.3.4'] = digest
        if not complete:
            (self.root/'assets'/ASSETS[-1]).unlink()
            self.state['release']['assets'] = [x for x in self.state['release']['assets'] if x['name'] != ASSETS[-1]]
        self.save()
        return digest

    def assert_consistent(self):
        digest = self.state['tags']['0.3.4']
        self.assertEqual(digest, self.state['tags']['latest'])
        self.assertFalse(self.state['release']['draft'])
        self.assertEqual({x['name'] for x in self.state['release']['assets']}, set(ASSETS+['SHA256SUMS']))
        for arch, data in self.state['images'][digest]['servers'].items():
            self.assertEqual(data, (self.root/'assets'/('orbitbeacon-server-linux-'+arch)).read_text())
        for line in (self.root/'assets'/'SHA256SUMS').read_text().splitlines():
            checksum, name = line.split()
            self.assertEqual(checksum, hashlib.sha256((self.root/'assets'/name).read_bytes()).hexdigest())
        ref = self.git('ls-remote','origin','refs/tags/v0.3.4*').splitlines()
        self.assertEqual(ref[-1].split()[0], self.sha)

    def test_release_failure_does_not_expose_version_or_latest_and_reruns(self):
        self.run_release(False, FAIL_RELEASE='1')
        self.assertNotIn('0.3.4', self.state['tags'])
        self.assertNotIn('latest', self.state['tags'])
        staged = self.state['tags'][self.sha]
        builds = self.state['builds']
        self.run_release()
        self.assert_consistent()
        self.assertEqual(staged, self.state['tags']['0.3.4'])
        self.assertEqual(builds, self.state['builds'])

    def test_partial_same_sha_reuses_manifest_and_preserves_existing_assets(self):
        digest = self.seed()
        before = {p.name:p.read_bytes() for p in (self.root/'assets').iterdir()}
        self.run_release()
        self.assert_consistent()
        self.assertEqual(digest, self.state['tags']['0.3.4'])
        for name,data in before.items(): self.assertEqual(data,(self.root/'assets'/name).read_bytes())

    def test_partial_other_sha_refuses_without_writes(self):
        self.seed(self.old)
        before = (self.root/'state.json').read_bytes()
        assets = {p.name:p.read_bytes() for p in (self.root/'assets').iterdir()}
        self.run_release(False)
        self.assertEqual(before, (self.root/'state.json').read_bytes())
        self.assertEqual(assets, {p.name:p.read_bytes() for p in (self.root/'assets').iterdir()})

    def test_complete_old_release_is_normal_skip(self):
        self.seed(self.old, complete=True, annotated=True)
        before = (self.root/'state.json').read_bytes()
        self.run_release()
        self.assertEqual(before, (self.root/'state.json').read_bytes())

    def test_foreign_dangling_tag_refuses(self):
        self.git('tag','-a','v0.3.4','-m','foreign',self.old)
        self.git('push','-q','origin','refs/tags/v0.3.4')
        before = (self.root/'state.json').read_bytes()
        self.run_release(False)
        self.assertEqual(before, (self.root/'state.json').read_bytes())

    def test_mixed_platform_revision_refuses(self):
        digest = self.seed(complete=True)
        self.state['images'][digest]['revisions'] = {'arm64': self.old}
        self.save()
        before = (self.root/'state.json').read_bytes()
        self.run_release(False)
        self.assertEqual(before, (self.root/'state.json').read_bytes())

    def test_foreign_release_binary_refuses_without_upload(self):
        self.seed()
        asset = self.root/'assets'/ASSETS[0]
        data = asset.read_text().replace(self.sha, self.old)
        asset.write_text(data)
        before = (self.root/'state.json').read_bytes()
        self.run_release(False)
        self.assertEqual(before, (self.root/'state.json').read_bytes())

    def test_release_only_same_sha_recovers_without_overwriting_assets(self):
        self.seed(image=False)
        before = {p.name:p.read_bytes() for p in (self.root/'assets').iterdir()}
        self.run_release()
        self.assert_consistent()
        for name,data in before.items(): self.assertEqual(data,(self.root/'assets'/name).read_bytes())

    def test_same_sha_annotated_dangling_tag_recovers(self):
        self.git('tag','-a','v0.3.4','-m','same source',self.sha)
        self.git('push','-q','origin','refs/tags/v0.3.4')
        self.run_release()
        self.assert_consistent()

    def test_foreign_partial_version_image_without_tag_refuses(self):
        self.seed(self.old)
        self.git('push','-q','origin',':refs/tags/v0.3.4')
        self.state['release'] = None
        self.state['tags']['0.3.4'] = self.state['tags'].pop(self.old)
        for p in (self.root/'assets').iterdir(): p.unlink()
        self.save()
        before = (self.root/'state.json').read_bytes()
        self.run_release(False)
        self.assertEqual(before, (self.root/'state.json').read_bytes())

    def test_version_and_stage_manifest_conflict_refuses(self):
        digest = self.seed()
        other = 'sha256:'+'a'*64
        self.state['images'][other] = dict(self.state['images'][digest])
        self.state['tags']['0.3.4'] = other
        self.save()
        before = (self.root/'state.json').read_bytes()
        self.run_release(False)
        self.assertEqual(before, (self.root/'state.json').read_bytes())

    def test_interrupted_asset_upload_recovers(self):
        self.run_release(False, FAIL_UPLOAD=ASSETS[2])
        self.assertNotIn('0.3.4', self.state['tags'])
        self.assertNotIn('latest', self.state['tags'])
        self.assertTrue(self.state['release']['draft'])
        uploaded = {p.name:p.read_bytes() for p in (self.root/'assets').iterdir()}
        self.run_release()
        self.assert_consistent()
        for name,data in uploaded.items(): self.assertEqual(data,(self.root/'assets'/name).read_bytes())

    def test_interrupted_promotion_recovers_without_rebuild(self):
        self.run_release(False, FAIL_VERSION='1')
        digest = self.state['tags']['latest']
        builds = self.state['builds']
        self.assertNotIn('0.3.4', self.state['tags'])
        self.run_release()
        self.assert_consistent()
        self.assertEqual(builds,self.state['builds'])
        self.assertEqual(digest,self.state['tags']['latest'])
        self.run_release()
        self.assertEqual(builds,self.state['builds'])

    def test_complete_same_sha_skips_without_moving_newer_latest(self):
        self.seed(complete=True)
        self.state['tags']['latest'] = 'sha256:'+'f'*64
        self.save()
        before = (self.root/'state.json').read_bytes()
        self.run_release()
        self.assertEqual(before, (self.root/'state.json').read_bytes())

    def test_release_failure_keeps_previous_deployable_latest(self):
        previous = 'sha256:'+'e'*64
        self.state['tags']['latest'] = previous
        self.save()
        self.run_release(False, FAIL_RELEASE='1')
        self.assertEqual(previous, self.state['tags']['latest'])
        self.assertNotIn('0.3.4', self.state['tags'])

    def test_release_lookup_error_fails_without_mutation(self):
        before = (self.root/'state.json').read_bytes()
        self.run_release(False, FAIL_LOOKUP='1')
        self.assertEqual(before, (self.root/'state.json').read_bytes())


if __name__ == '__main__':
    unittest.main()
