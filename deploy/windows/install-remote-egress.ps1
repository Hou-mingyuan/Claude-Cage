[CmdletBinding()]
param(
    [string]$Root = 'C:\ProgramData\Cage Remote',
    [string]$Bundle
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false)
$identity=[Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]::new($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Deployment administrator required.' }
$profile=Join-Path $Root 'windows-guest.json'
$binding=Get-Content -LiteralPath $profile -Raw -Encoding UTF8 | ConvertFrom-Json
$installRoot=Split-Path -Parent (Split-Path -Parent $binding.relay_executable)
if (-not $Bundle) { $Bundle=Join-Path $installRoot 'boundary-r11' }
if ($binding.mode -ne 'remote') { throw 'Remote binding required.' }
$executable=Join-Path $Bundle 'remote-egress.exe'
$credentialPath=Join-Path $Bundle 'remote-egress.json'
$taskName='Cage Remote Egress ' + $binding.vm_id
if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) { throw 'Existing broker task must be preserved.' }
if (-not (Test-Path -LiteralPath $credentialPath) -or (Get-Item -LiteralPath $credentialPath).Length -eq 0) {
    # Protect the empty file before writing the scoped credential.
    if (-not (Test-Path -LiteralPath $credentialPath)) {
        $file=[IO.File]::Open($credentialPath,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
        $file.Dispose()
    }
        $acl=[Security.AccessControl.FileSecurity]::new()
        $acl.SetAccessRuleProtection($true,$false)
        foreach ($sid in @('S-1-5-18','S-1-5-32-544')) {
            $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($sid),'FullControl','Allow'))
        }
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($binding.gui_user_sid),'Read','Allow'))
        Set-Acl -LiteralPath $credentialPath -AclObject $acl
    $file=[IO.File]::Open($credentialPath,[IO.FileMode]::Open,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try {
        $random=New-Object byte[] 32
        $rng=[Security.Cryptography.RandomNumberGenerator]::Create()
        try { $rng.GetBytes($random) } finally { $rng.Dispose() }
        $token=([BitConverter]::ToString($random)).Replace('-','').ToLowerInvariant()
        $bytes=[Text.Encoding]::UTF8.GetBytes((@{token=$token} | ConvertTo-Json -Compress))
        $file.Write($bytes,0,$bytes.Length)
    } finally { $file.Dispose() }
}
& (Join-Path $Bundle 'Cage.GuestClipboard.exe') remote-network-policy broker-install --profile $profile
if ($LASTEXITCODE) { throw 'Broker WFP policy failed.' }
$principal=New-ScheduledTaskPrincipal -UserId $identity.Name -LogonType Interactive -RunLevel Highest
$arguments='serve "' + (Join-Path $Root 'config.json') + '" "' + $credentialPath + '"'
# Keep the broker attached to the task without exposing a closable console.
$launch='$process=Start-Process -FilePath ''' + [System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($executable) + ''' -ArgumentList ''' + [System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($arguments) + ''' -WindowStyle Hidden -Wait -PassThru; exit $process.ExitCode'
$encoded=[Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($launch))
$action=New-ScheduledTaskAction -Execute (Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe') -Argument ('-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand ' + $encoded)
$settings=New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([timespan]::Zero) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName $taskName -Principal $principal -Action $action -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $taskName
[pscustomobject]@{task=$taskName;candidate=$true;workload_ready=$false} | ConvertTo-Json -Compress
