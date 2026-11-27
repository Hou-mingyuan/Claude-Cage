import { spawn } from 'node:child_process';
import { lstat, realpath } from 'node:fs/promises';
import { parseVersion } from './version.mjs';

const cli = '/usr/lib/claude-cage/cage';

// Privileged commands are fixed system programs. sudo prompts on the terminal,
// independently of Claude's stdin. No npm file ever runs as root.
function system(executable, args, { root = false, capture = false } = {}) {
  if (root && process.getuid() !== 0) {
    args = ['--', executable, ...args];
    executable = '/usr/bin/sudo';
  }
  return new Promise((resolve, reject) => {
    const child = spawn(executable, args, {
      cwd: '/', env: { PATH: '/usr/sbin:/usr/bin:/sbin:/bin', LC_ALL: 'C', TERM: process.env.TERM || 'dumb' },
      stdio: ['ignore', capture ? 'pipe' : process.stderr, process.stderr],
    });
    let output = '';
    if (capture) {
      child.stdout.setEncoding('utf8');
      child.stdout.on('data', data => {
        output += data;
        if (output.length > 16384) child.kill();
      });
    }
    child.once('error', reject);
    child.once('close', (code, signal) => {
      if (code === 0) resolve(output.trim());
      else reject(new Error(`${executable} ended with ${signal || code}. See the system diagnostic above.`));
    });
  });
}

export async function findInstallation() {
  let info;
  try { info = await lstat(cli); }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
  const directory = await lstat('/usr/lib/claude-cage');
  if (!info.isFile() || info.uid !== 0 || (info.mode & 0o022) || !(info.mode & 0o111) ||
      !directory.isDirectory() || directory.uid !== 0 || (directory.mode & 0o022) || await realpath(cli) !== cli) {
    throw new Error('Check the ownership and permissions of the system Cage installation.');
  }
  const owner = await system('/usr/bin/dpkg-query', ['--search', cli], { capture: true });
  const state = await system('/usr/bin/dpkg-query', ['--show', '--showformat=${db:Status-Status} ${Architecture}', 'claude-cage'], { capture: true });
  if (owner !== 'claude-cage: ' + cli || state !== 'installed amd64') {
    throw new Error('The Cage executable does not belong to an installed claude-cage amd64 system package.');
  }
  return cli;
}

export async function installRelease(plan) {
  const version = plan.release.slice(1);
  parseVersion(version);
  const name = `Cage-${version}-linux-amd64.deb`;
  const url = `https://github.com/Lee-xeo/Claude-Cage/releases/download/v${version}/${name}`;
  if (plan.release !== 'v' + version || plan.asset !== name || plan.url !== url || plan.platform !== 'linux-amd64' ||
      !/^[a-f0-9]{64}$/.test(plan.sha256) || !Number.isSafeInteger(plan.size) || plan.size <= 0 || plan.size > 2 ** 31) {
    throw new Error('Invalid official Debian release information.');
  }
  const asRoot = { root: true }, readRoot = { root: true, capture: true };
  const stage = await system('/usr/bin/mktemp', ['-d', '/var/tmp/claude-cage-npm.XXXXXXXX'], readRoot);
  if (!/^\/var\/tmp\/claude-cage-npm\.[A-Za-z0-9]{8}$/.test(stage)) throw new Error('Invalid system staging directory.');
  const archive = stage + '/claude-cage.deb';
  try {
    // APT can drop network acquisition to its dedicated sandbox account. The
    // calling user cannot change the directory or the package at any point.
    await system('/usr/bin/chown', ['_apt:root', stage], asRoot);
    await system('/usr/lib/apt/apt-helper', ['-o', 'Acquire::Retries=2', 'download-file', url, archive, 'SHA256:' + plan.sha256], asRoot);
    await system('/usr/bin/chown', ['--no-dereference', 'root:root', stage, archive], asRoot);
    const file = await system('/usr/bin/stat', ['--format=%F %s', archive], readRoot);
    if (file !== 'regular file ' + plan.size) throw new Error('APT did not acquire the expected regular package file.');
    const checksum = await system('/usr/bin/sha256sum', [archive], readRoot);
    const fields = await system('/usr/bin/dpkg-deb', ['--show', '--showformat=${Package}\t${Version}\t${Architecture}', archive], readRoot);
    if (checksum !== plan.sha256 + '  ' + archive || fields !== 'claude-cage\t' + version.replace('-', '~') + '\tamd64') {
      throw new Error('The acquired .deb does not match the official digest, package name, architecture or version.');
    }
    await system('/usr/bin/chmod', ['0644', archive], asRoot);
    await system('/usr/bin/chmod', ['0755', stage], asRoot);
    await system('/usr/bin/apt-get', ['-o', 'DPkg::Lock::Timeout=60', '--no-remove', '--assume-yes', 'install', archive], asRoot);
    return await findInstallation();
  } finally {
    // Remove only the unique root-owned directory just returned by mktemp.
    await system('/usr/bin/rm', ['-rf', '--', stage], asRoot).catch(() => console.error('Cage: could not remove ' + stage));
  }
}
