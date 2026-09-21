# [INPUT]: 项目源码、Go 编译器、Node.js 运行时及可选命令行参数 (Version, Architecture, OutputRoot)
# [OUTPUT]: 在 build/release/ 下生成可直接上传至 Linux 服务器解压部署的 icloud-hme-linux-{arch}-v{version}.tar.gz 完整交付包
# [POS]: scripts/ 的 Linux 跨平台构建与分发自动化工具，与 package-windows.ps1 对称
# [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

param(
  [string]$Version = "1.0.0",
  [ValidateSet("amd64", "arm64", "all")][string]$Architecture = "amd64",
  [string]$OutputRoot = "build\release"
)

$ErrorActionPreference = "Stop"

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$outputRootFullPath = [System.IO.Path]::GetFullPath((Join-Path $repoRoot $OutputRoot))

Write-Host "========================================================" -ForegroundColor Cyan
Write-Host "  iCloud Hide My Email Linux 生产包自动化构建脚本" -ForegroundColor Cyan
Write-Host "========================================================" -ForegroundColor Cyan

# 1. 构建内嵌 Web 前端
Write-Host "[1/4] 清理陈旧产物并构建前端..." -ForegroundColor Yellow
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

# 2. 准备构建元数据与编译器
Write-Host "[2/4] 准备 Go 编译器与注入元信息..." -ForegroundColor Yellow
$goCmd = if (Get-Command go -ErrorAction SilentlyContinue) { "go" } elseif (Test-Path "C:\Program Files\Go\bin\go.exe") { "C:\Program Files\Go\bin\go.exe" } else { "go" }
$buildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$commit = "unknown"
if (Get-Command git -ErrorAction SilentlyContinue) {
  $rev = & git -C $repoRoot rev-parse --short HEAD 2>$null
  if ($LASTEXITCODE -eq 0 -and $rev) { $commit = $rev.Trim() }
}

$targetArchs = if ($Architecture -eq "all") { @("amd64", "arm64") } else { @($Architecture) }

New-Item -ItemType Directory -Path $outputRootFullPath -Force | Out-Null

foreach ($arch in $targetArchs) {
  $packageName = "icloud-hme-linux-$arch-v$Version"
  $stageDir = Join-Path $outputRootFullPath $packageName
  $tarPath = Join-Path $outputRootFullPath "$packageName.tar.gz"

  Write-Host "[3/4] 交叉编译 Linux $arch 二进制..." -ForegroundColor Yellow
  $binName = "icloud-hme_linux_$arch"
  $binPath = Join-Path $stageDir $binName

  if (Test-Path -LiteralPath $stageDir) {
    Remove-Item -LiteralPath $stageDir -Recurse -Force
  }
  if (Test-Path -LiteralPath $tarPath) {
    Remove-Item -LiteralPath $tarPath -Force
  }

  New-Item -ItemType Directory -Path $stageDir | Out-Null
  New-Item -ItemType Directory -Path (Join-Path $stageDir "data") | Out-Null
  New-Item -ItemType Directory -Path (Join-Path $stageDir "deploy") | Out-Null

  Push-Location $repoRoot
  try {
    $env:GOOS = "linux"
    $env:GOARCH = $arch
    $env:CGO_ENABLED = "0"
    & $goCmd build -trimpath -ldflags "-s -w -buildid= -X main.version=$Version -X main.commit=$commit -X main.buildTime=$buildTime" -o $binPath .
    if ($LASTEXITCODE -ne 0) { throw "go build ($arch) 失败 (exit $LASTEXITCODE)" }
  } finally {
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    Pop-Location
  }

  # 复制一份作为标准名称 icloud-hme
  Copy-Item -LiteralPath $binPath -Destination (Join-Path $stageDir "icloud-hme") -Force

  Write-Host "[4/4] 收集配置模板与运维服务文件 ($arch)..." -ForegroundColor Yellow
  Copy-Item -LiteralPath (Join-Path $repoRoot "icloud-hme.service") -Destination (Join-Path $stageDir "icloud-hme.service") -Force
  Copy-Item -LiteralPath (Join-Path $repoRoot ".env.example") -Destination (Join-Path $stageDir ".env.example") -Force
  Copy-Item -LiteralPath (Join-Path $repoRoot "deploy\install.sh") -Destination (Join-Path $stageDir "install.sh") -Force
  Copy-Item -LiteralPath (Join-Path $repoRoot "deploy\install.sh") -Destination (Join-Path $stageDir "deploy\install.sh") -Force
  Copy-Item -LiteralPath (Join-Path $repoRoot "deploy\nginx.conf") -Destination (Join-Path $stageDir "deploy\nginx.conf") -Force
  Copy-Item -LiteralPath (Join-Path $repoRoot "deploy\Caddyfile") -Destination (Join-Path $stageDir "deploy\Caddyfile") -Force
  if (Test-Path (Join-Path $repoRoot "accounts.json.template")) {
    Copy-Item -LiteralPath (Join-Path $repoRoot "accounts.json.template") -Destination (Join-Path $stageDir "data\accounts.example.json") -Force
  }

  # 压缩为 tar.gz
  $tarCmd = Get-Command tar.exe -ErrorAction SilentlyContinue
  if ($tarCmd) {
    Write-Host "正在生成标准 Linux 归档: $tarPath..." -ForegroundColor Green
    & tar.exe -czf $tarPath -C $outputRootFullPath $packageName
    if ($LASTEXITCODE -ne 0) {
      Write-Warning "tar.exe 打包失败，尝试使用 Compress-Archive 生成 zip 兜底"
      Compress-Archive -Path "$stageDir\*" -DestinationPath (Join-Path $outputRootFullPath "$packageName.zip") -CompressionLevel Optimal
    }
  } else {
    Write-Host "未检测到 tar.exe，生成 zip 交付包..." -ForegroundColor Green
    Compress-Archive -Path "$stageDir\*" -DestinationPath (Join-Path $outputRootFullPath "$packageName.zip") -CompressionLevel Optimal
  }

  Write-Host "交付包构建成功: $packageName" -ForegroundColor Green
}

Write-Host "全部完成! 产物位于: $outputRootFullPath" -ForegroundColor Cyan
