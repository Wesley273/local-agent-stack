# 一键拉起全部服务（按依赖顺序：控制面 → MCP → Mock LLM → Sidecar → 示例 Agent → 数据面）
# 用法: powershell -ExecutionPolicy Bypass -File scripts\start-all.ps1
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '..\config\env.ps1')

$CP = $CP_PORT; $DP = $DP_PORT; $MECHA = $MECHA_PORT
$LLM = $LLM_PORT; $MCP = $MCP_PORT

function Start-Proc($name, $file, $argList, $workdir, $port) {
    if (Test-Port $port) { Write-Warn "$name port $port already in use — assuming already running"; return $true }
    $outLog = Join-Path $LOG_DIR "$name.out.log"
    $errLog = Join-Path $LOG_DIR "$name.err.log"
    $safeArgs = @($argList | Where-Object { $null -ne $_ -and $_ -ne '' })
    $sp = @{ FilePath = $file; WorkingDirectory = $workdir; PassThru = $true; WindowStyle = 'Hidden';
             RedirectStandardOutput = $outLog; RedirectStandardError = $errLog }
    if ($safeArgs.Count -gt 0) { $sp['ArgumentList'] = $safeArgs }
    $p = Start-Process @sp
    Set-Content -Path (Join-Path $PID_DIR "$name.pid") -Value $p.Id -Encoding UTF8
    Write-Info "$name pid=$($p.Id) port=$port logs=$outLog"
    return $true
}

function Read-Cp($path) {
    if (Test-Path $path) { return (Get-Content -LiteralPath $path -Raw -Encoding UTF8).Trim() }
    return ''
}

Write-Info "== 1. 控制面 (Java 11, :$CP) =="
$cpClasses = Join-Path $ROOT 'services\control-plane\target\classes'
$cpDeps = Read-Cp (Join-Path $ROOT 'services\control-plane\cp.txt')
$cpCp = "$cpClasses;$cpDeps"
Start-Proc 'control-plane' $JAVA11 @('-cp', "`"$cpCp`"", 'las.cp.Main', $CP) $ROOT $CP

Write-Info "== 2. MCP 注册中心 (Java 11, :$MCP) =="
$mcpClasses = Join-Path $ROOT 'services\mcp-registry\target\classes'
$mcpDeps = Read-Cp (Join-Path $ROOT 'services\mcp-registry\cp.txt')
$mcpCp = "$mcpClasses;$mcpDeps"
Start-Proc 'mcp-registry' $JAVA11 @('-cp', "`"$mcpCp`"", 'las.mcp.Main', $MCP) $ROOT $MCP

Write-Info "== 3. Mock LLM (Python, :$LLM) =="
$pyExe = if (Test-Path $VENV_PY) { $VENV_PY } else { $PY }
Start-Proc 'mock-llm' $pyExe @('-u', (Join-Path $ROOT 'services\mock-llm\main.py'), $LLM) $ROOT $LLM

Write-Info "== 4. 示例 Agents (Go/Java/Python) =="
$goAgentExe = Join-Path $ROOT 'bin\go-agent.exe'
if (Test-Path $goAgentExe) { Start-Proc 'go-agent' $goAgentExe @($AGENT_GO_PORT) $ROOT $AGENT_GO_PORT }
$javaAgentClasses = Join-Path $ROOT 'examples\java-agent\target\classes'
$javaAgentDeps = Read-Cp (Join-Path $ROOT 'examples\java-agent\cp.txt')
if (Test-Path $javaAgentClasses) { Start-Proc 'java-agent' $JAVA22 @('-cp', "`"$javaAgentClasses;$javaAgentDeps`"", 'las.example.JavaAgent', $AGENT_JAVA_PORT) $ROOT $AGENT_JAVA_PORT }
Start-Proc 'python-agent' $pyExe @('-u', (Join-Path $ROOT 'examples\python-agent\main.py'), $AGENT_PY_PORT) $ROOT $AGENT_PY_PORT

Write-Info "== 5. AgentMecha Sidecar (Go, :$MECHA) =="
$mechaExe = Join-Path $ROOT 'bin\agentmecha.exe'
if (-not (Test-Path $mechaExe)) {
    Push-Location $ROOT; & $GO build -o $mechaExe .\services\agentmecha; Pop-Location
}
Start-Proc 'agentmecha' $mechaExe @() $ROOT $MECHA

Write-Info "== 6. 数据面 AI 网关 (Go, :$DP) =="
$dpExe = Join-Path $ROOT 'bin\data-plane.exe'
if (-not (Test-Path $dpExe)) {
    Push-Location $ROOT; & $GO build -o $dpExe .\services\data-plane; Pop-Location
}
Start-Proc 'data-plane' $dpExe @() $ROOT $DP

Write-Info "== 等待全部 /health =="
$all = @(
    @{n='control-plane'; p=$CP},
    @{n='mcp-registry';  p=$MCP},
    @{n='mock-llm';      p=$LLM},
    @{n='go-agent';      p=$AGENT_GO_PORT},
    @{n='java-agent';    p=$AGENT_JAVA_PORT},
    @{n='python-agent';  p=$AGENT_PY_PORT},
    @{n='agentmecha';    p=$MECHA},
    @{n='data-plane';    p=$DP}
)
$ok = 0
foreach ($s in $all) { if (Wait-Health "http://127.0.0.1:$($s.p)" $s.n 30) { $ok++ } }
Write-Info "ready $ok/$($all.Count)"
Write-Info "DP_URL=$($DP_URL)  CP_URL=$($CP_URL)"
if ($ok -lt $all.Count) { Write-Err "部分服务未就绪，请看 logs\ 目录"; exit 1 }
Write-Ok "ALL SERVICES UP"; exit 0
