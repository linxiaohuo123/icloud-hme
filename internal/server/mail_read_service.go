/**
 * [INPUT]: 依赖 context, errors, strings, sync, time, strconv, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 MailReadService, NewMailReadService, BatchItemResult, batchMessageItemReq
 * [POS]: internal/server 的邮件读取与统一详情缓存应用服务，封装收件箱读取、基于 MessageRef 的规范身份 Join 与缓存对称隔离
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/mail"
)

const (
	defaultMsgCacheTTL    = 10 * time.Minute
	defaultMsgCacheMaxCap = 1000
	defaultMailboxTTL     = 5 * time.Minute
	defaultListCacheTTL   = 15 * time.Second
)

type messageCacheEntry struct {
	msg       *mail.FullMessage
	expiresAt time.Time
	provider  string
	method    string
}

type mailboxCacheEntry struct {
	folders   []mail.Folder
	expiresAt time.Time
}

type listCacheEntry struct {
	result    InboxResult
	expiresAt time.Time
}

type inFlightListCall struct {
	done     chan struct{}
	res      InboxResult
	err      error
	refCount int
	cancel   context.CancelFunc
}

// batchMessageItemReq 前端或客户端批量请求单项
type batchMessageItemReq struct {
	MessageRef string `json:"message_ref"`
	Folder     string `json:"folder,omitempty"`
	UID        string `json:"uid,omitempty"`
	ID         string `json:"id,omitempty"`
}

// BatchItemResult 批量查询中每封邮件的精确对应结果
type BatchItemResult struct {
	RequestedRef string            `json:"requested_ref"`
	Message      *mail.FullMessage `json:"message,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// MailReadService 统管邮件读取、规范 MessageRef 校验、单源详情缓存、目录缓存与收件箱请求去重合并
type MailReadService struct {
	be       Backend
	cacheMu  sync.RWMutex
	cache    map[string]messageCacheEntry
	cacheTTL time.Duration
	cacheCap int

	mailboxMu    sync.RWMutex
	mailboxCache map[string]mailboxCacheEntry
	mailboxTTL   time.Duration

	listMu       sync.RWMutex
	listCache    map[string]listCacheEntry
	listTTL      time.Duration

	flightMu     sync.Mutex
	inFlightList map[string]*inFlightListCall
}

// NewMailReadService 创建统一邮件读取服务
func NewMailReadService(be Backend) *MailReadService {
	return &MailReadService{
		be:           be,
		cache:        make(map[string]messageCacheEntry),
		cacheTTL:     defaultMsgCacheTTL,
		cacheCap:     defaultMsgCacheMaxCap,
		mailboxCache: make(map[string]mailboxCacheEntry),
		mailboxTTL:   defaultMailboxTTL,
		listCache:    make(map[string]listCacheEntry),
		listTTL:      defaultListCacheTTL,
		inFlightList: make(map[string]*inFlightListCall),
	}
}

// CacheLen 返回当前详情缓存条目数
func (s *MailReadService) CacheLen() int {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return len(s.cache)
}

func normalizeInboxQueryKey(q InboxQuery) string {
	folder := strings.ToUpper(strings.TrimSpace(q.Folder))
	if folder == "" {
		folder = "INBOX"
	}
	alias := strings.ToLower(strings.TrimSpace(q.Alias))
	return fmt.Sprintf("%s:%s:%s:%d:%d:%v:%d", q.AccountID, alias, folder, q.Limit, q.Days, q.WithBody, q.SinceUID)
}

func cloneInboxResult(res InboxResult) InboxResult {
	msgs := make([]mail.Message, len(res.Messages))
	copy(msgs, res.Messages)
	return InboxResult{
		AccountID: res.AccountID,
		Alias:     res.Alias,
		Folder:    res.Folder,
		Count:     res.Count,
		Messages:  msgs,
		Method:    res.Method,
	}
}

// InvalidateMailboxCache 清理指定账号的目录缓存
func (s *MailReadService) InvalidateMailboxCache(accountID string) {
	s.mailboxMu.Lock()
	defer s.mailboxMu.Unlock()
	delete(s.mailboxCache, strings.TrimSpace(accountID))
}

// InvalidateListCache 清理指定账号的收件箱快照缓存
func (s *MailReadService) InvalidateListCache(accountID string) {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	prefix := strings.TrimSpace(accountID) + ":"
	for k := range s.listCache {
		if strings.HasPrefix(k, prefix) {
			delete(s.listCache, k)
		}
	}
}

// ListInbox 读取收件箱列表 (支持列表快照复用与基于共享 context 的 in-flight 请求合并)
func (s *MailReadService) ListInbox(ctx context.Context, q InboxQuery) (InboxResult, error) {
	if err := ctx.Err(); err != nil {
		return InboxResult{}, err
	}

	queryKey := normalizeInboxQueryKey(q)
	now := time.Now()

	// 1. 若非增量同步且未指定强制刷新，优先利用列表快照缓存
	if q.SinceUID == 0 && !q.Refresh {
		s.listMu.RLock()
		entry, hit := s.listCache[queryKey]
		s.listMu.RUnlock()
		if hit && now.Before(entry.expiresAt) {
			return cloneInboxResult(entry.result), nil
		}
	}

	// 2. 增量 SinceUID 轮询直接穿透回源，严禁被普通列表合并
	if q.SinceUID > 0 {
		return s.be.ListInboxContext(ctx, q)
	}

	// 3. 同键在途请求合并 (In-flight Singleflight): 某个调用方取消不影响其他调用方，全部调用方离开才取消底层工作
	s.flightMu.Lock()
	if call, ok := s.inFlightList[queryKey]; ok {
		call.refCount++
		s.flightMu.Unlock()

		select {
		case <-ctx.Done():
			s.flightMu.Lock()
			call.refCount--
			if call.refCount <= 0 {
				call.cancel()
			}
			s.flightMu.Unlock()
			return InboxResult{}, ctx.Err()
		case <-call.done:
			if call.err != nil {
				return InboxResult{}, call.err
			}
			return cloneInboxResult(call.res), nil
		}
	}

	flightCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	call := &inFlightListCall{
		done:     make(chan struct{}),
		refCount: 1,
		cancel:   cancel,
	}
	s.inFlightList[queryKey] = call
	s.flightMu.Unlock()

	go func() {
		res, err := s.be.ListInboxContext(flightCtx, q)
		cancel()

		s.flightMu.Lock()
		call.res = res
		call.err = err
		close(call.done)
		delete(s.inFlightList, queryKey)

		// 仅成功结果写入缓存，取消或报错绝不写入
		if err == nil {
			s.listMu.Lock()
			if s.listCache == nil {
				s.listCache = make(map[string]listCacheEntry)
			}
			s.listCache[queryKey] = listCacheEntry{
				result:    res,
				expiresAt: time.Now().Add(s.listTTL),
			}
			s.listMu.Unlock()
		}
		s.flightMu.Unlock()
	}()

	select {
	case <-ctx.Done():
		s.flightMu.Lock()
		call.refCount--
		if call.refCount <= 0 {
			call.cancel()
		}
		s.flightMu.Unlock()
		return InboxResult{}, ctx.Err()
	case <-call.done:
		if call.err != nil {
			return InboxResult{}, call.err
		}
		return cloneInboxResult(call.res), nil
	}
}

// ListMailboxes 读取邮箱文件夹列表 (带 5 分钟 TTL 目录缓存与 context 贯穿)
func (s *MailReadService) ListMailboxes(ctx context.Context, accountID string) ([]mail.Folder, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, &BackendError{Status: 400, Code: "VALIDATION_ERROR", Message: "参数缺失: account_id"}
	}

	// 1. 检查目录短缓存，避免与优先邮件列表竞争同账号唯一 IMAP 连接
	s.mailboxMu.RLock()
	entry, hit := s.mailboxCache[accountID]
	s.mailboxMu.RUnlock()

	now := time.Now()
	if hit && now.Before(entry.expiresAt) {
		out := make([]mail.Folder, len(entry.folders))
		copy(out, entry.folders)
		return out, nil
	}

	// 2. 回源查询 (贯穿 ctx)
	folders, err := s.be.ListMailboxesContext(ctx, accountID)
	if err != nil {
		return nil, err
	}

	s.mailboxMu.Lock()
	if s.mailboxCache == nil {
		s.mailboxCache = make(map[string]mailboxCacheEntry)
	}
	s.mailboxCache[accountID] = mailboxCacheEntry{
		folders:   folders,
		expiresAt: now.Add(s.mailboxTTL),
	}
	s.mailboxMu.Unlock()

	out := make([]mail.Folder, len(folders))
	copy(out, folders)
	return out, nil
}

// GetMessageDetail 读取单封邮件详情 (带规范 MessageRef 校验与缓存隔离，全链路 context 贯穿)
func (s *MailReadService) GetMessageDetail(ctx context.Context, accountID, rawID string) (*mail.FullMessage, string, string, bool, error) {
	accountID = strings.TrimSpace(accountID)
	rawID = strings.TrimSpace(rawID)
	if accountID == "" || rawID == "" {
		return nil, "", "", false, &BackendError{Status: 400, Code: "VALIDATION_ERROR", Message: "参数缺失: account_id 或邮件 ID 无效"}
	}

	ref, err := mail.ParseMessageRef(rawID, accountID)
	if err != nil {
		if errors.Is(err, mail.ErrAccountMismatch) {
			return nil, "", "", false, &BackendError{Status: 403, Code: "FORBIDDEN", Message: "跨账号邮件读取被拒绝"}
		}
		return nil, "", "", false, &BackendError{Status: 400, Code: "VALIDATION_ERROR", Message: "邮件引用格式无效"}
	}
	if ref.AccountID != "" && ref.AccountID != accountID {
		return nil, "", "", false, &BackendError{Status: 403, Code: "FORBIDDEN", Message: "跨账号邮件读取被拒绝"}
	}

	cacheKey := ref.CacheKey()
	s.cacheMu.RLock()
	entry, hit := s.cache[cacheKey]
	s.cacheMu.RUnlock()

	if hit && time.Now().Before(entry.expiresAt) {
		return entry.msg, entry.provider, entry.method, true, nil
	}

	message, err := s.be.GetMessageContext(ctx, accountID, rawID)
	if err != nil {
		return nil, "", "", false, err
	}

	provider := message.Provider
	if provider == "" {
		provider = message.Message.Provider
	}
	if provider == "" {
		provider = "imap"
	}
	message.Provider = provider
	message.Message.Provider = provider
	method := message.Method
	if method == "" {
		if provider == "webmail" {
			method = "web_api"
		} else {
			method = "imap"
		}
	}
	message.Method = method

	s.putCache(cacheKey, message, provider, method)
	return message, provider, method, false, nil
}

// GetMessagesBatch 批量读取多封邮件详情 (PR-02 & PR-08: 基于规范 MessageRef 的 Identity Join，严禁按下标对齐)
func (s *MailReadService) GetMessagesBatch(ctx context.Context, accountID string, reqItems []batchMessageItemReq) ([]*mail.FullMessage, []BatchItemResult, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, nil, &BackendError{Status: 400, Code: "VALIDATION_ERROR", Message: "account_id 必填"}
	}
	if len(reqItems) == 0 {
		return []*mail.FullMessage{}, []BatchItemResult{}, nil
	}
	if len(reqItems) > 50 {
		return nil, nil, &BackendError{Status: 400, Code: "VALIDATION_ERROR", Message: "单次最多批量获取 50 封邮件"}
	}

	type pendingItem struct {
		rawRef string
		refObj mail.MessageRef
	}

	var results []BatchItemResult
	var out []*mail.FullMessage
	var pendingIMAP []pendingItem
	now := time.Now()

	// 1. 逐项解析与前置缓存匹配
	for _, m := range reqItems {
		rawRef := strings.TrimSpace(m.MessageRef)
		folder := strings.TrimSpace(m.Folder)
		if folder == "" {
			folder = "INBOX"
		}
		rawUID := strings.TrimSpace(m.UID)
		if rawUID == "" {
			rawUID = strings.TrimSpace(m.ID)
		}

		var refObj mail.MessageRef
		var refErr error

		if rawRef != "" {
			refObj, refErr = mail.ParseMessageRef(rawRef, accountID)
		} else if uid, err := strconv.ParseUint(rawUID, 10, 32); err == nil && uid > 0 {
			refObj = mail.MessageRef{
				Provider:  "imap",
				AccountID: accountID,
				Mailbox:   folder,
				UID:       uint32(uid),
			}
			rawRef = refObj.Encode()
		} else if rawUID != "" {
			refObj = mail.MessageRef{
				Provider:  "webmail",
				AccountID: accountID,
				ThreadID:  rawUID,
			}
			rawRef = refObj.Encode()
		} else {
			refErr = errors.New("missing message_ref or uid")
		}

		if refErr != nil {
			results = append(results, BatchItemResult{
				RequestedRef: rawRef,
				Error:        refErr.Error(),
			})
			continue
		}

		cacheKey := refObj.CacheKey()
		s.cacheMu.RLock()
		entry, hit := s.cache[cacheKey]
		s.cacheMu.RUnlock()

		if hit && now.Before(entry.expiresAt) {
			results = append(results, BatchItemResult{
				RequestedRef: rawRef,
				Message:      entry.msg,
			})
			out = append(out, entry.msg)
			continue
		}

		// WebMail 单条直接拉取
		if refObj.Provider == "webmail" {
			msg, err := s.be.GetMessageContext(ctx, accountID, refObj.ThreadID)
			if err != nil {
				results = append(results, BatchItemResult{
					RequestedRef: rawRef,
					Error:        err.Error(),
				})
			} else {
				msg.Provider = "webmail"
				msg.Method = "web_api"
				s.putCache(cacheKey, msg, "webmail", "web_api")
				results = append(results, BatchItemResult{
					RequestedRef: rawRef,
					Message:      msg,
				})
				out = append(out, msg)
			}
			continue
		}

		pendingIMAP = append(pendingIMAP, pendingItem{
			rawRef: rawRef,
			refObj: refObj,
		})
	}

	// 2. 批量拉取 IMAP 邮件并通过 Identity Map 严格按真实 MessageRef Join，严禁按下标绑定
	if len(pendingIMAP) > 0 {
		var imapRefs []mail.MessageRef
		for _, pi := range pendingIMAP {
			imapRefs = append(imapRefs, pi.refObj)
		}

		fetched, err := s.be.GetMessagesContext(ctx, accountID, imapRefs)
		if err != nil {
			if len(out) == 0 && len(reqItems) == len(pendingIMAP) {
				return nil, nil, err
			}
			for _, pi := range pendingIMAP {
				results = append(results, BatchItemResult{
					RequestedRef: pi.rawRef,
					Error:        err.Error(),
				})
			}
		} else {
			// 构造规范身份查找表 (Identity Map)
			// 每个 fetched message 规范化为 canonical MessageRef，以 ref.CacheKey() 为唯一主键
			idMap := make(map[string]*mail.FullMessage)
			for _, f := range fetched {
				if f == nil {
					continue
				}
				var ref mail.MessageRef
				if f.MessageRef != "" {
					if parsed, err := mail.ParseMessageRef(f.MessageRef, accountID); err == nil {
						ref = parsed
					}
				}
				if ref.AccountID == "" {
					ref.AccountID = accountID
				}
				if ref.Provider == "" {
					if f.Provider != "" {
						ref.Provider = f.Provider
					} else {
						ref.Provider = "imap"
					}
				}
				if ref.Mailbox == "" {
					ref.Mailbox = f.Folder
				}
				if strings.EqualFold(ref.Mailbox, "inbox") {
					ref.Mailbox = "INBOX"
				}
				if ref.UIDValidity == 0 {
					ref.UIDValidity = f.UIDValidity
				}
				if ref.UID == 0 {
					ref.UID = f.UID
				}
				if ref.ThreadID == "" {
					ref.ThreadID = f.ThreadID
				}
				f.MessageRef = ref.Encode()
				f.AccountID = accountID
				idMap[ref.CacheKey()] = f
			}

			// 严格按 requestedRef.CacheKey() 直接 join，严禁 accountless identity fallback
			for _, pi := range pendingIMAP {
				matched := idMap[pi.refObj.CacheKey()]
				if matched != nil {
					provider := matched.Provider
					if provider == "" {
						provider = "imap"
					}
					method := matched.Method
					if method == "" {
						method = "imap"
					}
					matched.Provider = provider
					matched.Method = method

					// 仅将真实邮件写进自己请求的规范 cacheKey，绝不串号
					s.putCache(pi.refObj.CacheKey(), matched, provider, method)
					results = append(results, BatchItemResult{
						RequestedRef: pi.rawRef,
						Message:      matched,
					})
					out = append(out, matched)
				} else {
					results = append(results, BatchItemResult{
						RequestedRef: pi.rawRef,
						Error:        "message not found",
					})
				}
			}
		}
	}

	return out, results, nil
}

func (s *MailReadService) putCache(key string, msg *mail.FullMessage, provider, method string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == nil {
		s.cache = make(map[string]messageCacheEntry)
	}
	now := time.Now()
	if len(s.cache) >= s.cacheCap {
		for k, e := range s.cache {
			if now.After(e.expiresAt) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= s.cacheCap {
			s.cache = make(map[string]messageCacheEntry)
		}
	}
	s.cache[key] = messageCacheEntry{
		msg:       msg,
		expiresAt: now.Add(s.cacheTTL),
		provider:  provider,
		method:    method,
	}
}
