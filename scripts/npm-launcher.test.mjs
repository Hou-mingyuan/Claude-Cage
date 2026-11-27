import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { cp, mkdir, mkdtemp, readFile, readdir, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { nativeCommand } from '../packaging/npm/launcher.mjs';
import { installRelease as installDebianRelease } from '../packaging/npm/linux.mjs';

async function temp(t) {
  const dir = await mkdtemp(join(tmpdir(), 'cage-npm-test-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  return dir;
}

test('compatible native install is reused offline and legacy commands remain untouched', { skip: process.platform === 'win32' }, async t => {
  const cli = join(await temp(t), 'cage');
  const native = async version => writeFile(cli, `#!/bin/sh\n[ "$#" = 1 ] && [ "$1" = version ] || exit 68\nprintf '%s\\n' "${version}"\n`, { mode: 0o700 });
  const adapter = { findInstallation: async () => cli };
  const noNetwork = () => assert.fail('unexpected download');
  await native('0.3.0');
  assert.equal(await nativeCommand('0.2.2', adapter, ['version'], noNetwork), cli);
  await native('0.2.1');
  await assert.rejects(nativeCommand('0.2.1', adapter, ['version'], noNetwork), /requires Cage 0\.2\.2/);
  adapter.upgradeExisting = async (_cli, old, required) => {
    assert.equal(old, '0.2.1'); assert.equal(required, '0.2.2');
    return cli;
  };
  await assert.rejects(nativeCommand('0.2.2', adapter, ['version'], noNetwork), /Complete the native update/);
});

test('first run pins the tag and verifies the installed native version', { skip: process.platform === 'win32' }, async t => {
  const cli = join(await temp(t), 'cage');
  await writeFile(cli, '#!/bin/sh\nprintf "0.2.3\\n"\n', { mode: 0o700 });
  const adapter = { findInstallation: async () => null, installRelease: async () => cli };
  let tag;
  const planFor = async options => {
    tag = options.version;
    return { release: tag, unsigned: false, size: 12 };
  };
  await assert.rejects(nativeCommand('0.2.2', adapter, ['version'], planFor, async plan => plan), /requested 0\.2\.2/);
  assert.equal(tag, 'v0.2.2');
  await writeFile(cli, '#!/bin/sh\ncase "$1" in version) echo 0.2.2-qa.1;; help) echo "cage update-tools";; *) exit 68;; esac\n', { mode: 0o700 });
  await assert.rejects(nativeCommand('0.2.2-qa.1', adapter, ['update', '--yes'], planFor, async plan => plan), /工作台的更新入口/);
  await assert.rejects(nativeCommand('0.2.2-qa.1', adapter, ['sarah', '-p', 'private prompt'], planFor, async plan => plan), /按名称选择 Claude 账号/);
});

test('npm handles read-only commands and forwards private arguments only to native builds that support them', async t => {
  const dir = await temp(t);
  const root = fileURLToPath(new URL('..', import.meta.url));
  const moduleDir = join(dir, 'packaging/npm');
  await cp(join(root, 'packaging/npm'), moduleDir, { recursive: true });
  for (const name of ['www/public', 'home', 'temp']) await mkdir(join(dir, name), { recursive: true });
  await cp(join(root, 'www/public/agent-bootstrap.mjs'), join(dir, 'www/public/agent-bootstrap.mjs'));
  await writeFile(join(dir, 'package.json'), JSON.stringify({ version: '0.3.0' }));
  const platform = { darwin: 'macos', linux: 'linux', win32: 'windows' }[process.platform];
  await writeFile(join(moduleDir, platform + '.mjs'), `
    export const findInstallation = async () => {
      if (process.env.TEST_NATIVE_VERSION === 'unverified') throw new Error('Installation could not be verified');
      return process.env.TEST_NATIVE_VERSION ? process.execPath : null;
    };
    export const installRelease = () => { throw new Error('Unexpected native installation'); };
    export const upgradeExisting = () => { throw new Error('Unexpected native upgrade'); };
  `);
  // Each simulated native command records if the launcher invoked it.
  const trace = command => `require("node:fs").appendFileSync("native-calls", ${JSON.stringify(command + '\n')});`;
  await writeFile(join(dir, 'version'), trace('version') + 'console.log(process.env.TEST_NATIVE_VERSION)');
  await writeFile(join(dir, 'help'), trace('help') + `
    const preview = process.env.TEST_NATIVE_VERSION.split('.').at(-1);
    console.log(preview === '1' ? 'cage update-tools' : 'cage update [--check] [--yes]');
    if (preview === '6' || preview === '8') console.log('cage claude [args...]   Run Claude Code');
    if (preview === '8') console.log('cage <name> [args...]   Use a named account');
  `);
  await writeFile(join(dir, 'update'), trace('update') + 'console.log(JSON.stringify(process.argv.slice(2)));process.exitCode = 17;');
  for (const command of ['claude', 'sarah', 'status']) {
    await writeFile(join(dir, command), trace(command) + 'console.log(JSON.stringify(process.argv.slice(2)))');
  }
  const asset = 'Cage-0.3.0-' + ({ darwin: 'macos-' + (process.arch === 'arm64' ? 'arm64' : 'amd64') + '.dmg', linux: 'linux-amd64.deb', win32: 'windows-amd64-setup.exe' })[process.platform];
  const distribution = 'https://github.com/Lee-xeo/Claude-Cage/releases/download/v0.3.0/';
  const preload = join(dir, 'release.mjs');
  await writeFile(preload, `
    import { appendFileSync } from 'node:fs';
    globalThis.fetch = async (url, { method }) => {
      appendFileSync('requests', method + ' ' + url.href + '\\n');
      if (url.href === ${JSON.stringify(distribution + 'SHA256SUMS')} && method === 'GET') return new Response('a'.repeat(64) + '  ' + ${JSON.stringify(asset)});
      if (url.href === ${JSON.stringify(distribution + asset)} && method === 'HEAD') return new Response(null, { headers: { 'Content-Length': '42' } });
      throw new Error('Unexpected archive download');
    };
  `);
  const invoke = async (version, args) => {
    await writeFile(join(dir, 'requests'), '');
    await writeFile(join(dir, 'native-calls'), '');
    const result = spawnSync(process.execPath, ['--import', preload, join(moduleDir, 'cage.mjs'), ...args], {
      cwd: dir, encoding: 'utf8', env: {
        ...process.env, HOME: join(dir, 'home'), USERPROFILE: join(dir, 'home'), TMPDIR: join(dir, 'temp'),
        TMP: join(dir, 'temp'), TEMP: join(dir, 'temp'), TEST_NATIVE_VERSION: version,
      },
    });
    assert.deepEqual(await readdir(join(dir, 'home')), []);
    assert.deepEqual(await readdir(join(dir, 'temp')), []);
    return { ...result, calls: await readFile(join(dir, 'native-calls'), 'utf8'), requests: await readFile(join(dir, 'requests'), 'utf8') };
  };
  for (const installed of ['', '0.2.1', '0.2.2-qa.1']) {
    const result = await invoke(installed, ['update', '--yes', '--check']);
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /0\.3\.0/);
    assert.ok(result.stdout.includes(installed || '尚未安装'));
    assert.equal(result.requests, `GET ${distribution}SHA256SUMS\nHEAD ${distribution}${asset}\n`);
  }
  for (const installed of ['0.2.2', '0.2.2-qa.4']) {
    const current = await invoke(installed, ['update', '--check', '-y']);
    assert.equal(current.status, 17);
    assert.equal(current.stdout.trim(), '["--check","-y"]');
    assert.equal(current.requests, '');
  }
  for (const [installed, args] of [['0.2.2-qa.1', ['update', '--yes']], ['0.3.1-qa.1', ['update']], ['0.2.2-qa.1', ['version']]]) {
    const legacy = await invoke(installed, args);
    assert.equal(legacy.status, 1);
    assert.match(legacy.stderr, /工作台的更新入口/);
    assert.equal(legacy.calls, 'version\nhelp\n');
    assert.equal(legacy.requests, '');
  }
  for (const [version, calls] of [['', ''], ['unverified', ''], ['0.2.1', 'version\n'], ['0.3.1-qa.4', 'version\nhelp\n']]) {
    const help = await invoke(version, []);
    assert.equal(help.status, 0, help.stderr);
    assert.ok(help.stdout.includes('cage claude [args...]'));
    assert.ok(help.stdout.includes('cage <name> [args...]'));
    assert.equal(help.calls, calls);
    assert.equal(help.requests, '');
    for (const args of [['-p', 'private prompt'], ['a private prompt'], [''], ['two\twords', 'private prompt']]) {
      const loose = await invoke(version, args);
      assert.equal(loose.status, 2);
      assert.match(loose.stderr, /cage claude/);
      assert.doesNotMatch(loose.stderr + loose.stdout, /private prompt/);
      assert.equal(loose.calls + loose.requests, '');
    }
  }
  const unverified = await invoke('unverified', ['sarah', '-p', 'private prompt']);
  assert.equal(unverified.status, 1);
  assert.match(unverified.stderr, /Installation could not be verified/);
  assert.equal(unverified.calls + unverified.requests, '');
  for (const [version, first] of [['0.3.1-qa.4', 'sarah'], ['0.3.1-qa.6', 'sarah'], ['0.3.1-qa.4', 'claude'], ['0.3.1-qa.4', 'sarahh']]) {
    const legacy = await invoke(version, [first, '-p', 'private prompt']);
    assert.equal(legacy.status, 1);
    assert.match(legacy.stderr, /按名称选择 Claude 账号/);
    assert.doesNotMatch(legacy.stderr + legacy.stdout, /private prompt/);
    assert.equal(legacy.calls, 'version\nhelp\n');
    assert.equal(legacy.requests, '');
  }
  for (const [version, command, calls] of [
    ['0.3.1-qa.8', 'claude', 'version\nhelp\nclaude\n'],
    ['0.3.1-qa.8', 'sarah', 'version\nhelp\nsarah\n'],
    ['0.3.1', 'sarah', 'version\nsarah\n'],
  ]) {
    const args = ['-p', 'a quoted "word" and *', '--output-format', 'json'];
    const ready = await invoke(version, [command, ...args]);
    assert.equal(ready.status, 0, ready.stderr);
    assert.equal(ready.calls, calls);
    assert.deepEqual(JSON.parse(ready.stdout), args);
    assert.equal(ready.requests, '');
  }
  for (const first of ['claude', 'sarah']) {
    const ready = await invoke('0.3.1', [first, 'a private prompt']);
    assert.equal(ready.status, 0, ready.stderr);
    assert.deepEqual(JSON.parse(ready.stdout), ['a private prompt']);
    assert.equal(ready.calls, `version\n${first}\n`);
  }
  const previewHelp = await invoke('0.3.1-qa.8', ['--help']);
  assert.equal(previewHelp.status, 0);
  assert.match(previewHelp.stdout, /Use a named account/);
  assert.equal(previewHelp.calls, 'version\nhelp\n');
  const status = await invoke('0.3.1-qa.4', ['status']);
  assert.equal(status.status, 0, status.stderr);
  assert.equal(status.calls, 'version\nstatus\n');
  const nativeUpdate = await invoke('0.2.2-qa.4', ['update', '--yes']);
  assert.equal(nativeUpdate.status, 17);
  assert.equal(nativeUpdate.stdout.trim(), '["--yes"]');
  assert.equal(nativeUpdate.requests, '');
  for (const [args, code] of [[['update', '--help'], 0], [['--help'], 0], [['update', '--check', 'bad'], 2], [['update', '--help', '-y'], 2]]) {
    const result = await invoke('', args);
    assert.equal(result.status, code);
    assert.match(result.stdout + result.stderr, /cage update/);
    assert.equal(result.requests, '');
  }
});

test('Debian installation rejects a different URL or asset before authorization', async () => {
  const plan = {
    release: 'v0.2.2', platform: 'linux-amd64', asset: 'Cage-0.2.2-linux-amd64.deb',
    url: 'https://github.com/Lee-xeo/Claude-Cage/releases/download/v0.2.2/Cage-0.2.2-linux-amd64.deb',
    size: 100, sha256: 'a'.repeat(64),
  };
  for (const change of [{ url: 'file:///tmp/example.deb' }, { asset: 'other.deb' }, { size: 2 ** 31 + 1 }, { sha256: 'bad' }]) {
    await assert.rejects(installDebianRelease({ ...plan, ...change }), /Invalid official Debian release/);
  }
});

test('native handoff preserves arguments, cwd, binary stdin, stdout and exit status', async t => {
  const dir = await temp(t);
  const module = new URL('../packaging/npm/launcher.mjs', import.meta.url).href;
  const runner = join(dir, 'run.mjs');
  await writeFile(runner, `import { runNative } from ${JSON.stringify(module)}; process.exitCode = await runNative(process.execPath, process.argv.slice(2));`);
  const program = 'const fs=require("node:fs");fs.writeFileSync(1, fs.readFileSync(0));process.stderr.write(JSON.stringify([process.cwd(),...process.argv.slice(1)]));process.exitCode=23;';
  const args = ['-p', 'two words', 'quote"and*', '--'];
  const input = Buffer.from([0, 10, 255, 9]);
  const result = spawnSync(process.execPath, [runner, '-e', program, '--', ...args], { cwd: dir, input });
  assert.equal(result.status, 23);
  assert.deepEqual(result.stdout, input);
  assert.deepEqual(JSON.parse(result.stderr.toString()), [await realpath(dir), ...args]);
});

test('npm tarball contains both aliases, all platform adapters and no installation lifecycle', async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const pkg = JSON.parse(await readFile(join(root, 'package.json')));
  assert.equal(pkg.name, 'claude-cage');
  assert.equal(pkg.bin.cage, pkg.bin['claude-cage']);
  assert.deepEqual(Object.keys(pkg.dependencies || {}), []);
  assert.deepEqual(Object.keys(pkg.scripts || {}), []);
  for (const name of [pkg.bin.cage, 'packaging/npm/linux.mjs', 'packaging/npm/macos.mjs', 'packaging/npm/windows.mjs', 'www/public/agent-bootstrap.mjs']) {
    assert.ok(pkg.files.includes(name)); await readFile(join(root, name));
  }
});
