/**
 * [INPUT]: 依赖 internal/account, internal/auth, internal/mail, internal/webui
 * [OUTPUT]: 对外提供 Server 结构体, New, Run, Handler
 * [POS]: internal/server 的主入口，统一 API 与 WebUI 和补货入库；停机先取消业务，HTTP 与资源关闭共享总体预算，调用方等待超时后清理继续并支持再次等待；准入计数泄漏限时等待后不阻塞资源关闭
 * [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md
 */

package server

import (
	"context"
	"errors"
	"fmt"
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

	// 并发与准入控制配置 (PR-CONCURRENCY T1/R10)
	MaxInflightGlobal         int
	MaxInflightPerPrincipal   int
	MaxWaitersGlobal          int
	MaxWaitersPerPrincipal    int
	MaxWaitersPerKey          int
	MaxGlobalActiveVReq       int
	MaxPerPrincipalActiveVReq int
}

// Server 封装 Gin 引擎、账号后端与认证。
type Server struct {
	be              Backend
	auth            *auth.Manager
	limiter         *auth.Limiter
	requestLimiter  *RequestLimiter
	cfg             Config
	r               *gin.Engine
	eventBus        *mail.EventBus
	aliasBuffer     *AliasBuffer
	syncWorker      *MailSyncWorker
	cookieMon       *CookieMonitor
	leasePruner     *LeasePruner
	notifier        *notify.Sender
	store           *store.Store
	allocService    *AliasAllocationService
	verifyService   *VerificationService
	mailReadService *MailReadService
	scheduler       *scheduler.Scheduler
	startedAt       time.Time
	autoSyncWg      sync.WaitGroup     // 跟踪启动预热任务收敛 (PR-05 F10)
	ctx             context.Context    // 【BUG-11】停机信号,由 Close() 触发 cancel
	cancel          context.CancelFunc // 【BUG-11】停机信号取消函数
	reqMu           sync.RWMutex       // 保护在途 HTTP 请求生命周期与优雅停机同步 (T01/T02)
	reqWg           sync.WaitGroup     // 等待所有已接纳的 HTTP 请求 handler 彻底完成
	stopping        bool               // 标记停止接纳新业务
	closeOnce       sync.Once          // 确保唯一后台清理协程只启动一次
	closeDone       chan struct{}      // 真实清理完成通知信号
	closeErr        error              // 最终持久化/底层连接池关闭错误
	linkKeyMu       sync.Mutex         // 保护签名直链子密钥缓存与 epoch 递增
	linkKey         []byte             // 当前 epoch 的签名直链子密钥缓存 (nil 表示待加载)
}

