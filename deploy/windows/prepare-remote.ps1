[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Payload,
    [string]$Root = 'C:\ProgramData\Cage Remote',
    [string]$Install = 'C:\Program Files\Workspace Runtime',
    [string]$User = 'WorkspaceUser'
)
$ErrorActionPreference = 'Stop'
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]::new($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run this deployment as an administrator.' }
$controllerSID = $identity.User.Value
$utf8 = [Text.UTF8Encoding]::new($false)
function Set-PrivateAcl([string]$Path, [string[]]$Readers = @()) {
    $item = Get-Item -LiteralPath $Path
    $acl = if ($item.PSIsContainer) { [Security.AccessControl.DirectorySecurity]::new() } else { [Security.AccessControl.FileSecurity]::new() }
    $acl.SetAccessRuleProtection($true, $false)
    $acl.SetOwner([Security.Principal.SecurityIdentifier]::new($controllerSID))
    $inherit = if ($item.PSIsContainer) { [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit' } else { [Security.AccessControl.InheritanceFlags]::None }
    foreach ($sid in @($controllerSID, 'S-1-5-18')) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($sid), 'FullControl', $inherit, 'None', 'Allow'))
    }
    foreach ($sid in $Readers) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($sid), 'ReadAndExecute', $inherit, 'None', 'Allow'))
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}
foreach ($path in @($Root, $Install)) {
    if (Test-Path -LiteralPath $path) { throw "Destination already exists; preserve it and use the existing deployment: $path" }
}
if (Get-LocalUser -Name $User -ErrorAction SilentlyContinue) { throw 'The GUI account already exists; it must not be replaced.' }
foreach ($name in @('cage.exe', 'bridge\Cage.GuestClipboard.exe', 'bridge\cage-guest-files.exe')) {
    if (-not (Test-Path -LiteralPath (Join-Path $Payload $name) -PathType Leaf)) { throw "Payload missing: $name" }
}
$machine = [guid](Get-ItemPropertyValue -LiteralPath 'HKLM:\SOFTWARE\Microsoft\Cryptography' -Name MachineGuid)
if ($machine -eq [guid]::Empty) { throw 'Windows machine identity is missing.' }
New-Item -ItemType Directory -Path $Root | Out-Null
Set-PrivateAcl $Root
New-Item -ItemType Directory -Path $Install | Out-Null
Set-PrivateAcl $Install @('S-1-5-32-545')
Copy-Item -LiteralPath (Join-Path $Payload 'cage.exe') -Destination $Install
Copy-Item -LiteralPath (Join-Path $Payload 'bridge') -Destination $Install -Recurse
$random = [byte[]]::new(32)
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
try { $rng.GetBytes($random) } finally { $rng.Dispose() }
$password = ConvertTo-SecureString ('Cage!' + [Convert]::ToBase64String($random) + 'a9') -AsPlainText -Force
[Array]::Clear($random, 0, $random.Length)
$account = New-LocalUser -Name $User -Password $password -Description 'Dedicated workbench user'
foreach ($group in @('S-1-5-32-545', 'S-1-5-32-555')) {
    if (-not (Get-LocalGroupMember -SID $group | Where-Object { $_.SID.Value -eq $account.SID.Value })) {
        Add-LocalGroupMember -SID $group -Member $account
    }
}
$sid = $account.SID.Value
$credentialPath = Join-Path $Root 'gui-credential.clixml'
[Management.Automation.PSCredential]::new("$env:COMPUTERNAME\$User", $password) | Export-Clixml -LiteralPath $credentialPath
Set-PrivateAcl $credentialPath
$instance = [guid]::NewGuid().ToString('D')
$guestData = Join-Path $env:ProgramData "Cage Remote Guest\$instance"
New-Item -ItemType Directory -Path $guestData | Out-Null
Set-PrivateAcl $guestData
$secret = [byte[]]::new(32)
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
try { $rng.GetBytes($secret) } finally { $rng.Dispose() }
$guestToken = Join-Path $guestData 'pairing.key'
[IO.File]::WriteAllBytes($guestToken, $secret)
[Array]::Clear($secret, 0, $secret.Length)
Set-PrivateAcl $guestToken
$relay = Join-Path $Install 'bridge\Cage.GuestClipboard.exe'
$pairing = Join-Path $Root 'pairing.dpapi'
& $relay pair --vm-id $instance --sid $sid --token-file $guestToken --output $pairing
if ($LASTEXITCODE) { throw 'Could not protect the controller pairing.' }
foreach ($path in @($guestData, $guestToken)) {
    $isDirectory = (Get-Item -LiteralPath $path).PSIsContainer
    $acl = if ($isDirectory) { [Security.AccessControl.DirectorySecurity]::new() } else { [Security.AccessControl.FileSecurity]::new() }
    $acl.SetAccessRuleProtection($true, $false)
    $acl.SetOwner([Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'))
    $inherit = if ($isDirectory) { [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit' } else { [Security.AccessControl.InheritanceFlags]::None }
    foreach ($owner in @('S-1-5-18', 'S-1-5-32-544')) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($owner), 'FullControl', $inherit, 'None', 'Allow'))
    }
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($sid), 'ReadAndExecute', $inherit, 'None', 'Allow'))
    Set-Acl -LiteralPath $path -AclObject $acl
}
$instanceDirectory = Join-Path $Root 'instance'
New-Item -ItemType Directory -Path $instanceDirectory | Out-Null
Set-PrivateAcl $instanceDirectory
$profile = [ordered]@{version=1; mode='remote'; sandbox_package=$instance; machine_id=$machine.ToString('D'); vm_id=$instance; gui_user_sid=$sid; gui_user=$User; instance_directory=$instanceDirectory; relay_executable=$relay; pairing_file=$pairing}
$profilePath = Join-Path $Root 'windows-guest.json'
[IO.File]::WriteAllText($profilePath, ($profile | ConvertTo-Json), $utf8)
Set-PrivateAcl $profilePath
# Establish the account-only offline baseline before its GUI task can run.
# This is not the complete controlled-egress provider and does not mark the
# workbench ready. Administrator SSH/RDP and adapter routing are unchanged.
& $relay remote-network-policy install --profile $profilePath
if ($LASTEXITCODE) { throw 'Could not install the dedicated account network baseline.' }
$guiArgs = 'serve-local --vm-id {0} --sid {1} --token-file "{2}" --machine-id {3}' -f $instance,$sid,$guestToken,$machine.ToString('D')
$guiAction = New-ScheduledTaskAction -Execute $relay -Argument $guiArgs
$guiPrincipal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
$guiTrigger = New-ScheduledTaskTrigger -AtLogOn -User $sid
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName "Cage Remote GUI $instance" -Action $guiAction -Principal $guiPrincipal -Trigger $guiTrigger -Settings $settings | Out-Null
# The private controller runs as the deploying account, matching existing
# controller-file ownership and user-scoped DPAPI. It never runs AI workloads.
$config = Join-Path $Root 'config.json'
$script = '$env:CLAUDE_CAGE_CONFIG=''' + [System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($config) + '''; & ''' + [System.Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent((Join-Path $Install 'cage.exe')) + ''' serve'
$encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($script))
$action = New-ScheduledTaskAction -Execute "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" -Argument "-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand $encoded"
$principal = New-ScheduledTaskPrincipal -UserId $controllerSID -LogonType Interactive -RunLevel Highest
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $controllerSID
$controllerTask = "Cage Remote Controller $instance"
Register-ScheduledTask -TaskName $controllerTask -Action $action -Principal $principal -Trigger $trigger -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $controllerTask
[pscustomobject]@{profile=$profilePath; config=$config; gui_user=$User; gui_sid=$sid; instance=$instance; credential_file=$credentialPath; controller_task=$controllerTask; phase='entry_only_workload_not_ready'} | ConvertTo-Json
