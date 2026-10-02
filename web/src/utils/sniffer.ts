/**
 * [INPUT]: 依赖纯文本或 HTML 字符串输入
 * [OUTPUT]: 对外提供 extractVerifyCode, extractMagicLink, extractOTP, extractOTPMemoized, buildSniffContext, stripHtml, toHalfWidth 与 parseSenderInfo 函数及 SenderInfo, OTPResult 类型
 * [POS]: web/src/utils 的文本分析与验证码/链接嗅探工具；完整正文优先于摘要，保留主题边界，支持可见 HTML 文本、实体解码、歧义拒绝与 URL 隔离
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
    .replace(/\u200D|[\u200B\u200C\uFEFF]/g, '')
    .replace(/\u00A0/g, ' ')
    .replace(/\u2011/g, '-')
}

/**
 * 完整正文为权威内容，未加载正文时使用摘要；避免重复和截断摘要污染候选。
 */
export function buildSniffContext(subject?: string, preview?: string, body?: string): string {
  const content = body?.trim() ? body : preview
  const title = toHalfWidth(subject ?? '')
  if (!content) return title
  const text = content.includes('<') && content.includes('>') ? stripHtml(content) : content
  // Internal field delimiter: a real body newline must not be mistaken for
  // a subject boundary, or join a numeric subject with the body.
  return `${title}\u0000${toHalfWidth(text)}`
}

const multiLangKeywordPattern =
  "(?:" +
  "验证码|校验码|动态码|确认码|安全码|动态口令|口令|验证|激活|确认|" +
  "驗證碼|校驗碼|動態碼|確認碼|安全碼|動態口令|驗證|確認|" +
  "\\b(?:one-time\\s*password|temporary\\s*password|verification(?:\\s*code)?|security(?:\\s*code)?|verify(?:\\s*code)?|" +
  "auth\\s*code|confirmation(?:\\s*code)?|login\\s*code|access\\s*code|passcode|\\bcode\\b|\\botp\\b|\\bpin\\b|\\bpassword\\b|" +
  "steam\\s*guard(?:\\s*code)?|2fa(?:\\s*code)?|\\bmfa\\b|two-factor(?:\\s*auth(?:entication)?)?(?:\\s*code)?)\\b|" +
  "인증\\s*코드|인증\\s*번호|인증번호|임시\\s*코드|확인\\s*코드|보안\\s*코드|패스코드|비밀번호|인증|코드|확인|보안|임시|" +
  "認証コード|確認コード|セキュリティコード|ワンタイムコード|認証|確認|パスコード|セキュリティ|" +
  "код\\s*подтверждения|проверочный\\s*код|код\\s*безопасности|одноразовый\\s*пароль|код\\s*доступа|\\bкод\\b|пароль|" +
  "código\\s*de\\s*verificación|código\\s*de\\s*seguridad|código\\s*de\\s*confirmación|código\\s*de\\s*acceso|" +
  "codigo\\s*de\\s*verificacao|código|codigo|clave|" +
  "code\\s*de\\s*vérification|code\\s*de\\s*sécurité|code\\s*de\\s*confirmation|code\\s*d'activation|" +
  "verifizierungscode|bestätigungscode|bestaetigungscode|sicherheitscode|aktivierungscode|einmalpasswort|verificatiecode|beveiligingscode|bestätigung|" +
  "codice\\s*di\\s*verifica|codice\\s*di\\s*sicurezza|codice\\s*di\\s*conferma|codice|" +
  "kod\\s*weryfikacyjny|\\bkod\\b|" +
  "mã\\s*xác\\s*thực|mã\\s*xác\\s*nhận|mã\\s*bảo\\s*mật|mã\\s*otp|\\bmã\\b|" +
  "doğrulama\\s*kodu|dogrulama\\s*kodu|güvenlik\\s*kodu|onay\\s*kodu|\\bkod\\b|\\bkodu\\b|doğrulama|" +
  "รหัส\\s*ยืนยัน|รหัส\\s*ชั่วคราว|รหัส\\s*ความปลอดภัย|รหัส\\s*ผ่าน\\s*ชั่วคราว|รหัส|ยืนยัน|" +
  "kode\\s*verifikasi|kode\\s*keamanan|kode\\s*konfirmasi|\\bkode\\b|" +
  "رمز\\s*التحقق|رمز\\s*الأمان|رمز\\s*التأكيد|\\bرمز\\b|" +
  "\\b(?:chatgpt|openai)\\b(?:\\s*(?:verification|auth|security|login|access)?\\s*(?:code|otp|pin|passcode)|\\s*(?:\\bis\\b|[:：=\\-]))" +
  ")"
