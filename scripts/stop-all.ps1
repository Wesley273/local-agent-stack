# 停止 start-all.ps1 拉起的全部服务
$ErrorActionPreference = 'Continue'
. (Join-Path $PSScriptRoot '..\config\env.ps1')

Get-ChildItem $PID_DIR -Filter '*.pid' -ErrorAction SilentlyContinue | ForEach-Object {
    $name = $_.BaseName
    $procId = (Get-Content $_.FullName -Raw).Trim()
    $p = Get-Process -Id $procId -ErrorAction SilentlyContinue
    if ($p) { Stop-Process -Id $procId -Force; Write-Ok "stopped $name (pid=$procId)" }
    else { Write-Warn "$name pid=$procId not running" }
    Remove-Item $_.FullName -Force
}
Write-Ok "stop-all done"
