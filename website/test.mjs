import { readFileSync, existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';

const root = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(resolve(root, 'index.html'), 'utf8');
const js = readFileSync(resolve(root, 'app.js'), 'utf8');
const css = readFileSync(resolve(root, 'styles.css'), 'utf8');

test('local assets and fragment links resolve', () => {
  const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map((match) => match[1]);
  assert.equal(new Set(ids).size, ids.length, 'IDs must be unique');
  for (const [, link] of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
    if (link === '#') continue;
    if (link.startsWith('#')) assert.ok(ids.includes(link.slice(1)), link);
    else if (!link.startsWith('https:')) assert.ok(existsSync(resolve(root, link)), link);
  }
});

test('page has metadata, one main heading, and no external asset dependencies', () => {
  assert.match(html, /<html lang="en">/);
  assert.match(html, /name="viewport"/);
  assert.match(html, /name="description"/);
  assert.equal((html.match(/<h1\b/g) || []).length, 1);
  assert.ok(!/<(?:script|img)[^>]+src="https?:/i.test(html));
  assert.ok(!/@import|url\(\s*["']?https?:/i.test(css));
});

test('demo does not masquerade as live data or make API calls', () => {
  assert.match(html, /INTERACTIVE DEMO/);
  assert.match(html, /SIMULATED OUTPUT/);
  assert.match(html, /No cluster connection/);
  assert.doesNotMatch(js, /\b(fetch|XMLHttpRequest|WebSocket)\s*\(/);
  new vm.Script(js);
});

test('tabs have matching labelled panels and keyboard support', () => {
  for (const [, id, panel] of html.matchAll(/id="(tab-[^"]+)" role="tab"[^>]+aria-controls="([^"]+)"/g)) {
    assert.ok(html.includes(`id="${panel}" role="tabpanel" aria-labelledby="${id}"`));
  }
  assert.match(js, /ArrowRight/);
  assert.match(js, /ArrowLeft/);
  assert.match(js, /Escape/);
  assert.match(css, /prefers-reduced-motion/);
});

test('setup commands match local Make targets', () => {
  const makefile = readFileSync(resolve(root, '../Makefile'), 'utf8');
  for (const [, command] of html.matchAll(/^make (local-[a-z-]+)$/gm)) {
    assert.ok(makefile.includes(command), command);
  }
  assert.match(html, /not current features/);
  assert.match(html, /no multi-user authentication/);
});
