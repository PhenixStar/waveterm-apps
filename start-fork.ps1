# Start the custom Wave Terminal fork (with GPU/Disk/Net/Dials sysinfo)
$env:WAVETERM_HOME = "D:\Dev\waveterm-test-home"
Write-Host "Starting Wave Terminal fork (WAVETERM_HOME=$env:WAVETERM_HOME)..." -ForegroundColor Cyan
Start-Process -FilePath "D:\Dev\waveterm\make\win-unpacked\Wave.exe"
Write-Host "Fork launched. Uses separate data dir to avoid conflicts." -ForegroundColor Green
