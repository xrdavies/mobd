import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { baseURL, checkPromotion, prepareDistribution, publish, publishInstaller, sha256 } from './release.mjs';

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'mob-release-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const assets = join(root, 'assets'), output = join(root, 'output');
  mkdirSync(assets);
  for (const name of ['mobd_1.2.3_darwin_arm64.tar.gz', 'mobd_1.2.3_linux_amd64.tar.gz', 'mobd_1.2.3_amd64.deb', 'mobd_1.2.3_linux_arm64.tar.gz', 'mobd_1.2.3_arm64.deb']) {
    writeFileSync(join(assets, name), `fixture ${name}`);
  }
  const manifests = prepareDistribution(assets, output, { version: '1.2.3', base: 'https://download.example.com', team: 'A123456789', date: '2026-10-14T00:00:00Z', notes: 'New release' });
  return { output, manifests };
}

test('distribution identifies a missing download origin', () => {
  for (const base of [undefined, '']) {
    assert.throws(() => baseURL(base), /Missing DOWNLOAD_BASE_URL/);
  }
});

test('distribution rejects unsafe download origins', () => {
  assert.equal(baseURL('https://download.example.com/'), 'https://download.example.com');
  for (const base of ['http://download.example.com', 'https://user:pass@example.com', 'https://example.com/path', 'https://example.com/?token=x', 'https://pub-123.r2.dev']) {
    assert.throws(() => baseURL(base));
  }
});

test('manifests agree with payloads, pin the installer identity', t => {
  const { output, manifests } = fixture(t);
  assert.deepEqual(Object.keys(manifests), ['mobd']);
  const platform = manifests.mobd.platforms['darwin-aarch64'];
  assert.match(platform.url, /\/mobd\/v1.2.3\/mobd_1.2.3_darwin_arm64.tar.gz$/);
  assert.equal(platform.sha256, sha256(readFileSync(join(output, 'mobd/v1.2.3/mobd_1.2.3_darwin_arm64.tar.gz'))));
  assert.equal(manifests.mobd.download.url, platform.url);
  for (const [key, arch] of [['linux-x86_64', 'amd64'], ['linux-aarch64', 'arm64']]) {
    const linux = manifests.mobd.platforms[key];
    assert.match(linux.url, new RegExp(`/mobd_1.2.3_linux_${arch}.tar.gz$`));
    assert.equal(linux.sha256, sha256(readFileSync(join(output, `mobd/v1.2.3/mobd_1.2.3_linux_${arch}.tar.gz`))));
    assert.match(linux.deb.url, new RegExp(`/mobd_1.2.3_${arch}.deb$`));
    assert.equal(linux.deb.sha256, sha256(readFileSync(join(output, `mobd/v1.2.3/mobd_1.2.3_${arch}.deb`))));
  }
  assert.equal(readFileSync(join(output, 'mobd/v1.2.3/SHA256SUMS'), 'utf8').split('\n').filter(Boolean).length, 5);
  const installer = readFileSync(join(output, 'mobd/v1.2.3/install.sh'), 'utf8');
  assert.ok(installer.includes("download_base='https://download.example.com'"));
  assert.ok(installer.includes("apple_team='A123456789'"));
  assert.ok(!installer.includes('@DOWNLOAD_BASE_URL@'));
});

function remote(events, overrides = {}) {
  return {
    async readJSON() { return null; }, async digest() { return null; },
    async upload(key, path, immutable) { events.push({ type: 'upload', key, immutable }); },
    async verify(key) { events.push({ type: 'verify', key }); },
    ...overrides,
  };
}

test('all downloads are verified before the stable pointer is published', async t => {
  const { output, manifests } = fixture(t), events = [];
  await publish(output, manifests, remote(events));
  const firstMutable = events.findIndex(e => e.type === 'upload' && !e.immutable);
  assert.ok(events.slice(0, firstMutable).some(e => e.type === 'verify'));
  assert.ok(events.slice(firstMutable).every(e => e.type === 'upload' && !e.immutable));
  assert.deepEqual(events.filter(e => e.key.endsWith('/latest.json')).map(e => e.key), ['mobd/latest.json']);
});

test('failed public download verification never updates stable', async t => {
  const { output, manifests } = fixture(t), events = [];
  await assert.rejects(publish(output, manifests, remote(events, { async verify() { throw Error('Public checksum mismatch'); } })), /checksum/);
  assert.ok(events.every(e => e.immutable));
});

test('changed versioned objects and backwards promotions fail before any upload', async t => {
  const { output, manifests } = fixture(t), events = [];
  await assert.rejects(publish(output, manifests, remote(events, { async digest() { return 'different'; } })), /overwrite/);
  assert.equal(events.length, 0);
  assert.throws(() => checkPromotion({ version: '1.10.0' }, { version: '1.2.3' }), /backwards/);
  assert.doesNotThrow(() => checkPromotion({ version: '1.2.3' }, { version: '1.10.0' }));
  assert.throws(() => checkPromotion({ version: '1.2.3', notes: 'a' }, { version: '1.2.3', notes: 'b' }), /different metadata/);
});

test('retries preserve matching versioned files and finish promotion', async t => {
  const { output, manifests } = fixture(t), events = [];
  await publish(output, manifests, remote(events, {
    async digest(key) { return sha256(readFileSync(join(output, key))); },
    async readJSON(key) { return key.endsWith('/latest.json') ? manifests[key.split('/')[0]] : []; },
  }));
  assert.equal(events.filter(e => e.type === 'upload' && e.immutable).length, 0);
  assert.equal(events.filter(e => e.key.endsWith('/latest.json')).length, 1);
});

test('publication only reads and writes the mobd namespace', async t => {
  const { output, manifests } = fixture(t), events = [], reads = [];
  await publish(output, manifests, remote(events, {
    async readJSON(key) { reads.push(key); return null; },
    async digest(key) { reads.push(key); return null; },
  }));
  assert.ok(reads.length > 0);
  assert.ok(reads.every(key => key.startsWith('mobd/')));
  assert.ok(events.every(event => event.key.startsWith('mobd/')));
});

test('installer update verifies the stable binary and changes only the public script', async t => {
  const { output, manifests } = fixture(t), events = [];
  await publishInstaller(output, manifests.mobd, remote(events, {
    async readJSON() { return manifests.mobd; },
    async verify(key, digest) { events.push({ type: 'verify', key, digest }); },
  }));
  assert.deepEqual(events, [
    { type: 'verify', key: 'mobd/v1.2.3/mobd_1.2.3_darwin_arm64.tar.gz', digest: manifests.mobd.download.sha256 },
    { type: 'upload', key: 'mobd/install.sh', immutable: false },
    { type: 'verify', key: 'mobd/install.sh', digest: sha256(readFileSync(join(output, 'mobd/v1.2.3/install.sh'))) },
  ]);
});

test('installer update refuses another stable release or changed metadata before uploading', async t => {
  const { output, manifests } = fixture(t), events = [];
  for (const current of [null, { ...manifests.mobd, version: '1.2.4' }, { ...manifests.mobd, notes: 'changed' }]) {
    await assert.rejects(publishInstaller(output, manifests.mobd, remote(events, {
      async readJSON() { return current; },
    })), /current stable release/);
  }
  assert.equal(events.length, 0);
});

test('installer update stops if the published binary fails verification', async t => {
  const { output, manifests } = fixture(t), events = [];
  await assert.rejects(publishInstaller(output, manifests.mobd, remote(events, {
    async readJSON() { return manifests.mobd; },
    async verify() { throw Error('Public checksum mismatch'); },
  })), /checksum/);
  assert.equal(events.length, 0);
});
