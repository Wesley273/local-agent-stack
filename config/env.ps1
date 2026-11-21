# local-agent-stack 统一环境加载（所有脚本 dot-source 本文件）
# 用法: . (Join-Path $PSScriptRoot '..\config\env.ps1')
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
$OutputEncoding = [System.Text.UTF8Encoding]::new($false)

$thisFile = $MyInvocation.MyCommand.Path
$ROOT = Split-Path -Parent (Split-Path -Parent $thisFile)
$WORKSPACE = Split-Path -Parent $ROOT
$CONFIG = Join-Path $ROOT 'config\ports.env'
if (-not (Test-Path -LiteralPath $CONFIG)) { throw "env.ps1: ports.env not found at $CONFIG" }

# 直接把每个 KEY 导出为同名普通变量（最可靠，避开 PowerShell 作用域陷阱）。
# 用法：脚本里直接用 $CP_PORT / $DP_URL / $DEMO_API_KEY ...
foreach ($rawLine in (Get-Content -LiteralPath $CONFIG -Encoding UTF8)) {
    $line = $rawLine.Trim()
    if (-not $line -or $line.StartsWith('#') -or -not $line.Contains('=')) { continue }
    $eqAt = $line.IndexOf('=')
    $key = $line.Substring(0, $eqAt).Trim()
    $val = $line.Substring($eqAt + 1).Trim()
    Set-Variable -Name $key -Value $val -Scope Global -Force
    Set-Item -Path "env:$key" -Value $val -ErrorAction SilentlyContinue
}
if (-not $CP_PORT) { throw "env.ps1: CP_PORT not loaded from $CONFIG" }

$GO = (Get-Command go -ErrorAction Stop).Source
$JAVA11 = 'C:\Java\jdk-11\bin\java.exe'
$JAVAC11 = 'C:\Java\jdk-11\bin\javac.exe'
$JAVA22 = 'C:\Java\jdk-22\bin\java.exe'
$JAVAC22 = 'C:\Java\jdk-22\bin\javac.exe'
$MVN = 'C:\Program Files\apache-maven-3.9.9\bin\mvn.cmd'
$PY = 'C:\Miniconda3\python.exe'
$VENV_PY = Join-Path $ROOT '.venv\Scripts\python.exe'

$DATA_DIR = Join-Path $ROOT 'data'
$LOG_DIR = Join-Path $ROOT 'logs'
$PID_DIR = Join-Path $ROOT '.pids'
foreach ($d in @($DATA_DIR, $LOG_DIR, $PID_DIR)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }
}

function Write-Info($msg) { Write-Host "[INFO ] $msg" -ForegroundColor Cyan }
function Write-Ok($msg)   { Write-Host "[ OK  ] $msg" -ForegroundColor Green }
function Write-Warn($msg) { Write-Host "[WARN ] $msg" -ForegroundColor Yellow }
function Write-Err($msg)  { Write-Host "[ERROR] $msg" -ForegroundColor Red }

function Test-Port($port) {
    try {
        $c = New-Object System.Net.Sockets.TcpClient
        $iar = $c.BeginConnect('127.0.0.1', [int]$port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne(400)) { $c.Close(); return $false }
        $c.EndConnect($iar); $c.Close(); return $true
    } catch { return $false }
}

function Wait-Health($url, $name, $timeoutSec = 30) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            # 用 TcpClient 探活而不是 Invoke-RestMethod：后者在子进程托管 + 中文
            # 控制台下偶发阻塞，且某些服务 /health 返回纯文本会触发解析等待。
            $tcp = New-Object System.Net.Sockets.TcpClient
            $iar = $tcp.BeginConnect('127.0.0.1', [int]($url -replace '^.*:', ''), $null, $null)
            if ($iar.AsyncWaitHandle.WaitOne(600)) {
                $tcp.EndConnect($iar); $tcp.Close()
                Write-Ok "$name ready ($url)"; return $true
            }
            $tcp.Close()
        } catch { }
        Start-Sleep -Milliseconds 400
    }
    Write-Err "$name NOT ready after ${timeoutSec}s ($url)"
    return $false
}
