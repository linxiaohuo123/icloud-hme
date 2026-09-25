/**
 * [INPUT]: 依赖 bytes, context, database/sql, encoding/json, errors, fmt, os, path/filepath, strings, testing, time, icloud-hme/internal/hme, icloud-hme/internal/security, icloud-hme/internal/store
 * [OUTPUT]: 提供 TestPR06 与 PR-09 备份不迁移源库、V1 备份恢复至 V2、错误密钥回滚与受保护凭据验证单测
 * [POS]: internal/store 的备份与离线恢复一致性单测套件 (PR-09)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/security"
)

// TestPR06_BackupIncludesCommittedWALData 验证在 WAL 模式下无需手动 checkpoint，CreateBackup 也能捕获最新 committed 事务数据
func TestPR06_BackupIncludesCommittedWALData(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	// 确认 WAL 模式
	var journalMode string
	if err := st.db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil || journalMode != "wal" {
		t.Fatalf("数据库未处于 WAL 模式: mode=%s, err=%v", journalMode, err)
	}

	ctx := context.Background()

	// 写入 committed 数据 (令牌与设置)
	err = st.SaveToken(APIToken{
		ID:        "tok_wal_1",
		Name:      "wal_bot",
		Token:     "sec_wal_test",
		CreatedAt: "2026-09-24T00:00:00Z",
		Scopes:    "allocate",
	})
	if err != nil {
		t.Fatalf("SaveToken failed: %v", err)
	}

	if err := st.SaveSetting("wal_key", "wal_value"); err != nil {
		t.Fatalf("SaveSetting failed: %v", err)
	}

	// 严禁依赖手工 checkpoint，直接执行 CreateBackup
	backupPath := filepath.Join(dir, "wal_backup.db")
	if err := st.CreateBackup(ctx, backupPath); err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}

	// 校验备份文件只读打开
	bakDB, err := sql.Open("sqlite", filepath.ToSlash(backupPath)+"?mode=ro")
	if err != nil {
		t.Fatalf("打开备份文件失败: %v", err)
	}
	defer bakDB.Close()

	// 检查 quick_check
	var qc string
	if err := bakDB.QueryRow("PRAGMA quick_check;").Scan(&qc); err != nil || qc != "ok" {
		t.Fatalf("备份 quick_check 失败: %s (%v)", qc, err)
	}

	// 检查 committed 数据必须存在
	var tokName, tokHash, scopes string
	err = bakDB.QueryRow("SELECT name, token_hash, scopes FROM api_tokens WHERE id='tok_wal_1'").Scan(&tokName, &tokHash, &scopes)
	if err != nil {
		t.Fatalf("未能在备份中找到刚写入的 WAL committed 数据: %v", err)
	}
	if tokName != "wal_bot" || tokHash != HashToken("sec_wal_test") || scopes != "allocate" {
		t.Fatalf("WAL 备份中数据字段不匹配: %s, %s, %s", tokName, tokHash, scopes)
	}

	var settingVal string
	if err := bakDB.QueryRow("SELECT value FROM settings WHERE key='wal_key'").Scan(&settingVal); err != nil || settingVal != "wal_value" {
		t.Fatalf("备份中 settings 丢失或不匹配: %s (err=%v)", settingVal, err)
	}
}

// TestPR06_BackupPreservesCriticalState 验证备份快照对关键业务状态（包括高敏凭据、Cookie、Token、MagicLink、幂等日志等）逐字段保持一致
func TestPR06_BackupPreservesCriticalState(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// 1. Account 包含明文 Cookie, app_password, mailbox
	accRecord := &AccountRecord{
		ID:            "acc_crit_1",
		Name:          "Critical Account",
		RealEmail:     "crit_real@icloud.com",
		ICloudEmail:   "crit_cloud@icloud.com",
		CookiesJSON:   `{"my_apple_cookie":"secret_session_token_xyz"}`,
		Host:          "p100-mail.icloud.com",
		ServiceURL:    "https://setup.icloud.com/hme/v1",
		Proxy:         "socks5://127.0.0.1:1080",
		AppPassword:   "abcd-efgh-ijkl-mnop",
		MailboxJSON:   `{"provider":"163","email":"crit@163.com","imap_host":"imap.163.com","imap_port":993,"password":"mailbox_secret_pwd"}`,
		Status:        "active",
		AliasTotal:    12,
		AliasActive:   10,
		LastValidated: "2026-09-24T12:00:00Z",
		LastError:     "",
		CreatedAt:     "2026-09-20T00:00:00Z",
		TagsJSON:      `["vip","prod"]`,
		UpdatedAt:     "2026-09-24T12:00:00Z",
	}
	if err := st.SaveAccount(accRecord); err != nil {
		t.Fatalf("SaveAccount failed: %v", err)
	}

	// 2. Token
	if err := st.SaveToken(APIToken{
		ID:        "tok_crit_1",
		Name:      "crit_token",
		Token:     "api_token_secret_9999",
		CreatedAt: "2026-09-24T00:00:00Z",
		Scopes:    "admin,allocate",
	}); err != nil {
		t.Fatalf("SaveToken failed: %v", err)
	}

	// 3. Inventory
	if err := st.AddInventoryAlias("acc_crit_1", hme.Alias{
		Email:       "crit_alias@icloud.com",
		AnonymousID: "anon_alias_id_123",
		Active:      true,
	}, "replenish", true); err != nil {
		t.Fatalf("AddInventoryAlias failed: %v", err)
	}

	// 4. Allocation
	if _, err := st.RecordAllocation(&AliasAllocation{
		AllocationID: "alloc_crit_1",
		AliasEmail:   "crit_alias@icloud.com",
		AccountID:    "acc_crit_1",
		OwnerKind:    "token",
		OwnerID:      "tok_crit_1",
		BusinessTag:  "tag_crit",
		AllocatedAt:  "2026-09-24T00:00:00Z",
		Status:       "allocated",
	}, "crit_token"); err != nil {
		t.Fatalf("RecordAllocation failed: %v", err)
	}

	// 5. Operation with request_hash
	if _, err := st.db.Exec(`
		INSERT INTO operations (
			operation_id, principal_kind, principal_id, operation_kind, idempotency_key, request_hash, state, candidate_email, result_ref, error_code, created_at, updated_at
		) VALUES (
			'op_crit_1', 'token', 'tok_crit_1', 'allocate', 'idem_key_1', 'hash_sha256_abcdef', 'pending', 'crit_alias@icloud.com', '', '', '2026-09-24T00:00:00Z', '2026-09-24T00:00:00Z'
		)
	`); err != nil {
		t.Fatalf("Insert operation failed: %v", err)
	}

	// 6. Reserve Intent
	if _, err := st.db.Exec(`
		INSERT INTO hme_reserve_intents (
			intent_id, account_id, candidate_email, label, state, anonymous_id, result_ref, error_message, created_at, updated_at
		) VALUES (
			'intent_crit_1', 'acc_crit_1', 'crit_cand@icloud.com', 'label_test', 'pending', 'anon_1', '', '', '2026-09-24T00:00:00Z', '2026-09-24T00:00:00Z'
		)
	`); err != nil {
		t.Fatalf("Insert reserve intent failed: %v", err)
	}

	// 7. Verification Request with code & magic_link
	err = st.CreateVerificationRequest(ctx, &VerificationRequest{
		RequestID:           "vreq_crit_1",
		PrincipalKind:       "token",
		PrincipalID:         "tok_crit_1",
		LeaseID:             "alloc_crit_1",
		AliasEmail:          "crit_alias@icloud.com",
		Status:              "succeeded",
		CreatedAt:           "2026-09-24T00:00:00Z",
		ExpiresAt:           "2026-09-25T00:00:00Z",
		BaselineProvider:    "163",
		BaselineMailbox:     "INBOX",
		BaselineUIDValidity: 12345,
		BaselineUID:         67890,
		MatchedEventRef:     "event_msg_ref_1",
		Code:                "987654",
		MagicLink:           "https://verify.apple.com/auth?token=magic_link_xyz",
	})
	if err != nil {
		t.Fatalf("CreateVerificationRequest failed: %v", err)
	}

	// 执行 CreateBackup
	backupPath := filepath.Join(dir, "critical_state.db")
	if err := st.CreateBackup(ctx, backupPath); err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}

	// 打开 backup 逐字段比较
	bakDB, err := sql.Open("sqlite", filepath.ToSlash(backupPath)+"?mode=ro")
	if err != nil {
		t.Fatalf("Open backup failed: %v", err)
	}
	defer bakDB.Close()

	// 校验 Account
	var accOut AccountRecord
	err = bakDB.QueryRow(`
		SELECT id, name, real_email, icloud_email, cookies, host, service_url, proxy, app_password, mailbox, status, alias_total, alias_active, last_validated, last_error, created_at, tags, updated_at
		FROM accounts WHERE id='acc_crit_1'
	`).Scan(
		&accOut.ID, &accOut.Name, &accOut.RealEmail, &accOut.ICloudEmail, &accOut.CookiesJSON, &accOut.Host,
		&accOut.ServiceURL, &accOut.Proxy, &accOut.AppPassword, &accOut.MailboxJSON, &accOut.Status,
		&accOut.AliasTotal, &accOut.AliasActive, &accOut.LastValidated, &accOut.LastError, &accOut.CreatedAt,
		&accOut.TagsJSON, &accOut.UpdatedAt,
	)
	if err != nil {
		t.Fatalf("查询备份中 accounts 失败: %v", err)
	}
	// 验证备份库中敏感字段已完成 AES-256-GCM 加密，绝不存在明文泄漏
	if !security.IsEncrypted(accOut.CookiesJSON) {
		t.Fatalf("备份中 Cookie 必须为 enc:v1 密文，当前为明文: %s", accOut.CookiesJSON)
	}
	if !security.IsEncrypted(accOut.AppPassword) {
		t.Fatalf("备份中 AppPassword 必须为 enc:v1 密文，当前为明文: %s", accOut.AppPassword)
	}
	if !security.IsEncrypted(accOut.MailboxJSON) {
		t.Fatalf("备份中 Mailbox 必须为 enc:v1 密文，当前为明文: %s", accOut.MailboxJSON)
	}

	testCipher := st.Cipher()

	decCookies, err := testCipher.Decrypt(accOut.CookiesJSON, security.AccountAAD("acc_crit_1", "cookies"))
	if err != nil || string(decCookies) != accRecord.CookiesJSON {
		t.Fatalf("解密备份中 Cookie 失败或内容不符: %v", err)
	}

	decAppPass, err := testCipher.Decrypt(accOut.AppPassword, security.AccountAAD("acc_crit_1", "app_password"))
	if err != nil || string(decAppPass) != accRecord.AppPassword {
		t.Fatalf("解密备份中 AppPassword 失败或内容不符: %v", err)
	}

	decMailbox, err := testCipher.Decrypt(accOut.MailboxJSON, security.AccountAAD("acc_crit_1", "mailbox"))
	if err != nil || string(decMailbox) != accRecord.MailboxJSON {
		t.Fatalf("解密备份中 Mailbox 失败或内容不符: %v", err)
	}

	// 校验 Token: 必须为不可逆 token_hash
	var tokHash, tokScopes string
	if err := bakDB.QueryRow("SELECT token_hash, scopes FROM api_tokens WHERE id='tok_crit_1'").Scan(&tokHash, &tokScopes); err != nil {
		t.Fatalf("查询备份中 api_tokens 失败: %v", err)
	}
	if tokHash != HashToken("api_token_secret_9999") || tokScopes != "admin,allocate" {
		t.Fatalf("Token 关键字段不匹配: hash=%s, scopes=%s", tokHash, tokScopes)
	}

	// 校验 Inventory
	var invAccount, invProviderID string
	if err := bakDB.QueryRow("SELECT account_id, provider_alias_id FROM alias_inventory WHERE email='crit_alias@icloud.com'").Scan(&invAccount, &invProviderID); err != nil {
		t.Fatalf("查询备份中 alias_inventory 失败: %v", err)
	}
	if invAccount != "acc_crit_1" || invProviderID != "anon_alias_id_123" {
		t.Fatalf("Inventory 关键字段不匹配: acc=%s, prov=%s", invAccount, invProviderID)
	}

	// 校验 Allocation
	var allocOwnerKind, allocOwnerID string
	if err := bakDB.QueryRow("SELECT owner_kind, owner_id FROM alias_allocations WHERE allocation_id='alloc_crit_1'").Scan(&allocOwnerKind, &allocOwnerID); err != nil {
		t.Fatalf("查询备份中 alias_allocations 失败: %v", err)
	}
	if allocOwnerKind != "token" || allocOwnerID != "tok_crit_1" {
		t.Fatalf("Allocation 关键字段不匹配: kind=%s, id=%s", allocOwnerKind, allocOwnerID)
	}

	// 校验 Operation
	var opHash, opState string
	if err := bakDB.QueryRow("SELECT request_hash, state FROM operations WHERE operation_id='op_crit_1'").Scan(&opHash, &opState); err != nil {
		t.Fatalf("查询备份中 operations 失败: %v", err)
	}
	if opHash != "hash_sha256_abcdef" || opState != "pending" {
		t.Fatalf("Operation 关键字段不匹配: hash=%s, state=%s", opHash, opState)
	}

	// 校验 Reserve Intent
	var intentAccount, intentCand string
	if err := bakDB.QueryRow("SELECT account_id, candidate_email FROM hme_reserve_intents WHERE intent_id='intent_crit_1'").Scan(&intentAccount, &intentCand); err != nil {
		t.Fatalf("查询备份中 hme_reserve_intents 失败: %v", err)
	}
	if intentAccount != "acc_crit_1" || intentCand != "crit_cand@icloud.com" {
		t.Fatalf("Reserve Intent 关键字段不匹配: acc=%s, cand=%s", intentAccount, intentCand)
	}

	// 校验 Verification Request
	var vCode, vMagicLink, vStatus string
	var vUIDValidity int64
	if err := bakDB.QueryRow("SELECT code, magic_link, status, baseline_uidvalidity FROM verification_requests WHERE request_id='vreq_crit_1'").Scan(&vCode, &vMagicLink, &vStatus, &vUIDValidity); err != nil {
		t.Fatalf("查询备份中 verification_requests 失败: %v", err)
	}
	if vCode != "987654" || vMagicLink != "https://verify.apple.com/auth?token=magic_link_xyz" || vStatus != "succeeded" || vUIDValidity != 12345 {
		t.Fatalf("VerificationRequest 关键字段不匹配: code=%s, magic=%s, status=%s, uidv=%d", vCode, vMagicLink, vStatus, vUIDValidity)
	}
}

// TestPR06_RestoreRoundTrip 验证完整离线备份与恢复全流程链路
func TestPR06_RestoreRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	st, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	ctx := context.Background()

	// 1. 写入业务状态
	if err := st.SaveToken(APIToken{
		ID:        "tok_round_1",
		Name:      "round_bot",
		Token:     "sec_round_123",
		CreatedAt: "2026-09-24T00:00:00Z",
		Scopes:    "allocate",
	}); err != nil {
		t.Fatalf("SaveToken failed: %v", err)
	}

	if err := st.SaveSetting("k_round", "v_round_original"); err != nil {
		t.Fatalf("SaveSetting failed: %v", err)
	}

	// 2. 导出备份
	backupDir := t.TempDir()
	backupPath := filepath.Join(backupDir, "offline_backup.db")
	if err := st.CreateBackup(ctx, backupPath); err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}

	// 3. 关闭 Store (Restore 要求服务已停止)
	if err := st.Close(); err != nil {
		t.Fatalf("Close store failed: %v", err)
	}

	// 4. 模拟运行环境数据被篡改与污染
	corruptDB, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dataDir, "icloud_hme.db")))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = corruptDB.Exec("UPDATE settings SET value='tampered_value' WHERE key='k_round';")
	_, _ = corruptDB.Exec("DELETE FROM api_tokens;")
	corruptDB.Close()

	// 5. 执行离线 Restore
	if err := RestoreDatabase(ctx, dataDir, backupPath); err != nil {
		t.Fatalf("RestoreDatabase failed: %v", err)
	}

	// 6. 重新以正常 NewStore 打开恢复后的数据库
	reopenedStore, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("重新打开恢复后的 Store 失败: %v", err)
	}
	defer reopenedStore.Close()

	// 7. 验证原状态完全恢复
	tokens := reopenedStore.ListTokens()
	if len(tokens) != 1 || tokens[0].ID != "tok_round_1" || !reopenedStore.ValidateToken("sec_round_123") {
		t.Fatalf("恢复后 Token 状态未正确还原: %+v", tokens)
	}

	val := reopenedStore.GetSetting("k_round")
	if val != "v_round_original" {
		t.Fatalf("恢复后 Setting 值未正确还原: 期望 'v_round_original', 实际: '%s'", val)
	}

	// 8. 验证 pre-restore 备份存在
	backupsEntries, err := os.ReadDir(filepath.Join(dataDir, "backups"))
	if err != nil || len(backupsEntries) == 0 {
		t.Fatalf("期望保留 pre-restore 备份快照, 但目录为空: %v", err)
	}
	foundPreRestore := false
	for _, entry := range backupsEntries {
		if strings.HasPrefix(entry.Name(), "pre-restore-") {
			foundPreRestore = true
			break
		}
	}
	if !foundPreRestore {
		t.Fatalf("未找到 pre-restore-*.db 快照文件")
	}
}

// TestPR06_CorruptBackupCannotReplaceLiveDatabase 验证损坏的备份绝不能覆盖现存数据库
func TestPR06_CorruptBackupCannotReplaceLiveDatabase(t *testing.T) {
	dataDir := t.TempDir()
	st, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	// 写入现有正常数据
	if err := st.SaveSetting("important_key", "healthy_live_data"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// 构造一个物理损坏的备份文件
	corruptDir := t.TempDir()
	corruptBackupPath := filepath.Join(corruptDir, "corrupt_backup.db")
	corruptBytes := make([]byte, 4096)
	copy(corruptBytes, []byte("SQLite format 3\x00"))
	for i := 16; i < len(corruptBytes); i++ {
		corruptBytes[i] = 0xee
	}
	if err := os.WriteFile(corruptBackupPath, corruptBytes, 0600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 尝试执行 Restore，期望必须失败
	err = RestoreDatabase(ctx, dataDir, corruptBackupPath)
	if err == nil {
		t.Fatalf("RestoreDatabase 面对损坏备份未报错，安全门禁失效")
	}

	// 重新打开现有 Store，验证 live 数据原封不动
	reopenedStore, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("恢复失败后现有数据未能保持可用: %v", err)
	}
	defer reopenedStore.Close()

	val := reopenedStore.GetSetting("important_key")
	if val != "healthy_live_data" {
		t.Fatalf("现有 live 数据遭到破坏: 实际值: %s", val)
	}
}

// TestPR06_FutureBackupRejected 验证版本高于当前版本的备份被明确拒绝恢复
func TestPR06_FutureBackupRejected(t *testing.T) {
	dataDir := t.TempDir()
	st, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	st.Close()

	// 构造一个未来版本的备份文件 (user_version = 99)
	futureDir := t.TempDir()
	futurePath := filepath.Join(futureDir, "future_backup.db")
	futureDB, err := sql.Open("sqlite", filepath.ToSlash(futurePath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := futureDB.Exec("CREATE TABLE test (id INT); PRAGMA user_version = 99;"); err != nil {
		t.Fatal(err)
	}
	_ = futureDB.Close()

	ctx := context.Background()

	// 尝试恢复未来版本备份，期望被拒绝
	err = RestoreDatabase(ctx, dataDir, futurePath)
	if err == nil {
		t.Fatalf("RestoreDatabase 未能拒绝未来版本的备份 (version 99)")
	}
	if !strings.Contains(err.Error(), "newer than supported version") {
		t.Fatalf("错误信息未包含指定契约 'newer than supported version': %v", err)
	}
}

// TestPR06_RestoreRefusesWhileDatabaseInUse 验证运行中的数据库禁止执行 Restore
func TestPR06_RestoreRefusesWhileDatabaseInUse(t *testing.T) {
	dataDir := t.TempDir()
	st, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer st.Close()

	if err := st.SaveSetting("critical_key", "live_data"); err != nil {
		t.Fatalf("SaveSetting failed: %v", err)
	}

	// 制作一个合法的待恢复备份
	backupDir := t.TempDir()
	validBackup := filepath.Join(backupDir, "valid_backup.db")
	backupStore, err := NewStore(filepath.Join(backupDir, "src"))
	if err != nil {
		t.Fatalf("backupStore failed: %v", err)
	}
	if err := backupStore.SaveSetting("critical_key", "backup_data"); err != nil {
		t.Fatalf("backupStore save setting failed: %v", err)
	}
	ctx := context.Background()
	if err := backupStore.CreateBackup(ctx, validBackup); err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	backupStore.Close()

	// 2. 保持 st 不关闭，直接调用 RestoreDatabase
	err = RestoreDatabase(ctx, dataDir, validBackup)
	if err == nil {
		t.Fatalf("预期 RestoreDatabase 在数据库被占用时报错，但返回了 nil")
	}
	// 4. 必须返回 database in use
	if !strings.Contains(err.Error(), "database is in use") {
		t.Fatalf("预期包含 'database is in use'，实际得到: %v", err)
	}

	// 5. live DB 数据完全不变
	if val := st.GetSetting("critical_key"); val != "live_data" {
		t.Fatalf("live DB 数据被篡改: 期望 live_data，实际: %s", val)
	}

	// 6. backup 也完全不变
	bakDB, err := sql.Open("sqlite", filepath.ToSlash(validBackup)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var bakVal string
	if err := bakDB.QueryRow("SELECT value FROM settings WHERE key = 'critical_key'").Scan(&bakVal); err != nil || bakVal != "backup_data" {
		_ = bakDB.Close()
		t.Fatalf("backup 文件被意外修改: %s (%v)", bakVal, err)
	}
	_ = bakDB.Close()

	// 7. Close Store
	if err := st.Close(); err != nil {
		t.Fatalf("st.Close failed: %v", err)
	}

	// 8. 再次 Restore 才允许成功 (不要 sleep)
	if err := RestoreDatabase(ctx, dataDir, validBackup); err != nil {
		t.Fatalf("Store 关闭后再次 RestoreDatabase 失败: %v", err)
	}

	// 校验恢复后的结果
	reopened, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("恢复后打开 Store 失败: %v", err)
	}
	defer reopened.Close()
	if val := reopened.GetSetting("critical_key"); val != "backup_data" {
		t.Fatalf("恢复后数据不符合预期: 期望 backup_data，实际: %s", val)
	}
}

// TestPR06_PreRestoreBackupFailureLeavesLiveDatabaseUntouched 验证前置备份失败时立即 fail closed，不破坏现有库
func TestPR06_PreRestoreBackupFailureLeavesLiveDatabaseUntouched(t *testing.T) {
	dataDir := t.TempDir()
	st, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	if err := st.SaveSetting("critical_key", "original"); err != nil {
		t.Fatalf("SaveSetting failed: %v", err)
	}
	st.Close()

	// 2. 创建有效 restore backup: critical_key=backup
	backupDir := t.TempDir()
	validBackup := filepath.Join(backupDir, "valid.db")
	backupStore, err := NewStore(filepath.Join(backupDir, "src"))
	if err != nil {
		t.Fatalf("backupStore init failed: %v", err)
	}
	if err := backupStore.SaveSetting("critical_key", "backup"); err != nil {
		t.Fatalf("backupStore save setting failed: %v", err)
	}
	ctx := context.Background()
	if err := backupStore.CreateBackup(ctx, validBackup); err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	backupStore.Close()

	// 3. 在 pre-restore backup 前确定性注入 error
	injectedErr := errors.New("injected pre-restore backup failure")
	SetBeforePreRestoreBackupHookForTest(func() error {
		return injectedErr
	})
	defer SetBeforePreRestoreBackupHookForTest(nil)

	// 4. RestoreDatabase 必须失败
	err = RestoreDatabase(ctx, dataDir, validBackup)
	if err == nil {
		t.Fatalf("预期 RestoreDatabase 注入失败，但返回成功")
	}

	// 5. 重新 NewStore
	reopened, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("重新打开 Store 失败: %v", err)
	}
	defer reopened.Close()

	// 6. critical_key 仍严格为 original
	// 7. 没有 swap，8. 没有删除 live data，9. 不应留下假成功的 restore
	if val := reopened.GetSetting("critical_key"); val != "original" {
		t.Fatalf("critical_key 未能保持 original: 得到 %s", val)
	}
}

// TestPR06_PostSwapValidationFailureRestoresExactPreRestoreState 验证 swap 后验证失败必须从权威一致性快照回滚
func TestPR06_PostSwapValidationFailureRestoresExactPreRestoreState(t *testing.T) {
	dataDir := t.TempDir()
	st, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	if err := st.SaveSetting("critical_key", "original"); err != nil {
		t.Fatalf("SaveSetting failed: %v", err)
	}
	st.Close()

	// 2. 创建有效 restore backup: critical_key=new
	backupDir := t.TempDir()
	validBackup := filepath.Join(backupDir, "valid_new.db")
	backupStore, err := NewStore(filepath.Join(backupDir, "src"))
	if err != nil {
		t.Fatalf("backupStore init failed: %v", err)
	}
	if err := backupStore.SaveSetting("critical_key", "new"); err != nil {
		t.Fatalf("backupStore save setting failed: %v", err)
	}
	ctx := context.Background()
	if err := backupStore.CreateBackup(ctx, validBackup); err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	backupStore.Close()

	// 3. 使用 hook 在新 DB 已 swap 后、NewStore 验证前注入失败
	injectedErr := errors.New("injected post-swap validation failure")
	SetBeforeRestoredDatabaseValidationHookForTest(func() error {
		return injectedErr
	})
	defer SetBeforeRestoredDatabaseValidationHookForTest(nil)

	// 4. 调用 Restore，预期返回 error
	err = RestoreDatabase(ctx, dataDir, validBackup)
	if err == nil {
		t.Fatalf("预期 RestoreDatabase 失败，但返回成功")
	}

	// 5. 随后重新 NewStore
	reopened, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("重新打开 Store 失败: %v", err)
	}
	defer reopened.Close()

	// 6. key 必须仍为 original
	if val := reopened.GetSetting("critical_key"); val != "original" {
		t.Fatalf("回滚后数据未恢复为 original: 得到 %s", val)
	}

	// 7. 且 pre-restore snapshot 存在
	backupsDir := filepath.Join(dataDir, "backups")
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		t.Fatalf("读取 backups 目录失败: %v", err)
	}
	foundPreRestore := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pre-restore-") && strings.HasSuffix(entry.Name(), ".db") {
			foundPreRestore = true
			break
		}
	}
	if !foundPreRestore {
		t.Fatalf("未找到 pre-restore-*.db 快照文件")
	}
}

// TestPR09_BackupDoesNotMigrateSourceDatabase 验证 offline backup API 绝不修改源数据库，
// 保持 user_version=1、原始 schema 与明文数据不发生任何迁移变更，且备份文件物理完整。
func TestPR09_BackupDoesNotMigrateSourceDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "icloud_hme.db")

	const (
		sentinelToken   = "PLAINTEXT_LEGACY_TOKEN_PR09_112233"
		sentinelCookie  = "PLAINTEXT_COOKIE_PR09_445566"
		sentinelAppPass = "PLAINTEXT_APP_PASSWORD_PR09_778899"
		sentinelMailbox = "PLAINTEXT_MAILBOX_PR09_AABBCC"
		sentinelProxy   = "PLAINTEXT_PROXY_PR09_DDEEFF"
	)

	// 1. 构造真实 V1 DB: user_version = 1
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
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
	if _, err := db.Exec("PRAGMA user_version = 1;"); err != nil {
		t.Fatalf("设置 user_version 失败: %v", err)
	}

	// 插入 plaintext legacy API token (包含明文 token 列)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`
		INSERT INTO api_tokens (id, name, token, created_at, scopes)
		VALUES ('tok_v1_plain', 'v1_token', ?, ?, 'admin');
	`, sentinelToken, now); err != nil {
		t.Fatalf("插入明文 token 失败: %v", err)
	}

	// 插入 plaintext account credential (明文 cookies, app_password, mailbox, proxy)
	cookiesJSON, _ := json.Marshal(map[string]string{"session": sentinelCookie})
	if _, err := db.Exec(`
		INSERT INTO accounts (id, name, real_email, cookies, app_password, mailbox, proxy, status, created_at, updated_at)
		VALUES ('acc_v1_plain', 'Plain Account', 'plain@example.com', ?, ?, ?, ?, 'active', ?, ?);
	`, string(cookiesJSON), sentinelAppPass, sentinelMailbox, sentinelProxy, now, now); err != nil {
		t.Fatalf("插入明文账号失败: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("关闭源数据库失败: %v", err)
	}

	// 2. 执行新的 offline backup API: CreateDatabaseBackup
	ctx := context.Background()
	backupPath := filepath.Join(dir, "v1_offline_backup.db")
	if err := CreateDatabaseBackup(ctx, dir, backupPath); err != nil {
		t.Fatalf("CreateDatabaseBackup 失败: %v", err)
	}

	// 3. 断言 source DB:
	// - user_version 仍 = 1
	// - 旧 schema 不变 (api_tokens 仍保留 token 列)
	// - plaintext 数据不变
	// - 没有新增 V2 schema migration side effects
	srcDB, err := sql.Open("sqlite", filepath.ToSlash(dbPath)+"?mode=ro")
	if err != nil {
		t.Fatalf("打开源数据库检验失败: %v", err)
	}
	defer srcDB.Close()

	var srcVer int
	if err := srcDB.QueryRow("PRAGMA user_version;").Scan(&srcVer); err != nil || srcVer != 1 {
		t.Fatalf("断言失败: 源数据库 user_version 预期保持为 1，实际: %d (err: %v)", srcVer, err)
	}

	hasTokenCol, err := tableHasColumn(srcDB, "api_tokens", "token")
	if err != nil || !hasTokenCol {
		t.Fatalf("断言失败: 源数据库 schema 发生漂移，api_tokens 必须仍包含 token 列 (has=%v, err=%v)", hasTokenCol, err)
	}

	var rawTok string
	if err := srcDB.QueryRow("SELECT token FROM api_tokens WHERE id = 'tok_v1_plain'").Scan(&rawTok); err != nil || rawTok != sentinelToken {
		t.Fatalf("断言失败: 源数据库明文 token 被篡改或迁移: %s (err=%v)", rawTok, err)
	}

	var rawCookies, rawAppPass, rawMailbox, rawProxy string
	if err := srcDB.QueryRow("SELECT cookies, app_password, mailbox, proxy FROM accounts WHERE id = 'acc_v1_plain'").Scan(&rawCookies, &rawAppPass, &rawMailbox, &rawProxy); err != nil {
		t.Fatalf("断言失败: 查询源数据库凭据出错: %v", err)
	}
	if !strings.Contains(rawCookies, sentinelCookie) || rawAppPass != sentinelAppPass || rawMailbox != sentinelMailbox || rawProxy != sentinelProxy {
		t.Fatalf("断言失败: 源数据库明文凭据被修改: cookies=%s, pass=%s", rawCookies, rawAppPass)
	}
	if security.IsEncrypted(rawCookies) || security.IsEncrypted(rawAppPass) {
		t.Fatalf("断言失败: 源数据库产生了 V2 加密副作用!")
	}

	// 验证源库只读 DSN 契约: mode=ro 连接硬拒绝任何写操作
	roDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", filepath.ToSlash(dbPath)))
	if err != nil {
		t.Fatalf("打开测试只读连接失败: %v", err)
	}
	defer roDB.Close()
	if _, err := roDB.Exec("INSERT INTO api_tokens (id, name, token, created_at, scopes) VALUES ('tok_ro_fail', 'fail', 'x', 'x', 'x');"); err == nil {
		t.Fatalf("断言失败: mode=ro 只读连接竟然允许执行写操作!")
	}

	// 4. 断言 backup DB:
	// - user_version 同样保持为 1
	// - 数据完整
	// - quick_check == ok
	bakDB, err := sql.Open("sqlite", filepath.ToSlash(backupPath)+"?mode=ro")
	if err != nil {
		t.Fatalf("打开备份库检验失败: %v", err)
	}
	defer bakDB.Close()

	var bakVer int
	if err := bakDB.QueryRow("PRAGMA user_version;").Scan(&bakVer); err != nil || bakVer != 1 {
		t.Fatalf("断言失败: 备份库 user_version 预期保持为 1，实际: %d", bakVer)
	}

	if err := quickCheck(bakDB); err != nil {
		t.Fatalf("断言失败: 备份库 quick_check 失败: %v", err)
	}

	var bakTok string
	if err := bakDB.QueryRow("SELECT token FROM api_tokens WHERE id = 'tok_v1_plain'").Scan(&bakTok); err != nil || bakTok != sentinelToken {
		t.Fatalf("断言失败: 备份库明文 token 丢失或不符: %s (err=%v)", bakTok, err)
	}

	var bakCookies, bakAppPass, bakMailbox, bakProxy string
	if err := bakDB.QueryRow("SELECT cookies, app_password, mailbox, proxy FROM accounts WHERE id = 'acc_v1_plain'").Scan(&bakCookies, &bakAppPass, &bakMailbox, &bakProxy); err != nil {
		t.Fatalf("断言失败: 查询备份库凭据出错: %v", err)
	}
	if !strings.Contains(bakCookies, sentinelCookie) || bakAppPass != sentinelAppPass || bakMailbox != sentinelMailbox || bakProxy != sentinelProxy {
		t.Fatalf("断言失败: 备份库明文凭据不完整: cookies=%s, pass=%s", bakCookies, bakAppPass)
	}
}

// TestPR09_RestoreV1BackupMigratesToV2 验证当前二进制能够正确将 V1 备份恢复到 live 库并自动完成 V1->V2 迁移，
// 包括 Token 哈希迁移、凭据密文化、物理明文擦除，且 backup 源文件本身不被修改。
func TestPR09_RestoreV1BackupMigratesToV2(t *testing.T) {
	tempDir := t.TempDir()
	liveDir := filepath.Join(tempDir, "live")
	backupPath := filepath.Join(tempDir, "v1_legacy_backup.db")

	const (
		sentinelToken   = "am_PLAINTEXT_SECRET_V1_SENTINEL_PR09"
		sentinelCookie  = "COOKIE_SECRET_V1_SENTINEL_PR09"
		sentinelAppPass = "APP_PASS_V1_SENTINEL_PR09"
		sentinelMailbox = "MAILBOX_CONFIG_V1_SENTINEL_PR09"
		sentinelNotify  = "https://feishu.example.com/hook/V1_SENTINEL_PR09"
	)

	// 1. 构造 V1 backup
	bakDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatalf("创建备份数据库失败: %v", err)
	}

	tx, err := bakDB.Begin()
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if err := migrateV0ToV1(tx); err != nil {
		t.Fatalf("migrateV0ToV1 失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交 V1 初始化失败: %v", err)
	}
	if _, err := bakDB.Exec("PRAGMA user_version = 1;"); err != nil {
		t.Fatalf("设置 user_version 失败: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	cookiesJSON, _ := json.Marshal(map[string]string{"session": sentinelCookie})
	if _, err := bakDB.Exec(`
		INSERT INTO accounts (id, name, real_email, cookies, app_password, mailbox, proxy, status, created_at, updated_at)
		VALUES ('acc_v1_mig', 'V1 Account', 'v1@test.com', ?, ?, ?, '', 'active', ?, ?);
	`, string(cookiesJSON), sentinelAppPass, sentinelMailbox, now, now); err != nil {
		t.Fatalf("插入 V1 测试账号失败: %v", err)
	}

	if _, err := bakDB.Exec(`
		INSERT INTO api_tokens (id, name, token, created_at, scopes)
		VALUES ('tok_v1_mig', 'V1 Token', ?, ?, 'admin');
	`, sentinelToken, now); err != nil {
		t.Fatalf("插入 V1 测试令牌失败: %v", err)
	}

	notifyJSON, _ := json.Marshal(map[string]interface{}{"feishu_webhook": sentinelNotify})
	if _, err := bakDB.Exec(`
		INSERT INTO settings (key, value, updated_at)
		VALUES ('notify_settings', ?, ?);
	`, string(notifyJSON), now); err != nil {
		t.Fatalf("插入 V1 通知配置失败: %v", err)
	}

	if err := bakDB.Close(); err != nil {
		t.Fatalf("关闭备份数据库失败: %v", err)
	}

	// 记录 backup source 文件的初始状态
	bakFiBefore, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("读取 backup 文件信息失败: %v", err)
	}

	// 2. 执行 RestoreDatabaseWithCipher(..., keyA)
	keyA := genTestKey(0x11)
	cipherA, err := security.NewSecretCipher(keyA)
	if err != nil {
		t.Fatalf("创建 cipherA 失败: %v", err)
	}

	ctx := context.Background()
	if err := RestoreDatabaseWithCipher(ctx, liveDir, backupPath, cipherA); err != nil {
		t.Fatalf("RestoreDatabaseWithCipher 失败: %v", err)
	}

	// 断言 backup source 文件本身不被修改
	bakFiAfter, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("读取 backup 文件信息失败: %v", err)
	}
	if bakFiAfter.ModTime() != bakFiBefore.ModTime() || bakFiAfter.Size() != bakFiBefore.Size() {
		t.Fatalf("断言失败: backup source 文件在 restore 过程中被意外修改!")
	}

	// 3. 打开恢复后的 live DB 并断言：
	// - user_version == 2
	// - 原 legacy token secret 升级后仍可通过认证
	// - api_tokens 不再保存 plaintext token column
	// - protected credentials 为 enc:v1
	// - 正确 key 可以读取原始 credential
	st, err := NewStoreWithCipher(liveDir, cipherA)
	if err != nil {
		t.Fatalf("使用 cipherA 打开恢复后的数据库失败: %v", err)
	}

	var liveVer int
	if err := st.db.QueryRow("PRAGMA user_version;").Scan(&liveVer); err != nil || liveVer != 2 {
		st.Close()
		t.Fatalf("断言失败: live DB user_version 预期为 2，实际为: %d (err: %v)", liveVer, err)
	}

	// api_tokens 不再保存 plaintext token 列
	hasTokenCol, err := tableHasColumn(st.db, "api_tokens", "token")
	if err != nil || hasTokenCol {
		st.Close()
		t.Fatalf("断言失败: live DB 中 api_tokens 严禁保留明文 token 列 (has=%v, err=%v)", hasTokenCol, err)
	}

	// 原 legacy token secret 升级后仍可通过认证
	if !st.ValidateToken(sentinelToken) {
		st.Close()
		t.Fatalf("断言失败: 原 legacy token secret (%s) 无法通过认证", sentinelToken)
	}

	// 检查 raw SQLite 列已为 enc:v1:
	var rawCookies, rawAppPass, rawMailbox string
	if err := st.db.QueryRow("SELECT cookies, app_password, mailbox FROM accounts WHERE id = 'acc_v1_mig'").Scan(&rawCookies, &rawAppPass, &rawMailbox); err != nil {
		st.Close()
		t.Fatalf("查询恢复后 live DB 失败: %v", err)
	}
	if !security.IsEncrypted(rawCookies) || !security.IsEncrypted(rawAppPass) || !security.IsEncrypted(rawMailbox) {
		st.Close()
		t.Fatalf("断言失败: protected credentials 必须为 enc:v1 密文 (cookies=%s, pass=%s)", rawCookies, rawAppPass)
	}

	// 正确 key 可以读取原始 credential
	rec, err := st.GetAccount("acc_v1_mig")
	if err != nil || rec == nil {
		st.Close()
		t.Fatalf("GetAccount 失败: %v", err)
	}
	if !strings.Contains(rec.CookiesJSON, sentinelCookie) || rec.AppPassword != sentinelAppPass || rec.MailboxJSON != sentinelMailbox {
		st.Close()
		t.Fatalf("解密出的账号凭据不匹配: cookies=%s, pass=%s, mailbox=%s", rec.CookiesJSON, rec.AppPassword, rec.MailboxJSON)
	}

	st.Close() // 刷盘关闭以确保物理文件写入

	// 4. 扫描 live: icloud_hme.db, wal, shm (若存在): 不得包含测试 plaintext sentinel
	sentinels := [][]byte{
		[]byte(sentinelCookie),
		[]byte(sentinelAppPass),
		[]byte(sentinelMailbox),
		[]byte(sentinelNotify),
	}
	checkFiles := []string{
		filepath.Join(liveDir, "icloud_hme.db"),
		filepath.Join(liveDir, "icloud_hme.db-wal"),
		filepath.Join(liveDir, "icloud_hme.db-shm"),
	}
	for _, fPath := range checkFiles {
		data, err := os.ReadFile(fPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("读取 live 文件 %s 失败: %v", fPath, err)
		}
		for _, s := range sentinels {
			if bytes.Contains(data, s) {
				t.Fatalf("安全违规: live 文件 %s 物理存在明文敏感 sentinel: %s", fPath, string(s))
			}
		}
	}
}

// TestPR09_RestoreWrongKeyLeavesLiveDatabaseUntouched 验证当用错误 Master Key 执行恢复时，
// 恢复必须 fail closed 拒绝，原 live DB 完好恢复为 restore 前状态，且 pre-restore 快照完好保留。
func TestPR09_RestoreWrongKeyLeavesLiveDatabaseUntouched(t *testing.T) {
	tempDir := t.TempDir()
	liveDir := filepath.Join(tempDir, "live")
	backupDir := filepath.Join(tempDir, "backup")

	keyA := genTestKey(0xAA)
	cipherA, err := security.NewSecretCipher(keyA)
	if err != nil {
		t.Fatalf("创建 cipherA 失败: %v", err)
	}

	keyB := genTestKey(0xBB)
	cipherB, err := security.NewSecretCipher(keyB)
	if err != nil {
		t.Fatalf("创建 cipherB 失败: %v", err)
	}

	const (
		liveSentinel   = "LIVE_APP_PASSWORD_KEY_A_PR09"
		backupSentinel = "BACKUP_APP_PASSWORD_KEY_B_PR09"
	)

	// 1. 初始化 live DB: 使用 key A
	stLive, err := NewStoreWithCipher(liveDir, cipherA)
	if err != nil {
		t.Fatalf("初始化 live Store 失败: %v", err)
	}
	if err := stLive.SaveAccount(&AccountRecord{
		ID:          "acc_live_1",
		Name:        "Live Account",
		AppPassword: liveSentinel,
		Status:      "active",
	}); err != nil {
		t.Fatalf("写入 live 账号失败: %v", err)
	}
	stLive.Close()

	// 2. 初始化 backup DB: 使用 key B
	stBak, err := NewStoreWithCipher(backupDir, cipherB)
	if err != nil {
		t.Fatalf("初始化 backup Store 失败: %v", err)
	}
	if err := stBak.SaveAccount(&AccountRecord{
		ID:          "acc_bak_1",
		Name:        "Backup Account",
		AppPassword: backupSentinel,
		Status:      "active",
	}); err != nil {
		t.Fatalf("写入 backup 账号失败: %v", err)
	}
	ctx := context.Background()
	backupPath := filepath.Join(tempDir, "backup_key_b.db")
	if err := stBak.CreateBackup(ctx, backupPath); err != nil {
		t.Fatalf("创建 key B 备份失败: %v", err)
	}
	stBak.Close()

	// 3. 当前环境使用 key A 执行 restore: 尝试恢复 key B 的备份
	err = RestoreDatabaseWithCipher(ctx, liveDir, backupPath, cipherA)
	// 预期：restore fail closed，不能报告 success
	if err == nil {
		t.Fatalf("断言失败: 用错误密钥 (key A) 恢复 key B 备份竟然未报错!")
	}

	// 4. 验证原 live DB 被恢复为 restore 前状态：
	// key A 仍可以打开并读取 live credentials
	stReopened, err := NewStoreWithCipher(liveDir, cipherA)
	if err != nil {
		t.Fatalf("断言失败: 回滚后无法用原 key A 打开 live DB: %v", err)
	}
	defer stReopened.Close()

	rec, err := stReopened.GetAccount("acc_live_1")
	if err != nil || rec == nil {
		t.Fatalf("断言失败: 回滚后未能读取原 live 账号: %v", err)
	}
	if rec.AppPassword != liveSentinel {
		t.Fatalf("断言失败: 回滚后 live 凭据不匹配: 预期 %s, 实际 %s", liveSentinel, rec.AppPassword)
	}

	// 5. 验证 pre-restore snapshot 保留
	backupsDir := filepath.Join(liveDir, "backups")
	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		t.Fatalf("读取 backups 目录失败: %v", err)
	}
	foundPreRestore := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pre-restore-") && strings.HasSuffix(entry.Name(), ".db") {
			foundPreRestore = true
			break
		}
	}
	if !foundPreRestore {
		t.Fatalf("断言失败: pre-restore 快照未保留!")
	}
}

// TestPR09_ValidateProtectedSecrets 针对 Store.ValidateProtectedSecrets 执行纯本地目标单测
func TestPR09_ValidateProtectedSecrets(t *testing.T) {
	keyA := genTestKey(0x33)
	cipherA, _ := security.NewSecretCipher(keyA)
	keyB := genTestKey(0x44)
	cipherB, _ := security.NewSecretCipher(keyB)

	t.Run("PassWithCorrectKey", func(t *testing.T) {
		dir := t.TempDir()
		st, err := NewStoreWithCipher(dir, cipherA)
		if err != nil {
			t.Fatalf("NewStoreWithCipher failed: %v", err)
		}
		defer st.Close()

		if err := st.SaveAccount(&AccountRecord{
			ID:          "acc_val_1",
			CookiesJSON: `{"token":"foo"}`,
			AppPassword: "pass",
			MailboxJSON: `{"host":"imap"}`,
			Proxy:       "http://proxy",
		}); err != nil {
			t.Fatalf("SaveAccount failed: %v", err)
		}
		if err := st.SaveEncryptedSetting("notify_settings", `{"webhook":"bar"}`, security.NotifySettingsAAD()); err != nil {
			t.Fatalf("SaveEncryptedSetting failed: %v", err)
		}

		if err := st.ValidateProtectedSecrets(); err != nil {
			t.Fatalf("ValidateProtectedSecrets 应该成功，但报错: %v", err)
		}
	})

	t.Run("FailWithWrongKey", func(t *testing.T) {
		dir := t.TempDir()
		st, err := NewStoreWithCipher(dir, cipherA)
		if err != nil {
			t.Fatalf("NewStoreWithCipher failed: %v", err)
		}
		if err := st.SaveAccount(&AccountRecord{
			ID:          "acc_val_wrong",
			AppPassword: "secret_pass",
		}); err != nil {
			t.Fatalf("SaveAccount failed: %v", err)
		}
		st.Close()

		// 用 key B 打开
		stB, err := newStoreWithLock(dir, nil, cipherB)
		if err != nil {
			t.Fatalf("newStoreWithLock failed: %v", err)
		}
		defer stB.Close()

		if err := stB.ValidateProtectedSecrets(); err == nil {
			t.Fatalf("ValidateProtectedSecrets 用错误密钥应该失败，但返回成功")
		}
	})

	t.Run("FailWithPlaintextSecret", func(t *testing.T) {
		dir := t.TempDir()
		st, err := NewStoreWithCipher(dir, cipherA)
		if err != nil {
			t.Fatalf("NewStoreWithCipher failed: %v", err)
		}
		defer st.Close()

		// 绕过 SaveAccount 直接往 accounts 表写入未加密明文
		if _, err := st.db.Exec(`
			INSERT INTO accounts (id, name, real_email, app_password, status, created_at, updated_at)
			VALUES ('acc_plain', 'Plain', 'plain@test.com', 'PLAINTEXT_SECRET', 'active', datetime('now'), datetime('now'));
		`); err != nil {
			t.Fatalf("插入明文失败: %v", err)
		}

		if err := st.ValidateProtectedSecrets(); err == nil {
			t.Fatalf("ValidateProtectedSecrets 遇到明文凭据应该失败，但返回成功")
		}
	})

	t.Run("FailWithNilCipherWhenSecretsPresent", func(t *testing.T) {
		dir := t.TempDir()
		st, err := NewStoreWithCipher(dir, cipherA)
		if err != nil {
			t.Fatalf("NewStoreWithCipher failed: %v", err)
		}
		if err := st.SaveAccount(&AccountRecord{
			ID:          "acc_no_key",
			AppPassword: "secret_pass",
		}); err != nil {
			t.Fatalf("SaveAccount failed: %v", err)
		}
		st.Close()

		// 用 nil cipher 打开 (且环境无 key)
		t.Setenv("ICLOUD_HME_MASTER_KEY", "")
		t.Setenv("ICLOUD_HME_MASTER_KEY_FILE", "")
		stNil, err := newStoreWithLock(dir, nil, nil)
		if err != nil {
			t.Fatalf("newStoreWithLock failed: %v", err)
		}
		defer stNil.Close()

		if err := stNil.ValidateProtectedSecrets(); err == nil {
			t.Fatalf("ValidateProtectedSecrets 在无 cipher 情况下应该失败，但返回成功")
		}
	})
}

