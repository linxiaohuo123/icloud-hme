/**
 * [INPUT]: 依赖 net/url, regexp, strings, unicode, unicode/utf8 标准库与 preview.go stripHTML
 * [OUTPUT]: 对外提供 OTPResult, ExtractOTP 及邮件 ExtractOTP 方法，独立仲裁 MIME 内容字段
 * [POS]: internal/mail 的验证码与激活链接嗅探器，供 server/verify_handler 消费；完整候选提取、上下文筛选、歧义拒绝与独立 URL 解析
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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
		`\b(?:one-time\s*password|temporary\s*password|verification(?:\s*code)?|security(?:\s*code)?|verify(?:\s*code)?|` +
		`auth\s*code|confirmation(?:\s*code)?|login\s*code|access\s*code|passcode|\bcode\b|\botp\b|\bpin\b|\bpassword\b|` +
		`steam\s*guard(?:\s*code)?|2fa(?:\s*code)?|\bmfa\b|two-factor(?:\s*auth(?:entication)?)?(?:\s*code)?)\b|` +
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
		// 德语 / 荷兰语
		`verifizierungscode|bestätigungscode|bestaetigungscode|sicherheitscode|aktivierungscode|einmalpasswort|verificatiecode|beveiligingscode|bestätigung|` +
		// 意大利语
		`codice\s*di\s*verifica|codice\s*di\s*sicurezza|codice\s*di\s*conferma|codice|` +
		// 波兰语
		`kod\s*weryfikacyjny|\bkod\b|` +
		// 越南语
		`mã\s*xác\s*thực|mã\s*xác\s*nhận|mã\s*bảo\s*mật|mã\s*otp|\bmã\b|` +
		// 土耳其语
		`doğrulama\s*kodu|dogrulama\s*kodu|güvenlik\s*kodu|onay\s*kodu|\bkod\b|\bkodu\b|doğrulama|` +
		// 泰文 (Thai)
		`รหัส\s*ยืนยัน|รหัส\s*ชั่วคราว|รหัส\s*ความปลอดภัย|รหัส\s*ผ่าน\s*ชั่วคราว|รหัส|ยืนยัน|` +
		// 印尼语 / 马来语 (Indonesian / Malay)
		`kode\s*verifikasi|kode\s*keamanan|kode\s*konfirmasi|\bkode\b|` +
		// 阿拉伯语
		`رمز\s*التحقق|رمز\s*الأمان|رمز\s*التأكيد|\bرمز\b|` +
		// 核心服务与鉴权平台 (ChatGPT / OpenAI)
		`\b(?:chatgpt|openai)\b(?:\s*(?:verification|auth|security|login|access)?\s*(?:code|otp|pin|passcode)|\s*(?:\bis\b|[:：=\-]))` +
		`)`
)

var (
	copulaPattern      = `(?:\bis\b|为|是|\best\b|\bes\b|\bist\b|\blautet\b|è|la|является|para|คือ|adalah|[:：=])`
	keywordRegex       = regexp.MustCompile(`(?i)` + multiLangKeywordPattern)
	forwardCodeContext = regexp.MustCompile(`(?i)` + multiLangKeywordPattern + `[^\r\n]{0,64}?` + copulaPattern + `[\s:：=\-{"“『「【(<]*$`)
	directCodeContext  = regexp.MustCompile(`(?i)` + multiLangKeywordPattern + `[\s:：=\-{"“『「【(<]+$`)
	reverseCodeContext = regexp.MustCompile(`(?i)^[^\r\n\d]{0,48}?` + multiLangKeywordPattern)
	// Consume the whole numeric run first, including overlong/invalid groups.
	digitRunRegex       = regexp.MustCompile(`[0-9]+(?:[\s-]+[0-9]+)*`)
	negativePrefixRegex = regexp.MustCompile(`(?i)(?:\b(?:order|tracking|invoice|receipt|bill|barcode|ticket|account\s*(?:number|no|id)|phone|tel|fax|reference)\b|订单|发票|账单|快递|运单|账号|编号)[^\r\n\d]{0,10}$`)
	negativeSuffixRegex = regexp.MustCompile(`(?i)^[^\r\n\d]{0,10}(?:\b(?:order|tracking|invoice|receipt|bill|ticket|account\s*(?:number|no|id)|phone|tel)\b|订单|发票|账单|快递|运单|账号|编号)`)
	steamGuardRegex     = regexp.MustCompile(`(?i)(?:steam\s*guard|guard\s*code)[^\r\n]{0,80}?(?:` + copulaPattern + `|\n)\s*([A-Z0-9]{5})\b`)
	urlRegex            = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
)

// ExtractOTP selects a unique strongest candidate. Conflicting candidates never
// complete a verification request, even if the message also contains a link.
func ExtractOTP(subject, body string) *OTPResult {
	return extractOTPFields(subject, []string{body})
}

// ExtractOTP preserves decoded MIME boundaries while keeping previews suitable for display.
func (m Message) ExtractOTP() *OTPResult {
	if len(m.otpBodies) > 0 {
		return extractOTPFields(m.Subject, m.otpBodies)
	}
	body := m.Preview
	if body == "" {
		body = m.Body
	}
	return ExtractOTP(m.Subject, body)
}

func (m FullMessage) ExtractOTP() *OTPResult {
	base := m.Message
	base.Body = m.Body
	return base.ExtractOTP()
}

func extractOTPFields(subject string, bodies []string) *OTPResult {
	subject = toHalfWidth(subject)
	fields := make([]string, 1, len(bodies)+1)
	fields[0] = subject
	for _, body := range bodies {
		fields = append(fields, cleanEmailText(body))
	}
	link := extractMagicLink(strings.Join(fields, "\n"))
	for i := range fields {
		fields[i] = urlRegex.ReplaceAllString(fields[i], " ")
	}
	subject = fields[0]
	text := strings.Join(fields, "\n")
	bestRank := 0
	candidates := map[string]bool{}
	add := func(code string, rank int) {
		if rank > bestRank {
			bestRank = rank
			clear(candidates)
		}
		if rank == bestRank {
			candidates[code] = true
		}
	}
	for _, field := range fields {
		for _, loc := range digitRunRegex.FindAllStringIndex(field, -1) {
			start, end := loc[0], loc[1]
			if !numericBoundary(field, start, end) {
				continue
			}
			code := normalizedDigitRun(field[start:end])
			if code == "" {
				continue
			}
			before, after := contextAround(field, start, end)
			if len(code) == 4 && strings.HasPrefix(after, "年") {
				continue
			}
			if negativePrefixRegex.MatchString(before) || negativeSuffixRegex.MatchString(after) {
				continue
			}
			if forwardCodeContext.MatchString(before) || directCodeContext.MatchString(before) || reverseCodeContext.MatchString(after) {
				add(code, 2)
			} else if keywordRegex.MatchString(subject) && !isYear(code) && !isDummyCode(code) {
				add(code, 1)
			}
		}
	}
	for _, m := range steamGuardRegex.FindAllStringSubmatch(text, -1) {
		code := strings.ToUpper(m[1])
		if isAlphanumericOTP(code) {
			add(code, 2)
		}
	}
	if len(candidates) > 1 {
		return nil
	}
	result := &OTPResult{MagicLink: link}
	for code := range candidates {
		result.Code = code
	}
	if result.Code == "" && result.MagicLink == "" {
		return nil
	}
	return result
}

func normalizedDigitRun(raw string) string {
	groups := strings.FieldsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || r == '-' })
	code := strings.Join(groups, "")
	if len(code) < 4 || len(code) > 8 {
		return ""
	}
	if len(groups) == 1 {
		return code
	}
	if len(groups) == 2 && len(groups[0]) >= 3 && len(groups[0]) <= 4 && len(groups[1]) >= 3 && len(groups[1]) <= 4 {
		return code
	}
	for _, group := range groups {
		if len(group) != 1 {
			return ""
		}
	}
	return code
}

func numericBoundary(text string, start, end int) bool {
	// ASCII letters/digits/underscore indicate an identifier rather than an OTP.
	word := func(r rune) bool {
		return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_'
	}
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if word(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if word(r) {
			return false
		}
	}
	return true
}

func contextAround(text string, start, end int) (string, string) {
	left, right := start, end
	for n := 0; n < 96 && left > 0; n++ {
		_, size := utf8.DecodeLastRuneInString(text[:left])
		left -= size
	}
	for n := 0; n < 64 && right < len(text); n++ {
		_, size := utf8.DecodeRuneInString(text[right:])
		right += size
	}
	return text[left:start], text[end:right]
}

func extractMagicLink(text string) string {
	links := map[string]bool{}
	for _, raw := range urlRegex.FindAllString(text, -1) {
		raw = strings.TrimRight(raw, ".,;!，。；！)")
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil {
			continue
		}
		path := strings.ToLower(u.Path)
		matched := strings.Contains(path, "verify") || strings.Contains(path, "confirm") || strings.Contains(path, "activate") || strings.Contains(path, "validation")
		for key := range u.Query() {
			if strings.EqualFold(key, "token") {
				matched = true
			}
		}
		if matched {
			links[raw] = true
		}
	}
	if len(links) != 1 {
		return ""
	}
	for link := range links {
		return link
	}
	return ""
}

func cleanEmailText(s string) string {
	if strings.Contains(s, "<") && strings.Contains(s, ">") {
		s = stripHTML(s)
	}
	return toHalfWidth(s)
}

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
