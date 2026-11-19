#requires -Version 7.0
[CmdletBinding()]
param(
    [string]$DependencyCache,
    [string]$OutputDirectory,
    [string]$MsysDirectory = $env:CAGE_MSYS2_ROOT
)
. (Join-Path $PSScriptRoot 'windows-common.ps1')
if (-not $IsWindows) { throw 'Build the Windows QEMU runtime in an MSYS2 UCRT64 environment on Windows.' }
$repository = Split-Path $PSScriptRoot -Parent
if (-not $DependencyCache) { $DependencyCache = Join-Path $repository '_output/windows-deps' }
if (-not $OutputDirectory) { $OutputDirectory = Join-Path $DependencyCache 'qemu-cage' }
$DependencyCache = [IO.Path]::GetFullPath($DependencyCache)
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
$dependencies = Get-Content (Join-Path $repository 'packaging/windows/dependencies.json') -Raw | ConvertFrom-Json
$patchDirectory = Join-Path $repository 'patches/qemu'
$inputs = @((Join-Path $PSScriptRoot 'build-qemu-windows.sh'), $PSCommandPath) + @(Get-ChildItem $patchDirectory -Filter '*.patch' | Sort-Object Name | ForEach-Object FullName)
$fingerprintText = $dependencies.qemuSource.hash + $dependencies.qemuKeycodemap.hash + (($inputs | ForEach-Object { (Get-FileHash $_ -Algorithm SHA256).Hash }) -join '')
$fingerprint = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($fingerprintText)))
$manifestPath = Join-Path $OutputDirectory 'cage-build.json'
if (Test-Path $manifestPath) {
    $manifest = Get-Content $manifestPath -Raw | ConvertFrom-Json
    if ($manifest.fingerprint -eq $fingerprint) {
        foreach ($file in $manifest.files) {
            if ((Get-FileHash -LiteralPath (Join-Path $OutputDirectory $file.name) -Algorithm SHA256).Hash -ne $file.sha256) {
                throw 'The cached QEMU runtime differs from its build manifest. Use a new output directory.'
            }
        }
        Assert-WindowsQemuRuntime $OutputDirectory
        Write-Host "Verified Windows QEMU runtime at $OutputDirectory"
        return
    }
}
if (-not $MsysDirectory) {
    $MsysDirectory = @('C:\msys64', 'D:\msys64') | Where-Object { Test-Path (Join-Path $_ 'ucrt64/bin/gcc.exe') } | Select-Object -First 1
}
if (-not $MsysDirectory) {
    throw 'Set CAGE_MSYS2_ROOT to an MSYS2 installation with the UCRT64 packages listed in docs/windows.md#windows-源码构建.'
}
$MsysDirectory = [IO.Path]::GetFullPath($MsysDirectory)
$shortPath = '^[A-Za-z]:[\\/][A-Za-z0-9_.\\/-]*$'
if ($MsysDirectory -notmatch $shortPath) {
    throw 'CAGE_MSYS2_ROOT must be a short local path with ASCII characters and no spaces (for example C:\msys64).'
}
$required = @('usr/bin/bash.exe', 'usr/bin/tar.exe', 'usr/bin/patch.exe', 'ucrt64/bin/gcc.exe',
    'ucrt64/bin/ninja.exe', 'ucrt64/bin/python.exe', 'ucrt64/bin/pkg-config.exe', 'ucrt64/bin/strip.exe', 'ucrt64/bin/objdump.exe')
$missing = @($required | Where-Object { -not (Test-Path -LiteralPath (Join-Path $MsysDirectory $_) -PathType Leaf) })
if ($missing.Count) { throw "Missing MSYS2 tools: $($missing -join ', '). Install the UCRT64 packages listed in docs/windows.md#windows-源码构建." }

