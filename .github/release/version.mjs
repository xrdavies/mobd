// make version: the version in the version files, the last release tag and what would come next.
// --check [vX.Y.Z] [--ref COMMIT]: fail unless the version files agree (and match the tag), read
// from the working tree or from COMMIT. CI runs it on main; the release workflow on the tag.
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { PARTS, VERSION_FILES, compareVersions, increment, readVersion } from './bump.mjs';
import { version } from './release.mjs';

const NAME = 'mobd';
const root = fileURLToPath(new URL('../../', import.meta.url));
const git = (...args) => execFileSync('git', args, {
  cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'],
}).trim();

try {
  const args = process.argv.slice(2);
  if (args[0] === '--check') {
    const tag = args[1]?.startsWith('v') ? args[1] : undefined;
    const rest = args.slice(tag ? 2 : 1);
    if (rest.length && (rest.length !== 2 || rest[0] !== '--ref')) throw Error('Usage: version.mjs --check [vX.Y.Z] [--ref COMMIT]');
    const ref = rest[1];
    const found = readVersion(root, ref ? file => git('show', `${ref}:${file}`) + '\n' : undefined);
    if (tag && found !== version(tag.slice(1))) {
      throw Error(`Tag ${tag} points at a commit whose version files say ${found}; tag the release with make release.`);
    }
    console.log(`${NAME} ${found} in ${VERSION_FILES.join(', ')}${tag ? `, matching ${tag}` : ''}.`);
  } else if (args.length) {
    throw Error('Usage: make version');
  } else {
    const current = readVersion(root);
    const tags = git('tag', '--list', 'v*').split('\n').map(t => t.slice(1)).filter(v => {
      try { return version(v); } catch { return false; }
    }).sort(compareVersions);
    const latest = tags.at(-1);
    console.log(`${NAME} ${current} (${VERSION_FILES.join(', ')})`);
    if (latest) {
      const count = Number(git('rev-list', '--count', `v${latest}..HEAD`));
      console.log(`Last release tag: v${latest}, ${count} commit${count === 1 ? '' : 's'} since (local tags; git fetch --tags to refresh)`);
      if (compareVersions(latest, current) > 0) console.log(`The version files are behind v${latest}; release with make release VERSION=vX.Y.Z.`);
    } else {
      console.log('Not released yet (no local release tags).');
    }
    console.log(`Next: ${PARTS.slice().reverse().map(p => `make release-${p} → v${increment(current, p)}`).join(' · ')}`);
  }
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
