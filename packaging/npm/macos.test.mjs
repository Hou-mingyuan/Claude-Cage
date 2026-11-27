import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { lstat, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { findInstallation, installRelease, upgradeExisting } from './macos.mjs';

test('a pre-CLI installation is protected before its executable runs', async () => {
  await assert.rejects(upgradeExisting('/missing/old-installation/cage', '0.2.1', '0.2.2'), /About Cage/);
});

test('an unverified local app cannot trigger a parallel installation', { skip: process.platform !== 'darwin' }, async t => {
  if (await lstat('/Applications/Cage.app').catch(() => null)) return t.skip('system-wide Cage can take precedence');
  const home = await mkdtemp(join(tmpdir(), 'cage-npm-existing-'));
  const before = { home: process.env.HOME, path: process.env.PATH };
  try {
    process.env.HOME = home;
    process.env.PATH = '/usr/bin:/bin';
    await mkdir(join(home, 'Applications/Cage.app'), { recursive: true });
    await assert.rejects(findInstallation(), /existing Cage application could not be verified/);
  } finally {
    process.env.HOME = before.home;
    process.env.PATH = before.path;
    await rm(home, { recursive: true, force: true });
  }
});

test('rejects altered disk image before launching a system installer', { skip: process.platform !== 'darwin' }, async () => {
  const dir = await mkdtemp(join(tmpdir(), 'cage-npm-mac-test-'));
  try {
    const arch = process.arch === 'arm64' ? 'arm64' : 'amd64';
    const asset = `Cage-0.2.2-macos-${arch}.dmg`;
    const plan = { release: 'v0.2.2', asset, platform: 'darwin-' + arch, size: 3, path: join(dir, asset),
      url: 'https://github.com/Lee-xeo/Claude-Cage/releases/download/v0.2.2/' + asset,
      sha256: createHash('sha256').update('old').digest('hex') };
    await writeFile(plan.path, 'new');
    await assert.rejects(installRelease(plan, null), /SHA-256/);
    await assert.rejects(installRelease({ ...plan, unsigned: true }, null), /Invalid official/);
    await assert.rejects(installRelease(plan, '/existing/cage'), /existing installation/);
  } finally { await rm(dir, { recursive: true, force: true }); }
});
