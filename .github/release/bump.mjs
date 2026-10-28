// make release writes the version into the files that carry it, then commits them before tagging.
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { version } from './release.mjs';

export const VERSION_FILES = ['cmd/mobd/main.go'];

const LINE = /^var version = "([^"]*)"$/gm;

// The version the version files agree on. read(file) returns a file's text; by default from root.
export function readVersion(root, read = file => readFileSync(join(root, file), 'utf8')) {
  const file = VERSION_FILES[0];
  const found = [...read(file).matchAll(LINE)];
  if (found.length !== 1) throw Error(`${file}: expected one var version line`);
  return version(found[0][1]);
}

// Updates the version files under root; returns the ones that changed. Nothing is written unless
// every file has the expected shape.
export function bumpVersion(root, version) {
  const file = VERSION_FILES[0];
  const before = readFileSync(join(root, file), 'utf8');
  if ((before.match(LINE) ?? []).length !== 1) throw Error(`${file}: expected one var version line`);
  const after = before.replace(LINE, `var version = "${version}"`);
  if (after === before) return [];
  writeFileSync(join(root, file), after);
  return [file];
}

export const PARTS = ['major', 'minor', 'patch'];

// The next version after current for make release-major, -minor or -patch.
export function increment(current, part) {
  const i = PARTS.indexOf(part);
  if (i < 0) throw Error(`Expected one of ${PARTS.join(', ')}`);
  const n = version(current).split('.').map(Number);
  n[i]++;
  n.fill(0, i + 1);
  return n.join('.');
}

export function compareVersions(a, b) {
  const x = version(a).split('.').map(BigInt), y = version(b).split('.').map(BigInt);
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] < y[i] ? -1 : 1;
  return 0;
}
