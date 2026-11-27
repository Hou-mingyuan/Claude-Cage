import { execFile, spawn } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { constants, tmpdir } from 'node:os';
import { isAbsolute, join } from 'node:path';
import { promisify } from 'node:util';
import { downloadPlan, makePlan, parseArgs } from '../../www/public/agent-bootstrap.mjs';
import { compareVersions, parseVersion } from './version.mjs';

const run = promisify(execFile);
// Prereleases of 0.2.2 can carry this CLI. Older release lines forward
// unfamiliar words to Claude and must never see the user's arguments.
const cliFloor = '0.2.2-0';
const updateUsage = 'Usage: cage update [--check] [--yes]';
const installationGuide = 'https://claude-cage.com/docs/installation/';
// These commands were already handled by Cage in the first 0.2.2 previews.
// Other first words may be account names and must never reach their Claude fallback.
const legacyManagement = new Set([
  'version', '--version', '-V', 'licenses', 'agent', 'start', 'status', 'up', 'login', 'stop', 'restart',
  'serve', 'desktop', 'install-cli', 'init', 'setup', 'check', 'doctor', 'run', 'shell', 'access', 'name',
  'list', 'info', 'proxy-start', 'proxy-stop', 'update-tools', 'guest-os', 'vm-start', 'hostagent',
  'container-runtime', 'container-configure', 'container-install-script', 'container-network', 'container-tap',
  'server-entry', 'diagnostic-event', 'diagnostics', 'log-relay', 'app-update', 'prepare-update', 'serve-desktop',
  'clipboard-files', 'upload-files', 'browser-file', 'desktop-file', 'share-project', 'interface-language',
  'verify-web-assets', 'verify-vm-assets',
]);

function showLauncherHelp() {
  console.log(`Cage npm launcher
  cage claude [args...]  Run Claude Code with the default CLI account.
  cage <name> [args...]  Use a named Claude account for this invocation.
  cage account list      List account numbers, IDs, names and Claude email addresses.
  cage account rename 03 sarah   Name account 03 so you can run cage sarah.
  cage login             Sign in to Claude in the Cage browser.
  cage start | up | stop | restart | status   Manage the local environment.
  cage version           Install or use the native Cage application and print its version.
  cage update --check    Check for a release without installing.
  Installation guide: ${installationGuide}`);
}

function fetchOfficial(url, options) {
  const target = new URL(url);
  if (target.protocol !== 'https:' || target.port || target.username || target.password ||
      !['github.com', 'api.github.com', 'objects.githubusercontent.com', 'release-assets.githubusercontent.com'].includes(target.hostname)) {
    throw new Error('The release redirect is outside the official distribution service.');
  }
  return fetch(target, options);
}

async function installedVersion(executable) {
  if (!isAbsolute(executable)) throw new Error('Cage returned an invalid installation path.');
  const { stdout } = await run(executable, ['version'], { timeout: 12000, maxBuffer: 4096, windowsHide: true });
  const version = stdout.trim();
  parseVersion(version);
  return version;
}

async function nativeCapabilities(executable, version) {
  if (!parseVersion(version).prerelease.length) return { update: true, accounts: true };
  // Preview builds of the same version can implement different command sets.
  // Query the native help with a fixed argument, before giving it any user input.
  const { stdout } = await run(executable, ['help'], { timeout: 12000, maxBuffer: 256 * 1024, windowsHide: true });
  return {
    help: stdout,
    update: /^cage[ \t]+update[ \t]+[^\r\n]*--check\b/m.test(stdout),
    accounts: /^cage[ \t]+claude[ \t]+\[args\.\.\.\](?=[ \t\r]|$)/m.test(stdout) &&
      /^cage[ \t]+<name>[ \t]+\[args\.\.\.\](?=[ \t\r]|$)/m.test(stdout),
  };
}

async function nativeHelp(adapter) {
  const executable = await adapter.findInstallation();
  if (!executable) return null;
  const version = await installedVersion(executable);
  if (compareVersions(version, cliFloor) < 0) return null;
  const capabilities = await nativeCapabilities(executable, version);
  return capabilities.accounts ? { executable, help: capabilities.help } : null;
}

function workbenchUpdateRequired(installed) {
  return new Error(`Cage ${installed} 尚未提供终端更新命令。请通过现有工作台的更新入口升级，再重试。`);
}

function accountRoutingRequired(installed) {
  return new Error(`Cage ${installed} 尚未支持按名称选择 Claude 账号。请在 Cage 工作台更新应用，再重试。`);
}

async function verifyInstallation(executable, version, existing, update, accounts) {
  const installed = await installedVersion(executable);
  if (existing && compareVersions(installed, version) < 0) {
    throw new Error(`Cage ${version} or later is required. Complete the native update, then retry.`);
  }
  if (!existing && installed !== version) {
    throw new Error(`The installed Cage reports ${installed}; the npm package requested ${version}.`);
  }
  if (update || accounts) {
    const capabilities = await nativeCapabilities(executable, installed);
    if (update && !capabilities.update) throw workbenchUpdateRequired(installed);
    if (accounts && !capabilities.accounts) throw accountRoutingRequired(installed);
  }
  return executable;
}

async function pinnedRelease(options, planFor) {
  const plan = await planFor(options, fetchOfficial);
  if (plan.release !== options.version || plan.unsigned || !Number.isSafeInteger(plan.size) || plan.size <= 0 || plan.size > 2 ** 31) {
    throw new Error('The release does not match this npm package or exceeds the supported installer size.');
  }
  return plan;
}

