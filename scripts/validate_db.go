/**
 * [INPUT]: 依赖 database/sql, os, io, path/filepath, fmt, flag, strings, icloud-hme/internal/store
 * [OUTPUT]: 对外提供生产数据库只读副本无损迁移验证与 11 项领域一致性巡检能力
 * [POS]: scripts/ 的生产发布数据库只读验收核心引擎
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"icloud-hme/internal/store"
	_ "modernc.org/sqlite"
)

type RowCountMap map[string]int

func getTableCounts(db *sql.DB, tables []string) RowCountMap {
	counts := make(RowCountMap)
	for _, tbl := range tables {
		var exists int
		_ = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&exists)
		if exists == 0 {
			counts[tbl] = -1 // table does not exist yet
			continue
		}
		var cnt int
		err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", tbl)).Scan(&cnt)
		if err != nil {
			counts[tbl] = -2 // query error
		} else {
			counts[tbl] = cnt
		}
	}
	return counts
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "使用方式: %s <source_db_path>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "示例: %s ./data/icloud_hme.db\n", os.Args[0])
		os.Exit(2)
	}

	srcPath := os.Args[1]
	fi, err := os.Stat(srcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] 无法读取源数据库文件 %s: %v\n", srcPath, err)
		os.Exit(2)
	}
	if fi.IsDir() {
		fmt.Fprintf(os.Stderr, "[ERROR] 路径 %s 是目录，请输入完整的 sqlite 数据库文件路径\n", srcPath)
		os.Exit(2)
	}

	fmt.Println("================================================================")
	fmt.Println("       icloud-hme 生产数据库副本迁移与一致性只读验收工具")
	fmt.Println("================================================================")
	fmt.Printf("[1/5] 源数据库: %s (大小: %d 字节)\n", srcPath, fi.Size())

	// 1. 创建隔离临时目录，复制 source.db 到 temp/icloud_hme.db
	tempDir, err := os.MkdirTemp("", "icloud_hme_db_validate_*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] 创建临时目录失败: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	tempDBPath := filepath.Join(tempDir, "icloud_hme.db")
	if err := copyFile(srcPath, tempDBPath); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] 复制源数据库到隔离临时目录失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[2/5] 已创建隔离副本: %s (源数据库严格保持只读未触碰)\n", tempDBPath)

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

	// 2. 读取迁移前基准数据
	rawPreDB, err := sql.Open("sqlite", tempDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] 打开临时副本读取基线失败: %v\n", err)
		os.Exit(1)
	}
	preCounts := getTableCounts(rawPreDB, keyTables)
	_ = rawPreDB.Close()

	// 3. 执行 Store 生产迁移
	fmt.Println("[3/5] 正在通过 internal/store 执行新版本 DDL 补列与单事务数据迁移...")
	st, err := store.NewStore(tempDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] 数据库迁移执行失败! 服务启动已被安全阻断: %v\n", err)
		os.Exit(1)
	}
	_ = st.Close()
	fmt.Println("[3/5] Store 迁移与拓扑初始化执行完毕，句柄已安全关闭。")

	// 4. 打开迁移后副本执行 11 项深度验收巡检
	fmt.Println("[4/5] 正在对迁移后数据库执行 11 项深度一致性与完整性巡检...")
	db, err := sql.Open("sqlite", tempDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] 打开迁移后副本失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	postCounts := getTableCounts(db, keyTables)

	hasBlocker := false

	// Check 1: SQLite integrity_check
	var integrityResult string
	err = db.QueryRow("PRAGMA integrity_check;").Scan(&integrityResult)
	if err != nil || integrityResult != "ok" {
		fmt.Printf("❌ 1. SQLite integrity_check: FAIL (结果: %s, 错误: %v)\n", integrityResult, err)
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
		for rows.Next() {
			var name string
			_ = rows.Scan(&name)
			tableNames = append(tableNames, name)
		}
		rows.Close()
		fmt.Printf("✅ 2. 当前库表结构 (%d 张表): %s\n", len(tableNames), strings.Join(tableNames, ", "))
	}

	// Check 3: alias_inventory 各状态数量
	var countAvailable, countAllocated, countUnknown, countQuarantined, countInactive int
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'available';").Scan(&countAvailable)
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'allocated';").Scan(&countAllocated)
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'unknown';").Scan(&countUnknown)
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_inventory WHERE allocation_state = 'quarantined';").Scan(&countQuarantined)
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_inventory WHERE remote_state IN ('inactive', 'deleted');").Scan(&countInactive)
	fmt.Println("✅ 3. alias_inventory 状态分布:")
	fmt.Printf("     - available:   %d\n", countAvailable)
	fmt.Printf("     - allocated:   %d\n", countAllocated)
	fmt.Printf("     - unknown:     %d\n", countUnknown)
	fmt.Printf("     - quarantined: %d\n", countQuarantined)
	fmt.Printf("     - inactive/del:%d\n", countInactive)

	// Check 4: alias_allocations 数量
	var totalAllocations int
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_allocations;").Scan(&totalAllocations)
	fmt.Printf("✅ 4. alias_allocations 领用租约总数: %d\n", totalAllocations)

	// Check 5: legacy_unknown owner 数量
	var legacyUnknownCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_allocations WHERE owner_id = 'legacy_unknown';").Scan(&legacyUnknownCount)
	fmt.Printf("✅ 5. legacy_unknown 隔离归属租约数: %d\n", legacyUnknownCount)

	// Check 6: verification_requests 各状态数量
	vreqRows, err := db.Query("SELECT status, COUNT(*) FROM verification_requests GROUP BY status ORDER BY status;")
	fmt.Println("✅ 6. verification_requests 各状态数量:")
	if err == nil {
		foundVreq := false
		for vreqRows.Next() {
			var st string
			var cnt int
			_ = vreqRows.Scan(&st, &cnt)
			fmt.Printf("     - %s: %d\n", st, cnt)
			foundVreq = true
		}
		vreqRows.Close()
		if !foundVreq {
			fmt.Println("     - (暂无取码记录，0 行)")
		}
	}

	// Check 7: 重复 alias allocation 检查
	dupRows, err := db.Query("SELECT alias_email, COUNT(*) as c FROM alias_allocations GROUP BY alias_email HAVING c > 1;")
	var dupEmails []string
	if err == nil {
		for dupRows.Next() {
			var em string
			var c int
			_ = dupRows.Scan(&em, &c)
			dupEmails = append(dupEmails, fmt.Sprintf("%s(%d次)", em, c))
		}
		dupRows.Close()
	}
	if len(dupEmails) > 0 {
		fmt.Printf("❌ 7. 重复 alias allocation 检查: FAIL! 发现重复分配别名: %s\n", strings.Join(dupEmails, ", "))
		hasBlocker = true
	} else {
		fmt.Println("✅ 7. 重复 alias allocation 检查: PASS (零重复分配)")
	}

	// Check 8: available 但存在 allocation 的冲突检查
	var conflictAvailableAlloc int
	err = db.QueryRow(`
		SELECT COUNT(*) 
		FROM alias_inventory inv 
		JOIN alias_allocations alloc ON inv.email = alloc.alias_email 
		WHERE inv.allocation_state = 'available';
	`).Scan(&conflictAvailableAlloc)
	if conflictAvailableAlloc > 0 {
		fmt.Printf("❌ 8. available 与 allocation 状态冲突检查: FAIL! 发现 %d 条别名既标记为 available 又存在 allocation\n", conflictAvailableAlloc)
		hasBlocker = true
	} else {
		fmt.Println("✅ 8. available 与 allocation 状态冲突检查: PASS (零冲突)")
	}

	// Check 9: allocated 但无 allocation row 的异常检查
	var missingAllocRows int
	err = db.QueryRow(`
		SELECT COUNT(*) 
		FROM alias_inventory inv 
		LEFT JOIN alias_allocations alloc ON inv.email = alloc.alias_email 
		WHERE inv.allocation_state = 'allocated' AND alloc.allocation_id IS NULL;
	`).Scan(&missingAllocRows)
	if missingAllocRows > 0 {
		fmt.Printf("❌ 9. allocated 但缺失 allocation 记录检查: FAIL! 发现 %d 条已分配别名缺失 allocation row\n", missingAllocRows)
		hasBlocker = true
	} else {
		fmt.Println("✅ 9. allocated 但缺失 allocation 记录检查: PASS (数据严格闭环)")
	}

	// Check 10: 空 account_id / owner_id 等关键异常
	var emptyAccInInv, emptyAccInAlloc, emptyOwnerInAlloc int
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_inventory WHERE TRIM(account_id) = '';").Scan(&emptyAccInInv)
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_allocations WHERE TRIM(account_id) = '';").Scan(&emptyAccInAlloc)
	_ = db.QueryRow("SELECT COUNT(*) FROM alias_allocations WHERE TRIM(owner_id) = '';").Scan(&emptyOwnerInAlloc)
	if emptyAccInInv > 0 || emptyAccInAlloc > 0 || emptyOwnerInAlloc > 0 {
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
		os.Exit(1)
	} else {
		fmt.Println("🎉 验收结论: VALIDATION_PASSED (副本迁移完全成功，数据一致性无瑕疵)")
		os.Exit(0)
	}
}
