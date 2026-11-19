# Only loaded by the isolated remote PowerShell entry. Git for Windows may
# terminate with STATUS_INVALID_HANDLE when attached directly to an AppContainer
# console. Stream its stdout through PowerShell's pipe and retain its exit code.
# Child processes still inherit the AppContainer token and the same Job object.
function global:git {
    & git.exe @args | ForEach-Object { $_ }
    $global:LASTEXITCODE = $LASTEXITCODE
}
Set-Location -LiteralPath $env:USERPROFILE

# User-installed tools and caches stay in the package home; the bundled runtime
# remains read-only. Invoking npm through node avoids Windows shim/console issues.
$env:PYTHONUSERBASE = Join-Path $env:USERPROFILE 'Tools\Python'
$env:PYTHONPATH = Join-Path $PSScriptRoot 'remote-python'
$env:npm_config_prefix = Join-Path $env:USERPROFILE 'Tools\npm'
$env:npm_config_cache = Join-Path $env:USERPROFILE 'Cache\npm'
$toolNode = Join-Path $env:USERPROFILE 'Tools\node-v24.22.2-win-x64'
$toolPython = Split-Path -Parent (Get-Command python -CommandType Application).Source
$toolVenv = Join-Path $env:USERPROFILE 'Tools\PythonEnv'
if (-not (Test-Path -LiteralPath (Join-Path $toolVenv 'Scripts\python.exe'))) {
    # Reuse the bundled pip read-only, but install dependencies into a separate
    # writable environment instead of changing the managed Python installation.
    & (Join-Path $toolPython 'python.exe') -m venv --without-pip --system-site-packages $toolVenv | ForEach-Object { $_ }
    if ($LASTEXITCODE -ne 0) { Write-Warning 'Python 工作环境初始化失败，仍可使用基础 Python 和浏览器。' }
}
if (Test-Path -LiteralPath (Join-Path $toolVenv 'Scripts\python.exe')) { $env:VIRTUAL_ENV = $toolVenv }
$env:PATH = ((Join-Path $toolVenv 'Scripts'), $toolNode, $env:npm_config_prefix, (Join-Path $env:PYTHONUSERBASE 'Python313\Scripts'), (Join-Path $toolPython 'Scripts'), $env:PATH) -join ';'
function global:pip {
    & python -m pip @args | ForEach-Object { $_ }
    $global:LASTEXITCODE = $LASTEXITCODE
}
function global:npm {
    & node (Join-Path (Split-Path -Parent (Get-Command node -CommandType Application).Source) 'node_modules\npm\bin\npm-cli.js') @args | ForEach-Object { $_ }
    $global:LASTEXITCODE = $LASTEXITCODE
}
function global:npx {
    & node (Join-Path (Split-Path -Parent (Get-Command node -CommandType Application).Source) 'node_modules\npm\bin\npx-cli.js') @args | ForEach-Object { $_ }
    $global:LASTEXITCODE = $LASTEXITCODE
}
