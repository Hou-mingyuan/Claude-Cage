#requires -Version 7.0
# Make sure the Microsoft WebView2 runtime is installed on a Windows CI runner.
#
# The runtime is machine-wide and updated in the background by EdgeUpdate.
# Some runner images already include it, and its updater may be installing at
# the same moment, in which case the offline installer exits with an error
# such as 0x8004090D even though the runtime ends up present. Like the Cage
# installer, accept a runtime that is already registered; otherwise install
# the pinned, hash-checked and signature-checked offline installer, and retry
# a failed attempt after checking whether the runtime appeared anyway.
[CmdletBinding()]
param(
    [string]$Cache = (Join-Path $env:RUNNER_TEMP 'update-dependencies'),
    [int]$Attempts = 3
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'windows-common.ps1')

$appId = '{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}'
function Get-WebView2Version {
    # EdgeUpdate is a 32-bit program, so machine-wide registrations live in
    # the WOW6432Node view; per-user installs use HKCU.
    foreach ($key in @(
            "HKLM:\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\$appId",
            "HKLM:\SOFTWARE\Microsoft\EdgeUpdate\Clients\$appId",
            "HKCU:\Software\Microsoft\EdgeUpdate\Clients\$appId")) {
        $version = (Get-ItemProperty -LiteralPath $key -Name pv -ErrorAction SilentlyContinue).pv
        if ($version -and $version -ne '0.0.0.0') { return $version }
    }
    return $null
}

$installed = Get-WebView2Version
if ($installed) {
    Write-Host "WebView2 runtime $installed is already installed."
    return
}

$dependency = (Get-Content (Join-Path $PSScriptRoot '../packaging/windows/dependencies.json') -Raw | ConvertFrom-Json).webview2
$runtime = Get-VerifiedDependency $dependency $Cache
$signature = Get-AuthenticodeSignature -LiteralPath $runtime
if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation') {
    throw 'WebView2 prerequisite signature is not trusted.'
}

$failures = @()
for ($attempt = 1; $attempt -le $Attempts; $attempt++) {
    $start = [Diagnostics.ProcessStartInfo]::new($runtime)
    $start.UseShellExecute = $false
    $start.WorkingDirectory = $Cache
    $start.ArgumentList.Add('/silent'); $start.ArgumentList.Add('/install')
    $process = [Diagnostics.Process]::Start($start)
    try {
        if (!$process.WaitForExit(180000)) { $process.Kill($true); throw 'WebView2 prerequisite installation timed out.' }
        $code = $process.ExitCode
    } finally { $process.Dispose() }

    $installed = Get-WebView2Version
    if ($code -eq 0) {
        Write-Host "Installed WebView2 runtime $installed."
        return
    }
    if ($installed) {
        Write-Host ("WebView2 installer exited with 0x{0:X8}, but runtime {1} is now registered." -f $code, $installed)
        return
    }
    $failures += '0x{0:X8}' -f $code
    Write-Warning ("WebView2 installation attempt $attempt of $Attempts exited with 0x{0:X8}." -f $code)
    if ($attempt -lt $Attempts) { Start-Sleep -Seconds 20 }
}
throw "WebView2 prerequisite failed after $Attempts attempts ($($failures -join ', '))."
