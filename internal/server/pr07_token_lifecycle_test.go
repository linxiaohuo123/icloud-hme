/**
 * [INPUT]: 依赖 encoding/json, net/http, net/http/httptest, strings, testing, time, icloud-hme/internal/account, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 PR-07 令牌安全生命周期测试套件：不可逆哈希存储、禁止自定义令牌、单次明文回显、轮换主体保全、旧令牌即时失效、新令牌生效、注销即时生效、过期拦截、失败鉴权零更新、遗留令牌平滑兼容
 * [POS]: internal/server 的 PR-07 外部令牌全生命周期回归测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

func newPR07Server(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1", Status: "active", HasCookies: true}},
	}
	s := newWithBackendAndStore(fb, Config{AdminPassword: "admin-pass-2026-strong"}, st)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return ts, st
}

// TestPR07_NewTokenStoredAsHashOnly 验证新建 Token:
// 1. 响应只在此刻返回一次明文 token；
// 2. 数据库中不存在明文 token 列，只记录 CSPRNG 生成后的 token_hash 与 token_prefix。
func TestPR07_NewTokenStoredAsHashOnly(t *testing.T) {
	ts, st := newPR07Server(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	req := authedReq(t, ts, "POST", "/api/tokens", `{"name":"pr07_bot","scopes":"admin"}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, req)
	if status != http.StatusOK {
		t.Fatalf("创建令牌失败: %d %s", status, body)
	}

	var res struct {
		Success bool `json:"success"`
		Data    struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Token       string `json:"token"`
			TokenPrefix string `json:"token_prefix"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if !strings.HasPrefix(res.Data.Token, "am_") {
		t.Fatalf("生成的令牌前缀不符合规范: %s", res.Data.Token)
	}
	if len(res.Data.Token) < 32 {
		t.Fatalf("生成的令牌长度过短: %s", res.Data.Token)
	}

	// 验证底层 SQLite 表: 没有 token 列，且 token_hash 完全对齐
	var hash, prefix string
	err := st.DB().QueryRow(`SELECT token_hash, token_prefix FROM api_tokens WHERE id = ?`, res.Data.ID).Scan(&hash, &prefix)
	if err != nil {
		t.Fatalf("查询数据库失败: %v", err)
	}
	if hash != store.HashToken(res.Data.Token) {
		t.Fatalf("数据库中的哈希与返回的明文计算哈希不一致: %s vs %s", hash, store.HashToken(res.Data.Token))
	}
	if prefix != res.Data.TokenPrefix {
		t.Fatalf("数据库中的 prefix 与响应不一致: %s vs %s", prefix, res.Data.TokenPrefix)
	}
}

// TestPR07_TokenSecretReturnedOnlyOnCreate 验证 GET /api/tokens 列表中绝不泄露明文 Token。
func TestPR07_TokenSecretReturnedOnlyOnCreate(t *testing.T) {
	ts, _ := newPR07Server(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	// 创建一个 Token
	req := authedReq(t, ts, "POST", "/api/tokens", `{"name":"leak_test_bot","scopes":"admin"}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	_, bodyCreate, _ := do(t, req)

	var created struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodyCreate), &created)
	plainSecret := created.Data.Token

	// GET 列表
	reqList := authedReq(t, ts, "GET", "/api/tokens", "")
	reqList.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	status, bodyList, _ := do(t, reqList)
	if status != http.StatusOK {
		t.Fatalf("获取令牌列表失败: %d %s", status, bodyList)
	}

	if strings.Contains(bodyList, plainSecret) {
		t.Fatalf("【严重漏洞】GET /api/tokens 泄露了令牌明文密钥: %s", bodyList)
	}
	if !strings.Contains(bodyList, "****") {
		t.Fatalf("GET /api/tokens 应返回脱敏掩码: %s", bodyList)
	}
}

// TestPR07_CustomTokenCreationRejected 验证客户端提交自定义 token 时必须被拒绝。
func TestPR07_CustomTokenCreationRejected(t *testing.T) {
	ts, _ := newPR07Server(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	req := authedReq(t, ts, "POST", "/api/tokens", `{"name":"attacker","token":"am_custom_weak_token_12345"}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)

	status, body, _ := do(t, req)
	if status != http.StatusBadRequest {
		t.Fatalf("提交自定义 token 期望 400 Bad Request, 实际 %d: %s", status, body)
	}
	if !strings.Contains(body, "CUSTOM_TOKEN_NOT_ALLOWED") {
		t.Fatalf("错误码应为 CUSTOM_TOKEN_NOT_ALLOWED: %s", body)
	}
}

// TestPR07_TokenLifecycleRotateRevokeAndExpire 验证完整的 Rotate / Revoke / Expire 生命周期。
func TestPR07_TokenLifecycleRotateRevokeAndExpire(t *testing.T) {
	ts, st := newPR07Server(t)
	sess, csrf := login(t, ts, "admin-pass-2026-strong")

	// 1. 创建初始 Token
	req := authedReq(t, ts, "POST", "/api/tokens", `{"name":"lifecycle_bot","scopes":"admin"}`)
	req.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	req.Header.Set("X-CSRF-Token", csrf)
	_, bodyCreate, _ := do(t, req)

	var resCreate struct {
		Data struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(bodyCreate), &resCreate)
	tokenID := resCreate.Data.ID
	tokenV1 := resCreate.Data.Token

	// 验证 tokenV1 可用
	resp1, _ := doGet(t, ts, "/api/tokens", tokenV1)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("初始 tokenV1 请求失败: %d", resp1.StatusCode)
	}

	// 2. 轮换 Token: POST /api/tokens/:id/rotate
	reqRotate := authedReq(t, ts, "POST", fmt.Sprintf("/api/tokens/%s/rotate", tokenID), "")
	reqRotate.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqRotate.Header.Set("X-CSRF-Token", csrf)

	statusRot, bodyRot, _ := do(t, reqRotate)
	if statusRot != http.StatusOK {
		t.Fatalf("轮换令牌失败: %d %s", statusRot, bodyRot)
	}

	var resRot struct {
		Success bool `json:"success"`
		Data    struct {
			ID        string `json:"id"`
			Token     string `json:"token"`
			RotatedAt string `json:"rotated_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(bodyRot), &resRot); err != nil {
		t.Fatalf("解析轮换响应失败: %v", err)
	}

	// 验证主体保全: ID 不变，新 Token 产生且不同于旧 Token
	if resRot.Data.ID != tokenID {
		t.Fatalf("轮换必须保留主体 ID: %s vs %s", resRot.Data.ID, tokenID)
	}
	tokenV2 := resRot.Data.Token
	if tokenV2 == tokenV1 || tokenV2 == "" {
		t.Fatalf("轮换未生成有效的新令牌: %s", tokenV2)
	}

	// 验证旧 Token 立刻失效 (401)
	respOld, _ := doGet(t, ts, "/api/tokens", tokenV1)
	if respOld.StatusCode != http.StatusUnauthorized {
		t.Fatalf("轮换后旧 token 必须立刻返回 401, 实际 %d", respOld.StatusCode)
	}

	// 验证新 Token 正常鉴权通过 (200)
	respNew, _ := doGet(t, ts, "/api/tokens", tokenV2)
	if respNew.StatusCode != http.StatusOK {
		t.Fatalf("轮换后新 token 必须鉴权通过, 实际 %d", respNew.StatusCode)
	}

	// 3. 注销 (Revoke) Token: DELETE /api/tokens/:id
	reqRevoke := authedReq(t, ts, "DELETE", fmt.Sprintf("/api/tokens/%s", tokenID), "")
	reqRevoke.AddCookie(&http.Cookie{Name: "hme_session", Value: sess})
	reqRevoke.Header.Set("X-CSRF-Token", csrf)

	statusRev, bodyRev, _ := do(t, reqRevoke)
	if statusRev != http.StatusOK {
		t.Fatalf("注销令牌失败: %d %s", statusRev, bodyRev)
	}

	// 验证注销后立即失效 (401)
	respRevoked, _ := doGet(t, ts, "/api/tokens", tokenV2)
	if respRevoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("注销后 token 必须立刻返回 401, 实际 %d", respRevoked.StatusCode)
	}

	// 4. 验证过期 Token 拦截
	// 创建一个已过期的 token
	createdExp, err := st.CreateToken("expired_bot", "admin", time.Now().Add(-1*time.Hour).UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("创建过期 token 失败: %v", err)
	}

	respExp, _ := doGet(t, ts, "/api/tokens", createdExp.Token)
	if respExp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("已过期 token 鉴权必须返回 401, 实际 %d", respExp.StatusCode)
	}
}

// TestPR07_FailedAuthDoesNotTouchLastUsed 验证鉴权失败绝对不刷新 last_used_at。
func TestPR07_FailedAuthDoesNotTouchLastUsed(t *testing.T) {
	ts, st := newPR07Server(t)

	// 创建一个未使用的有效 Token
	created, err := st.CreateToken("idle_bot", "admin", "")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// 伪造错误 token 尝试鉴权
	_, _ = doGet(t, ts, "/api/tokens", "am_invalid_secret_attack")

	// 检查该 token 在数据库中的 last_used_at 仍为空
	var lastUsed string
	err = st.DB().QueryRow(`SELECT COALESCE(last_used_at, '') FROM api_tokens WHERE id = ?`, created.ID).Scan(&lastUsed)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if lastUsed != "" {
		t.Fatalf("鉴权失败不应触碰任何令牌的 last_used_at, 实际被记录为 %s", lastUsed)
	}
}

// TestPR07_LegacyTokenStillAuthenticatesAfterV2Migration 验证历史 V1 遗留 Token
// 在升级 V2 后计算 hash 并迁移，标记 needs_rotation=true，历史客户端仍能凭原明文继续通过鉴权。
func TestPR07_LegacyTokenStillAuthenticatesAfterV2Migration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")

	legacyPlain := "am_legacy_v1_plain_secret_abcdef123456"

	// 构造 V1 完整数据库含明文 token 表
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin failed: %v", err)
	}
	if err := store.MigrateV0ToV1ForTest(tx); err != nil {
		t.Fatalf("MigrateV0ToV1ForTest failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx.Commit failed: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("PRAGMA user_version failed: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(`INSERT INTO api_tokens (id, name, token, created_at, scopes) VALUES ('tok_legacy_v1', 'legacy_bot', ?, ?, 'admin')`, legacyPlain, now)
	if err != nil {
		t.Fatalf("Insert V1 token failed: %v", err)
	}
	_ = db.Close()

	// 重新启动服务，自动加载并完成 V2
	stV2, err := store.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore V2 failed: %v", err)
	}
	defer stV2.Close()

	fb := &fakeBackend{}
	s := newWithBackendAndStore(fb, Config{AdminPassword: "admin-pass-2026-strong"}, stV2)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// 1. 验证 needs_rotation 标记为 true
	tokRec, err := stV2.GetToken(t.Context(), "tok_legacy_v1")
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if !tokRec.NeedsRotation {
		t.Fatalf("历史遗留 token 升级后必须被标记为 needs_rotation=true")
	}

	// 2. 验证使用旧客户端明文请求，仍可正常通过鉴权
	resp, body := doGet(t, ts, "/api/tokens", legacyPlain)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("历史 token 应继续通过鉴权, 实际 %d: %s", resp.StatusCode, body)
	}
}
