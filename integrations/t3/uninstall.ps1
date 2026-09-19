param([string]$DataDir = (Join-Path $env:USERPROFILE '.t3-usage'))
$ErrorActionPreference = 'Stop'
& (Join-Path $DataDir 'stop.ps1') -DataDir $DataDir
$shortcut = Join-Path ([Environment]::GetFolderPath('Startup')) 'T3 Token Usage.lnk'
if (Test-Path -LiteralPath $shortcut) { Remove-Item -LiteralPath $shortcut }
Write-Output "Autostart removed. Your ledger and binaries remain in $DataDir."
