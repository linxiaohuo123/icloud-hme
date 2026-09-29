@echo off
setlocal EnableExtensions DisableDelayedExpansion
title Camoufox 授权代理微服务 (可视化窗口模式)
chcp 65001 >nul
set "PROJECT_ENV=%~dp0.env"
if not defined ICLOUD_HME_CAMOUFOX_TOKEN if exist "%PROJECT_ENV%" (
    for /f "usebackq eol=# tokens=1,* delims==" %%A in ("%PROJECT_ENV%") do (
        if /i "%%A"=="ICLOUD_HME_CAMOUFOX_TOKEN" set "ICLOUD_HME_CAMOUFOX_TOKEN=%%B"
    )
)
if not defined ICLOUD_HME_CAMOUFOX_TOKEN (
    echo [错误] 未配置 ICLOUD_HME_CAMOUFOX_TOKEN，请在项目根目录 .env 中设置。
    exit /b 1
)
cd /d "%~dp0scripts\camoufox-agent"
echo =================================================================
echo   [Camoufox 认证代理] 正在以【可视化前台窗口】模式启动...
echo   说明: 登录时将自动在桌面弹出 Camoufox 浏览器窗口供实时查看
echo =================================================================
set CAMOUFOX_HEADLESS=false
.venv\Scripts\python.exe main.py
if errorlevel 1 (
    echo.
    echo 启动遇到错误，按任意键退出...
)
pause
