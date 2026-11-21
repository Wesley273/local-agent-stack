# 一键编译全部模块（含依赖下载）
# 用法: powershell -ExecutionPolicy Bypass -File scripts\build-all.ps1
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '..\config\env.ps1')

Write-Info "ROOT=$ROOT"
$fail = 0

function Run-Step($name, [scriptblock]$body) {
    Write-Info "== $name =="
    $t = Get-Date
    try {
        & $body
        $code = $LASTEXITCODE
        $sec = [int]((Get-Date) - $t).TotalSeconds
        if ($code -ne 0) { Write-Err "$name FAILED (exit=$code, ${sec}s)"; $script:fail++ }
        else { Write-Ok "$name OK (${sec}s)" }
    } catch {
        $sec = [int]((Get-Date) - $t).TotalSeconds
        Write-Err "$name EXCEPTION (${sec}s): $_"; $script:fail++
    }
}

# ---------- Go ----------
Push-Location $ROOT
try {
    Run-Step 'go mod download' { & $GO mod download }
    Run-Step 'go mod tidy'     { & $GO mod tidy }
    Run-Step 'go build ./...'  { & $GO build ./... }
    Run-Step 'go test ./...'   { & $GO test ./... -count=1 }
} finally { Pop-Location }

# ---------- Java (JDK 11 服务) ----------
foreach ($svc in @('control-plane', 'mcp-registry')) {
    $pom = Join-Path $ROOT "services\$svc\pom.xml"
    if (-not (Test-Path $pom)) { Write-Warn "$svc/pom.xml missing — skipped"; continue }
    $env:JAVA_HOME = 'C:\Java\jdk-11'
    Run-Step "mvn compile $svc (JDK11)" { & $MVN -B -q -f $pom clean compile }
}

# ---------- Java SDK + Java Agent (JDK 22 + 官方 A2A SDK) ----------
foreach ($p in @('sdk\java', 'examples\java-agent')) {
    $pom = Join-Path $ROOT "$p\pom.xml"
    if (-not (Test-Path $pom)) { Write-Warn "$p/pom.xml missing — skipped"; continue }
    $env:JAVA_HOME = 'C:\Java\jdk-22'
    Run-Step "mvn compile $p (JDK22)" { & $MVN -B -q -f $pom clean compile }
}

# ---------- Python ----------
$req = Join-Path $ROOT 'requirements.txt'
if (Test-Path $req) {
    if (-not (Test-Path $VENV_PY)) { Run-Step 'python -m venv' { & $PY -m venv (Join-Path $ROOT '.venv') } }
    $env:PYTHONIOENCODING = 'utf-8'
    Run-Step 'pip install -r requirements.txt' {
        & $VENV_PY -m pip install -q --disable-pip-version-check -r $req
    }
    Run-Step 'py_compile all .py' {
        $files = Get-ChildItem -Recurse -Filter '*.py' -Path (Join-Path $ROOT 'services'),(Join-Path $ROOT 'sdk'),(Join-Path $ROOT 'examples') -ErrorAction SilentlyContinue
        $bad = 0
        foreach ($f in $files) {
            & $VENV_PY -m py_compile $f.FullName
            if ($LASTEXITCODE -ne 0) { Write-Err "  py_compile $($f.Name)"; $bad++ }
        }
        if ($bad -gt 0) { exit 1 }
        Write-Host "  compiled $($files.Count) .py files"
    }
}

Write-Info "=========================================="
if ($fail -eq 0) { Write-Ok "BUILD ALL PASSED"; exit 0 }
Write-Err "BUILD FAILED: $fail step(s)"; exit 1
