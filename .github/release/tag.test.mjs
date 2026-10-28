import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { bumpVersion, readVersion, VERSION_FILES } from './bump.mjs';

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'mobd-tag-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const repo = join(root, 'repo'), remote = join(root, 'remote.git');
  mkdirSync(repo);
  const env = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1', GIT_TERMINAL_PROMPT: '0' };
  const git = (...args) => execFileSync('git', args, { cwd: repo, env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
  git('init', '--template=', '-b', 'main');
  git('init', '--bare', '--template=', '-b', 'main', remote);
  git('config', 'user.name', 'Release Test');
  git('config', 'user.email', 'release@example.com');
  mkdirSync(join(repo, '.github/release'), { recursive: true });
  for (const name of ['tag.mjs', 'release.mjs', 'bump.mjs', 'version.mjs']) {
    copyFileSync(new URL(name, import.meta.url), join(repo, '.github/release', name));
  }
  copyFileSync(new URL('../../Makefile', import.meta.url), join(repo, 'Makefile'));
  // The real version files, so the bump is tested against their actual shape.
  for (const file of VERSION_FILES) {
    mkdirSync(join(repo, dirname(file)), { recursive: true });
    copyFileSync(new URL(`../../${file}`, import.meta.url), join(repo, file));
  }
  git('add', '.');
  git('commit', '-m', 'Initial release fixture');
  git('remote', 'add', 'origin', remote);
  git('push', 'origin', 'main');
  // Answers yes to the confirmation unless told otherwise.
  const run = (...args) => {
    const input = typeof args.at(-1) === 'object' ? args.pop().input : 'y\n';
    return spawnSync(process.execPath, [join(repo, '.github/release/tag.mjs'), ...args], {
      cwd: root, env, encoding: 'utf8', input,
    });
  };
  const version = (...args) => spawnSync(process.execPath, [join(repo, '.github/release/version.mjs'), ...args], {
    cwd: root, env, encoding: 'utf8',
  });
  const commit = message => { git('commit', '-q', '--allow-empty', '-m', message); git('push', '-q', 'origin', 'main'); };
  return { repo, remote, env, git, run, version, commit };
}

test('make release commits the version files and pushes that commit with the annotated tag', t => {
  const { repo, env, git } = fixture(t);
  const head = git('rev-parse', 'HEAD');
  git('tag', '-a', 'v0.1.0', '-m', 'Unrelated local tag');
  git('config', 'push.followTags', 'true');
  const result = spawnSync('make', ['release', 'VERSION=v1.2.3'], { cwd: repo, env, encoding: 'utf8', input: 'y\n' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /Pushed v1\.2\.3/);
  assert.match(result.stdout, /Actions → Release mobd,/);
  assert.equal(git('cat-file', '-t', 'refs/tags/v1.2.3'), 'tag');
  assert.equal(git('tag', '--list', 'v1.2.3', '--format=%(contents:subject)'), 'mobd v1.2.3');
  const release = git('rev-parse', 'v1.2.3^{commit}');
  assert.equal(git('rev-parse', 'v1.2.3^{commit}~1'), head, 'the release commit sits on the published main');
  assert.equal(git('log', '-1', '--format=%s', release), 'chore(release): v1.2.3');
  assert.deepEqual(git('diff', '--name-only', head, release).split('\n').sort(), [...VERSION_FILES].sort());
  for (const file of VERSION_FILES) assert.match(readFileSync(join(repo, file), 'utf8'), /1\.2\.3|1002003/, file);
  assert.equal(git('ls-remote', '--refs', '--tags', 'origin'), `${git('rev-parse', 'v1.2.3')}\trefs/tags/v1.2.3`);
  assert.equal(git('ls-remote', 'origin', 'refs/heads/main'), `${release}\trefs/heads/main`);
  assert.equal(git('status', '--porcelain'), '');
});

test('invalid versions and extra arguments never create a tag', t => {
  const { git, run } = fixture(t);
  for (const args of [[], ['1.2.3'], ['v01.2.3'], ['v1.2.3-beta.1'], ['v1.2.3', 'extra']]) {
    const result = run(...args);
    assert.equal(result.status, 1, result.stderr);
  }
  assert.equal(git('tag', '--list'), '');
  assert.equal(git('ls-remote', '--tags', 'origin'), '');
});

test('untracked, unstaged and staged changes block tagging', t => {
  const { repo, git, run } = fixture(t);
  writeFileSync(join(repo, 'untracked'), 'Pending change');
  assert.match(run('v1.2.3').stderr, /Commit or stash/);
  rmSync(join(repo, 'untracked'));
  writeFileSync(join(repo, 'Makefile'), '# Pending change\n');
  assert.match(run('v1.2.3').stderr, /Commit or stash/);
  git('add', 'Makefile');
  assert.match(run('v1.2.3').stderr, /Commit or stash/);
  assert.equal(git('tag', '--list'), '');
});

test('other branches, detached HEAD and unpublished or outdated main block tagging', t => {
  const { git, run } = fixture(t);
  git('switch', '-c', 'feature');
  assert.match(run('v1.2.3').stderr, /Switch to main/);
  git('switch', '--detach');
  assert.match(run('v1.2.3').stderr, /Switch to main/);
  git('switch', 'main');
  git('commit', '--allow-empty', '-m', 'Unpublished');
  assert.match(run('v1.2.3').stderr, /must match origin\/main/);
  git('push', 'origin', 'main');
  git('reset', '--hard', 'HEAD~1');
  assert.match(run('v1.2.3').stderr, /must match origin\/main/);
  assert.equal(git('tag', '--list'), '');
});

test('existing and lower versions are rejected using live remote tags', t => {
  const { git, run, commit } = fixture(t);
  git('tag', 'v1.10.0');
  git('push', 'origin', 'refs/tags/v1.10.0');
  assert.match(run('v1.10.0').stderr, /Local tag .* already exists/);
  git('tag', '-d', 'v1.10.0');
  assert.match(run('v1.10.0').stderr, /Remote tag .* already exists/);
  assert.match(run('v1.2.3').stderr, /backwards/);
  assert.equal(git('tag', '--list'), '');
  assert.match(run('v1.11.0').stderr, /already released as v1\.10\.0/, 'the tagged commit is not released twice');
  commit('Change after v1.10.0');
  const result = run('v1.11.0');
  assert.equal(result.status, 0, result.stderr);
});

test('an unreachable remote fails before creating a tag', t => {
  const { repo, git, run } = fixture(t);
  git('remote', 'set-url', 'origin', join(repo, 'missing.git'));
  assert.equal(run('v1.2.3').status, 1);
  assert.equal(git('tag', '--list'), '');
});

test('a rejected push keeps the local release commit and tag and gives a retry command', t => {
  const { remote, git, run } = fixture(t);
  const before = git('rev-parse', 'HEAD');
  mkdirSync(join(remote, 'hooks'));
  writeFileSync(join(remote, 'hooks/pre-receive'), '#!/bin/sh\nexit 1\n', { mode: 0o755 });
  const result = run('v1.2.3');
  assert.equal(result.status, 1);
  assert.match(result.stderr, /release commit and tag v1\.2\.3 were kept/);
  assert.match(result.stderr, /push --atomic origin main refs\/tags\/v1\.2\.3:refs\/tags\/v1\.2\.3/);
  assert.equal(git('rev-parse', 'v1.2.3^{commit}'), git('rev-parse', 'HEAD'));
  assert.equal(git('ls-remote', '--tags', 'origin'), '');
  assert.equal(git('ls-remote', 'origin', 'refs/heads/main'), `${before}\trefs/heads/main`, 'main is not pushed without its tag');
  assert.equal(git('rev-parse', 'HEAD~1'), before);
});

test('version files that already match make no extra commit', t => {
  const { repo, git, run } = fixture(t);
  bumpVersion(repo, '1.2.3');
  git('commit', '-qam', 'Set version by hand');
  git('push', 'origin', 'main');
  const head = git('rev-parse', 'HEAD');
  const result = run('v1.2.3');
  assert.equal(result.status, 0, result.stderr);
  assert.equal(git('rev-parse', 'v1.2.3^{commit}'), head);
  assert.equal(git('ls-remote', 'origin', 'refs/heads/main'), `${head}\trefs/heads/main`);
});

test('a version file in an unexpected shape stops the release before anything is written', t => {
  const { repo, git, run } = fixture(t);
  writeFileSync(join(repo, VERSION_FILES[0]), 'no version here\n');
  git('commit', '-qam', 'Break the version file');
  git('push', 'origin', 'main');
  const head = git('rev-parse', 'HEAD');
  const result = run('v1.2.3');
  assert.equal(result.status, 1);
  assert.equal(git('rev-parse', 'HEAD'), head);
  assert.equal(git('status', '--porcelain'), '');
  assert.equal(git('tag', '--list'), '');
});

test('the repository version files agree', () => {
  readVersion(fileURLToPath(new URL('../../', import.meta.url)));
});

test('make release-patch, -minor and -major release the next version after the version files', t => {
  const { repo, env, git, run, commit } = fixture(t);
  const current = readVersion(repo);
  const [major, minor, patch] = current.split('.').map(Number);
  const result = spawnSync('make', ['release-patch'], { cwd: repo, env, encoding: 'utf8', input: 'y\n' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, new RegExp(`${current} \\(never released\\) → v${major}\\.${minor}\\.${patch + 1}`));
  assert.match(result.stdout, /1 commit on main:\n {2}[0-9a-f]+ Initial release fixture/);
  assert.equal(readVersion(repo), `${major}.${minor}.${patch + 1}`);
  commit('Feature');
  commit('Fix');
  const minorRun = run('minor');
  assert.equal(minorRun.status, 0, minorRun.stderr);
  assert.match(minorRun.stdout, new RegExp(`v${major}\\.${minor}\\.${patch + 1} → v${major}\\.${minor + 1}\\.0\n2 commits since`));
  commit('Breaking change');
  assert.equal(run('major').status, 0);
  assert.equal(readVersion(repo), `${major + 1}.0.0`);
  assert.deepEqual(git('ls-remote', '--refs', '--tags', 'origin').split('\n').map(l => l.split('refs/tags/')[1]).sort(),
    [`v${major + 1}.0.0`, `v${major}.${minor + 1}.0`, `v${major}.${minor}.${patch + 1}`].sort());
});

test('declining or not answering the confirmation changes nothing', t => {
  const { git, run } = fixture(t);
  const head = git('rev-parse', 'HEAD');
  for (const input of ['', 'n\n', 'maybe\n']) {
    const result = run('patch', { input });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /Cancelled; nothing was changed/);
  }
  assert.equal(git('rev-parse', 'HEAD'), head);
  assert.equal(git('status', '--porcelain'), '');
  assert.equal(git('tag', '--list'), '');
  assert.equal(git('ls-remote', '--tags', 'origin'), '');
});

test('a bump refuses version files that are behind the remote tags', t => {
  const { repo, git, run, commit } = fixture(t);
  const [major] = readVersion(repo).split('.').map(Number);
  git('tag', `v${major + 5}.0.0`);
  git('push', '-q', 'origin', `refs/tags/v${major + 5}.0.0`);
  git('tag', '-d', `v${major + 5}.0.0`);
  commit('After the stray tag');
  assert.match(run('patch').stderr, new RegExp(`origin already has v${major + 5}\\.0\\.0`));
  assert.equal(git('tag', '--list'), '');
  assert.equal(run(`v${major + 5}.0.1`).status, 0, 'an explicit version still works and fixes the files');
  assert.equal(readVersion(repo), `${major + 5}.0.1`);
});

test('unknown bump names never create a tag', t => {
  const { git, run } = fixture(t);
  for (const arg of ['Patch', 'micro', 'release']) assert.match(run(arg).stderr, /Usage/);
  assert.equal(git('tag', '--list'), '');
});

test('version.mjs shows the version and checks it against a tag', t => {
  const { repo, run, version } = fixture(t);
  const current = readVersion(repo);
  assert.match(version().stdout, new RegExp(`Not released yet[\\s\\S]*make release-patch → v`));
  assert.equal(run('patch').status, 0);
  const next = readVersion(repo);
  const shown = version();
  assert.equal(shown.status, 0, shown.stderr);
  assert.match(shown.stdout, new RegExp(`Last release tag: v${next.replaceAll('.', '\\.')}, 0 commits since`));
  assert.equal(version('--check').status, 0);
  assert.equal(version('--check', `v${next}`, '--ref', `v${next}`).status, 0);
  assert.equal(version('--check', `v${next}`).status, 0);
  const old = version('--check', `v${next}`, '--ref', 'HEAD~1');
  assert.equal(old.status, 1);
  assert.match(old.stderr, new RegExp(`version files say ${current.replaceAll('.', '\\.')}`));
  assert.equal(version('--check', 'v99.0.0').status, 1);
});
