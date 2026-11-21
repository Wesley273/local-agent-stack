# 三语言 SDK 端到端验证（E06/E07/E08 + 全链路）
# 前置：start-all.ps1 已跑通
$ErrorActionPreference = 'Continue'
. (Join-Path $PSScriptRoot '..\config\env.ps1')
$pyExe = if (Test-Path $VENV_PY) { $VENV_PY } else { $PY }
$env:PYTHONIOENCODING = 'utf-8'

Write-Info "===== 三语言 SDK 端到端 ====="
$fail = 0

# 用临时文件重定向 stdout/stderr：彻底避开 PowerShell 把 native stderr
# 包装成 NativeCommandError、以及 cmd /c 对含空格 classpath 的引号破坏。
function Invoke-Native($label, $file, $argList) {
    $outFile = Join-Path $LOG_DIR "e2e-$label.out.txt"
    $errFile = Join-Path $LOG_DIR "e2e-$label.err.txt"
    $oldPref = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $safeArgs = @($argList | Where-Object { $null -ne $_ -and $_ -ne '' })
        $sp = @{ FilePath = $file; WorkingDirectory = $ROOT; PassThru = $true; WindowStyle = 'Hidden'; Wait = $true;
                 RedirectStandardOutput = $outFile; RedirectStandardError = $errFile }
        if ($safeArgs.Count -gt 0) { $sp['ArgumentList'] = $safeArgs }
        $p = Start-Process @sp
        $code = $p.ExitCode
    } finally { $ErrorActionPreference = $oldPref }
    $out = (Get-Content -LiteralPath $outFile -Raw -Encoding UTF8 -ErrorAction SilentlyContinue) +
           (Get-Content -LiteralPath $errFile -Raw -Encoding UTF8 -ErrorAction SilentlyContinue)
    return [pscustomobject]@{ Label=$label; Code=$code; Out=$out }
}

# Go SDK example
$goExe = Join-Path $ROOT 'bin\sdk-go-example.exe'
if (Test-Path (Join-Path $ROOT 'sdk\go\example\main.go')) {
    Write-Info "-- Go SDK example --"
    Push-Location $ROOT
    & $GO build -o $goExe .\sdk\go\example | Out-Null
    $buildOk = ($LASTEXITCODE -eq 0)
    Pop-Location
    if ($buildOk) {
        $r = Invoke-Native 'go' $goExe @()
        if ($r.Code -eq 0) { Write-Ok "Go SDK example OK" } else { Write-Err "Go SDK example failed (exit=$($r.Code))"; $fail++ }
        ($r.Out -split "`n" | Where-Object { $_.Trim() } | Select-Object -Last 6) | ForEach-Object { "   $_" }
    } else { Write-Err "Go SDK example build failed"; $fail++ }
} else { Write-Warn "sdk/go/example/main.go 缺失 — 跳过" }

# Python SDK example
$pyExample = Join-Path $ROOT 'sdk\python\example\main.py'
if (Test-Path $pyExample) {
    Write-Info "-- Python SDK example --"
    # smoke-test 的 E16 会打满 app-demo 的 30/分钟配额；e2e 用 admin-key
    # （走 default 120/分钟）避免 429，契约 §4 的 app-demo 规则不被改动。
    $env:LAS_API_KEY = 'admin-key'
    $r = Invoke-Native 'python' $pyExe @($pyExample)
    if ($r.Code -eq 0) { Write-Ok "Python SDK example OK" } else { Write-Err "Python SDK example failed (exit=$($r.Code))"; $fail++ }
    ($r.Out -split "`n" | Where-Object { $_ -match '^\[OK\]|^\[A2AError\]|^\[ERROR\]|RESULT|未能完成' } | Select-Object -Last 10) | ForEach-Object { "   $_" }
} else { Write-Warn "sdk/python/example/main.py 缺失 — 跳过" }

# Java SDK example (JDK 22)
$javaClasses = Join-Path $ROOT 'sdk\java\target\classes'
if (Test-Path $javaClasses) {
    Write-Info "-- Java SDK example (JDK 22) --"
    $deps = ''
    $cpFile = Join-Path $ROOT 'sdk\java\cp.txt'
    if (Test-Path $cpFile) { $deps = (Get-Content -LiteralPath $cpFile -Raw -Encoding UTF8).Trim() }
    $cp = "$javaClasses;$deps"
    $r = Invoke-Native 'java' $JAVA22 @('-cp', $cp, 'las.sdk.Example')
    if ($r.Code -eq 0) { Write-Ok "Java SDK example OK" } else { Write-Err "Java SDK example failed (exit=$($r.Code))"; $fail++ }
    ($r.Out -split "`n" | Where-Object { $_.Trim() } | Select-Object -Last 6) | ForEach-Object { "   $_" }
} else { Write-Warn "sdk/java/target/classes 缺失（先跑 build-all.ps1）— 跳过" }

# 冒号路径 / NDJSON 128KB 专项
Write-Info "-- 冒号路径 & NDJSON 128KB 专项 --"
$KEY = $DEMO_API_KEY; $DP = $DP_URL
try {
    $r = Invoke-RestMethod -Uri "$DP/a2a/v1/message:send" -Method POST `
        -Headers @{'X-API-Key'=$KEY;'Content-Type'='application/json'} `
        -Body (@{message=@{kind='message';messageId='m1';role='user';parts=@(@{kind='text';text='hello'})}} | ConvertTo-Json -Depth 10 -Compress) `
        -UseBasicParsing -TimeoutSec 20
    $s = "冒号路径 message:send -> $($r | ConvertTo-Json -Compress -Depth 5)"
    Write-Ok $s.Substring(0, [Math]::Min(180, $s.Length))
} catch { Write-Err "冒号路径失败: $_"; $fail++ }

try {
    $r = Invoke-RestMethod -Uri "$DP/a2a/echo-agent" -Method POST `
        -Headers @{'X-API-Key'=$KEY;'Content-Type'='application/json'} `
        -Body (@{jsonrpc='2.0';id='long';method='SendMessage';params=@{message=@{kind='message';messageId='m-long';role='user';parts=@(@{kind='text';text='long:128000'})}}} | ConvertTo-Json -Depth 10 -Compress) `
        -UseBasicParsing -TimeoutSec 60
    $s = "NDJSON long:128000 -> $($r | ConvertTo-Json -Compress -Depth 6)"
    Write-Ok $s.Substring(0, [Math]::Min(220, $s.Length))
} catch { Write-Err "NDJSON 长任务失败: $_"; $fail++ }

if ($fail -eq 0) { Write-Ok "E2E PASSED"; exit 0 }
Write-Err "E2E FAILED: $fail"; exit 1
