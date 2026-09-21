/**
 * [INPUT]: 依赖 internal/account, internal/auth, internal/mail, internal/webui
 * [OUTPUT]: 对外提供 Server 结构体, New, Run, Handler
 * [POS]: internal/server 的主入口与路由注册中心，统一管理 API 与 WebUI 路由
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/auth"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/notify"
	"icloud-hme/internal/scheduler"
	"icloud-hme/internal/store"
	"icloud-hme/internal/webui"
)

// Config 是 Server 的启动配置。
type Config struct {
	DataDir               string
	Debug                 bool
	AdminPassword         string
	APIKey                string
	SessionTTL            time.Duration
	SecureCookie          bool
	CookieMonitorInterval time.Duration // Cookie 健康监控周期,0 取默认 30m
	// CookieMonitorThrottle 是 Cookie 校验的账号间节流间隔。
	// 0 表示自动摊平(使一轮校验落在 CookieMonitorInterval 之内)，显式配置则优先生效。
	CookieMonitorThrottle time.Duration
	// StartupSyncInterval 是启动预热时账号之间的提交间隔。
	// 0 表示自动摊平到约 10 分钟，显式配置则优先生效。
	StartupSyncInterval time.Duration
	// MailPollInterval 是取码长轮询期间的邮件轮询周期,0 取默认 2s。
	MailPollInterval time.Duration
	// LeaseRetention 是已用别名流水的保留期,0 表示永久保留(不清理)。
	LeaseRetention time.Duration
	// TrustedProxies 是可信反向代理的 IP/CIDR 列表(如 127.0.0.1、172.17.0.0/16)。
	//
	// 为空(默认)时不信任任何代理头,ClientIP() 始终取真实连接地址 —— 这是最安全的
	// 默认值，X-Forwarded-For 无法被伪造。代价是: 经 Nginx/Caddy 反代部署时所有请求
	// 都来自代理 IP，登录失败限流(15 分钟/5 次)会退化成**全局单桶**，攻击者只要持续
	// 提交错误口令就能把管理员长期锁在登录页之外。
	//
	// 因此经反代部署时应填**仅**代理自身的地址(如 127.0.0.1)。Gin 只在直连对端
	// 属于该列表时才采信 XFF，攻击者从外部直连时对端不是代理，XFF 会被忽略，
	// 既能恢复「按真实客户端限流」，又不会被伪造头绕过。
	TrustedProxies []string
}

type messageCacheEntry struct {
	msg       *mail.FullMessage
	expiresAt time.Time
	provider  string
	method    string
}

// Server 封装 Gin 引擎、账号后端与认证。
type Server struct {
	be          Backend
	auth        *auth.Manager
	limiter     *auth.Limiter
	cfg         Config
	r           *gin.Engine
	eventBus    *mail.EventBus
	aliasBuffer *AliasBuffer
	syncWorker  *MailSyncWorker
	reaper      *AliasReaper
	cookieMon   *CookieMonitor
	leasePruner *LeasePruner
	notifier    *notify.Sender
	store       *store.Store
	scheduler   *scheduler.Scheduler
	msgCacheMu  sync.RWMutex
	msgCache    map[string]messageCacheEntry
	startedAt   time.Time
	ctx         context.Context    // 【BUG-11】停机信号,由 Close() 触发 cancel
	cancel      context.CancelFunc // 【BUG-11】停机信号取消函数
}

// New 创建 Server。mgr 为账号管理器,st 为持久化存储(可为 nil),cfg 为安全配置。
func New(mgr *account.Manager, st *store.Store, cfg Config) (*Server, error) {
	if _, err := auth.NewManager(auth.Options{
		Password: cfg.AdminPassword,
		TTL:      cfg.SessionTTL,
	}); err != nil {
		return nil, err
	}
	if st == nil {
		var err error
		st, err = store.NewStore(cfg.DataDir)
		if err != nil {
			return nil, err
		}
	}
	return newWithBackendAndStore(&managerBackend{mgr: mgr, store: st}, cfg, st), nil
}

// newWithBackend 创建 Server 并注入 Backend(测试使用内存 fake)。
func newWithBackend(be Backend, cfg Config) *Server {
	if cfg.DataDir == "" {
		if tempDir, err := os.MkdirTemp("", "icloud_hme_test_*"); err == nil {
			cfg.DataDir = tempDir
		}
	}
	st, _ := store.NewStore(cfg.DataDir)
	return newWithBackendAndStore(be, cfg, st)
}

func newWithBackendAndStore(be Backend, cfg Config, st *store.Store) *Server {
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	eventBus := mail.NewEventBus(5 * time.Minute)
	aliasBuf := NewAliasBuffer(be, 3, 20*time.Second)
	mailPoll := cfg.MailPollInterval
	if mailPoll <= 0 {
		mailPoll = 2 * time.Second
	}
	syncWorker := NewMailSyncWorker(be, st, eventBus, mailPoll)
	reaper := NewAliasReaper(be, 1*time.Hour, 2*time.Hour)
	notifier := notify.NewSender()
	mon := NewCookieMonitor(be, cfg.CookieMonitorInterval, notifier)
	mon.SetThrottle(cfg.CookieMonitorThrottle)

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		be:          be,
		limiter:     auth.NewLimiter(nil, 15*time.Minute, 5, 10000),
		cfg:         cfg,
		eventBus:    eventBus,
		aliasBuffer: aliasBuf,
		syncWorker:  syncWorker,
		reaper:      reaper,
		cookieMon:   mon,
		notifier:    notifier,
		store:       st,
		leasePruner: NewLeasePruner(st, cfg.LeaseRetention),
		startedAt:   time.Now(),
		ctx:         ctx,
		cancel:      cancel,
	}
	// 调度器在 Server 组装完成后注入，使 creator 能复用统一的流水入账口径
	s.scheduler = scheduler.NewScheduler(st, func(accountID, label string) (*hme.CreateResult, error) {
		res, err := be.CreateAlias(accountID, label)
		if err == nil && res != nil {
			syncWorker.RegisterAliasAccount(res.Email, accountID)
			// 调度产出的别名同样必须入流水，否则审计账本与 tag 对账会漏统计全部定时产出
			s.recordLease(accountID, res.Email, s.scheduledTag(accountID), "scheduler")
		}
		return res, err
	}, be.ListAccounts)
	if st != nil {
		notifier.UpdateSettings(s.loadNotifySettings())
	}
	// 生产后端拉取到别名列表时自动登记「别名 → 母号」路由，
	// 使存量别名(未入出号流水表的)也能被定向拉信，而不必依赖有上限的盲扫。
	if mb, ok := be.(*managerBackend); ok {
		mb.onAliasesFetched = func(accountID string, aliases []hme.Alias) {
			if len(aliases) == 0 {
				return
			}
			emails := make([]string, 0, len(aliases))
			for i := range aliases {
				if aliases[i].Email != "" {
					emails = append(emails, aliases[i].Email)
				}
			}
			syncWorker.RegisterAliasAccounts(accountID, emails)
		}
	}
	s.auth, _ = auth.NewManager(auth.Options{
		Password: cfg.AdminPassword,
		TTL:      cfg.SessionTTL,
	})
	hasAuth := cfg.AdminPassword != "" || cfg.APIKey != ""
	s.r = gin.New()
	s.r.Use(gin.Logger(), gin.Recovery(), securityHeadersMiddleware(), dnsRebindingMiddleware(hasAuth))
	// 默认不信任任意代理头,登录限流使用真实连接 IP;
	// 显式配置 TrustedProxies 时才采信来自这些地址的 X-Forwarded-For。
	_ = s.r.SetTrustedProxies(cfg.TrustedProxies)
	s.register()
	return s
}

// Run 启动 HTTP 服务并开启后台引擎，支持响应中断信号优雅停机。
func (s *Server) Run(addr string) error {
	hasAuth := s.cfg.AdminPassword != "" || s.cfg.APIKey != ""
	if err := validateListenAddress(addr, hasAuth); err != nil {
		return err
	}
	s.aliasBuffer.Start()
	s.syncWorker.Start()
	s.reaper.Start()
	s.cookieMon.Start()
	s.leasePruner.Start()
	s.scheduler.Start()
	s.notifier.Start()
	go s.autoSyncAccounts()

	httpServer := &http.Server{
		Addr:    addr,
		Handler: s.r,
		// 防 Slowloris:不给「读头」超时的话，攻击者只需保持半开连接即可耗尽连接数。
		// 刻意不设 WriteTimeout —— verify-code 长轮询最长挂起 120s，设了会误杀正常请求。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-quit:
		log.Printf("[Server] 捕获退出信号 (%s)，开始优雅停机...", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shutdownErr := httpServer.Shutdown(ctx)
	s.Close()
	return shutdownErr
}

// 启动预热的分摊参数。
const (
	// startupSyncBudget 是预热全部账号的目标耗时上限(自动摊平时使用)
	startupSyncBudget = 10 * time.Minute
	// defaultStartupSyncGap 是账号间提交间隔的上限(与历史硬编码值一致)
	defaultStartupSyncGap = 2 * time.Second
	// minStartupSyncGap 是账号间提交间隔的下限,防止账号极多时对 Apple 形成突发
	minStartupSyncGap = 200 * time.Millisecond
	// startupSyncConcurrency 限制预热期间的并发在途请求数
	startupSyncConcurrency = 4
)

// startupSyncGap 计算启动预热时账号之间的提交间隔。
func (s *Server) startupSyncGap(n int) time.Duration {
	if n <= 1 {
		return 0
	}
	if s.cfg.StartupSyncInterval > 0 {
		return s.cfg.StartupSyncInterval
	}
	gap := startupSyncBudget / time.Duration(n)
	if gap > defaultStartupSyncGap {
		gap = defaultStartupSyncGap
	}
	if gap < minStartupSyncGap {
		gap = minStartupSyncGap
	}
	return gap
}

// autoSyncAccounts 服务启动后自动在后台对齐所有健康账号的别名基数。
//
// 账号间按 startupSyncGap 摊平提交并限制并发，避免两个极端:
// 固定 2 秒会让 2000 账号预热耗时 66 分钟；不限速则会对 Apple 形成突发。
func (s *Server) autoSyncAccounts() {
	// 【BUG-11 修复】所有等待都响应 s.ctx，SIGTERM 到达时立即退出而非卡在 Sleep 中
	select {
	case <-s.ctx.Done():
		return
	case <-time.After(1 * time.Second):
	}

	accounts := s.be.ListAccounts()
	targets := make([]account.Summary, 0, len(accounts))
	for _, acc := range accounts {
		if acc.HasCookies && acc.Status != "error" {
			targets = append(targets, acc)
		}
	}
	if len(targets) == 0 {
		return
	}

	gap := s.startupSyncGap(len(targets))
	log.Printf("[Server] 启动预热: %d 个账号, 提交间隔=%v, 并发=%d", len(targets), gap, startupSyncConcurrency)

	sem := make(chan struct{}, startupSyncConcurrency)
	var wg sync.WaitGroup
	for i, acc := range targets {
		if i > 0 && gap > 0 {
			select {
			case <-s.ctx.Done():
				log.Printf("[Server] 启动预热被停机信号中断")
				wg.Wait()
				return
			case <-time.After(gap):
			}
		}
		wg.Add(1)
		sem <- struct{}{}
		id := acc.ID
		goSafe("auto-sync-accounts", func() {
			defer wg.Done()
			defer func() { <-sem }()
			_, _ = s.be.ListAliases(id)
		})
	}
	wg.Wait()
	log.Printf("[Server] 启动预热完成: %d 个账号", len(targets))
}

// Close 停止后台工作引擎。
func (s *Server) Close() {
	// 【BUG-11 修复】通知所有引用 s.ctx 的后台 goroutine 立即停止
	if s.cancel != nil {
		s.cancel()
	}
	s.aliasBuffer.Stop()
	s.syncWorker.Stop()
	s.reaper.Stop()
	s.cookieMon.Stop()
	s.leasePruner.Stop()
	s.scheduler.Stop()
	s.notifier.Stop()
	if s.store != nil {
		_ = s.store.Close()
	}
}

// Handler 返回底层 gin 引擎(便于测试)。
func (s *Server) Handler() http.Handler { return s.r }

func (s *Server) register() {
	api := s.r.Group("/api")
	api.Use(apiCacheControlMiddleware(), bodyLimitMiddleware())
	{
		// ===== 认证(公开) =====
		api.POST("/auth/login", s.handleLogin)
		api.GET("/auth/session", s.handleSession)

		// ===== 受保护路由:统一 requireSession (支持 API Key 旁路) =====
		authed := api.Group("")
		authed.Use(requireSession(s.auth, s.cfg.APIKey, s.store))
		{
			authed.POST("/auth/logout", csrfCheck(s.auth), s.handleLogout)

			// ===== 最小权限: 出号作用域 (对外发放令牌的默认能力) =====
			alloc := authed.Group("")
			alloc.Use(requireScope(store.ScopeAllocate))
			{
				alloc.POST("/quick-create", csrfCheck(s.auth), s.quickCreateHandler)
				alloc.POST("/alias/lease", csrfCheck(s.auth), s.quickCreateHandler)
				alloc.POST("/allocate", csrfCheck(s.auth), s.quickCreateHandler)
				alloc.POST("/external/v1/allocate", csrfCheck(s.auth), s.quickCreateHandler)
			}

			// ===== 最小权限: 取码作用域 =====
			verify := authed.Group("")
			verify.Use(requireScope(store.ScopeVerify))
			{
				verify.GET("/verify-code", s.verifyCodeHandler)
				verify.GET("/external/v1/verify-code", s.verifyCodeHandler)
			}

			// ===== 管理面: 仅管理员会话 / admin 作用域令牌可达 =====
			adm := authed.Group("")
			adm.Use(requireScope(store.ScopeAdmin))
			{
				// 【PR-01 安全止损】直接建号与母号邮件读取仅对管理员开放，普通外部令牌不可跨权调用
				adm.POST("/create", csrfCheck(s.auth), s.createAliasHandler)
				adm.POST("/create/batch", csrfCheck(s.auth), s.createAliasBatchHandler)
				adm.GET("/inbox", s.listInboxHandler)
				adm.GET("/inbox/:message_id", s.getMessageHandler)
				adm.GET("/messages/:id", s.getMessagePrimeHandler)
				adm.POST("/messages", s.getMessagesHandler)
				adm.GET("/mailboxes", s.listMailboxesHandler)
				adm.DELETE("/inbox/:message_id", csrfCheck(s.auth), s.deleteMessageHandler)
				adm.DELETE("/messages/:id", csrfCheck(s.auth), s.deleteMessageHandler)

				// ===== 账号管理 =====
				adm.GET("/accounts", s.listAccountsHandler)
				adm.GET("/accounts/:id", s.getAccountHandler)
				adm.POST("/accounts", csrfCheck(s.auth), s.addAccountHandler)
				adm.PATCH("/accounts/:id", csrfCheck(s.auth), s.updateAccountHandler)
				adm.PUT("/accounts/:id/proxy", csrfCheck(s.auth), s.updateProxyHandler)
				adm.PUT("/accounts/:id/cookies", csrfCheck(s.auth), s.updateCookiesHandler)
				adm.POST("/accounts/:id/password", csrfCheck(s.auth), s.setAppPasswordHandler)
				adm.PUT("/accounts/:id/mailbox", csrfCheck(s.auth), s.setMailboxHandler)
				adm.POST("/accounts/:id/login", csrfCheck(s.auth), s.loginAccountHandler)
				adm.DELETE("/accounts/:id", csrfCheck(s.auth), s.removeAccountHandler)

				// ===== 自动化作业编排 =====
				adm.GET("/create/jobs", s.listCreateJobsHandler)
				adm.POST("/create/jobs", csrfCheck(s.auth), s.upsertCreateJobHandler)
				adm.POST("/create/jobs/:id/pause", csrfCheck(s.auth), s.pauseCreateJobHandler)
				adm.POST("/create/jobs/:id/resume", csrfCheck(s.auth), s.resumeCreateJobHandler)
				adm.DELETE("/create/jobs/:id", csrfCheck(s.auth), s.deleteCreateJobHandler)

				// ===== 代理连通性检测 =====
				adm.POST("/proxy/check", csrfCheck(s.auth), s.checkProxyHandler)

				// ===== 别名管理 =====
				adm.GET("/aliases", s.listAliasesHandler)
				adm.GET("/aliases/export", s.exportAliasesHandler)
				adm.PATCH("/aliases/:id", csrfCheck(s.auth), s.updateAliasHandler)
				adm.POST("/aliases/batch-update", csrfCheck(s.auth), s.batchUpdateAliasHandler)
				adm.POST("/aliases/:id/deactivate", csrfCheck(s.auth), s.deactivateAliasHandler)
				adm.POST("/aliases/:id/reactivate", csrfCheck(s.auth), s.reactivateAliasHandler)
				adm.DELETE("/aliases/:id", csrfCheck(s.auth), s.deleteAliasHandler)

				// ===== 中台扩展: 业务标识 =====
				adm.GET("/tags", s.listTagsHandler)
				adm.POST("/tags", csrfCheck(s.auth), s.createTagHandler)
				adm.PATCH("/tags/:id", csrfCheck(s.auth), s.updateTagHandler)
				adm.DELETE("/tags/:id", csrfCheck(s.auth), s.deleteTagHandler)

				// ===== 中台扩展: 外部令牌 (令牌本体只回显掩码, 杜绝令牌互相收割) =====
				adm.GET("/tokens", s.listTokensHandler)
				adm.POST("/tokens", csrfCheck(s.auth), s.createTokenHandler)
				adm.DELETE("/tokens/:id", csrfCheck(s.auth), s.deleteTokenHandler)

				// ===== 中台扩展: 已用别名流水 =====
				adm.GET("/leases", s.listLeasesHandler)
				adm.PATCH("/leases/:id/status", csrfCheck(s.auth), s.updateLeaseStatusHandler)

				// ===== 中台扩展: 定时调度任务 =====
				adm.GET("/schedule/configs", s.listScheduleConfigsHandler)
				adm.PUT("/schedule/configs/:account_id", csrfCheck(s.auth), s.updateScheduleConfigHandler)
				adm.POST("/schedule/run-now", csrfCheck(s.auth), s.runScheduleNowHandler)
				adm.GET("/schedule/logs", s.getScheduleLogsHandler)
				adm.GET("/schedule/status", s.scheduleStatusHandler)

				// ===== 系统设置: 通知 =====
				adm.GET("/settings/notify", s.getNotifySettingsHandler)
				adm.PUT("/settings/notify", csrfCheck(s.auth), s.updateNotifySettingsHandler)
				adm.POST("/settings/notify/test", csrfCheck(s.auth), s.testNotifyHandler)

				// ===== 系统 =====
				adm.POST("/reload", csrfCheck(s.auth), s.reloadConfigHandler)
				adm.GET("/system/stats", s.systemStatsHandler)
			}
		}
	}
	// API 404 返回 JSON,绝不让 NoRoute 把拼错的 API 路径变成 HTML
	s.r.NoRoute(func(c *gin.Context) {
		if c.Request.URL.Path == "/api" || strings.HasPrefix(c.Request.URL.Path, "/api/") {
			failCode(c, http.StatusNotFound, "NOT_FOUND", "接口不存在")
			return
		}
		// 其余路径交给 webui(SPA fallback)
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.String(http.StatusMethodNotAllowed, "方法不允许")
			return
		}
		webui.Handler(webuiFS).ServeHTTP(c.Writer, c.Request)
	})
}

// webuiFS 是内嵌前端资源(可被测试替换)。
var webuiFS = func() fs.FS {
	f, err := webui.Embedded()
	if err != nil {
		return nil
	}
	return f
}()

// ====================================================================
// 系统
// ====================================================================

func (s *Server) reloadConfigHandler(c *gin.Context) {
	if err := s.be.Reload(); err != nil {
		backendFail(c, err)
		return
	}
	ok(c, gin.H{"message": "配置已重新加载"})
}
