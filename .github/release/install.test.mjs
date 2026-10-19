import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { prepareDistribution, sha256 } from './release.mjs';

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'mob-installer-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const bin = join(root, 'bin'), home = join(root, 'home'), downloads = join(root, 'downloads'), payload = join(root, 'payload');
  for (const dir of [bin, home, downloads, payload]) mkdirSync(dir);
  const script = (name, contents) => writeFileSync(join(bin, name), `#!/bin/sh\n${contents}\n`, { mode: 0o755 });
  script('id', 'echo 501');
  script('uname', '[ "$1" = -s ] && echo Darwin || echo arm64');
  script('sw_vers', 'echo 13.0');
  script('launchctl', '[ "${TEST_RUNNING:-0}" = 1 ]');
  script('pgrep', 'exit 1');
  script('codesign', `
[ "$1" = --verify ] && [ "$2" = --strict ] && [ "$3" = --verbose=2 ] && [ "$4" = -R ] || exit 1
[ "$5" = '=anchor apple generic and identifier "com.xrdavies.mobd" and certificate leaf[subject.OU] = "A123456789" and certificate leaf[field.1.2.840.113635.100.6.1.13] exists' ] || { echo 'invalid requirement specification' >&2; exit 1; }
[ -f "$6" ] && [ "\${TEST_BAD_SIGNATURE:-0}" != 1 ]
`);
  script('curl', 'while [ "$#" -gt 0 ]; do case "$1" in https://*) url="$1" ;; -o) shift; dest="$1" ;; esac; shift; done\ncp "$TEST_DOWNLOADS/${url##*/}" "$dest"');
  writeFileSync(join(bin, 'plutil'), `#!${process.execPath}\nconst fs=require('fs'); const args=process.argv.slice(2); let v=JSON.parse(fs.readFileSync(args.at(-1),'utf8')); for(const k of args[1].split('.')) v=v[k]; process.stdout.write(String(v));\n`, { mode: 0o755 });
  writeFileSync(join(payload, 'mobd'), '#!/bin/sh\necho "mobd 1.2.3"\n', { mode: 0o755 });
  writeFileSync(join(payload, 'LICENSE'), 'license');
  writeFileSync(join(payload, 'README.md'), 'readme');
  const archive = join(downloads, 'mobd_1.2.3_darwin_arm64.tar.gz');
  execFileSync('tar', ['-czf', archive, '-C', payload, 'mobd', 'LICENSE', 'README.md'], { env: { ...process.env, COPYFILE_DISABLE: '1' } });
  const manifest = { version: '1.2.3', platforms: { 'darwin-aarch64': { sha256: sha256(readFileSync(archive)) } } };
  for (const name of ['latest.json', 'release.json']) writeFileSync(join(downloads, name), JSON.stringify(manifest));
  const installer = join(root, 'install.sh');
  writeFileSync(installer, readFileSync(new URL('./install.sh', import.meta.url), 'utf8').replaceAll('@DOWNLOAD_BASE_URL@', 'https://download.example.com').replaceAll('@APPLE_TEAM_ID@', 'A123456789'));
  const installed = join(home, '.local/bin/mobd');
  const run = (extra = {}, args = []) => spawnSync('sh', [installer, ...args], { encoding: 'utf8', env: { ...process.env, HOME: home, PATH: `${bin}:${process.env.PATH}`, TEST_DOWNLOADS: downloads, ...extra } });
  return { run, installed, home, downloads, archive };
}

test('installer installs validated payload without changing existing configuration', t => {
  const f = fixture(t);
  mkdirSync(join(f.home, '.mobd'));
  writeFileSync(join(f.home, '.mobd/config.json'), '{"listen":"127.0.0.1:8080"}');
  const result = f.run();
  assert.equal(result.status, 0, result.stderr);
  assert.equal(execFileSync(f.installed, ['--version'], { encoding: 'utf8' }).trim(), 'mobd 1.2.3');
  assert.equal(readFileSync(join(f.home, '.mobd/config.json'), 'utf8'), '{"listen":"127.0.0.1:8080"}');
});

