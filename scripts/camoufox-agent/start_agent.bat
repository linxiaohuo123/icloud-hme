@echo off
setlocal EnableExtensions DisableDelayedExpansion
chcp 65001 >nul
title Camoufox 自动化上号代理服务
cd /d "%~dp0"

rem 仅从项目根目录 .env 读取代理令牌，不向子进程注入其他配置。
set "PROJECT_ENV=%~dp0..\..\.env"
if not defined ICLOUD_HME_CAMOUFOX_TOKEN if exist "%PROJECT_ENV%" (
    for /f "usebackq eol=# tokens=1,* delims==" %%A in ("%PROJECT_ENV%") do (
        if /i "%%A"=="ICLOUD_HME_CAMOUFOX_TOKEN" set "ICLOUD_HME_CAMOUFOX_TOKEN=%%B"
    )
)
if not defined ICLOUD_HME_CAMOUFOX_TOKEN (
    echo [错误] 未配置 ICLOUD_HME_CAMOUFOX_TOKEN，请在项目根目录 .env 中设置。
    exit /b 1
)

echo ========================================================
echo        iCloud HME - Camoufox 自动化上号代理服务
echo ========================================================
echo.

where uv >nul 2>&1
if %errorlevel% equ 0 (
    echo [1/3] 检测到 uv 包管理器，正在准备独立虚拟环境...
    if not exist ".venv" (
        uv venv .venv
    )
    echo [2/3] 安装依赖包...
    uv pip install -r requirements.txt
    echo [3/3] 下载 Camoufox 定制版 Firefox 核心...
    uv run python -m camoufox fetch
    echo.
    echo 正在启动 Camoufox 代理服务 (端口 8089)...
    uv run python main.py
) else (
    echo [1/3] 使用原生 Python 环境准备...
    if not exist ".venv" (
        python -m venv .venv
    )
    call .venv\Scripts\activate.bat
    echo [2/3] 安装依赖包...
    pip install -r requirements.txt
    echo [3/3] 下载 Camoufox 定制版 Firefox 核心...
    python -m camoufox fetch
    echo.
    echo 正在启动 Camoufox 代理服务 (端口 8089)...
    python main.py
)

pause
