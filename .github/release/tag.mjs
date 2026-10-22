import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { bumpVersion } from './bump.mjs';
import { checkPromotion, version } from './release.mjs';

const root = fileURLToPath(new URL('../../', import.meta.url));
const git = (...args) => execFileSync('git', args, {
  cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'],
}).trim();

try {
  const [tag, ...extra] = process.argv.slice(2);
  if (!tag?.startsWith('v') || extra.length) throw Error('Usage: make release VERSION=vMAJOR.MINOR.PATCH');
  const nextVersion = version(tag.slice(1));
  if (git('branch', '--show-current') !== 'main') throw Error('Switch to main before releasing.');
  if (git('status', '--porcelain', '--untracked-files=all')) throw Error('Commit or stash all changes before releasing.');
  if (git('tag', '--list', tag)) throw Error(`Local tag ${tag} already exists; check Actions or push that tag instead of recreating it.`);

  const head = git('rev-parse', 'HEAD');
  const refs = git('ls-remote', '--refs', 'origin', 'refs/heads/main', 'refs/tags/v*')
    .split('\n').map(line => line.split('\t'));
  if (refs.find(([, ref]) => ref === 'refs/heads/main')?.[0] !== head) {
    throw Error('Local main must match origin/main. Pull or push your commits first, then retry.');
  }
  const tagRef = `refs/tags/${tag}`;
  for (const [, ref] of refs) {
    if (ref === tagRef) throw Error(`Remote tag ${tag} already exists; check Actions for its release.`);
    if (!ref?.startsWith('refs/tags/v')) continue;
    let previous;
    try { previous = version(ref.slice('refs/tags/v'.length)); }
    catch { continue; }
    checkPromotion({ version: previous }, { version: nextVersion });
  }

  // The release commit records the version in the repository's files; the tag points at it.
  const changed = bumpVersion(root, nextVersion);
  if (changed.length) {
    git('add', '--', ...changed);
    git('commit', '-q', '-m', `chore(release): ${tag}`);
  }
  const commit = git('rev-parse', 'HEAD');
  git('tag', '-a', tag, commit, '-m', `mobd ${tag}`);
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
