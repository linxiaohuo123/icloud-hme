package mail

import (
	"strings"
	"testing"
)

func TestSanitizePreviewRemovesInvisibleHTMLBlocks(t *testing.T) {
	raw := `<html><head><style>@font-face { font-family: Söhne; } body { color: red; }</style></head><body><p>验证码：123456</p><script>alert(1)</script></body></html>`
	if got := sanitizePreview(raw); got != "验证码：123456" {
		t.Fatalf("sanitizePreview() = %q, want readable body", got)
	}
}

func TestSanitizePreviewDropsCSSOnlyContent(t *testing.T) {
	raw := `@font-face { font-family: Söhne; } .ExternalClass { line-height: 100%; } #bodyTable { width: 560px; } body { font-family: Helvetica, Arial, sans-serif; }`
	if got := sanitizePreview(raw); got != "" {
		t.Fatalf("sanitizePreview() = %q, want empty CSS preview", got)
	}
}

func TestSanitizePreviewRemovesCSSPrefixAndKeepsBody(t *testing.T) {
	raw := `@font-face { font-family: Söhne; } .ExternalClass { line-height: 100%; } 正文内容`
	if got := sanitizePreview(raw); got != "正文内容" {
		t.Fatalf("sanitizePreview() = %q, want body text", got)
	}
}

func TestSanitizePreviewKeepsNormalText(t *testing.T) {
	raw := `订单号：A-123; 请在 10:00 前完成验证。`
	if got := sanitizePreview(raw); got != raw {
		t.Fatalf("sanitizePreview() = %q, want %q", got, raw)
	}
}

func TestDecodeAppleRelay(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "noreply_at_email_openai_com_beqe84f0f0ndc7_gcph8541@icloud.com",
			want: "noreply@email.openai.com",
		},
		{
			in:   "service_at_domain_com_cn_hash123456@icloud.com",
			want: "service@domain.com.cn",
		},
		{
			in:   "notify_at_github_com_token99@icloud.com",
			want: "notify@github.com",
		},
		{
			in:   "direct@gmail.com",
			want: "direct@gmail.com",
		},
		{
			in:   "user@icloud.com",
			want: "user@icloud.com",
		},
	}

	for _, tc := range cases {
		if got := decodeAppleRelay(tc.in); got != tc.want {
			t.Errorf("decodeAppleRelay(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizePreviewPreservesLinksForMagicLink(t *testing.T) {
	raw := `<p>请点击下方链接激活您的账号：</p><a href="https://example.com/activate?token=sec_987654">立即激活</a>`
	got := sanitizePreview(raw)
	if !strings.Contains(got, "https://example.com/activate?token=sec_987654") {
		t.Fatalf("sanitizePreview() = %q, want link URL preserved", got)
	}
	otp := ExtractOTP("激活邮件", got)
	if otp == nil || otp.MagicLink != "https://example.com/activate?token=sec_987654" {
		t.Fatalf("ExtractOTP failed to extract magic link from preview: %+v", otp)
	}
}
