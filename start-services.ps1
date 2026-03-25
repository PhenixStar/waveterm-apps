# Start Wave Terminal web services
Write-Host "Starting Wave Terminal web services..." -ForegroundColor Cyan

# Dashboard on port 9800
$dash = Start-Process -FilePath "D:\Dev\waveterm-apps\bin\wt-dashboard-web.exe" -WorkingDirectory "D:\Dev\waveterm-apps" -PassThru -WindowStyle Minimized
Write-Host "  Dashboard started (PID $($dash.Id)) -> http://localhost:9800" -ForegroundColor Green

# Docker panel on port 9801
$docker = Start-Process -FilePath "D:\Dev\waveterm-apps\bin\wt-docker-panel-web.exe" -ArgumentList "-config","D:\Dev\waveterm-apps\machines.json" -WorkingDirectory "D:\Dev\waveterm-apps" -PassThru -WindowStyle Minimized
Write-Host "  Docker panel started (PID $($docker.Id)) -> http://localhost:9801" -ForegroundColor Green

Write-Host "`nServices running. Open Wave Terminal to test widgets." -ForegroundColor Cyan
Write-Host "To stop: Stop-Process -Id $($dash.Id),$($docker.Id)" -ForegroundColor DarkGray
