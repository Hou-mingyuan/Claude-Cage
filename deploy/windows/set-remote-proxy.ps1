[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Sources,
    [string]$Root = 'C:\ProgramData\Cage Remote',
    [string]$Install
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$binding = Get-Content -LiteralPath (Join-Path $Root 'windows-guest.json') -Raw -Encoding UTF8 | ConvertFrom-Json
$installRoot=Split-Path -Parent (Split-Path -Parent $binding.relay_executable)
if (-not $Install) { $Install=$installRoot }
if ($binding.mode -ne 'remote') { throw 'This operation requires the Windows remote profile.' }
$sourcePath = Join-Path $Root 'proxy\sources.json'
$configPath = Join-Path $Root 'config.json'
$previous = Get-Content -LiteralPath $sourcePath -Raw -Encoding UTF8 | ConvertFrom-Json
$candidate = Get-Content -LiteralPath $Sources -Raw -Encoding UTF8 | ConvertFrom-Json
if ($candidate.version -ne 1 -or -not $candidate.selected -or $candidate.revision -notmatch '^[a-f0-9]{48}$') { throw 'Invalid selected proxy.' }
# Keep this server's private local endpoint, not the staging machine's auth.
$candidate.auth = $previous.auth
$candidate.port = $previous.port
$config = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8 | ConvertFrom-Json
$config | Add-Member -MemberType NoteProperty -Name proxy_revision -Value $candidate.revision -Force
$suffix = Get-Date -Format 'yyyyMMdd-HHmmss'
Copy-Item -LiteralPath $sourcePath -Destination (Join-Path $Root "proxy\sources.before-switch-$suffix.json")
Copy-Item -LiteralPath $configPath -Destination (Join-Path $Root "config.before-switch-$suffix.json")
$env:CLAUDE_CAGE_CONFIG = $configPath
& (Join-Path $Install 'cage.exe') proxy-stop
if ($LASTEXITCODE) { throw 'Could not stop the existing managed proxy.' }
$taskName = 'Cage Remote Proxy ' + $binding.vm_id
if ((Get-ScheduledTask -TaskName $taskName).State -eq 'Running') { Stop-ScheduledTask -TaskName $taskName }
$utf8 = [Text.UTF8Encoding]::new($false)
[IO.File]::WriteAllText($sourcePath, ($candidate | ConvertTo-Json -Depth 40), $utf8)
[IO.File]::WriteAllText($configPath, ($config | ConvertTo-Json -Depth 30), $utf8)
Start-ScheduledTask -TaskName $taskName
$node = @($candidate.sources.nodes | Where-Object id -eq $candidate.selected)[0]
[pscustomobject]@{Selected=$node.name;Type=$node.type;Task=$taskName;BackupSuffix=$suffix;WorkloadsEnabled=$false} | ConvertTo-Json -Compress
