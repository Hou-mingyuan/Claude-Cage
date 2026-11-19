[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Archive,
    [string]$Root = 'C:\ProgramData\Cage Remote',
    [string]$Bundle
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
$expected='02fe458be20493fbdf43f61ea20610b811ee6c738ab1676c61b9cfcd1a33c860'
if ((Get-FileHash -LiteralPath $Archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expected) { throw 'PowerShell archive differs from the official v7.6.6 asset.' }
$binding=Get-Content -LiteralPath (Join-Path $Root 'windows-guest.json') -Raw -Encoding UTF8 | ConvertFrom-Json
$installRoot=Split-Path -Parent (Split-Path -Parent $binding.relay_executable)
if (-not $Bundle) { $Bundle=Join-Path $installRoot 'boundary-r11' }
$destination=Join-Path $Bundle 'powershell'
if (Test-Path -LiteralPath $destination) { throw 'An existing PowerShell bundle must be preserved.' }
Expand-Archive -LiteralPath $Archive -DestinationPath $destination
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class CagePackageIdentity {
    [DllImport("userenv.dll", CharSet=CharSet.Unicode)] public static extern int DeriveAppContainerSidFromAppContainerName(string name, out IntPtr sid);
    [DllImport("advapi32.dll")] public static extern IntPtr FreeSid(IntPtr sid);
}
'@
$packageName = if ($binding.sandbox_package) {
    if ($binding.sandbox_package -cne ([guid]$binding.vm_id).ToString('D')) { throw 'Invalid package binding.' }
    $binding.sandbox_package
} else { 'Cage.Remote.' + ([guid]$binding.vm_id).ToString('N') }
$pointer=[IntPtr]::Zero
[Runtime.InteropServices.Marshal]::ThrowExceptionForHR([CagePackageIdentity]::DeriveAppContainerSidFromAppContainerName($packageName,[ref]$pointer))
try { $package=[Security.Principal.SecurityIdentifier]::new($pointer) }
finally { [void][CagePackageIdentity]::FreeSid($pointer) }
$acl=Get-Acl -LiteralPath $destination
$acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($package,'ReadAndExecute','ContainerInherit,ObjectInherit','None','Allow'))
Set-Acl -LiteralPath $destination -AclObject $acl
& (Join-Path $destination 'pwsh.exe') -NoLogo -NoProfile -NonInteractive -Command '$PSVersionTable.PSVersion.ToString()'
if ($LASTEXITCODE) { throw 'PowerShell startup failed.' }
