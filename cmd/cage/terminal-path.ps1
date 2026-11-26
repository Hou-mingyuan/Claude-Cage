param(
    [Parameter(Mandatory = $true)][string]$InstallLocation,
    [ValidateSet('Register', 'Unregister')][string]$Action = 'Register'
)
$ErrorActionPreference = 'Stop'
$dir = [IO.Path]::GetFullPath((Join-Path $InstallLocation 'resources'))
if ($dir.Contains(';')) { throw 'The installation path cannot be represented in PATH.' }
$currentUser = [Microsoft.Win32.Registry]::CurrentUser
$environment = $currentUser.CreateSubKey('Environment')
$owner = $currentUser.CreateSubKey('Software\ClaudeCage\TerminalCommand')
try {
    $raw = [string]$environment.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    $kind = if ($environment.GetValueNames() -contains 'Path') {
        $environment.GetValueKind('Path')
    } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
    $parts = @($raw.Split(';'))
    $ours = [string]$owner.GetValue('OwnedPath', '')
    if ($Action -eq 'Unregister' -and $ours -ne $dir) { return }
    if ($Action -eq 'Register' -and $ours -eq $dir -and $parts -contains $dir) { return }
    if ($ours) { $parts = @($parts | Where-Object { $_ -ne $ours }) }
    $owner.DeleteValue('OwnedPath', $false)
    if ($Action -eq 'Register' -and $parts -notcontains $dir) {
        if (!$raw) { $parts = @() }
        $parts += $dir
        $environment.SetValue('Path', ($parts -join ';'), $kind)
        $owner.SetValue('OwnedPath', $dir)
    } elseif (($parts -join ';') -ne $raw) {
        $environment.SetValue('Path', ($parts -join ';'), $kind)
    }
} finally { $owner.Dispose(); $environment.Dispose() }
