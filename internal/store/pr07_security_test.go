/**
 * [INPUT]: 依赖 bytes, context, database/sql, os, path/filepath, strings, testing, time, icloud-hme/internal/security, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 PR-07 核心安全测试用例：物理明文消亡、备份密文化、错误密钥 Fail-Closed、AAD 跨字段/账号防篡改、离线密钥轮换
 * [POS]: internal/store 的 PR-07 安全规格与威胁模型专项回归测试
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/security"
	_ "modernc.org/sqlite"
)

func genTestKey(b byte) []byte {
	k := make([]byte, security.KeySize)
	for i := range k {
		k[i] = b
	}
	return k
}

// TestPR07_MigratedDatabaseContainsNoPlaintextSecrets 验证迁移至 V2 且 VACUUM 之后，
// live 数据库物理文件的 raw bytes 中绝对不包含任何明文敏感 sentinel。
func TestPR07_MigratedDatabaseContainsNoPlaintextSecrets(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")

	const (
		sentinelToken       = "PLAINTEXT_SECRET_TOKEN_SENTINEL_998877"
		sentinelCookie      = "PLAINTEXT_COOKIE_VALUE_SENTINEL_112233"
		sentinelAppPass     = "PLAINTEXT_APP_PASSWORD_SENTINEL_445566"
		sentinelMailPass    = "PLAINTEXT_MAIL_PASSWORD_SENTINEL_778899"
		sentinelProxy       = "PLAINTEXT_PROXY_AUTH_SENTINEL_AABBCC"
		sentinelNotifyHook  = "https://feishu.example.com/hook/PLAINTEXT_NOTIFY_SENTINEL_DDEEFF"
	)

	// 1. 手工构造一个 V1 数据库 (含明文 token 列和明文凭据)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if err := migrateV0ToV1(tx); err != nil {
		t.Fatalf("migrateV0ToV1 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交 V1 初始化失败: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("设置 user_version 失败: %v", err)
	}

	// 插入带明文 sentinel 的数据
	cookiesJSON, _ := json.Marshal(map[string]string{"session": sentinelCookie})
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(`
		INSERT INTO accounts (id, name, real_email, cookies, app_password, mailbox, proxy, status, created_at, updated_at)
		VALUES ('acc_pr07_mig', 'Test Mig', 'mig@test.com', ?, ?, ?, ?, 'active', ?, ?)
	`, string(cookiesJSON), sentinelAppPass, sentinelMailPass, sentinelProxy, now, now)
	if err != nil {
		t.Fatalf("插入 V1 测试账号失败: %v", err)
	}

	_, err = db.Exec(`
		INSERT INTO api_tokens (id, name, token, created_at, scopes)
		VALUES ('tok_pr07_mig', 'Test Token', ?, ?, 'admin')
	`, sentinelToken, now)
	if err != nil {
		t.Fatalf("插入 V1 测试令牌失败: %v", err)
	}

	notifyJSON, _ := json.Marshal(map[string]interface{}{
		"feishu_webhook": sentinelNotifyHook,
	})
	_, err = db.Exec(`
		INSERT INTO settings (key, value, updated_at)
		VALUES ('notify_settings', ?, ?)
	`, string(notifyJSON), now)
	if err != nil {
		t.Fatalf("插入 V1 通知配置失败: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("关闭 V1 初始数据库失败: %v", err)
	}

	// 2. 使用 SecretCipher 启动 Store，触发 V1 -> V2 顺序迁移与 VACUUM
	key := genTestKey(0x11)
	cipher, err := security.NewSecretCipher(key)
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}

	st, err := NewStoreWithCipher(dir, cipher)
	if err != nil {
		t.Fatalf("NewStoreWithCipher: %v", err)
	}

	// 验证业务能通过解密读取数据
	acc, err := st.GetAccount("acc_pr07_mig")
	if err != nil {
		t.Fatalf("GetAccount failed: %v", err)
	}
	accCookies := UnmarshalCookies(acc.CookiesJSON)
	if accCookies["session"] != sentinelCookie {
		t.Fatalf("解密 cookies 不符: %v", accCookies)
	}
	if acc.AppPassword != sentinelAppPass {
		t.Fatalf("解密 app_password 不符: %v", acc.AppPassword)
	}

	// 验证 token 可以凭明文正常认证
	_, name, scopes, ok := st.ValidateTokenPrincipal(sentinelToken)
	if !ok || name != "Test Token" || scopes != "admin" {
		t.Fatalf("历史 token 认证失败: ok=%v, name=%s, scopes=%s", ok, name, scopes)
	}

	// 关闭 Store 以刷盘所有 WAL
	if err := st.Close(); err != nil {
		t.Fatalf("关闭 Store 失败: %v", err)
	}

	// 3. 读取磁盘数据库文件的原始物理字节流 (含 WAL / SHM 边车文件)，断言绝不包含明文 sentinel
	scanFiles := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
	}

	sentinels := []struct {
		name     string
		sentinel string
	}{
		{"API Token Plaintext", sentinelToken},
		{"Apple Cookie Sentinel", sentinelCookie},
		{"App Password Sentinel", sentinelAppPass},
		{"Mailbox Password Sentinel", sentinelMailPass},
		{"Proxy Auth Sentinel", sentinelProxy},
		{"Notify Webhook Sentinel", sentinelNotifyHook},
	}

	for _, fPath := range scanFiles {
		content, err := os.ReadFile(fPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("读取磁盘文件 %s 失败: %v", fPath, err)
		}
		for _, s := range sentinels {
			if bytes.Contains(content, []byte(s.sentinel)) {
				t.Fatalf("【安全红线踩雷】V2 物理文件 (%s) 中依然检测到明文残留: %s (%q)", filepath.Base(fPath), s.name, s.sentinel)
			}
		}
	}
}

// TestPR07_BackupContainsOnlyEncryptedCredentialsAndTokenHashes 验证生成的备份库只含密文和哈希。
func TestPR07_BackupContainsOnlyEncryptedCredentialsAndTokenHashes(t *testing.T) {
	dir := t.TempDir()
	key := genTestKey(0x22)
	cipher, _ := security.NewSecretCipher(key)

	st, err := NewStoreWithCipher(dir, cipher)
	if err != nil {
		t.Fatalf("NewStoreWithCipher: %v", err)
	}
	defer st.Close()

	// 创建测试账号
	now := time.Now().UTC().Format(time.RFC3339)
	err = st.SaveAccount(&AccountRecord{
		ID:          "acc_backup_test",
		Name:        "Backup Acc",
		RealEmail:   "bak@test.com",
		CookiesJSON: `{"cookie_key":"cookie_secret_123"}`,
		AppPassword: "app_secret_456",
		MailboxJSON: `{"mailbox":"mail_secret_789"}`,
		Proxy:       "http://user:proxy_secret_abc@proxy.com:8080",
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}

	// 创建 API Token
	created, err := st.CreateToken("BackupToken", "admin", "")
	if err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}

	// 保存通知配置
	notifyRaw := `{"feishu_webhook":"https://open.feishu.cn/open-apis/bot/v2/hook/secret-token-bak"}`
	if err := st.SaveEncryptedSetting("notify_settings", notifyRaw, security.NotifySettingsAAD()); err != nil {
		t.Fatalf("SaveEncryptedSetting failed: %v", err)
	}

	// 生成备份
	backupPath := filepath.Join(dir, "backup.db")
	if err := st.CreateBackup(context.Background(), backupPath); err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}

	// 打开备份库进行 raw SQL 检查
	bDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatalf("打开备份数据库失败: %v", err)
	}
	defer bDB.Close()

	// 1. accounts 密文检查
	var encCookies, encAppPass, encMailbox, encProxy string
	err = bDB.QueryRow(`SELECT cookies, app_password, mailbox, proxy FROM accounts WHERE id = 'acc_backup_test'`).Scan(
		&encCookies, &encAppPass, &encMailbox, &encProxy,
	)
	if err != nil {
		t.Fatalf("查询备份 accounts 失败: %v", err)
	}

	fields := map[string]string{
		"cookies":      encCookies,
		"app_password": encAppPass,
		"mailbox":      encMailbox,
		"proxy":        encProxy,
	}
	for name, val := range fields {
		if !strings.HasPrefix(val, security.EnvelopePrefixV1) {
			t.Fatalf("备份库中 %s 不是标准密文: %s", name, val)
		}
	}

	// 2. api_tokens 检查: 绝无 token 列，只有 token_hash
	hasToken, err := tableHasColumn(bDB, "api_tokens", "token")
	if err != nil {
		t.Fatalf("检查列失败: %v", err)
	}
	if hasToken {
		t.Fatalf("备份库中的 api_tokens 仍然存在 token 列")
	}

	var hash, prefix string
	err = bDB.QueryRow(`SELECT token_hash, token_prefix FROM api_tokens WHERE id = ?`, created.ID).Scan(&hash, &prefix)
	if err != nil {
		t.Fatalf("查询备份 token 失败: %v", err)
	}
	if hash != HashToken(created.Token) {
		t.Fatalf("备份中 token_hash 不一致: %s vs %s", hash, HashToken(created.Token))
	}

	// 3. settings 密文检查
	var encNotify string
	err = bDB.QueryRow(`SELECT value FROM settings WHERE key = 'notify_settings'`).Scan(&encNotify)
	if err != nil {
		t.Fatalf("查询备份 settings 失败: %v", err)
	}
	if !strings.HasPrefix(encNotify, security.EnvelopePrefixV1) {
		t.Fatalf("备份中 notify_settings 不是密文: %s", encNotify)
	}
}

// TestPR07_WrongMasterKeyFailsClosed 验证使用错误主密钥加载时，虽可打开 schema，
// 但在随后 GetAccount / ListAllAccounts 解密凭据时必须 Fail-Closed 绝不静默放行或返回脏数据。
// (在生产环境中，因 account.NewManager 启动时会预加载账号列表，故正常 server startup 亦会直接失败)。
func TestPR07_WrongMasterKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	keyA := genTestKey(0x33)
	keyB := genTestKey(0x44)

	cipherA, _ := security.NewSecretCipher(keyA)
	cipherB, _ := security.NewSecretCipher(keyB)

	// 1. 用 keyA 写入账号
	stA, err := NewStoreWithCipher(dir, cipherA)
	if err != nil {
		t.Fatalf("NewStoreWithCipher A failed: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	err = stA.SaveAccount(&AccountRecord{
		ID:          "acc_fail_closed",
		Name:        "Fail Closed Acc",
		RealEmail:   "fc@test.com",
		CookiesJSON: `{"foo":"bar"}`,
		AppPassword: "secret_password",
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}
	if err := stA.Close(); err != nil {
		t.Fatalf("Close A failed: %v", err)
	}

	// 2. 用 keyB 打开
	stB, err := NewStoreWithCipher(dir, cipherB)
	if err != nil {
		t.Fatalf("NewStoreWithCipher B failed: %v", err)
	}
	defer stB.Close()

	// 尝试解密读取: 必须明确报错，不可返回假成功或空数据
	_, err = stB.GetAccount("acc_fail_closed")
	if err == nil {
		t.Fatalf("使用错误密钥读取账号凭据应失败，但成功了")
	}

	_, err = stB.ListAllAccounts()
	if err == nil {
		t.Fatalf("使用错误密钥批量读取账号列表应失败，但成功了")
	}
}

// TestPR07_CiphertextCannotBeSwappedAcrossAccounts 验证 AAD 防篡改：
// 跨账号迁移密文或同账号跨列移动密文，认证加密解密必须立即失败。
func TestPR07_CiphertextCannotBeSwappedAcrossAccounts(t *testing.T) {
	dir := t.TempDir()
	key := genTestKey(0x55)
	cipher, _ := security.NewSecretCipher(key)

	st, err := NewStoreWithCipher(dir, cipher)
	if err != nil {
		t.Fatalf("NewStoreWithCipher failed: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	// 账号 1
	_ = st.SaveAccount(&AccountRecord{
		ID:          "acc_alice",
		Name:        "Alice",
		RealEmail:   "alice@test.com",
		CookiesJSON: `{"alice":"cookies"}`,
		AppPassword: "alice_password",
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	// 账号 2
	_ = st.SaveAccount(&AccountRecord{
		ID:          "acc_bob",
		Name:        "Bob",
		RealEmail:   "bob@test.com",
		CookiesJSON: `{"bob":"cookies"}`,
		AppPassword: "bob_password",
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	// 读取 alice 的 cookies 密文
	var aliceEncCookies string
	err = st.DB().QueryRow(`SELECT cookies FROM accounts WHERE id = 'acc_alice'`).Scan(&aliceEncCookies)
	if err != nil {
		t.Fatalf("QueryRow failed: %v", err)
	}

	// 篡改 1: 把 Alice 的 cookies 密文强行写入 Bob 的 cookies 列 (跨实体替换攻击)
	_, err = st.DB().Exec(`UPDATE accounts SET cookies = ? WHERE id = 'acc_bob'`, aliceEncCookies)
	if err != nil {
		t.Fatalf("UPDATE failed: %v", err)
	}

	// 此时读取 Bob: 必须 AAD 解密失败
	_, err = st.GetAccount("acc_bob")
	if err == nil {
		t.Fatalf("跨账号篡改密文后 GetAccount 应报错，但静默成功")
	}

	// 篡改 2: 把 Alice 的 cookies 密文强行写入 Alice 的 app_password 列 (同账号跨字段替换攻击)
	_, err = st.DB().Exec(`UPDATE accounts SET app_password = ? WHERE id = 'acc_alice'`, aliceEncCookies)
	if err != nil {
		t.Fatalf("UPDATE failed: %v", err)
	}

	// 此时读取 Alice: 必须 AAD 解密失败
	_, err = st.GetAccount("acc_alice")
	if err == nil {
		t.Fatalf("跨字段篡改密文后 GetAccount 应报错，但静默成功")
	}
}

// TestPR07_MasterKeyRotationRoundTrip 验证离线 Master Key 轮换工具 RotateCredentials。
func TestPR07_MasterKeyRotationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyOld := genTestKey(0x66)
	keyNew := genTestKey(0x77)

	cipherOld, _ := security.NewSecretCipher(keyOld)
	cipherNew, _ := security.NewSecretCipher(keyNew)

	// 1. 用 keyOld 初始化并写入数据
	stOld, err := NewStoreWithCipher(dir, cipherOld)
	if err != nil {
		t.Fatalf("NewStoreWithCipher old failed: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	err = stOld.SaveAccount(&AccountRecord{
		ID:          "acc_rot_test",
		Name:        "Rot Account",
		RealEmail:   "rot@test.com",
		CookiesJSON: `{"auth":"token_123"}`,
		AppPassword: "app_pass_rot",
		MailboxJSON: `{"mailbox":"mail_pass_rot"}`,
		Proxy:       "http://proxy_rot@proxy.com:8080",
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}

	notifyPlain := `{"feishu_webhook":"https://open.feishu.cn/hook/rot-test","bark_url":"https://api.day.app/rot-key"}`
	if err := stOld.SaveEncryptedSetting("notify_settings", notifyPlain, security.NotifySettingsAAD()); err != nil {
		t.Fatalf("SaveEncryptedSetting failed: %v", err)
	}
	if err := stOld.Close(); err != nil {
		t.Fatalf("Close old failed: %v", err)
	}

	// 2. 执行离线轮换
	if err := RotateCredentials(dir, cipherOld, cipherNew); err != nil {
		t.Fatalf("RotateCredentials failed: %v", err)
	}

	// 3. 用 keyNew 启动，验证全量凭据解密回读无损
	stNew, err := NewStoreWithCipher(dir, cipherNew)
	if err != nil {
		t.Fatalf("NewStoreWithCipher new failed: %v", err)
	}

	acc, err := stNew.GetAccount("acc_rot_test")
	if err != nil {
		t.Fatalf("新密钥读取账号失败: %v", err)
	}
	accCookies := UnmarshalCookies(acc.CookiesJSON)
	if accCookies["auth"] != "token_123" {
		t.Fatalf("Cookies 解密不匹配: %v", accCookies)
	}
	if acc.AppPassword != "app_pass_rot" {
		t.Fatalf("AppPassword 解密不匹配: %v", acc.AppPassword)
	}
	if acc.MailboxJSON != `{"mailbox":"mail_pass_rot"}` {
		t.Fatalf("MailboxJSON 解密不匹配: %v", acc.MailboxJSON)
	}
	if acc.Proxy != "http://proxy_rot@proxy.com:8080" {
		t.Fatalf("Proxy 解密不匹配: %v", acc.Proxy)
	}

	notifyDecrypted, err := stNew.GetEncryptedSetting("notify_settings", security.NotifySettingsAAD())
	if err != nil {
		t.Fatalf("新密钥读取通知配置失败: %v", err)
	}
	if notifyDecrypted != notifyPlain {
		t.Fatalf("通知配置解密不匹配: %s vs %s", notifyDecrypted, notifyPlain)
	}

	// 4. 验证用旧密钥启动无法再解密
	_ = stNew.Close()
	stOldAgain, err := NewStoreWithCipher(dir, cipherOld)
	if err != nil {
		t.Fatalf("NewStoreWithCipher old again failed: %v", err)
	}
	defer stOldAgain.Close()

	if _, err := stOldAgain.GetAccount("acc_rot_test"); err == nil {
		t.Fatalf("轮换后用旧密钥读取账号应失败，但成功了")
	}
}

// TestPR07_V2CleanupFailureDoesNotFinalizeSchemaVersion 验证 V1->V2 物理清理失败时不得标记 user_version=2，
// 且重启重试后能够安全完成并抹除磁盘物理明文碎片。
func TestPR07_V2CleanupFailureDoesNotFinalizeSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")

	const (
		sentinelCookie  = "SENTINEL_V1_COOKIE_FAIL_TEST_123"
		sentinelAppPass = "SENTINEL_V1_APP_PASS_FAIL_TEST_456"
		sentinelToken   = "am_sentinel_v1_token_fail_test_789"
	)

	// 1. 构造真实 V1 DB 并写入明文数据
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("创建数据库失败: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if err := MigrateV0ToV1ForTest(tx); err != nil {
		t.Fatalf("MigrateV0ToV1ForTest 失败: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	cookieJSON, _ := json.Marshal(map[string]string{"session": sentinelCookie})
	if _, err := tx.Exec(`
		INSERT INTO accounts (id, name, real_email, icloud_email, cookies, host, service_url, proxy, app_password, mailbox, status, created_at, updated_at)
		VALUES ('acc_v2_fail_test', 'Fail Test', 'fail@test.com', 'fail@icloud.com', ?, 'host.com', 'https://service', '', ?, '', 'active', ?, ?)
	`, string(cookieJSON), sentinelAppPass, now, now); err != nil {
		t.Fatalf("写入账号失败: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO api_tokens (id, name, token, created_at, scopes)
		VALUES ('tok_v2_fail_test', 'Fail Tok', ?, ?, 'admin')
	`, sentinelToken, now); err != nil {
		t.Fatalf("写入 token 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交 V1 事务失败: %v", err)
	}
	_ = db.Close()

	key := genTestKey(0x55)
	cipher, _ := security.NewSecretCipher(key)

	// 2. 注入 physical cleanup failure
	SetBeforeV2PhysicalCleanupHookForTest(func() error {
		return errors.New("simulated physical cleanup crash")
	})
	defer SetBeforeV2PhysicalCleanupHookForTest(nil)

	// 3. NewStoreWithCipher 必须失败
	stFail, err := NewStoreWithCipher(dir, cipher)
	if err == nil {
		stFail.Close()
		t.Fatalf("注入清理失败时 NewStoreWithCipher 应该报错，但返回了成功")
	}

	// 4. 检查：user_version 仍然是 1
	chkDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开检查 DB 失败: %v", err)
	}
	var v int
	if err := chkDB.QueryRow("PRAGMA user_version;").Scan(&v); err != nil {
		t.Fatalf("查询 user_version 失败: %v", err)
	}
	if v != 1 {
		t.Fatalf("物理清理失败时 user_version 绝不能变成 2，必须保持 1，当前为: %d", v)
	}
	_ = chkDB.Close()

	// 5. 移除 hook
	SetBeforeV2PhysicalCleanupHookForTest(nil)

	// 6. 再次 NewStoreWithCipher，必须幂等重试成功
	stSuccess, err := NewStoreWithCipher(dir, cipher)
	if err != nil {
		t.Fatalf("移除故障后重试 NewStoreWithCipher 失败: %v", err)
	}

	// 7. 检查 user_version == 2
	if err := stSuccess.db.QueryRow("PRAGMA user_version;").Scan(&v); err != nil || v != 2 {
		t.Fatalf("重试成功后 user_version 必须为 2，当前为: %d (err: %v)", v, err)
	}

	// 验证业务能通过解密读取数据
	acc, err := stSuccess.GetAccount("acc_v2_fail_test")
	if err != nil {
		t.Fatalf("重试后 GetAccount 失败: %v", err)
	}
	if acc.AppPassword != sentinelAppPass {
		t.Fatalf("重试后解密密码不符: %s", acc.AppPassword)
	}
	_, _, _, ok := stSuccess.ValidateTokenPrincipal(sentinelToken)
	if !ok {
		t.Fatalf("重试后 token 认证失败")
	}

	_ = stSuccess.Close()

	// 8. 扫描磁盘物理文件 (含 live DB, WAL, SHM)，明文 sentinel 绝不存在
	scanFiles := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
	}
	sentinels := []string{sentinelCookie, sentinelAppPass, sentinelToken}
	for _, fPath := range scanFiles {
		content, err := os.ReadFile(fPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("读取磁盘文件 %s 失败: %v", fPath, err)
		}
		for _, s := range sentinels {
			if bytes.Contains(content, []byte(s)) {
				t.Fatalf("物理文件 %s 中残留明文 sentinel: %q", filepath.Base(fPath), s)
			}
		}
	}
}

// TestPR07_MalformedTokenExpiryFailsClosed 验证畸形 expires_at 必须 Fail-Closed
func TestPR07_MalformedTokenExpiryFailsClosed(t *testing.T) {
	dir := t.TempDir()
	key := genTestKey(0x66)
	cipher, _ := security.NewSecretCipher(key)

	st, err := NewStoreWithCipher(dir, cipher)
	if err != nil {
		t.Fatalf("NewStoreWithCipher failed: %v", err)
	}
	defer st.Close()

	// 1. CreateToken 传入非法 expiresAt 必须直接报错
	if _, err := st.CreateToken("MalformedCreate", "admin", "not-a-valid-time"); err == nil {
		t.Fatalf("CreateToken 接收非法 expires_at 必须报错拒绝，但返回了成功")
	}

	// 2. 创建正常 Token
	created, err := st.CreateToken("ValidExpiryTok", "admin", time.Now().Add(24*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("CreateToken 失败: %v", err)
	}

	// 3. raw SQL 直接篡改 expires_at 为非法格式
	if _, err := st.db.Exec(`UPDATE api_tokens SET expires_at = 'not-a-valid-rfc3339', last_used_at = NULL WHERE id = ?`, created.ID); err != nil {
		t.Fatalf("篡改 expires_at 失败: %v", err)
	}

	// 4. ValidateTokenPrincipal 必须认证失败 (ok == false)
	_, _, _, ok := st.ValidateTokenPrincipal(created.Token)
	if ok {
		t.Fatalf("面对畸形 expires_at，ValidateTokenPrincipal 必须 Fail-Closed 判定为认证失败，但返回了 true")
	}

	// 5. last_used_at 不得更新
	var lastUsed sql.NullString
	if err := st.db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id = ?`, created.ID).Scan(&lastUsed); err != nil {
		t.Fatalf("查询 last_used_at 失败: %v", err)
	}
	if lastUsed.Valid && lastUsed.String != "" {
		t.Fatalf("认证失败不得推进 last_used_at，当前为: %s", lastUsed.String)
	}

	// 6. GetToken 面对畸形 expires_at 必须返回 error
	if _, err := st.GetToken(context.Background(), created.ID); err == nil {
		t.Fatalf("GetToken 面对畸形 expires_at 必须返回 error，但返回了 nil")
	}

	// 7. RotateToken 面对畸形 expires_at 必须拒绝轮换
	if _, err := st.RotateToken(created.ID); err == nil {
		t.Fatalf("RotateToken 面对畸形 expires_at 必须拒绝轮换，但返回了成功")
	}
}

// TestPR07_RotationCleanupFailureRollsBackToOldKey 验证当轮换事务 commit 后、物理清理失败时，
// 必须安全回滚到 pre-rotation 快照，旧 Master Key 依然为权威 key，新 key 不得成为权威 key。
func TestPR07_RotationCleanupFailureRollsBackToOldKey(t *testing.T) {
	dir := t.TempDir()
	keyA := genTestKey(0x77)
	keyB := genTestKey(0x88)
	cipherA, _ := security.NewSecretCipher(keyA)
	cipherB, _ := security.NewSecretCipher(keyB)

	// 1. key A 创建 DB 并写入凭据
	stA, err := NewStoreWithCipher(dir, cipherA)
	if err != nil {
		t.Fatalf("NewStoreWithCipher A failed: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	err = stA.SaveAccount(&AccountRecord{
		ID:          "acc_rot_rollback",
		Name:        "Rollback Account",
		RealEmail:   "rb@test.com",
		CookiesJSON: `{"token":"session_a_value"}`,
		AppPassword: "app_password_a",
		MailboxJSON: `{"pass":"mailbox_a"}`,
		Proxy:       "http://proxy_a@host:8080",
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}
	if err := stA.Close(); err != nil {
		t.Fatalf("Close stA failed: %v", err)
	}

	// 2. 设置物理清理 hook 注入 failure
	SetBeforeRotationPhysicalCleanupHookForTest(func() error {
		return errors.New("simulated rotation cleanup failure")
	})
	defer SetBeforeRotationPhysicalCleanupHookForTest(nil)

	// 3. 执行 RotateCredentials，必须返回 error
	err = RotateCredentials(dir, cipherA, cipherB)
	if err == nil {
		t.Fatalf("注入清理失败时 RotateCredentials 必须报错，但返回了 nil")
	}

	// 4. 用 key A 重新打开，必须能够正常读取全部凭据
	stAAgain, err := NewStoreWithCipher(dir, cipherA)
	if err != nil {
		t.Fatalf("回滚后用旧 key A 打开 Store 失败: %v", err)
	}
	defer stAAgain.Close()

	accA, err := stAAgain.GetAccount("acc_rot_rollback")
	if err != nil {
		t.Fatalf("回滚后旧 key A 读取账号失败: %v", err)
	}
	if accA.AppPassword != "app_password_a" {
		t.Fatalf("回滚后凭据内容受损: %s", accA.AppPassword)
	}

	// 5. 用 key B 打开读取必须失败 (key B 不得成为权威 key)
	_ = stAAgain.Close()
	stB, err := NewStoreWithCipher(dir, cipherB)
	if err != nil {
		t.Fatalf("用 key B 打开 Store 失败: %v", err)
	}
	defer stB.Close()

	if _, err := stB.GetAccount("acc_rot_rollback"); err == nil {
		t.Fatalf("回滚后 key B 不得成为权威 key，但读取成功了")
	}

	// 6. 验证 pre-rotation backup 依然完好保留
	entries, err := os.ReadDir(filepath.Join(dir, "backups"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("pre-rotation backup 必须保留，未找到备份文件: %v", err)
	}
}

// TestPR07_RotationRemovesOldCiphertextFromLiveFiles 验证轮换成功后，旧 ciphertext 完全从 live 文件中消除
func TestPR07_RotationRemovesOldCiphertextFromLiveFiles(t *testing.T) {
	dir := t.TempDir()
	keyA := genTestKey(0x99)
	keyB := genTestKey(0xAA)
	cipherA, _ := security.NewSecretCipher(keyA)
	cipherB, _ := security.NewSecretCipher(keyB)

	// 1. key A 写 credential sentinel
	stA, err := NewStoreWithCipher(dir, cipherA)
	if err != nil {
		t.Fatalf("NewStoreWithCipher A failed: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	err = stA.SaveAccount(&AccountRecord{
		ID:          "acc_rot_clean",
		Name:        "Clean Account",
		RealEmail:   "clean@test.com",
		CookiesJSON: `{"token":"sentinel_cookie_value_for_rotation_cleanup"}`,
		AppPassword: "app_pass_sentinel_cleanup",
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}

	// 2. raw SQL 读取轮换前的旧 ciphertext envelope
	var oldCookiesCiphertext string
	if err := stA.db.QueryRow("SELECT cookies FROM accounts WHERE id = 'acc_rot_clean'").Scan(&oldCookiesCiphertext); err != nil {
		t.Fatalf("读取旧 cookies 密文失败: %v", err)
	}
	if !strings.HasPrefix(oldCookiesCiphertext, security.EnvelopePrefixV1) {
		t.Fatalf("旧 cookies 不是 enc:v1 密文: %s", oldCookiesCiphertext)
	}

	if err := stA.Close(); err != nil {
		t.Fatalf("Close stA failed: %v", err)
	}

	// 3. 执行 RotateCredentials A -> B
	if err := RotateCredentials(dir, cipherA, cipherB); err != nil {
		t.Fatalf("RotateCredentials failed: %v", err)
	}

	// 4. 扫描 live 物理文件 (排除 backups 目录)
	liveFiles := []string{
		filepath.Join(dir, "icloud_hme.db"),
		filepath.Join(dir, "icloud_hme.db-wal"),
		filepath.Join(dir, "icloud_hme.db-shm"),
	}

	for _, fPath := range liveFiles {
		content, err := os.ReadFile(fPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("读取磁盘文件 %s 失败: %v", fPath, err)
		}
		if bytes.Contains(content, []byte(oldCookiesCiphertext)) {
			t.Fatalf("【安全红线踩雷】轮换后的 live 文件 %s 中依然残留旧 key 密文 envelope: %s", filepath.Base(fPath), oldCookiesCiphertext)
		}
	}
}

// TestPR07_RotationRejectsSameMasterKey 验证当新旧密钥相同时，RotateCredentials 必须明确拒绝
func TestPR07_RotationRejectsSameMasterKey(t *testing.T) {
	dir := t.TempDir()
	key := genTestKey(0xBB)
	cipher1, _ := security.NewSecretCipher(key)
	cipher2, _ := security.NewSecretCipher(key)

	err := RotateCredentials(dir, cipher1, cipher2)
	if err == nil {
		t.Fatalf("新旧密钥相同时 RotateCredentials 应该拒绝，但返回成功")
	}
	if !strings.Contains(err.Error(), "new master key must differ from current master key") {
		t.Fatalf("期望错误提示 'new master key must differ from current master key'，实际为: %v", err)
	}
}

