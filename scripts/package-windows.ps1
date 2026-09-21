param(
  [string]$Version = "1.0.0",
  [string]$OutputRoot = "build\release"
)

$ErrorActionPreference = "Stop"

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$outputRootFullPath = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $OutputRoot))
$packageName = "icloud-hme-windows-portable-v$Version"
$stageDir = Join-Path $outputRootFullPath $packageName
$zipPath = Join-Path $outputRootFullPath "$packageName.zip"

Write-Host "========================================================" -ForegroundColor Cyan
Write-Host "  iCloud Hide My Email Windows Portable Packaging Script" -ForegroundColor Cyan
Write-Host "========================================================" -ForegroundColor Cyan

# 1. Build frontend
# $ErrorActionPreference 不覆盖原生进程的非零退出码，必须逐个显式检查 $LASTEXITCODE，
# 否则前端/Go 构建失败仍会打出坏包(内嵌 placeholder.txt 或上一版旧前端)。
Write-Host "[1/4] Building web frontend..." -ForegroundColor Yellow
# 必须先手工清理内嵌目录: outDir 位于 web/ 项目根之外，Vite 的 emptyOutDir /
# --emptyOutDir 实测并不会清空它，历史 index-*.js 会持续累积，并被
# //go:embed dist/* 递归打进二进制(每个约 400KB，且旧资源仍可被公开访问)。
# 保留 placeholder.txt(受 git 跟踪，dist 为空时 //go:embed 需要它作为兜底)。
$distDir = Join-Path $repoRoot "internal\webui\dist"
Remove-Item -LiteralPath (Join-Path $distDir "assets") -Recurse -Force -ErrorAction SilentlyContinue
Remove-Item -LiteralPath (Join-Path $distDir "index.html") -Force -ErrorAction SilentlyContinue
Push-Location (Join-Path $repoRoot "web")
try {
  & npm run build
  if ($LASTEXITCODE -ne 0) { throw "npm run build 失败 (exit $LASTEXITCODE)" }
} finally {
  Pop-Location
}
if (-not (Test-Path (Join-Path $distDir "index.html"))) {
  throw "前端构建未产出 index.html，内嵌界面会 503"
}

# 2. Build Go binary
Write-Host "[2/4] Building Windows binary..." -ForegroundColor Yellow
$goCmd = if (Get-Command go -ErrorAction SilentlyContinue) { "go" } elseif (Test-Path "C:\Program Files\Go\bin\go.exe") { "C:\Program Files\Go\bin\go.exe" } else { "go" }
$exePath = Join-Path $repoRoot "icloud-hme.exe"
# 注入构建时间与提交号:否则线上 /api/system/stats 只显示 version，
# 排查问题时无法判断手中二进制与哪个提交对应。
$buildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$commit = "unknown"
if (Get-Command git -ErrorAction SilentlyContinue) {
  $rev = & git -C $repoRoot rev-parse --short HEAD 2>$null
  if ($LASTEXITCODE -eq 0 -and $rev) { $commit = $rev.Trim() }
}
Push-Location $repoRoot
try {
  & $goCmd build -trimpath -ldflags "-s -w -X main.version=$Version -X main.commit=$commit -X main.buildTime=$buildTime" -o $exePath .
  if ($LASTEXITCODE -ne 0) { throw "go build 失败 (exit $LASTEXITCODE)" }
} finally {
  Pop-Location
}

# 3. Stage files
Write-Host "[3/4] Staging portable release files..." -ForegroundColor Yellow
New-Item -ItemType Directory -Path $outputRootFullPath -Force | Out-Null
if (Test-Path -LiteralPath $stageDir) {
  Remove-Item -LiteralPath $stageDir -Recurse -Force
}
if (Test-Path -LiteralPath $zipPath) {
  Remove-Item -LiteralPath $zipPath -Force
}

New-Item -ItemType Directory -Path $stageDir | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stageDir "data") | Out-Null

Copy-Item -LiteralPath $exePath -Destination (Join-Path $stageDir "icloud-hme.exe") -Force
Copy-Item -LiteralPath (Join-Path $repoRoot "start.bat") -Destination (Join-Path $stageDir "start.bat") -Force
Copy-Item -LiteralPath (Join-Path $repoRoot "stop.bat") -Destination (Join-Path $stageDir "stop.bat") -Force
Copy-Item -LiteralPath (Join-Path $repoRoot ".env.example") -Destination (Join-Path $stageDir ".env.example") -Force
if (Test-Path (Join-Path $repoRoot "accounts.json.template")) {
  Copy-Item -LiteralPath (Join-Path $repoRoot "accounts.json.template") -Destination (Join-Path $stageDir "data\accounts.example.json") -Force
}

# 4. Zip archive
Write-Host "[4/4] Creating zip archive: $zipPath..." -ForegroundColor Green
Compress-Archive -Path "$stageDir\*" -DestinationPath $zipPath -CompressionLevel Optimal

Write-Host "Package created successfully: $zipPath" -ForegroundColor Green