const copulaPattern = '(?:\\bis\\b|为|是|\\best\\b|\\bes\\b|\\bist\\b|\\blautet\\b|è|la|является|para|คือ|adalah|[:：=])'
const keywordRegex = new RegExp(multiLangKeywordPattern, 'i')
const forwardContext = new RegExp(`${multiLangKeywordPattern}[^\\r\\n]{0,64}?${copulaPattern}[\\s:：=\\-{"“『「【(<]*$`, 'i')
const directContext = new RegExp(`${multiLangKeywordPattern}[\\s:：=\\-{"“『「【(<]+$`, 'i')
const reverseContext = new RegExp(`^[^\\r\\n\\d]{0,48}?${multiLangKeywordPattern}`, 'i')
const negativePrefix = /(?:\b(?:order|tracking|invoice|receipt|bill|barcode|ticket|account\s*(?:number|no|id)|phone|tel|fax|reference)\b|订单|发票|账单|快递|运单|账号|编号)[^\r\n\d]{0,10}$/i
const negativeSuffix = /^[^\r\n\d]{0,10}(?:\b(?:order|tracking|invoice|receipt|bill|ticket|account\s*(?:number|no|id)|phone|tel)\b|订单|发票|账单|快递|运单|账号|编号)/i
const steamGuardRegex = new RegExp(`(?:steam\\s*guard|guard\\s*code)[^\\r\\n]{0,80}?(?:${copulaPattern}|\\n)\\s*([A-Z0-9]{5})\\b`, 'gi')
const urlRegex = /https?:\/\/[^\s"'<>]+/gi

function normalizedDigitRun(raw: string): string | null {
  const groups = raw.split(/[\s-]+/)
  const code = groups.join('')
  if (code.length < 4 || code.length > 8) return null
  if (groups.length === 1 || groups.every(group => group.length === 1)) return code
  if (groups.length === 2 && groups.every(group => group.length >= 3 && group.length <= 4)) return code
  return null
}

function isWeakNoise(code: string): boolean {
  const num = Number(code)
  return (code.length === 4 && num >= 2020 && num <= 2035) ||
    (code.length === 6 && num >= 202000 && num <= 203599) || /^(\d)\1+$/.test(code)
}

function isAlphanumericOTP(code: string): boolean {
  return /^[A-Z0-9]{5}$/.test(code) && /[A-Z]/.test(code) &&
    (/[0-9]/.test(code) || !/[AEIOU]/.test(code))
}

/** Only a unique strongest candidate is usable; URL digits never participate. */
export function extractOTP(rawText: string): OTPResult | null {
  if (!rawText) return null
  const cleanFields = rawText.split('\u0000').map(field => toHalfWidth(field.includes('<') && field.includes('>') ? stripHtml(field) : field))
  const magicLink = extractMagicLink(cleanFields.join('\n'))
  const fields = cleanFields.map(field => field.replace(urlRegex, ' '))
  const subject = fields[0]
  const text = fields.join('\n')
  let bestRank = 0
  const candidates = new Set<string>()
  function add(code: string, rank: number) {
    if (rank > bestRank) { bestRank = rank; candidates.clear() }
    if (rank === bestRank) candidates.add(code)
  }
  for (const field of fields) {
    for (const match of field.matchAll(/[0-9]+(?:[\s-]+[0-9]+)*/g)) {
      const start = match.index
      const end = start + match[0].length
      if (/[A-Za-z0-9_]/.test(field[start - 1] || '') || /[A-Za-z0-9_]/.test(field[end] || '')) continue
      const code = normalizedDigitRun(match[0])
      if (!code) continue
      const before = Array.from(field.slice(Math.max(0, start - 192), start)).slice(-96).join('')
      const after = Array.from(field.slice(end, end + 128)).slice(0, 64).join('')
      if (code.length === 4 && after.startsWith('年')) continue
      if (negativePrefix.test(before) || negativeSuffix.test(after)) continue
      if (forwardContext.test(before) || directContext.test(before) || reverseContext.test(after)) add(code, 2)
      else if (keywordRegex.test(subject) && !isWeakNoise(code)) add(code, 1)
    }
  }
  for (const match of text.matchAll(steamGuardRegex)) {
    const code = match[1].toUpperCase()
    if (isAlphanumericOTP(code)) add(code, 2)
  }
  if (candidates.size > 1) return null
  const code = candidates.values().next().value
  if (!code && !magicLink) return null
  return { code, magicLink: magicLink ?? undefined }
}

export function extractVerifyCode(text: string): string | null {
  return extractOTP(text)?.code ?? null
}

/** Validate path/query semantics and reject conflicting activation links. */
export function extractMagicLink(text: string): string | null {
  const links = new Set<string>()
  for (const match of text.matchAll(urlRegex)) {
    const raw = match[0].replace(/[.,;!，。；！)]+$/, '')
    try {
      const url = new URL(raw)
      if (!url.hostname || url.username || url.password) continue
      if (/verify|confirm|activate|validation/i.test(decodeURIComponent(url.pathname)) ||
          Array.from(url.searchParams.keys()).some(key => key.toLowerCase() === 'token')) links.add(raw)
    } catch {
      // Malformed URLs are not actionable verification links.
    }
  }
  return links.size === 1 ? links.values().next().value ?? null : null
}

