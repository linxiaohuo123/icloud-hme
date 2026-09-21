/**
 * [INPUT]: 依赖 gin, net/http, sort, strings, time, icloud-hme/internal/account, icloud-hme/internal/hme, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 quickCreateHandler, selectAccountCandidates, selectAccountByTag
 * [POS]: server 的一键别名快速租用与分销出号接口，支持标签亲和性隔离、配额感知负载均衡与多候选故障平滑切换
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
}

// selectAccountCandidates 根据业务标签与小时配额筛选候选母号列表。
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
	_ = c.ShouldBindJSON(&req)
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
	if len([]rune(req.Label)) > 200 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误: label 最长 200 字符")
		return
	}

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

	tokenName, _ := c.Get("token_name")
	tokenNameStr, _ := tokenName.(string)

	s.recordLease(accountID, res.Email, req.Tag, tokenNameStr)

	ok(c, gin.H{
		"email":      res.Email,
		"account_id": accountID,
		"label":      res.Label,
		"tag":        req.Tag,
		"created_at": res.CreatedAt,
	})
}
