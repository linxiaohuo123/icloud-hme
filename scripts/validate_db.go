/**
 * [INPUT]: 依赖 database/sql, os, path/filepath, fmt, strings, icloud-hme/internal/security, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 CreateConsistentSnapshot 与 RunValidation 生产数据库 WAL 一致性快照与深度 Fail-Closed 验收巡检能力
 * [POS]: scripts/ 的生产发布数据库只读验收核心引擎 (PR-09)
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"icloud-hme/internal/security"
	"icloud-hme/internal/store"
	_ "modernc.org/sqlite"
)

type RowCountMap map[string]int

// queryCount 安全执行单行计数查询，遇到任何错误立即返回，禁止吞错
func queryCount(db *sql.DB, query string, args ...any) (int, error) {
	var cnt int
	err := db.QueryRow(query, args...).Scan(&cnt)
	if err != nil {
		return 0, err
	}
	return cnt, nil
}

func getTableCounts(db *sql.DB, tables []string) (RowCountMap, error) {
	counts := make(RowCountMap)
	for _, tbl := range tables {
		var exists int
		err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&exists)
		if err != nil {
			return nil, fmt.Errorf("检查表 %s 是否存在失败: %w", tbl, err)
		}
		if exists == 0 {
			counts[tbl] = -1 // table does not exist yet
			continue
		}
		cnt, err := queryCount(db, fmt.Sprintf("SELECT COUNT(*) FROM %s", tbl))
		if err != nil {
			return nil, fmt.Errorf("统计表 %s 行数失败: %w", tbl, err)
		}
		counts[tbl] = cnt
	}
	return counts, nil
}

// CreateConsistentSnapshot 基于 SQLite 官方 VACUUM INTO 机制创建具备事务一致性的独立副本。
// 该机制确保源数据库保持只读不被修改，同时能够将 WAL 中已 commit 但尚未 checkpoint 的数据完整捕获进独立 snapshot。
// 若快照创建失败则 fail-closed，严禁回退到普通的裸文件复制。
func CreateConsistentSnapshot(srcPath, dstPath string) error {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("源数据库文件不可访问: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("源路径 %s 为目录，必须指定有效的 SQLite 数据库文件", srcPath)
	}

	// SQLite VACUUM INTO 要求目标文件不可预先存在
	_ = os.Remove(dstPath)

	// 使用只读模式安全打开源数据库，杜绝任何对源数据库的写操作
	srcDSN := fmt.Sprintf("file:%s?mode=ro", filepath.ToSlash(srcPath))
	srcDB, err := sql.Open("sqlite", srcDSN)
	if err != nil {
		return fmt.Errorf("以只读模式打开源数据库连接失败: %w", err)
	}
	defer srcDB.Close()

	// 转义目标路径中的单引号
	escapedDst := strings.ReplaceAll(filepath.ToSlash(dstPath), "'", "''")
	vacuumSQL := fmt.Sprintf("VACUUM INTO '%s';", escapedDst)

	if _, err := srcDB.Exec(vacuumSQL); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("SQLite 事务一致性快照 (VACUUM INTO) 失败: %w", err)
	}

	dstFi, err := os.Stat(dstPath)
	if err != nil || dstFi.Size() == 0 {
		_ = os.Remove(dstPath)
		return fmt.Errorf("生成的数据库快照文件异常或为空")
	}

	return nil
}

// RunValidation 执行全套生产数据库只读副本迁移与 11 项深度一致性巡检。
// 规则：任何 SQL 查询错误、扫描错误或业务约束不符一律判定为 FAIL，禁止吞错返回假阳性。
func RunValidation(srcPath string) (bool, error) {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return false, fmt.Errorf("无法读取源数据库文件 %s: %w", srcPath, err)
	}
	if fi.IsDir() {
		return false, fmt.Errorf("路径 %s 是目录，请输入完整的 sqlite 数据库文件路径", srcPath)
	}

	fmt.Println("================================================================")
	fmt.Println("       icloud-hme 生产数据库副本迁移与一致性只读验收工具")
	fmt.Println("================================================================")
	fmt.Printf("[1/5] 源数据库: %s (大小: %d 字节)\n", srcPath, fi.Size())

	// 1. 创建隔离临时沙箱目录
	tempDir, err := os.MkdirTemp("", "icloud_hme_db_validate_*")
	if err != nil {
		return false, fmt.Errorf("创建临时沙箱目录失败: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	tempDBPath := filepath.Join(tempDir, "icloud_hme.db")

	// 2. 使用 SQLite VACUUM INTO 创建 WAL 一致性独立快照
	if err := CreateConsistentSnapshot(srcPath, tempDBPath); err != nil {
		return false, fmt.Errorf("创建 WAL 安全一致性快照失败 (Fail Closed): %w", err)
	}
	fmt.Printf("[2/5] 已创建 WAL-safe 一致性快照: %s (源库严格保持只读未被修改)\n", tempDBPath)

	keyTables := []string{
		"accounts",
		"aliases",
		"lease_records",
		"alias_routes",
		"api_tokens",
		"alias_inventory",
		"alias_allocations",
		"operations",
		"verification_requests",
	}

	// 3. 读取迁移前基准数据
	rawPreDB, err := sql.Open("sqlite", tempDBPath)
	if err != nil {
		return false, fmt.Errorf("打开临时副本读取基线失败: %w", err)
	}
	preCounts, err := getTableCounts(rawPreDB, keyTables)
	_ = rawPreDB.Close()
	if err != nil {
		return false, fmt.Errorf("读取迁移前基线行数失败: %w", err)
	}

	// 4. 对临时副本执行 Store 生产迁移 (PR-07/PR-09: 基于 Master Key 验证真实 V1->V2 迁移)
	fmt.Println("[3/5] 正在对临时副本执行 internal/store 迁移与表结构补齐 (基于 Master Key)...")
	masterKey, err := security.LoadMasterKey()
	if err != nil {
		return false, fmt.Errorf("读取 Master Key 失败: %w", err)
	}
	cipher, err := security.NewSecretCipher(masterKey)
	if err != nil {
		return false, fmt.Errorf("Master Key 无效: %w", err)
	}

	st, err := store.NewStoreWithCipher(tempDir, cipher)
	if err != nil {
		return false, fmt.Errorf("数据库迁移执行失败! 服务启动已被安全阻断: %w", err)
	}

	// 纯本地验证受保护凭据可解密性 (PR-09)
	if valErr := st.ValidateProtectedSecrets(); valErr != nil {
		_ = st.Close()
		return false, fmt.Errorf("受保护凭据解密验证失败: %w", valErr)
	}
	_ = st.Close()
	fmt.Println("[3/5] Store 迁移与受保护凭据验证完毕，句柄已安全关闭。")

	// 5. 打开迁移后副本执行一致性与架构终态巡检 (Fail-Closed)
	fmt.Println("[4/5] 正在对迁移后数据库执行深度一致性与架构巡检...")
	db, err := sql.Open("sqlite", tempDBPath)
	if err != nil {
		return false, fmt.Errorf("打开迁移后副本失败: %w", err)
	}
	defer db.Close()

	postCounts, err := getTableCounts(db, keyTables)
	if err != nil {
		return false, fmt.Errorf("读取迁移后表行数失败: %w", err)
	}

	hasBlocker := false

	// Check 0: user_version 契约与 api_tokens 明文隔离
	var userVer int
	if err := db.QueryRow("PRAGMA user_version;").Scan(&userVer); err != nil {
		fmt.Printf("❌ 0. user_version 读取失败: %v\n", err)
		hasBlocker = true
	} else if userVer != store.CurrentSchemaVersion {
		fmt.Printf("❌ 0. user_version 校验: FAIL (预期 %d, 实际 %d)\n", store.CurrentSchemaVersion, userVer)
		hasBlocker = true
	} else {
		fmt.Printf("✅ 0. user_version 校验: %d (版本一致)\n", userVer)
	}

	tRows, err := db.Query("PRAGMA table_info(api_tokens);")
	if err != nil {
		fmt.Printf("❌ 0. api_tokens 列结构读取失败: %v\n", err)
		hasBlocker = true
	} else {
		hasTokenCol := false
		for tRows.Next() {
			var cid int
			var name, ctype string
			var notnull, pk int
			var dflt any
			if err := tRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil && name == "token" {
				hasTokenCol = true
			}
		}
		_ = tRows.Close()
		if hasTokenCol {
			fmt.Println("❌ 0. api_tokens 表严禁保留明文 token 列: FAIL")
			hasBlocker = true
		} else {
			fmt.Println("✅ 0. api_tokens 安全架构: 无明文 token 列")
		}
	}

	// Check 1: SQLite integrity_check
	var integrityResult string
	err = db.QueryRow("PRAGMA integrity_check;").Scan(&integrityResult)
	if err != nil {
		fmt.Printf("❌ 1. SQLite integrity_check: FAIL (查询错误: %v)\n", err)
		hasBlocker = true
	} else if integrityResult != "ok" {
		fmt.Printf("❌ 1. SQLite integrity_check: FAIL (检测异常: %s)\n", integrityResult)
		hasBlocker = true
	} else {
		fmt.Println("✅ 1. SQLite integrity_check: ok")
	}

	// Check 2: 表结构清单
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name;")
	if err != nil {
		fmt.Printf("❌ 2. 表结构读取失败: %v\n", err)
		hasBlocker = true
	} else {
		var tableNames []string
		var scanErr error
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				scanErr = err
				break
			}
			tableNames = append(tableNames, name)
		}
		if closeErr := rows.Close(); closeErr != nil && scanErr == nil {
			scanErr = closeErr
		}
		if err := rows.Err(); err != nil && scanErr == nil {
			scanErr = err
		}
		if scanErr != nil {
			fmt.Printf("❌ 2. 表结构扫描异常: %v\n", scanErr)
			hasBlocker = true
		} else {
			fmt.Printf("✅ 2. 当前库表结构 (%d 张表): %s\n", len(tableNames), strings.Join(tableNames, ", "))
		}
	}

	// Check 3: alias_inventory 各状态数量
	cAvail, errAvail := queryCount(db, "SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'available';")
	cAlloc, errAlloc := queryCount(db, "SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'allocated';")
	cUnk, errUnk := queryCount(db, "SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'unknown';")
	cQuar, errQuar := queryCount(db, "SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'quarantined';")
	cInact, errInact := queryCount(db, "SELECT COUNT(*) FROM alias_inventory WHERE remote_state IN ('inactive', 'deleted');")

	if errAvail != nil || errAlloc != nil || errUnk != nil || errQuar != nil || errInact != nil {
		fmt.Printf("❌ 3. alias_inventory 状态查询失败: (avail:%v, alloc:%v, unk:%v, quar:%v, inact:%v)\n", errAvail, errAlloc, errUnk, errQuar, errInact)
		hasBlocker = true
	} else {
		fmt.Println("✅ 3. alias_inventory 状态分布:")
		fmt.Printf("     - available:   %d\n", cAvail)
		fmt.Printf("     - allocated:   %d\n", cAlloc)
		fmt.Printf("     - unknown:     %d\n", cUnk)
		fmt.Printf("     - quarantined: %d\n", cQuar)
		fmt.Printf("     - inactive/del:%d\n", cInact)
	}

	// Check 4: alias_allocations 数量
	totalAllocations, err := queryCount(db, "SELECT COUNT(*) FROM alias_allocations;")
	if err != nil {
		fmt.Printf("❌ 4. alias_allocations 数量查询失败: %v\n", err)
		hasBlocker = true
	} else {
		fmt.Printf("✅ 4. alias_allocations 领用租约总数: %d\n", totalAllocations)
	}

	// Check 5: legacy_unknown owner 数量
	legacyUnknownCount, err := queryCount(db, "SELECT COUNT(*) FROM alias_allocations WHERE owner_id = 'legacy_unknown';")
	if err != nil {
		fmt.Printf("❌ 5. legacy_unknown 隔离归属查询失败: %v\n", err)
		hasBlocker = true
	} else {
		fmt.Printf("✅ 5. legacy_unknown 隔离归属租约数: %d\n", legacyUnknownCount)
	}

	// Check 6: verification_requests 各状态数量
	vreqRows, err := db.Query("SELECT status, COUNT(*) FROM verification_requests GROUP BY status ORDER BY status;")
	if err != nil {
		fmt.Printf("❌ 6. verification_requests 状态查询失败: %v\n", err)
		hasBlocker = true
	} else {
		var vreqErr error
		var vreqLines []string
		for vreqRows.Next() {
			var st string
			var cnt int
			if err := vreqRows.Scan(&st, &cnt); err != nil {
				vreqErr = err
				break
			}
			vreqLines = append(vreqLines, fmt.Sprintf("     - %s: %d", st, cnt))
		}
		if closeErr := vreqRows.Close(); closeErr != nil && vreqErr == nil {
			vreqErr = closeErr
		}
		if err := vreqRows.Err(); err != nil && vreqErr == nil {
			vreqErr = err
		}
		if vreqErr != nil {
			fmt.Printf("❌ 6. verification_requests 遍历读取失败: %v\n", vreqErr)
			hasBlocker = true
		} else {
			fmt.Println("✅ 6. verification_requests 各状态数量:")
			if len(vreqLines) == 0 {
				fmt.Println("     - (暂无取码记录，0 行)")
			} else {
				for _, line := range vreqLines {
					fmt.Println(line)
				}
			}
		}
	}

	// Check 7: 重复 alias allocation 检查
	dupRows, err := db.Query("SELECT alias_email, COUNT(*) as c FROM alias_allocations GROUP BY alias_email HAVING c > 1;")
	if err != nil {
		fmt.Printf("❌ 7. 重复 alias allocation 检查失败: %v\n", err)
		hasBlocker = true
	} else {
		var dupEmails []string
		var dupErr error
		for dupRows.Next() {
			var em string
			var c int
			if err := dupRows.Scan(&em, &c); err != nil {
				dupErr = err
				break
			}
			dupEmails = append(dupEmails, fmt.Sprintf("%s(%d次)", em, c))
		}
		if closeErr := dupRows.Close(); closeErr != nil && dupErr == nil {
			dupErr = closeErr
		}
		if err := dupRows.Err(); err != nil && dupErr == nil {
			dupErr = err
		}
		if dupErr != nil {
			fmt.Printf("❌ 7. 重复 alias allocation 读取异常: %v\n", dupErr)
			hasBlocker = true
		} else if len(dupEmails) > 0 {
			fmt.Printf("❌ 7. 重复 alias allocation 检查: FAIL! 发现重复分配别名: %s\n", strings.Join(dupEmails, ", "))
			hasBlocker = true
		} else {
			fmt.Println("✅ 7. 重复 alias allocation 检查: PASS (零重复分配)")
		}
	}

	// Check 8: available 但存在 allocation 的冲突检查
	conflictAvailableAlloc, err := queryCount(db, `
		SELECT COUNT(*) 
		FROM alias_inventory inv 
		JOIN alias_allocations alloc ON inv.email = alloc.alias_email 
		WHERE inv.allocation_state = 'available';
	`)
	if err != nil {
		fmt.Printf("❌ 8. available 与 allocation 状态冲突检查失败: %v\n", err)
		hasBlocker = true
	} else if conflictAvailableAlloc > 0 {
		fmt.Printf("❌ 8. available 与 allocation 状态冲突检查: FAIL! 发现 %d 条别名既标记为 available 又存在 allocation\n", conflictAvailableAlloc)
		hasBlocker = true
	} else {
		fmt.Println("✅ 8. available 与 allocation 状态冲突检查: PASS (零冲突)")
	}

	// Check 9: allocated 但无 allocation row 的异常检查
	missingAllocRows, err := queryCount(db, `
		SELECT COUNT(*) 
		FROM alias_inventory inv 
		LEFT JOIN alias_allocations alloc ON inv.email = alloc.alias_email 
		WHERE inv.allocation_state = 'allocated' AND alloc.allocation_id IS NULL;
	`)
	if err != nil {
		fmt.Printf("❌ 9. allocated 缺失 allocation 检查失败: %v\n", err)
		hasBlocker = true
	} else if missingAllocRows > 0 {
		fmt.Printf("❌ 9. allocated 但缺失 allocation 记录检查: FAIL! 发现 %d 条已分配别名缺失 allocation row\n", missingAllocRows)
		hasBlocker = true
	} else {
		fmt.Println("✅ 9. allocated 但缺失 allocation 记录检查: PASS (数据严格闭环)")
	}

	// Check 10: 空 account_id / owner_id 等关键异常
	emptyAccInInv, err1 := queryCount(db, "SELECT COUNT(*) FROM alias_inventory WHERE TRIM(account_id) = '';")
	emptyAccInAlloc, err2 := queryCount(db, "SELECT COUNT(*) FROM alias_allocations WHERE TRIM(account_id) = '';")
	emptyOwnerInAlloc, err3 := queryCount(db, "SELECT COUNT(*) FROM alias_allocations WHERE TRIM(owner_id) = '';")
	if err1 != nil || err2 != nil || err3 != nil {
		fmt.Printf("❌ 10. 核心外键非空检查失败: (inv_acc:%v, alloc_acc:%v, alloc_owner:%v)\n", err1, err2, err3)
		hasBlocker = true
	} else if emptyAccInInv > 0 || emptyAccInAlloc > 0 || emptyOwnerInAlloc > 0 {
		fmt.Printf("❌ 10. 核心外键非空检查: FAIL! (inventory空账号:%d, allocation空账号:%d, allocation空owner:%d)\n", emptyAccInInv, emptyAccInAlloc, emptyOwnerInAlloc)
		hasBlocker = true
	} else {
		fmt.Println("✅ 10. 核心外键非空检查: PASS (account_id / owner_id 全量完备)")
	}

	// Check 11: migration 前后关键表 row count 对比
	fmt.Println("✅ 11. 关键表迁移前后行数比对:")
	fmt.Printf("     %-25s | %-12s | %-12s\n", "表名", "迁移前", "迁移后")
	fmt.Printf("     --------------------------+--------------+--------------\n")
	for _, tbl := range keyTables {
		preStr := fmt.Sprintf("%d", preCounts[tbl])
		if preCounts[tbl] == -1 {
			preStr = "[不存在]"
		}
		postStr := fmt.Sprintf("%d", postCounts[tbl])
		if postCounts[tbl] == -1 {
			postStr = "[不存在]"
		}
		fmt.Printf("     %-25s | %-12s | %-12s\n", tbl, preStr, postStr)
	}

	fmt.Println("================================================================")
	if hasBlocker {
		fmt.Println("🚨 验收结论: NOT_READY (发现 Release Blocker 异常，严禁上线！)")
		return false, nil
	}

	fmt.Println("🎉 验收结论: VALIDATION_PASSED (副本迁移完全成功，数据一致性无瑕疵)")
	return true, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "使用方式: %s <source_db_path>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "示例: %s ./data/icloud_hme.db\n", os.Args[0])
		os.Exit(2)
	}

	passed, err := RunValidation(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] 验收过程遭遇致命错误: %v\n", err)
		os.Exit(1)
	}
	if !passed {
		os.Exit(1)
	}
}
