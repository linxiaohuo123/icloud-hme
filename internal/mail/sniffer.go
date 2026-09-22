/**
 * [INPUT]: 依赖 regexp, strings 标准库与 preview.go stripHTML
 * [OUTPUT]: 对外提供 OTPResult, ExtractOTP
 * [POS]: internal/mail 的验证码与激活链接嗅探器，供 server/verify_handler 消费；支持全球主流语言全语境、HTML 盒式空格码、Steam Guard 混合码、标题闭合符号强标注与防负向词穿透
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
	// multiLangKeywordPattern 覆盖全球 15+ 主流语言验证词元 (含 2FA/MFA/Steam Guard)
	multiLangKeywordPattern = `(?:` +
		// 中文 (简/繁)
		`验证码|校验码|动态码|确认码|安全码|动态口令|口令|验证|激活|确认|` +
		`驗證碼|校驗碼|動態碼|確認碼|安全碼|動態口令|驗證|確認|` +
		// 英文与通用缩写
		`one-time\s*password|temporary\s*password|verification(?:\s*code)?|security(?:\s*code)?|verify(?:\s*code)?|` +
		`auth\s*code|confirmation(?:\s*code)?|login\s*code|access\s*code|passcode|\bcode\b|\botp\b|\bpin\b|\bpassword\b|` +
		`steam\s*guard(?:\s*code)?|2fa(?:\s*code)?|\bmfa\b|two-factor(?:\s*auth(?:entication)?)?(?:\s*code)?|` +
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
	// copulaPattern 包含各类语言的主谓系动词与强分隔符
	copulaPattern = `(?:is|为|是|est|es|ist|lautet|è|la|является|para|[:：=])`

	// contextCopulaCodeRegex 匹配带修饰状语/介词短语的系词验证码
	// 例: "Your verification code for order #839201 is 492019", "您的订单 123456 验证码是 492019"
	contextCopulaCodeRegex = regexp.MustCompile(`(?i)(` + multiLangKeywordPattern + `)[^\r\n]{0,64}?(?:` + copulaPattern + `)\s*[:：\s-]*\b([0-9]{4,8})\b`)

	// contextDirectCodeRegex 匹配紧随关键词的验证码 (中间无系词，例: "security code 9283", "code: 482019")
	contextDirectCodeRegex = regexp.MustCompile(`(?i)(` + multiLangKeywordPattern + `)\s*[:：\s-]+\b([0-9]{4,8})\b`)

	// contextSpacedCodeRegex 匹配 HTML 盒式布局下按单个空格拆分的数字 (例: "5 7 6 9 3 2")
	contextSpacedCodeRegex = regexp.MustCompile(`(?i)(` + multiLangKeywordPattern + `)[^\r\n]{0,64}?(?:` + copulaPattern + `|\s)[:：\s-]*\b([0-9](?:\s+[0-9]){3,7})\b`)

	// contextHyphenCodeRegex 匹配带关键词的连字号/双段验证码 (例: "code: 123-456", "123 456")
	contextHyphenCodeRegex = regexp.MustCompile(`(?i)(` + multiLangKeywordPattern + `)[^\r\n]{0,64}?(?:` + copulaPattern + `|\s)[:：\s-]*\b([0-9]{3,4}[-\s][0-9]{3,4})\b`)

	// reverseContextCodeRegex 匹配数字在前、关键词在后的倒装语序 (例: "123456 is your code", "839201 为确认码")
	reverseContextCodeRegex = regexp.MustCompile(`(?i)\b([0-9]{4,8})\b[^\r\n\d]{0,24}?(?:is(?:\s+(?:your|the|a|an))?|为(?:您(?:的)?)?|是(?:你(?:的)?)?|为本次|作为|입니다|입력|est|es|ist|è|la|является|para)?[^\r\n\d]{0,24}?` + multiLangKeywordPattern)

	// steamGuardRegex 匹配 Steam Guard 5 位混合码，严格锚定冒号、换行或系词，杜绝 English 动词/介词干扰
	steamGuardRegex = regexp.MustCompile(`(?i)(?:steam\s*guard|guard\s*code)[^\r\n]{0,50}?(?:` + copulaPattern + `|\n)\s*([A-Z0-9]{5})\b`)

	// bracketCodeRegex 匹配闭合符号强标注的验证码 (支持 [ ] ( ) 【 】 「 」 『 』 { } < > « » “ ”)
	bracketCodeRegex = regexp.MustCompile(`(?:\[|\(|【|「|“|"|\{|<|«|『)\s*([0-9]{4,8})\s*(?:\]|\)|】|」|”|"|\}|>|»|』)`)

	// spacedDigitsRegex 兜底匹配盒式单个空格分隔的数字串
	spacedDigitsRegex = regexp.MustCompile(`\b([0-9](?:\s+[0-9]){3,7})\b`)

	// standaloneDigitRegex 兜底匹配独立的 4-8 位纯数字
	standaloneDigitRegex = regexp.MustCompile(`\b([0-9]{4,8})\b`)

	// negativePrefixRegex 匹配紧贴在数字前的负向业务词 (限制在同单行内)
	negativePrefixRegex = regexp.MustCompile(`(?i)(?:order|tracking|invoice|receipt|bill|barcode|ticket|account\s*(?:number|no|id)|phone|tel|fax|订单|发票|账单|快递|运单|账号|编号)[^\r\n\d]{0,10}#?\s*$`)

	// negativeSuffixRegex 匹配紧贴在数字后的负向业务词 (限制在同单行内)
	negativeSuffixRegex = regexp.MustCompile(`(?i)^[^\r\n\d]{0,10}(?:order|tracking|invoice|receipt|bill|ticket|account\s*(?:number|no|id)|phone|tel|订单|发票|账单|快递|运单|账号|编号)`)

	// magicLinkRegex 匹配常见的激活/确认链接
	magicLinkRegex = regexp.MustCompile(`https?://[^\s"'<>]+(?:verify|confirm|activate|validation|token=)[^\s"'<>]*`)
)

// ExtractOTP 从邮件标题和正文中提取验证码与激活链接 (全链路清洗 + 盒式重组 + 防穿透)。
func ExtractOTP(subject, body string) *OTPResult {
	cleanSub := toHalfWidth(subject)
	cleanBdy := cleanEmailText(body)
	text := cleanSub + "\n" + cleanBdy
	if strings.TrimSpace(text) == "" {
		return nil
	}

	res := &OTPResult{}

	// 1. 标题中的闭合符号强特征优先捕获 (例: [849201], 『576932』, {492019})
	if match := bracketCodeRegex.FindStringSubmatchIndex(cleanSub); len(match) >= 4 {
		c := cleanSub[match[2]:match[3]]
		if !isYear(c) && !isDummyCode(c) {
			res.Code = c
		}
	}

	// 2. 正向多语言上下文匹配 (支持连续数字、盒式空格码、连字号，阻断中介负向词穿透)
	if res.Code == "" {
		res.Code = extractContextCode(text)
	}

	// 3. 反向倒装语序匹配 (例: "123456 is your code", "839201 为确认码")
	if res.Code == "" {
		if match := reverseContextCodeRegex.FindStringSubmatchIndex(text); len(match) >= 4 {
			c := text[match[2]:match[3]]
			if !isYear(c) && !isDummyCode(c) && !isNegativeContext(text, match[2], match[3]) {
				res.Code = c
			}
		}
	}

	// 4. Steam Guard 5 位大写字母数字混合码
	if res.Code == "" {
		if match := steamGuardRegex.FindStringSubmatch(text); len(match) > 1 {
			c := strings.ToUpper(strings.TrimSpace(match[1]))
			if isAlphanumericOTP(c) && !isYear(c) && !isDummyCode(c) {
				res.Code = c
			}
		}
	}

	// 5. 兜底策略：在确认为验证类邮件时，提取最佳独立数字 (含盒式空格码)
	if res.Code == "" && (isVerificationEmail(text) || isSubjectVerification(cleanSub)) {
		res.Code = findBestDigitCode(text)
	}

	// 6. 激活链接嗅探
	if link := magicLinkRegex.FindString(text); link != "" {
		res.MagicLink = link
	}

	if res.Code == "" && res.MagicLink == "" {
		return nil
	}
	return res
}

// extractContextCode 正向扫描上下文，严格阻断 "verification code for order #839201 is 492019" 类型的穿透干扰
func extractContextCode(text string) string {
	for _, re := range []*regexp.Regexp{contextCopulaCodeRegex, contextDirectCodeRegex, contextSpacedCodeRegex, contextHyphenCodeRegex} {
		matches := re.FindAllStringSubmatchIndex(text, -1)
		for _, m := range matches {
			if len(m) < 6 {
				continue
			}
			codeStart, codeEnd := m[4], m[5]
			rawCode := text[codeStart:codeEnd]
			code := cleanCode(rawCode)
			if isYear(code) || isDummyCode(code) {
				continue
			}
			if isNegativeContext(text, codeStart, codeEnd) {
				continue
			}
			return code
		}
	}
	return ""
}

// cleanEmailText 剥除 HTML、CSS 与标签，保留纯净文本
func cleanEmailText(s string) string {
	if strings.Contains(s, "<") && strings.Contains(s, ">") {
		s = stripHTML(s)
	}
	return toHalfWidth(s)
}

// toHalfWidth 全角数字转半角，剔除零宽字符与特殊空格
func toHalfWidth(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '０' && r <= '９' {
			b.WriteRune(r - '０' + '0')
		} else if r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff' {
			continue
		} else if r == '\u00a0' {
			b.WriteByte(' ')
		} else if r == '\u2011' {
			b.WriteByte('-')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isYear(s string) bool {
	return (len(s) == 4 && s >= "2020" && s <= "2035") ||
		(len(s) == 6 && s >= "202000" && s <= "203599")
}

func isDummyCode(s string) bool {
	if len(s) == 0 {
		return true
	}
	allSame := true
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			allSame = false
			break
		}
	}
	return allSame
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

// isVerificationEmail 判定邮件是否属于验证类邮件 (全球主流语言通杀)
func isVerificationEmail(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{
		// 中文 (简/繁)
		"验证码", "校验码", "动态码", "确认码", "安全码", "动态口令", "口令", "验证", "激活", "确认",
		"驗證碼", "校驗碼", "動態碼", "確認碼", "安全碼", "動態口令", "驗證", "確認",
		// 英语
		"code", "verification", "verify", "security", "pin", "otp", "passcode", "password", "auth", "confirm", "confirmation", "login", "access", "temporary", "one-time", "steam guard", "2fa", "mfa",
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

// isNegativeContext 判定数字前后是否有订单/账单/发票等负向修饰词紧密绑定 (严格限定在单行内)
func isNegativeContext(text string, start, end int) bool {
	pStart := start - 15
	if pStart < 0 {
		pStart = 0
	}
	if lineStart := strings.LastIndex(text[:start], "\n"); lineStart != -1 && lineStart >= pStart {
		pStart = lineStart + 1
	}
	if negativePrefixRegex.MatchString(text[pStart:start]) {
		return true
	}

	sEnd := end + 15
	if sEnd > len(text) {
		sEnd = len(text)
	}
	if lineEnd := strings.Index(text[end:], "\n"); lineEnd != -1 && end+lineEnd < sEnd {
		sEnd = end + lineEnd
	}
	if negativeSuffixRegex.MatchString(text[end:sEnd]) {
		return true
	}
	return false
}

// isAlphanumericOTP 判定是否为合法 5 位混合码 (如 Steam Guard)，杜绝普通英文单词 (如 allow, enter)
func isAlphanumericOTP(s string) bool {
	if len(s) != 5 {
		return false
	}
	hasLetter, hasDigit := false, false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
		} else if r >= '0' && r <= '9' {
			hasDigit = true
		} else {
			return false
		}
	}
	if !hasLetter {
		return false
	}
	if !hasDigit {
		// 纯字母：Steam Guard 不包含元音 (A, E, I, O, U) 以免产生不雅单词
		for _, ch := range strings.ToUpper(s) {
			if ch == 'A' || ch == 'E' || ch == 'I' || ch == 'O' || ch == 'U' {
				return false
			}
		}
	}
	return true
}

// findBestDigitCode 提取最可能是验证码的纯数字串 (排除年份、虚拟占位符与负向业务词)
func findBestDigitCode(text string) string {
	// 优先检查盒式空格拆分码 (例: "5 7 6 9 3 2")
	for _, m := range spacedDigitsRegex.FindAllStringSubmatchIndex(text, -1) {
		if len(m) >= 4 {
			c := cleanCode(text[m[2]:m[3]])
			if len(c) >= 4 && len(c) <= 8 && !isYear(c) && !isDummyCode(c) && !isNegativeContext(text, m[2], m[3]) {
				return c
			}
		}
	}

	matches := standaloneDigitRegex.FindAllStringIndex(text, -1)
	var candidates []string
	for _, loc := range matches {
		start, end := loc[0], loc[1]
		m := text[start:end]
		if isYear(m) || isDummyCode(m) || isNegativeContext(text, start, end) {
			continue
		}
		candidates = append(candidates, m)
		// 6 位纯数字优先直接命中
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
