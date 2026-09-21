package mail

import (
	"testing"
)

func TestExtractOTP(t *testing.T) {
	tests := []struct {
		name         string
		subject      string
		body         string
		expectedCode string
		expectedLink string
	}{
		{
			name:         "中文验证码冒号",
			subject:      "欢迎注册",
			body:         "您的验证码是：849201，请在 10 分钟内输入。",
			expectedCode: "849201",
		},
		{
			name:         "英文 verification code",
			subject:      "Your verification code",
			body:         "Here is your verification code: 394821 for login.",
			expectedCode: "394821",
		},
		{
			name:         "英文 security code 短横或空格",
			subject:      "Security Alert",
			body:         "Use security code 9283 to verify your account.",
			expectedCode: "9283",
		},
		{
			name:         "英文 your code is",
			subject:      "Sign in to your account",
			body:         "Your code is: 482019",
			expectedCode: "482019",
		},
		{
			name:         "包含激活链接",
			subject:      "Confirm your email",
			body:         "Click here: https://example.com/confirm?token=xyz123 to activate.",
			expectedLink: "https://example.com/confirm?token=xyz123",
		},
		{
			name:         "跨行且包含干扰词 Your code is",
			subject:      "Your verification code",
			body:         "Your code is: 582910. Do not share it.",
			expectedCode: "582910",
		},
		{
			name:         "5位验证码且包含年份干扰",
			subject:      "2026年注册验证",
			body:         "您的验证口令是 78912",
			expectedCode: "78912",
		},
		{
			name:         "连字号验证码 (123-456)",
			subject:      "Telegram login code",
			body:         "Your login code is 123-456. Do not give this code to anyone.",
			expectedCode: "123456",
		},
		{
			name:         "带空格连字号验证码与 One-time password",
			subject:      "Your One-Time Password",
			body:         "Your one-time password is: 987 654 for verification.",
			expectedCode: "987654",
		},
		{
			name:         "反向倒装且包含修饰定语_中文",
			subject:      "登录确认",
			body:         "849201 为您的本次登录验证码，切勿泄露。",
			expectedCode: "849201",
		},
		{
			name:         "反向倒装且包含服务商品牌修饰_英文",
			subject:      "Security Code",
			body:         "492019 is your AWS verification code. Valid for 10 minutes.",
			expectedCode: "492019",
		},
		{
			name:         "反向倒装5位安全码",
			subject:      "Account PIN",
			body:         "58291 is your security code.",
			expectedCode: "58291",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ExtractOTP(tt.subject, tt.body)
			if res == nil {
				t.Fatalf("ExtractOTP 返回 nil")
			}
			if tt.expectedCode != "" && res.Code != tt.expectedCode {
				t.Errorf("Code 期望 %s, 实际 %s", tt.expectedCode, res.Code)
			}
			if tt.expectedLink != "" && res.MagicLink != tt.expectedLink {
				t.Errorf("MagicLink 期望 %s, 实际 %s", tt.expectedLink, res.MagicLink)
			}
		})
	}
}

func BenchmarkExtractOTP(b *testing.B) {
	subject := "Your verification code"
	body := "Here is your verification code: 394821 for login. Or click https://example.com/confirm?token=xyz123 to activate."
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ExtractOTP(subject, body)
	}
}
