package auth

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

// fixedNow 固定当前时间,便于测试过期。
var fixedNow = time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)

// fixedRandom 确定性随机源(用于生成 session ID / CSRF / salt)。
type fixedRandom struct {
	seq uint64
}

func (r *fixedRandom) Read(p []byte) (int, error) {
	for i := range p {
		r.seq++
		p[i] = byte(r.seq >> (8 * (uint(i) % 8)))
	}
	return len(p), nil
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	opts := Options{
		Password: "admin-pass-2026-strong",
		TTL:      12 * time.Hour,
		Now:      func() time.Time { return fixedNow },
		Random:   &fixedRandom{},
	}
	m, err := NewManager(opts)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	return m
}

// TestManagerWrongPassword 验证错误密码被拒绝。
func TestManagerWrongPassword(t *testing.T) {
	m := newTestManager(t)
	if _, _, ok := m.Login("wrong-password"); ok {
		t.Fatal("错误密码不应登录成功")
	}
}

// TestManagerLoginSuccess 验证正确密码登录并生成会话。
func TestManagerLoginSuccess(t *testing.T) {
	m := newTestManager(t)
	id, sess, ok := m.Login("admin-pass-2026-strong")
	if !ok {
		t.Fatal("正确密码应登录成功")
	}
	if id == "" {
		t.Fatal("session ID 不应为空")
	}
	if sess.CSRFToken == "" {
		t.Fatal("CSRF token 不应为空")
	}
	if !sess.ExpiresAt.Equal(fixedNow.Add(12 * time.Hour)) {
		t.Fatalf("过期时间错误: %v", sess.ExpiresAt)
	}
	// 两个登录 token 不相同(即使使用确定性随机源,ID 也应变化)
	id2, sess2, ok2 := m.Login("admin-pass-2026-strong")
	if !ok2 {
		t.Fatal("第二次登录应成功")
	}
	if id == id2 {
		t.Fatal("两个登录的 session ID 不应相同")
	}
	if sess.CSRFToken == sess2.CSRFToken {
		t.Fatal("两个登录的 CSRF 不应相同")
	}
}

// TestManagerValidate 验证会话校验与 CSRF。
func TestManagerValidate(t *testing.T) {
	m := newTestManager(t)
	id, sess, ok := m.Login("admin-pass-2026-strong")
	if !ok {
		t.Fatal("登录失败")
	}
	if got, ok2 := m.Validate(id); !ok2 || got.CSRFToken != sess.CSRFToken {
		t.Fatalf("有效会话应通过校验: %+v ok=%v", got, ok2)
	}
	if _, ok2 := m.Validate("nonexistent"); ok2 {
		t.Fatal("不存在的会话不应通过")
	}
	if !m.ValidateCSRF(id, sess.CSRFToken) {
		t.Fatal("正确 CSRF 应通过")
	}
	if m.ValidateCSRF(id, "wrong-token") {
		t.Fatal("错误 CSRF 不应通过")
	}
}

// TestManagerSessionExpiry 验证会话过期后失效。
func TestManagerSessionExpiry(t *testing.T) {
	now := fixedNow
	opts := Options{
		Password: "admin-pass-2026-strong",
		TTL:      12 * time.Hour,
		Now:      func() time.Time { return now },
		Random:   &fixedRandom{},
	}
	m, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	id, _, ok := m.Login("admin-pass-2026-strong")
	if !ok {
		t.Fatal("登录失败")
	}
	// 时间越过 TTL 后会话应失效
	now = now.Add(13 * time.Hour)
	if _, ok := m.Validate(id); ok {
		t.Fatal("过期会话不应通过校验")
	}
}

// TestManagerLogout 验证退出立即失效。
func TestManagerLogout(t *testing.T) {
	m := newTestManager(t)
	id, _, ok := m.Login("admin-pass-2026-strong")
	if !ok {
		t.Fatal("登录失败")
	}
	m.Logout(id)
	if _, ok := m.Validate(id); ok {
		t.Fatal("退出后会话应失效")
	}
	if m.ValidateCSRF(id, "token") {
		t.Fatal("退出后 CSRF 不应通过")
	}
}