export async function nativeCommand(version, adapter, args = [], planFor = makePlan, download = downloadPlan) {
  const first = args[0];
  const update = first === 'update';
  if (update && args.length === 2 && ['-h', '--help'].includes(args[1])) {
    console.log(`${updateUsage}\n  --check  Check for a release without installing.\n  --yes    Confirm installation.`);
    return null;
  }
  if (update && args.slice(1).some(arg => !['--check', '--yes', '-y'].includes(arg))) {
    throw Object.assign(new Error(updateUsage), { exitCode: 2 });
  }
  if (args.length === 0 || ['--help', '-h', 'help'].includes(first)) {
    // Built-in help also works when an installation cannot be verified or read.
    const native = await nativeHelp(adapter).catch(() => null);
    if (!native) showLauncherHelp();
    else if (native.help !== undefined) process.stdout.write(native.help);
    else return native.executable;
    return null;
  }
  const checkOnly = update && args.includes('--check');
  const debugHostAgent = first === '--debug' && args.find(arg => arg !== '--debug') === 'hostagent';
  if (!first || /\s/u.test(first) || (first.startsWith('-') && !legacyManagement.has(first) && !debugHostAgent)) {
    throw Object.assign(new Error('请用 cage claude [args...] 传入提示词或 Claude Code 参数，或用 cage <name> [args...] 为本次调用选择账号。'), { exitCode: 2 });
  }
  const accounts = !update && !legacyManagement.has(first) && !debugHostAgent;
  const options = parseArgs(['plan', '--version', version]);
  const existing = await adapter.findInstallation();
  let installed;
  if (existing) {
    installed = await installedVersion(existing);
    if (compareVersions(installed, cliFloor) >= 0) {
      const ready = compareVersions(installed, version) >= 0;
      const usesNativeUpdater = !ready && Boolean(adapter.upgradeExisting);
      const capabilities = update || usesNativeUpdater || (ready && accounts)
        ? await nativeCapabilities(existing, installed) : undefined;
      if (update || usesNativeUpdater) {
        const supported = capabilities.update;
        if (supported && update) return existing;
        if (!supported && !checkOnly && (ready || usesNativeUpdater)) throw workbenchUpdateRequired(installed);
      }
      if (!update && ready) {
        if (accounts && !capabilities.accounts) throw accountRoutingRequired(installed);
        return existing;
      }
    }
  }
  if (checkOnly) {
    console.log(installed ? `本机 Cage ${installed} 尚未提供终端更新检查。` : '本机尚未安装原生 Cage。');
    console.log(`此 npm 包对应 Cage ${version}。安装说明见 ${installationGuide}`);
    const plan = await pinnedRelease(options, planFor);
    console.log(`已确认当前平台的官方发行包 ${plan.asset} 可用。`);
    return null;
  }
  if (compareVersions(version, cliFloor) < 0) {
    throw new Error('The terminal launcher requires Cage 0.2.2 or later. Install the claude-cage npm package for that release once it is published.');
  }
  if (existing && adapter.upgradeExisting) {
    const executable = await adapter.upgradeExisting(existing, installed, version);
    return verifyInstallation(executable, version, existing, update, accounts);
  }
  console.error(`Preparing the official Cage ${version} application for ${options.platform}…`);
  const plan = await pinnedRelease(options, planFor);
  let directory;
  try {
    let installer = plan;
    if (process.platform !== 'linux') {
      directory = await mkdtemp(join(tmpdir(), 'claude-cage-npm-'));
      installer = await download(plan, directory, fetchOfficial);
    }
    const executable = await adapter.installRelease(installer, existing);
    return await verifyInstallation(executable, version, existing, update, accounts);
  } finally {
    if (directory) await rm(directory, { recursive: true, force: true });
  }
}

export async function runNative(executable, args) {
  if (process.platform !== 'win32' && typeof process.execve === 'function') {
    process.execve(executable, [executable, ...args], process.env);
  }
  // Node 20 and Windows have no execve. Inherit the original handles so the
  // native CLI keeps its console, pipes and current project directory.
  const child = spawn(executable, args, { stdio: 'inherit' });
  const signals = process.platform === 'win32' ? ['SIGINT', 'SIGTERM', 'SIGBREAK'] : ['SIGINT', 'SIGTERM', 'SIGHUP'];
  const handlers = new Map();
  for (const signal of signals) {
    const handler = () => {
      // An interactive terminal already sends Ctrl+C to the foreground group.
      if ((signal === 'SIGINT' || signal === 'SIGBREAK') && process.stdin.isTTY) return;
      try { child.kill(signal); } catch { /* Windows may already have delivered the console event. */ }
    };
    handlers.set(signal, handler);
    process.on(signal, handler);
  }
  try {
    const [code, signal] = await new Promise((resolve, reject) => {
      child.once('error', reject);
      child.once('exit', (code, signal) => resolve([code, signal]));
    });
    if (signal && process.platform !== 'win32') {
      for (const [name, handler] of handlers) process.off(name, handler);
      process.kill(process.pid, signal);
    }
    return code ?? 128 + (constants.signals[signal] ?? 1);
  } finally {
    for (const [name, handler] of handlers) process.off(name, handler);
  }
}
