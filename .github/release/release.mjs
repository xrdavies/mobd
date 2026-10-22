import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export function version(value) {
  if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(value ?? '')) throw Error('Expected a stable MAJOR.MINOR.PATCH version');
  return value;
}

export function baseURL(value) {
  if (!value) throw Error('Missing DOWNLOAD_BASE_URL: set it as a Variable or Secret in the release environment');
  const url = new URL(value);
  if (url.protocol !== 'https:' || url.username || url.password || url.port || url.search || url.hash || url.pathname !== '/' || !/^[a-z0-9.-]+$/.test(url.hostname) || url.hostname.endsWith('.r2.dev')) {
    throw Error('DOWNLOAD_BASE_URL must be an HTTPS custom-domain origin without a path or credentials');
  }
  return url.origin;
}

export const sha256 = (data) => createHash('sha256').update(data).digest('hex');
const json = (data) => JSON.stringify(data, null, 2) + '\n';

export function prepareDistribution(assets, output, options) {
  const v = version(options.version);
  const base = baseURL(options.base);
  if (!/^[A-Z0-9]{10}$/.test(options.team ?? '')) throw Error('Invalid APPLE_TEAM_ID');
  if (!Number.isFinite(Date.parse(options.date))) throw Error('Missing release publication date');
  const prefix = `mobd/v${v}`;
  const stem = `mobd_${v}_darwin_arm64`;
  // Linux (only with options.linux, all architectures or none): tarballs for install.sh, Debian
  // packages for apt.
  const linux = options.linux ? { 'linux-x86_64': 'amd64', 'linux-aarch64': 'arm64' } : {};
  const names = [`${stem}.tar.gz`, ...Object.values(linux).flatMap(arch => [`mobd_${v}_linux_${arch}.tar.gz`, `mobd_${v}_${arch}.deb`])];
  const put = (key, data) => {
    mkdirSync(dirname(join(output, key)), { recursive: true });
    writeFileSync(join(output, key), data);
  };
  const hashes = {};
  for (const name of names) {
    const data = readFileSync(join(assets, name));
    if (!data.length) throw Error(`Empty artifact: ${name}`);
    hashes[name] = sha256(data);
    put(`${prefix}/${name}`, data);
  }
  const payload = `${stem}.tar.gz`;
  const asset = name => ({ url: `${base}/${prefix}/${name}`, sha256: hashes[name] });
  const platforms = { 'darwin-aarch64': asset(payload) };
  for (const [key, arch] of Object.entries(linux)) {
    platforms[key] = { ...asset(`mobd_${v}_linux_${arch}.tar.gz`), deb: asset(`mobd_${v}_${arch}.deb`) };
  }
  const download = payload;
  const manifest = {
    version: v, notes: options.notes || `mobd ${v}`, pub_date: options.date,
    platforms,
    download: { url: `${base}/${prefix}/${download}`, sha256: hashes[download] },
    checksums_url: `${base}/${prefix}/SHA256SUMS`,
  };
  put(`${prefix}/release.json`, json(manifest));
  put(`${prefix}/SHA256SUMS`, names.map(name => `${hashes[name]}  ${name}\n`).join(''));
  const template = readFileSync(new URL('./install.sh', import.meta.url), 'utf8');
  put(`${prefix}/install.sh`, template.replaceAll('@DOWNLOAD_BASE_URL@', base).replaceAll('@APPLE_TEAM_ID@', options.team));
  return { mobd: manifest };
}

export function checkPromotion(current, next) {
  if (!current) return;
  const a = version(current.version).split('.').map(BigInt);
  const b = version(next.version).split('.').map(BigInt);
  for (let i = 0; i < 3; i++) {
    if (b[i] > a[i]) return;
    if (b[i] < a[i]) throw Error(`Refusing to move stable backwards from ${current.version} to ${next.version}`);
  }
  if (json(current) !== json(next)) throw Error(`Version ${next.version} is already published with different metadata`);
}

function files(root, prefix = '') {
  return readdirSync(join(root, prefix), { withFileTypes: true }).flatMap(entry => {
    const key = prefix ? `${prefix}/${entry.name}` : entry.name;
    return entry.isDirectory() ? files(root, key) : [key];
  });
}

