# One-click: start web services + Wave Terminal
Write-Host "=== Wave Terminal MBP Launcher ===" -ForegroundColor Cyan
Write-Host ""

# Start web services
& "$PSScriptRoot\start-services.ps1"
Write-Host ""

# Wait for servers to be ready
Write-Host "Waiting for servers..." -ForegroundColor Yellow
Start-Sleep -Seconds 2

# Check health
try { $null = Invoke-WebRequest -Uri "http://localhost:9800" -TimeoutSec 3 -UseBasicParsing; Write-Host "  Dashboard: OK" -ForegroundColor Green } catch { Write-Host "  Dashboard: not ready yet (will auto-retry in Wave)" -ForegroundColor Yellow }
try { $null = Invoke-WebRequest -Uri "http://localhost:9801" -TimeoutSec 3 -UseBasicParsing; Write-Host "  Docker panel: OK" -ForegroundColor Green } catch { Write-Host "  Docker panel: not ready yet" -ForegroundColor Yellow }

Write-Host "`nReady. Open Wave Terminal now." -ForegroundColor Cyan
