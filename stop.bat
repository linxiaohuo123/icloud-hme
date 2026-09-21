@echo off
chcp 65001 >nul
title iCloud HME 服务停止器

echo ========================================================
echo        iCloud Hide My Email 服务优雅停止器
echo ========================================================
echo.

echo [1/2] 正在查找并安全终止 icloud-hme.exe 进程...
taskkill /F /IM icloud-hme.exe >nul 2>&1

echo [2/2] 正在检查并释放端口 8081 占用...
for /f "tokens=5" %%a in ('netstat -aon ^| findstr :8081 ^| findstr LISTENING') do (
    echo [提示] 释放端口 8081 占用 PID %%a
    taskkill /F /PID %%a >nul 2>&1
)

echo.
echo ========================================================
echo [成功] iCloud HME 服务已安全停止并释放端口！
echo ========================================================
echo.
pause
