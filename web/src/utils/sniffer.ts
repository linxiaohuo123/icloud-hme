/**
 * [INPUT]: 依赖纯文本或 HTML 字符串输入
 * [OUTPUT]: 对外提供 extractVerifyCode, buildSniffContext, stripHtml 与 parseSenderInfo 函数及 SenderInfo 类型
 * [POS]: web/src/utils 的文本分析工具；倒装句支持定语修饰容限并锚定 \bpin\b 与 \bcode\b，独立 6 位数字仅在验证类关键词下回退
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

/**
 * 辅助函数：合并邮件主题与正文摘要构建完整嗅探上下文，消灭短路漏检
 */
export function buildSniffContext(subject?: string, preview?: string): string {
  return [subject, preview].filter(Boolean).join(' ')
}

/**
 * 从文本中提取 4-8 位验证码
 * 优先匹配带有验证码上下文的关键词（支持正向句式与反向倒装句式）
 */
export function extractVerifyCode(text: string): string | null {
  if (!text) return null

  // 1. 上下文强特征正则 (正向关键词优先，支持连字号与倒装语序)
  const contextualPatterns = [
    // 正向: 关键词在前，数字在后 (支持中文、英文、韩文、日文，容限扩充至 32 字符)
    /(?:验证码|校验码|动态码|确认码|安全码|动态口令|口令|인증\s*코드|인증\s*번호|인증번호|임시\s*코드|확인\s*코드|보안\s*코드|認証コード|確認コード)[^\d]{0,32}(?:为|是|:|：|\s)?\s*([0-9]{4,8})\b/i,
    /(?:code|verification|verify|pin|otp|password|passcode)[^\d]{0,12}(?:is|:|：|\s)?\s*([0-9]{4,8})\b/i,
    /(?:code|pin)\s*[:#：]\s*([0-9]{4,8})\b/i,
    // 连字号格式 (例: "code: 123-456", "验证码 839-201", "123 456")
    /(?:验证码|校验码|动态码|确认码|인증\s*코드|인증\s*번호|임시\s*코드|code|verification|verify|pin|otp|password)[^\d]{0,32}(?:is|为|是|:|：|\s)?\s*([0-9]{3,4}[-\s][0-9]{3,4})\b/i,
    // 反向: 对齐后端 reverseContextCodeRegex，\bcode\b 与 \bpin\b 避免误伤，容许修饰定语
    /\b([0-9]{4,8})\b[^\r\n\d]{0,24}?(?:is(?:\s+(?:your|the|a|an))?|为(?:您(?:的)?)?|是(?:你(?:的)?)?|为本次|作为|입니다|입력)?[^\r\n\d]{0,24}?(?:验证码|校验码|动态码|确认码|安全码|动态口令|口令|인증\s*코드|인증\s*번호|인증번호|임시\s*코드|확인\s*코드|보안\s*코드|認証コード|確認コード|one-time\s*password|temporary\s*password|verification\s*code|security\s*code|verify\s*code|auth\s*code|passcode|\bcode\b|otp|\bpin\b)/i,
  ]

  for (const pattern of contextualPatterns) {
    const match = text.match(pattern)
    if (match?.[1]) {
      return match[1].replace(/[-\s]/g, '')
    }
  }

  // 2. 独立 6 位数字仅在验证类关键词下回退 (排除 2020-2035 年月伪码)
  if (!hasVerifyKeyword(text)) return null
  const sixDigitMatches = text.matchAll(/\b([0-9]{6})\b/g)
  for (const m of sixDigitMatches) {
    const val = m[1]
    const num = Number(val)
    if (num >= 202000 && num <= 203599) continue
    return val
  }

  return null
}

function hasVerifyKeyword(text: string): boolean {
  return /(?:验证码|校验码|动态码|确认码|安全码|动态口令|\b口令\b|验证|激活|确认|인증|코드|확인|보안|임시|認証|パスコード|one-time\s*password|temporary\s*password|verification(?:\s+code)?|verify(?:\s+code)?|security\s+code|auth\s+code|passcode|\bcode\b|\botp\b|\bpin\b|\bpassword\b)/i.test(text)
}

/**
 * 剔除 HTML 标签并转换常见实体，获得纯文本
 */
export function stripHtml(html: string): string {
  if (!html) return ''
  return html
    .replace(/<style[^>]*>[\s\S]*?<\/style>/gi, '')
    .replace(/<script[^>]*>[\s\S]*?<\/script>/gi, '')
    .replace(/<[^>]+>/g, ' ')
    .replace(/&nbsp;/gi, ' ')
    .replace(/&amp;/gi, '&')
    .replace(/&lt;/gi, '<')
    .replace(/&gt;/gi, '>')
    .replace(/&quot;/gi, '"')
    .replace(/&#39;/gi, "'")
    .replace(/\s+/g, ' ')
    .trim()
}

export interface SenderInfo {
  name: string
  email: string
  initial: string
  color: string
}

const AVATAR_COLORS = [
  '#2563eb', '#7c3aed', '#db2777', '#ea580c', '#059669',
  '#0891b2', '#4f46e5', '#d97706', '#dc2626', '#475569',
]

function getAvatarColor(str: string): string {
  let hash = 0
  for (let i = 0; i < str.length; i++) {
    hash = (hash << 5) - hash + str.charCodeAt(i)
    hash |= 0
  }
  return AVATAR_COLORS[Math.abs(hash) % AVATAR_COLORS.length]
}

export function parseSenderInfo(raw?: string): SenderInfo {
  const trimmed = (raw || '').trim()
  if (!trimmed) {
    return { name: '未知发件人', email: '', initial: '?', color: '#94a3b8' }
  }

  let name = ''
  let email = ''
  const angleMatch = trimmed.match(/^(.*?)\s*<([^>]+)>$/)
  if (angleMatch) {
    name = angleMatch[1].replace(/^["']|["']$/g, '').trim()
    email = angleMatch[2].trim()
  } else {
    email = trimmed
  }

  if (email.includes('_at_') && email.endsWith('@icloud.com')) {
    const atIdx = email.indexOf('_at_')
    const rest = email.slice(atIdx + 4, email.indexOf('@icloud.com'))
    const parts = rest.split('_')
    const brand = parts.find(
      (p) => p.length > 2 && !['com', 'cn', 'net', 'org', 'email', 'mail', 'service'].includes(p),
    )
    if (!name && brand) {
      name = brand.charAt(0).toUpperCase() + brand.slice(1)
    }
  }

  if (!name) {
    name = email.split('@')[0] || email
  }

  const initial = (name.replace(/^[^a-zA-Z0-9\u4e00-\u9fa5]+/, '').charAt(0) || '?').toUpperCase()
  return {
    name,
    email,
    initial,
    color: getAvatarColor(name || email),
  }
}
