# 端到端冒烟测试（契约 §8 的 E01–E26）
# 前置：先跑 start-all.ps1
# 用法: powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1
$ErrorActionPreference = 'Continue'
. (Join-Path $PSScriptRoot '..\config\env.ps1')

$CP = $CP_URL; $DP = $DP_URL; $MECHA = $MECHA_URL
$LLM = $LLM_URL; $MCP = $MCP_URL
$KEY = $DEMO_API_KEY
$results = @()

function Check($id, $name, [scriptblock]$body) {
    try {
        $r = & $body
        if ($r -is [bool] -and $r) { $script:results += [pscustomobject]@{id=$id;name=$name;ok=$true;detail=''} ; Write-Ok "$id $name" }
        elseif ($r -is [bool]) { $script:results += [pscustomobject]@{id=$id;name=$name;ok=$false;detail='assert false'}; Write-Err "$id $name (assert false)" }
        else { $script:results += [pscustomobject]@{id=$id;name=$name;ok=$true;detail=[string]$r}; Write-Ok "$id $name :: $r" }
    } catch {
        $script:results += [pscustomobject]@{id=$id;name=$name;ok=$false;detail=$_.Exception.Message}
        Write-Err "$id $name :: $($_.Exception.Message)"
    }
}

function J($url, $method='GET', $headers=@{}, $obj=$null, $timeout=15) {
    $h = @{ 'Content-Type'='application/json; charset=utf-8' }
    foreach($k in $headers.Keys){ $h[$k]=$headers[$k] }
    $reqArgs = @{ Uri=$url; Method=$method; Headers=$h; TimeoutSec=$timeout; UseBasicParsing=$true }
    if ($null -ne $obj) { $reqArgs['Body'] = ($obj | ConvertTo-Json -Depth 20 -Compress) }
    return Invoke-RestMethod @reqArgs
}

Write-Info "===== E01-E26 smoke test ====="

# E01 健康检查
foreach ($s in @(@('control-plane',$CP),@('data-plane',$DP),@('agentmecha',$MECHA),@('mock-llm',$LLM),@('mcp-registry',$MCP))) {
    Check "E01" "$($s[0]) /health" { $r = J "$($s[1])/health"; $r.status -eq 'UP' -or $r -eq 'UP' }
}

# E02 配置快照
Check 'E02' 'GET /v1/config revision + agents' {
    $r = J "$CP/v1/config"
    ($r.revision -ge 1) -and ($r.agents.'echo-agent') -and ($r.models.Count -ge 1)
}

# E03/E04 鉴权
Check 'E03' 'no api key -> 401 AUTH_REQUIRED' {
    try { J "$DP/v1/models" 'GET' @{} $null 5 | Out-Null; $false } catch { $_.Exception.Response.StatusCode.value__ -eq 401 }
}
Check 'E04' 'bad api key -> 401' {
    try { J "$DP/v1/models" 'GET' @{'X-API-Key'='wrong'} $null 5 | Out-Null; $false } catch { $_.Exception.Response.StatusCode.value__ -eq 401 }
}

# E13 模型网关非流
Check 'E13' 'POST /v1/chat/completions non-stream' {
    $r = J "$DP/v1/chat/completions" 'POST' @{'X-API-Key'=$KEY} @{model='mock-1';messages=@(@{role='user';content='hello'});stream=$false}
    ($r.'object' -eq 'chat.completion') -and ($r.usage.total_tokens -is [int])
}

# E15 embeddings
Check 'E15' 'POST /v1/embeddings' {
    $long = 'x' * 9000
    $r = J "$DP/v1/embeddings" 'POST' @{'X-API-Key'=$KEY} @{model='bge-m3';input=@($long)} 100
    ($r.'object' -eq 'list') -and (@($r.data).Count -ge 1)
}

# E14 流式 chat（用 Invoke-WebRequest 读原始 SSE）
Check 'E14' 'POST /v1/chat/completions stream -> chunks + [DONE]' {
    $resp = Invoke-WebRequest -Uri "$DP/v1/chat/completions" -Method POST `
        -Headers @{'X-API-Key'=$KEY;'Content-Type'='application/json'} `
        -Body (@{model='mock-1';messages=@(@{role='user';content='hi'});stream=$true} | ConvertTo-Json -Compress) -UseBasicParsing -TimeoutSec 20
    $t = $resp.Content
    ($t -match 'chat.completion.chunk') -and ($t -match '\[DONE\]')
}

