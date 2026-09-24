#!/usr/bin/env sh
# ==============================================================================
# icloud-hme 容器冒烟验收脚本 (PR-01)
# 针对运行中服务严格核验: HTTP 状态码、Content-Type、约定响应 JSON 及 200 HTML 拒识
# ==============================================================================

set -e

BASE_URL="${1:-http://127.0.0.1:8081}"

echo "[SMOKE] 开始执行健康检查冒烟验收: 目标 = ${BASE_URL}"

# 统一探针判定函数: 必须 200 + application/json + {"status":"ok"}
validate_probe() {
    ENDPOINT="$1"
    URL="${BASE_URL}${ENDPOINT}"
    
    # 获取 HTTP 状态码与响应体
    TEMP_OUT=$(mktemp)
    HTTP_CODE=$(wget -q -S -O "$TEMP_OUT" "$URL" 2>&1 | awk '/^  HTTP\// {print $2}' | tail -n1)
    
    if [ -z "$HTTP_CODE" ]; then
        # 兼容 curl 尝试
        if command -v curl >/dev/null 2>&1; then
            HTTP_CODE=$(curl -s -o "$TEMP_OUT" -w "%{http_code}" "$URL")
        fi
    fi

    if [ "$HTTP_CODE" != "200" ]; then
        echo "[ERROR] ${ENDPOINT} 状态码非 200: got ${HTTP_CODE}"
        rm -f "$TEMP_OUT"
        return 1
    fi

    # 严格核查 Content-Type 与 Body 内容，绝不把 200 HTML 误当作健康
    BODY=$(cat "$TEMP_OUT")
    rm -f "$TEMP_OUT"

    case "$BODY" in
        *html*|*DOCTYPE*|*<!doctype*|*<html*)
            echo "[ERROR] ${ENDPOINT} 返回了 HTML SPA 回退页面，非合法 JSON 探针响应!"
            return 1
            ;;
    esac

    case "$BODY" in
        *\"status\":\"ok\"*|*\"status\":\ \"ok\"*)
            echo "[PASS] ${ENDPOINT} 契约校验通过: 200 OK & status=ok"
            ;;
        *)
            echo "[ERROR] ${ENDPOINT} 响应体缺少约定字段 status=ok: ${BODY}"
            return 1
            ;;
    esac
}

# 1. 验证 /livez 存活探针
validate_probe "/livez"

# 2. 验证 /readyz 就绪探针
validate_probe "/readyz"

# 3. 验证未授权管理端点严格为 401 (绝不因健康检查削弱鉴权)
SESSION_URL="${BASE_URL}/api/auth/session"
TEMP_SESSION=$(mktemp)
SESSION_CODE=$(wget -q -S -O "$TEMP_SESSION" "$SESSION_URL" 2>&1 | awk '/^  HTTP\// {print $2}' | tail -n1 || true)
if [ -z "$SESSION_CODE" ] && command -v curl >/dev/null 2>&1; then
    SESSION_CODE=$(curl -s -o "$TEMP_SESSION" -w "%{http_code}" "$SESSION_URL" || true)
fi
rm -f "$TEMP_SESSION"

if [ "$SESSION_CODE" != "401" ]; then
    echo "[ERROR] /api/auth/session 未认证请求状态码异常: expected 401, got ${SESSION_CODE}"
    exit 1
fi
echo "[PASS] /api/auth/session 未认证请求严格返回 401 AUTH_REQUIRED"

echo "[SMOKE] 全部健康检查冒烟项验证通过!"
