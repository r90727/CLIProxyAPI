param([string]$DataDir = (Join-Path $env:USERPROFILE '.t3-usage'))
$ErrorActionPreference = 'Stop'
$binary = Join-Path $DataDir 'bin\t3-usage.exe'
$pidFile = Join-Path $DataDir 'runtime.json'
if (Test-Path -LiteralPath $pidFile) {
    $state = Get-Content -LiteralPath $pidFile -Raw | ConvertFrom-Json
    $running = Get-Process -Id $state.pid -ErrorAction SilentlyContinue
    if ($running -and $running.Path -eq $binary -and $running.StartTime.ToUniversalTime().ToString('o') -eq $state.started) {
        Write-Output 'T3 usage is already running: http://127.0.0.1:8318'
        exit 0
    }
}
$process = Start-Process -FilePath $binary -ArgumentList @('--db', ('"' + (Join-Path $DataDir 'usage.sqlite') + '"')) -WorkingDirectory $DataDir -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $DataDir 'stdout.log') -RedirectStandardError (Join-Path $DataDir 'stderr.log')
@{pid=$process.Id; started=$process.StartTime.ToUniversalTime().ToString('o')} | ConvertTo-Json | Set-Content -LiteralPath $pidFile -Encoding UTF8
Write-Output 'T3 usage started: http://127.0.0.1:8318'