// New 创建 Server。mgr 为账号管理器,st 为持久化存储(可为 nil),cfg 为安全配置。
func New(mgr *account.Manager, st *store.Store, cfg Config) (*Server, error) {
	if cfg.AdminPassword == "" {
		return nil, fmt.Errorf("管理员密码长度不能少于 8 个字符")
	}
	createdStore := false
	if st == nil {
		var err error
		st, err = store.NewStore(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		createdStore = true
	}
	srv, err := newWithBackendAndStoreWithError(&managerBackend{mgr: mgr, store: st}, cfg, st)
	if err != nil && createdStore {
		_ = st.Close()
	}
	return srv, err
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
	srv, _ := newWithBackendAndStoreWithError(be, cfg, st)
	return srv
}

func newWithBackendAndStoreWithError(be Backend, cfg Config, st *store.Store) (*Server, error) {
	var revocations auth.RevocationStore
	if st != nil {
		revocations = st
	}
	var authManager *auth.Manager
	if cfg.AdminPassword != "" {
		var err error
		authManager, err = auth.NewManager(auth.Options{
			Password:    cfg.AdminPassword,
			TTL:         cfg.SessionTTL,
			Revocations: revocations,
		})
		if err != nil {
			return nil, err
		}
	}
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	eventBus := mail.NewEventBus(5 * time.Minute)
	aliasBuf := NewAliasBuffer(be, 3, 20*time.Second)
	mailPoll := cfg.MailPollInterval
	if mailPoll <= 0 {
		mailPoll = 1 * time.Second
	}
	syncWorker := NewMailSyncWorker(be, st, eventBus, mailPoll)
	notifier := notify.NewSender()
	mon := NewCookieMonitor(be, cfg.CookieMonitorInterval, notifier)
	if st != nil {
		mon.poolAvailable = st.CountAuthoritativeAvailableAliases
	}
	mon.SetThrottle(cfg.CookieMonitorThrottle)

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		be:              be,
		limiter:         auth.NewLimiter(nil, 15*time.Minute, 5, 10000),
		cfg:             cfg,
		eventBus:        eventBus,
		aliasBuffer:     aliasBuf,
		syncWorker:      syncWorker,
		cookieMon:       mon,
		notifier:        notifier,
		store:           st,
		allocService:    NewAliasAllocationService(st, be, syncWorker),
		verifyService:   NewVerificationService(be, st, eventBus, syncWorker),
		mailReadService: NewMailReadService(be),
		leasePruner:     NewLeasePruner(st, cfg.LeaseRetention),
		startedAt:       time.Now(),
		ctx:             ctx,
		cancel:          cancel,
		closeDone:       make(chan struct{}),
	}
	reqLimiterCfg := RequestLimiterConfig{
		MaxWaitersGlobal:        cfg.MaxWaitersGlobal,
		MaxWaitersPerPrincipal:  cfg.MaxWaitersPerPrincipal,
		MaxWaitersPerKey:        cfg.MaxWaitersPerKey,
		MaxInflightGlobal:       cfg.MaxInflightGlobal,
		MaxInflightPerPrincipal: cfg.MaxInflightPerPrincipal,
	}
	s.requestLimiter = NewRequestLimiter(reqLimiterCfg)
	s.verifyService.SetRequestLimiter(s.requestLimiter)
	s.verifyService.SetServerContext(ctx)
	if cfg.MaxGlobalActiveVReq > 0 || cfg.MaxPerPrincipalActiveVReq > 0 {
		s.verifyService.SetMaxLimitsForTest(cfg.MaxGlobalActiveVReq, cfg.MaxPerPrincipalActiveVReq)
	}

	// 调度器在 Server 组装完成后注入，语义闭环为可用库存补货 (PR-06 §9.4, Issue 10, PR-05 F10)
	s.scheduler = scheduler.NewScheduler(st, func(ctx context.Context, accountID, label string) (*hme.CreateResult, error) {
		res, err := be.CreateAliasForReplenishmentContext(ctx, accountID, label)
		if err == nil && res != nil {
			syncWorker.RegisterAliasAccount(res.Email, accountID)
			// Commit replenishment inventory, routing and intent completion together.
			// 严禁在补货时创建 consumer allocation / lease，确保普通出号能原子认领
			if st != nil {
				if addErr := st.AddReplenishedInventoryAlias(ctx, accountID, hme.Alias{
					Email:       res.Email,
					AnonymousID: res.AnonymousID,
					Label:       res.Label,
					CreatedAt:   res.CreatedAt,
					Active:      true,
				}); addErr != nil {
					log.Printf("[Scheduler] 补货入库失败 email=%s: %v", res.Email, addErr)
					return nil, fmt.Errorf("补货入库持久化失败 email=%s: %w", res.Email, addErr)
				}
			}
		}
		return res, err
	}, be.ListAccounts)
	if st != nil {
		notifySt, err := s.loadNotifySettings()
		if err != nil {
			cancel()
			return nil, fmt.Errorf("加载通知配置失败 (Fail Closed): %w", err)
		}
		notifier.UpdateSettings(notifySt)
		if n, err := st.ReconcileAvailableInventory(); err == nil && n > 0 {
			log.Printf("[Server] 存量库存安全对齐完成: 已隔离/收敛 %d 个受保护或异常别名", n)
		}
	}
	if reconciler, ok := be.(interface {
		ReconcileUnresolvedIntents(context.Context) ([]store.HmeReserveIntent, error)
	}); ok {
		if list, err := reconciler.ReconcileUnresolvedIntents(context.Background()); err != nil {
			log.Printf("[Server] 启动恢复未决别名失败，将在后续建号前重试: %v", err)
		} else if len(list) > 0 {
			log.Printf("[Server] 启动恢复: 扫描处理 %d 个未决 HME reserve 意图", len(list))
		}
	}
	if st != nil {
		if recovered, err := st.RecoverRemoteAllocationOperations(context.Background()); err != nil {
			log.Printf("[Server] 恢复未完成出号失败: %v", err)
		} else if recovered > 0 {
			log.Printf("[Server] 恢复 %d 个已确认创建的出号操作", recovered)
		}
	}
	if mb, ok := be.(*managerBackend); ok {
		mb.recoverCamoufoxTasks()
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
	s.auth = authManager
	hasAuth := cfg.AdminPassword != "" || cfg.APIKey != ""
	s.r = gin.New()
	s.r.Use(s.requestTrackingMiddleware())
	s.r.Use(gin.LoggerWithConfig(gin.LoggerConfig{SkipQueryString: true}), gin.Recovery(), securityHeadersMiddleware(), dnsRebindingMiddleware(hasAuth))
	// 默认不信任任意代理头,登录限流使用真实连接 IP;
	// 显式配置 TrustedProxies 时才采信来自这些地址的 X-Forwarded-For。
	_ = s.r.SetTrustedProxies(cfg.TrustedProxies)
	s.register()
	return s, nil
}

// requestTrackingMiddleware 统一跟踪所有已接纳的 HTTP 请求生命周期，使请求 context 与服务关停取消信号严格联动 (T01/T02)。
func (s *Server) requestTrackingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		s.reqMu.RLock()
		stopping := s.stopping || (s.ctx != nil && s.ctx.Err() != nil)
		if stopping {
			s.reqMu.RUnlock()
			failCode(c, http.StatusServiceUnavailable, "SERVER_BUSY", "服务正在停机收尾")
			return
		}
		s.reqWg.Add(1)
		s.reqMu.RUnlock()

		defer s.reqWg.Done()

		if s.ctx != nil {
			reqCtx, reqCancel := context.WithCancel(c.Request.Context())
			defer reqCancel()
			go func() {
				select {
				case <-s.ctx.Done():
					reqCancel()
				case <-reqCtx.Done():
				}
			}()
			c.Request = c.Request.WithContext(reqCtx)
		}

		c.Next()
	}
}

