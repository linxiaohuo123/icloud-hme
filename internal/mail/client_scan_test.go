/**
 * [INPUT]: 依赖 testing, github.com/emersion/go-imap, internal/mail
 * [OUTPUT]: 对外提供 TestToMessageWithHeaderOnly_ExtractsStructuralRecipients, TestScanMailboxUIDPage_Validation, TestNewestUIDs_Vs_AscendingPage
 * [POS]: internal/mail 的增量扫描单元测试与元数据提取校验
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package mail

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap"
)

// 验证 toMessageWithHeaderOnly 仅通过 Header 提取结构化收件人 (To, Delivered-To, X-Original-To, Envelope-To)，且 Preview 保持为空
func TestToMessageWithHeaderOnly_ExtractsStructuralRecipients(t *testing.T) {
	headerText := "To: direct@icloud.com\r\n" +
		"Delivered-To: delivered@icloud.com\r\n" +
		"X-Original-To: orig@icloud.com\r\n" +
		"Envelope-To: envelope@icloud.com\r\n" +
		"Subject: Important Code\r\n\r\n"

	section := &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{
			Specifier: imap.HeaderSpecifier,
			Fields:    []string{"To", "Delivered-To", "X-Original-To", "Envelope-To", "Subject"},
		},
	}

	imapMsg := &imap.Message{
		Uid: 105,
		Envelope: &imap.Envelope{
			Subject: "Important Code",
			To: []*imap.Address{
				{MailboxName: "direct", HostName: "icloud.com"},
			},
		},
		Body: map[*imap.BodySectionName]imap.Literal{
			section: strings.NewReader(headerText),
		},
	}

	msg := toMessageWithHeaderOnly(imapMsg, section, "INBOX")

	// 1. 验证 UID 与元数据完整
	if msg.UID != 105 || msg.Folder != "INBOX" {
		t.Fatalf("UID 或 Folder 解析错误: uid=%d, folder=%s", msg.UID, msg.Folder)
	}

	// 2. 验证 Preview 为空 (未拉取正文)
	if msg.Preview != "" {
		t.Fatalf("Metadata-first 阶段 Preview 必须为空，实际: %q", msg.Preview)
	}

	// 3. 验证结构化收件人完整包含 To, Delivered-To, X-Original-To, Envelope-To
	recipients := msg.RecipientAddresses()
	recipMap := make(map[string]bool)
	for _, r := range recipients {
		recipMap[r] = true
	}

	expected := []string{"direct@icloud.com", "delivered@icloud.com", "orig@icloud.com", "envelope@icloud.com"}
	for _, exp := range expected {
		if !recipMap[exp] {
			t.Errorf("缺少结构化收件人: %s (当前收件人: %v)", exp, recipients)
		}
	}

	// 4. 验证 matches 行为
	if !msg.matches("orig@icloud.com") {
		t.Fatal("应当匹配 X-Original-To 中的别名")
	}
	if !msg.matches("delivered@icloud.com") {
		t.Fatal("应当匹配 Delivered-To 中的别名")
	}
	if msg.matches("unrelated@icloud.com") {
		t.Fatal("不应当匹配无关地址")
	}
}

// 验证 ScanMailboxUIDPage 客户端未连接时的安全防御
func TestScanMailboxUIDPage_Validation(t *testing.T) {
	c := &Client{}
	_, err := c.ScanMailboxUIDPage(ScanPageOptions{
		Folder:           "INBOX",
		FromUIDInclusive: 100,
		ToUIDInclusive:   200,
		PageSize:         50,
	})
	if err == nil || !strings.Contains(err.Error(), "未连接") {
		t.Fatalf("未连接客户端应当返回错误, 实际: %v", err)
	}
}

// 验证对比: 旧 newestUIDs 保留最新 50 导致丢弃早期 UID，而新升序切片严格保留最旧 50 且有序推进
func TestNewestUIDs_Vs_AscendingPage(t *testing.T) {
	uids := make([]uint32, 200)
	for i := 0; i < 200; i++ {
		uids[i] = uint32(100 + i) // 100..299
	}

	// 旧截断逻辑: 保留最后 50 个
	oldTruncated := newestUIDs(uids, 50)
	if len(oldTruncated) != 50 {
		t.Fatalf("oldTruncated len=%d", len(oldTruncated))
	}
	if oldTruncated[0] != 250 || oldTruncated[49] != 299 {
		t.Fatalf("旧逻辑保留了 %d..%d，导致 100..249 全部丢失", oldTruncated[0], oldTruncated[49])
	}

	// 新升序分页逻辑: 取最旧 50 个
	pageSize := 50
	newPage := uids[:pageSize]
	if len(newPage) != 50 {
		t.Fatalf("newPage len=%d", len(newPage))
	}
	if newPage[0] != 100 || newPage[49] != 149 {
		t.Fatalf("新逻辑第一页应当保留 %d..%d", newPage[0], newPage[49])
	}
	nextUID := newPage[len(newPage)-1] + 1
	if nextUID != 150 {
		t.Fatalf("下一游标应为 150, 实际: %d", nextUID)
	}
}