// TestManagerStoresHashedKeys 验证 map 不以原始 session ID 为键。
func TestManagerStoresHashedKeys(t *testing.T) {
	m := newTestManager(t)
	id, _, ok := m.Login("admin-pass-2026-strong")
	if !ok {
		t.Fatal("登录失败")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.sessions {
		keyStr := string(key[:])
		if strings.Contains(keyStr, id) {
			t.Fatalf("map key 泄露原始 session ID: %q", keyStr)
		}
		if keyStr == id {
			t.Fatal("map 直接以 session ID 为键")
		}
		if sha256.Sum256([]byte(id)) != key {
			t.Fatalf("map key 不是 session ID 的 SHA-256: %q", keyStr)
		}
	}
}

// TestManagerRejectsWeakPassword 验证构造器拒绝短于 8 字符的密码。
func TestManagerRejectsWeakPassword(t *testing.T) {
	opts := Options{
		Password: "short",
		TTL:      12 * time.Hour,
		Now:      func() time.Time { return fixedNow },
		Random:   &fixedRandom{},
	}
	if _, err := NewManager(opts); err == nil {
		t.Fatal("短密码应被拒绝")
	}
	if _, err := NewManager(Options{}); err == nil {
		t.Fatal("空密码应被拒绝")
	}
	// 7 字符仍拒绝
	opts.Password = "1234567"
	if _, err := NewManager(opts); err == nil {
		t.Fatal("7 字符密码应被拒绝")
	}
	// 8 字符接受
	opts.Password = "12345678"
	if _, err := NewManager(opts); err != nil {
		t.Fatalf("8 字符密码应被接受: %v", err)
	}
}

// TestManagerSessionLimit 验证会话数量上限与淘汰。
func TestManagerSessionLimit(t *testing.T) {
	// 不同过期时间:模拟最早过期的会话被优先淘汰
	now := fixedNow
	opts := Options{
		Password: "admin-pass-2026-strong",
		TTL:      12 * time.Hour,
		Now:      func() time.Time { return now },
		Random:   &fixedRandom{},
	}
	m, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, maxSessions+5)
	for i := 0; i < maxSessions+5; i++ {
		id, _, ok := m.Login("admin-pass-2026-strong")
		if !ok {
			t.Fatalf("第 %d 次登录失败", i)
		}
		ids = append(ids, id)
		now = now.Add(time.Minute)
	}
	m.mu.Lock()
	count := len(m.sessions)
	m.mu.Unlock()
	if count > maxSessions {
		t.Fatalf("会话数 %d 超过上限 %d", count, maxSessions)
	}
	// 最早的会话应被淘汰
	if _, ok := m.Validate(ids[0]); ok {
		t.Fatal("最早过期的会话应被淘汰")
	}
	// 最新的会话应仍有效
	if _, ok := m.Validate(ids[len(ids)-1]); !ok {
		t.Fatal("最新会话应仍有效")
	}
}

// TestManagerConstantTimeCompare 验证 CSRF 使用常量时间比较(仅检查行为正确)。
func TestManagerConstantTimeCompare(t *testing.T) {
	m := newTestManager(t)
	id, sess, ok := m.Login("admin-pass-2026-strong")
	if !ok {
		t.Fatal("登录失败")
	}
	// 构造与正确 token 长度相同但内容不同的 token
	wrong := strings.Repeat("A", len(sess.CSRFToken))
	if m.ValidateCSRF(id, wrong) {
		t.Fatal("错误的 CSRF 不应通过")
	}
	if m.ValidateCSRF(id, sess.CSRFToken[:len(sess.CSRFToken)-1]+"B") {
		t.Fatal("尾字符不同的 CSRF 不应通过")
	}
}

// TestManagerBadReader 验证随机源失败时构造器报错。
func TestManagerBadReader(t *testing.T) {
	opts := Options{
		Password: "admin-pass-2026-strong",
		TTL:      12 * time.Hour,
		Now:      func() time.Time { return fixedNow },
		Random:   bytes.NewReader(nil), // EOF
	}
	if _, err := NewManager(opts); err == nil {
		t.Fatal("随机源失败时构造器应报错")
	}
}

// TestManagerRestartResilience 验证服务重启后旧会话自动恢复，用户无需重新登录。
func TestManagerRestartResilience(t *testing.T) {
	opts := Options{
		Password: "admin-pass-2026-strong",
		TTL:      12 * time.Hour,
		Now:      func() time.Time { return fixedNow },
		Random:   &fixedRandom{},
	}
	m1, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	id, sess, ok := m1.Login("admin-pass-2026-strong")
	if !ok {
		t.Fatal("登录失败")
	}

	// 模拟服务进程重启：新建 m2 实例（sessions 内存 map 为空）
	m2, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.sessions) != 0 {
		t.Fatal("新实例初始 sessions map 应为空")
	}

	// 校验旧 sessionID 应通过自愈验证
	gotSess, ok := m2.Validate(id)
	if !ok {
		t.Fatal("服务重启后合法 sessionID 应通过自愈验证")
	}
	if !gotSess.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Fatalf("过期时间不一致: got %v, want %v", gotSess.ExpiresAt, sess.ExpiresAt)
	}
	if gotSess.CSRFToken != sess.CSRFToken {
		t.Fatalf("自愈后 CSRF Token 发生变化: got %s, want %s", gotSess.CSRFToken, sess.CSRFToken)
	}
	if !m2.ValidateCSRF(id, sess.CSRFToken) {
		t.Fatal("客户端保存的原 CSRF 应在服务重启后继续通过校验")
	}
}
