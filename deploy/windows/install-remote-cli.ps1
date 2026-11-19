[CmdletBinding()]
param(
    [string]$Profile = 'C:\ProgramData\Cage Remote\windows-guest.json',
    [Parameter(Mandatory)][string]$Source,
    [Parameter(Mandatory)][string]$Packages,
    [Parameter(Mandatory)][string]$Manifest
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
& (Join-Path $PSScriptRoot 'enable-remote-batch-setup.ps1') -Profile $Profile
$binding = Get-Content -LiteralPath $Profile -Raw -Encoding UTF8 | ConvertFrom-Json
$bridge = Split-Path -Parent $binding.relay_executable
$stage = Join-Path $bridge 'setup-packages'
$null = New-Item -ItemType Directory -Path $stage -Force
$cli = Get-Content -LiteralPath $Manifest -Raw -Encoding UTF8 | ConvertFrom-Json
foreach ($name in @('uv','python','git','gh','claude')) {
    $spec = $cli.$name
    if ($spec.filename -ne [IO.Path]::GetFileName($spec.filename) -or $spec.sha256 -notmatch '^[a-fA-F0-9]{64}$') { throw 'Invalid selected tool package.' }
    $path = Join-Path $Packages $spec.filename
    if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $spec.sha256) { throw ('Tool package does not match its selected release: ' + $name) }
    Copy-Item -LiteralPath $path -Destination $stage -Force
}
Copy-Item -LiteralPath (Join-Path $Source 'install-cli.ps1') -Destination $bridge -Force
Copy-Item -LiteralPath $Manifest -Destination (Join-Path $stage 'resolved-cli-dependencies.json') -Force
$receipt = Join-Path $stage 'cli-receipt.in'
$failure = Join-Path $stage 'cli-setup-error.txt'
foreach ($path in @($receipt,$failure)) {
    if (-not (Test-Path -LiteralPath $path)) { $file = [IO.File]::Open($path,'CreateNew','Write','None'); $file.Dispose() }
    if ((Get-Item -LiteralPath $path).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Invalid setup output.' }
    $acl = Get-Acl -LiteralPath $path
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($binding.gui_user_sid),'Read,Write','Allow'))
    Set-Acl -LiteralPath $path -AclObject $acl
    [IO.File]::WriteAllText($path,'')
}
$taskName = 'Cage Remote CLI Setup ' + $binding.vm_id
if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) { throw 'A CLI setup task already exists; preserve the active installation.' }
function Quote-PS([string]$value) { return "'" + [System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($value) + "'" }
$toolPath = if ($binding.sandbox_package) { 'AppData\Local\Programs\Tools' } else { 'Cage\CLI\tools' }
$command = '$ErrorActionPreference=''Stop''; try { & ' + (Quote-PS (Join-Path $bridge 'install-cli.ps1')) + ' -BundleDirectory ' + (Quote-PS $stage) + ' -BridgeDirectory ' + (Quote-PS $bridge) + ' -ToolDirectory (Join-Path $env:USERPROFILE ' + (Quote-PS $toolPath) + ') -WorkbenchSetup; exit 0 } catch { [IO.File]::WriteAllText(' + (Quote-PS $failure) + ',$_.Exception.Message); exit 1 }'
$encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command))
$action = New-ScheduledTaskAction -Execute "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" -Argument ('-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand ' + $encoded)
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 10) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
$credential = Import-Clixml -LiteralPath (Join-Path (Split-Path -Parent $Profile) 'gui-credential.clixml')
Register-ScheduledTask -TaskName $taskName -Action $action -Settings $settings -User $credential.UserName -Password $credential.GetNetworkCredential().Password -RunLevel Limited | Out-Null
try {
    Start-ScheduledTask -TaskName $taskName
    $deadline = [DateTime]::UtcNow.AddMinutes(10)
    do {
        Start-Sleep -Seconds 1
        $task = Get-ScheduledTask -TaskName $taskName
        $info = Get-ScheduledTaskInfo -TaskName $taskName
    } while (($info.LastTaskResult -eq 267011 -or $task.State -eq 'Running') -and [DateTime]::UtcNow -lt $deadline)
    if ($task.State -eq 'Running' -or $info.LastTaskResult -ne 0) {
        throw ('Ordinary-account CLI setup failed (' + $info.LastTaskResult + '): ' + [IO.File]::ReadAllText($failure))
    }
    $handle = [IO.File]::Open($receipt,'Open','Read','Read')
    try {
        if ($handle.Length -lt 2 -or $handle.Length -gt 16384) { throw 'Invalid CLI setup receipt.' }
        $reader = [IO.StreamReader]::new($handle)
        $value = $reader.ReadToEnd() | ConvertFrom-Json
    } finally { $handle.Dispose() }
    $userProfile = (Get-CimInstance Win32_UserProfile -Filter ("SID='" + $binding.gui_user_sid + "'")).LocalPath
    if (-not $userProfile) { throw 'The ordinary account profile is not available.' }
    $allowed = [IO.Path]::GetFullPath((Join-Path $userProfile $toolPath)) + '\'
    $result = [ordered]@{version=1;claude_version=$value.claude_version}
    foreach ($name in @('python','claude','git_bash','gh')) {
        $path = [IO.Path]::GetFullPath($value.$name)
        $hash = $name + '_sha256'
        if (-not $path.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase) -or $value.$hash -notmatch '^[a-fA-F0-9]{64}$') { throw 'The CLI tool must remain in the dedicated account directory.' }
        $item = Get-Item -LiteralPath $path
        for ($ancestor=$item; $ancestor; $ancestor=$(if($ancestor -is [IO.FileInfo]){$ancestor.Directory}else{$ancestor.Parent})) {
            if ($ancestor.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'A CLI tool is redirected outside its dedicated directory.' }
        }
        if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $value.$hash) { throw 'A CLI tool changed after installation.' }
        $result[$name]=$path; $result[$hash]=$value.$hash.ToLowerInvariant()
    }
    # Do not turn a user-written receipt into an endorsement of an arbitrary
    # native Claude executable. The selected release is still the authority.
    if ($value.claude_version -cne $cli.claude.version -or $result.claude_sha256 -ne $cli.claude.sha256) { throw 'Claude Code does not match the selected official release.' }
    $signature = Get-AuthenticodeSignature -LiteralPath $result.claude
    if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'Anthropic,? PBC') { throw 'The Claude Code signature is invalid.' }
    $published = Join-Path $bridge 'cli-tools.json'
    [IO.File]::WriteAllText($published, ($result | ConvertTo-Json), [Text.UTF8Encoding]::new($false))
    $acl = [Security.AccessControl.FileSecurity]::new()
    $acl.SetAccessRuleProtection($true,$false)
    $acl.SetOwner([Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'))
    foreach ($sid in @('S-1-5-18','S-1-5-32-544')) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($sid),'FullControl','Allow'))
    }
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($binding.gui_user_sid),'Read','Allow'))
    Set-Acl -LiteralPath $published -AclObject $acl
    [pscustomobject]@{phase='cli_tools_installed';user=$binding.gui_user;claude_version=$value.claude_version;interactive_session_started=$false;workload_ready=$false} | ConvertTo-Json -Compress
} finally {
    if ((Get-ScheduledTask -TaskName $taskName).State -eq 'Running') { Stop-ScheduledTask -TaskName $taskName }
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
}