// 优雅停机默认超时预算 (PR-05 F10)。统一约束 HTTP Shutdown 与后台各 Worker 收敛。
const defaultShutdownTimeout = 10 * time.Second

// limiterDrainTimeout 是 handler 全部返回后等待准入计数归零的上限；变量形式供测试缩短。
var limiterDrainTimeout = 2 * time.Second

// Run 启动 HTTP 服务并开启后台引擎，支持响应中断信号优雅停机。
func (s *Server) Run(addr string) error {
	hasAuth := s.cfg.AdminPassword != "" || s.cfg.APIKey != ""
	if err := validateListenAddress(addr, hasAuth); err != nil {
		return err
	}
	s.aliasBuffer.Start()
	s.syncWorker.Start()
	s.cookieMon.Start()
	s.leasePruner.Start()
	s.scheduler.Start()
	s.notifier.Start()
	s.startAutoSyncAccounts()

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
	defer signal.Stop(quit)

	var serveErr error
	select {
	case err := <-errCh:
		serveErr = err
	case sig := <-quit:
		log.Printf("[Server] 捕获退出信号 (%s)，开始优雅停机...", sig)
	}

	return errors.Join(serveErr, s.shutdownHTTP(httpServer))
}

// shutdownHTTP 先停止准入和广播取消，HTTP drain 与资源清理共享一个总体预算。
func (s *Server) shutdownHTTP(httpServer *http.Server) error {
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancelShutdown()
	s.startClose()
	httpErr := httpServer.Shutdown(shutdownCtx)
	if httpErr != nil && !errors.Is(httpErr, http.ErrServerClosed) {
		log.Printf("[Server] HTTP 停机等待超时或异常，强制断开剩余连接: %v", httpErr)
		_ = httpServer.Close()
	}

	if err := s.CloseContext(shutdownCtx); err != nil {
		log.Printf("[Server] 后台引擎关闭异常: %v", err)
		return errors.Join(httpErr, err)
	}
	if httpErr != nil {
		return httpErr
	}
	log.Printf("[Server] 服务已优雅停机")
	return nil
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

// startAutoSyncAccounts 启动账号基数预热协程，严格保证 Add(+1) 发生在 launch 之前 (PR-05 F10)。
func (s *Server) startAutoSyncAccounts() {
	s.autoSyncWg.Add(1)
	go s.autoSyncAccounts()
}

// autoSyncAccounts 服务启动后自动在后台对齐所有健康账号的别名基数。
//
// 账号间按 startupSyncGap 摊平提交并限制并发，避免两个极端:
// 固定 2 秒会让 2000 账号预热耗时 66 分钟；不限速则会对 Apple 形成突发。
func (s *Server) autoSyncAccounts() {
	defer s.autoSyncWg.Done()

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
			_, _ = s.be.ListAliasesContext(s.ctx, id)
		})
	}
	wg.Wait()
	log.Printf("[Server] 启动预热完成: %d 个账号", len(targets))
}

