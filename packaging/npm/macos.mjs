import { createHash } from 'node:crypto';
import { execFile, spawn } from 'node:child_process';
import { createReadStream } from 'node:fs';
import { lstat, mkdir, mkdtemp, open, readFile, realpath, rm, rmdir } from 'node:fs/promises';
import { homedir, tmpdir } from 'node:os';
import { basename, dirname, isAbsolute, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { promisify } from 'node:util';
import { compareVersions, parseVersion } from './version.mjs';

const run = promisify(execFile);
const bundleID = 'io.github.lee-xeo.claude-cage';
const publisher = 'anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "WBR2J2NFHN"';
const bundleRequirement = publisher + ' and identifier "' + bundleID + '"';
const cliPath = bundle => join(bundle, 'Contents', 'Resources', 'cage');
const tools = { timeout: 180000, maxBuffer: 1024 * 1024, encoding: 'utf8' };

async function verifyBundle(bundle) {
  const cli = cliPath(bundle);
  if (await realpath(cli) !== cli) throw new Error('The Cage CLI must be inside its signed application.');
  await run('/usr/bin/codesign', ['--verify', '--deep', '--strict', '-R=' + bundleRequirement, bundle], tools);
  return cli;
}

async function installedAt(target, minimum) {
  const cli = await verifyBundle(target);
  const info = join(target, 'Contents/Info.plist');
  const { stdout } = await run('/usr/bin/plutil', ['-extract', 'CageVersion', 'raw', '-o', '-', info], tools);
  if (compareVersions(stdout.trim(), minimum) < 0) throw new Error(`Use the existing Cage updater for ${target}, then retry.`);
  return cli;
}

export async function findInstallation() {
  const candidates = new Set();
  for (const directory of [...(process.env.PATH || '').split(':'), join(homedir(), '.local', 'bin')]) {
    if (!isAbsolute(directory)) continue;
    const executable = await realpath(join(directory, 'cage')).catch(() => null);
    if (!executable) continue;
    const bundle = dirname(dirname(dirname(executable)));
    if (bundle.endsWith('.app') && cliPath(bundle) === executable) candidates.add(bundle);
  }
  for (const bundle of [join(homedir(), 'Applications', 'Cage.app'), '/Applications/Cage.app']) {
    const info = await lstat(bundle).catch(error => { if (error.code !== 'ENOENT') throw error; return null; });
    if (info) candidates.add(await realpath(bundle).catch(() => bundle));
  }
  for (const bundle of candidates) {
    try { return await verifyBundle(bundle); } catch { /* Try the next installation. */ }
  }
  if (candidates.size) throw new Error('An existing Cage application could not be verified. Check its Developer ID or update it from its workbench before retrying.');
  return null;
}

async function installReceipt(file) {
  try {
    const info = await lstat(file, { bigint: true });
    if (!info.isFile() || info.size > 16384n) return null;
    return { id: String(info.ino) + '/' + String(info.mtimeNs), result: JSON.parse(await readFile(file, 'utf8')) };
  } catch { return null; }
}

export async function upgradeExisting(cli, installedVersion, requestedVersion) {
  if (compareVersions(installedVersion, '0.2.2-0') < 0) {
    throw new Error(`Cage ${installedVersion} does not support terminal updates. Open the current Cage workbench and update from About Cage, then run your command again. Your installation and running tasks have been kept.`);
  }
  const tty = await open('/dev/tty', 'r+').catch(() => null);
  if (!tty) throw new Error('Open a terminal and update the existing application from About Cage or its native cage update command, then retry.');
  const config = resolve(process.env.CLAUDE_CAGE_CONFIG || join(homedir(), '.claude-cage', 'config.json'));
  const receipt = join(dirname(config), 'updates', 'last-result.json');
  const prior = await installReceipt(receipt);
  try {
    const exit = await new Promise((ok, fail) => {
      const child = spawn(cli, ['update'], { stdio: [tty.fd, tty.fd, tty.fd] });
      child.once('error', fail);
      child.once('close', ok);
    });
    if (exit !== 0) throw new Error('The native Cage updater did not complete. Follow its message and retry.');
  } finally { await tty.close(); }
  const until = Date.now() + 110000;
  while (Date.now() < until) {
    const next = await installReceipt(receipt);
    if (next && next.id !== prior?.id) {
      if (next.result?.success !== true) throw new Error(next.result?.message || 'The installed Cage update did not start successfully.');
      if (compareVersions(next.result.version, requestedVersion) < 0) break;
      return verifyBundle(dirname(dirname(dirname(cli))));
    }
    await delay(500);
  }
  throw new Error(`The installed application has not confirmed Cage ${requestedVersion} or later. Check the update in About Cage and retry.`);
}

async function verifyReleaseCopy(bundle, version, machine) {
  if ((await lstat(bundle)).isSymbolicLink()) throw new Error('The release contains a symbolic link in place of Cage.app.');
  const cli = await verifyBundle(bundle);
  const plist = join(bundle, 'Contents', 'Info.plist');
  const value = async key => (await run('/usr/bin/plutil', ['-extract', key, 'raw', '-o', '-', plist], tools)).stdout.trim();
  if (await value('CageVersion') !== version || await value('CFBundleExecutable') !== 'Cage') throw new Error('Cage.app does not match the requested version or application executable.');
  for (const executable of [join(bundle, 'Contents/MacOS/Cage'), cli, join(bundle, 'Contents/Resources/mihomo'), join(bundle, 'Contents/Resources/CageUpdateInstaller')]) {
    if (await realpath(executable) !== executable) throw new Error('A required Cage program is outside the signed application.');
    await run('/usr/bin/lipo', [executable, '-verify_arch', machine], tools);
    await run('/usr/bin/codesign', ['--verify', '--strict', '-R=' + publisher, executable], tools);
  }
  await run('/usr/sbin/spctl', ['--assess', '--type', 'execute', bundle], tools);
}

// renamex_np(RENAME_EXCL) publishes the already verified directory atomically.
// The system operation refuses an existing destination, including an empty one.
const publishApp = `ObjC.bindFunction('renamex_np', ['int', ['char *', 'char *', 'unsigned int']]);
function run(argv) { return $.renamex_np(argv[0], argv[1], 4); }`;

export async function installRelease(plan, currentCLI) {
  const version = plan.release?.slice(1);
  parseVersion(version);
  const arch = process.arch === 'arm64' ? 'arm64' : process.arch === 'x64' ? 'amd64' : '';
  const asset = `Cage-${version}-macos-${arch}.dmg`;
  if (currentCLI || !arch || plan.unsigned || plan.release !== 'v' + version || plan.platform !== 'darwin-' + arch ||
      plan.asset !== asset || plan.url !== `https://github.com/Lee-xeo/Claude-Cage/releases/download/${plan.release}/${asset}` ||
      typeof plan.path !== 'string' || !isAbsolute(plan.path) || basename(plan.path) !== asset ||
      !/^[a-f0-9]{64}$/.test(plan.sha256)) throw new Error('Invalid official macOS release or existing installation.');
  const file = await lstat(plan.path);
  if (!file.isFile() || file.size !== plan.size || !file.size || file.size > 2 ** 31) throw new Error('The downloaded Cage disk image has an unexpected size or type.');
  const hash = createHash('sha256');
  for await (const block of createReadStream(plan.path)) hash.update(block);
  if (hash.digest('hex') !== plan.sha256) throw new Error('The downloaded Cage disk image failed its SHA-256 check.');
  await run('/usr/bin/codesign', ['--verify', '--strict', '-R=' + publisher, plan.path], tools);
  await run('/usr/sbin/spctl', ['--assess', '--type', 'open', '--context', 'context:primary-signature', plan.path], tools);

  const applications = join(homedir(), 'Applications');
  await mkdir(applications, { recursive: true });
  const folder = await lstat(applications);
  if (!folder.isDirectory() || folder.uid !== process.getuid()) throw new Error('The destination must be your own Applications directory.');
  const target = join(await realpath(applications), 'Cage.app');
  const present = await lstat(target).catch(error => { if (error.code !== 'ENOENT') throw error; return null; });
  if (present) return installedAt(target, version);
  const stage = await mkdtemp(join(dirname(target), '.cage-install-'));
  const copy = join(stage, 'Cage.app');
  try {
    const mount = await mkdtemp(join(await realpath(tmpdir()), 'cage-npm-mount-'));
    let attached = false;
    try {
      await run('/usr/bin/hdiutil', ['attach', '-readonly', '-nobrowse', '-noautoopen', '-mountpoint', mount, plan.path], tools);
      attached = true;
      const source = join(mount, 'Cage.app');
      if ((await lstat(source)).isSymbolicLink()) throw new Error('Invalid application in the official disk image.');
      await run('/usr/bin/ditto', [source, copy], tools);
    } finally {
      if (attached) {
        try { await run('/usr/bin/hdiutil', ['detach', mount], tools); }
        catch { await run('/usr/bin/hdiutil', ['detach', '-force', mount], tools); }
      }
      await rmdir(mount).catch(error => { if (error.code !== 'ENOENT') throw error; });
    }
    await verifyReleaseCopy(copy, version, arch === 'arm64' ? 'arm64' : 'x86_64');
    const { stdout } = await run('/usr/bin/osascript', ['-l', 'JavaScript', '-e', publishApp, '--', copy, target], tools);
    if (stdout.trim() !== '0') {
      if (await lstat(target).catch(() => null)) return installedAt(target, version);
      throw new Error(`Could not publish Cage.app at ${target}. Check the destination permissions and try again.`);
    }
    return cliPath(target);
  } finally { await rm(stage, { recursive: true, force: true }); }
}
