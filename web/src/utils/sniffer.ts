/**
 * [INPUT]: 依赖纯文本或 HTML 字符串输入
 * [OUTPUT]: 对外提供 extractVerifyCode, extractMagicLink, extractOTP, buildSniffContext, stripHtml, toHalfWidth 与 parseSenderInfo 函数及 SenderInfo, OTPResult 类型
 * [POS]: web/src/utils 的文本分析与验证码/链接嗅探工具；与 internal/mail/sniffer.go 深度对齐，支持全球多语言、HTML 盒式空格码、Steam Guard 混合码与防穿透
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

export interface OTPResult {
  code?: string
  magicLink?: string
}

/**
 * 全角数字转半角，剔除零宽字符与特殊空格
 */
export function toHalfWidth(s: string): string {
  if (!s) return ''
  return s
    .replace(/[\uFF10-\uFF19]/g, (ch) => String.fromCharCode(ch.charCodeAt(0) - 0xfee0))
    .replace(/[\u200B\u200C\u200D\uFEFF]/g, '')
    .replace(/\u00A0/g, ' ')
    .replace(/\u2011/g, '-')
}

/**
 * 辅助函数：合并邮件主题、正文摘要与正文内容构建完整嗅探上下文，消灭短路漏检
 */
export function buildSniffContext(subject?: string, preview?: string, body?: string): string {
  const parts: string[] = []
  if (subject) parts.push(toHalfWidth(subject))
  if (preview) {
    const p = preview.includes('<') && preview.includes('>') ? stripHtml(preview) : preview
    parts.push(toHalfWidth(p))
  }
  if (body && body !== preview) {
    const b = body.includes('<') && body.includes('>') ? stripHtml(body) : body
    parts.push(toHalfWidth(b))
  }
  return parts.filter(Boolean).join('\n')
}

const multiLangKeywordPattern =
  '(?:' +
  // 中文 (简/繁)
  '验证码|校验码|动态码|确认码|安全码|动态口令|口令|验证|激活|确认|' +
  '驗證碼|校驗碼|動態碼|確認碼|安全碼|動態口令|驗證|確認|' +
  // 英文
  'one-time\\s*password|temporary\\s*password|verification(?:\\s*code)?|security(?:\\s*code)?|verify(?:\\s*code)?|' +
  'auth\\s*code|confirmation(?:\\s*code)?|login\\s*code|access\\s*code|passcode|\\bcode\\b|\\botp\\b|\\bpin\\b|\\bpassword\\b|' +
  'steam\\s*guard(?:\\s*code)?|2fa(?:\\s*code)?|\\bmfa\\b|two-factor(?:\\s*auth(?:entication)?)?(?:\\s*code)?|' +
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

const copulaPattern = '(?:is|为|是|est|es|ist|lautet|è|la|является|para|[:：=])'

