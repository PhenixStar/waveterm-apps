# Stop all Wave Terminal web services
$pidFile = "$PSScriptRoot\.service-pids"

if (Test-Path $pidFile) {
    $pids = Get-Content $pidFile
    $pids | ForEach-Object {
        $p = Get-Process -Id $_ -ErrorAction SilentlyContinue
        if ($p) {
            Stop-Process -Id $_ -Force
            Write-Host "  Stopped PID $_ ($($p.ProcessName))" -ForegroundColor Yellow
        }
    }
    Remove-Item $pidFile
    Write-Host "All services stopped." -ForegroundColor Cyan
} else {
    # Fallback: kill by name
    $killed = 0
    foreach ($name in @("wt-dashboard-web", "wt-docker-panel-web")) {
        Get-Process -Name $name -ErrorAction SilentlyContinue | ForEach-Object {
            Stop-Process -Id $_.Id -Force
            Write-Host "  Stopped $name (PID $($_.Id))" -ForegroundColor Yellow
            $killed++
        }
    }
    if ($killed -eq 0) { Write-Host "No services running." -ForegroundColor DarkGray }
    else { Write-Host "Stopped $killed processes." -ForegroundColor Cyan }
}
