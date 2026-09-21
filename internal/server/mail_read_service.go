/**
 * [INPUT]: 依赖 context, sync, time, errors, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 MailReadService, NewMailReadService, BatchMessageItem, BatchMessageResult
 * [POS]: internal/server 的邮件读取与统一详情缓存应用服务 (PR-02 & PR-08 §11.2)，封装收件箱读取、基于 MessageRef 的对称缓存与批量容错查询
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/mail"
)

const (
	defaultMsgCacheTTL     = 5 * time.Minute
	defaultMsgCacheMaxCap  = 1000
)

// BatchMessageItem 批量查询中单项的返回结果 (PR-02 §6.4)
type BatchMessageItem struct {
	RequestedRef string            `json:"requested_ref"`
	Message      *mail.FullMessage `json:"message,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// BatchMessageResult 批量详情结果
type BatchMessageResult struct {
	AccountID string             `json:"account_id"`
	Items     []BatchMessageItem `json:"items"`
}

// MailReadService 统一管理收件箱读取与详情缓存
type MailReadService struct {
	be         Backend
	cacheMu    sync.RWMutex
	cache      map[string]messageCacheEntry
	cacheTTL   time.Duration
	cacheCap   int
}

func NewMailReadService(be Backend) *MailReadService {
	return &MailReadService{
		be:       be,
		cache:    make(map[string]messageCacheEntry),
		cacheTTL: defaultMsgCacheTTL,
		cacheCap: defaultMsgCacheMaxCap,
	}
}

// ListInbox 读取收件箱列表
func (s *MailReadService) ListInbox(ctx context.Context, q InboxQuery) (InboxResult, error) {
	return s.be.ListInbox(q)
}

// ListMailboxes 读取邮箱文件夹列表
func (s *MailReadService) ListMailboxes(ctx context.Context, accountID string) ([]mail.Folder, error) {
	return s.be.ListMailboxes(accountID)
}

// GetMessageDetail 读取单封邮件详情 (带缓存对称性隔离)
func (s *MailReadService) GetMessageDetail(ctx context.Context, accountID, rawID string) (*mail.FullMessage, error) {
	accountID = strings.TrimSpace(accountID)
	rawID = strings.TrimSpace(rawID)
	if accountID == "" || rawID == "" {
		return nil, &BackendError{Status: 400, Code: "VALIDATION_ERROR", Message: "参数缺失: account_id 或邮件 ID 无效"}
	}

	ref, err := mail.ParseMessageRef(rawID, accountID)
	if err != nil {
		if errors.Is(err, mail.ErrAccountMismatch) {
			return nil, &BackendError{Status: 403, Code: "FORBIDDEN", Message: "跨账号邮件读取被拒绝"}
		}
		return nil, &BackendError{Status: 400, Code: "VALIDATION_ERROR", Message: "邮件引用格式无效"}
	}

	cacheKey := ref.CacheKey()

	s.cacheMu.RLock()
	if entry, ok := s.cache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		s.cacheMu.RUnlock()
		return entry.msg, nil
	}
	s.cacheMu.RUnlock()

	msg, err := s.be.GetMessage(accountID, rawID)
	if err != nil {
		return nil, err
	}

	s.cacheMu.Lock()
	if len(s.cache) >= s.cacheCap {
		// 超过上限重置一半
		for k := range s.cache {
			delete(s.cache, k)
			if len(s.cache) < s.cacheCap/2 {
				break
			}
		}
	}
	s.cache[cacheKey] = messageCacheEntry{
		msg:       msg,
		expiresAt: time.Now().Add(s.cacheTTL),
	}
	s.cacheMu.Unlock()

	return msg, nil
}

// GetMessagesBatch 批量读取多封邮件详情 (逐项容错，单封失败不阻断批次)
func (s *MailReadService) GetMessagesBatch(ctx context.Context, accountID string, refs []string) (*BatchMessageResult, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, &BackendError{Status: 400, Code: "VALIDATION_ERROR", Message: "参数缺失: account_id"}
	}

	res := &BatchMessageResult{
		AccountID: accountID,
		Items:     make([]BatchMessageItem, len(refs)),
	}

	for i, rawID := range refs {
		rawID = strings.TrimSpace(rawID)
		res.Items[i].RequestedRef = rawID
		if rawID == "" {
			res.Items[i].Error = "empty reference"
			continue
		}
		msg, err := s.GetMessageDetail(ctx, accountID, rawID)
		if err != nil {
			res.Items[i].Error = err.Error()
		} else {
			res.Items[i].Message = msg
		}
	}

	return res, nil
}