// CloseContext 停止后台工作引擎，严格遵守优雅停机生命周期顺序与单一预算约束 (T01/T02):
//  1. 发送上下文取消信号，停止接纳新请求，通知已接纳请求立即取消
//  2. 启动唯一清理协程持续执行，不受单个调用方等待超时影响
//  3. 等待所有后台 worker 收敛并等待所有在途 HTTP handler 彻底退出
//  4. 只有在所有在途 HTTP handler 和后台任务归零后，才关闭底层连接池与 Store
func (s *Server) startClose() {
	s.closeOnce.Do(func() {
		if s.closeDone == nil {
			s.closeDone = make(chan struct{})
		}
		// 1. 停止接收新工作，通知所有引用 s.ctx 的后台 goroutine 与在途请求立即取消
		s.reqMu.Lock()
		s.stopping = true
		s.reqMu.Unlock()

		if s.cancel != nil {
			s.cancel()
		}

		// 2. 启动唯一的后台清理协程持续执行，直到真实收尾 (T01/T02)
		go func() {
			defer close(s.closeDone)
			var wg sync.WaitGroup

			// Scheduler 调度器
			if s.scheduler != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = s.scheduler.StopContext(context.Background())
				}()
			}

			// MailSyncWorker 同步器
			if s.syncWorker != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.syncWorker.Stop()
				}()
			}

			// CookieMonitor 监控器
			if s.cookieMon != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.cookieMon.Stop()
				}()
			}

			// AliasBuffer
			if s.aliasBuffer != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.aliasBuffer.Stop()
				}()
			}

			// LeasePruner
			if s.leasePruner != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.leasePruner.Stop()
				}()
			}

			// Notifier
			if s.notifier != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.notifier.Stop()
				}()
			}

			// 启动预热任务
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.autoSyncWg.Wait()
			}()

			wg.Wait()

			// 严格等待所有已接纳的 HTTP handler 生命周期彻底完成 (T01)
			s.reqWg.Wait()

			// handler 全部返回后名额应已释放 (S04)；仍有残留说明计数泄漏，限时等待后记录并继续收尾，
			// 不能让泄漏的计数永久阻止连接池与 Store 关闭。
			if s.requestLimiter != nil {
				deadline := time.Now().Add(limiterDrainTimeout)
				for {
					stats := s.requestLimiter.Stats()
					if stats.ActiveWaiters == 0 && stats.ActiveInflight == 0 {
						break
					}
					if time.Now().After(deadline) {
						log.Printf("[Server] 停机时准入计数未归零 (waiters=%d inflight=%d)，继续关闭资源", stats.ActiveWaiters, stats.ActiveInflight)
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
			}

			if closer, ok := s.be.(interface{ Close() }); ok {
				closer.Close()
			}
			if s.store != nil {
				s.closeErr = s.store.Close()
			}
		}()
	})
}

