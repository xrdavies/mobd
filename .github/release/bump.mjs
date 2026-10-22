// make release writes the version into the files that carry it, then commits them before tagging.
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

export const VERSION_FILES = ['cmd/mobd/main.go'];

// Updates the version files under root; returns the ones that changed. Nothing is written unless
// every file has the expected shape.
export function bumpVersion(root, version) {
  const file = 'cmd/mobd/main.go';
  const before = readFileSync(join(root, file), 'utf8');
  const line = /^var version = "[^"]*"$/gm;
  if ((before.match(line) ?? []).length !== 1) throw Error(`${file}: expected one var version line`);
  const after = before.replace(line, `var version = "${version}"`);
  if (after === before) return [];
  writeFileSync(join(root, file), after);
  return [file];
}
