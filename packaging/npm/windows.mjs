import { execFile } from 'node:child_process';
import { lstat, readFile, realpath } from 'node:fs/promises';
import { win32 as path } from 'node:path';
import { promisify } from 'node:util';

const run = promisify(execFile);
const appID = 'io.github.lee-xeo.claude-cage';
const registryLocation = String.raw`
$base = [Microsoft.Win32.RegistryKey]::OpenBaseKey('CurrentUser', 'Registry64')
try {
  $key = $base.OpenSubKey('Software\Microsoft\Windows\CurrentVersion\Uninstall\ClaudeCage')
  if ($key) {
    try { [Console]::Write([string]$key.GetValue('InstallLocation', '')) } finally { $key.Dispose() }
  }
} finally { $base.Dispose() }
`;

async function powershell(script, extraEnv = {}) {
  const root = process.env.SystemRoot;
  if (!root || !path.isAbsolute(root)) throw new Error('Cannot locate Windows PowerShell in the system directory.');
  const exe = path.join(root, 'System32', 'WindowsPowerShell', 'v1.0', 'powershell.exe');
  const preamble = "$ErrorActionPreference = 'Stop'\n$ProgressPreference = 'SilentlyContinue'\n[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)\n";
  // Windows PowerShell can wrap its error stream in CLIXML when its handles
  // are redirected. Use stdout for one structured failure and keep stderr private.
  const onFailure = '\n} catch { [Console]::Out.WriteLine((@{error=$_.Exception.Message} | ConvertTo-Json -Compress)); exit 1 }';
  const command = Buffer.from(preamble + 'try {\n' + script + onFailure, 'utf16le').toString('base64');
  const { stdout } = await run(exe, ['-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', command], {
    env: { ...process.env, ...extraEnv }, windowsHide: true, maxBuffer: 65536,
  }).catch(error => {
    // execFile includes the entire encoded script in its default error.
    let reason;
    try { reason = JSON.parse(error.stdout).error; } catch { /* The system process could not start. */ }
    throw new Error(typeof reason === 'string' && reason.length ? reason : 'Windows could not complete the Cage installation check.');
  });
  return stdout.trim();
}

export async function findInstallation() {
  const location = await powershell(registryLocation);
  if (!location) return null;
  if (!/^[A-Za-z]:[\\/]/.test(location) || path.parse(location).root === path.normalize(location)) {
    throw new Error('Windows has an invalid Cage installation location. Repair the registered installation.');
  }
  const root = path.normalize(location);
  const cli = path.join(root, 'resources', 'cage.exe');
  const marker = path.join(root, 'Cage.install-id');
  for (const file of [marker, path.join(root, 'Cage.exe'), cli]) {
    if (!(await lstat(file)).isFile() || (await realpath(file)).toLowerCase() !== file.toLowerCase()) {
      throw new Error('The registered Cage installation contains a missing or redirected program. Repair it using the official installer.');
    }
  }
  if ((await readFile(marker, 'utf8')).trim() !== appID) throw new Error('The registered installation does not belong to Cage.');
  return cli;
}

// Hold the exact download open against writes and renames until NSIS exits.
// Authenticode verifies the certificate chain. Match the official organization
// and country used by the native updater, including after certificate renewal.
const install = String.raw`
$file = $env:CAGE_NPM_INSTALLER
$handle = [IO.File]::Open($file, 'Open', 'Read', 'Read')
try {
  $sha = [Security.Cryptography.SHA256]::Create()
  try { $hash = [BitConverter]::ToString($sha.ComputeHash($handle)).Replace('-', '') } finally { $sha.Dispose() }
  if ($hash -ne $env:CAGE_NPM_DIGEST) { throw 'The installer SHA-256 changed after download.' }
  $signature = Get-AuthenticodeSignature -LiteralPath $file
  if ($signature.Status -ne 'Valid' -or !$signature.SignerCertificate) { throw 'Windows could not verify the Authenticode signature of the Cage installer.' }
  $parts = $signature.SignerCertificate.SubjectName.Decode('UseNewLines') -split '[\r\n]+' | ForEach-Object { $_.Trim() }
  $org = @($parts | Where-Object { $_ -match '^O=' })
  $country = @($parts | Where-Object { $_ -match '^C=' })
  if ($org.Count -ne 1 -or $country.Count -ne 1 -or $org[0] -cne 'O=LI SHI HAO' -or $country[0] -cne 'C=CN') { throw 'The installer is not signed by the official Cage publisher.' }
  $info = [Diagnostics.FileVersionInfo]::GetVersionInfo($file)
  if ($info.ProductName -cne 'Cage' -or $info.ProductVersion -cne $env:CAGE_NPM_VERSION) { throw 'The signed installer has a different product name or version.' }
  $start = [Diagnostics.ProcessStartInfo]::new($file, '/S')
  $start.UseShellExecute = $false
  $start.WorkingDirectory = [IO.Path]::GetDirectoryName($file)
  foreach ($name in @('CAGE_NPM_INSTALLER','CAGE_NPM_DIGEST','CAGE_NPM_VERSION')) { $start.EnvironmentVariables.Remove($name) }
  $installer = [Diagnostics.Process]::Start($start)
  try {
    $installer.WaitForExit()
    if ($installer.ExitCode -ne 0) { throw ('Cage setup exited with code ' + $installer.ExitCode + '. Save your work, stop the environment and quit Cage before retrying.') }
  } finally { $installer.Dispose() }
} finally { $handle.Dispose() }
`;

export async function installRelease(plan, currentCLI) {
  const version = plan.release.replace(/^v/, '');
  if (plan.platform !== 'win32-amd64' || !/^\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?$/.test(version) ||
      plan.asset !== `Cage-${version}-windows-amd64-setup.exe` || !path.isAbsolute(plan.path) ||
      path.basename(plan.path) !== plan.asset || !/^[a-f0-9]{64}$/i.test(plan.sha256)) {
    throw new Error('This release has no official signed Cage installer for Windows x64.');
  }
  // The per-user installer uses the original registered path for an upgrade,
  // preserves user data, and checks that any existing installation can be replaced.
  if ((await findInstallation()) !== currentCLI) throw new Error('The registered Cage installation changed. Retry the command.');
  process.stderr.write(`Installing the official Cage ${version} package for this Windows user…\n`);
  await powershell(install, { CAGE_NPM_INSTALLER: plan.path, CAGE_NPM_DIGEST: plan.sha256, CAGE_NPM_VERSION: version });
  const installed = await findInstallation();
  if (!installed || (currentCLI && installed.toLowerCase() !== currentCLI.toLowerCase())) {
    throw new Error('Setup finished without registering Cage in the expected location.');
  }
  return installed;
}
