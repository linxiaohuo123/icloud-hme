/**
 * [INPUT]: 依赖 gin, net/http, sort, strings, time, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 quickCreateHandler, selectAccountCandidates, selectAccountByTag, selectPoolAccounts
 * [POS]: server 的一键别名快速租用与分销出号接口，支持 Pool-First 别名池交错轮转、惰性切块流式认领 (Lazy Chunking 零内存分配)、标签隔离与平滑容灾
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/store"
)

type quickCreateReq struct {
	AccountID string `json:"account_id"`
	Label     string `json:"label"`
	Tag       string `json:"tag"`
	Mode      string `json:"mode"` // "pool" (默认池优先) | "pool_only" (仅池化) | "create" (强制现场新建)
}

// selectAccountCandidates 根据业务标签与小时配额筛选候选母号列表 (用于现场新建别名场景)。
// 调度原则：
//  1. 业务隔离：优先专属标签匹配账号，其次回退至未打标的通用公共账号，绝不跨业务借用。
//  2. 双维熔断：严格排除 AliasTotal >= 500 或 AliasActive >= 500 的账号。
//  3. 配额感知与负载均衡：
//     若 st != nil，优先划分当前小时仍有剩余配额 (RemainingQuota > 0) 的账号。
//     在同配额梯度内，按 AliasTotal 升序排序（最低水位优先分配）；
//     若部分账号配额耗尽，有配额账号排在前面，配额耗尽账号追加在后作为重试备选。
func selectAccountCandidates(accounts []account.Summary, tag string, st *store.Store) []string {
	tag = strings.TrimSpace(strings.ToLower(tag))
	var candidates []account.Summary

	if tag != "" && tag != "default" {
		for _, acc := range accounts {
			if acc.Status == "active" && acc.HasCookies && acc.AliasTotal < 500 && acc.AliasActive < 500 {
				for _, t := range acc.Tags {
					if strings.EqualFold(t, tag) {
						candidates = append(candidates, acc)
						break
					}
				}
			}
		}
	}

	// 回退到公共账号池 (未打任何业务标签的账号)
	if len(candidates) == 0 {
		for _, acc := range accounts {
			if acc.Status == "active" && acc.HasCookies && len(acc.Tags) == 0 && acc.AliasTotal < 500 && acc.AliasActive < 500 {
				candidates = append(candidates, acc)
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	sortByTotal := func(list []account.Summary) {
		sort.Slice(list, func(i, j int) bool {
			return list[i].AliasTotal < list[j].AliasTotal
		})
	}

	if st == nil {
		sortByTotal(candidates)
		res := make([]string, len(candidates))
		for i, acc := range candidates {
			res[i] = acc.ID
		}
		return res
	}

	var withQuota []account.Summary
	var noQuota []account.Summary
	for _, acc := range candidates {
		if st.RemainingQuota(acc.ID) > 0 {
			withQuota = append(withQuota, acc)
		} else {
			noQuota = append(noQuota, acc)
		}
	}

	sortByTotal(withQuota)
	sortByTotal(noQuota)

	var ordered []account.Summary
	if len(withQuota) > 0 {
		ordered = append(ordered, withQuota...)
		ordered = append(ordered, noQuota...)
	} else {
		ordered = noQuota
	}

	res := make([]string, len(ordered))
	for i, acc := range ordered {
		res[i] = acc.ID
	}
	return res
}

// selectPoolAccounts 筛选适合从别名池领号的母号 (对齐业务标签隔离；具备凭据；不受 500 上限限制因为已有别名无需新建)。
func selectPoolAccounts(accounts []account.Summary, tag string) []string {
	tag = strings.TrimSpace(strings.ToLower(tag))
	var matched []string

	if tag != "" && tag != "default" {
		for _, acc := range accounts {
			if acc.Status == "active" && (acc.HasCookies || acc.HasAppPassword) {
				for _, t := range acc.Tags {
					if strings.EqualFold(t, tag) {
						matched = append(matched, acc.ID)
						break
					}
				}
			}
		}
	}

	// 回退到公共账号池 (未打任何业务标签的账号)
	if len(matched) == 0 {
		for _, acc := range accounts {
			if acc.Status == "active" && (acc.HasCookies || acc.HasAppPassword) && len(acc.Tags) == 0 {
				matched = append(matched, acc.ID)
			}
		}
	}
	return matched
}

// selectAccountByTag 根据业务标签筛选单个最优母号 (兼顾单点调用兼容性与单测验证)。
func selectAccountByTag(accounts []account.Summary, tag string) string {
	cands := selectAccountCandidates(accounts, tag, nil)
	if len(cands) == 0 {
		return ""
	}
	return cands[0]
}

// quickCreateHandler 处理 POST /api/quick-create、/api/allocate 与 /api/external/v1/allocate。
func (s *Server) quickCreateHandler(c *gin.Context) {
	var req quickCreateReq
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "请求体 JSON 格式错误: "+err.Error())
		return
	}
	if req.Label == "" {
		req.Label = c.Query("label")
	}
	if req.AccountID == "" {
		req.AccountID = c.Query("account_id")
	}
	if req.Tag == "" {
		req.Tag = c.Query("tag")
	}
	if req.Tag == "" {
		req.Tag = "default"
	}
	if req.Mode == "" {
		req.Mode = c.Query("mode")
	}
	if req.Mode == "" {
		req.Mode = "pool" // 默认别名池优先
	}
	if req.Mode != "pool" && req.Mode != "pool_only" && req.Mode != "create" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: mode 必须为 pool、pool_only 或 create")
		return
	}
	if len([]rune(req.Label)) > 200 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: label 最长 200 字符")
		return
	}
	if req.AccountID != "" {
		if _, err := s.be.GetAccount(req.AccountID); err != nil {
			backendFail(c, err)
			return
		}
	}

	tokenName, _ := c.Get("token_name")
	tokenNameStr, _ := tokenName.(string)
	if tokenNameStr == "" {
		tokenNameStr = "admin_console"
	}

	// 1. 别名池优先领用 (Pool-First): 在 mode != "create" 且存储可用时，优先从现存活跃别名池中原子划拨
	if req.Mode != "create" && s.store != nil {
		var poolAccountIDs []string
		if req.AccountID != "" {
			poolAccountIDs = []string{req.AccountID}
		} else {
			poolAccountIDs = selectPoolAccounts(s.be.ListAccounts(), req.Tag)
		}

		// 跨账号交错交织构建候选池 (Round-Robin Interleaving):
		// 避免单一账号被瞬时连续领空导致所有目标邮件砸向单一邮箱，
		// 实现领号与后续收信负载在多母号间极致均匀平摊。
		var accountAliases [][]store.PoolCandidate
		maxCount := 0
		for _, accID := range poolAccountIDs {
			aliases, _ := s.be.ListAliases(accID)
			var list []store.PoolCandidate
			for _, a := range aliases {
				if a.Active {
					list = append(list, store.PoolCandidate{
						AccountID: accID,
						Email:     a.Email,
					})
				}
			}
			if len(list) > 0 {
				accountAliases = append(accountAliases, list)
				if len(list) > maxCount {
					maxCount = len(list)
				}
			}
		}

		// 跨账号交错交织惰性切块探查 (Lazy Chunk Streaming & Round-Robin Interleaving):
		// 采用固定 500 容量切片复用，避免在数万别名场景下一次性贪婪分配巨额切片引发 GC 抖动。
		// 绝大多数情况下首批 500 条内即可命中可用号，实现亚毫秒级短路返回。
		const chunkSize = 500
		chunk := make([]store.PoolCandidate, 0, chunkSize)
		var claimed *store.LeaseRecord
		var claimErr error

		for i := 0; i < maxCount; i++ {
			for _, list := range accountAliases {
				if i < len(list) {
					chunk = append(chunk, list[i])
					if len(chunk) >= chunkSize {
						claimed, claimErr = s.store.ClaimPoolAlias(chunk, req.Tag, tokenNameStr)
						if claimErr != nil {
							failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "别名池认领失败: "+claimErr.Error())
							return
						}
						if claimed != nil {
							break
						}
						chunk = chunk[:0] // 原地重用底层数组，零内存再分配
					}
				}
			}
			if claimed != nil {
				break
			}
		}

		// 检查尾批不足 500 条的剩余候选
		if claimed == nil && len(chunk) > 0 {
			claimed, claimErr = s.store.ClaimPoolAlias(chunk, req.Tag, tokenNameStr)
			if claimErr != nil {
				failCode(c, http.StatusInternalServerError, "INTERNAL_ERROR", "别名池认领失败: "+claimErr.Error())
				return
			}
		}

		if claimed != nil {
			if s.syncWorker != nil {
				s.syncWorker.RegisterAliasAccount(claimed.Email, claimed.AccountID)
			}
			ok(c, gin.H{
				"email":      claimed.Email,
				"account_id": claimed.AccountID,
				"label":      req.Label,
				"tag":        req.Tag,
				"source":     "pool",
				"created_at": claimed.AllocatedAt,
			})
			return
		}

		// 若明确声明只走别名池 (pool_only)，池空即终止，绝不上游新建打扰 Apple
		if req.Mode == "pool_only" {
			failCode(c, http.StatusServiceUnavailable, "POOL_EMPTY", "当前业务池无可用预存别名，请等待定时补货或使用 mode=pool")
			return
		}
	}

	// 2. 降级现场新建 (On-Demand Creation): 池已空或强制指定 mode="create"
	var res *hme.CreateResult
	var accountID string
	var err error

	if req.AccountID != "" {
		res, err = s.be.CreateAlias(req.AccountID, req.Label)
		accountID = req.AccountID
	} else {
		candidates := selectAccountCandidates(s.be.ListAccounts(), req.Tag, s.store)
		for _, candID := range candidates {
			res, err = s.be.CreateAlias(candID, req.Label)
			if err == nil {
				accountID = candID
				break
			}
		}
	}

	if err != nil {
		backendFail(c, err)
		return
	}
	if res == nil {
		failCode(c, http.StatusServiceUnavailable, "NO_ACCOUNT_AVAILABLE", "当前标签没有可用的健康 iCloud 账号或配额已耗尽")
		return
	}
	if s.syncWorker != nil {
		s.syncWorker.RegisterAliasAccount(res.Email, accountID)
	}

	s.recordLease(accountID, res.Email, req.Tag, tokenNameStr)

	ok(c, gin.H{
		"email":      res.Email,
		"account_id": accountID,
		"label":      res.Label,
		"tag":        req.Tag,
		"source":     "created",
		"created_at": res.CreatedAt,
	})
}
