package mail

import "testing"

func TestExtractOTPBoundaryCases(t *testing.T) {
	for _, tc := range []struct{ name, subject, body, want string }{
		{"year_shaped_code", "Login", "Your verification code is 202619", "202619"},
		{"repeated_digits", "Login", "Your verification code is 111111", "111111"},
		{"four_plus_four", "Login", "Your verification code is 1234-5678", "12345678"},
		{"tab_separated", "Login", "Your verification code is 5\t7\t6\t9\t3\t2", "576932"},
		{"bracketed_order_overrides_code", "Order [839201] confirmation", "Your verification code is 492019", "492019"},
		{"ordinary_bracketed_order", "Order [839201] shipped", "Your parcel is on its way.", ""},
		{"substring_keyword", "Shopping update", "Reference: 654321. Your parcel is on its way.", ""},
		{"link_token_is_not_otp", "Confirm your email", "Click https://example.com/confirm?token=839201", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ExtractOTP(tc.subject, tc.body)
			got := ""
			if r != nil {
				got = r.Code
			}
			if got != tc.want {
				t.Errorf("got code=%q want=%q result=%+v", got, tc.want, r)
			}
		})
	}
}
func TestExtractOTPUniqueCandidate(t *testing.T) {
	for _, tc := range []struct{ name, subject, body, code, link string }{
		{"explicit four digit year", "Login", "Your code is 2026", "2026", ""},
		{"zero code", "Login", "Your OTP is 000000", "000000", ""},
		{"long number", "Verification code", "Your code is 1234567890", "", ""},
		{"long grouped number", "Verification code", "Your code is 1234-5678-9012", "", ""},
		{"long spaced number", "Verification code", "Your code is 1 2 3 4 5 6 7 8 9", "", ""},
		{"two explicit codes", "Login", "Your code is 123456. Your code is 654321.", "", ""},
		{"subject conflicts with body", "Your code is 123456", "Your code is 654321", "", ""},
		{"same code twice", "Your code is 123456", "Your code is 123456", "123456", ""},
		{"two weak candidates", "Verification code", "123456 or 654321", "", ""},
		{"one weak candidate", "Verification code", "Please enter the number below.\n482019", "482019", ""},
		{"strong wins over weak", "Verification code", "Reference details 839201.\nYour code is 492019", "492019", ""},
		{"phone number", "Security notice", "Phone: 1234-5678", "", ""},
		{"identifier", "Verification code", "User ID: ABC123456", "", ""},
		{"hotpot", "Hotpot update", "Reference: 654321", "", ""},
		{"barcode", "Barcode", "654321", "", ""},
		{"newline digits", "Verification code", "Your code is 5\n7\n6\n9\n3\n2", "576932", ""},
		{"numeric link", "Confirm your email", "Click https://example.com/confirm?token=839201", "", "https://example.com/confirm?token=839201"},
		{"html link", "Confirm your email", `<a href="https://example.com/confirm?token=abc&amp;next=%2Fhome">Confirm</a>`, "", "https://example.com/confirm?token=abc&next=%2Fhome"},
		{"trailing punctuation", "Confirm your email", "(https://example.com/Verify?token=abc).", "", "https://example.com/Verify?token=abc"},
		{"two links", "Confirm your email", "https://example.com/confirm?token=a https://example.com/confirm?token=b", "", ""},
		{"domain keyword insufficient", "News", "https://verify.example.com/news", "", ""},
		{"ambiguous codes with link", "Verification code", "Your code is 123456. Your code is 654321. https://example.com/confirm?token=a", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractOTP(tc.subject, tc.body)
			code, link := "", ""
			if got != nil {
				code, link = got.Code, got.MagicLink
			}
			if code != tc.code || link != tc.link {
				t.Fatalf("got=%+v want code=%q link=%q", got, tc.code, tc.link)
			}
		})
	}
}