# A system-wide MSYS2 installation may have an unwritable /tmp. All QEMU
# paths and native compiler scratch files must stay under a usable short path.
$roots = if ($env:CAGE_QEMU_BUILD_ROOT) { @($env:CAGE_QEMU_BUILD_ROOT) } else {
    @((Join-Path $MsysDirectory 'tmp'), $env:RUNNER_TEMP, [IO.Path]::GetTempPath())
}
$stage = $null
foreach ($root in $roots) {
    if (-not $root) { continue }
    $root = [IO.Path]::GetFullPath($root)
    if ($root -notmatch $shortPath) { continue }
    $candidate = Join-Path $root ('cage-qemu-' + [guid]::NewGuid().ToString('N').Substring(0, 12))
    try { $stage = (New-Item -Path $candidate -ItemType Directory -Force -ErrorAction Stop).FullName; break }
    catch [UnauthorizedAccessException], [IO.IOException] { continue }
}
if (-not $stage) {
    throw 'QEMU needs a writable, short local build path with ASCII characters and no spaces. Create one and set CAGE_QEMU_BUILD_ROOT to it.'
}
$built = Join-Path $DependencyCache ('qemu-runtime-' + [guid]::NewGuid().ToString('N'))
$savedSystem, $savedHere, $savedPath = $env:MSYSTEM, $env:CHERE_INVOKING, $env:PATH
$savedTemp, $savedTmp, $savedTmpDir = $env:TEMP, $env:TMP, $env:TMPDIR
$complete = $false
try {
    $uclib = Join-Path $MsysDirectory 'ucrt64/bin'
    $msysbin = Join-Path $MsysDirectory 'usr/bin'
    $bash = Join-Path $msysbin 'bash.exe'
    $env:PATH = "$uclib;$msysbin;$savedPath"
    try { Invoke-BuildTool (Join-Path $uclib 'pkg-config.exe') @('--print-errors', '--exists', 'glib-2.0', 'pixman-1', 'slirp') }
    catch { throw 'Missing QEMU libraries in MSYS2 UCRT64. Install glib2, pixman and libslirp from the packages listed in docs/windows.md#windows-源码构建.' }
    $archive = Get-VerifiedGitArchive $dependencies.qemuSource $DependencyCache
    $keymapArchive = Get-VerifiedDependency $dependencies.qemuKeycodemap $DependencyCache
    $env:MSYSTEM = 'UCRT64'
    $env:CHERE_INVOKING = '1'
    $script = Join-Path $PSScriptRoot 'build-qemu-windows.sh'
    Invoke-BuildTool $bash @('-l', $script, 'configure', $stage, $archive, $keymapArchive, $patchDirectory)
    # Start native Ninja directly. Its child MSYS tools can initialize their
    # own runtime rather than inheriting Bash's MSYS process namespace.
    $env:TEMP = $env:TMP = $env:TMPDIR = Join-Path $stage 'tmp'
    Invoke-BuildTool (Join-Path $uclib 'ninja.exe') @('-C', (Join-Path $stage 'build'), '-j', '6', 'qemu-system-x86_64.exe', 'qemu-img.exe')
    Invoke-BuildTool $bash @('-l', $script, 'package', $stage, $built, ([Environment]::SystemDirectory))
    Assert-WindowsQemuRuntime $built
    $files = @(Get-ChildItem $built -File | ForEach-Object {
        @{ name = $_.Name; sha256 = (Get-FileHash $_.FullName -Algorithm SHA256).Hash }
    })
    @{ fingerprint = $fingerprint; source = $dependencies.qemuSource.commit; files = $files } |
        ConvertTo-Json -Depth 4 | Set-Content (Join-Path $built 'cage-build.json')
    New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
    Copy-Item (Join-Path $built '*') $OutputDirectory -Force
    $complete = $true
} finally {
    $env:MSYSTEM = $savedSystem
    $env:CHERE_INVOKING = $savedHere
    $env:PATH = $savedPath
    $env:TEMP = $savedTemp; $env:TMP = $savedTmp; $env:TMPDIR = $savedTmpDir
    if (Test-Path -LiteralPath $built) { Remove-Item -LiteralPath $built -Recurse -Force -ErrorAction SilentlyContinue }
    if ($complete) { Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue }
    else { Write-Warning "QEMU build files retained at $stage" }
}
Write-Host "Built Windows QEMU runtime at $OutputDirectory"
