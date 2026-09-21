/**
 * [INPUT]: 依赖 regexp, strings 标准库
 * [OUTPUT]: 对外提供 OTPResult, ExtractOTP
 * [POS]: internal/mail 的验证码与激活链接嗅探器，供 server/verify_handler 消费；支持全球主流语言 (中英韩日俄西法德意越土阿等) 全语境、标题括号强标注与通用结构通杀提取，严格防守年份与订单账单负向干扰
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"regexp"
	"strings"
)

// OTPResult 包含从邮件中提取的验证信息。
type OTPResult struct {
	Code      string `json:"code,omitempty"`
	MagicLink string `json:"magic_link,omitempty"`
}

const (
	// multiLangKeywordPattern 覆盖中、英、韩、日、俄、西、法、德、葡、意、越、土、阿等全球主要语言的验证码词元
	multiLangKeywordPattern = `(?:` +
		// 中文 (简/繁)
		`验证码|校验码|动态码|确认码|安全码|动态口令|口令|验证|激活|确认|` +
		`驗證碼|校驗碼|動態碼|確認碼|安全碼|動態口令|驗證|確認|` +
		// 英文
		`one-time\s*password|temporary\s*password|verification(?:\s*code)?|security(?:\s*code)?|verify(?:\s*code)?|` +
		`auth\s*code|confirmation(?:\s*code)?|login\s*code|access\s*code|passcode|\bcode\b|\botp\b|\bpin\b|\bpassword\b|` +
		// 韩文
		`인증\s*코드|인증\s*번호|인증번호|임시\s*코드|확인\s*코드|보안\s*코드|패스코드|비밀번호|인증|코드|확인|보안|임시|` +
		// 日文
		`認証コード|確認コード|セキュリティコード|ワンタイムコード|認証|確認|パスコード|セキュリティ|` +
		// 俄文
		`код\s*подтверждения|проверочный\s*код|код\s*безопасности|одноразовый\s*пароль|код\s*доступа|\bкод\b|пароль|` +
		// 西班牙语 / 葡萄牙语
		`código\s*de\s*verificación|código\s*de\s*seguridad|código\s*de\s*confirmación|código\s*de\s*acceso|` +
		`codigo\s*de\s*verificacao|código|codigo|clave|` +
		// 法语
		`code\s*de\s*vérification|code\s*de\s*sécurité|code\s*de\s*confirmation|code\s*d'activation|` +
		// 德语
		`bestätigungscode|bestaetigungscode|sicherheitscode|aktivierungscode|einmalpasswort|bestätigung|` +
		// 意大利语
		`codice\s*di\s*verifica|codice\s*di\s*sicurezza|codice\s*di\s*conferma|codice|` +
		// 越南语
		`mã\s*xác\s*thực|mã\s*xác\s*nhận|mã\s*bảo\s*mật|mã\s*otp|\bmã\b|` +
		// 土耳其语
		`doğrulama\s*kodu|dogrulama\s*kodu|güvenlik\s*kodu|onay\s*kodu|\bkod\b|\bkodu\b|doğrulama|` +
		// 阿拉伯语
		`رمز\s*التحقق|رمز\s*الأمان|رمز\s*التأكيد|\bرمز\b` +
		`)`
)

var (
	// contextCodeRegex 优先匹配带上下文关键词的 4-8 位纯数字验证码 (全球多语言)
	contextCodeRegex = regexp.MustCompile(`(?i)` + multiLangKeywordPattern + `[^\r\n\d]{0,32}?(?:is|为|是|est|es|ist|è|la|является|para|:|：|\s)*[:：\s-]*\b([0-9]{4,8})\b`)

	// contextHyphenCodeRegex 匹配带上下文关键词的连字号/空格验证码 (例: "code: 123-456", "123 456", "인증 987-654")
	contextHyphenCodeRegex = regexp.MustCompile(`(?i)` + multiLangKeywordPattern + `[^\r\n\d]{0,32}?(?:is|为|是|est|es|ist|è|la|является|para|:|：|\s)*[:：\s-]*\b([0-9]{3,4}[-\s][0-9]{3,4})\b`)

	// reverseContextCodeRegex 匹配数字在前、关键词在后的倒装语序 (例: "123456 is your code", "839201 为确认码", "492019 c'est votre code")
	reverseContextCodeRegex = regexp.MustCompile(`(?i)\b([0-9]{4,8})\b[^\r\n\d]{0,24}?(?:is(?:\s+(?:your|the|a|an))?|为(?:您(?:的)?)?|是(?:你(?:的)?)?|为本次|作为|입니다|입력|est|es|ist|è|la|является|para)?[^\r\n\d]{0,24}?` + multiLangKeywordPattern)

	// bracketCodeRegex 匹配标题或正文中由括号、书名号、引号醒目标注的 4-8 位验证码 (例: "[576932]", "【839201】", "(492019)")
	bracketCodeRegex = regexp.MustCompile(`(?:\[|\(|【|「|“|")\s*([0-9]{4,8})\s*(?:\]|\)|】|」|”|")`)

	// standaloneDigitRegex 兜底匹配独立的 4-8 位数字
	standaloneDigitRegex = regexp.MustCompile(`\b([0-9]{4,8})\b`)

	// negativeContextRegex 排除订单号、账单号、快递运单、条形码等假验证码
	negativeContextRegex = regexp.MustCompile(`(?i)(?:order|tracking|invoice|receipt|bill|barcode|unicode|encode|ticket|account\s*number|phone|tel|fax|订单|发票|账单|快递|运单|编号)`)

	// magicLinkRegex 匹配常见的激活/确认链接
	magicLinkRegex = regexp.MustCompile(`https?://[^\s"'<>]+(?:verify|confirm|activate|validation|token=)[^\s"'<>]*`)
)

// ExtractOTP 从邮件标题和正文中提取验证码与激活链接。
func ExtractOTP(subject, body string) *OTPResult {
	text := subject + "\n" + body
	if strings.TrimSpace(text) == "" {
		return nil
	}

	res := &OTPResult{}

	// 1. 优先提取带明确多语言上下文的验证码 (关键词在前 或 数字在前 或 连字号格式)
	if match := contextCodeRegex.FindStringSubmatch(text); len(match) > 1 {
		res.Code = match[1]
	} else if match := reverseContextCodeRegex.FindStringSubmatch(text); len(match) > 1 {
		res.Code = match[1]
	} else if match := contextHyphenCodeRegex.FindStringSubmatch(text); len(match) > 1 {
		res.Code = cleanCode(match[1])
	} else if match := bracketCodeRegex.FindStringSubmatch(subject); len(match) > 1 && !isYear(match[1]) {
		// 标题中的括号优先捕获
		res.Code = match[1]
	}

	// 2. 若未提取到，且标题或正文属于验证类邮件（或标题含 brackets），尝试兜底提取独立数字
	if res.Code == "" && (isVerificationEmail(text) || isSubjectVerification(subject)) {
		res.Code = findBestDigitCode(text)
	}

	// 3. 提取激活链接
	if link := magicLinkRegex.FindString(text); link != "" {
		res.MagicLink = link
	}

	if res.Code == "" && res.MagicLink == "" {
		return nil
	}
	return res
}

func isYear(s string) bool {
	return len(s) == 4 && s >= "2020" && s <= "2035"
}

func isSubjectVerification(subject string) bool {
	if subject == "" {
		return false
	}
	return bracketCodeRegex.MatchString(subject) || isVerificationEmail(subject)
}

func cleanCode(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return strings.TrimSpace(s)
}

// isVerificationEmail 判定邮件是否属于验证类邮件 (全球主流语言通杀)。
func isVerificationEmail(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{
		// 中文 (简/繁)
		"验证码", "校验码", "动态码", "确认码", "安全码", "动态口令", "口令", "验证", "激活", "确认",
		"驗證碼", "校驗碼", "動態碼", "確認碼", "安全碼", "動態口令", "驗證", "確認",
		// 英语
		"code", "verification", "verify", "security", "pin", "otp", "passcode", "password", "auth", "confirm", "confirmation", "login", "access", "temporary", "one-time",
		// 韩语
		"인증", "코드", "확인", "보안", "임시", "인증번호", "임시코드", "비밀번호",
		// 日语
		"認証", "コード", "確認", "セキュリティ", "ワンタイム", "パスコード",
		// 俄语
		"код", "проверочный", "подтверждение", "подтвердить", "пароль", "одноразовый", "авторизация",
		// 西班牙语 / 葡萄牙语
		"código", "codigo", "verificación", "verificacao", "verificar", "seguridad", "seguranca", "clave", "confirmar", "confirmación", "confirmacao",
		// 法语
		"vérification", "sécurité", "confirmer", "confirmation",
		// 德语
		"bestätigung", "bestätigen", "sicherheit", "sicherheitscode", "bestätigungscode", "einmalpasswort", "verifizierung",
		// 意大利语
		"codice", "verifica", "sicurezza", "conferma",
		// 越南语
		"mã", "xác thực", "xác nhận", "bảo mật",
		// 土耳其语
		"doğrulama", "güvenlik", "onay", "şifre",
		// 阿拉伯语
		"رمز", "تحقق", "تأكيد", "أمان",
	}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// findBestDigitCode 提取最可能是验证码的纯数字串 (排除年份与负向业务词)。
func findBestDigitCode(text string) string {
	matches := standaloneDigitRegex.FindAllStringIndex(text, -1)
	var candidates []string
	for _, loc := range matches {
		start, end := loc[0], loc[1]
		m := text[start:end]
		if isYear(m) {
			continue
		}
		// 检查前后 30 字符窗口，排除订单/发票/账单等假验证码干扰
		windowStart := start - 30
		if windowStart < 0 {
			windowStart = 0
		}
		windowEnd := end + 30
		if windowEnd > len(text) {
			windowEnd = len(text)
		}
		contextWindow := text[windowStart:windowEnd]
		if negativeContextRegex.MatchString(contextWindow) {
			continue
		}

		candidates = append(candidates, m)
		// 6 位纯数字在全球 2FA 中占 95% 以上，优先直接命中
		if len(m) == 6 {
			return m
		}
	}
	for _, c := range candidates {
		if len(c) == 4 {
			return c
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}
