#requires -Version 7.0
. (Join-Path $PSScriptRoot 'windows-common.ps1')
$compiler = Get-BuildCommand @('makensis.exe', 'makensis')
$root = Join-Path ([IO.Path]::GetTempPath()) ('cage-nsis-options-' + [guid]::NewGuid().ToString('N'))
$null = New-Item -Path $root -ItemType Directory
$source = Join-Path $root 'fixture.nsi'
$setup = Join-Path $root 'fixture.exe'
$report = Join-Path $root 'options.txt'
$options = Join-Path $PSScriptRoot '../packaging/windows/CageInstallerOptions.nsh'
try {
    # Run the real parser in an installer with no file or registry operations.
    @'
Unicode true
RequestExecutionLevel user
SilentInstall silent
!include "LogicLib.nsh"
!include "${OPTIONS_FILE}"
OutFile "${SETUP}"
Function .onInit
  StrCpy $DesktopShortcut "1"
  StrCpy $StartMenuShortcuts "1"
  Call ParseCageInstallOptions
  FileOpen $0 "${REPORT}" w
  FileWrite $0 "$CageInstallDirectory|$DesktopShortcut|$StartMenuShortcuts|$UpdateMode|$DiagnosticFile|$CageOptionError"
  FileClose $0
  ${If} $CageOptionError != ""
    SetErrorLevel 2
    Quit
  ${EndIf}
FunctionEnd
Section
SectionEnd
'@ | Set-Content -LiteralPath $source -Encoding utf8
    Invoke-BuildTool $compiler @('/V2', '/INPUTCHARSET', 'UTF8', "/DOPTIONS_FILE=$options", "/DSETUP=$setup", "/DREPORT=$report", $source)
    function Check-InstallerOptions([string]$Arguments, [int]$Code, [string]$Expected) {
        $process = Start-Process -FilePath $setup -ArgumentList $Arguments -PassThru
        try {
            if (-not $process.WaitForExit(15000)) { $process.Kill(); throw 'Installer argument fixture timed out.' }
            $actual = Get-Content -LiteralPath $report -Raw
            if ($process.ExitCode -ne $Code -or $actual -notmatch $Expected) {
                throw "Unexpected installer options for '$Arguments': exit $($process.ExitCode), '$actual'."
            }
        } finally { $process.Dispose() }
    }
    Check-InstallerOptions '/S /UPDATE /DESKTOPSHORTCUT=0 /STARTMENUSHORTCUT=1 /DIAGNOSTICS="D:\Test Folder\report.json" /D=D:\Cage Builds\Cage' 0 '^D:\\Cage Builds\\Cage\|0\|1\|1\|D:\\Test Folder\\report\.json\|$'
    Check-InstallerOptions '/S /STARTMENUSHORTCUT=0 /DESKTOPSHORTCUT=1' 0 '^\|1\|0\|0\|\|$'
    Check-InstallerOptions '/S /UPDATE /D=D:\Cage /DESKTOPSHORTCUT=0 /STARTMENUSHORTCUT=0' 2 '/D= must be the last option'
    Check-InstallerOptions '/S /D=D:\Cage /D=D:\Other' 2 '/D= must be the last option'
    Check-InstallerOptions '/S /D="D:\Cage Builds\Cage"' 2 '/D= must be the last option'
    Check-InstallerOptions '/S "/D=D:\Cage Builds\Cage"' 2 '/D= must be the last option'
    Check-InstallerOptions '/S /D=Cage' 2 '/D= must be the last option'
    Check-InstallerOptions '/S /DESKTOPSHORTCUT=00 /D=D:\Cage' 2 'DESKTOPSHORTCUT must be 0 or 1'
    Write-Host 'Windows installer argument tests passed.'
} finally { Remove-Item -LiteralPath $root -Recurse -Force }