export async function publish(output, manifests, remote) {
  // Validate release metadata and immutable keys before writing anything.
  const histories = {};
  for (const [product, manifest] of Object.entries(manifests)) {
    checkPromotion(await remote.readJSON(`${product}/latest.json`), manifest);
    const history = await remote.readJSON(`${product}/releases.json`) ?? [];
    if (!Array.isArray(history)) throw Error(`Invalid ${product} release history`);
    histories[product] = [manifest, ...history.filter(item => item.version !== manifest.version)];
  }
  const keys = files(output);
  const pending = [];
  for (const key of keys) {
    const digest = sha256(readFileSync(join(output, key)));
    const existing = await remote.digest(key);
    if (existing !== null && existing !== digest) throw Error(`Refusing to overwrite published object: ${key}`);
    if (existing === null) pending.push(key);
  }
  for (const key of pending) await remote.upload(key, join(output, key), true);
  for (const key of keys) await remote.verify(key, sha256(readFileSync(join(output, key))));
  // The stable pointers are written only after every public artifact is readable.
  const installerKey = keys.find(key => key.endsWith('/install.sh'));
  await remote.upload('mobd/install.sh', join(output, installerKey), false);
  for (const [product, manifest] of Object.entries(manifests)) {
    const historyFile = join(output, `${product}-releases.json`);
    const latestFile = join(output, `${product}-latest.json`);
    writeFileSync(historyFile, json(histories[product]));
    writeFileSync(latestFile, json(manifest));
    await remote.upload(`${product}/releases.json`, historyFile, false);
    await remote.upload(`${product}/latest.json`, latestFile, false);
  }
}

export async function publishInstaller(output, manifest, remote) {
  if (json(await remote.readJSON('mobd/latest.json')) !== json(manifest)) {
    throw Error('Installer updates require the current stable release with unchanged metadata');
  }
  const prefix = `mobd/v${version(manifest.version)}`;
  await remote.verify(`${prefix}/mobd_${manifest.version}_darwin_arm64.tar.gz`, manifest.download.sha256);
  const installer = join(output, prefix, 'install.sh');
  await remote.upload('mobd/install.sh', installer, false);
  await remote.verify('mobd/install.sh', sha256(readFileSync(installer)));
}

function r2Remote(output, base) {
  const account = process.env.R2_ACCOUNT_ID;
  const bucket = process.env.R2_BUCKET;
  if (!/^[a-f0-9]{32}$/.test(account ?? '') || !/^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/.test(bucket ?? '')) throw Error('Invalid R2_ACCOUNT_ID or R2_BUCKET');
  for (const key of ['AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY']) if (!process.env[key]) throw Error(`Missing ${key}`);
  const aws = (...args) => execFileSync('aws', ['--endpoint-url', `https://${account}.r2.cloudflarestorage.com`, ...args], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  const missing = error => /\((NoSuchKey|404|NotFound)\)/.test(error.stderr?.toString() ?? '');
  const scratch = join(output, 'remote.json');
  return {
    async readJSON(key) {
      try { aws('s3api', 'get-object', '--bucket', bucket, '--key', key, scratch); }
      catch (error) { if (missing(error)) return null; throw error; }
      return JSON.parse(readFileSync(scratch, 'utf8'));
    },
    async digest(key) {
      try { return JSON.parse(aws('s3api', 'head-object', '--bucket', bucket, '--key', key)).Metadata?.sha256 ?? ''; }
      catch (error) { if (missing(error)) return null; throw error; }
    },
    async upload(key, path, immutable) {
      const contentType = key.endsWith('.json') ? 'application/json' : key.endsWith('.sh') ? 'text/plain; charset=utf-8' : 'application/octet-stream';
      aws('s3', 'cp', path, `s3://${bucket}/${key}`, '--only-show-errors', '--content-type', contentType,
        '--cache-control', immutable ? 'public, max-age=31536000, immutable' : 'no-store',
        '--metadata', `sha256=${sha256(readFileSync(path))}`);
      console.log(`Uploaded ${key}`);
    },
    async verify(key, digest) {
      const response = await fetch(`${base}/${key}`, { signal: AbortSignal.timeout(120000) });
      if (!response.ok) throw Error(`Public download failed: ${key}: HTTP ${response.status}`);
      const hash = createHash('sha256');
      for await (const chunk of response.body) hash.update(chunk);
      if (hash.digest('hex') !== digest) throw Error(`Public checksum mismatch: ${key}`);
    },
  };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [command, path] = process.argv.slice(2);
  if (command === 'publish' || command === 'publish-installer') {
    const metadata = JSON.parse(readFileSync(join(path, 'release-metadata.json'), 'utf8'));
    if (metadata.isDraft || metadata.isPrerelease) throw Error('Only published stable releases can be distributed');
    const v = version(process.env.RELEASE_TAG?.replace(/^v/, ''));
    if (metadata.tagName !== `v${v}`) throw Error('Release tag mismatch');
    const output = join(process.env.RUNNER_TEMP, 'r2-distribution');
    if (existsSync(output)) throw Error('Distribution directory must be fresh');
    const base = baseURL(process.env.DOWNLOAD_BASE_URL);
    const linux = process.env.RELEASE_LINUX === 'true';
    const manifests = prepareDistribution(path, output, { version: v, base, team: process.env.APPLE_TEAM_ID, notes: metadata.body, date: metadata.publishedAt, linux });
    // Keep remote reads outside the immutable artifact tree.
    const remote = r2Remote(process.env.RUNNER_TEMP, base);
    if (command === 'publish-installer') await publishInstaller(output, manifests.mobd, remote);
    else await publish(output, manifests, remote);
  } else {
    throw Error('Usage: node release.mjs <publish|publish-installer> <assets-dir>');
  }
}
