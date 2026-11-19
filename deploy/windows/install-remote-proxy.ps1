[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Sources,
    [string]$Root = 'C:\ProgramData\Cage Remote',
    [string]$Install
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]::new($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run as the deployment administrator.' }
$binding = Get-Content -LiteralPath (Join-Path $Root 'windows-guest.json') -Raw -Encoding UTF8 | ConvertFrom-Json
$installRoot=Split-Path -Parent (Split-Path -Parent $binding.relay_executable)
if (-not $Install) { $Install=$installRoot }
if ($binding.mode -ne 'remote') { throw 'This installer requires the Windows remote profile.' }
$directory = Join-Path $Root 'proxy'
if (Test-Path -LiteralPath (Join-Path $directory 'sources.json')) { throw 'An existing proxy must be preserved; this is an initial import only.' }
$store = Get-Content -LiteralPath $Sources -Raw -Encoding UTF8 | ConvertFrom-Json
if ($store.version -ne 1 -or $store.port -ne 17896 -or $store.auth -notmatch '^[a-f0-9]{48}$' -or $store.revision -notmatch '^[a-f0-9]{48}$') { throw 'Invalid imported proxy settings.' }
$core = Join-Path $Install 'mihomo.exe'
if (-not (Test-Path -LiteralPath $core -PathType Leaf)) { throw 'The bundled Windows proxy core is missing.' }
$runner = Join-Path $PSScriptRoot 'run-remote-proxy.ps1'
if (-not (Test-Path -LiteralPath $runner -PathType Leaf)) { throw 'The proxy background runner is missing.' }
function Protect-ProxyPath([string]$Path) {
    $item = Get-Item -LiteralPath $Path
    $acl = if ($item.PSIsContainer) { [Security.AccessControl.DirectorySecurity]::new() } else { [Security.AccessControl.FileSecurity]::new() }
    $acl.SetAccessRuleProtection($true, $false)
    $acl.SetOwner($identity.User)
    $inherit = if ($item.PSIsContainer) { [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit' } else { [Security.AccessControl.InheritanceFlags]::None }
    foreach ($sid in @($identity.User, [Security.Principal.SecurityIdentifier]::new('S-1-5-18'))) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($sid, 'FullControl', $inherit, 'None', 'Allow'))
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}
New-Item -ItemType Directory -Path $directory -Force | Out-Null
Protect-ProxyPath $directory
$configPath = Join-Path $Root 'config.json'
$backupPath = Join-Path $Root ('config.before-proxy-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '.json')
Copy-Item -LiteralPath $configPath -Destination $backupPath
Protect-ProxyPath $backupPath
Copy-Item -LiteralPath $Sources -Destination (Join-Path $directory 'sources.json')
Protect-ProxyPath (Join-Path $directory 'sources.json')
$config = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8 | ConvertFrom-Json
$config.proxy_url = "socks5://cage:$($store.auth)@127.0.0.1:$($store.port)"
$config | Add-Member -MemberType NoteProperty -Name proxy_revision -Value $store.revision -Force
[IO.File]::WriteAllText($configPath, ($config | ConvertTo-Json -Depth 30), [Text.UTF8Encoding]::new($false))
Protect-ProxyPath $configPath
$taskName = 'Cage Remote Proxy ' + $binding.vm_id
$principal = New-ScheduledTaskPrincipal -UserId $identity.Name -LogonType Interactive -RunLevel Highest
$action = New-ScheduledTaskAction -Execute "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" -Argument ('-NoProfile -NonInteractive -WindowStyle Hidden -File "' + $runner + '" -Root "' + $Root + '" -Install "' + $Install + '"')
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $identity.Name
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([timespan]::Zero) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName $taskName -Principal $principal -Action $action -Trigger $trigger -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $taskName
[pscustomobject]@{Imported=$true;Port=$store.port;NodeCount=@($store.sources.nodes).Count;ConfigBackup=$backupPath;ProxyTask=$taskName;WorkloadsEnabled=$false} | ConvertTo-Json -Compress
