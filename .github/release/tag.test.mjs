import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

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
  for (const name of ['tag.mjs', 'release.mjs']) {
    copyFileSync(new URL(name, import.meta.url), join(repo, '.github/release', name));
  }
  copyFileSync(new URL('../../Makefile', import.meta.url), join(repo, 'Makefile'));
  git('add', '.');
  git('commit', '-m', 'Initial release fixture');
  git('remote', 'add', 'origin', remote);
  git('push', 'origin', 'main');
  const run = (...args) => spawnSync(process.execPath, [join(repo, '.github/release/tag.mjs'), ...args], {
    cwd: root, env, encoding: 'utf8',
  });
  return { repo, remote, env, git, run };
}

test('make release pushes only the requested annotated tag at the published commit', t => {
  const { repo, env, git } = fixture(t);
  const head = git('rev-parse', 'HEAD');
  git('tag', '-a', 'v0.1.0', '-m', 'Unrelated local tag');
  git('config', 'push.followTags', 'true');
  const result = spawnSync('make', ['release', 'VERSION=v1.2.3'], { cwd: repo, env, encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /Pushed v1\.2\.3/);
  assert.match(result.stdout, /Release mobd macOS arm64/);
  assert.equal(git('cat-file', '-t', 'refs/tags/v1.2.3'), 'tag');
  assert.equal(git('tag', '--list', 'v1.2.3', '--format=%(contents:subject)'), 'mobd v1.2.3');
  assert.equal(git('rev-parse', 'v1.2.3^{commit}'), head);
  assert.equal(git('ls-remote', '--refs', '--tags', 'origin'), `${git('rev-parse', 'v1.2.3')}\trefs/tags/v1.2.3`);
  assert.equal(git('ls-remote', 'origin', 'refs/heads/main'), `${head}\trefs/heads/main`);
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
  const { git, run } = fixture(t);
  git('tag', 'v1.10.0');
  git('push', 'origin', 'refs/tags/v1.10.0');
  assert.match(run('v1.10.0').stderr, /Local tag .* already exists/);
  git('tag', '-d', 'v1.10.0');
  assert.match(run('v1.10.0').stderr, /Remote tag .* already exists/);
  assert.match(run('v1.2.3').stderr, /backwards/);
  assert.equal(git('tag', '--list'), '');
  const result = run('v1.11.0');
  assert.equal(result.status, 0, result.stderr);
});

test('an unreachable remote fails before creating a tag', t => {
  const { repo, git, run } = fixture(t);
  git('remote', 'set-url', 'origin', join(repo, 'missing.git'));
  assert.equal(run('v1.2.3').status, 1);
  assert.equal(git('tag', '--list'), '');
});

test('a rejected push keeps the local tag and gives a retry command', t => {
  const { remote, git, run } = fixture(t);
  mkdirSync(join(remote, 'hooks'));
  writeFileSync(join(remote, 'hooks/pre-receive'), '#!/bin/sh\nexit 1\n', { mode: 0o755 });
  const result = run('v1.2.3');
  assert.equal(result.status, 1);
  assert.match(result.stderr, /local tag v1\.2\.3 was kept/);
  assert.match(result.stderr, /push origin refs\/tags\/v1\.2\.3:refs\/tags\/v1\.2\.3/);
  assert.equal(git('rev-parse', 'v1.2.3^{commit}'), git('rev-parse', 'HEAD'));
  assert.equal(git('ls-remote', '--tags', 'origin'), '');
});
