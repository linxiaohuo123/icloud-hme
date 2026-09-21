/**
 * [INPUT]: 依赖 sync, time, log, strings, server.Backend
 * [OUTPUT]: 对外提供 AliasReaper, NewAliasReaper
 * [POS]: server 的僵尸别名垃圾回收器，定期扫描并释放超期未使用的别名，防止 500 个配额枯竭
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AliasReaper 僵尸别名垃圾回收器。
type AliasReaper struct {
	be       Backend
	interval time.Duration
	maxAge   time.Duration
	stopCh   chan struct{}
	once     sync.Once
	stopOnce sync.Once
}

// NewAliasReaper 创建收割机实例。
// interval: 巡检频率 (默认 1 小时)
// maxAge: 别名闲置超过该时间则自动停用 (默认 2 小时)
func NewAliasReaper(be Backend, interval, maxAge time.Duration) *AliasReaper {
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	if maxAge <= 0 {
		maxAge = 2 * time.Hour
	}
	return &AliasReaper{
		be:       be,
		interval: interval,
		maxAge:   maxAge,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动后台垃圾回收协程。
// 响应用户“不清理”指令，彻底停用后台静默清理/停用，保障任何既有别名绝不被自动处置。
func (r *AliasReaper) Start() {
	// no-op: 彻底停用后台自动清理与停用
}

// Stop 停止收割机 (线程安全且幂等)。
func (r *AliasReaper) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
}

func (r *AliasReaper) loop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.reapOnce()
		}
	}
}

// reapOnce 执行单次全量扫描与停用回收。
func (r *AliasReaper) reapOnce() {
	accounts := r.be.ListAccounts()
	now := time.Now()

	for _, acc := range accounts {
		if !acc.HasCookies {
			continue
		}

		aliases, err := r.be.ListAliases(acc.ID)
		if err != nil || len(aliases) == 0 {
			continue
		}

		for _, a := range aliases {
			if !a.Active || a.CreatedAt == "" {
				continue
			}

			// 解析创建时间
			created, err := time.Parse(time.RFC3339, a.CreatedAt)
			if err != nil {
				// 尝试解析不带秒或其它格式
				created, err = time.Parse("2006-01-02T15:04:05Z07:00", a.CreatedAt)
			}
			if err != nil {
				// 防御性兼容纯数字秒/毫秒时间戳
				if f, errNum := strconv.ParseFloat(a.CreatedAt, 64); errNum == nil && f > 0 {
					n := int64(f)
					if n < 1e11 {
						created = time.Unix(n, 0)
						err = nil
					} else if n < 1e14 {
						created = time.UnixMilli(n)
						err = nil
					}
				}
			}
			if err != nil {
				continue
			}

			// 仅自动清理预热废弃别名或明确标记为临时的租用别名，绝不误杀用户正常别名
			isTemporary := a.Label == "prewarmed" || a.Label == "lease" || strings.HasPrefix(a.Label, "temp_")
			if isTemporary && now.Sub(created) > r.maxAge {
				if _, err := r.be.SetAliasActive(acc.ID, a.AnonymousID, false); err == nil {
					log.Printf("[Reaper] 成功停用超期临时别名: account=%s email=%s label=%s age=%v", acc.ID, a.Email, a.Label, now.Sub(created).Round(time.Minute))
				}
			}
		}
	}
}
