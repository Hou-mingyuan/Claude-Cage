#requires -Version 7.0
. (Join-Path $PSScriptRoot 'windows-common.ps1')

function New-QemuCacheFixture {
    param([string]$Directory, [string]$Fingerprint)
    [void][IO.Directory]::CreateDirectory($Directory)
    $files = foreach ($name in @('qemu-system-x86_64.exe', 'qemu-img.exe', 'build-packages.txt', 'build-source-packages.txt')) {
        $path = Join-Path $Directory $name
        [IO.File]::WriteAllText($path, "$name $Fingerprint")
        @{ name = $name; sha256 = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash }
    }
    @{ fingerprint = $Fingerprint; files = @($files) } | ConvertTo-Json -Depth 4 |
        Set-Content -LiteralPath (Join-Path $Directory 'cage-build.json')
}

$root = Join-Path (Get-WindowsWorkbenchTestBase) ('cage-qemu-cache-test-' + [guid]::NewGuid().ToString('N'))
[void][IO.Directory]::CreateDirectory($root)
try {
    $first = 'a' * 64
    $second = 'b' * 64
    $cached = Join-Path $root 'qemu-cache'
    $candidate = Join-Path $root 'candidate'
    New-QemuCacheFixture $cached $first
    New-QemuCacheFixture $candidate $second
    $lock = Enter-WindowsQemuOutputLock $cached
    try {
        $busy = $false
        try { [IO.File]::Open("$cached.lock", 'Open', 'ReadWrite', 'None').Dispose() }
        catch [IO.IOException] { $busy = $true }
        if (-not $busy) { throw 'The cache lock permitted a concurrent writer.' }

        $rejected = $false
        try { Publish-WindowsQemuRuntime -StagedDirectory "$candidate-missing" -Directory $cached -ManagedCache }
        catch [IO.IOException] { $rejected = $true }
        if (-not $rejected -or -not (Test-WindowsQemuBuildManifest $cached $first)) {
            throw 'A failed publish did not restore the previous verified runtime.'
        }

        # Simulate interruption after copying the new manifest over the old files.
        Copy-Item -LiteralPath (Join-Path $candidate 'cage-build.json') -Destination $cached -Force
        if (Test-WindowsQemuBuildManifest $cached $second) { throw 'A mixed cache passed manifest validation.' }
        Publish-WindowsQemuRuntime -StagedDirectory $candidate -Directory $cached -ManagedCache
        if (-not (Test-WindowsQemuBuildManifest $cached $second) -or (Test-Path -LiteralPath $candidate)) {
            throw 'The cache did not recover by publishing the verified candidate directory.'
        }
        $unowned = Join-Path $root 'unowned'
        [void][IO.Directory]::CreateDirectory($unowned)
        $notes = Join-Path $unowned 'notes.txt'
        [IO.File]::WriteAllText($notes, 'keep')
        New-QemuCacheFixture $candidate $first
        $rejected = $false
        try { Publish-WindowsQemuRuntime -StagedDirectory $candidate -Directory $unowned }
        catch { $rejected = $_.Exception.Message -like 'Choose an empty directory*' }
        if (-not $rejected -or [IO.File]::ReadAllText($notes) -ne 'keep') { throw 'A custom QEMU output replaced an unrelated directory.' }
    } finally { $lock.Dispose() }
    (Enter-WindowsQemuOutputLock $cached).Dispose()

    $downloads = Join-Path $root 'downloads'
    $script:downloadContent = 'truncated payload'
    function Invoke-WebRequest {
        param($Uri, $OutFile, $TimeoutSec, $MaximumRedirection)
        [IO.File]::WriteAllText($OutFile, $script:downloadContent)
    }
    $verifiedContent = 'verified payload'
    $dependency = @{
        url = 'https://example.com/keymaps.tar.gz'
        file = 'keymaps.tar.gz'
        algorithm = 'SHA256'
        hash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($verifiedContent)))
    }
    $rejected = $false
    try { $null = Get-VerifiedDependency $dependency $downloads }
    catch { $rejected = $_.Exception.Message -like 'Downloaded dependency checksum mismatch*' }
    if (-not $rejected -or @(Get-ChildItem -LiteralPath $downloads -Force).Count) { throw 'A failed download left an unverified cache entry.' }
    $script:downloadContent = $verifiedContent
    $archive = Get-VerifiedDependency $dependency $downloads
    if ([IO.File]::ReadAllText($archive) -ne $verifiedContent) { throw 'The verified download was not published on retry.' }
    Write-Output 'PASS: QEMU cache locking, rollback, interrupted rebuild recovery, custom output ownership, and dependency retry.'
} finally { Remove-Item -LiteralPath $root -Recurse -Force }
