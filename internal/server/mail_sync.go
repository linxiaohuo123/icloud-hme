/**
 * [INPUT]: 依赖 context, sync, time, icloud-hme/internal/account, mail, store
 * [OUTPUT]: 对外提供 MailSyncWorker, NewMailSyncWorker, ForgetAccount
 * [POS]: server 的后台邮件同步器，按结构化收件人索引活跃别名，逐项隔离损坏正文并保留失败游标，绑定物理来源和读取快照，按当前任务集合维护扫描覆盖，代际失效仅作用于边界读取前已观察的任务
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

var reProxyCredsInLog = regexp.MustCompile(`([a-zA-Z0-9+.-]+://)([^:@/]+):([^@/]+)@`)

func sanitizeLogMessage(msg string) string {
	return reProxyCredsInLog.ReplaceAllString(msg, "${1}${2}:***@")
}

// publishedWindow 是「已发布邮件」指纹的保留窗口，需覆盖 ListInbox 的检索窗口(1 天)。
const publishedWindow = 26 * time.Hour

// maxUnknownAliasProbeAccounts 限制「别名归属未知」时盲扫的账号数上限。
const maxUnknownAliasProbeAccounts = 20

// maxAliasRouteCache 限制内存路由缓存的条目数。
const maxAliasRouteCache = 50000

// PR-07 §10.2 & PR-04A F07: 账号间有界并发、慢账号超时隔离与增量扫描分页大小常量
const (
	maxConcurrentAccountSync = 5
	accountSyncTimeout       = 8 * time.Second
	verificationScanPageSize = 50
)

type checkpointKey struct {
	accountID   string
	mailbox     string
	uidValidity uint32
}

type checkpointState struct {
	NextUID        uint32
	AliasRequests  map[string]string // normalized alias -> covered active request set
	AliasBaselines map[string]uint32 // normalized alias -> covered baseline UID
}

// aggregatedInboxBatch 聚合相同物理外部收件箱配置的活跃别名批次 (PR-CONCURRENCY)
type aggregatedInboxBatch struct {
	source           string
	watches          []store.ActiveVerificationWatch
	mailboxCtx       context.Context   // 固定该批次实际读取的配置，不能每个 IMAP 阶段重新取配置
	fingerprint      string            // 物理邮箱端点唯一指纹
	repAccountID     string            // 代表母号 ID (用于发起底层的 IMAP 网络调用)
	allAliases       []string          // 聚合的所有别名列表
	aliasRealAccount map[string]string // normalized alias -> 别名真实母号 ID
}

// MailSyncWorker 后台增量邮件同步器。
type MailSyncWorker struct {
	be             Backend
	eventBus       *mail.EventBus
	store          *store.Store
	interval       time.Duration
	triggerCh      chan struct{}
	stopCh         chan struct{}
	ctx            context.Context
	cancel         context.CancelFunc
	once           sync.Once
	stopOnce       sync.Once
	wg             sync.WaitGroup
	fetchWg        sync.WaitGroup
	mu             sync.RWMutex
	aliasToAccount map[string]string                  // alias (lower) -> accountID
	published      map[string]time.Time               // "account|folder|uid|recipient" -> 首次发布时间
	probeCursor    map[string]int                     // alias -> 下一批待探测账号的索引
	probeAccounts  []string                           // 可探测账号列表，用于识别账号变动
	probeAliasNext int                                // 下一轮优先探测的别名索引
	checkpoints    map[checkpointKey]*checkpointState // (accountID, mailbox, uidValidity) -> checkpoint state (PR-04A F07)

	beforeVerificationPersistHook func(ctx context.Context, target string, uid uint32) error // test hook (PR-04B)
}

// NewMailSyncWorker 创建邮件同步器。st 可为 nil(仅退化为内存归属映射)。
func NewMailSyncWorker(be Backend, st *store.Store, eventBus *mail.EventBus, interval time.Duration) *MailSyncWorker {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	return &MailSyncWorker{
		be:             be,
		store:          st,
		eventBus:       eventBus,
		interval:       interval,
		triggerCh:      make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		ctx:            workerCtx,
		cancel:         workerCancel,
		aliasToAccount: make(map[string]string),
		published:      make(map[string]time.Time),
		probeCursor:    make(map[string]int),
		checkpoints:    make(map[checkpointKey]*checkpointState),
	}
}

// SetBeforeVerificationPersistHookForTest 设置用于测试的持久化前故障注入 hook (PR-04B)
func (w *MailSyncWorker) SetBeforeVerificationPersistHookForTest(fn func(ctx context.Context, target string, uid uint32) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.beforeVerificationPersistHook = fn
}

// markPublished 判断该封邮件是否首次针对该收件人发布，并登记指纹。
//
// 【正确性红线】ListInbox 每轮都会把 Days:1 窗口内的全部历史邮件重新返回，
// 若不做邮件级去重，worker 会在订阅者出现后 2 秒内把【上一次已经用过的验证码】
// 重新广播出去——fresh=true 也挡不住(它只跳过内存缓存预填)。因此这里必须去重。
//
// 【P0-6 修复】去重指纹必须基于规范 MessageRef.CacheKey() + recipient 确定，
// 严格包含 UIDVALIDITY，绝不可在邮箱重建后将新代际邮件误判为已发历史邮件。
func (w *MailSyncWorker) markPublished(accountID, folder string, uidValidity, uid uint32, threadID, provider, email string, source ...string) bool {
	mailbox := strings.TrimSpace(folder)
	if mailbox == "" {
		mailbox = "INBOX"
	}
	if provider == "" {
		provider = "imap"
	}
	ref := mail.MessageRef{
		Provider:    provider,
		AccountID:   accountID,
		Mailbox:     mailbox,
		UIDValidity: uidValidity,
		UID:         uid,
		ThreadID:    threadID,
	}
	key := ref.CacheKey() + "|" + strings.ToLower(strings.TrimSpace(email))
	if len(source) > 0 {
		key += "|" + source[0]
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.published == nil {
		w.published = make(map[string]time.Time)
	}
	if _, seen := w.published[key]; seen {
		return false
	}
	w.published[key] = time.Now()
	return true
}

// RegisterAliasAccount 记录别名与账号的归属映射(仅内存)。
//
// 出号链路已通过 RecordLease 写穿持久化路由表，故此处不必再写库。
func (w *MailSyncWorker) RegisterAliasAccount(alias, accountID string) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" || accountID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.putRouteLocked(alias, accountID)
}

// ForgetAccount 移除已删除账号的内存路由，避免后续扫描继续定向到不存在的账号。
func (w *MailSyncWorker) ForgetAccount(accountID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for alias, id := range w.aliasToAccount {
		if id == accountID {
			delete(w.aliasToAccount, alias)
		}
	}
	for key := range w.checkpoints {
		if key.accountID == accountID {
			delete(w.checkpoints, key)
		}
	}
}

// RegisterAliasAccounts 批量登记一批别名的归属，并写穿持久化路由表。
//
// 这是「别名归属自愈」的核心: 凡是拉取过一次某账号的别名列表(启动预热、
// GUI 浏览、手动刷新)，其全部别名都会立刻且持久地变得可路由。
func (w *MailSyncWorker) RegisterAliasAccounts(accountID string, emails []string) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || len(emails) == 0 {
		return
	}
	w.mu.Lock()
	for _, email := range emails {
		if norm := strings.ToLower(strings.TrimSpace(email)); norm != "" {
			w.putRouteLocked(norm, accountID)
		}
	}
	w.mu.Unlock()

	if w.store != nil {
		_ = w.store.UpsertAliasRoutes(accountID, emails)
	}
}

// putRouteLocked 写入内存路由缓存，超出上限时整体重置(须持 w.mu)。
func (w *MailSyncWorker) putRouteLocked(alias, accountID string) {
	if w.aliasToAccount == nil || len(w.aliasToAccount) >= maxAliasRouteCache {
		w.aliasToAccount = make(map[string]string, maxAliasRouteCache/10+1)
	}
	w.aliasToAccount[alias] = accountID
}

// GetAliasAccount 获取指定别名映射的账号 ID。
func (w *MailSyncWorker) GetAliasAccount(alias string) string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.aliasToAccount[strings.ToLower(strings.TrimSpace(alias))]
}

// GetAliasAccountOK 与 GetAliasAccount 相同，但额外返回是否已登记归属。
func (w *MailSyncWorker) GetAliasAccountOK(alias string) (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	id, ok := w.aliasToAccount[strings.ToLower(strings.TrimSpace(alias))]
	return id, ok
}

// Start 启动后台拉信协程。
func (w *MailSyncWorker) Start() {
	w.once.Do(func() {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.loop()
		}()
	})
}

// Stop 停止同步协程并平稳等待在途轮次与网络调用完全结束 (线程安全且幂等，PR-07 §10.4 & PR-08 Final Hardening §3)。
func (w *MailSyncWorker) Stop() {
	w.stopOnce.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
		close(w.stopCh)
	})
	w.wg.Wait()
	w.fetchWg.Wait()
}

// Trigger 立即唤醒同步器执行一轮同步 (非阻塞)。
func (w *MailSyncWorker) Trigger() {
	if w == nil {
		return
	}
	select {
	case w.triggerCh <- struct{}{}:
	default:
	}
}

// loop 定时执行批量拉信与事件分发。
func (w *MailSyncWorker) loop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	// 【BUG-03 修复】独立定时器清理过期指纹,避免在高频 markPublished 写锁内做 O(N) 遍历
	cleanupTicker := time.NewTicker(5 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-w.triggerCh:
			w.syncOnce()
		case <-ticker.C:
			w.syncOnce()
		case <-cleanupTicker.C:
			w.cleanupPublished()
			if w.eventBus != nil {
				w.eventBus.CleanupCache()
			}
		}
	}
}

// cleanupPublished 批量淘汰过期的已发布指纹,防止长期运行内存无界增长。
func (w *MailSyncWorker) cleanupPublished() {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, t := range w.published {
		if now.Sub(t) > publishedWindow {
			delete(w.published, k)
		}
	}
}

// syncOnce 执行单轮增量邮件同步。
func (w *MailSyncWorker) syncOnce() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC RECOVER] mail_sync.syncOnce: %v", r)
		}
	}()

	// PR-04B: 扫描需求来源为 EventBus subscribers UNION 数据库中未过期的活跃 verification requests
	hasSubscribers := w.eventBus.HasSubscribers()
	var persistentAliases []string
	var watches []store.ActiveVerificationWatch
	if w.store != nil {
		var err error
		watches, err = w.store.ListActiveVerificationWatches(w.ctx)
		if err != nil {
			if w.ctx.Err() == nil {
				log.Printf("[MailSync] 读取活跃取码任务失败 (禁止当作零任务丢弃): %v", err)
			}
			return
		}
		for _, watch := range watches {
			persistentAliases = append(persistentAliases, watch.AliasEmail)
		}
	}

	if !hasSubscribers && len(persistentAliases) == 0 {
		return
	}

	watchedSet := make(map[string]struct{})
	for _, email := range w.eventBus.SubscribedEmails() {
		norm := strings.ToLower(strings.TrimSpace(email))
		if norm != "" {
			watchedSet[norm] = struct{}{}
		}
	}
	for _, email := range persistentAliases {
		norm := strings.ToLower(strings.TrimSpace(email))
		if norm != "" {
			watchedSet[norm] = struct{}{}
		}
	}
	if len(watchedSet) == 0 {
		return
	}

	accounts := w.be.ListAccounts()
	if len(accounts) == 0 {
		return
	}

	// 1. 按已知归属账号对待查别名进行分组聚合，消灭 N x M 广播风暴
	accountQueries := make(map[string][]string) // accountID -> []alias
	var unknownAliases []string

	w.mu.RLock()
	for email := range watchedSet {
		if accID, ok := w.aliasToAccount[email]; ok {
			accountQueries[accID] = append(accountQueries[accID], email)
		} else {
			unknownAliases = append(unknownAliases, email)
		}
	}
	w.mu.RUnlock()

	// 1b. 内存未命中时查持久化路由表(alias_routes 主键点查)，命中即升级为定向拉取。
	if len(unknownAliases) > 0 && w.store != nil {
		remain := unknownAliases[:0]
		for _, alias := range unknownAliases {
			accID, err := w.resolveAccountFromStoreContext(w.ctx, alias)
			if err == nil && accID != "" {
				w.RegisterAliasAccount(alias, accID)
				accountQueries[accID] = append(accountQueries[accID], alias)
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				// 数据库查询异常时显式记录，严禁将存储故障当成未知别名降级进入盲探
				log.Printf("[MailSync] 解析别名 %s 归属母号失败(DB异常): %v", alias, err)
				continue
			}
			remain = append(remain, alias)
		}
		unknownAliases = remain
	}

	// 2. 将待查母号按照底层物理收件箱配置进行同源聚合，消灭多母号共享同一个 Gmail 时的重复扫描与连接池串行排队 (PR-CONCURRENCY)
	inboxBatches := make(map[string]*aggregatedInboxBatch)
	for accID, aliases := range accountQueries {
		mailboxCtx, source, fp, err := w.be.CaptureMailboxContext(w.ctx, accID)
		if err != nil {
			if w.ctx.Err() == nil {
				log.Printf("[MailSync] 固定邮箱配置失败 account=%s: %v", accID, err)
			}
			continue
		}
		if fp == "" {
			fp = "single|" + accID
		}
		batch, exists := inboxBatches[fp]
		if !exists {
			batch = &aggregatedInboxBatch{
				source:           source,
				watches:          watches,
				mailboxCtx:       mailboxCtx,
				fingerprint:      fp,
				repAccountID:     accID,
				aliasRealAccount: make(map[string]string),
			}
			inboxBatches[fp] = batch
		}
		for _, alias := range aliases {
			batch.allAliases = append(batch.allAliases, alias)
			batch.aliasRealAccount[alias] = accID
		}
	}

	sem := make(chan struct{}, maxConcurrentAccountSync)
	var fetchWg sync.WaitGroup

inboxLoop:
	for _, batch := range inboxBatches {
		if w.ctx.Err() != nil {
			break inboxLoop
		}
		batch := batch
		select {
		case <-w.ctx.Done():
			break inboxLoop
		case sem <- struct{}{}:
			accountCtx, accountCancel := context.WithTimeout(mail.WithBackgroundOp(batch.mailboxCtx), accountSyncTimeout)
			fetchWg.Add(1)
			w.fetchWg.Add(1)
			go func() {
				defer func() {
					accountCancel()
					<-sem
					w.fetchWg.Done()
					fetchWg.Done()
					if r := recover(); r != nil {
						log.Printf("[PANIC RECOVER] mail_sync.fetchAndPublishAggregatedBatch for %s (%s): %v", batch.fingerprint, batch.repAccountID, r)
					}
				}()
				w.fetchAndPublishAggregatedBatch(accountCtx, batch)
			}()
		}
	}
	fetchWg.Wait()

	// 3. 仍无法归属的别名按批轮转探测；监听期间新邮件可能随时到达。
	unknownSet := make(map[string]struct{}, len(unknownAliases))
	for _, alias := range unknownAliases {
		unknownSet[alias] = struct{}{}
	}
	w.mu.Lock()
	for alias := range w.probeCursor {
		if _, watched := unknownSet[alias]; !watched {
			delete(w.probeCursor, alias)
		}
	}
	w.mu.Unlock()
	if len(unknownAliases) > 0 {
		if w.ctx.Err() != nil {
			return
		}
		var eligible []account.Summary
		var accountIDs []string
		for _, acc := range accounts {
			if acc.HasAppPassword || acc.HasCookies || acc.Mailbox != nil {
				eligible = append(eligible, acc)
				accountIDs = append(accountIDs, acc.ID)
			}
		}
		if len(eligible) == 0 {
			return
		}
		w.mu.Lock()
		if !slices.Equal(w.probeAccounts, accountIDs) {
			w.probeAccounts = accountIDs
			w.probeCursor = make(map[string]int)
		}
		w.mu.Unlock()
		sort.Strings(unknownAliases)
		w.mu.Lock()
		aliasStart := w.probeAliasNext % len(unknownAliases)
		w.probeAliasNext = (aliasStart + 1) % len(unknownAliases)
		w.mu.Unlock()
		type probeQuery struct {
			alias     string
			accountID string
			result    InboxResult
			err       error
		}
		queries := make([]*probeQuery, 0, maxUnknownAliasProbeAccounts)
		budget := maxUnknownAliasProbeAccounts
		for n := 0; n < len(unknownAliases) && budget > 0; n++ {
			alias := unknownAliases[(aliasStart+n)%len(unknownAliases)]
			w.mu.RLock()
			start := w.probeCursor[alias]
			w.mu.RUnlock()
			batchSize := min(budget, len(eligible)-start)
			for i := 0; i < batchSize; i++ {
				queries = append(queries, &probeQuery{alias: alias, accountID: eligible[start+i].ID})
			}
			budget -= batchSize
			w.mu.Lock()
			w.probeCursor[alias] = (start + batchSize) % len(eligible)
			w.mu.Unlock()
		}
		tasks := make([]func(), 0, len(queries))
		for _, query := range queries {
			q := query
			tasks = append(tasks, func() {
				probeCtx, cancel := context.WithTimeout(w.ctx, accountSyncTimeout)
				defer cancel()
				q.result, q.err = w.be.ListInboxContext(probeCtx, InboxQuery{
					AccountID: q.accountID,
					Alias:     q.alias,
					Folder:    "all",
					Limit:     5,
					Days:      1,
				})
			})
		}
		runBounded(maxConcurrentAccountSync, "mail_sync.probeAlias", tasks)
		resolved := make(map[string]struct{})
		for _, query := range queries {
			if _, ok := resolved[query.alias]; ok {
				continue
			}
			if query.err != nil {
				if w.ctx.Err() == nil {
					log.Printf("[MailSync] 探测账号 %s 失败: %v", query.accountID, query.err)
				}
				continue
			}
			for _, msg := range query.result.Messages {
				if msgMatchesRecipient(msg, query.alias) {
					w.RegisterAliasAccounts(query.accountID, []string{query.alias})
					w.mu.Lock()
					delete(w.probeCursor, query.alias)
					w.mu.Unlock()
					resolved[query.alias] = struct{}{}
					accountCtx, cancel := context.WithTimeout(w.ctx, accountSyncTimeout)
					w.fetchAndPublishBatch(accountCtx, query.accountID, []string{query.alias})
					cancel()
					break
				}
			}
		}
	}
}

// fetchAndPublishBatch 按账号增量批量拉取邮件 (单账号兼容封装)。
func (w *MailSyncWorker) fetchAndPublishBatch(ctx context.Context, accountID string, aliases []string) bool {
	batch := &aggregatedInboxBatch{
		fingerprint:      "single|" + accountID,
		repAccountID:     accountID,
		allAliases:       aliases,
		aliasRealAccount: make(map[string]string),
	}
	for _, a := range aliases {
		batch.aliasRealAccount[a] = accountID
	}
	return w.fetchAndPublishAggregatedBatch(ctx, batch)
}

func (w *MailSyncWorker) fetchAndPublishBatchResult(ctx context.Context, accountID string, aliases []string) (bool, error) {
	batch := &aggregatedInboxBatch{
		fingerprint:      "single|" + accountID,
		repAccountID:     accountID,
		allAliases:       aliases,
		aliasRealAccount: make(map[string]string),
	}
	for _, a := range aliases {
		batch.aliasRealAccount[a] = accountID
	}
	return w.fetchAndPublishAggregatedBatchResult(ctx, batch)
}

// fetchAndPublishAggregatedBatch 针对聚合同源物理外部收件箱的活跃别名批次执行批量同步 (PR-CONCURRENCY)。
func (w *MailSyncWorker) fetchAndPublishAggregatedBatch(ctx context.Context, batch *aggregatedInboxBatch) bool {
	matched, err := w.fetchAndPublishAggregatedBatchResult(ctx, batch)
	if err != nil && ctx.Err() == nil {
		log.Printf("[MailSync] 物理邮箱 %s (代表母号 %s) 同步失败: %v", batch.fingerprint, batch.repAccountID, err)
	}
	return matched
}

func (w *MailSyncWorker) fetchAndPublishAggregatedBatchResult(ctx context.Context, batch *aggregatedInboxBatch) (bool, error) {
	if len(batch.allAliases) == 0 {
		return false, nil
	}
	ctx = mail.WithBackgroundOp(ctx)
	if batch.mailboxCtx == nil {
		if w.store != nil {
			var err error
			batch.watches, err = w.store.ListActiveVerificationWatches(ctx)
			if err != nil {
				return false, err
			}
		}
		mailboxCtx, source, _, err := w.be.CaptureMailboxContext(ctx, batch.repAccountID)
		if err != nil {
			return false, err
		}
		ctx, batch.source = mailboxCtx, source
	}
	if w.store != nil {
		var ids []string
		for _, watch := range batch.watches {
			alias := strings.ToLower(strings.TrimSpace(watch.AliasEmail))
			if _, ok := batch.aliasRealAccount[alias]; ok {
				ids = append(ids, watch.RequestID)
			}
		}
		if err := w.store.InvalidateVerificationRequestsForSourceMismatch(ctx, ids, batch.source); err != nil {
			return false, err
		}
	}

	var strictAliases, legacyAliases []string
	currentBaselines := make(map[string]uint32)
	var globalMinUID uint32
	var scanErr error

	if w.store != nil {
		// 批量拉取活跃别名的最小基线，彻底消除逐别名 N+1 次 SQL 查询
		baselines, _, err := w.store.GetVerificationScanBaselines(ctx, batch.allAliases, batch.source)
		if err != nil {
			return false, fmt.Errorf("批量查询基线失败: %w", err)
		}
		for _, alias := range batch.allAliases {
			norm := strings.ToLower(strings.TrimSpace(alias))
			if norm == "" {
				continue
			}
			uid := baselines[norm]
			if uid > 0 {
				strictAliases = append(strictAliases, norm)
				currentBaselines[norm] = uid
				if globalMinUID == 0 || uid < globalMinUID {
					globalMinUID = uid
				}
			} else {
				legacyAliases = append(legacyAliases, norm)
			}
		}
	} else {
		for _, alias := range batch.allAliases {
			norm := strings.ToLower(strings.TrimSpace(alias))
			if norm != "" {
				legacyAliases = append(legacyAliases, norm)
			}
		}
	}

	var strictMatched, legacyMatched bool
	if len(strictAliases) > 0 && globalMinUID > 0 {
		var err error
		strictMatched, err = w.scanAndPublishPagesAggregated(ctx, batch, strictAliases, currentBaselines, "INBOX", globalMinUID)
		if err != nil {
			scanErr = err
		}
	}
	if len(legacyAliases) > 0 {
		var err error
		legacyMatched, err = w.fetchAndPublishLegacyBatch(ctx, batch, legacyAliases)
		if err != nil {
			scanErr = err
		}
	}
	return strictMatched || legacyMatched, scanErr
}

// fetchAndPublishLegacyBatch 针对无 baseline 的旧模式订阅者执行基于 ListInboxContext 的单批拉取
func (w *MailSyncWorker) fetchAndPublishLegacyBatch(ctx context.Context, batch *aggregatedInboxBatch, aliases []string) (bool, error) {
	if len(aliases) == 0 {
		return false, nil
	}
	accountID := batch.repAccountID
	var q InboxQuery
	if len(aliases) == 1 {
		q = InboxQuery{
			AccountID: accountID,
			Alias:     aliases[0],
			Folder:    "all",
			Limit:     5,
			Days:      1,
			WithBody:  true,
		}
	} else {
		q = InboxQuery{
			AccountID: accountID,
			Folder:    "all",
			Limit:     10,
			Days:      1,
			WithBody:  true,
		}
	}

	res, err := w.be.ListInboxContext(ctx, q)
	if err != nil {
		return false, err
	}

	aliasSet := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		aliasSet[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}

	hasMatch := false
	found := make(map[string]bool, len(aliases))
	process := func(messages []mail.Message) error {
		for _, msg := range messages {
			bodyText := msg.Preview
			if bodyText == "" {
				bodyText = msg.Body
			}
			otp := mail.ExtractOTP(msg.Subject, bodyText)
			if otp == nil {
				continue
			}

			for _, target := range msg.RecipientAddresses() {
				if _, watched := aliasSet[target]; !watched {
					continue
				}
				found[target] = true
				realAccID := batch.aliasRealAccount[target]
				if realAccID == "" {
					realAccID = accountID
				}
				targetRef := mail.MessageRef{
					Provider:    msg.Provider,
					AccountID:   realAccID,
					Mailbox:     msg.Folder,
					UIDValidity: msg.UIDValidity,
					UID:         msg.UID,
					ThreadID:    msg.ThreadID,
				}
				targetMsgRef := targetRef.Encode()

				if w.store != nil {
					completedReqs, err := w.store.CompleteMatchingVerificationRequests(ctx, store.VerificationEventInput{
						Source:      batch.source,
						AliasEmail:  target,
						Provider:    msg.Provider,
						Mailbox:     msg.Folder,
						UIDValidity: msg.UIDValidity,
						UID:         msg.UID,
						MessageRef:  targetMsgRef,
						Code:        otp.Code,
						MagicLink:   otp.MagicLink,
						Now:         time.Now().UTC(),
					})
					if err != nil {
						return err
					}
					if len(completedReqs) > 0 {
						hasMatch = true
					}
				}
				if !w.markPublished(realAccID, msg.Folder, msg.UIDValidity, msg.UID, msg.ThreadID, msg.Provider, target, batch.source) {
					continue
				}
				w.eventBus.PublishEvent(&mail.CachedOTP{
					Source:      batch.source,
					EventID:     targetMsgRef,
					AccountID:   realAccID,
					Email:       target,
					Folder:      msg.Folder,
					UIDValidity: msg.UIDValidity,
					UID:         msg.UID,
					OTP:         otp,
					Subject:     msg.Subject,
					From:        msg.From,
					Date:        msg.Date,
				})
				hasMatch = true
			}
		}
		return nil
	}
	if err := process(res.Messages); err != nil {
		return hasMatch, err
	}
	// 共享窗口截满时，对未命中别名做定向查询，避免其他别名的新邮件挤掉验证码。
	if len(aliases) > 1 && len(res.Messages) == q.Limit {
		for _, alias := range aliases {
			if found[alias] {
				continue
			}
			q.Alias = alias
			q.Limit = 5
			aliasRes, err := w.be.ListInboxContext(ctx, q)
			if err != nil {
				return hasMatch, err
			}
			if err := process(aliasRes.Messages); err != nil {
				return hasMatch, err
			}
		}
	}
	return hasMatch, nil
}

// scanAndPublishPagesAggregated 针对聚合同源物理外部收件箱执行增量分页扫描并精准分发给真实母号 (PR-CONCURRENCY)。
func (w *MailSyncWorker) scanAndPublishPagesAggregated(ctx context.Context, batch *aggregatedInboxBatch, aliases []string, currentBaselines map[string]uint32, folder string, baselineUID uint32) (bool, error) {
	if len(aliases) == 0 || baselineUID == 0 {
		return false, nil
	}
	if folder == "" {
		folder = "INBOX"
	}

	accountID := batch.repAccountID

	// 代际失效只能作用于读取边界前已观察到的任务；边界之后创建的任务可能已采集到更新代际。
	if w.store != nil && batch.watches == nil {
		watches, err := w.store.ListActiveVerificationWatches(ctx)
		if err != nil {
			return false, err
		}
		batch.watches = watches
	}
	observedAliases := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		observedAliases[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}
	var observedIDs []string
	for _, watch := range batch.watches {
		if _, ok := observedAliases[strings.ToLower(strings.TrimSpace(watch.AliasEmail))]; ok {
			observedIDs = append(observedIDs, watch.RequestID)
		}
	}

	// 1. 固定 upper bound: 每轮 account scan 开始时获取 UIDVALIDITY 与 UIDNEXT (RFC 3501 UIDNEXT-1)
	provider, uidValidity, uidNext, err := w.be.GetMailboxBoundaryContext(ctx, accountID, folder)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[MailSync] 获取物理邮箱 %s (母号 %s) 邮箱边界失败: %v", batch.fingerprint, accountID, sanitizeLogMessage(err.Error()))
		}
		return false, err
	}
	if provider != "imap" || uidValidity == 0 {
		return false, fmt.Errorf("账号 %s 的邮箱边界不可用于 IMAP 扫描", accountID)
	}

	// 2. Resumable checkpoint 决策与跨代际失效清理 (以物理收件箱端点指纹为粒度，杜绝同一 Gmail 轮换代表母号时重复扫描)
	accountKey := batch.fingerprint
	if accountKey == "" || strings.HasPrefix(accountKey, "single|") {
		accountKey = batch.repAccountID
	}
	cpKey := checkpointKey{
		accountID:   accountKey,
		mailbox:     folder,
		uidValidity: uidValidity,
	}

	w.mu.Lock()
	for k := range w.checkpoints {
		if k.accountID == accountKey && k.mailbox == folder && k.uidValidity != uidValidity {
			delete(w.checkpoints, k)
		}
	}

	cp, exists := w.checkpoints[cpKey]
	if !exists {
		cp = &checkpointState{
			NextUID:        baselineUID,
			AliasBaselines: make(map[string]uint32, len(currentBaselines)),
			AliasRequests:  make(map[string]string),
		}
		w.checkpoints[cpKey] = cp
	} else if cp.AliasBaselines == nil {
		cp.AliasBaselines = make(map[string]uint32)
	}
	w.mu.Unlock()

	currentRequests := make(map[string]string)
	if cp.AliasRequests == nil {
		cp.AliasRequests = make(map[string]string)
	}
	// 3. 代际突变清理与基线重新校准 (使用批量接口)
	if w.store != nil {
		if _, err := w.store.InvalidateVerificationRequestsForGenerationMismatchByIDs(ctx, observedIDs, folder, uidValidity, batch.source); err != nil {
			return false, err
		}
		baselines, requests, err := w.store.GetVerificationScanBaselines(ctx, aliases, batch.source)
		if err != nil {
			return false, err
		}
		currentRequests = requests
		var activeAliases []string
		newBaselines := make(map[string]uint32, len(aliases))
		var newMinBaseline uint32
		for _, alias := range aliases {
			uid := baselines[alias]
			if uid > 0 {
				activeAliases = append(activeAliases, alias)
				newBaselines[alias] = uid
				if newMinBaseline == 0 || uid < newMinBaseline {
					newMinBaseline = uid
				}
			}
		}
		aliases = activeAliases
		currentBaselines = newBaselines
		baselineUID = newMinBaseline
		if len(aliases) == 0 || baselineUID == 0 {
			return false, nil
		}
	}

	if uidNext <= 1 {
		return false, nil
	}
	scanUpperUID := uidNext - 1

	w.mu.Lock()
	cursor := cp.NextUID
	rewound := false

	for alias, curB := range currentBaselines {
		coveredB, covered := cp.AliasBaselines[alias]
		if !covered || curB != coveredB || cp.AliasRequests[alias] != currentRequests[alias] {
			if curB < cursor {
				cursor = curB
				rewound = true
			}
		}
	}
	// 历史任务的覆盖不能证明后来任务已扫描；同时回收已离开观察集合的条目。
	for alias := range cp.AliasBaselines {
		if _, active := currentBaselines[alias]; !active {
			delete(cp.AliasBaselines, alias)
			delete(cp.AliasRequests, alias)
		}
	}

	if !rewound && baselineUID > cursor {
		cursor = baselineUID
		rewound = true
	}

	if rewound {
		cp.NextUID = cursor
	}
	w.mu.Unlock()

	if cursor > scanUpperUID {
		return false, nil
	}

	aliasSet := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		aliasSet[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}

	hasMatch := false
	var scanErr error

	// 4. 分页增量扫描循环 (UID 从旧到新 UID ascending，pageSize=50)
	for cursor <= scanUpperUID {
		if err := ctx.Err(); err != nil {
			scanErr = err
			break
		}

		pageRes, err := w.be.ScanMailboxUIDPage(ctx, ScanPageQuery{
			AccountID:        accountID,
			Folder:           folder,
			UIDValidity:      uidValidity,
			FromUIDInclusive: cursor,
			ToUIDInclusive:   scanUpperUID,
			PageSize:         verificationScanPageSize,
		})
		if err != nil {
			scanErr = err
			if errors.Is(err, mail.ErrUIDValidityMismatch) {
				w.mu.Lock()
				delete(w.checkpoints, cpKey)
				w.mu.Unlock()
			}
			break
		}
		if pageRes.UIDValidity != uidValidity {
			scanErr = mail.ErrUIDValidityMismatch
			w.mu.Lock()
			delete(w.checkpoints, cpKey)
			w.mu.Unlock()
			break
		}

		if len(pageRes.Messages) == 0 {
			w.mu.Lock()
			cp.NextUID = scanUpperUID + 1
			for alias, curB := range currentBaselines {
				if curB <= scanUpperUID {
					cp.AliasBaselines[alias] = curB
					cp.AliasRequests[alias] = currentRequests[alias]
				}
			}
			w.mu.Unlock()
			break
		}

		// 第一阶段: Metadata-first，对活跃 aliases 做结构化收件人匹配，筛选 candidate UID
		var candidateUIDs []uint32
		for _, msg := range pageRes.Messages {
			for _, recipient := range msg.RecipientAddresses() {
				if _, watched := aliasSet[recipient]; watched {
					candidateUIDs = append(candidateUIDs, msg.UID)
					break
				}
			}
		}

		// 第二阶段: 仅对 candidate UID 单批拉取正文并提取验证码
		if len(candidateUIDs) > 0 {
			refs := make([]mail.MessageRef, 0, len(candidateUIDs))
			for _, u := range candidateUIDs {
				refs = append(refs, mail.MessageRef{
					Provider:    "imap",
					AccountID:   accountID,
					Mailbox:     folder,
					UIDValidity: uidValidity,
					UID:         u,
				})
			}
			fullMsgs, err := w.be.GetMessagesContext(ctx, accountID, refs)
			var readErr *mail.BatchReadError
			if err != nil && !errors.As(err, &readErr) {
				scanErr = err
				break
			}
			if readErr != nil {
				scanErr = readErr
				retryable := false
				for _, failure := range readErr.Failures {
					if !failure.Permanent {
						retryable = true
						continue
					}
					log.Printf("[MailSync] 邮件正文不可解析 account=%s mailbox=%s uid_validity=%d uid=%d: %v", accountID, failure.Ref.Mailbox, failure.Ref.UIDValidity, failure.Ref.UID, failure.Err)
				}
				if retryable {
					break
				}
			}

			pageDurableFailed := false
			for _, fullMsg := range fullMsgs {
				if fullMsg == nil {
					continue
				}
				bodyText := fullMsg.Preview
				if bodyText == "" {
					bodyText = fullMsg.Body
				}
				otp := mail.ExtractOTP(fullMsg.Subject, bodyText)
				if otp == nil {
					continue
				}

				for _, target := range fullMsg.RecipientAddresses() {
					if _, watched := aliasSet[target]; !watched {
						continue
					}

					// Hook: 供测试注入在持久化前的故障 (PR-04B)
					w.mu.RLock()
					hook := w.beforeVerificationPersistHook
					w.mu.RUnlock()
					if hook != nil {
						if err := hook(ctx, target, fullMsg.UID); err != nil {
							log.Printf("[MailSync] 测试故障注入失败 (%s UID=%d): %v", target, fullMsg.UID, err)
							scanErr = err
							pageDurableFailed = true
							break
						}
					}

					// 确保业务交付归属于真实的别名母号，严防跨母号串码 (PR-CONCURRENCY)
					realAccID := batch.aliasRealAccount[target]
					if realAccID == "" {
						realAccID = accountID
					}

					// 严禁就地修改多个目标别名共享的 fullMsg，根据真实母号构造专属 MessageRef
					targetRef := mail.MessageRef{
						Provider:    "imap",
						AccountID:   realAccID,
						Mailbox:     folder,
						UIDValidity: fullMsg.UIDValidity,
						UID:         fullMsg.UID,
					}
					targetMsgRef := targetRef.Encode()

					// PR-04B 核心规则: 先写 verification_requests durable result，成功后再做 EventBus Publish。
					if w.store != nil {
						completedReqs, err := w.store.CompleteMatchingVerificationRequests(ctx, store.VerificationEventInput{
							Source:      batch.source,
							AliasEmail:  target,
							Provider:    "imap",
							Mailbox:     folder,
							UIDValidity: fullMsg.UIDValidity,
							UID:         fullMsg.UID,
							MessageRef:  targetMsgRef,
							Code:        otp.Code,
							MagicLink:   otp.MagicLink,
							Now:         time.Now().UTC(),
						})
						if err != nil {
							log.Printf("[MailSync] 持久化验证码终态失败 (%s UID=%d): %v", target, fullMsg.UID, err)
							scanErr = err
							pageDurableFailed = true
							break
						}
						if len(completedReqs) > 0 {
							hasMatch = true
						}
					}

					// 数据库落库成功后，使用真实母号 ID 进行发布与去重
					if w.markPublished(realAccID, fullMsg.Folder, fullMsg.UIDValidity, fullMsg.UID, fullMsg.ThreadID, fullMsg.Provider, target, batch.source) {
						w.eventBus.PublishEvent(&mail.CachedOTP{
							Source:      batch.source,
							EventID:     targetMsgRef,
							AccountID:   realAccID, // 严格交付真实母号 ID
							Email:       target,
							Folder:      fullMsg.Folder,
							UIDValidity: fullMsg.UIDValidity,
							UID:         fullMsg.UID,
							OTP:         otp,
							Subject:     fullMsg.Subject,
							From:        fullMsg.From,
							Date:        fullMsg.Date,
						})
						hasMatch = true
					}
				}
				if pageDurableFailed {
					break
				}
			}

			if pageDurableFailed {
				break
			}
		}

		maxUIDInPage := pageRes.Messages[len(pageRes.Messages)-1].UID
		nextCursor := maxUIDInPage + 1
		if pageRes.NextUID > nextCursor {
			nextCursor = pageRes.NextUID
		}
		cursor = nextCursor

		w.mu.Lock()
		cp.NextUID = cursor
		for alias, curB := range currentBaselines {
			if curB < cursor {
				cp.AliasBaselines[alias] = curB
				cp.AliasRequests[alias] = currentRequests[alias]
			}
		}
		w.mu.Unlock()

		if !pageRes.HasMore || cursor > scanUpperUID {
			break
		}
	}

	return hasMatch, scanErr
}

// resolveAccountFromStoreContext 按「持久化路由表 → 出号流水」的顺序解析别名归属，传递 Context 并区分真实故障 (R07)。
func (w *MailSyncWorker) resolveAccountFromStoreContext(ctx context.Context, alias string) (string, error) {
	if w.store == nil {
		return "", sql.ErrNoRows
	}
	accountID, err := w.store.FindAliasRouteContext(ctx, alias)
	if err == nil && accountID != "" {
		return accountID, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	accountID, err = w.store.FindLeaseAccountContext(ctx, alias)
	if err == nil && accountID != "" {
		_ = w.store.UpsertAliasRoutes(accountID, []string{alias})
		return accountID, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return "", sql.ErrNoRows
}

// msgMatchesRecipient 核验邮件是否明确发给目标别名 (通过 To 或信封收件人头)。
func msgMatchesRecipient(msg mail.Message, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return false
	}
	for _, r := range msg.RecipientAddresses() {
		if r == target {
			return true
		}
	}
	return false
}
