package account

import (
	"testing"

	"icloud-hme/internal/hme"
)

func TestDeriveICloudEmail(t *testing.T) {
	tests := []struct {
		name     string
		info     *hme.AccountInfo
		expected string
	}{
		{
			name: "primary email is icloud.com",
			info: &hme.AccountInfo{
				PrimaryEmail: "user@icloud.com",
				AppleID:      "user@gmail.com",
			},
			expected: "user@icloud.com",
		},
		{
			name: "apple ID is me.com",
			info: &hme.AccountInfo{
				PrimaryEmail: "phone_123456",
				AppleID:      "user@me.com",
			},
			expected: "user@me.com",
		},
		{
			name: "apple ID is mac.com",
			info: &hme.AccountInfo{
				PrimaryEmail: "",
				AppleID:      "user@mac.com",
			},
			expected: "user@mac.com",
		},
		{
			name: "third party email should not fabricate fake icloud.com",
			info: &hme.AccountInfo{
				PrimaryEmail: "test12345@qq.com",
				AppleID:      "test12345@qq.com",
			},
			expected: "",
		},
		{
			name: "phone and third party email should not fabricate fake icloud.com",
			info: &hme.AccountInfo{
				PrimaryEmail: "+8613800000000",
				AppleID:      "myuser@163.com",
			},
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := deriveICloudEmail(tc.info)
			if actual != tc.expected {
				t.Errorf("deriveICloudEmail() = %q, expected %q", actual, tc.expected)
			}
		})
	}
}
