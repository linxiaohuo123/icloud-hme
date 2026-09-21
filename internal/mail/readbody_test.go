package mail

import (
	"net/mail"
	"strings"
	"testing"
)

// 嵌套 multipart(mixed 套 alternative):正文在最内层,不递归会把正文丢成空串导致验证码嗅探失效
func TestReadBodyNestedMultipart(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: alias@icloud.com\r\n" +
		"Subject: code\r\n" +
		"Content-Type: multipart/mixed; boundary=outer\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=inner\r\n" +
		"\r\n" +
		"--inner\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"您的验证码是 123456\r\n" +
		"--inner\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<b>您的验证码是 123456</b>\r\n" +
		"--inner--\r\n" +
		"--outer\r\n" +
		"Content-Type: application/octet-stream; name=\"a.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"AAAA\r\n" +
		"--outer--\r\n"

	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	body, err := readBody(msg)
	if err != nil {
		t.Fatalf("readBody failed: %v", err)
	}
	if !strings.Contains(body, "123456") {
		t.Fatalf("nested body lost: %q", body)
	}
}

// 普通单部分纯文本不回归
func TestReadBodyPlainSinglePart(t *testing.T) {
	raw := "From: sender@example.com\r\nTo: alias@icloud.com\r\nSubject: s\r\n\r\nhello world"
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	body, err := readBody(msg)
	if err != nil {
		t.Fatalf("readBody failed: %v", err)
	}
	if body != "hello world" {
		t.Fatalf("plain body mismatch: %q", body)
	}
}

// 测试非 UTF-8 (GBK) 字符集的自动转码
func TestDecodeBodyCharsetGBK(t *testing.T) {
	// "验证码: 889900" in GBK
	gbkBytes := []byte("\xd1\xe9\xd6\xa4\xc2\xeb: 889900")
	ct := "text/plain; charset=gbk"
	decoded := decodeBodyCharset(gbkBytes, ct)
	if !strings.Contains(decoded, "验证码: 889900") {
		t.Fatalf("GBK decoding failed, got: %q", decoded)
	}

	// 测试默认 UTF-8 直通
	utf8Bytes := []byte("hello 验证码")
	if decodeBodyCharset(utf8Bytes, "text/plain; charset=utf-8") != "hello 验证码" {
		t.Fatalf("UTF-8 passthrough failed")
	}
}

// 测试 multipart 附件跳过: 附件中的虚假数字不干扰正文验证码提取
func TestReadBodySkipAttachment(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: alias@icloud.com\r\n" +
		"Subject: code\r\n" +
		"Content-Type: multipart/mixed; boundary=mix\r\n" +
		"\r\n" +
		"--mix\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"您的真实验证码是 778899\r\n" +
		"--mix\r\n" +
		"Content-Type: text/plain; name=\"fake.txt\"\r\n" +
		"Content-Disposition: attachment; filename=\"fake.txt\"\r\n" +
		"\r\n" +
		"干扰数字 000000\r\n" +
		"--mix--\r\n"

	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	body, err := readBody(msg)
	if err != nil {
		t.Fatalf("readBody failed: %v", err)
	}
	if !strings.Contains(body, "778899") {
		t.Fatalf("real code lost: %q", body)
	}
	if strings.Contains(body, "000000") {
		t.Fatalf("attachment content should be skipped, but got: %q", body)
	}
}

// 测试 multipart/alternative 下如果纯文本部分未包含验证码，但 HTML 包含验证码时，两者均被保留
func TestReadBodyMultipartAlternativeCombinesPlainAndHTML(t *testing.T) {
	raw := "From: sender@example.com\r\n" +
		"To: alias@icloud.com\r\n" +
		"Subject: Verification\r\n" +
		"Content-Type: multipart/alternative; boundary=boundary_alt\r\n" +
		"\r\n" +
		"--boundary_alt\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Please view this email in an HTML compatible browser.\r\n" +
		"--boundary_alt\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>Your security code is <strong>654321</strong></p>\r\n" +
		"--boundary_alt--\r\n"

	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	body, err := readBody(msg)
	if err != nil {
		t.Fatalf("readBody failed: %v", err)
	}
	if !strings.Contains(body, "654321") {
		t.Fatalf("verification code in HTML must not be dropped: %q", body)
	}
	otp := ExtractOTP("Verification", body)
	if otp == nil || otp.Code != "654321" {
		t.Fatalf("ExtractOTP failed to extract code: %+v", otp)
	}
}

