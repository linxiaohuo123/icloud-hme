/**
 * [INPUT]: 依赖 regexp, strings 标准库
 * [OUTPUT]: 对外提供 OTPResult, ExtractOTP
 * [POS]: internal/mail 的验证码与激活链接嗅探器，供 server/verify_handler 消费；支持正反向语序与修饰定语容差提取，严格防守 \botp\b、\bpin\b 与 \bcode\b 词边界
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

var (
	// contextCodeRegex 优先匹配带上下文关键词的 4-8 位纯数字验证码
	contextCodeRegex = regexp.MustCompile(`(?i)(?:验证码|校验码|动态码|确认码|安全码|动态口令|口令|one-time\s*password|temporary\s*password|verification\s*code|security\s*code|verify\s*code|auth\s*code|passcode|\bcode\b|\botp\b|\bpin\b)[^\r\n\d]{0,12}?(?:is|为|是)?[:：\s-]*\b([0-9]{4,8})\b`)

	// contextHyphenCodeRegex 匹配带上下文关键词的连字号验证码 (例: "code: 123-456", "验证码 839-201", "pin is 123 456")
	contextHyphenCodeRegex = regexp.MustCompile(`(?i)(?:验证码|校验码|动态码|确认码|安全码|动态口令|口令|one-time\s*password|temporary\s*password|verification\s*code|security\s*code|verify\s*code|auth\s*code|passcode|\bcode\b|\botp\b|\bpin\b)[^\r\n\d]{0,12}?(?:is|为|是)?[:：\s-]*\b([0-9]{3,4}[-\s][0-9]{3,4})\b`)

	// reverseContextCodeRegex 匹配数字在前、关键词在后的 4-8 位纯数字验证码 (例: "123456 is your verification code", "839201 为本次确认码", "492019 is your AWS verification code")
	reverseContextCodeRegex = regexp.MustCompile(`(?i)\b([0-9]{4,8})\b[^\r\n\d]{0,20}?(?:is(?:\s+(?:your|the|a|an))?|为(?:您(?:的)?)?|是(?:你(?:的)?)?|为本次|作为)?[^\r\n\d]{0,20}?(?:验证码|校验码|动态码|确认码|安全码|动态口令|口令|one-time\s*password|temporary\s*password|verification\s*code|security\s*code|verify\s*code|auth\s*code|passcode|\bcode\b|\botp\b|\bpin\b)`)

	// standaloneDigitRegex 兜底匹配独立的 4-8 位数字
	standaloneDigitRegex = regexp.MustCompile(`\b([0-9]{4,8})\b`)

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

	// 1. 优先提取带明确上下文的验证码 (关键词在前 或 数字在前 或 连字号格式)
	if match := contextCodeRegex.FindStringSubmatch(text); len(match) > 1 {
		res.Code = match[1]
	} else if match := reverseContextCodeRegex.FindStringSubmatch(text); len(match) > 1 {
		res.Code = match[1]
	} else if match := contextHyphenCodeRegex.FindStringSubmatch(text); len(match) > 1 {
		res.Code = cleanCode(match[1])
	}

	// 2. 若未提取到，且标题或正文包含关键词，尝试兜底提取独立数字
	if res.Code == "" && isVerificationEmail(text) {
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

func cleanCode(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return strings.TrimSpace(s)
}

// isVerificationEmail 判定邮件是否属于验证类邮件。
func isVerificationEmail(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{"验证码", "校验码", "动态码", "确认码", "安全码", "动态口令", "口令", "验证", "code", "verification", "verify", "activate", "激活", "确认", "one-time password", "password"}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// findBestDigitCode 提取最可能是验证码的纯数字串。
func findBestDigitCode(text string) string {
	matches := standaloneDigitRegex.FindAllString(text, -1)
	var candidates []string
	for _, m := range matches {
		// 排除年份 (2020-2035)
		if len(m) == 4 && m >= "2020" && m <= "2035" {
			continue
		}
		candidates = append(candidates, m)
		// 常见 4 或 6 位验证码优先
		if len(m) == 4 || len(m) == 6 {
			return m
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}
