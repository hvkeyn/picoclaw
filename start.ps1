# PicoClaw - Unified Setup & Launch Script
# Builds picoclaw + dashboard, then opens the web panel

$ErrorActionPreference = "Stop"

$ProjectDir     = $PSScriptRoot
$BuildDir       = Join-Path $ProjectDir "build"
$BinaryPath     = Join-Path $BuildDir "picoclaw.exe"
$DashboardPath  = Join-Path $BuildDir "dashboard.exe"
$CmdDir         = Join-Path $ProjectDir "cmd\picoclaw"
$DashboardPort  = 18080

function Write-Step($icon, $msg) {
    Write-Host "  $icon " -NoNewline
    Write-Host $msg
}

function Write-Header($msg) {
    Write-Host ""
    Write-Host "  $msg" -ForegroundColor Cyan
    Write-Host "  $('-' * $msg.Length)" -ForegroundColor DarkGray
}

# ── Banner ──────────────────────────────────────────────
Write-Host ""
Write-Host "  PicoClaw Launcher" -ForegroundColor Yellow
Write-Host "  =================" -ForegroundColor DarkGray

# ── 1. Check Go ─────────────────────────────────────────
Write-Header "Prerequisites"

$goCmd = Get-Command go -ErrorAction SilentlyContinue
if (-not $goCmd) {
    Write-Step "X" "Go is not installed. Download from https://go.dev/dl/"
    Write-Host ""
    Start-Process "https://go.dev/dl/"
    exit 1
}
$goVer = (go version) -replace 'go version ',''
Write-Step ([char]0x2713) "Go: $goVer"

# ── 2. Dependencies ─────────────────────────────────────
Write-Header "Dependencies"

$goSumExists = Test-Path (Join-Path $ProjectDir "go.sum")
$needDeps = $false
if (-not $goSumExists) {
    $needDeps = $true
} else {
    $modResult = go mod verify 2>&1
    if ($LASTEXITCODE -ne 0) { $needDeps = $true }
}

if ($needDeps) {
    Write-Step "..." "Downloading dependencies..."
    Push-Location $ProjectDir
    go mod download
    if ($LASTEXITCODE -ne 0) {
        Write-Step "X" "Failed to download dependencies"
        Pop-Location
        exit 1
    }
    Pop-Location
    Write-Step ([char]0x2713) "Dependencies installed"
} else {
    Write-Step ([char]0x2713) "Dependencies OK"
}

# ── 3. Build picoclaw ──────────────────────────────────
Write-Header "Build"

if (-not (Test-Path $BuildDir)) {
    New-Item -ItemType Directory -Path $BuildDir -Force | Out-Null
}

# Determine if rebuild is needed (check source modification times)
function NeedsBuild($target, $srcDir) {
    if (-not (Test-Path $target)) { return $true }
    $targetTime = (Get-Item $target).LastWriteTime
    $sourceFiles = Get-ChildItem -Path $srcDir -Include "*.go","*.html" -Recurse -ErrorAction SilentlyContinue |
        Where-Object { $_.FullName -notlike "*\build\*" }
    if (-not $sourceFiles) { return $true }
    $newest = ($sourceFiles | Sort-Object LastWriteTime -Descending | Select-Object -First 1).LastWriteTime
    return $newest -gt $targetTime
}

# -- picoclaw binary --
if (NeedsBuild $BinaryPath $ProjectDir) {
    Write-Step "..." "Building picoclaw..."

    # Copy workspace for go:embed
    $workspaceSrc = Join-Path $ProjectDir "workspace"
    $workspaceDst = Join-Path $CmdDir "workspace"
    if (Test-Path $workspaceSrc) {
        if (Test-Path $workspaceDst) { Remove-Item $workspaceDst -Recurse -Force }
        Copy-Item $workspaceSrc $workspaceDst -Recurse
    }

    $version = "dev"
    $gitCommit = "unknown"
    $buildTime = Get-Date -Format "yyyy-MM-ddTHH:mm:ss"
    $goVersion = ((go version) -split ' ')[2]

    try {
        $v = git describe --tags --always --dirty 2>$null
        if ($v) { $version = $v }
        $c = git rev-parse --short=8 HEAD 2>$null
        if ($c) { $gitCommit = $c }
    } catch {}

    $ldflags = "-X main.version=$version -X main.gitCommit=$gitCommit -X main.buildTime=$buildTime -X main.goVersion=$goVersion -s -w"

    Push-Location $ProjectDir
    cmd /c "go build -v -tags stdjson -ldflags `"$ldflags`" -o `"$BinaryPath`" ./cmd/picoclaw"
    $buildResult = $LASTEXITCODE
    Pop-Location

    if ($buildResult -ne 0) {
        Write-Step "X" "picoclaw build failed"
        exit 1
    }
    Write-Step ([char]0x2713) "picoclaw.exe built"
} else {
    Write-Step ([char]0x2713) "picoclaw.exe up to date"
}

# -- dashboard binary --
if (NeedsBuild $DashboardPath (Join-Path $ProjectDir "cmd\dashboard")) {
    Write-Step "..." "Building dashboard..."

    Push-Location $ProjectDir
    cmd /c "go build -v -o `"$DashboardPath`" ./cmd/dashboard"
    $buildResult = $LASTEXITCODE
    Pop-Location

    if ($buildResult -ne 0) {
        Write-Step "X" "dashboard build failed"
        exit 1
    }
    Write-Step ([char]0x2713) "dashboard.exe built"
} else {
    Write-Step ([char]0x2713) "dashboard.exe up to date"
}

# ── 4. Check if dashboard already running ───────────────
$dashURL = "http://localhost:$DashboardPort"
$alreadyRunning = $false
try {
    $resp = Invoke-WebRequest -Uri $dashURL -TimeoutSec 2 -ErrorAction Stop
    if ($resp.StatusCode -eq 200) { $alreadyRunning = $true }
} catch {}

if ($alreadyRunning) {
    Write-Header "Status"
    Write-Step ([char]0x2713) "Dashboard already running at $dashURL"
    Start-Process $dashURL
    exit 0
}

# ── 5. Launch dashboard ────────────────────────────────
Write-Header "Launching"
Write-Step "..." "Starting web dashboard on port $DashboardPort..."
Write-Host ""

Push-Location $ProjectDir
$dashProcess = Start-Process -FilePath $DashboardPath `
    -ArgumentList "--port $DashboardPort" `
    -PassThru -NoNewWindow
Pop-Location

# Wait for dashboard to become available
$maxWait = 10
$waited = 0
$ready = $false
while ($waited -lt $maxWait) {
    Start-Sleep -Seconds 1
    $waited++
    if ($dashProcess.HasExited) {
        Write-Step "X" "Dashboard exited with code $($dashProcess.ExitCode)"
        exit 1
    }
    try {
        $resp = Invoke-WebRequest -Uri $dashURL -TimeoutSec 2 -ErrorAction Stop
        if ($resp.StatusCode -eq 200) { $ready = $true; break }
    } catch {}
}

Write-Host ""
if ($ready) {
    Write-Step ([char]0x2713) "Dashboard running at $dashURL"
    Write-Host ""
    Write-Host "  Open in browser: $dashURL" -ForegroundColor Green
    Write-Host "  Press Ctrl+C to stop." -ForegroundColor DarkGray
    Write-Host ""
} else {
    Write-Step "!" "Dashboard may still be starting... PID: $($dashProcess.Id)"
}

try {
    Wait-Process -Id $dashProcess.Id
} catch {}

Write-Host ""
Write-Step ([char]0x2713) "Dashboard stopped."
