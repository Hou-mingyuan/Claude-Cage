[CmdletBinding()]
param(
    [string]$Profile = 'C:\ProgramData\Cage Remote\windows-guest.json',
    [Parameter(Mandatory)][string]$Source,
    [switch]$SkipEditor
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]::new($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run the remote component installer as administrator.' }
$binding = Get-Content -LiteralPath $Profile -Raw -Encoding UTF8 | ConvertFrom-Json
$machine = [guid](Get-ItemPropertyValue -LiteralPath 'HKLM:\SOFTWARE\Microsoft\Cryptography' -Name MachineGuid)
if ($binding.mode -ne 'remote' -or [guid]$binding.machine_id -ne $machine) { throw 'This is not the bound remote Windows machine.' }
$account = Get-LocalUser -Name $binding.gui_user
if ($account.SID.Value -ne $binding.gui_user_sid -or (Get-LocalGroupMember -SID 'S-1-5-32-544' | Where-Object { $_.SID.Value -eq $account.SID.Value })) { throw 'The bound workbench account must be a standard local user.' }
$bridge = Split-Path -Parent $binding.relay_executable
$cache = Join-Path (Split-Path -Parent $Profile) 'component-downloads'
$null = New-Item -ItemType Directory -Force -Path $cache
$packages = Get-Content -LiteralPath (Join-Path $Source 'windows-workbench-packages.json') -Raw -Encoding UTF8 | ConvertFrom-Json
. (Join-Path $Source 'windows-workbench-packages.ps1')
# Reuse the product's pinned-package verification. Only the download transport
# differs from the host installer; no system proxy, PATH or account is changed.
function Invoke-WorkbenchDownload([string]$Address, [string]$Target, [string]$Name, [string]$Cache, [string]$Digest = '') {
    [Console]::WriteLine('Downloading ' + $Name)
    & "$env:SystemRoot\System32\curl.exe" --fail --location --silent --show-error --connect-timeout 20 --max-time 300 --output $Target $Address
    if ($LASTEXITCODE) { throw ('Could not download ' + $Name) }
}
$files = @{}
$names = @('winfsp','sshfs','node','playwright','playwrightCore')
if (-not $SkipEditor) { $names += @('editorCLI','editorServer') }
foreach ($name in $names) {
    $files[$name] = Get-PinnedPackage $packages.$name $cache
    Write-Output ('Package ready: ' + $name)
}
foreach ($name in @('winfsp','sshfs')) {
    $present = if ($name -eq 'winfsp') { Test-Path 'C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll' } else { Test-Path 'C:\Program Files\SSHFS-Win\bin\sshfs-win.exe' }
    if (-not $present) {
        $log = Join-Path $cache ($name + '-install.log')
        $process = Start-Process -FilePath "$env:SystemRoot\System32\msiexec.exe" -ArgumentList ('/i "' + $files[$name] + '" /qn /norestart /L*v "' + $log + '"') -WindowStyle Hidden -Wait -PassThru
        if ($process.ExitCode -notin @(0,3010)) { throw ("File component install failed: $name ($($process.ExitCode))") }
        if ($process.ExitCode -eq 3010) { Write-Output ('Restart requested by ' + $name + '; no restart performed.') }
    }
}
Add-Type -AssemblyName System.IO.Compression.FileSystem
$node = Join-Path $bridge 'node.exe'
if (-not (Test-Path -LiteralPath $node)) {
    $zip = [IO.Compression.ZipFile]::OpenRead($files.node)
    try { [IO.Compression.ZipFileExtensions]::ExtractToFile($zip.GetEntry('node-v24.22.2-win-x64/node.exe'), $node) } finally { $zip.Dispose() }
}
$archives = @(@('playwright','playwright'), @('playwrightCore','playwright\node_modules\playwright-core'))
if (-not $SkipEditor) { $archives += ,@('editorServer','vscode-server') }
foreach ($entry in $archives) {
    $destination = Join-Path $bridge $entry[1]
    if (-not (Test-Path -LiteralPath $destination)) { Expand-PinnedTar $files[$entry[0]] $destination }
}
$editorCLI = Join-Path $bridge 'vscode-cli'
if (-not $SkipEditor -and -not (Test-Path -LiteralPath $editorCLI)) { [IO.Compression.ZipFile]::ExtractToDirectory($files.editorCLI, $editorCLI) }
foreach ($name in @('modules','split-extension','windows-browser.mjs','browser-protocol.mjs','windows-browser-extensions.mjs','windows-phone-browser.mjs')) {
    Copy-Item -LiteralPath (Join-Path $Source $name) -Destination $bridge -Recurse -Force
}
$utf8 = [Text.UTF8Encoding]::new($false)
if (-not $SkipEditor) { [IO.File]::WriteAllText((Join-Path $bridge 'editor-runtime.json'), '{"commit":"261c84cb1eddf11235f9e2c921425e9663fbe556"}', $utf8) }
# Installation must not make BrowserRpc report a network that does not exist.
if (-not (Test-Path -LiteralPath (Join-Path $bridge 'guest-config.json'))) {
    [IO.File]::WriteAllText((Join-Path $bridge 'guest-config.json'), '{"network_mode":"remote-pending"}', $utf8)
}
& $node --version
if ($LASTEXITCODE) { throw 'The bundled Node runtime did not start.' }
[pscustomobject]@{phase='components_installed';bridge=$bridge;project_driver=(Test-Path 'C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll');project_client=(Test-Path 'C:\Program Files\SSHFS-Win\bin\sshfs-win.exe');workload_ready=$false} | ConvertTo-Json
