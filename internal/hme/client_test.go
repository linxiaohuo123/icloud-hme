package hme

import (
	"reflect"
	"testing"
	"time"
)

func TestValidationURLs(t *testing.T) {
	tests := []struct {
		name string
		host string
		want []string
	}{
		{
			name: "全球账号只用全球端点",
			host: "icloud.com",
			want: []string{"https://setup.icloud.com/setup/ws/1/validate"},
		},
		{
			name: "国区账号回退全球端点",
			host: "icloud.com.cn",
			want: []string{
				"https://setup.icloud.com.cn/setup/ws/1/validate",
				"https://setup.icloud.com/setup/ws/1/validate",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{Host: tt.host}
			if got := client.validationURLs(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("validationURLs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRequestOrigin(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://setup.icloud.com/setup/ws/1/validate", "https://www.icloud.com"},
		{"https://setup.icloud.com.cn/setup/ws/1/validate", "https://www.icloud.com.cn"},
		{"https://p123-maildomainws.icloud.com.cn/v2/hme/list", "https://www.icloud.com.cn"},
		{"https://p123-maildomainws.icloud.com/v2/hme/list", "https://www.icloud.com"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := requestOrigin(tt.url); got != tt.want {
				t.Fatalf("requestOrigin(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestParseAliasList_TimestampNormalization(t *testing.T) {
	jsonBody := `{
		"success": true,
		"result": {
			"hmeEmails": [
				{
					"hme": "ms_num@icloud.com",
					"anonymousId": "anon_1",
					"label": "ms_num",
					"isActive": true,
					"createTimestamp": 1789000919081
				},
				{
					"hme": "ms_str@icloud.com",
					"anonymousId": "anon_2",
					"label": "ms_str",
					"isActive": true,
					"createTimestamp": "1789000919081"
				},
				{
					"hme": "iso_str@icloud.com",
					"anonymousId": "anon_3",
					"label": "iso_str",
					"isActive": true,
					"createdAt": "2026-09-10T00:41:59Z"
				}
			]
		}
	}`

	aliases := parseAliasList(jsonBody)
	if len(aliases) != 3 {
		t.Fatalf("expected 3 aliases, got %d", len(aliases))
	}

	for _, a := range aliases {
		if a.CreatedAt == "" {
			t.Fatalf("alias %s CreatedAt is empty", a.Email)
		}
		parsed, err := time.Parse(time.RFC3339, a.CreatedAt)
		if err != nil {
			t.Fatalf("alias %s CreatedAt %q is not valid RFC3339: %v", a.Email, a.CreatedAt, err)
		}
		if parsed.Year() != 2026 {
			t.Fatalf("alias %s year is %d, expected 2026", a.Email, parsed.Year())
		}
	}
}
