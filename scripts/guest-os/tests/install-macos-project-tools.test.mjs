import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

// The installer only runs as root inside a Cage macOS guest, so the tests
// source its pure version/receipt decision helpers directly and assert the
// consumer-visible decisions and failure messages.
const installerPath = resolve('scripts/guest-os/install-macos-project-tools.sh');
const source = readFileSync(installerPath, 'utf8');

const helpersStart = source.indexOf('normalize_version() {');
const gateStart = source.indexOf('require_gui_install_mode() {');
assert.ok(helpersStart > 0, 'normalize_version helper not found');
assert.ok(gateStart > helpersStart, 'decision helpers not found after normalize_version');
const helpersEnd = source.indexOf('\n}', gateStart);
assert.ok(helpersEnd > gateStart, 'require_gui_install_mode end not found');
const helpers = source.slice(helpersStart, helpersEnd + 2);

function runBash(snippet) {
  const script = [
    'set -euo pipefail',
    'driver=/Library/Filesystems/macfuse.fs',
    "pinned_version='5.5.0'",
    helpers,
    snippet,
  ].join('\n');
  return spawnSync('/bin/bash', ['-c', script], { encoding: 'utf8' });
}

// decide_macfuse_action reports its choice through macfuse_action on success,
// and through decision_error plus a failing status on refusal.
function runDecision(installed, receipt) {
  return runBash(
    `if decide_macfuse_action '${installed}' '${receipt}'; then ` +
      `printf '%s\\n' "$macfuse_action"; ` +
      `else printf '%s\\n' "$decision_error" >&2; exit 3; fi`,
  );
}

test('a guest without macFUSE takes the fresh install path', () => {
  const result = runDecision('', '');
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'install');
});

test('an up-to-date bundle with a matching receipt needs no installer run', () => {
  const result = runDecision('5.5.0', '5.5.0');
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'none');
});

test('a newer installation with a matching receipt stays up to date', () => {
  const result = runDecision('5.6.0', '5.6.0');
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout.trim(), 'none');
});

test('an older installation is upgraded instead of failing before the installer', () => {
  for (const installed of ['5.4.2', '5.4', '4.9.9']) {
    const result = runDecision(installed, '5.4.2');
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), 'upgrade', installed);
  }
});

test('a newer installation with an abnormal receipt refuses to downgrade', () => {
  for (const receipt of ['', '5.4.2', 'garbage']) {
    const result = runDecision('5.6.0', receipt);
    assert.equal(result.status, 3, result.stdout);
    assert.match(result.stderr, /newer than the pinned 5\.5\.0 installer/);
    assert.match(result.stderr, /downgrade/);
    assert.match(result.stderr, /Keep the existing installation/);
    assert.match(
      result.stderr,
      /pkgutil --pkg-info io\.macfuse\.installer\.components\.core/,
      receipt,
    );
    assert.doesNotMatch(result.stderr, /--forget|remove|retried/i);
  }
});

test('an unrecognized installation is diagnosed without replacing it', () => {
  for (const installed of ['beta1', '5.5.0-beta1']) {
    const result = runDecision(installed, '');
    assert.equal(result.status, 3, result.stdout);
    assert.match(result.stderr, /unrecognized/);
    assert.match(result.stderr, /Keep the existing installation/);
    assert.match(
      result.stderr,
      /plutil -p \/Library\/Filesystems\/macfuse\.fs\/Contents\/Info\.plist/,
      installed,
    );
    assert.match(result.stderr, /will not replace a build it cannot identify/);
    assert.doesNotMatch(result.stderr, /--forget|remove|retried/i);
  }
});

test('a receipt that is missing, stale, or unparseable triggers a repair', () => {
  const cases = [
    ['5.5.0', ''],
    ['5.5.0', '5.4.2'],
    ['5.5.0', '5.6.0'],
    ['5.5.0', 'garbage'],
  ];
  for (const [installed, receipt] of cases) {
    const result = runDecision(installed, receipt);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), 'repair', `${installed} / ${receipt}`);
  }
});

test('version comparison is numeric per component, not lexicographic', () => {
  const cases = [
    [['5.5.0', '5.5.0'], '='],
    [['5.10.0', '5.9.0'], '>'],
    [['5.9.0', '5.10.0'], '<'],
    [['4.9.9', '5.0.0'], '<'],
    [['5.5.1', '5.5.0'], '>'],
  ];
  for (const [[a, b], expected] of cases) {
    const result = runBash(`version_cmp '${a}' '${b}'`);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), expected, `${a} vs ${b}`);
  }
});

test('normalize_version accepts two- and three-part versions with leading zeros', () => {
  const cases = [
    ['5.5', '5.5.0'],
    ['5.05.0', '5.5.0'],
    ['05.05.00', '5.5.0'],
  ];
  for (const [input, expected] of cases) {
    const result = runBash(`normalize_version '${input}'`);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), expected, input);
  }
  const bad = runBash("normalize_version '5.5.0-beta' >/dev/null");
  assert.equal(bad.status, 1);
});

test('an existing installation cannot be touched without the GUI installer mode', () => {
  const fresh = runBash('require_gui_install_mode install');
  assert.equal(fresh.status, 0, fresh.stderr);
  for (const action of ['upgrade', 'repair']) {
    const result = runBash(
      `if require_gui_install_mode ${action}; then exit 9; fi; printf '%s\\n' "$decision_error"`,
    );
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /Apple Installer GUI/);
    assert.match(result.stdout, /--gui-authorize/);
  }
});
