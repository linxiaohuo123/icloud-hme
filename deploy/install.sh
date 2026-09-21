#!/bin/bash
# [INPUT]: Linux 操作系统环境、root/sudo 权限，当前解包或源码目录
# [OUTPUT]: 自动化完成系统用户创建、目录赋权、二进制部署、环境变量引导与 systemd 服务注册
# [POS]: deploy/ 的 Linux 一键幂等安装与升级运维脚本
# [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

set -e

INSTALL_DIR="/opt/icloud-hme"
DATA_DIR="$INSTALL_DIR/data"
SERVICE_NAME="icloud-hme"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
ENV_FILE="/etc/icloud-hme.env"

echo "========================================================"
echo "  iCloud Hide My Email Linux 生产环境部署安装向导"
echo "========================================================"

# 1. 权限检查
if [ "$(id -u)" -ne 0 ]; then
    echo "!! 请使用 root 或 sudo 权限运行此脚本: sudo bash $0" >&2
    exit 1
fi

# 2. 系统架构探测
RAW_ARCH="$(uname -m)"
case "$RAW_ARCH" in
    x86_64|amd64)
        ARCH="amd64"
        ;;
    aarch64|arm64)
        ARCH="arm64"
        ;;
    *)
        echo "!! 不受支持的 CPU 架构: $RAW_ARCH (仅支持 amd64 / arm64)" >&2
        exit 1
        ;;
esac
echo "==> 检测到系统架构: $ARCH ($RAW_ARCH)"

# 3. 寻找待安装的二进制
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

CANDIDATES=(
    "$SCRIPT_DIR/icloud-hme_linux_${ARCH}"
    "$SCRIPT_DIR/icloud-hme"
    "$ROOT_DIR/build/icloud-hme_linux_${ARCH}"
    "$ROOT_DIR/build/icloud-hme"
    "$ROOT_DIR/icloud-hme_linux_${ARCH}"
    "$ROOT_DIR/icloud-hme"
)

BIN_SRC=""
for c in "${CANDIDATES[@]}"; do
    if [ -f "$c" ]; then
        BIN_SRC="$c"
        break
    fi
done

if [ -z "$BIN_SRC" ]; then
    echo "!! 未找到适合当前架构 ($ARCH) 的二进制文件。" >&2
    echo "   候选路径均不存在: ${CANDIDATES[*]}" >&2
    echo "   请先在源码根目录运行构建或下载 Release 产物。" >&2
    exit 1
fi
echo "==> 选用二进制文件: $BIN_SRC"

# 4. 创建专有隔离系统用户
NOLOGIN_SHELL="/usr/sbin/nologin"
if [ ! -x "$NOLOGIN_SHELL" ]; then
    if [ -x "/sbin/nologin" ]; then
        NOLOGIN_SHELL="/sbin/nologin"
    else
        NOLOGIN_SHELL="/bin/false"
    fi
fi

if ! id -u icloud-hme >/dev/null 2>&1; then
    echo "==> 创建系统服务用户 icloud-hme (Shell: $NOLOGIN_SHELL)"
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --user-group --home "$INSTALL_DIR" --shell "$NOLOGIN_SHELL" icloud-hme 2>/dev/null || \
        useradd --system --home "$INSTALL_DIR" --shell "$NOLOGIN_SHELL" icloud-hme
    elif command -v adduser >/dev/null 2>&1; then
        adduser -S -D -h "$INSTALL_DIR" -s "$NOLOGIN_SHELL" icloud-hme
    fi
else
    echo "==> 系统服务用户 icloud-hme 已存在"
fi

# 5. 准备目录结构与可执行程序
mkdir -p "$DATA_DIR"
TARGET_BIN="$INSTALL_DIR/icloud-hme_linux_${ARCH}"
install -m 755 "$BIN_SRC" "$TARGET_BIN"
ln -sf "$TARGET_BIN" "$INSTALL_DIR/icloud-hme"
echo "==> 二进制安装至: $TARGET_BIN (软链至 $INSTALL_DIR/icloud-hme)"

# 6. 初始化环境配置文件 /etc/icloud-hme.env
if [ ! -f "$ENV_FILE" ]; then
    echo "==> 初始化配置文件: $ENV_FILE"
    if command -v openssl >/dev/null 2>&1; then
        GEN_PASSWORD="$(openssl rand -hex 12)"
    else
        GEN_PASSWORD="$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom 2>/dev/null | head -c 16 || echo 'IcloudHmePass99Secret')"
    fi
    install -m 600 -o root -g root /dev/null "$ENV_FILE"
    cat > "$ENV_FILE" <<EOF
# iCloud Hide My Email 生产环境配置
ICLOUD_HME_ADMIN_PASSWORD=$GEN_PASSWORD
ICLOUD_HME_SECURE_COOKIE=true
ICLOUD_HME_TRUSTED_PROXIES=127.0.0.1
TZ=Asia/Shanghai
EOF
    echo "--------------------------------------------------------"
    echo "【重要提示】系统已为你自动生成管理员初始密码:"
    echo "  管理员密码: $GEN_PASSWORD"
    echo "  配置文件:   $ENV_FILE"
    echo "--------------------------------------------------------"
else
    echo "==> 保留已有配置文件: $ENV_FILE"
fi

# 7. 部署并调整 systemd 服务
SVC_SRC=""
if [ -f "$SCRIPT_DIR/icloud-hme.service" ]; then
    SVC_SRC="$SCRIPT_DIR/icloud-hme.service"
elif [ -f "$ROOT_DIR/icloud-hme.service" ]; then
    SVC_SRC="$ROOT_DIR/icloud-hme.service"
fi

if [ -n "$SVC_SRC" ]; then
    echo "==> 安装 systemd 服务: $SERVICE_FILE"
    cp "$SVC_SRC" "$SERVICE_FILE"
    # 统一将 ExecStart 对齐到 /opt/icloud-hme/icloud-hme 通用入口
    sed -i "s|/opt/icloud-hme/icloud-hme_linux_[a-zA-Z0-9_]*|/opt/icloud-hme/icloud-hme|g" "$SERVICE_FILE"
fi

# 8. 修正数据目录权限
chown -R icloud-hme:icloud-hme "$INSTALL_DIR" 2>/dev/null || chown -R icloud-hme "$INSTALL_DIR"

# 9. 重新加载并启动
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME"
    echo "==> systemd 服务已加载并设为开机自启"
else
    echo "==> 未检测到运行中的 systemd 环境，跳过服务自动注册 (可手动运行 /opt/icloud-hme/icloud-hme 启动)"
fi

echo ""
echo "========================================================"
echo "  安装完成! 运维常用命令:"
echo "    启动服务:   systemctl start $SERVICE_NAME"
echo "    停止服务:   systemctl stop $SERVICE_NAME"
echo "    查看状态:   systemctl status $SERVICE_NAME"
echo "    查看日志:   journalctl -u $SERVICE_NAME -f"
echo "========================================================"
