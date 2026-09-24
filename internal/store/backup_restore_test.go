/**
 * [INPUT]: 依赖 context, database/sql, os, path/filepath, strings, testing, time, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 提供 TestPR06_BackupIncludesCommittedWALData, TestPR06_BackupPreservesCriticalState, TestPR06_RestoreRoundTrip, TestPR06_CorruptBackupCannotReplaceLiveDatabase, TestPR06_FutureBackupRejected
 * [POS]: internal/store 的备份与离线恢复一致性单测套件 (PR-06 Baseline)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"icloud-hme/internal/hme"
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
	var tokName, tokSecret, scopes string
	err = bakDB.QueryRow("SELECT name, token, scopes FROM api_tokens WHERE id='tok_wal_1'").Scan(&tokName, &tokSecret, &scopes)
	if err != nil {
		t.Fatalf("未能在备份中找到刚写入的 WAL committed 数据: %v", err)
	}
	if tokName != "wal_bot" || tokSecret != "sec_wal_test" || scopes != "allocate" {
		t.Fatalf("WAL 备份中数据字段不匹配: %s, %s, %s", tokName, tokSecret, scopes)
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
	if accOut.CookiesJSON != accRecord.CookiesJSON {
		t.Fatalf("Cookie 字段不匹配: 期望 %s, 实际 %s", accRecord.CookiesJSON, accOut.CookiesJSON)
	}
	if accOut.AppPassword != accRecord.AppPassword {
		t.Fatalf("AppPassword 字段不匹配: 期望 %s, 实际 %s", accRecord.AppPassword, accOut.AppPassword)
	}
	if accOut.MailboxJSON != accRecord.MailboxJSON {
		t.Fatalf("Mailbox 字段不匹配: 期望 %s, 实际 %s", accRecord.MailboxJSON, accOut.MailboxJSON)
	}

	// 校验 Token
	var tokToken, tokScopes string
	if err := bakDB.QueryRow("SELECT token, scopes FROM api_tokens WHERE id='tok_crit_1'").Scan(&tokToken, &tokScopes); err != nil {
		t.Fatalf("查询备份中 api_tokens 失败: %v", err)
	}
	if tokToken != "api_token_secret_9999" || tokScopes != "admin,allocate" {
		t.Fatalf("Token 关键字段不匹配: token=%s, scopes=%s", tokToken, tokScopes)
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
	if len(tokens) != 1 || tokens[0].ID != "tok_round_1" || tokens[0].Token != "sec_round_123" {
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