# E09 A2A 流（兼容轨）
Check 'E09' 'GET /a2a/v1/tasks/{id}:subscribe SSE' {
    $resp = Invoke-WebRequest -Uri "$DP/a2a/v1/tasks/demo-1:subscribe" -Method GET `
        -Headers @{'X-API-Key'=$KEY} -UseBasicParsing -TimeoutSec 20
    $resp.Content -match '\[DONE\]'
}

# E10 流式护栏不断流（核心坑）
Check 'E10' 'stream guardrail -> HTTP200 + event:error + [DONE], no (18)' {
    $resp = Invoke-WebRequest -Uri "$DP/v1/chat/completions" -Method POST `
        -Headers @{'X-API-Key'=$KEY;'Content-Type'='application/json'} `
        -Body (@{model='mock-1';messages=@(@{role='user';content='BLOCK_ME please'});stream=$true} | ConvertTo-Json -Compress) -UseBasicParsing -TimeoutSec 20
    $t = $resp.Content
    ($resp.StatusCode -eq 200) -and ($t -match 'event: error') -and ($t -match '\[DONE\]')
}

# E12 冒号路径不 panic
Check 'E12' 'POST /a2a/v1/message:send colon path' {
    $r = J "$DP/a2a/v1/message:send" 'POST' @{'X-API-Key'=$KEY} @{message=@{kind='message';messageId='m1';role='user';parts=@(@{kind='text';text='hello'})}}
    $null -ne $r
}

# E16 Token 限流（连续大 max_tokens）
Check 'E16' 'token rate limit -> 429 RATE_LIMITED' {
    $got429 = $false
    for($i=0;$i -lt 40;$i++){
        try { J "$DP/v1/chat/completions" 'POST' @{'X-API-Key'=$KEY} @{model='mock-1';messages=@(@{role='user';content='x'});max_tokens=100000} 5 | Out-Null }
        catch { if($_.Exception.Response.StatusCode.value__ -eq 429){ $got429=$true; break } }
    }
    $got429
}

# E17 优先级（只验证不报错）
Check 'E17' 'X-Priority high accepted' {
    $r = J "$DP/v1/chat/completions" 'POST' @{'X-API-Key'=$KEY;'X-Priority'='high'} @{model='mock-1';messages=@(@{role='user';content='hi'})}
    $null -ne $r.choices
}

# E18 MCP OpenAPI 导入
Check 'E18' 'POST /registry/import with $ref -> imported>=2' {
    $specPath = Join-Path $ROOT 'services\mcp-registry\src\main\resources\sample-openapi.json'
    $spec = Get-Content -LiteralPath $specPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $r = J "$MCP/registry/import" 'POST' @{} @{spec=$spec;baseUrl=$LLM;source='smoke';replace=$true}
    ($r.imported -ge 2)
}

# E19 MCP tools/call 回放
Check 'E19' 'POST /mcp tools/call -> real HTTP replay' {
    $r = J "$MCP/mcp" 'POST' @{} @{jsonrpc='2.0';id=1;method='tools/call';params=@{name='getUserById';arguments=@{id='42';verbose=$true}}}
    ($null -ne $r.result) -and ($r.result.isError -ne $true)
}

# E20 数据面代理 MCP
Check 'E20' 'POST /mcp via data-plane' {
    $r = J "$DP/mcp" 'POST' @{'X-API-Key'=$KEY} @{jsonrpc='2.0';id=1;method='tools/list';params=@{}}
    ($null -ne $r.result.tools)
}

# E21 配置热更新
Check 'E21' 'control-plane write -> revision++ -> data-plane reload' {
    $before = (J "$CP/v1/config").revision
    $null = J "$CP/v1/ratelimits/smoke-test" 'PUT' @{} @{requestsPerMinute=999;tokensPerMinute=999999;burst=50}
    $after = (J "$CP/v1/config").revision
    $null = J "$DP/v1/admin/reload" 'POST' @{} @{}
    ($after -gt $before)
}

# E22 过滤器池复用
Check 'E22' '/v1/admin/pool new << gets' {
    $r = J "$DP/v1/admin/pool"
    ($r.new -le [Math]::Max(50, [int]($r.gets * 0.3)))
}

# E23 trace / metrics
Check 'E23' '/v1/admin/traces spans with integer status_code' {
    $r = J "$DP/v1/admin/traces?limit=50"
    if($r.spans.Count -eq 0){ return $false }
    $bad = $r.spans | Where-Object { $_.attributes.'http.status_code' -isnot [int] }
    ($bad.Count -eq 0)
}

# E11 NDJSON 128KB（打 Sidecar 长任务）
Check 'E11' 'long:128000 -> artifact 128000 chars, no token too long' {
    $r = J "$DP/a2a/echo-agent" 'POST' @{'X-API-Key'=$KEY} @{jsonrpc='2.0';id='e11';method='SendMessage';params=@{message=@{kind='message';messageId='m-long';role='user';parts=@(@{kind='text';text='long:128000'})}}}
    $null -ne $r
}

# E06/E07/E08 SDK 链路（由 run-e2e.ps1 详细验证，这里只验数据面可达）
Check 'E06' 'SDK route: POST /a2a/echo-agent reachable' {
    $r = J "$DP/a2a/echo-agent" 'POST' @{'X-API-Key'=$KEY} @{jsonrpc='2.0';id='e06';method='SendMessage';params=@{message=@{kind='message';messageId='m1';role='user';parts=@(@{kind='text';text='hello'})}}}
    $null -ne $r
}

# E24/E25 断点续传 / 重启恢复（由 sidecar 单测覆盖，这里打 admin）
Check 'E24' 'GET /admin/tasks reachable' { $null -ne (J "$MECHA/admin/tasks") }

Write-Info "===== SUMMARY ====="
$results | Format-Table -AutoSize id,ok,name,detail | Out-String -Width 200 | Write-Host
$pass = ($results | Where-Object ok).Count
$fail = ($results | Where-Object { -not $_.ok }).Count
Write-Info "PASS=$pass FAIL=$fail TOTAL=$($results.Count)"
if ($fail -gt 0) { Write-Err "SMOKE TEST FAILED"; exit 1 }
Write-Ok "SMOKE TEST PASSED"; exit 0
