/**
 * [INPUT]: 依赖 os, path/filepath, runtime, time, gin
 * [OUTPUT]: 对外提供 (Server).systemStatsHandler 与 (CookieMonitor).Interval
 * [POS]: internal/server 的运行时可观测性端点，暴露进程/存储/后台引擎的关键水位供运维判断容量
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
)

// systemStatsHandler 处理 GET /api/system/stats（仅 admin 作用域）。
//
// 故意不引入 Prometheus 客户端:先用零依赖的运行时快照把「几千账号跑起来之后
// 到底卡在哪」暴露出来。真需要时序采集时再在外层接 exporter。
func (s *Server) systemStatsHandler(c *gin.Context) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	stats := gin.H{
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"goroutines":     runtime.NumGoroutine(),
		"process": gin.H{
			"heap_alloc_bytes":  mem.HeapAlloc,
			"heap_sys_bytes":    mem.HeapSys,
			"gc_cycles":         mem.NumGC,
			"gc_pause_total_ns": mem.PauseTotalNs,
		},
	}

	// 存储水位
	if s.store != nil {
		accounts, _ := s.store.AccountCount()
		stats["store"] = gin.H{
			"accounts":      accounts,
			"alias_routes":  s.store.CountAliasRoutes(),
			"leases":        s.store.CountLeases(),
			"db_size_bytes": s.dbSizeBytes(),
		}
	}

	// 内存缓存水位(用于判断是否接近上限)
	s.msgCacheMu.RLock()
	stats["message_cache_entries"] = len(s.msgCache)
	s.msgCacheMu.RUnlock()

	// 后台引擎状态
	stats["engines"] = gin.H{
		"mail_subscribers":        s.eventBus.HasSubscribers(),
		"cookie_monitor_interval": s.cookieMon.Interval().String(),
		"scheduler":               s.scheduler.Status(),
		"lease_pruner": gin.H{
			"enabled":     s.leasePruner.Enabled(),
			"retention":   s.leasePruner.retention.String(),
			"recent_logs": s.leasePruner.Logs(),
		},
	}

	ok(c, stats)
}

// dbSizeBytes 返回 SQLite 主库及其 WAL/SHM 边车文件的合计字节数。
func (s *Server) dbSizeBytes() int64 {
	if s.cfg.DataDir == "" {
		return 0
	}
	var total int64
	for _, name := range []string{"icloud_hme.db", "icloud_hme.db-wal", "icloud_hme.db-shm"} {
		if info, err := os.Stat(filepath.Join(s.cfg.DataDir, name)); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Interval 返回当前 Cookie 健康监控周期。
func (m *CookieMonitor) Interval() time.Duration { return m.interval }