test('checksum and signature failures preserve the installed executable', t => {
  const f = fixture(t);
  mkdirSync(join(f.home, '.local/bin'), { recursive: true });
  writeFileSync(f.installed, 'old binary');
  assert.notEqual(f.run({ TEST_BAD_SIGNATURE: '1' }).status, 0);
  assert.equal(readFileSync(f.installed, 'utf8'), 'old binary');
  writeFileSync(f.archive, 'corrupt archive');
  const result = f.run();
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Checksum mismatch/);
  assert.equal(readFileSync(f.installed, 'utf8'), 'old binary');
});

test('running daemon, invalid versions and unsupported arguments never install', t => {
  const f = fixture(t);
  assert.match(f.run({ TEST_RUNNING: '1' }).stderr, /mobd is running/);
  assert.match(f.run({}, ['--version', '../../bad']).stderr, /Invalid stable version/);
  assert.match(f.run({}, ['--install-dir', 'relative']).stderr, /absolute path/);
  assert.equal(existsSync(f.installed), false);
});

// The Linux branch reads release.json as release.mjs writes it.
test('installer on Linux picks the architecture and verifies its checksum', t => {
  const root = mkdtempSync(join(tmpdir(), 'mob-installer-linux-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const bin = join(root, 'bin'), home = join(root, 'home'), assets = join(root, 'assets'), out = join(root, 'out'), payload = join(root, 'payload');
  for (const dir of [bin, home, assets, payload]) mkdirSync(dir);
  const script = (name, contents) => writeFileSync(join(bin, name), `#!/bin/sh\n${contents}\n`, { mode: 0o755 });
  script('id', 'echo 1000');
  script('uname', '[ "$1" = -s ] && echo Linux || echo "${TEST_ARCH:-x86_64}"');
  script('systemctl', 'exit 1');
  script('pgrep', 'exit 1');
  script('codesign', 'echo "codesign must not run on Linux" >&2; exit 1');
  script('sha256sum', 'shasum -a 256 "$@"');
  writeFileSync(join(payload, 'LICENSE'), 'license');
  writeFileSync(join(payload, 'README.md'), 'readme');
  for (const arch of ['amd64', 'arm64']) {
    writeFileSync(join(payload, 'mobd'), `#!/bin/sh\necho "mobd 1.2.3"\necho ${arch} > "$HOME/arch"\n`, { mode: 0o755 });
    execFileSync('tar', ['-czf', join(assets, `mobd_1.2.3_linux_${arch}.tar.gz`), '-C', payload, 'mobd', 'LICENSE', 'README.md'], { env: { ...process.env, COPYFILE_DISABLE: '1' } });
    writeFileSync(join(assets, `mobd_1.2.3_${arch}.deb`), 'deb');
  }
  writeFileSync(join(assets, 'mobd_1.2.3_darwin_arm64.tar.gz'), 'mac');
  const manifest = prepareDistribution(assets, out, { version: '1.2.3', base: 'https://download.example.com', team: 'A123456789', date: '2026-10-14T00:00:00Z' }).mobd;
  const downloads = join(out, 'mobd/v1.2.3');
  writeFileSync(join(downloads, 'latest.json'), JSON.stringify(manifest, null, 2) + '\n');
  script('curl', `while [ "$#" -gt 0 ]; do case "$1" in https://*) url="$1" ;; -o) shift; dest="$1" ;; esac; shift; done\ncp "${downloads}/\${url##*/}" "$dest"`);
  const installer = join(downloads, 'install.sh');
  const run = (extra = {}) => spawnSync('sh', [installer], { encoding: 'utf8', env: { ...process.env, HOME: home, PATH: `${bin}:${process.env.PATH}`, ...extra } });
  for (const [machine, arch] of [['x86_64', 'amd64'], ['aarch64', 'arm64']]) {
    const result = run({ TEST_ARCH: machine });
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /sudo apt install xvfb openbox/);
    execFileSync(join(home, '.local/bin/mobd'), { env: { ...process.env, HOME: home } });
    assert.equal(readFileSync(join(home, 'arch'), 'utf8').trim(), arch);
  }
  writeFileSync(join(downloads, 'mobd_1.2.3_linux_arm64.tar.gz'), 'corrupt');
  assert.match(run({ TEST_ARCH: 'aarch64' }).stderr, /Checksum mismatch/);
  assert.match(run({ TEST_ARCH: 'riscv64' }).stderr, /x86_64 and arm64/);
});