const bracketCodeRegex = /(?:\[|\(|【|「|“|"|\{|<|«|『)\s*([0-9]{4,8})\s*(?:\]|\)|】|」|”|"|\}|>|»|』)/

const negativePrefixRegex =
  /(?:order|tracking|invoice|receipt|bill|barcode|ticket|account\s*(?:number|no|id)|phone|tel|fax|订单|发票|账单|快递|运单|账号|编号)[^\r\n\d]{0,10}#?\s*$/i

const negativeSuffixRegex =
  /^[^\r\n\d]{0,10}(?:order|tracking|invoice|receipt|bill|ticket|account\s*(?:number|no|id)|phone|tel|订单|发票|账单|快递|运单|账号|编号)/i

const magicLinkRegex =
  /https?:\/\/[^\s"'<>]+(?:verify|confirm|activate|validation|token=)[^\s"'<>]*/i

function isNegativeContext(text: string, start: number, end: number): boolean {
  let pStart = Math.max(0, start - 15)
  const lineStart = text.lastIndexOf('\n', start)
  if (lineStart !== -1 && lineStart >= pStart) {
    pStart = lineStart + 1
  }
  if (negativePrefixRegex.test(text.slice(pStart, start))) {
    return true
  }

  let sEnd = Math.min(text.length, end + 15)
  const lineEnd = text.indexOf('\n', end)
  if (lineEnd !== -1 && end + lineEnd < sEnd) {
    sEnd = end + lineEnd
  }
  if (negativeSuffixRegex.test(text.slice(end, sEnd))) {
    return true
  }
  return false
}

function isYear(s: string): boolean {
  const num = Number(s)
  return (
    (s.length === 4 && num >= 2020 && num <= 2035) ||
    (s.length === 6 && num >= 202000 && num <= 203599)
  )
}

function isDummyCode(s: string): boolean {
  if (!s) return true
  for (let i = 1; i < s.length; i++) {
    if (s[i] !== s[0]) return false
  }
  return true
}

function isAlphanumericOTP(s: string): boolean {
  if (s.length !== 5) return false
  let hasLetter = false
  let hasDigit = false
  for (let i = 0; i < s.length; i++) {
    const ch = s[i]
    if ((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')) {
      hasLetter = true
    } else if (ch >= '0' && ch <= '9') {
      hasDigit = true
    } else {
      return false
    }
  }
  if (!hasLetter) return false
  if (!hasDigit) {
    const upper = s.toUpperCase()
    for (let i = 0; i < upper.length; i++) {
      if ('AEIOU'.includes(upper[i])) return false
    }
  }
  return true
}

/**
 * 从文本中提取 4-8 位验证码
 * 覆盖正向系词、紧密前缀、盒式空格、倒装句式与 Steam Guard
 */
export function extractVerifyCode(rawText: string): string | null {
  if (!rawText) return null
  const text = toHalfWidth(rawText)

  // 1. 闭合符号强标注 (例: "[849201]", "『576932』", "{492019}")
  const bracketMatch = text.match(bracketCodeRegex)
  if (bracketMatch?.[1] && !isYear(bracketMatch[1]) && !isDummyCode(bracketMatch[1])) {
    return bracketMatch[1]
  }

  // 2. 正向系词/冒号匹配 (防穿透，支持 "for order #839201 is 492019")
  const forwardPatterns = [
    new RegExp(
      `${multiLangKeywordPattern}[^\\r\\n]{0,64}?${copulaPattern}\\s*[:：\\s-]*\\b([0-9]{4,8})\\b`,
      'i',
    ),
    new RegExp(`${multiLangKeywordPattern}\\s*[:：\\s-]+\\b([0-9]{4,8})\\b`, 'i'),
    new RegExp(
      `${multiLangKeywordPattern}[^\\r\\n]{0,64}?(?:${copulaPattern}|\\s)[:：\\s-]*\\b([0-9](?:\\s+[0-9]){3,7})\\b`,
      'i',
    ),
    new RegExp(
      `${multiLangKeywordPattern}[^\\r\\n]{0,64}?(?:${copulaPattern}|\\s)[:：\\s-]*\\b([0-9]{3,4}[-\\s][0-9]{3,4})\\b`,
      'i',
    ),
  ]

  for (const pattern of forwardPatterns) {
    const match = text.match(pattern)
    if (match?.[1] && match.index !== undefined) {
      const code = match[1].replace(/[-\s]/g, '')
      if (isYear(code) || isDummyCode(code)) continue
      const codeStart = match.index + (match[0].length - match[1].length)
      const codeEnd = codeStart + match[1].length
      if (isNegativeContext(text, codeStart, codeEnd)) continue
      return code
    }
  }

  // 3. 反向倒装语序匹配 (例: "123456 is your code", "839201 为确认码")
  const reversePattern = new RegExp(
    `\\b([0-9]{4,8})\\b[^\\r\\n\\d]{0,24}?(?:is(?:\\s+(?:your|the|a|an))?|为(?:您(?:的)?)?|是(?:你(?:的)?)?|为本次|作为|입니다|입력|est|es|ist|è|la|является|para)?[^\\r\\n\\d]{0,24}?${multiLangKeywordPattern}`,
    'i',
  )
  const revMatch = text.match(reversePattern)
  if (revMatch?.[1] && revMatch.index !== undefined) {
    const code = revMatch[1]
    if (!isYear(code) && !isDummyCode(code)) {
      const start = revMatch.index
      const end = start + code.length
      if (!isNegativeContext(text, start, end)) {
        return code
      }
    }
  }

  // 4. Steam Guard 5 位大写字母数字混合码
  const steamGuardPattern = new RegExp(
    `(?:steam\\s*guard|guard\\s*code)[^\\r\\n]{0,50}?(?:${copulaPattern}|\\n)\\s*([A-Z0-9]{5})\\b`,
    'i',
  )
  const sgMatch = text.match(steamGuardPattern)
  if (sgMatch?.[1]) {
    const code = sgMatch[1].toUpperCase().trim()
    if (isAlphanumericOTP(code) && !isYear(code) && !isDummyCode(code)) {
      return code
    }
  }

  // 5. 兜底策略：在确认为验证类邮件时，提取最佳独立数字
  if (hasVerifyKeyword(text)) {
    // 优先检查盒式空格码 (全局遍历所有候选)
    for (const spacedMatch of text.matchAll(/\b([0-9](?:\s+[0-9]){3,7})\b/g)) {
      if (spacedMatch[1] && spacedMatch.index !== undefined) {
        const code = spacedMatch[1].replace(/\s+/g, '')
        if (
          code.length >= 4 &&
          code.length <= 8 &&
          !isYear(code) &&
          !isDummyCode(code) &&
          !isNegativeContext(text, spacedMatch.index, spacedMatch.index + spacedMatch[1].length)
        ) {
          return code
        }
      }
    }

    const sixDigitMatches = text.matchAll(/\b([0-9]{6})\b/g)
    for (const m of sixDigitMatches) {
      const val = m[1]
      if (isYear(val) || isDummyCode(val)) continue
      const idx = m.index ?? 0
      if (isNegativeContext(text, idx, idx + val.length)) continue
      return val
    }

    const fourDigitMatches = text.matchAll(/\b([0-9]{4})\b/g)
    for (const m of fourDigitMatches) {
      const val = m[1]
      if (isYear(val) || isDummyCode(val)) continue
      const idx = m.index ?? 0
      if (isNegativeContext(text, idx, idx + val.length)) continue
      return val
    }
  }

  return null
}

/**
 * 从文本中提取激活/验证链接
 */
export function extractMagicLink(text: string): string | null {
  if (!text) return null
  const m = text.match(magicLinkRegex)
  return m ? m[0] : null
}

/**
 * 综合提取验证码与激活链接
 */
export function extractOTP(text: string): OTPResult | null {
  const code = extractVerifyCode(text)
  const magicLink = extractMagicLink(text)
  if (!code && !magicLink) return null
  return { code: code ?? undefined, magicLink: magicLink ?? undefined }
}

function hasVerifyKeyword(text: string): boolean {
  return new RegExp(multiLangKeywordPattern, 'i').test(text)
}

/**
 * 剔除 HTML 标签与样式并转换实体，获得纯净文本 (保留 a 标签 href 供链接嗅探)
 */
export function stripHtml(html: string): string {
  if (!html) return ''
  return html
    .replace(/<style[^>]*>[\s\S]*?<\/style>/gi, '')
    .replace(/<script[^>]*>[\s\S]*?<\/script>/gi, '')
    .replace(/<a\b[^>]*?\bhref=["']?([^"'\s>]+)["']?[^>]*>([\s\S]*?)<\/a>/gi, '$2 ( $1 )')
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
