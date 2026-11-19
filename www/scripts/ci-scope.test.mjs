import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';

const pr = readFileSync('.github/workflows/pr-checks.yml', 'utf8');
const ci = readFileSync('.github/workflows/ci.yml', 'utf8');
const loop = pr.match(/          while IFS= read -r -d '' path; do\n([\s\S]*?)          done < "\$RUNNER_TEMP\/pr-files"/)?.[1];
const ignores = [...ci.matchAll(/^      - '([^']+)'$/gm)].map((match) => match[1]);

function prScope(paths) {
  assert.ok(loop, 'PR path classification exists');
  const script = `platforms=false; website=false\nwhile IFS= read -r -d '' path; do\n${loop}\ndone\nprintf '%s %s' "$platforms" "$website"`;
  const result = spawnSync('bash', ['-c', script], { input: Buffer.from(paths.map((p) => `${p}\0`).join('')), encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  return result.stdout;
}

function ignoredOnPush(path) {
  return ignores.some((pattern) => {
    const regex = pattern.split('**').map((part) => part.split('*').map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('[^/]*')).join('.*');
    return new RegExp(`^${regex}$`).test(path);
  });
}

test('website and README changes skip platforms on PR and main push', () => {
  for (const path of ['README.md', 'README.fr.md', 'www/src/components/CageHome.astro', 'assets/community-qq.png', 'assets/community-wechat.png', 'assets/network-checks.png', 'assets/claude-cage-character.png', 'assets/community-browser-check.png', 'assets/community-browser-check-cropped.png']) {
    assert.equal(prScope([path]), 'false true', path);
    assert.equal(ignoredOnPush(path), true, path);
  }
});

test('mixed changes and unknown assets retain platform checks', () => {
  for (const path of ['assets/new-image.png', 'logo.png', 'internal/workbench/server.go']) {
    assert.match(prScope(['README.md', path]), /^true /, path);
    assert.equal(ignoredOnPush(path), false, path);
  }
});
