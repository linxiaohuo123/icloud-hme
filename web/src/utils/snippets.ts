/**
 * [INPUT]: 依赖基础字符串参数 origin, tag, token
 * [OUTPUT]: 对外提供 buildLeaseCommand, buildCurlSnippet, buildPythonSnippet
 * [POS]: web/src/utils 的外部 v2 接入脚本与命令模板生成器，服务于 BusinessTagsPage
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

/** 把任意字符串安全包进 shell 单引号 */
function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

/** 组装外部注册机领号命令；token 传占位符即为示例文案 */
export function buildLeaseCommand(origin: string, tag: string, token: string): string {
  return `curl -X POST "${origin}/api/external/v2/allocate" \\
  -H "Authorization: Bearer ${token}" \\
  -H "Idempotency-Key: lease-$(date +%s)-$RANDOM" \\
  -H "Content-Type: application/json" \\
  -d ${shellQuote(JSON.stringify({ tag }))}`
}

/** 组装完整 cURL 接入示例流水线 */
export function buildCurlSnippet(origin: string, tag: string, token: string): string {
  return `# ==============================================================================
# 步骤 1: 认领别名 (仅分配库存池中已就绪的别名，号池为空返回 503 POOL_EMPTY)
# Idempotency-Key 必填：网络重试时复用同一个键，避免重复出号
# 可选参数: tag (业务标识, 默认 default), label (别名备注)
# ==============================================================================
${buildLeaseCommand(origin, tag, token)}

# 步骤 1 响应示例 (记下 allocation_id):
# {
#   "success": true,
#   "data": {
#     "allocation_id": "alloc_6f8b2a1c",
#     "email": "mysterious.tiger_0x@icloud.com",
#     "source": "pool",
#     "status": "allocated",
#     "allocated_at": "2026-09-20T10:00:00Z"
#   }
# }

# ==============================================================================
# 步骤 2: 创建取码任务，锁定邮件基线 (之后到达的邮件才会被认作验证码)
# ==============================================================================
curl -X POST "${origin}/api/external/v2/verification-requests" \\
  -H "Authorization: Bearer ${token}" \\
  -H "Content-Type: application/json" \\
  -d '{"lease_id":"alloc_6f8b2a1c"}'

# 步骤 2 响应示例 (baseline_ready 为 true 后再去目标网站发送验证码):
# {
#   "success": true,
#   "data": { "request_id": "vreq_8a3d1e4f", "status": "ready", "baseline_ready": true }
# }

# ==============================================================================
# 步骤 3: 在目标网站触发发送验证码，然后长轮询取码
# timeout 为最长等待秒数 (默认 0 即时查询，上限 120)
# ==============================================================================
curl "${origin}/api/external/v2/verification-requests/vreq_8a3d1e4f?timeout=60" \\
  -H "Authorization: Bearer ${token}"

# 步骤 3 响应示例:
# {
#   "success": true,
#   "data": {
#     "request_id": "vreq_8a3d1e4f",
#     "alias_email": "mysterious.tiger_0x@icloud.com",
#     "status": "succeeded",
#     "code": "849201",
#     "magic_link": ""
#   }
# }`
}

/** 组装完整 Python 接入示例流水线 */
export function buildPythonSnippet(origin: string, tag: string, token: string): string {
  return `import uuid
import requests

# iCloud HME 外部 v2 接入完整流水线示例
BASE_URL = "${origin}"
API_TOKEN = "${token}"
TAG = ${JSON.stringify(tag)}

headers = {
    "Authorization": f"Bearer {API_TOKEN}",
    "Content-Type": "application/json"
}

# ----------------------------------------------------------------------
# 步骤 1: 认领别名 (网络重试时复用同一个 Idempotency-Key，避免重复出号)
# ----------------------------------------------------------------------
alloc_resp = requests.post(
    f"{BASE_URL}/api/external/v2/allocate",
    json={"tag": TAG},
    headers={**headers, "Idempotency-Key": str(uuid.uuid4())},
    timeout=15
).json()

if not alloc_resp.get("success"):
    raise RuntimeError(f"领号失败: {alloc_resp.get('code')} {alloc_resp.get('message')}")

email = alloc_resp["data"]["email"]
allocation_id = alloc_resp["data"]["allocation_id"]
print(f"[*] 成功获取别名邮箱: {email} (业务标识: {TAG})")

# ----------------------------------------------------------------------
# 步骤 2: 创建取码任务，锁定邮件基线
# ----------------------------------------------------------------------
vreq_resp = requests.post(
    f"{BASE_URL}/api/external/v2/verification-requests",
    json={"lease_id": allocation_id},
    headers=headers,
    timeout=15
).json()

if not vreq_resp.get("success") or not vreq_resp["data"].get("baseline_ready"):
    raise RuntimeError(f"创建取码任务失败: {vreq_resp.get('code')} {vreq_resp.get('message')}")

request_id = vreq_resp["data"]["request_id"]

# ----------------------------------------------------------------------
# 步骤 3: 在目标平台填表注册，触发验证码邮件
# (此处调用你的自动化注册逻辑，如 Selenium / Playwright / 协议提交)
# ----------------------------------------------------------------------

# ----------------------------------------------------------------------
# 步骤 4: 长轮询提取验证码
# ----------------------------------------------------------------------
verify_resp = requests.get(
    f"{BASE_URL}/api/external/v2/verification-requests/{request_id}",
    params={"timeout": 60},
    headers=headers,
    timeout=65
).json()

data = verify_resp.get("data") or {}
if verify_resp.get("success") and data.get("status") == "succeeded":
    print(f"[+] 提取验证码成功: {data.get('code') or data.get('magic_link')}")
else:
    print(f"[-] 未取到验证码: {data.get('status') or verify_resp.get('code')} {verify_resp.get('message', '')}")
`
}
