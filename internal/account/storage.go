/**
 * [INPUT]: 依赖 os, path/filepath, encoding/json, bytes, time, strings, fmt
 * [OUTPUT]: 对外提供 ParseCookieInput, load, save, copyAccount, cloneCookies, importEditedExample, parseAccountsBytes
 * [POS]: internal/account 的原子持久化存储、Cookie 智能序列化与 accounts.example.json 范本自适应导入层
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"icloud-hme/internal/store"
)

// cloneCookies 返回 Cookie map 的独立副本。
func cloneCookies(cookies map[string]string) map[string]string {
	if cookies == nil {
		return nil
	}
	cloned := make(map[string]string, len(cookies))
	for k, v := range cookies {
		cloned[k] = v
	}
	return cloned
}

// copyAccount 返回账号的深拷贝(含 Cookies map),必须在持锁时调用。
func copyAccount(acc *Account) *Account {
	if acc == nil {
		return nil
	}
	cp := *acc
	cp.Cookies = cloneCookies(acc.Cookies)
	return &cp
}

func (m *Manager) load() error {
	// SQLite 持久化: 优先从数据库读取
	if m.store != nil {
		recs, err := m.store.ListAllAccounts()
		if err != nil {
			return fmt.Errorf("从 SQLite 加载账号失败: %w", err)
		}
		if len(recs) > 0 {
			m.accounts = make(map[string]*Account, len(recs))
			for _, rec := range recs {
				acc := recordToAccount(rec)
				m.accounts[acc.ID] = acc
			}
			return nil
		}
		// SQLite 表为空 → 尝试从 JSON 迁移（首次升级场景）
	}

	// JSON 回退（兼容老数据或测试模式）
	raw, err := os.ReadFile(m.dataFile)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		if bakAccounts, bakErr := m.recoverBackup(); bakErr == nil && len(bakAccounts) > 0 {
			m.accounts = bakAccounts
			if err := m.migrateToSQLite(); err != nil {
				return err
			}
			return nil
		}
		if err != nil && os.IsNotExist(err) {
			return m.importEditedExample()
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("accounts 文件为空且无有效备份")
	}
	accounts, err := parseAccountsBytes(raw)
	if err != nil {
		if bakAccounts, bakErr := m.recoverBackup(); bakErr == nil && len(bakAccounts) > 0 {
			m.accounts = bakAccounts
			if err := m.migrateToSQLite(); err != nil {
				return err
			}
			return nil
		}
		return err
	}
	m.accounts = accounts
	if err := m.migrateToSQLite(); err != nil {
		return err
	}
	return nil
}

// migrateToSQLite 将内存中的 accounts 一次性批量写入 SQLite 并归档 JSON 文件 (Fail-Closed: 失败直接返回 error 杜绝脑裂)。
func (m *Manager) migrateToSQLite() error {
	if m.store == nil || len(m.accounts) == 0 {
		return nil
	}
	recs := make([]*store.AccountRecord, 0, len(m.accounts))
	for _, acc := range m.accounts {
		recs = append(recs, accountToRecord(acc))
	}
	if err := m.store.SaveAccountsBatch(recs); err != nil {
		return fmt.Errorf("迁移账号至 SQLite 失败: %w", err)
	}
	// 归档 JSON 源文件
	_ = os.Remove(m.dataFile + ".migrated")
	if err := os.Rename(m.dataFile, m.dataFile+".migrated"); err != nil {
		// 记录归档失败日志但 SQLite 已安全落库
	}
	_ = os.Remove(m.dataFile + ".bak")
	_ = os.Remove(m.dataFile + ".tmp")
	return nil
}

func (m *Manager) recoverBackup() (map[string]*Account, error) {
	bakRaw, err := os.ReadFile(m.dataFile + ".bak")
	if err != nil {
		return nil, err
	}
	accounts, err := parseAccountsBytes(bakRaw)
	if err != nil || len(accounts) == 0 {
		return nil, fmt.Errorf("备份文件无效: %w", err)
	}
	_ = os.WriteFile(m.dataFile, bakRaw, 0600)
	return accounts, nil
}

// importEditedExample 当 accounts.json 缺失时，探测 accounts.example.json 并安全提取用户填入的账号。
func (m *Manager) importEditedExample() error {
	examplePath := filepath.Join(m.dataDir, "accounts.example.json")
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	accounts, err := parseAccountsBytes(raw)
	if err != nil {
		return err
	}
	configured := make(map[string]*Account)
	for id, acc := range accounts {
		if isEditedExampleAccount(acc) {
			configured[id] = acc
		}
	}
	if len(configured) == 0 {
		return nil
	}
	m.accounts = configured
	return m.save()
}

// parseAccountsBytes 解析 accounts 字节流，自适应对象、字典及数组等多种 JSON 容器结构。
func parseAccountsBytes(raw []byte) (map[string]*Account, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return make(map[string]*Account), nil
	}

	// 1. 数组结构: [ {...}, {...} ]
	if trimmed[0] == '[' {
		var list []*Account
		if err := json.Unmarshal(trimmed, &list); err == nil {
			return accountListToMap(list), nil
		}
	}

	// 2. 对象或字典结构
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &rawMap); err != nil {
		return nil, fmt.Errorf("accounts 文件必须是合法 JSON: %w", err)
	}

	// 2.1 检查是否嵌套 accounts 包装层
	if accountsRaw, ok := rawMap["accounts"]; ok {
		trimmedAcc := bytes.TrimSpace(accountsRaw)
		if len(trimmedAcc) > 0 && trimmedAcc[0] == '[' {
			var list []*Account
			if err := json.Unmarshal(trimmedAcc, &list); err == nil {
				return accountListToMap(list), nil
			}
		}
		var dict map[string]*Account
		if err := json.Unmarshal(accountsRaw, &dict); err == nil {
			ensureAccountIDs(dict)
			return dict, nil
		}
	}

	// 2.2 直接字典: { "acc_1": {...} }
	var dict map[string]*Account
	if err := json.Unmarshal(trimmed, &dict); err == nil {
		ensureAccountIDs(dict)
		return dict, nil
	}

	return nil, fmt.Errorf("无法解析 accounts 数据结构")
}

func accountListToMap(list []*Account) map[string]*Account {
	res := make(map[string]*Account, len(list))
	for _, acc := range list {
		if acc != nil && acc.ID != "" {
			res[acc.ID] = acc
		}
	}
	return res
}

func ensureAccountIDs(dict map[string]*Account) {
	for id, acc := range dict {
		if acc != nil && acc.ID == "" {
			acc.ID = id
		}
	}
}

// isEditedExampleAccount 检查账号是否包含真实有效凭据(非范本占位符)。
func isEditedExampleAccount(acc *Account) bool {
	if acc == nil {
		return false
	}
	for _, value := range acc.Cookies {
		if isUserConfiguredValue(value) {
			return true
		}
	}
	return isUserConfiguredValue(acc.RealEmail) ||
		isUserConfiguredValue(acc.ICloudEmail) ||
		isUserConfiguredValue(acc.AppPassword)
}

func isUserConfiguredValue(val string) bool {
	normalized := strings.ToLower(strings.Trim(strings.TrimSpace(val), `"'`))
	if normalized == "" {
		return false
	}
	placeholders := []string{
		"paste_your_cookie_value_here",
		"your_email@icloud.com",
		"example@icloud.com",
		"xxxx-xxxx-xxxx-xxxx",
		"apple-id-password",
		"your-app-password",
	}
	for _, p := range placeholders {
		if strings.Contains(normalized, p) {
			return false
		}
	}
	return true
}

// save 将当前所有账号持久化（全量写入 SQLite 或 JSON 回退）。
// 调用方必须持有 m.mu.Lock()。大多数场景应优先使用 saveAccount 单行写入。
func (m *Manager) save() error {
	if m.store != nil {
		// SQLite 模式: 批量全量写（仅在 Reload/importEditedExample 等场景使用）
		for _, acc := range m.accounts {
			if err := m.store.SaveAccount(accountToRecord(acc)); err != nil {
				return err
			}
		}
		return nil
	}
	// JSON 回退
	return m.saveJSON()
}

// saveAccount 将单个账号写入 SQLite（细粒度写入，替代全量 save）。
// 调用方必须持有 m.mu.Lock()。
func (m *Manager) saveAccount(acc *Account) error {
	if m.store != nil {
		return m.store.SaveAccount(accountToRecord(acc))
	}
	return m.saveJSON()
}

// deleteAccountFromStore 从 SQLite 删除单个账号。
func (m *Manager) deleteAccountFromStore(id string) error {
	if m.store != nil {
		return m.store.DeleteAccount(id)
	}
	return m.saveJSON()
}

// saveJSON 旧式 JSON 全量写入（回退路径）。
func (m *Manager) saveJSON() error {
	wrapper := struct {
		Accounts  map[string]*Account `json:"accounts"`
		UpdatedAt string              `json:"updated_at"`
	}{
		Accounts:  m.accounts,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	tmpFile := m.dataFile + ".tmp"
	if err := os.WriteFile(tmpFile, raw, 0600); err != nil {
		return err
	}
	_ = os.Remove(m.dataFile + ".bak")
	_ = os.Rename(m.dataFile, m.dataFile+".bak")
	if err := os.Rename(tmpFile, m.dataFile); err != nil {
		_ = os.Rename(m.dataFile+".bak", m.dataFile)
		return err
	}
	return nil
}

// ────────────────────────────────────────────────────────────────
// Account ↔ AccountRecord 转换
// ────────────────────────────────────────────────────────────────

func accountToRecord(acc *Account) *store.AccountRecord {
	mbJSON := ""
	if acc.Mailbox != nil {
		if b, err := json.Marshal(acc.Mailbox); err == nil {
			mbJSON = string(b)
		}
	}
	return &store.AccountRecord{
		ID:            acc.ID,
		Name:          acc.Name,
		RealEmail:     acc.RealEmail,
		ICloudEmail:   acc.ICloudEmail,
		CookiesJSON:   store.MarshalCookies(acc.Cookies),
		Host:          acc.Host,
		ServiceURL:    acc.ServiceURL,
		Proxy:         acc.Proxy,
		AppPassword:   acc.AppPassword,
		MailboxJSON:   mbJSON,
		Status:        acc.Status,
		AliasTotal:    acc.AliasTotal,
		AliasActive:   acc.AliasActive,
		LastValidated: acc.LastValidated,
		LastError:     acc.LastError,
		CreatedAt:     acc.CreatedAt,
		TagsJSON:      store.MarshalTags(acc.Tags),
		UpdatedAt:     time.Now().Format(time.RFC3339),
	}
}

func recordToAccount(rec *store.AccountRecord) *Account {
	var mb *MailboxConfig
	if rec.MailboxJSON != "" {
		mb = &MailboxConfig{}
		if err := json.Unmarshal([]byte(rec.MailboxJSON), mb); err != nil {
			mb = nil
		}
	}
	return &Account{
		ID:            rec.ID,
		Name:          rec.Name,
		RealEmail:     rec.RealEmail,
		ICloudEmail:   rec.ICloudEmail,
		Cookies:       store.UnmarshalCookies(rec.CookiesJSON),
		Host:          rec.Host,
		ServiceURL:    rec.ServiceURL,
		Proxy:         rec.Proxy,
		AppPassword:   rec.AppPassword,
		Mailbox:       mb,
		Status:        rec.Status,
		AliasTotal:    rec.AliasTotal,
		AliasActive:   rec.AliasActive,
		LastValidated: rec.LastValidated,
		LastError:     rec.LastError,
		CreatedAt:     rec.CreatedAt,
		Tags:          store.UnmarshalTags(rec.TagsJSON),
	}
}

// ParseCookieInput 解析 Cookie 输入,支持全格式智能自适应:
//   - Header String: "name1=value1; name2=value2; ..." 或带 "Cookie: " 前缀
//   - JSON 数组: [{"name":"k","value":"v"}] (Chrome 插件导出)
//   - JSON 对象: {"name1":"value1","name2":"value2"}
//   - Netscape 格式: 制表符分隔的 .txt Cookie 文件内容
//   - 多行 Header: 每行 key=val 或 key: val
//
// 空输入返回错误。
func ParseCookieInput(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("空白输入 — 请粘贴 Cookie 字符串、JSON 或 Netscape 格式")
	}

	// 剥离常见的 HTTP Header 前缀
	if strings.HasPrefix(strings.ToLower(raw), "cookie:") {
		raw = strings.TrimSpace(raw[7:])
	}

	// 1. JSON 对象格式: {"name":"value"}
	if strings.HasPrefix(raw, "{") {
		var cookies map[string]string
		if err := json.Unmarshal([]byte(raw), &cookies); err == nil && cookies != nil {
			out := make(map[string]string, len(cookies))
			for k, v := range cookies {
				if v != "" {
					out[k] = v
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}

	// 2. 浏览器插件导出的 JSON 数组格式: [{"name":"k","value":"v"}, ...]
	if strings.HasPrefix(raw, "[") {
		var list []struct {
			Name  string `json:"name"`
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &list); err == nil && len(list) > 0 {
			out := make(map[string]string, len(list))
			for _, item := range list {
				n := item.Name
				if n == "" {
					n = item.Key
				}
				if n != "" && item.Value != "" {
					out[n] = item.Value
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}

	// 3. Netscape 格式: 制表符分隔的 .txt (7 列: domain, sub, path, secure, expiry, name, value)
	if strings.Contains(raw, "\t") {
		cookies := make(map[string]string)
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) >= 7 {
				name := strings.TrimSpace(parts[5])
				val := strings.TrimSpace(parts[6])
				if name != "" && val != "" {
					cookies[name] = val
				}
			}
		}
		if len(cookies) > 0 {
			return cookies, nil
		}
	}

	// 4. Header String (分号或换行分隔)
	cookies := make(map[string]string)
	unified := strings.ReplaceAll(raw, "\r\n", ";")
	unified = strings.ReplaceAll(unified, "\n", ";")
	for _, part := range strings.Split(unified, ";") {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "#") {
			continue
		}
		idx := strings.Index(part, "=")
		if idx <= 0 {
			idx = strings.Index(part, ":")
		}
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if name != "" && value != "" {
			cookies[name] = value
		}
	}
	if len(cookies) == 0 {
		return nil, fmt.Errorf("无法解析 Cookie 输入,请提供 Header String、JSON 或 Netscape 格式")
	}
	return cookies, nil
}
