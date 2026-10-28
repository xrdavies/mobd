// make release-patch|-minor|-major and make release VERSION=vX.Y.Z: check main, show what is about
// to be released, ask, then commit the version files, tag that commit and push both atomically.
import { execFileSync } from 'node:child_process';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { PARTS, bumpVersion, compareVersions, increment, readVersion } from './bump.mjs';
import { checkPromotion, version } from './release.mjs';

const NAME = 'mobd';
const USAGE = 'Usage: make release-patch, release-minor or release-major, or make release VERSION=vMAJOR.MINOR.PATCH';
const root = fileURLToPath(new URL('../../', import.meta.url));
const git = (...args) => execFileSync('git', args, {
  cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'],
}).trim();

// Anything but y or yes, including no answer at all, is a no.
function confirm(question) {
  return new Promise(resolve => {
    const rl = createInterface({ input: process.stdin, output: process.stdout });
    rl.on('close', () => resolve(false));
    rl.question(question, answer => { resolve(/^y(es)?$/i.test(answer.trim())); rl.close(); });
  });
}

try {
  const [arg, ...extra] = process.argv.slice(2);
  if (!arg || extra.length || !(PARTS.includes(arg) || arg.startsWith('v'))) throw Error(USAGE);
  const current = readVersion(root);
  const nextVersion = PARTS.includes(arg) ? increment(current, arg) : version(arg.slice(1));
  const tag = `v${nextVersion}`;
  if (git('branch', '--show-current') !== 'main') throw Error('Switch to main before releasing.');
  if (git('status', '--porcelain', '--untracked-files=all')) throw Error('Commit or stash all changes before releasing.');
  if (git('tag', '--list', tag)) throw Error(`Local tag ${tag} already exists; check Actions or push that tag instead of recreating it.`);

  const head = git('rev-parse', 'HEAD');
  const refs = git('ls-remote', 'origin', 'refs/heads/main', 'refs/tags/v*')
    .split('\n').map(line => line.split('\t'));
  if (refs.find(([, ref]) => ref === 'refs/heads/main')?.[0] !== head) {
    throw Error('Local main must match origin/main. Pull or push your commits first, then retry.');
  }
  // Release tags on origin and the commits they point at (the ^{} line of an annotated tag).
  const tags = new Map();
  for (const [sha, ref] of refs) {
    const m = ref?.match(/^refs\/tags\/v([^^]+)(\^\{\})?$/);
    if (m && (m[2] || !tags.has(m[1]))) tags.set(m[1], sha);
  }
  if (tags.has(nextVersion)) throw Error(`Remote tag ${tag} already exists; check Actions for its release.`);
  const released = [...tags.keys()].filter(v => { try { return version(v); } catch { return false; } }).sort(compareVersions);
  const latest = released.at(-1);
  if (PARTS.includes(arg) && latest && compareVersions(latest, current) > 0) {
    throw Error(`The version files say ${current}, but origin already has v${latest}. Release an explicit version instead: make release VERSION=vX.Y.Z`);
  }
  for (const previous of released) {
    checkPromotion({ version: previous }, { version: nextVersion });
    if (tags.get(previous) === head) throw Error(`HEAD is already released as v${previous}; there is nothing new to release.`);
  }

  let log = [];
  try {
    log = execFileSync('git', ['log', '--format=%h %s', ...(latest ? [`${tags.get(latest)}..HEAD`] : ['HEAD'])], {
      cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'],
    }).trim().split('\n').filter(Boolean);
  } catch {}
  console.log(`${NAME} ${latest ? `v${latest}` : `${current} (never released)`} → ${tag}`);
  console.log(`${log.length} commit${log.length === 1 ? '' : 's'} ${latest ? `since v${latest}` : 'on main'}:`);
  for (const line of log.slice(0, 15)) console.log(`  ${line}`);
  if (log.length > 15) console.log(`  … and ${log.length - 15} more`);
  if (!await confirm(`Commit, tag and push ${NAME} ${tag}? [y/N] `)) throw Error('Cancelled; nothing was changed.');

  // The release commit records the version in the repository's files; the tag points at it.
  const changed = bumpVersion(root, nextVersion);
  if (changed.length) {
    git('add', '--', ...changed);
    git('commit', '-q', '-m', `chore(release): ${tag}`);
  }
  const commit = git('rev-parse', 'HEAD');
  const tagRef = `refs/tags/${tag}`;
  git('tag', '-a', tag, commit, '-m', `${NAME} ${tag}`);
  try {
    // Atomic: main and the tag land together or not at all.
    git('-c', 'push.followTags=false', 'push', '--atomic', 'origin', 'HEAD:refs/heads/main', `${tagRef}:${tagRef}`);
  } catch {
    throw Error(`Push failed; the local ${changed.length ? 'release commit and ' : ''}tag ${tag} were kept. Check git ls-remote origin ${tagRef}. If absent, retry: git -c push.followTags=false push --atomic origin main ${tagRef}:${tagRef}. If present, check Actions. Do not move the tag.`);
  }
  console.log(`Pushed ${tag} at ${commit.slice(0, 12)}${changed.length ? ` (release commit: ${changed.join(', ')})` : ''}.`);
  console.log('Next: check Actions → Release mobd, test the draft archive, then publish the stable Release.');
  console.log('Wait for Publish mobd downloads to R2 to finish before announcing the release.');
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
