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
DB_FILE="$DATA_DIR/icloud_hme.db"
HAS_DB=false
if [ -f "$DB_FILE" ] && [ -s "$DB_FILE" ]; then
    HAS_DB=true
fi

mkdir -p "$DATA_DIR"
TARGET_BIN="$INSTALL_DIR/icloud-hme_linux_${ARCH}"
install -m 755 "$BIN_SRC" "$TARGET_BIN"
ln -sf "$TARGET_BIN" "$INSTALL_DIR/icloud-hme"
echo "==> 二进制安装至: $TARGET_BIN (软链至 $INSTALL_DIR/icloud-hme)"

# 6. 环境配置文件 /etc/icloud-hme.env 检查与引导
HAS_ENV_FILE=false
if [ -f "$ENV_FILE" ]; then
    HAS_ENV_FILE=true
fi

HAS_MASTER_KEY=false
if [ "$HAS_ENV_FILE" = true ]; then
    if grep -qE '^[[:space:]]*ICLOUD_HME_MASTER_KEY(_FILE)?[[:space:]]*=' "$ENV_FILE"; then
        KEY_VAL="$(grep -E '^[[:space:]]*ICLOUD_HME_MASTER_KEY(_FILE)?[[:space:]]*=' "$ENV_FILE" | head -n 1 | cut -d= -f2- | tr -d '[:space:]')"
        if [ -n "$KEY_VAL" ]; then
            HAS_MASTER_KEY=true
        fi
    fi
fi
if [ -n "$ICLOUD_HME_MASTER_KEY" ] || [ -n "$ICLOUD_HME_MASTER_KEY_FILE" ]; then
    HAS_MASTER_KEY=true
fi

# 【红线阻断】已有数据库但缺少 Master Key 时严禁自动生成新 key，必须中止安装并明确提示
if [ "$HAS_DB" = true ] && [ "$HAS_MASTER_KEY" = false ]; then
    echo "========================================================" >&2
    echo "!! 错误: 检测到已有数据库 ($DB_FILE)，但配置中未找到 Master Key！" >&2
    echo "   严禁自动生成新的 Master Key，以防导致已有加密凭据永久损坏或覆盖。" >&2
    echo "   - 如果数据库已经是 V2：" >&2
    echo "     必须恢复原来的 Master Key，将其写入 $ENV_FILE (ICLOUD_HME_MASTER_KEY=<KEY>)。" >&2
    echo "   - 如果这是 V1 -> V2 第一次升级：" >&2
    echo "     请先生成并安全保存一个新的 32-byte Base64 Master Key (例如运行: openssl rand -base64 32)，" >&2
    echo "     写入 $ENV_FILE 后，再重新执行升级安装。" >&2
    echo "========================================================" >&2
    exit 1
fi

if [ "$HAS_ENV_FILE" = false ]; then
    echo "==> 初始化新环境配置文件: $ENV_FILE"

    # 生成管理员随机强口令 (CSPRNG, 严禁固定 fallback, 失败直接中断)
    GEN_PASSWORD=""
    if command -v openssl >/dev/null 2>&1; then
        GEN_PASSWORD="$(openssl rand -hex 16)"
    elif [ -r /dev/urandom ]; then
        GEN_PASSWORD="$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom 2>/dev/null | head -c 16 || true)"
    fi
    if [ -z "$GEN_PASSWORD" ] || [ ${#GEN_PASSWORD} -lt 8 ]; then
        echo "!! 生成随机管理员密码失败: 系统可靠 CSPRNG 不可用。安装已安全阻断。" >&2
        exit 1
    fi

    # 生成 32 字节 Base64 编码的 Master Key (CSPRNG, 严禁固定 fallback, 失败直接中断)
    GEN_MASTER_KEY=""
    if command -v openssl >/dev/null 2>&1; then
        GEN_MASTER_KEY="$(openssl rand -base64 32)"
    elif [ -r /dev/urandom ]; then
        GEN_MASTER_KEY="$(head -c 32 /dev/urandom 2>/dev/null | base64 | tr -d '\r\n' || true)"
    fi
    if [ -z "$GEN_MASTER_KEY" ]; then
        echo "!! 生成随机 Master Key 失败: 系统可靠 CSPRNG 不可用。安装已安全阻断。" >&2
        exit 1
    fi

    install -m 600 -o root -g root /dev/null "$ENV_FILE"
    cat > "$ENV_FILE" <<EOF
# iCloud Hide My Email 生产环境配置
ICLOUD_HME_ADMIN_PASSWORD=$GEN_PASSWORD
# 【核心机密】Master Key 用于认证加密 Apple Cookies、App 专用密码与通知 Secret。
# 【安全红线】丢失此密钥将无法解密恢复 V2 加密凭据，请务必安全备份！
ICLOUD_HME_MASTER_KEY=$GEN_MASTER_KEY
ICLOUD_HME_SECURE_COOKIE=true
ICLOUD_HME_TRUSTED_PROXIES=127.0.0.1
TZ=Asia/Shanghai
EOF
    echo "--------------------------------------------------------"
    echo "【重要提示】系统已为你自动生成管理员密码与凭据 Master Key:"
    echo "  管理员密码: $GEN_PASSWORD"
    echo "  Master Key: $GEN_MASTER_KEY"
    echo "  配置文件:   $ENV_FILE"
    echo "【安全警告】请务必将 Master Key 安全备份！"
    echo "  Master Key 丢失后将无法解密恢复 V2 数据库内的一切受保护凭据。"
    echo "--------------------------------------------------------"
else
    echo "==> 保留已有配置文件: $ENV_FILE"
    if [ "$HAS_MASTER_KEY" = false ]; then
        echo "【警告】已有配置文件 $ENV_FILE 中未检测到 ICLOUD_HME_MASTER_KEY，请确保服务启动前已正确注入该环境变量。"
    fi
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
