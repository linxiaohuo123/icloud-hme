/**
 * [INPUT]: 依赖 testing, time
 * [OUTPUT]: 对外提供 parseRetention / parseOptionalDuration / validateAdminPassword 的配置解析回归测试
 * [POS]: 主程序的启动配置校验单元测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package main

import (
	"testing"
	"time"
)

// 保留期解析:支持天数写法，且拒绝会误删近期流水的过短配置。
func TestParseRetention(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{raw: "", want: 0, wantErr: false},
		{raw: "180d", want: 180 * 24 * time.Hour, wantErr: false},
		{raw: " 90d ", want: 90 * 24 * time.Hour, wantErr: false},
		{raw: "4320h", want: 4320 * time.Hour, wantErr: false},
		{raw: "30d", want: 30 * 24 * time.Hour, wantErr: false},
		{raw: "0d", wantErr: true},
		{raw: "-5d", wantErr: true},
		{raw: "abcd", wantErr: true},
		// 小于 24h 会误删近期流水，必须拒绝
		{raw: "1h", wantErr: true},
		{raw: "30m", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseRetention(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("parseRetention(%q) 应报错, 实际返回 %v", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseRetention(%q) 不应报错: %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("parseRetention(%q) = %v, 期望 %v", tc.raw, got, tc.want)
		}
	}
}

// 节流间隔留空表示"自动"，非空时必须在安全范围内。
func TestParseOptionalDuration(t *testing.T) {
	for _, env := range []string{"TEST_OPT_DUR_A"} {
		t.Setenv(env, "")
		if d, err := parseOptionalDuration(env, 50*time.Millisecond, time.Minute); err != nil || d != 0 {
			t.Fatalf("空值应返回 0 且不报错, 实际 %v %v", d, err)
		}
		t.Setenv(env, "900ms")
		if d, err := parseOptionalDuration(env, 50*time.Millisecond, time.Minute); err != nil || d != 900*time.Millisecond {
			t.Fatalf("900ms 应被接受, 实际 %v %v", d, err)
		}
		t.Setenv(env, "1h")
		if _, err := parseOptionalDuration(env, 50*time.Millisecond, time.Minute); err == nil {
			t.Fatal("超出上限应报错")
		}
		t.Setenv(env, "1ms")
		if _, err := parseOptionalDuration(env, 50*time.Millisecond, time.Minute); err == nil {
			t.Fatal("低于下限应报错(防误配成近乎无节流)")
		}
		t.Setenv(env, "abc")
		if _, err := parseOptionalDuration(env, 50*time.Millisecond, time.Minute); err == nil {
			t.Fatal("非法格式应报错")
		}
	}
}

// 可信代理列表解析:留空必须得到 nil(不信任任何代理头)，
// 否则 X-Forwarded-For 可被伪造，登录限流会被绕过。
func TestParseTrustedProxies(t *testing.T) {
	if got := parseTrustedProxies(""); got != nil {
		t.Fatalf("空值必须返回 nil(不信任代理头), 实际 %#v", got)
	}
	if got := parseTrustedProxies("   "); got != nil {
		t.Fatalf("纯空白必须返回 nil, 实际 %#v", got)
	}
	if got := parseTrustedProxies(" , , "); got != nil {
		t.Fatalf("仅分隔符必须返回 nil, 实际 %#v", got)
	}

	got := parseTrustedProxies(" 127.0.0.1 , 172.17.0.0/16 ,, ::1 ")
	want := []string{"127.0.0.1", "172.17.0.0/16", "::1"}
	if len(got) != len(want) {
		t.Fatalf("解析结果 %#v, 期望 %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项 = %q, 期望 %q (须保留顺序且去除空白)", i, got[i], want[i])
		}
	}
}

// 仓库公开的占位口令必须被拒绝，否则照抄模板就等于把母号凭据交给任何人。
func TestValidateAdminPassword(t *testing.T) {
	blocked := []string{
		"",
		"short",
		"admin123456",
		"your_strong_password_here",
		"change_this_to_strong_password",
		"your_secret_api_key_here",
		"__CHANGE_ME__", // 大小写也应命中
	}
	for _, pw := range blocked {
		if err := validateAdminPassword(pw); err == nil {
			t.Fatalf("口令 %q 应被拒绝", pw)
		}
	}
	allowed := []string{
		"a-very-strong-random-passphrase-2026",
		"Zq7#kLm9$vBn2",
	}
	for _, pw := range allowed {
		if err := validateAdminPassword(pw); err != nil {
			t.Fatalf("口令 %q 应被接受: %v", pw, err)
		}
	}
}
