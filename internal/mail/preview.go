/**
 * [INPUT]: 依赖 html, regexp, strings
 * [OUTPUT]: 对外提供 sanitizePreview, sanitizePlainPreview 等邮件内容清洗与 CSS 剥离能力
 * [POS]: internal/mail 的正文解析与摘要脱敏工具
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	stdhtml "html"
	"regexp"
	"strings"
)

var (
	htmlTagRE        = regexp.MustCompile(`(?s)<[^>]+>`)
	htmlNoiseBlockRE = regexp.MustCompile(`(?is)<(style|script|head|title|noscript)\b[^>]*>.*?</(style|script|head|title|noscript)\s*>`)
	htmlCommentRE    = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlBreakRE      = regexp.MustCompile(`(?i)<br\s*/?>`)
	htmlBlockEndRE   = regexp.MustCompile(`(?i)</(p|div|tr|h[1-6])\s*>`)
	htmlListItemRE   = regexp.MustCompile(`(?i)<li\b[^>]*>`)

	cssSignalRE      = regexp.MustCompile(`(?i)(@font-face|@media|@import|@supports|@keyframes|(^|[\s;{])(-webkit-|-moz-|-ms-|mso-)[\w-]*|(^|[\s;{])(font-family|text-size-adjust|border-collapse|mso-table-[\w-]+)\s*:)`)
	cssDeclarationRE = regexp.MustCompile(`(?i)(^|[;{])\s*[-a-z_][\w-]*\s*:\s*[^;{}]+`)
)

// sanitizePreview converts an email preview to readable text and removes
// presentation-only HTML/CSS that mail clients commonly include.
func sanitizePreview(raw string) string {
	text := stripHTML(raw)
	return sanitizePlainPreview(text)
}

func sanitizePlainPreview(raw string) string {
	text := normalizePreview(raw)
	if text == "" || !looksLikeCSS(text) {
		return text
	}

	// Some upstream APIs return the contents of a <style> block without the
	// surrounding tags. Remove balanced CSS rules while preserving any text
	// that follows the stylesheet.
	text = normalizePreview(stripCSSRules(text))
	if text == "" || looksLikeCSS(text) {
		return ""
	}
	return text
}

// stripHTML removes tags and invisible document sections while keeping common
// line breaks so the preview remains readable.
func stripHTML(raw string) string {
	raw = htmlNoiseBlockRE.ReplaceAllString(raw, "")
	raw = htmlCommentRE.ReplaceAllString(raw, "")
	raw = htmlBreakRE.ReplaceAllString(raw, "\n")
	raw = htmlBlockEndRE.ReplaceAllString(raw, "\n")
	raw = htmlListItemRE.ReplaceAllString(raw, "\n- ")
	raw = htmlTagRE.ReplaceAllString(raw, "")
	return normalizePreview(stdhtml.UnescapeString(raw))
}

func normalizePreview(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func looksLikeCSS(text string) bool {
	braces := strings.Count(text, "{")
	semicolons := strings.Count(text, ";")
	signals := len(cssSignalRE.FindAllStringIndex(text, -1))
	declarations := len(cssDeclarationRE.FindAllStringIndex(text, -1))
	trimmed := strings.TrimSpace(text)
	selectorStart := strings.HasPrefix(trimmed, ".") ||
		strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "@")

	if braces == 0 {
		return signals >= 2 && semicolons >= 3
	}
	if selectorStart && declarations >= 1 {
		return true
	}
	return (signals >= 2 && semicolons >= 2) ||
		(braces >= 3 && declarations >= 3 && semicolons >= 3)
}

func stripCSSRules(text string) string {
	for {
		open := strings.IndexByte(text, '{')
		if open < 0 {
			return text
		}
		close := matchingBrace(text, open)
		if close < 0 {
			return text
		}

		start := open
		for start > 0 {
			if text[start-1] == '}' || text[start-1] == '\n' || text[start-1] == '\r' {
				break
			}
			start--
		}
		text = text[:start] + text[close+1:]
	}
}

func matchingBrace(text string, open int) int {
	depth := 0
	var quote byte
	escaped := false
	for i := open; i < len(text); i++ {
		ch := text[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
