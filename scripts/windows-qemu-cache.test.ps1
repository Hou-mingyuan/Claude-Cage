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

    $systemDirectory = 'C:\Windows\System32'
    $loader = Get-WindowsCompilerPreflightError -Output @("`e[31mgcc.exe: fatal error`e[0m", 'cage-compiler-check: exit status -1073741515 (0xC0000135)') `
        -ExitCode 1 -MsysDirectory 'D:\msys64\' -SystemDirectory $systemDirectory -SystemZlib $false
    if ($loader -notlike "*`$env:CC = 'gcc -BD:/msys64/ucrt64/bin/'; `$env:CXX = 'g++ -BD:/msys64/ucrt64/bin/'*" -or
        $loader -notlike '*zlib1.dll from C:\Windows\System32*' -or $loader -match "`e" -or $loader -notlike '*exit code 1 *') {
        throw "The compiler preflight did not explain the System32 zlib1.dll loader failure: $loader"
    }
    $assembler = Get-WindowsCompilerPreflightError -Output @('C:/msys64/ucrt64/x86_64-w64-mingw32/bin/as.exe returned 1 exit status') `
        -ExitCode 1 -MsysDirectory 'C:\msys64' -SystemDirectory $systemDirectory -SystemZlib $true
    if ($assembler -notlike "*'gcc -BC:/msys64/ucrt64/bin/'*") { throw 'An assembler failure beside a System32 zlib1.dll lacked the -B workaround.' }
    $generic = Get-WindowsCompilerPreflightError -Output @('undefined reference to `missing''') -ExitCode 1 -MsysDirectory 'C:\msys64' `
        -SystemDirectory $systemDirectory -SystemZlib $false
    if ($generic -like '*zlib1.dll*' -or $generic -notlike '*undefined reference*') { throw 'A generic compiler failure reported the zlib1.dll conflict.' }
    $shadow = Get-WindowsCompilerPreflightError -Output @('ld.exe: cannot find -lfoo') -ExitCode 1 -MsysDirectory 'C:\msys64' `
        -SystemDirectory $systemDirectory -SystemZlib $true
    if ($shadow -notlike '*C:\Windows\System32 contains zlib1.dll*' -or $shadow -like '*gcc -B*') { throw 'A System32 zlib1.dll was not mentioned as a possible cause.' }
    $long = Get-WindowsCompilerPreflightError -Output @(('x' * 5000) + 'tail') -ExitCode 1 -MsysDirectory 'C:\msys64' -SystemDirectory $systemDirectory -SystemZlib $false
    if ($long.Length -gt 3500 -or $long -notlike '*...*tail*') { throw 'Long compiler output was not truncated to its tail.' }
    if ((Get-WindowsCompilerPreflightError -Output $null -ExitCode 1 -MsysDirectory 'C:\msys64' -SystemDirectory $systemDirectory -SystemZlib $false) -notlike '*(no output)*') {
        throw 'An empty compiler output was not reported.'
    }
    $downloads = Join-Path $root 'downloads'
    $script:downloadContent = 'truncated payload'
    # Exercise the built-in download path offline: without Node on the lookup,
    # Get-VerifiedDependency uses the Invoke-WebRequest stub below instead of
    # fetch-build-artifact.mjs, whose behavior fetch-build-artifact.test.mjs covers.
    function Get-BuildCommand {
        param([string[]]$Names, [switch]$Optional)
        if ($Names -contains 'node') { return $null }
        throw "Unexpected build tool lookup in the dependency test: $($Names -join ', ')"
    }
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

    if ($IsWindows) {
        # Verify Windows fallback to Invoke-WebRequest when the shared curl fetcher fails (e.g. Schannel revocation offline).
        $fallbackDir = Join-Path $root 'downloads-fallback'
        $fallbackCalled = $false
        function Get-BuildCommand {
            param([string[]]$Names, [switch]$Optional)
            if ($Names -contains 'node') { return 'node.exe' }
            throw "Unexpected build tool lookup: $($Names -join ', ')"
        }
        function Invoke-BuildTool {
            param([string]$Tool, [string[]]$Arguments)
            $script:fallbackCalled = $true
            throw 'node.exe failed with exit code 1 (CRYPT_E_REVOCATION_OFFLINE)'
        }
        $fallbackArchive = Get-VerifiedDependency $dependency $fallbackDir
        if (-not $script:fallbackCalled -or [IO.File]::ReadAllText($fallbackArchive) -ne $verifiedContent) {
            throw 'Get-VerifiedDependency did not fall back to Invoke-WebRequest after shared downloader failure.'
        }
    }
    Write-Output 'PASS: QEMU cache locking, rollback, interrupted rebuild recovery, custom output ownership, compiler preflight diagnosis, and dependency retry.'
} finally { Remove-Item -LiteralPath $root -Recurse -Force }
