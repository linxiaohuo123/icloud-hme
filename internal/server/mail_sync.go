/**
 * [INPUT]: 依赖 sync, time, net/mail, icloud-hme/internal/mail, icloud-hme/internal/store
 * [OUTPUT]: 对外提供 MailSyncWorker, NewMailSyncWorker
 * [POS]: server 的后台邮件同步器 (PR-07 §10.2 & §10.4, PR-04A F07)，实现同账号增量 UID 升序分页扫描、固定上界、resumable checkpoint、metadata-first 过滤与正文按需批量拉取
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"log"
	stdmail "net/mail"
	"strings"
	"sync"
	"time"

	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// publishedWindow 是「已发布邮件」指纹的保留窗口，需覆盖 ListInbox 的检索窗口(1 天)。
const publishedWindow = 26 * time.Hour

// maxUnknownAliasProbeAccounts 限制「别名归属未知」时盲扫的账号数上限。
const maxUnknownAliasProbeAccounts = 20

// unknownAliasMissTTL 是盲扫失败的负缓存时长，避免每轮重复扫同一批账号。
const unknownAliasMissTTL = 10 * time.Minute

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
	AliasBaselines map[string]uint32 // normalized alias -> covered baseline UID
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
	aliasToAccount map[string]string                   // alias (lower) -> accountID
	published      map[string]time.Time                // "account|folder|uid|recipient" -> 首次发布时间
	probeMiss      map[string]time.Time                // alias -> 上次盲扫未命中的时间
	checkpoints    map[checkpointKey]*checkpointState // (accountID, mailbox, uidValidity) -> checkpoint state (PR-04A F07)
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
		probeMiss:      make(map[string]time.Time),
		checkpoints:    make(map[checkpointKey]*checkpointState),
	}
}

// markPublished 判断该封邮件是否首次针对该收件人发布，并登记指纹。
//
// 【正确性红线】ListInbox 每轮都会把 Days:1 窗口内的全部历史邮件重新返回，
// 若不做邮件级去重，worker 会在订阅者出现后 2 秒内把【上一次已经用过的验证码】
// 重新广播出去——fresh=true 也挡不住(它只跳过内存缓存预填)。因此这里必须去重。
//
// 【P0-6 修复】去重指纹必须基于规范 MessageRef.CacheKey() + recipient 确定，
// 严格包含 UIDVALIDITY，绝不可在邮箱重建后将新代际邮件误判为已发历史邮件。
func (w *MailSyncWorker) markPublished(accountID, folder string, uidValidity, uid uint32, threadID, provider, email string) bool {
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

	// 【安全与节流铁律】：仅在外部有活跃订阅等待验证码时才轮询，
	// 杜绝 7x24 小时每两秒狂刷 Apple 接口导致大号 Session 或 IP 被风控封锁。
	if !w.eventBus.HasSubscribers() {
		return
	}

	subscribedEmails := w.eventBus.SubscribedEmails()
	accounts := w.be.ListAccounts()
	if len(accounts) == 0 {
		return
	}

	// 1. 按已知归属账号对待查别名进行分组聚合，消灭 N x M 广播风暴
	accountQueries := make(map[string][]string) // accountID -> []alias
	var unknownAliases []string

	w.mu.RLock()
	for _, email := range subscribedEmails {
		norm := strings.ToLower(strings.TrimSpace(email))
		if accID, ok := w.aliasToAccount[norm]; ok {
			accountQueries[accID] = append(accountQueries[accID], norm)
		} else {
			unknownAliases = append(unknownAliases, norm)
		}
	}
	w.mu.RUnlock()

	// 1b. 内存未命中时查持久化路由表(alias_routes 主键点查)，命中即升级为定向拉取。
	//     这一步让"进程重启后内存表为空"不再退化成盲扫。
	if len(unknownAliases) > 0 && w.store != nil {
		remain := unknownAliases[:0]
		for _, alias := range unknownAliases {
			if accID, ok := w.resolveAccountFromStore(alias); ok {
				w.RegisterAliasAccount(alias, accID)
				accountQueries[accID] = append(accountQueries[accID], alias)
				continue
			}
			remain = append(remain, alias)
		}
		unknownAliases = remain
	}

	// 2. 对已知归属账号进行有界并发批量拉取与慢账号隔离 (PR-07 §10.2 & PR-08 Final Hardening §3)
	sem := make(chan struct{}, maxConcurrentAccountSync)
	var fetchWg sync.WaitGroup

accountLoop:
	for accID, aliases := range accountQueries {
		if w.ctx.Err() != nil {
			break accountLoop
		}
		accID := accID
		aliases := aliases
		select {
		case <-w.ctx.Done():
			break accountLoop
		case sem <- struct{}{}:
			accountCtx, accountCancel := context.WithTimeout(w.ctx, accountSyncTimeout)
			fetchWg.Add(1)
			w.fetchWg.Add(1)
			go func() {
				defer func() {
					accountCancel()
					<-sem
					w.fetchWg.Done()
					fetchWg.Done()
					if r := recover(); r != nil {
						log.Printf("[PANIC RECOVER] mail_sync.fetchAndPublishBatch for %s: %v", accID, r)
					}
				}()
				w.fetchAndPublishBatch(accountCtx, accID, aliases)
			}()
		}
	}
	fetchWg.Wait()

	// 3. 仍无法归属的野别名(纯粹在 Apple 侧手工创建、本系统从未见过):
	//    做「有上限 + 负缓存」的盲扫兜底。外部普通请求已在 HTTP 鉴权层拦截，无法触发未知别名。
	if len(unknownAliases) > 0 {
		if w.ctx.Err() != nil {
			return
		}
		var probeList []string
		for _, alias := range unknownAliases {
			if !w.isRecentProbeMiss(alias) {
				probeList = append(probeList, alias)
			}
		}
		if len(probeList) > 0 {
			probed := 0
		probeAccLoop:
			for _, acc := range accounts {
				if w.ctx.Err() != nil {
					break probeAccLoop
				}
				if probed >= maxUnknownAliasProbeAccounts {
					break
				}
				if !acc.HasAppPassword && !acc.HasCookies {
					continue
				}
				probed++
				for _, alias := range probeList {
					if err := w.ctx.Err(); err != nil {
						break probeAccLoop
					}
					probeCtx, probeCancel := context.WithTimeout(w.ctx, accountSyncTimeout)
					matched := w.fetchAndPublish(probeCtx, acc.ID, alias)
					probeCancel()
					if matched {
						// 盲扫发现归属: 写穿路由表，此后不再需要盲扫
						w.RegisterAliasAccounts(acc.ID, []string{alias})
					}
				}
			}
			// 仅对仍未归属的别名记负缓存，避免下一轮重复扫同一批账号
			for _, alias := range probeList {
				if _, known := w.GetAliasAccountOK(alias); !known {
					w.noteProbeMiss(alias)
				}
			}
		}
	}
}

// fetchAndPublishBatch 按账号增量批量拉取邮件并分发给多个别名等待者 (PR-07 §10.2 & PR-08 Final Hardening §3 & PR-04A F07)。
func (w *MailSyncWorker) fetchAndPublishBatch(ctx context.Context, accountID string, aliases []string) bool {
	if len(aliases) == 0 {
		return false
	}

	// PR-04A F07: 若所有待查别名均具备严格基线，单 alias 与多 alias 统一走 UID 升序增量分页扫描引擎
	if w.store != nil {
		var globalMinUID uint32
		allHaveBaseline := true
		currentBaselines := make(map[string]uint32, len(aliases))
		for _, alias := range aliases {
			norm := strings.ToLower(strings.TrimSpace(alias))
			uid, err := w.store.GetMinBaselineUIDByEmail(ctx, norm)
			if err != nil || uid == 0 {
				allHaveBaseline = false
				break
			}
			currentBaselines[norm] = uid
			if globalMinUID == 0 || uid < globalMinUID {
				globalMinUID = uid
			}
		}
		if allHaveBaseline && globalMinUID > 0 {
			return w.scanAndPublishPages(ctx, accountID, aliases, currentBaselines, "INBOX", globalMinUID)
		}
	}

	// 降级兜底: 无 baseline 的非严格验证码模式 (例如盲扫野别名或无 store 模式)
	var q InboxQuery
	if len(aliases) == 1 {
		q = InboxQuery{
			AccountID: accountID,
			Alias:     aliases[0],
			Folder:    "inbox",
			Limit:     5,
			Days:      1,
			WithBody:  true,
		}
	} else {
		q = InboxQuery{
			AccountID: accountID,
			Folder:    "inbox",
			Limit:     10,
			Days:      1,
			WithBody:  true,
		}
	}

	res, err := w.be.ListInboxContext(ctx, q)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[MailSync] 账号 %s 同步邮件失败: %v", accountID, err)
		}
		return false
	}
	if len(res.Messages) == 0 {
		return false
	}

	aliasSet := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		aliasSet[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}

	hasMatch := false
	for _, msg := range res.Messages {
		otp := mail.ExtractOTP(msg.Subject, msg.Preview)
		if otp == nil {
			continue
		}

		for target := range aliasSet {
			if !msgMatchesRecipient(msg, target) {
				continue
			}
			if !w.markPublished(accountID, msg.Folder, msg.UIDValidity, msg.UID, msg.ThreadID, msg.Provider, target) {
				continue
			}
			w.eventBus.PublishEvent(&mail.CachedOTP{
				EventID:     msg.MessageRef,
				AccountID:   accountID,
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
	return hasMatch
}

// scanAndPublishPages 执行基于 UID 升序增量分页、固定上界、resumable checkpoint 与 metadata-first 的流式扫描 (PR-04A F07)。
func (w *MailSyncWorker) scanAndPublishPages(ctx context.Context, accountID string, aliases []string, currentBaselines map[string]uint32, folder string, baselineUID uint32) bool {
	if len(aliases) == 0 || baselineUID == 0 {
		return false
	}
	if folder == "" {
		folder = "INBOX"
	}

	// 1. 固定 upper bound: 每轮 account scan 开始时获取 UIDVALIDITY 与 UIDNEXT (RFC 3501 UIDNEXT-1)
	provider, uidValidity, uidNext, err := w.be.GetMailboxBoundaryContext(ctx, accountID, folder)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[MailSync] 获取账号 %s 邮箱边界失败: %v", accountID, err)
		}
		return false
	}
	if provider != "imap" || uidValidity == 0 || uidNext <= 1 {
		return false
	}
	scanUpperUID := uidNext - 1

	// 2. Resumable checkpoint 决策与跨代际失效清理
	cpKey := checkpointKey{
		accountID:   accountID,
		mailbox:     folder,
		uidValidity: uidValidity,
	}

	w.mu.Lock()
	// UIDVALIDITY 改变: 旧 checkpoint 失效并清除
	for k := range w.checkpoints {
		if k.accountID == accountID && k.mailbox == folder && k.uidValidity != uidValidity {
			delete(w.checkpoints, k)
		}
	}

	cp, exists := w.checkpoints[cpKey]
	if !exists {
		cp = &checkpointState{
			NextUID:        baselineUID,
			AliasBaselines: make(map[string]uint32, len(currentBaselines)),
		}
		w.checkpoints[cpKey] = cp
	} else if cp.AliasBaselines == nil {
		cp.AliasBaselines = make(map[string]uint32)
	}

	cursor := cp.NextUID
	rewound := false

	// 检查当前所有活跃 subscribers 的覆盖情况:
	// 场景 1: earlier baseline rewind (新 subscriber 带来更早 baseline)
	// 场景 2: same baseline new alias rewind (新 subscriber 即使 baseline 与旧 checkpoint 相同或不更早，但尚未被当前 checkpoint 覆盖)
	// 场景 3: changed baseline rewind (同一 subscriber 重新发起验证且 baseline 变早)
	for alias, curB := range currentBaselines {
		coveredB, covered := cp.AliasBaselines[alias]
		if !covered || curB < coveredB {
			if curB < cursor {
				cursor = curB
				rewound = true
			}
		}
	}

	// 若未触发 rewind，且当前所有活跃 subscriber 的最小基线都晚于当前 cursor（旧 subscriber 均已结束），安全前进
	if !rewound && baselineUID > cursor {
		cursor = baselineUID
		rewound = true
	}

	// 发生 rewind 时，立即将 NextUID 保持在实际推进位置，防止中断时恢复为旧的更大游标
	if rewound {
		cp.NextUID = cursor
	}
	w.mu.Unlock()

	// 若 cursor 已经越过固定上界，说明本轮已无新邮件需要扫描
	if cursor > scanUpperUID {
		return false
	}

	aliasSet := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		aliasSet[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}

	hasMatch := false

	// 3. 分页增量扫描循环 (UID 从旧到新 UID ascending，pageSize=50)
	for cursor <= scanUpperUID {
		if err := ctx.Err(); err != nil {
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
			if errors.Is(err, mail.ErrUIDValidityMismatch) {
				w.mu.Lock()
				delete(w.checkpoints, cpKey)
				w.mu.Unlock()
			}
			break
		}
		if pageRes.UIDValidity != uidValidity {
			w.mu.Lock()
			delete(w.checkpoints, cpKey)
			w.mu.Unlock()
			break
		}

		if len(pageRes.Messages) == 0 {
			// 本轮范围内已无消息，推进到 scanUpperUID + 1
			w.mu.Lock()
			cp.NextUID = scanUpperUID + 1
			for alias, curB := range currentBaselines {
				if curB <= scanUpperUID {
					if prev, ok := cp.AliasBaselines[alias]; !ok || curB < prev {
						cp.AliasBaselines[alias] = curB
					}
				}
			}
			w.mu.Unlock()
			break
		}

		// 第一阶段: Metadata-first，对活跃 aliases 做结构化收件人匹配，筛选 candidate UID
		var candidateUIDs []uint32
		for _, msg := range pageRes.Messages {
			for target := range aliasSet {
				if msgMatchesRecipient(msg, target) {
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
			if err != nil {
				// 网络错误或取消: 不得提前推进未处理的本页 checkpoint
				break
			}

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

				for target := range aliasSet {
					if !msgMatchesRecipient(fullMsg.Message, target) {
						continue
					}
					if !w.markPublished(accountID, fullMsg.Folder, fullMsg.UIDValidity, fullMsg.UID, fullMsg.ThreadID, fullMsg.Provider, target) {
						continue
					}
					w.eventBus.PublishEvent(&mail.CachedOTP{
						EventID:     fullMsg.MessageRef,
						AccountID:   accountID,
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
		}

		// 只有一整 page 成功完成处理后，才能向前推进 checkpoint
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
				if prev, ok := cp.AliasBaselines[alias]; !ok || curB < prev {
					cp.AliasBaselines[alias] = curB
				}
			}
		}
		w.mu.Unlock()

		if !pageRes.HasMore || cursor > scanUpperUID {
			break
		}
	}

	return hasMatch
}

func (w *MailSyncWorker) fetchAndPublish(ctx context.Context, accountID, alias string) bool {
	return w.fetchAndPublishBatch(ctx, accountID, []string{alias})
}

// resolveAccountFromStore 按「持久化路由表 → 出号流水」的顺序解析别名归属。
func (w *MailSyncWorker) resolveAccountFromStore(alias string) (string, bool) {
	if w.store == nil {
		return "", false
	}
	if accountID, ok := w.store.FindAliasRoute(alias); ok {
		return accountID, true
	}
	if accountID, ok := w.store.FindLeaseAccount(alias); ok {
		_ = w.store.UpsertAliasRoutes(accountID, []string{alias})
		return accountID, true
	}
	return "", false
}

// isRecentProbeMiss 判断该别名是否刚盲扫失败过，避免每轮重复扫同一批账号。
func (w *MailSyncWorker) isRecentProbeMiss(alias string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	at, ok := w.probeMiss[alias]
	return ok && time.Since(at) < unknownAliasMissTTL
}

// noteProbeMiss 记录一次盲扫未命中，并顺带淘汰过期记录。
func (w *MailSyncWorker) noteProbeMiss(alias string) {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.probeMiss == nil {
		w.probeMiss = make(map[string]time.Time)
	}
	for k, t := range w.probeMiss {
		if now.Sub(t) > unknownAliasMissTTL {
			delete(w.probeMiss, k)
		}
	}
	w.probeMiss[alias] = now
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

// parseRecipientEmails 从收件人字段中解析所有邮箱地址。
func parseRecipientEmails(raw string) []string {
	raw = strings.TrimSpace(raw)
	var emails []string
	if list, err := stdmail.ParseAddressList(raw); err == nil && len(list) > 0 {
		for _, a := range list {
			if a.Address != "" {
				emails = append(emails, strings.ToLower(a.Address))
			}
		}
		return emails
	}

	// 兜底提取包含 @ 的部分
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		part = strings.Trim(part, "<>,;\"' \t\r\n")
		if strings.Contains(part, "@") {
			emails = append(emails, strings.ToLower(part))
		}
	}
	return emails
}
