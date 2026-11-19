[CmdletBinding()]
param(
    [string]$Root='C:\ProgramData\Cage Remote',
    [string]$Install,
    [string]$Bundle,
    [Parameter(Mandatory)][string]$ControllerCandidate
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
$profile=Join-Path $Root 'windows-guest.json'
$binding=Get-Content -LiteralPath $profile -Raw -Encoding UTF8 | ConvertFrom-Json
$installRoot=Split-Path -Parent (Split-Path -Parent $binding.relay_executable)
if (-not $Install) { $Install=$installRoot }
if (-not $Bundle) { $Bundle=Join-Path $installRoot 'boundary-r11' }
if ($binding.mode -ne 'remote') { throw 'Remote installation required.' }
$taskName='Cage Remote Controller '+$binding.vm_id
$controller=Join-Path $Install 'cage.exe'
if (!(Test-Path -LiteralPath $ControllerCandidate) -or !(Test-Path -LiteralPath (Join-Path $Bundle 'remote-shell.ps1'))) { throw 'Candidate files are missing.' }
$stamp=Get-Date -Format yyyyMMdd-HHmmss
Copy-Item -LiteralPath $profile -Destination ($profile+'.before-terminal-'+$stamp)
Copy-Item -LiteralPath $controller -Destination ($controller+'.before-terminal-'+$stamp)
Stop-ScheduledTask -TaskName $taskName
# The initial r5 controller has no enabled workload, and its wrapper task can
# leave the child alive. Retire only that exact deployment's serve process;
# proxy, egress, headless jobs and all other Cage installations stay untouched.
$processes=Get-CimInstance Win32_Process -Filter "Name='cage.exe'" | Where-Object {
    $_.ExecutablePath -eq $controller -and $_.CommandLine -eq ('"'+$controller+'" serve')
}
foreach ($process in $processes) {
    Stop-Process -Id $process.ProcessId
    Wait-Process -Id $process.ProcessId -Timeout 10 -ErrorAction SilentlyContinue
}
try {
    # NUL is a device object: its package-only ACL is reset by Windows reboot.
    # Reapply the existing policy before serving, not from an untrusted shell.
    $command='$env:CLAUDE_CAGE_CONFIG='''+[System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent((Join-Path $Root 'config.json'))+'''; & '''+[System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent((Join-Path $Bundle 'Cage.GuestClipboard.exe'))+''' remote-sandbox-policy install --profile '''+[System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($profile)+'''; if($LASTEXITCODE){exit $LASTEXITCODE}; & '''+[System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($ControllerCandidate)+''' serve'
    $encoded=[Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command))
    $action=New-ScheduledTaskAction -Execute "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" -Argument ('-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand '+$encoded)
    Set-ScheduledTask -TaskName $taskName -Action $action | Out-Null
    $binding.relay_executable=Join-Path $Bundle 'Cage.GuestClipboard.exe'
    [IO.File]::WriteAllText($profile,($binding | ConvertTo-Json -Depth 5),[Text.UTF8Encoding]::new($false))
} finally { Start-ScheduledTask -TaskName $taskName }
[pscustomobject]@{terminal='appcontainer-home';backup=$stamp;workload_ready=$false} | ConvertTo-Json -Compress