const otpCache = new Map<string, OTPResult | null>()
const MAX_OTP_CACHE_SIZE = 1000

/**
 * 纯函数记忆化：根据输入上下文缓存 OTP 提取结果，避免同一封邮件反复执行多道正则
 */
export function extractOTPMemoized(text: string): OTPResult | null {
  if (!text) return null
  if (text.length > 50000) {
    return extractOTP(text)
  }
  const hit = otpCache.get(text)
  if (hit !== undefined) return hit

  if (otpCache.size >= MAX_OTP_CACHE_SIZE) {
    const keys = Array.from(otpCache.keys()).slice(0, 200)
    for (const k of keys) otpCache.delete(k)
  }

  const result = extractOTP(text)
  otpCache.set(text, result)
  return result
}

/**
 * 剔除 HTML 标签与样式并转换实体，获得纯净文本 (保留 a 标签 href 供链接嗅探)
 */
export function stripHtml(html: string): string {
  if (!html) return ''
  const text = html
    .replace(/<(style|script|head|title|noscript|template)\b[^>]*>[\s\S]*?<\/\1\s*>/gi, '')
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/<a\b[^>]*?\bhref=["']?([^"'\s>]+)["']?[^>]*>([\s\S]*?)<\/a>/gi, '$2 ( $1 )')
    .replace(/<[^>]+>/g, ' ')
  // A detached textarea decodes HTML character references exactly once.
  // Tags were already removed; the result is rendered as React text.
  const decoder = document.createElement('textarea')
  decoder.innerHTML = text
  return decoder.value.replace(/\s+/g, ' ').trim()
}

export interface SenderInfo {
  name: string
  email: string
  initial: string
}

export function parseSenderInfo(raw?: string): SenderInfo {
  const trimmed = (raw || '').trim()
  if (!trimmed) {
    return { name: '未知发件人', email: '', initial: '?' }
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
  }
}
