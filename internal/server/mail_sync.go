/**
 * [INPUT]: 依赖 sync, time, net/mail, icloud-hme/internal/mail
 * [OUTPUT]: 对外提供 MailSyncWorker, NewMailSyncWorker
 * [POS]: server 的后台单协程拉信同步器，独占 IMAP 连接，集中解析后向 EventBus 广播；支持 Trigger 即时事件唤醒消灭轮询盲等
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
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
//
// 盲扫是 O(账号数) 次上游 IMAP 拉取，而本函数每 2 秒跑一轮:
// 2000 账号下单个未知别名就会变成每 2 秒 2000 次 IMAP 请求，必然触发 Apple 风控。
const maxUnknownAliasProbeAccounts = 20

// unknownAliasMissTTL 是盲扫失败的负缓存时长，避免每轮重复扫同一批账号。
const unknownAliasMissTTL = 10 * time.Minute

// maxAliasRouteCache 限制内存路由缓存的条目数。
//
// 内存表只是缓存，持久化的 alias_routes 才是真相源:超限直接重置，
// 最坏代价是重置后每个别名多一次主键点查，不影响正确性却能避免几十万条常驻内存。
const maxAliasRouteCache = 50000

// MailSyncWorker 后台单协程邮件同步器。
type MailSyncWorker struct {
	be             Backend
	eventBus       *mail.EventBus
	store          *store.Store
	interval       time.Duration
	triggerCh      chan struct{}
	stopCh         chan struct{}
	once           sync.Once
	stopOnce       sync.Once
	mu             sync.RWMutex
	aliasToAccount map[string]string    // alias (lower) -> accountID
	published      map[string]time.Time // "account|folder|uid|recipient" -> 首次发布时间
	probeMiss      map[string]time.Time // alias -> 上次盲扫未命中的时间
}

// NewMailSyncWorker 创建邮件同步器。st 可为 nil(仅退化为内存归属映射)。
func NewMailSyncWorker(be Backend, st *store.Store, eventBus *mail.EventBus, interval time.Duration) *MailSyncWorker {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &MailSyncWorker{
		be:             be,
		store:          st,
		eventBus:       eventBus,
		interval:       interval,
		triggerCh:      make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		aliasToAccount: make(map[string]string),
		published:      make(map[string]time.Time),
		probeMiss:      make(map[string]time.Time),
	}
}

// markPublished 判断该封邮件是否首次针对该收件人发布，并登记指纹。
//
// 【正确性红线】ListInbox 每轮都会把 Days:1 窗口内的全部历史邮件重新返回，
// 若不做邮件级去重，worker 会在订阅者出现后 2 秒内把【上一次已经用过的验证码】
// 重新广播出去——fresh=true 也挡不住(它只跳过内存缓存预填)。因此这里必须去重。
//
// 【BUG-03 修复】过期指纹清理已移至独立的 cleanupPublished 定时器(每 5 分钟),
// 此处只做 O(1) 的查重与插入,避免在写锁内执行 O(N) 全表遍历拖垮并发吞吐。
func (w *MailSyncWorker) markPublished(accountID, folder, msgID, email string) bool {
	key := strings.Join([]string{accountID, folder, msgID, email}, "|")
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
		go w.loop()
	})
}

// Stop 停止同步协程 (线程安全且幂等)。
func (w *MailSyncWorker) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
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

	// 2. 对已知归属的账号进行定向拉取
	for accID, aliases := range accountQueries {
		for _, alias := range aliases {
			w.fetchAndPublish(accID, alias)
		}
	}

	// 3. 仍无法归属的野别名(纯粹在 Apple 侧手工创建、本系统从未见过):
	//    做「有上限 + 负缓存」的盲扫兜底。路由表完备时这一步基本不会触发。
	if len(unknownAliases) > 0 {
		var probeList []string
		for _, alias := range unknownAliases {
			if !w.isRecentProbeMiss(alias) {
				probeList = append(probeList, alias)
			}
		}
		if len(probeList) > 0 {
			probed := 0
			for _, acc := range accounts {
				if probed >= maxUnknownAliasProbeAccounts {
					break
				}
				if !acc.HasAppPassword && !acc.HasCookies {
					continue
				}
				probed++
				for _, alias := range probeList {
					if w.fetchAndPublish(acc.ID, alias) {
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

// resolveAccountFromStore 按「持久化路由表 → 出号流水」的顺序解析别名归属。
// 两者都是索引点查，绝不会遍历账号列表。
func (w *MailSyncWorker) resolveAccountFromStore(alias string) (string, bool) {
	if w.store == nil {
		return "", false
	}
	if accountID, ok := w.store.FindAliasRoute(alias); ok {
		return accountID, true
	}
	// 兜底: 路由表尚未覆盖的历史数据(如回填前的流水)，命中后补写路由表
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

func (w *MailSyncWorker) fetchAndPublish(accountID, alias string) bool {
	res, err := w.be.ListInbox(InboxQuery{
		AccountID: accountID,
		Alias:     alias,
		Limit:     3,
		Days:      1,
	})
	if err != nil || len(res.Messages) == 0 {
		return false
	}

	hasMatch := false
	for _, msg := range res.Messages {
		otp := mail.ExtractOTP(msg.Subject, msg.Preview)
		if otp == nil {
			continue
		}

		target := strings.ToLower(strings.TrimSpace(alias))
		if target != "" {
			// 定向匹配：仅向当前待查别名广播，绝不向 To 头中包含的其它无关主号或抄送地址交叉泄露 OTP
			if !w.markPublished(accountID, msg.Folder, msg.ID, target) {
				continue
			}
			w.eventBus.Publish(target, accountID, msg.Subject, msg.From, msg.Date, otp)
			hasMatch = true
		} else {
			recipients := parseRecipientEmails(msg.To)
			for _, email := range recipients {
				// 同一封历史邮件绝不重复广播(否则二次取码会拿到已消费的陈旧 OTP)
				if !w.markPublished(accountID, msg.Folder, msg.ID, email) {
					continue
				}
				w.eventBus.Publish(email, accountID, msg.Subject, msg.From, msg.Date, otp)
				hasMatch = true
			}
		}
	}
	return hasMatch
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
