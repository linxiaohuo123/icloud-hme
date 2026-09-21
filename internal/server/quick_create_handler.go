/**
 * [INPUT]: 依赖 gin, net/http, sort, strings, errors, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 quickCreateHandler, selectAccountCandidates, selectAccountByTag, selectPoolAccounts
 * [POS]: server 的统一出号入口门面 (Section I)，统一委托 AliasAllocationService 驱动单一库存真相源
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
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

	if tag != "" {
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

// quickCreateHandler 处理 POST /api/quick-create、/api/alias/lease、/api/allocate 与 /api/external/v1/allocate。
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

	p, _ := getPrincipal(c)

	// 统一调用 AliasAllocationService 单一真相源出号
	allocRes, err := s.allocService.Allocate(c.Request.Context(), p, AllocationRequest{
		Tag:       req.Tag,
		Label:     req.Label,
		AccountID: req.AccountID,
		Mode:      req.Mode,
	})

	if err != nil {
		if errors.Is(err, ErrAllocationNotReady) {
			failCode(c, http.StatusServiceUnavailable, "ALLOCATION_STATE_NOT_READY", "存储层未就绪，无法安全验证别名分配状态")
			return
		}
		if errors.Is(err, ErrForbiddenAccountID) {
			failCode(c, http.StatusForbidden, "FORBIDDEN", "普通外部令牌禁止指定 account_id 出号")
			return
		}
		if errors.Is(err, ErrForbiddenRemoteCreation) {
			failCode(c, http.StatusServiceUnavailable, "ALLOCATION_STATE_NOT_READY", "现场按需创建能力未就绪，当前仅支持已验证库存池分配 (mode=pool)")
			return
		}
		if errors.Is(err, ErrTagNotAllowed) {
			failCode(c, http.StatusForbidden, "FORBIDDEN", "指定的业务标签不在该主体授权范围内")
			return
		}
		if errors.Is(err, ErrPoolEmpty) {
			c.Header("Retry-After", "60")
			failCode(c, http.StatusServiceUnavailable, "POOL_EMPTY", "当前业务池无可用预存别名，请等待定时补货或使用 mode=pool")
			return
		}
		backendFail(c, err)
		return
	}

	ok(c, gin.H{
		"email":      allocRes.Allocation.AliasEmail,
		"account_id": allocRes.Allocation.AccountID,
		"label":      req.Label,
		"tag":        allocRes.Allocation.BusinessTag,
		"source":     allocRes.Source,
		"created_at": allocRes.Allocation.AllocatedAt,
	})
}
