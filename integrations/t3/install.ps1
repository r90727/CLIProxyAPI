param(
    [string]$DataDir = (Join-Path $env:USERPROFILE '.t3-usage'),
    [string]$GoBinary = 'go',
    [switch]$NoBuild,
    [switch]$NoAutoStart
)
$ErrorActionPreference = 'Stop'
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
if (-not $NoBuild) {
    Push-Location $repo
    try {
        & $GoBinary build -o bin/t3-usage.exe ./cmd/t3-usage
        if ($LASTEXITCODE -ne 0) { throw 'Usage tracker build failed' }
        & $GoBinary build -o bin/cli-proxy-api.exe ./cmd/server
        if ($LASTEXITCODE -ne 0) { throw 'Proxy build failed' }
    } finally { Pop-Location }
}
if (Test-Path -LiteralPath (Join-Path $DataDir 'stop.ps1')) { & (Join-Path $DataDir 'stop.ps1') -DataDir $DataDir }
New-Item -ItemType Directory -Force (Join-Path $DataDir 'bin') | Out-Null
foreach ($name in @('t3-usage.exe','cli-proxy-api.exe')) { Copy-Item -LiteralPath (Join-Path $repo "bin\$name") -Destination (Join-Path $DataDir 'bin') -Force }
foreach ($name in @('start.ps1','stop.ps1','uninstall.ps1')) { Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination $DataDir -Force }
if (-not $NoAutoStart) {
    $startup = [Environment]::GetFolderPath('Startup')
    $shell = New-Object -ComObject WScript.Shell
    $shortcut = $shell.CreateShortcut((Join-Path $startup 'T3 Token Usage.lnk'))
    $shortcut.TargetPath = (Join-Path ([Environment]::GetFolderPath('System')) 'WindowsPowerShell\v1.0\powershell.exe')
    $shortcut.Arguments = '-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + (Join-Path $DataDir 'start.ps1') + '" -DataDir "' + $DataDir + '"'
    $shortcut.WindowStyle = 7
    $shortcut.Save()
}
"[InternetShortcut]`r`nURL=http://127.0.0.1:8318/" | Set-Content -LiteralPath (Join-Path $DataDir 'Token Usage.url') -Encoding ASCII
& (Join-Path $DataDir 'start.ps1') -DataDir $DataDir
