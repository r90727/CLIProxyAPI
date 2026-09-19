param([string]$DataDir = (Join-Path $env:USERPROFILE '.t3-usage'))
$ErrorActionPreference = 'Stop'
$pidFile = Join-Path $DataDir 'runtime.json'
if (Test-Path -LiteralPath $pidFile) {
    $state = Get-Content -LiteralPath $pidFile -Raw | ConvertFrom-Json
    $running = Get-Process -Id $state.pid -ErrorAction SilentlyContinue
    $binary = Join-Path $DataDir 'bin\t3-usage.exe'
    if ($running -and $running.Path -eq $binary -and $running.StartTime.ToUniversalTime().ToString('o') -eq $state.started) {
        Stop-Process -Id $state.pid
        $running.WaitForExit()
    }
    Remove-Item -LiteralPath $pidFile
}
