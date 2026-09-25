/**
 * [INPUT]: 依赖 bytes, encoding/base64, fmt, io, mime, mime/multipart, mime/quotedprintable, net/mail, strings, time, github.com/emersion/go-imap, github.com/emersion/go-message/charset
 * [OUTPUT]: 对外提供 toMessage, toMessageWithBody, toMessageWithHeaderOnly, readBody, decodeHeader, decodeAppleRelay, folderRole, folderSortRank
 * [POS]: internal/mail 的 MIME 多级解析与字符集转码中心，负责将原始邮件协议流清洗为高层结构体
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/utf7"
	"github.com/emersion/go-message/charset"
)

// ============================================================================
// 邮件消息与目录角色解析
// ============================================================================

func toMessage(msg *imap.Message, folder ...string) Message {
	m := Message{}
	if len(folder) > 0 && folder[0] != "" {
		m.Folder = folder[0]
	}
	if _, fetched := msg.Items[imap.FetchFlags]; fetched || msg.Flags != nil {
		unread := !hasAttr(msg.Flags, imap.SeenFlag)
		m.Unread = &unread
	}
	if msg.Uid > 0 {
		m.ID = fmt.Sprintf("%d", msg.Uid)
		m.UID = msg.Uid
		m.Provider = "imap"
	}
	if msg.Envelope != nil {
		if len(msg.Envelope.From) > 0 {
			f := msg.Envelope.From[0]
			name := strings.TrimSpace(decodeHeader(f.PersonalName))
			addr := f.Address()
			cleanAddr := decodeAppleRelay(addr)
			if cleanAddr == "" {
				cleanAddr = addr
			}
			if name != "" {
				m.From = fmt.Sprintf("%s <%s>", name, cleanAddr)
			} else {
				m.From = cleanAddr
			}
		}
		if len(msg.Envelope.To) > 0 {
			addrs := make([]string, 0, len(msg.Envelope.To))
			for _, a := range msg.Envelope.To {
				addrs = append(addrs, a.Address())
			}
			m.To = strings.Join(addrs, ", ")
		}
		m.Subject = strings.Join(strings.Fields(decodeHeader(msg.Envelope.Subject)), " ")
		if !msg.Envelope.Date.IsZero() {
			m.Date = msg.Envelope.Date.UTC().Format(time.RFC3339)
		} else if !msg.InternalDate.IsZero() {
			m.Date = msg.InternalDate.UTC().Format(time.RFC3339)
		}
	}
	if m.From != "" {
		m.From = strings.Join(strings.Fields(decodeHeader(m.From)), " ")
	}
	if m.To != "" {
		m.To = strings.Join(strings.Fields(decodeHeader(m.To)), " ")
	}
	return m
}

// toMessageWithBody 在 toMessage 基础上解析正文填充 Preview(供 OTP 提取)。
func toMessageWithBody(msg *imap.Message, folder ...string) Message {
	m := toMessage(msg, folder...)
	for _, section := range []*imap.BodySectionName{{Peek: true}, {}} {
		r := msg.GetBody(section)
		if r == nil {
			continue
		}
		em, err := mail.ReadMessage(r)
		if err != nil {
			continue
		}
		body, err := readBody(em)
		if err != nil {
			continue
		}
		m.Preview = strings.TrimSpace(body)
		recipients := extractStructuralRecipients(m.To, em.Header)
		m.match = strings.Join(recipients, "\n")
		break
	}
	return m
}

// toMessageWithHeaderOnly 仅解析 Envelope 与指定 Header 节 (不拉取正文)，供 PR-04A metadata-first 过滤使用。
func toMessageWithHeaderOnly(msg *imap.Message, section *imap.BodySectionName, folder ...string) Message {
	m := toMessage(msg, folder...)
	if section != nil {
		if r := msg.GetBody(section); r != nil {
			if em, err := mail.ReadMessage(r); err == nil {
				recipients := extractStructuralRecipients(m.To, em.Header)
				m.match = strings.Join(recipients, "\n")
			}
		}
	}
	if m.match == "" {
		recipients := extractStructuralRecipients(m.To, nil)
		m.match = strings.Join(recipients, "\n")
	}
	return m
}

// extractStructuralRecipients 提取真实的信封与投递收件人地址 (To, Cc, Delivered-To, X-Original-To, Envelope-To)。
// 严禁纳入 From, Subject, Preview 等非收件人字段 (PR-06 Section 9.4, V06/V07)。
func extractStructuralRecipients(toHeader string, header mail.Header) []string {
	seen := make(map[string]struct{})
	var recipients []string
	add := func(raw string) {
		if strings.TrimSpace(raw) == "" {
			return
		}
		if list, err := mail.ParseAddressList(raw); err == nil && len(list) > 0 {
			for _, a := range list {
				norm := strings.ToLower(strings.TrimSpace(a.Address))
				if norm != "" {
					if _, ok := seen[norm]; !ok {
						seen[norm] = struct{}{}
						recipients = append(recipients, norm)
					}
				}
			}
			return
		}
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
			part = strings.Trim(part, "<>,;\"' \t\r\n")
			if strings.Contains(part, "@") {
				norm := strings.ToLower(strings.TrimSpace(part))
				if norm != "" {
					if _, ok := seen[norm]; !ok {
						seen[norm] = struct{}{}
						recipients = append(recipients, norm)
					}
				}
			}
		}
	}

	add(toHeader)
	if header != nil {
		add(header.Get("To"))
		add(header.Get("Cc"))
		add(header.Get("Delivered-To"))
		add(header.Get("X-Original-To"))
		add(header.Get("Envelope-To"))
		add(header.Get("X-Forwarded-To"))
		add(header.Get("Resent-To"))
		add(header.Get("X-Envelope-To"))
		add(header.Get("Original-Recipient"))
		add(header.Get("X-Apple-Original-To"))
		add(header.Get("X-Apple-Recipient"))
	}
	return recipients
}

// RecipientAddresses 返回本封邮件中所有经解析的结构化收件人地址。
func (m Message) RecipientAddresses() []string {
	if m.match != "" {
		return strings.Split(m.match, "\n")
	}
	return extractStructuralRecipients(m.To, nil)
}

// matches 判断本邮件是否匹配指定目标收件人。
// 严正约束：只能在结构化收件人头 (To, Cc, Delivered-To 等) 中核对，
// 严禁匹配 Preview、Subject 或 From；缺少 To 绝不自动填补 (PR-06 Section 9.4, V06/V07)。
func (m Message) matches(recipient string) bool {
	needle := strings.ToLower(strings.TrimSpace(recipient))
	if needle == "" {
		return true
	}
	recipients := m.RecipientAddresses()
	for _, addr := range recipients {
		if addr == needle {
			return true
		}
		if strings.HasPrefix(needle, "@") && strings.HasSuffix(addr, needle) {
			return true
		}
	}
	return false
}

func hasAttr(attrs []string, attr string) bool {
	for _, item := range attrs {
		if strings.EqualFold(item, attr) {
			return true
		}
	}
	return false
}

func folderRole(name string, attrs []string) string {
	switch {
	case strings.EqualFold(name, "INBOX"):
		return "inbox"
	case hasAttr(attrs, imap.JunkAttr):
		return "junk"
	case hasAttr(attrs, imap.SentAttr):
		return "sent"
	case hasAttr(attrs, imap.DraftsAttr):
		return "drafts"
	case hasAttr(attrs, imap.TrashAttr):
		return "trash"
	case hasAttr(attrs, imap.ArchiveAttr):
		return "archive"
	}

	lower := strings.ToLower(name)
	decoded, _ := utf7.Encoding.NewDecoder().String(name)
	lowerDecoded := strings.ToLower(decoded)

	isMatch := func(keywords ...string) bool {
		for _, kw := range keywords {
			if strings.Contains(lower, kw) || strings.Contains(lowerDecoded, kw) {
				return true
			}
		}
		return false
	}

	switch {
	case isMatch("junk", "spam", "bulk", "垃圾", "广告"):
		return "junk"
	case isMatch("sent", "已发送", "已发"):
		return "sent"
	case isMatch("draft", "草稿"):
		return "drafts"
	case isMatch("trash", "deleted", "已删除", "废纸篓"):
		return "trash"
	case isMatch("archive", "归档"):
		return "archive"
	default:
		return "custom"
	}
}

func folderSortRank(folder Folder) int {
	switch folder.Role {
	case "inbox":
		return 0
	case "junk":
		return 1
	case "archive":
		return 2
	case "sent":
		return 3
	case "drafts":
		return 4
	case "trash":
		return 5
	default:
		return 10
	}
}

// ============================================================================
// 邮件头与中继地址还原
// ============================================================================

// decodeHeader 解码 RFC 2047 编码的邮件头(如 =?UTF-8?B?xxx?=)。
func decodeHeader(s string) string {
	if s == "" {
		return ""
	}
	dec := mime.WordDecoder{CharsetReader: charset.Reader}
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// decodeAppleRelay 尝试还原 Apple Hide My Email 的反向中继发件人。
// 例: noreply_at_email_openai_com_beqe84f0f0ndc7_gcph8541@icloud.com -> noreply@email.openai.com
func decodeAppleRelay(addr string) string {
	lower := strings.ToLower(addr)
	if !strings.HasSuffix(lower, "@icloud.com") {
		return addr
	}
	atIdx := strings.Index(lower, "_at_")
	if atIdx <= 0 {
		return addr
	}
	// strings.ToLower 对极少数码位(如 U+023A/U+023E)会改变字节长度，
	// 用 lower 上求得的索引去切 addr 会越界 panic(远端邮件可构造触发)。
	// 越界时直接放弃还原，绝不 panic。
	endIdx := len(addr) - len("@icloud.com")
	if atIdx+4 > endIdx {
		return addr
	}
	user := addr[:atIdx]
	afterAt := addr[atIdx+4 : endIdx]

	parts := strings.Split(afterAt, "_")
	if len(parts) < 2 {
		return addr
	}

	for i := 1; i < len(parts); i++ {
		p := strings.ToLower(parts[i])
		if i+1 < len(parts) && isCompoundTLD(p, strings.ToLower(parts[i+1])) {
			return user + "@" + strings.Join(parts[:i+2], ".")
		}
		if isKnownTLD(p) {
			return user + "@" + strings.Join(parts[:i+1], ".")
		}
	}
	return addr
}

func isCompoundTLD(first, second string) bool {
	switch first {
	case "com", "net", "org", "edu", "gov", "co", "ne", "ac", "or":
		switch second {
		case "cn", "uk", "jp", "hk", "tw", "au", "nz", "kr", "sg", "br", "za":
			return true
		}
	}
	return false
}

func isKnownTLD(tld string) bool {
	switch tld {
	case "com", "cn", "net", "org", "edu", "gov", "io", "co", "ai", "me", "app",
		"dev", "xyz", "info", "biz", "cc", "tv", "top", "vip", "ltd", "hk", "tw",
		"jp", "kr", "de", "fr", "uk", "ru", "sg", "nl", "in", "it", "es", "pl",
		"se", "no", "fi", "br", "mx", "ch", "at", "be", "dk", "cz", "ie", "nz",
		"eu", "us", "ca", "tech", "cloud", "online", "site", "store", "space",
		"pro", "live", "link", "club", "fun", "icu", "shop", "work":
		return true
	default:
		return false
	}
}

// ============================================================================
// MIME 正文解析与编码转换
// ============================================================================

// decodeTransferData 还原 base64 或 quoted-printable 编码的字节数据。
func decodeTransferData(data []byte, encoding string) []byte {
	enc := strings.ToLower(strings.TrimSpace(encoding))
	if strings.Contains(enc, "base64") {
		cleaned := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, string(data))
		if decoded, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
			return decoded
		}
		if decoded, err := base64.RawStdEncoding.DecodeString(cleaned); err == nil {
			return decoded
		}
	} else if strings.Contains(enc, "quoted-printable") {
		r := quotedprintable.NewReader(bytes.NewReader(data))
		if decoded, err := io.ReadAll(r); err == nil {
			return decoded
		}
	}
	return data
}

// decodeBodyCharset 根据 Content-Type 里的 charset 参数将字节流转换为标准 UTF-8 字符串
func decodeBodyCharset(raw []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return string(raw)
	}
	name := params["charset"]
	if name == "" || strings.EqualFold(name, "utf-8") || strings.EqualFold(name, "us-ascii") {
		return string(raw)
	}
	reader, err := charset.Reader(name, bytes.NewReader(raw))
	if err != nil {
		return string(raw)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return string(raw)
	}
	return string(decoded)
}

// readBody 读取邮件正文, 支持 multipart 分块(含嵌套)、base64 与 quoted-printable 编码。
func readBody(msg *mail.Message) (string, error) {
	ct := msg.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mediaType = "text/plain"
	}

	const maxBodyRead = 512 * 1024 // 512KB 单块正文读取上限，防御超大垃圾附件阻塞 IMAP

	if strings.HasPrefix(mediaType, "multipart/") {
		raw, _ := io.ReadAll(io.LimitReader(msg.Body, maxBodyRead))
		decoded := decodeTransferData(raw, msg.Header.Get("Content-Transfer-Encoding"))
		plainText, htmlText := readMultipartBody(decoded, params["boundary"], 0)
		if plainText != "" && htmlText != "" {
			if strings.TrimSpace(plainText) == strings.TrimSpace(htmlText) {
				return plainText, nil
			}
			return plainText + "\n\n" + htmlText, nil
		}
		if plainText != "" {
			return plainText, nil
		}
		if htmlText != "" {
			return htmlText, nil
		}
		return "", nil
	}

	// 单部分邮件
	raw, err := io.ReadAll(io.LimitReader(msg.Body, maxBodyRead))
	if err != nil {
		return "", err
	}
	enc := msg.Header.Get("Content-Transfer-Encoding")
	decoded := decodeTransferData(raw, enc)
	content := decodeBodyCharset(decoded, ct)
	if strings.EqualFold(mediaType, "text/html") {
		return sanitizePreview(content), nil
	}
	return sanitizePlainPreview(content), nil
}

// readMultipartBody 遍历 multipart 各部件: 文本部件取预览, 嵌套 multipart(如 mixed 套 alternative)递归展开,
// 否则带附件验证邮件的真实正文会被静默丢成空串。显式排除 Content-Disposition: attachment 避免附件污染。
func readMultipartBody(body []byte, boundary string, depth int) (string, string) {
	if boundary == "" || depth >= 5 {
		return "", ""
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var plainText, htmlText string
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		disposition, _, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		if strings.EqualFold(disposition, "attachment") {
			_ = p.Close()
			continue
		}

		partCT := p.Header.Get("Content-Type")
		partEnc := p.Header.Get("Content-Transfer-Encoding")
		partData, _ := io.ReadAll(io.LimitReader(p, 512*1024))
		decoded := decodeTransferData(partData, partEnc)

		plain, html := readMultipartPart(decoded, partCT, depth)
		if plainText == "" {
			plainText = plain
		}
		if htmlText == "" {
			htmlText = html
		}
	}
	return plainText, htmlText
}

// readMultipartPart 把单个部件转为 (纯文本预览, HTML 预览); 部件本身是嵌套 multipart 时继续下钻。
func readMultipartPart(data []byte, partCT string, depth int) (string, string) {
	mediaType, params, err := mime.ParseMediaType(partCT)
	if err != nil {
		mediaType = "text/plain"
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		return readMultipartBody(data, params["boundary"], depth+1)
	}

	if strings.EqualFold(mediaType, "text/plain") {
		return sanitizePlainPreview(decodeBodyCharset(data, partCT)), ""
	}
	if strings.EqualFold(mediaType, "text/html") {
		return "", sanitizePreview(decodeBodyCharset(data, partCT))
	}
	return "", ""
}
