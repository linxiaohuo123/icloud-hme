@echo off
title Camoufox 浏览器可视化启动器
chcp 65001 >nul
cd /d %~dp0scripts\camoufox-agent
echo ====================================================
echo   正在为你启动 Camoufox 浏览器窗口...
echo ====================================================
set CAMOUFOX_HEADLESS=false
.venv\Scripts\python.exe show_browser.py
if errorlevel 1 (
    echo.
    echo 启动遇到异常，按任意键退出...
)
pause
