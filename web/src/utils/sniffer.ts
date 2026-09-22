/**
 * [INPUT]: 依赖纯文本或 HTML 字符串输入
 * [OUTPUT]: 对外提供 extractVerifyCode, buildSniffContext, stripHtml 与 parseSenderInfo 函数及 SenderInfo 类型
 * [POS]: web/src/utils 的文本分析工具；支持全球主流语言多语种验证词元、标题括号直提与通用结构通杀提取，严格防守年份与业务负向词干扰
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

/**
 * 辅助函数：合并邮件主题、正文摘要与正文内容构建完整嗅探上下文，消灭短路漏检
 */
export function buildSniffContext(subject?: string, preview?: string, body?: string): string {
  const parts: string[] = []
  if (subject) parts.push(subject)
  if (preview) {
    parts.push(preview.includes('<') && preview.includes('>') ? stripHtml(preview) : preview)
  }
  if (body && body !== preview) {
    parts.push(body.includes('<') && body.includes('>') ? stripHtml(body) : body)
  }
  return parts.filter(Boolean).join(' ')
}

const multiLangKeywordPattern =
  '(?:' +
  // 中文 (简/繁)
  '验证码|校验码|动态码|确认码|安全码|动态口令|口令|验证|激活|确认|' +
  '驗證碼|校驗碼|動態碼|確認碼|安全碼|動態口令|驗證|確認|' +
  // 英文
  'one-time\\s*password|temporary\\s*password|verification(?:\\s*code)?|security(?:\\s*code)?|verify(?:\\s*code)?|' +
  'auth\\s*code|confirmation(?:\\s*code)?|login\\s*code|access\\s*code|passcode|\\bcode\\b|\\botp\\b|\\bpin\\b|\\bpassword\\b|' +
  // 韩文
  '인증\\s*코드|인증\\s*번호|인증번호|임시\\s*코드|확인\\s*코드|보안\\s*코드|패스코드|비밀번호|인증|코드|확인|보안|임시|' +
  // 日文
  '認証コード|確認コード|セキュリティコード|ワンタイムコード|認証|確認|パスコード|セキュリティ|' +
  // 俄文
  'код\\s*подтверждения|проверочный\\s*код|код\\s*безопасности|одноразовый\\s*пароль|код\\s*доступа|\\bкод\\b|пароль|' +
  // 西班牙语 / 葡萄牙语
  'código\\s*de\\s*verificación|código\\s*de\\s*seguridad|código\\s*de\\s*confirmación|código\\s*de\\s*acceso|' +
  'codigo\\s*de\\s*verificacao|código|codigo|clave|' +
  // 法语
  "code\\s*de\\s*vérification|code\\s*de\\s*sécurité|code\\s*de\\s*confirmation|code\\s*d'activation|" +
  // 德语
  'bestätigungscode|bestaetigungscode|sicherheitscode|aktivierungscode|einmalpasswort|bestätigung|' +
  // 意大利语
  'codice\\s*di\\s*verifica|codice\\s*di\\s*sicurezza|codice\\s*di\\s*conferma|codice|' +
  // 越南语
  'mã\\s*xác\\s*thực|mã\\s*xác\\s*nhận|mã\\s*bảo\\s*mật|mã\\s*otp|\\bmã\\b|' +
  // 土耳其语
  'doğrulama\\s*kodu|dogrulama\\s*kodu|güvenlik\\s*kodu|onay\\s*kodu|\\bkod\\b|\\bkodu\\b|doğrulama|' +
  // 阿拉伯语
  'رمز\\s*التحقق|رمز\\s*الأمان|رمز\\s*التأكيد|\\bرمز\\b' +
  ')'

const negativeContextRegex =
  /(?:order|tracking|invoice|receipt|bill|barcode|unicode|encode|ticket|account|phone|tel|fax|订单|发票|账单|快递|运单|编号)/i

/**
 * 从文本中提取 4-8 位验证码
 * 优先匹配带有验证码上下文的关键词（支持正向句式与反向倒装句式）
 */
export function extractVerifyCode(text: string): string | null {
  if (!text) return null

  // 1. 上下文强特征正则 (正向关键词优先，支持连字号与倒装语序)
  const contextualPatterns = [
    // 括号/书名号/引号强标注 (例: "[576932]", "【839201】")
    /(?:\[|\(|【|「|“|")\s*([0-9]{4,8})\s*(?:\]|\)|】|」|”|")/,
    // 正向: 多语言关键词在前，数字在后
    new RegExp(
      `${multiLangKeywordPattern}[^\\r\\n\\d]{0,32}?(?:is|为|是|est|es|ist|è|la|является|para|:|：|\\s)*[:：\\s-]*\\b([0-9]{4,8})\\b`,
      'i',
    ),
    // 连字号格式 (例: "code: 123-456", "인증 987-654")
    new RegExp(
      `${multiLangKeywordPattern}[^\\r\\n\\d]{0,32}?(?:is|为|是|est|es|ist|è|la|является|para|:|：|\\s)*[:：\\s-]*\\b([0-9]{3,4}[-\\s][0-9]{3,4})\\b`,
      'i',
    ),
    // 反向: 数字在前，关键词在后
    new RegExp(
      `\\b([0-9]{4,8})\\b[^\\r\\n\\d]{0,24}?(?:is(?:\\s+(?:your|the|a|an))?|为(?:您(?:的)?)?|是(?:你(?:的)?)?|为本次|作为|입니다|입력|est|es|ist|è|la|является|para)?[^\\r\\n\\d]{0,24}?${multiLangKeywordPattern}`,
      'i',
    ),
  ]

  for (const pattern of contextualPatterns) {
    const match = text.match(pattern)
    if (match?.[1]) {
      const candidate = match[1].replace(/[-\s]/g, '')
      const num = Number(candidate)
      if (num >= 2020 && num <= 2035) continue
      return candidate
    }
  }

  // 2. 独立 6 位数字仅在验证类关键词下回退 (排除 2020-2035 年月伪码与负向业务词)
  if (!hasVerifyKeyword(text)) return null
  const sixDigitMatches = text.matchAll(/\b([0-9]{6})\b/g)
  for (const m of sixDigitMatches) {
    const val = m[1]
    const num = Number(val)
    if (num >= 202000 && num <= 203599) continue
    const idx = m.index ?? 0
    const windowStart = Math.max(0, idx - 30)
    const windowEnd = Math.min(text.length, idx + val.length + 30)
    const windowText = text.slice(windowStart, windowEnd)
    if (negativeContextRegex.test(windowText)) continue
    return val
  }

  return null
}

function hasVerifyKeyword(text: string): boolean {
  return new RegExp(multiLangKeywordPattern, 'i').test(text)
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
