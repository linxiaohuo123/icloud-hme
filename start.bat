@echo off
chcp 65001 >nul
title iCloud HME 服务启动器

echo ========================================================
echo        iCloud Hide My Email 自动化管理平台
echo ========================================================
echo.

if not exist "icloud-hme.exe" (
    echo [错误] 未找到 icloud-hme.exe，请先编译或检查文件路径！
    pause
    exit /b 1
)

if not exist ".env" (
    if not exist "data\.env" (
        echo [错误] 未找到 .env 配置文件！
        echo        请复制 .env.example 为 .env 并配置以下两项必填安全密钥：
        echo        1. ICLOUD_HME_ADMIN_PASSWORD (管理员后台登录强口令)
        echo        2. ICLOUD_HME_MASTER_KEY     (32字节Base64凭据根密钥，丢失无法恢复V2凭据)
        echo        出于安全考虑，服务不提供任何默认凭据。
        echo.
        pause
        exit /b 1
    )
)

echo [1/2] 正在检查端口 8081...
for /f "tokens=5" %%a in ('netstat -aon ^| findstr :8081 ^| findstr LISTENING') do (
    echo [提示] 发现端口 8081 已被进程 PID %%a 占用，正在释放...
    taskkill /F /PID %%a >nul 2>&1
)

echo [2/2] 正在启动服务 (默认监听: http://127.0.0.1:8081)...
echo [提示] 管理员密码与 Master Key 取自 .env 配置文件。
echo [提示] 若需排查问题，可在下方命令末尾追加 -debug 以启用调试日志(不影响鉴权)。
echo.

.\icloud-hme.exe -addr 127.0.0.1:8081
pause