func (s *Server) CloseContext(ctx context.Context) error {
	s.startClose()
	select {
	case <-s.closeDone:
		return s.closeErr
	case <-ctx.Done():
		select {
		case <-s.closeDone:
			return s.closeErr
		default:
			return ctx.Err()
		}
	}
}

// Close 提供向后兼容的无上下文停机接口，默认使用 defaultShutdownTimeout (10s)。
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()
	_ = s.CloseContext(ctx)
}

// Handler 返回底层 gin 引擎(便于测试)。
func (s *Server) Handler() http.Handler { return s.r }

func (s *Server) register() {
	// ===== 健康检查探针 (PR-01 / F01) =====
	// /livez 存活探针: 仅检查进程存活, 0 I/O, 不含敏感信息, 不依赖外部网络或存储
	s.r.GET("/livez", s.handleLivez)
	// /readyz 就绪探针: 检查核心存储健康, 严格 2s 超时, 严禁触碰 Apple, 不含敏感信息
	s.r.GET("/readyz", s.handleReadyz)

	// ===== 浏览器直链：取码 JSON、邮件预览与原文 (verify 作用域，允许 URL 携带令牌) =====
	s.r.GET("/mail/view-assets.css", serveMailDirectAsset("mail_direct.css", "text/css; charset=utf-8"))
	s.r.GET("/mail/view-assets.js", serveMailDirectAsset("mail_direct.js", "application/javascript; charset=utf-8"))
	mailGroup := s.r.Group("/mail")
	mailGroup.Use(apiCacheControlMiddleware())
	mailGroup.Use(requireSession(s.auth, s.cfg.APIKey, s.store, s.mailLinkPrincipal, s.requestLimiter))
	mailGroup.Use(requireScope(store.ScopeVerify))
	{
		mailGroup.GET("/code", s.verifyCodeHandler)
		mailGroup.GET("/code/:email", s.verifyCodeHandler)
		mailGroup.GET("/view", s.mailViewHandler)
		mailGroup.GET("/view/:email", s.mailViewHandler)
		mailGroup.GET("/raw", s.mailRawHandler)
		mailGroup.GET("/raw/:email", s.mailRawHandler)
	}

	api := s.r.Group("/api")
	api.Use(apiCacheControlMiddleware(), bodyLimitMiddleware())
	{
		// ===== 认证(公开) =====
		api.POST("/auth/login", s.handleLogin)
		api.GET("/auth/session", s.handleSession)

		// ===== 受保护路由:统一 requireSession (支持 API Key 旁路) =====
		authed := api.Group("")
		authed.Use(requireSession(s.auth, s.cfg.APIKey, s.store, nil, s.requestLimiter))
		{
			// ...
			authed.POST("/auth/logout", csrfCheck(s.auth), s.handleLogout)

			// ===== 管理面: 仅管理员会话 / admin 作用域令牌可达；外部令牌统一走 /api/external/v2 与 /mail 直链 =====
			adm := authed.Group("")
			adm.Use(requireScope(store.ScopeAdmin))
			{
				// 管理台出号：可指定母号与 mode=create
				adm.POST("/quick-create", csrfCheck(s.auth), s.quickCreateHandler)

				// 【PR-01 安全止损】直接建号与母号邮件读取仅对管理员开放，普通外部令牌不可跨权调用
				adm.POST("/create", csrfCheck(s.auth), s.createAliasHandler)
				adm.POST("/create/batch", csrfCheck(s.auth), s.createAliasBatchHandler)
				adm.GET("/inbox", s.listInboxHandler)
				adm.GET("/inbox/:message_id", s.getMessageHandler)
				adm.GET("/mailboxes", s.listMailboxesHandler)
				adm.DELETE("/inbox/:message_id", csrfCheck(s.auth), s.deleteMessageHandler)

				// ===== 账号管理 =====
				adm.GET("/accounts", s.listAccountsHandler)
				adm.GET("/accounts/:id", s.getAccountHandler)
				adm.POST("/accounts", csrfCheck(s.auth), s.addAccountHandler)
				adm.PATCH("/accounts/:id", csrfCheck(s.auth), s.updateAccountHandler)
				adm.PUT("/accounts/:id/proxy", csrfCheck(s.auth), s.updateProxyHandler)
				adm.PUT("/accounts/:id/cookies", csrfCheck(s.auth), s.updateCookiesHandler)
				adm.POST("/accounts/:id/password", csrfCheck(s.auth), s.setAppPasswordHandler)
				adm.PUT("/accounts/:id/mailbox", csrfCheck(s.auth), s.setMailboxHandler)
				adm.DELETE("/accounts/:id/mailbox", csrfCheck(s.auth), s.removeMailboxHandler)
				adm.POST("/accounts/:id/login", csrfCheck(s.auth), s.loginAccountHandler)
				adm.POST("/accounts/:id/login/cancel", csrfCheck(s.auth), s.cancelCamoufoxLoginHandler)
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
				adm.POST("/aliases/promote-to-pool", csrfCheck(s.auth), s.promoteAliasesToPoolHandler)

				// 单别名只读签名直链 (对外分发，不暴露令牌)
				adm.POST("/mail-links", csrfCheck(s.auth), s.createMailLinkHandler)
				adm.POST("/mail-links/revoke", csrfCheck(s.auth), s.revokeMailLinksHandler)

				// ===== 中台扩展: 业务标识 =====
				adm.GET("/tags", s.listTagsHandler)
				adm.POST("/tags", csrfCheck(s.auth), s.createTagHandler)
				adm.PATCH("/tags/:id", csrfCheck(s.auth), s.updateTagHandler)
				adm.DELETE("/tags/:id", csrfCheck(s.auth), s.deleteTagHandler)

				// ===== 中台扩展: 外部令牌 (不可逆哈希存储, 仅在创建/轮换时一次性可见) =====
				adm.GET("/tokens", s.listTokensHandler)
				adm.POST("/tokens", csrfCheck(s.auth), s.createTokenHandler)
				adm.POST("/tokens/:id/rotate", csrfCheck(s.auth), s.rotateTokenHandler)
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

				// ===== 系统设置: Camoufox 代理 =====
				adm.GET("/settings/camoufox", s.getCamoufoxSettingsHandler)
				adm.POST("/settings/camoufox/test", csrfCheck(s.auth), s.testCamoufoxHandler)

				// ===== 系统 =====
				adm.POST("/reload", csrfCheck(s.auth), s.reloadConfigHandler)
				adm.GET("/system/stats", s.systemStatsHandler)
			}
		}

		// ===== 外部 v2 独立路由组：仅允许 Bearer Token / API Key 认证，彻底拒绝 Cookie 会话，杜绝 CSRF (Issue 19 方案 A) =====
		v2 := api.Group("/external/v2")
		v2.Use(requireExternalV2Auth(s.cfg.APIKey, s.store, s.requestLimiter))
		{
			v2Alloc := v2.Group("")
			v2Alloc.Use(requireScope(store.ScopeAllocate))
			{
				v2Alloc.POST("/allocate", s.externalV2AllocateHandler)
				v2Alloc.GET("/operations/:operation_id", s.externalV2GetOperationHandler)
			}

			v2Verify := v2.Group("")
			v2Verify.Use(requireScope(store.ScopeVerify))
			{
				v2Verify.POST("/verification-requests", s.externalV2CreateVerificationRequestHandler)
				v2Verify.GET("/verification-requests/:request_id", s.externalV2GetVerificationRequestHandler)
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

// handleLivez 进程存活探针 (PR-01 / F01): 快速返回 200，无副作用，不查数据库。
func (s *Server) handleLivez(c *gin.Context) {
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleReadyz 服务就绪探针 (PR-01 / F01): 严格以 2s 超时探测核心数据库可用性，禁止调用 Apple，不泄露凭据。
func (s *Server) handleReadyz(c *gin.Context) {
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	if s.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "error": "store not initialized"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "error": "store ping failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
