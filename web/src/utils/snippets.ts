/**
 * [INPUT]: 依赖基础字符串参数 origin, tag, token
 * [OUTPUT]: 对外提供 buildLeaseCommand, buildCurlSnippet, buildPythonSnippet
 * [POS]: web/src/utils 的自动化接入脚本与命令模板生成器，服务于 BusinessTagsPage
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

/** 组装外部注册机领号命令；token 传占位符即为示例文案 */
export function buildLeaseCommand(origin: string, tag: string, token: string): string {
  const safeTag = encodeURIComponent(tag)
  return `curl -X POST "${origin}/api/quick-create?tag=${safeTag}" \\\n  -H "Authorization: Bearer ${token}"`
}

/** 组装完整 cURL 接入示例流水线 */
export function buildCurlSnippet(origin: string, tag: string, token: string): string {
  const safeTag = encodeURIComponent(tag)
  return `# ==============================================================================
# 步骤 1: 获取邮箱别名 (支持 URL Query 或 JSON Body，号池就绪时直接分配已缓冲别名)
# 注意事项: 现场向 Apple 申请别名约需 1~2 秒，客户端请求超时务必设为 10 秒以上
# 可选参数: tag (业务标识, 默认 default), label (别名备注), account_id (指定出号账号)
# ==============================================================================
curl -X POST "${origin}/api/quick-create?tag=${safeTag}" \\
  -H "Authorization: Bearer ${token}"

# 步骤 1 响应示例:
# {
#   "success": true,
#   "data": {
#     "email": "mysterious.tiger_0x@icloud.com",
#     "account_id": "acc_1bb36f84",
#     "label": "scheduled",
#     "tag": "${tag}",
#     "created_at": "2026-09-20T10:00:00Z"
#   }
# }

# ==============================================================================
# 步骤 2: 提取验证码 (长轮询等待邮件到达并自动解析)
# 参数说明:
#   email (必填): 待接码别名邮箱
#   timeout (可选): 最大等待秒数 (默认 30, 上限 120)
#   auto_delete (可选): 设为 true 时，接码成功后自动在后台停用别名，释放 iCloud 配额
# ==============================================================================
curl "${origin}/api/verify-code?email=mysterious.tiger_0x@icloud.com&timeout=60&auto_delete=true" \\
  -H "Authorization: Bearer ${token}"

# 步骤 2 响应示例:
# {
#   "success": true,
#   "data": {
#     "email": "mysterious.tiger_0x@icloud.com",
#     "code": "849201",
#     "magic_link": "",
#     "subject": "TikTok Verification Code: 849201",
#     "from": "verify@account.tiktok.com",
#     "date": "2026-09-20T10:01:00Z"
#   }
# }`
}

/** 组装完整 Python 接入示例流水线 */
export function buildPythonSnippet(origin: string, tag: string, token: string): string {
  return `import requests
import time

# iCloud HME 自动化注册机完整流水线示例
BASE_URL = "${origin}"
API_TOKEN = "${token}"
TAG = "${tag}"

headers = {
    "Authorization": f"Bearer {API_TOKEN}",
    "Content-Type": "application/json"
}

# ----------------------------------------------------------------------
# 步骤 1: 获取邮箱别名 (现场向 Apple 申请约需 1~2 秒，timeout 建议设为 10 秒以上)
# ----------------------------------------------------------------------
lease_resp = requests.post(
    f"{BASE_URL}/api/quick-create",
    params={"tag": TAG},
    headers=headers,
    timeout=10
).json()

if not lease_resp.get("success"):
    raise RuntimeError(f"领号失败: {lease_resp.get('message')}")

email = lease_resp["data"]["email"]
print(f"[*] 成功获取别名邮箱: {email} (业务标识: {TAG})")

# ----------------------------------------------------------------------
# 步骤 2: 在目标平台填表注册，触发验证码邮件
# (此处调用你的自动化注册逻辑，如 Selenium / Playwright / 协议提交)
# ----------------------------------------------------------------------
# time.sleep(3)

# ----------------------------------------------------------------------
# 步骤 3: 提取验证码 (长轮询等待邮件，auto_delete=True 成功后自动释放配额)
# ----------------------------------------------------------------------
verify_resp = requests.get(
    f"{BASE_URL}/api/verify-code",
    params={
        "email": email,
        "timeout": 60,
        "auto_delete": "true"  # 接码成功后自动后台停用该别名，释放配额
    },
    headers=headers,
    timeout=65
).json()

if verify_resp.get("success"):
    data = verify_resp["data"]
    print(f"[+] 提取验证码成功: {data.get('code')}")
    print(f"[+] 邮件主题: {data.get('subject')}")
else:
    print(f"[-] 等待验证码超时或失败: {verify_resp.get('message')}")
`
}
