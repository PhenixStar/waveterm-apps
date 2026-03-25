# Wave Terminal MBP — headless service launcher
# Servers run completely hidden (no windows). Use stop-all.ps1 to kill.

$pidFile = "$PSScriptRoot\.service-pids"

# Kill any previous instances
if (Test-Path $pidFile) {
    Get-Content $pidFile | ForEach-Object {
        Stop-Process -Id $_ -ErrorAction SilentlyContinue
    }
    Remove-Item $pidFile
}

Write-Host "Starting services (headless)..." -ForegroundColor Cyan

$dash = Start-Process -FilePath "$PSScriptRoot\bin\wt-dashboard-web.exe" `
    -WorkingDirectory $PSScriptRoot `
    -WindowStyle Hidden -PassThru

$docker = Start-Process -FilePath "$PSScriptRoot\bin\wt-docker-panel-web.exe" `
    -ArgumentList "-config","$PSScriptRoot\machines.json" `
    -WorkingDirectory $PSScriptRoot `
    -WindowStyle Hidden -PassThru

# Save PIDs for stop script
"$($dash.Id)`n$($docker.Id)" | Set-Content $pidFile

Write-Host "  Dashboard  PID $($dash.Id)  http://localhost:9800" -ForegroundColor Green
Write-Host "  Docker     PID $($docker.Id)  http://localhost:9801" -ForegroundColor Green

# Health check
Start-Sleep -Seconds 2
$ok = 0
try { $null = Invoke-WebRequest -Uri "http://localhost:9800" -TimeoutSec 3 -UseBasicParsing; $ok++; Write-Host "  Dashboard:  OK" -ForegroundColor Green } catch { Write-Host "  Dashboard:  starting..." -ForegroundColor Yellow }
try { $null = Invoke-WebRequest -Uri "http://localhost:9801" -TimeoutSec 3 -UseBasicParsing; $ok++; Write-Host "  Docker:     OK" -ForegroundColor Green } catch { Write-Host "  Docker:     starting..." -ForegroundColor Yellow }

Write-Host "`nReady ($ok/2 healthy). PIDs saved to .service-pids" -ForegroundColor Cyan
