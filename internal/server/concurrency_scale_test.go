// [INPUT]: Real TCP HTTP/SQLite and fakeBackend mailbox callbacks.
// [OUTPUT]: Short simulated dispatch load and SQL-seeded quota-boundary regressions.
// [POS]: internal/server component capacity tests; full assembly lives in concurrency_protocol_test.go.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/mail"
	"icloud-hme/internal/store"
)

// TestConcurrencyScale_Simulated2000WaitersSharedInbox 验证真实 TCP Socket 监听、真实 HTTP 客户端连接池、
// 真实 MailSync 聚合分发算法下，SQL 预置 2000 个任务的短时挂起与模拟邮件分发。
//
// 【测试范围与边界说明】:
//  1. 真实系统链路 (已验证):
//     - 本地真实 TCP Socket 监听 (127.0.0.1:0)
//     - 真实 HTTP 客户端连接池 (MaxConnsPerHost = 4000)
//     - 真实 Gin 路由解析、鉴权中间件、RequestLimiter 准入/挂起/释放
//     - 真实 SQLite WAL 存储读写与事务提交
//     - 真实 MailSync 聚合分发算法与 EventBus 事件广播
//  2. 模拟边界 (未连接真实公网):
//     - 底层 IMAP 操作使用 fakeBackend 回调模拟，未连接外部真实 Gmail 或假 IMAP 协议端口，
//     不验证公网 TLS、DNS 或 Gmail 每账号连接配额。
//     - 2000 个任务为单事务 SQL 预置 (10 分钟有效期)，专门用于压力测试长轮询维持与并发分发吞吐。
func TestConcurrencyScale_Simulated2000WaitersSharedInbox(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	tempDir := t.TempDir()

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer st.Close()

	// 1. 初始化 10 个母号共享同一个 Gmail 物理邮箱
	const numAccounts = 10
	const totalTasks = 2000

	sharedGmail := "shared_worker@gmail.com"
	fingerprint := "shared_fingerprint_01"

	fb := &fakeBackend{
		accounts: make([]account.Summary, numAccounts),
	}
	for i := 0; i < numAccounts; i++ {
		accID := fmt.Sprintf("acc_%02d", i+1)
		fb.accounts[i] = account.Summary{
			ID:        accID,
			RealEmail: sharedGmail,
			Status:    "active",
		}
	}
	fb.onGetMailboxEndpointFingerprint = func(accountID string) (string, bool) {
		return fingerprint, true
	}

	eventBus := mail.NewEventBus(5 * time.Minute)
	syncWorker := NewMailSyncWorker(fb, st, eventBus, 100*time.Millisecond)

	// 生产环境默认 Compose 配置等效值:
	// ICLOUD_HME_MAX_GLOBAL_ACTIVE_VREQ=2000
	// ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ=500
	// ICLOUD_HME_MAX_WAITERS_GLOBAL=4000
	// ICLOUD_HME_MAX_WAITERS_PRINCIPAL=2000
	// ICLOUD_HME_MAX_WAITERS_PER_KEY=8
	// ICLOUD_HME_MAX_INFLIGHT_GLOBAL=128
	// ICLOUD_HME_MAX_INFLIGHT_PRINCIPAL=64
	limiterCfg := RequestLimiterConfig{
		MaxWaitersGlobal:        4000,
		MaxWaitersPerPrincipal:  2000,
		MaxWaitersPerKey:        8,
		MaxInflightGlobal:       128,
		MaxInflightPerPrincipal: 64,
	}
	limiter := NewRequestLimiter(limiterCfg)

	vService := NewVerificationService(fb, st, eventBus, syncWorker)
	vService.SetRequestLimiter(limiter)
	vService.maxGlobal = 2000
	vService.maxPerPrincipal = 500

	r := gin.New()
	r.Use(requireExternalV2Auth("", st, limiter))
	v2 := r.Group("/api/external/v2")
	srv := &Server{
		store:          st,
		verifyService:  vService,
		eventBus:       eventBus,
		requestLimiter: limiter,
	}
	v2.POST("/verification-requests", srv.externalV2CreateVerificationRequestHandler)
	v2.GET("/verification-requests/:request_id", srv.externalV2GetVerificationRequestHandler)

	// 真实本地 TCP 监听
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	httpServer := &http.Server{
		Handler: r,
	}
	go func() {
		_ = httpServer.Serve(ln)
	}()
	defer httpServer.Close()

	baseURL := fmt.Sprintf("http://%s", ln.Addr().String())

	// 真实高并发 HTTP 客户端连接池
	transport := &http.Transport{
		MaxIdleConns:        4000,
		MaxIdleConnsPerHost: 4000,
		MaxConnsPerHost:     4000,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   70 * time.Second,
	}
	defer client.CloseIdleConnections()

	// 创建 4 个生产等效测试 API Token (每个主体承载 500 任务，合计 2000)
	const numTokens = 4
	tokens := make([]*store.CreatedToken, numTokens)
	for i := 0; i < numTokens; i++ {
		tok, err := st.CreateToken(fmt.Sprintf("scale_worker_%02d", i+1), store.ScopeVerify, "")
		if err != nil {
			t.Fatalf("create token failed: %v", err)
		}
		tokens[i] = tok
	}

	ctx := context.Background()

	// 2. 测量阶段 0: 真实 HTTP POST 任务建立吞吐基准 (测试创建 50 个任务速率)
	now := time.Now().UTC()
	createdAtStr := now.Format(time.RFC3339)
	expiresAtStr := now.Add(10 * time.Minute).Format(time.RFC3339) // 生产 10 分钟有效期

	// 先准备基准账号与部分 allocation
	for i := 0; i < numAccounts; i++ {
		accID := fmt.Sprintf("acc_%02d", i+1)
		_, _ = st.DB().ExecContext(ctx, `INSERT OR IGNORE INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES (?, ?, ?, 'active', '[]', ?, ?)`,
			accID, accID, sharedGmail, createdAtStr, createdAtStr)
	}

	const benchCreateCount = 50
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}

	for i := 0; i < benchCreateCount; i++ {
		leaseID := fmt.Sprintf("bench_lease_%03d", i+1)
		aliasEmail := fmt.Sprintf("bench_alias_%03d@icloud.com", i+1)
		accID := fmt.Sprintf("acc_%02d", (i%numAccounts)+1)
		_, _ = st.DB().ExecContext(ctx, `INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, status, allocated_at) VALUES (?, ?, ?, 'token', ?, 'allocated', ?)`,
			leaseID, aliasEmail, accID, tokens[0].ID, createdAtStr)
	}

	benchCreateStart := time.Now()
	for i := 0; i < benchCreateCount; i++ {
		leaseID := fmt.Sprintf("bench_lease_%03d", i+1)
		reqBody := fmt.Sprintf(`{"lease_id":"%s"}`, leaseID)
		req, _ := http.NewRequest("POST", baseURL+"/api/external/v2/verification-requests", bytes.NewBufferString(reqBody))
		req.Header.Set("Authorization", "Bearer "+tokens[0].Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP POST 创建任务基准失败: err=%v, resp=%+v", err, resp)
		}
		_ = resp.Body.Close()
	}
	benchCreateDuration := time.Since(benchCreateStart)
	taskCreationRate := float64(benchCreateCount) / benchCreateDuration.Seconds()
	t.Logf("【测量指标 0: 真实 HTTP POST 任务建立吞吐】: %d 个任务耗时 %v, 速率: %.0f requests/sec",
		benchCreateCount, benchCreateDuration, taskCreationRate)
	// 基准任务单独测量并清理，保持下面 SQL 压力夹具准确为 2000 / 4×500。
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM verification_requests WHERE lease_id LIKE 'bench_lease_%'`); err != nil {
		t.Fatal(err)
	}

	// 3. 准备阶段：在单事务中批量准备 2000 个任务所需的基础数据 (分布在 4 个生产 Token，每主体 500)
	t.Logf("开始在单事务中准备 %d 个取码任务与对应别名...", totalTasks)

	type taskInfo struct {
		requestID  string
		aliasEmail string
		accountID  string
		leaseID    string
		token      string
		tokenID    string
	}
	tasks := make([]taskInfo, totalTasks)

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx failed: %v", err)
	}

	stmtAlloc, err := tx.PrepareContext(ctx, `
		INSERT INTO alias_allocations (
			allocation_id, alias_email, account_id, owner_kind, owner_id, status, allocated_at
		) VALUES (?, ?, ?, 'token', ?, 'allocated', ?)
	`)
	if err != nil {
		t.Fatalf("prepare alloc stmt failed: %v", err)
	}
	defer stmtAlloc.Close()

	stmtVreq, err := tx.PrepareContext(ctx, `
		INSERT INTO verification_requests (
			request_id, principal_kind, principal_id, lease_id, alias_email,
			status, baseline_provider, baseline_mailbox, baseline_uidvalidity, baseline_uid,
			created_at, expires_at
		) VALUES (?, 'token', ?, ?, ?, 'ready', 'imap', 'INBOX', 1, 100, ?, ?)
	`)
	if err != nil {
		t.Fatalf("prepare vreq stmt failed: %v", err)
	}
	defer stmtVreq.Close()

	for i := 0; i < totalTasks; i++ {
		accID := fmt.Sprintf("acc_%02d", (i%numAccounts)+1)
		leaseID := fmt.Sprintf("lease_%04d", i+1)
		aliasEmail := fmt.Sprintf("alias_%04d@icloud.com", i+1)
		reqID := fmt.Sprintf("vreq_%04d", i+1)
		assignedToken := tokens[i/(totalTasks/numTokens)] // 4 个 Token，每个 500 个任务

		if _, err := stmtAlloc.ExecContext(ctx, leaseID, aliasEmail, accID, assignedToken.ID, createdAtStr); err != nil {
			t.Fatalf("insert alloc failed at %d: %v", i, err)
		}

		if _, err := stmtVreq.ExecContext(ctx, reqID, assignedToken.ID, leaseID, aliasEmail, createdAtStr, expiresAtStr); err != nil {
			t.Fatalf("insert vreq failed at %d: %v", i, err)
		}

		tasks[i] = taskInfo{
			requestID:  reqID,
			aliasEmail: aliasEmail,
			accountID:  accID,
			leaseID:    leaseID,
			token:      assignedToken.Token,
			tokenID:    assignedToken.ID,
		}
		syncWorker.RegisterAliasAccount(aliasEmail, accID)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tx failed: %v", err)
	}
	t.Logf("成功建立 %d 个有效持久化取码任务 (4 个主体，每主体 500 任务)", totalTasks)

	// 4. 测量阶段 1: 建立 2000 个真实挂起的长轮询 TCP Socket 连接
	t.Logf("开始通过真实 TCP Socket 建立 %d 个长轮询挂起请求...", totalTasks)
	startMem := runtime.MemStats{}
	runtime.ReadMemStats(&startMem)
	startGoroutines := runtime.NumGoroutine()

	waiterReadyWg := sync.WaitGroup{}
	waiterReadyWg.Add(totalTasks)

	var clientWg sync.WaitGroup

	var activeHolders atomic.Int64
	var requestErrCount atomic.Int64
	var statusErrCount atomic.Int64
	var lastErrMsg atomic.Value
	var lastStatusCode atomic.Int64

	clientCtx, cancelClients := context.WithCancel(context.Background())
	defer cancelClients()
	t.Cleanup(func() {
		cancelClients()
		joined := make(chan struct{})
		go func() { clientWg.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("simulated capacity clients did not join")
		}
	})

	type clientResult struct {
		requestID   string
		code        string
		latency     time.Duration
		deliveredAt time.Time
		status      int
	}
	resultsCh := make(chan clientResult, totalTasks)

	startWait := time.Now()
	const batchSize = 25
	for i := 0; i < totalTasks; i += batchSize {
		end := i + batchSize
		if end > totalTasks {
			end = totalTasks
		}
		for j := i; j < end; j++ {
			task := tasks[j]
			clientWg.Add(1)
			go func(tInfo taskInfo) {
				defer clientWg.Done()
				activeHolders.Add(1)
				waiterReadyWg.Done()

				req, err := http.NewRequestWithContext(clientCtx, "GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=60", baseURL, tInfo.requestID), nil)
				if err != nil {
					activeHolders.Add(-1)
					return
				}
				req.Header.Set("Authorization", "Bearer "+tInfo.token)

				callStart := time.Now()
				resp, err := client.Do(req)

				activeHolders.Add(-1)
				if err != nil {
					if clientCtx.Err() != nil {
						return
					}
					requestErrCount.Add(1)
					lastErrMsg.Store(err.Error())
					return
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					statusErrCount.Add(1)
					lastStatusCode.Store(int64(resp.StatusCode))
					bodySample, _ := io.ReadAll(resp.Body)
					lastErrMsg.Store(string(bodySample))
					return
				}

				var resBody struct {
					Data struct {
						Code   string `json:"code"`
						Status string `json:"status"`
					} `json:"data"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&resBody); err != nil {
					statusErrCount.Add(1)
					lastErrMsg.Store(err.Error())
					return
				}

				if resBody.Data.Status != "succeeded" {
					statusErrCount.Add(1)
					lastErrMsg.Store("unexpected task status: " + resBody.Data.Status)
					return
				}
				// 时间点为响应解码并确认终态；验证码比对由随后统一断言完成。
				deliveredAt := time.Now()

				resultsCh <- clientResult{
					requestID:   tInfo.requestID,
					code:        resBody.Data.Code,
					latency:     time.Since(callStart),
					deliveredAt: deliveredAt,
					status:      resp.StatusCode,
				}
			}(task)
		}
		// Establish each bounded batch before the next one. This test measures
		// held connections, while 128/64 short-stage quotas still apply under -race.
		// No rejected request is retried or removed from the error counters.
		deadline := time.Now().Add(10 * time.Second)
		for {
			stats := limiter.Stats()
			if requestErrCount.Load() != 0 || statusErrCount.Load() != 0 {
				t.Fatalf("waiter establishment failed without retry: HTTP=%d status=%d last=%v", requestErrCount.Load(), statusErrCount.Load(), lastErrMsg.Load())
			}
			if stats.ActiveWaiters == end && stats.ActiveInflight == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("batch did not reach held state: waiters=%d want=%d inflight=%d", stats.ActiveWaiters, end, stats.ActiveInflight)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// 等待所有 2000 个 goroutine 启动
	waiterReadyWg.Wait()

	// 等待全部 2000 个连接进入 EventBus 订阅挂起状态
	var stats RequestLimiterStats
	for attempt := 0; attempt < 100; attempt++ {
		time.Sleep(50 * time.Millisecond)
		stats = limiter.Stats()
		if stats.ActiveWaiters == totalTasks {
			break
		}
	}
	establishmentDuration := time.Since(startWait)
	tasksPerSec := float64(totalTasks) / establishmentDuration.Seconds()

	midGoroutines := runtime.NumGoroutine()
	var midMem runtime.MemStats
	runtime.ReadMemStats(&midMem)

	t.Logf("【测量指标 1: 真实 TCP Socket 2000 连接挂起状态】")
	t.Logf("  - ActiveWaiters: %d (期望 %d)", stats.ActiveWaiters, totalTasks)
	t.Logf("  - ActiveInflight: %d (期望 0，已安全释放)", stats.ActiveInflight)
	t.Logf("  - PeakWaiters: %d", stats.PeakWaiters)
	t.Logf("  - WaiterRejections: (Key: %d, Principal: %d, Global: %d)",
		stats.WaiterKeyRejections, stats.WaiterPrincipalRejections, stats.WaiterGlobalRejections)
	t.Logf("  - InflightRejections: (Principal: %d, Global: %d)",
		stats.InflightPrincipalRejections, stats.InflightGlobalRejections)
	t.Logf("  - HTTPRequestErrors: %d (last: %v)", requestErrCount.Load(), lastErrMsg.Load())
	t.Logf("  - StatusErrors: %d (last status: %d, body: %v)", statusErrCount.Load(), lastStatusCode.Load(), lastErrMsg.Load())
	t.Logf("  - Goroutine 变化: %d -> %d (+%d)", startGoroutines, midGoroutines, midGoroutines-startGoroutines)
	t.Logf("  - 堆内存占用: %.2f MB", float64(midMem.HeapAlloc)/(1024*1024))
	t.Logf("  - 建立耗时: %v, 建立速率: %.0f sockets/sec", establishmentDuration, tasksPerSec)

	if stats.ActiveWaiters != totalTasks {
		t.Fatalf("ActiveWaiters 期望 %d，实际为 %d", totalTasks, stats.ActiveWaiters)
	}
	if stats.ActiveInflight != 0 {
		t.Fatalf("挂起后 ActiveInflight 必须为 0 (已释放)，实际为 %d", stats.ActiveInflight)
	}

	// 5. 测量阶段 2: 挂起期间前台短请求正常执行，不被 2000 个挂起连接阻塞/饿死
	t.Logf("测试挂起期间新短请求准入能力 (通过真实 TCP Socket)...")
	shortReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=0", baseURL, tasks[0].requestID), nil)
	shortReq.Header.Set("Authorization", "Bearer "+tokens[0].Token)
	shortStart := time.Now()
	shortResp, err := client.Do(shortReq)
	shortDuration := time.Since(shortStart)
	if err != nil || shortResp.StatusCode != http.StatusOK {
		t.Fatalf("短请求被长轮询挂起饿死: err=%v, resp=%+v", err, shortResp)
	}
	_ = shortResp.Body.Close()
	t.Logf("前台短请求响应正常: 状态码 200, 耗时 %v", shortDuration)

	// 6. 测量阶段 3: 模拟邮件批量到达，通过真实 MailSyncWorker 聚合扫描分发，测量端到端收码时延
	t.Logf("配置第 1 轮 100 封模拟邮件，并触发 MailSyncWorker 真实聚合同步...")
	const batchDeliverCount = 100
	deliverEmails := make([]mail.Message, batchDeliverCount)
	deliverFulls := make(map[uint32]*mail.FullMessage, batchDeliverCount)
	expectedCodes := make(map[string]string, batchDeliverCount)

	for i := 0; i < batchDeliverCount; i++ {
		uid := uint32(200 + i)
		code := fmt.Sprintf("%06d", 100000+i)
		task := tasks[i]
		expectedCodes[task.requestID] = code

		deliverEmails[i] = mail.Message{
			UID:         uid,
			UIDValidity: 1,
			Folder:      "INBOX",
			To:          task.aliasEmail,
			AccountID:   task.accountID,
		}
		deliverFulls[uid] = &mail.FullMessage{
			Message: mail.Message{
				UID:         uid,
				UIDValidity: 1,
				Folder:      "INBOX",
				To:          task.aliasEmail,
				AccountID:   task.accountID,
				Subject:     "Your verification code",
			},
			Body:         fmt.Sprintf("Your verification code is %s", code),
			BodyComplete: true,
		}
	}

	var mailAvailable atomic.Int32 // 0: no mail, 1: batch 1, 2: batch 2
	var currentEmails []mail.Message
	var currentFulls map[uint32]*mail.FullMessage
	var mailMu sync.Mutex

	setMailBatch := func(emails []mail.Message, fulls map[uint32]*mail.FullMessage) {
		mailMu.Lock()
		defer mailMu.Unlock()
		currentEmails = emails
		currentFulls = fulls
	}

	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		if mailAvailable.Load() == 1 {
			return "imap", 1, 301, nil
		} else if mailAvailable.Load() == 2 {
			return "imap", 1, 401, nil
		}
		return "imap", 1, 100, nil
	}
	fb.onScanMailboxUIDPage = func(ctx context.Context, q ScanPageQuery) (ScanPageResult, error) {
		if mailAvailable.Load() == 0 {
			return ScanPageResult{UIDValidity: 1, Messages: nil}, nil
		}
		mailMu.Lock()
		msgs := currentEmails
		mailMu.Unlock()
		nextUID := uint32(300)
		if mailAvailable.Load() == 2 {
			nextUID = 400
		}
		return ScanPageResult{
			UIDValidity: 1,
			Messages:    msgs,
			NextUID:     nextUID,
		}, nil
	}
	fb.onGetMessagesContext = func(ctx context.Context, accountID string, refs []mail.MessageRef) ([]*mail.FullMessage, error) {
		mailMu.Lock()
		fulls := currentFulls
		mailMu.Unlock()
		var out []*mail.FullMessage
		for _, ref := range refs {
			if f, ok := fulls[ref.UID]; ok {
				copyMsg := *f
				copyMsg.AccountID = ref.AccountID
				copyMsg.MessageRef = ref.Encode()
				out = append(out, &copyMsg)
			}
		}
		return out, nil
	}

	// 开启第 1 轮邮件到达标志，记录起点并触发 MailSyncWorker 单轮真实同步
	setMailBatch(deliverEmails, deliverFulls)
	mailDeliverStart := time.Now()
	mailAvailable.Store(1)
	syncWorker.syncOnce()

	// 等待第 1 轮 100 个挂起的长轮询 TCP 连接接收到 HTTP 200 响应
	var clientLatencies []time.Duration
	var deliveryLatencies []time.Duration
	receivedTasks := make(map[string]string)
	for i := 0; i < batchDeliverCount; i++ {
		select {
		case res := <-resultsCh:
			if res.status != http.StatusOK {
				t.Fatalf("任务 %s 收到非 200 响应: %d", res.requestID, res.status)
			}
			receivedTasks[res.requestID] = res.code
			clientLatencies = append(clientLatencies, res.latency)
			deliveryLatencies = append(deliveryLatencies, res.deliveredAt.Sub(mailDeliverStart))
		case <-time.After(10 * time.Second):
			t.Fatalf("超时未收齐第 1 轮全部 100 个邮件到达交付结果 (已收 %d/%d)", len(receivedTasks), batchDeliverCount)
		}
	}
	totalDeliverDuration := time.Since(mailDeliverStart)

	sort.Slice(clientLatencies, func(i, j int) bool { return clientLatencies[i] < clientLatencies[j] })
	sort.Slice(deliveryLatencies, func(i, j int) bool { return deliveryLatencies[i] < deliveryLatencies[j] })

	t.Logf("【测量指标 2: 第 1 轮邮件可用到客户端交付端到端时延 (Mail-to-Delivery, 100 邮件)】")
	t.Logf("  - 全程总耗时: %v", totalDeliverDuration)
	t.Logf("  - 分发时延 (邮件可用->客户端收到交付): min=%v, p50=%v, p90=%v, p99=%v, max=%v",
		deliveryLatencies[0],
		deliveryLatencies[len(deliveryLatencies)*50/100],
		deliveryLatencies[len(deliveryLatencies)*90/100],
		deliveryLatencies[len(deliveryLatencies)*99/100],
		deliveryLatencies[len(deliveryLatencies)-1])
	t.Logf("  - 客户端全往返耗时 (包含长轮询挂起等待): min=%v, p50=%v, p90=%v, p99=%v, max=%v",
		clientLatencies[0],
		clientLatencies[len(clientLatencies)*50/100],
		clientLatencies[len(clientLatencies)*90/100],
		clientLatencies[len(clientLatencies)*99/100],
		clientLatencies[len(clientLatencies)-1])

	// 6b. 第 2 轮新邮件到达（测试连续同步与持续长轮询交付稳定性）
	t.Logf("配置第 2 轮 100 封模拟邮件 (对应任务 100~199)...")
	deliverEmails2 := make([]mail.Message, batchDeliverCount)
	deliverFulls2 := make(map[uint32]*mail.FullMessage, batchDeliverCount)
	for i := 0; i < batchDeliverCount; i++ {
		uid := uint32(300 + i)
		code := fmt.Sprintf("%06d", 200000+i)
		task := tasks[100+i]
		expectedCodes[task.requestID] = code

		deliverEmails2[i] = mail.Message{
			UID:         uid,
			UIDValidity: 1,
			Folder:      "INBOX",
			To:          task.aliasEmail,
			AccountID:   task.accountID,
		}
		deliverFulls2[uid] = &mail.FullMessage{
			Message: mail.Message{
				UID:         uid,
				UIDValidity: 1,
				Folder:      "INBOX",
				To:          task.aliasEmail,
				AccountID:   task.accountID,
				Subject:     "Your verification code round 2",
			},
			Body:         fmt.Sprintf("Your code is %s", code),
			BodyComplete: true,
		}
	}

	setMailBatch(deliverEmails2, deliverFulls2)
	mailDeliverStart2 := time.Now()
	mailAvailable.Store(2)
	syncWorker.syncOnce()

	for i := 0; i < batchDeliverCount; i++ {
		select {
		case res := <-resultsCh:
			if res.status != http.StatusOK {
				t.Fatalf("第 2 轮任务 %s 收到非 200 响应: %d", res.requestID, res.status)
			}
			receivedTasks[res.requestID] = res.code
		case <-time.After(10 * time.Second):
			t.Fatalf("超时未收齐第 2 轮邮件到达交付结果 (已收 %d/%d)", len(receivedTasks), batchDeliverCount*2)
		}
	}
	t.Logf("第 2 轮 100 封邮件分发与客户端交付完成，耗时 %v", time.Since(mailDeliverStart2))

	// 7. 核查业务归属、防串码及跨租户防越权断言
	unauthorizedTok, err := st.CreateToken("unauthorized_worker", store.ScopeVerify, "")
	if err != nil {
		t.Fatalf("create unauthorized token failed: %v", err)
	}

	for i := 0; i < batchDeliverCount*2; i++ {
		task := tasks[i]
		expectedCode := expectedCodes[task.requestID]
		if receivedTasks[task.requestID] != expectedCode {
			t.Fatalf("任务 %s HTTP 接收到的验证码不匹配！期望 %s, 实际 %s",
				task.requestID, expectedCode, receivedTasks[task.requestID])
		}

		rec, err := st.GetVerificationRequest(ctx, task.requestID, "token", task.tokenID)
		if err != nil || rec.Status != "succeeded" || rec.Code != expectedCode {
			t.Fatalf("任务 %s 数据库记录校验失败: rec=%+v, err=%v", task.requestID, rec, err)
		}

		alloc, err := st.GetPrincipalAllocationByID(ctx, rec.LeaseID, "token", task.tokenID)
		if err != nil || alloc == nil || alloc.AccountID != task.accountID {
			t.Fatalf("任务 %s 归属母号被篡改！期望 %s, 实际 %+v", task.requestID, task.accountID, alloc)
		}

		// 跨主体越权隔离断言：使用 unauthorizedTok 查询必须返回 404 (RESOURCE_NOT_FOUND)
		unauthReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=0", baseURL, task.requestID), nil)
		unauthReq.Header.Set("Authorization", "Bearer "+unauthorizedTok.Token)
		unauthResp, err := client.Do(unauthReq)
		if err != nil {
			t.Fatalf("unauth get failed: %v", err)
		}
		_ = unauthResp.Body.Close()
		if unauthResp.StatusCode != http.StatusNotFound {
			t.Fatalf("跨主体未授权查询应当返回 404, 实际为: %d", unauthResp.StatusCode)
		}
	}
	t.Logf("跨母号防串码与跨租户防越权校验 100%% 通过！")

	// 8. 清理阶段：客户端取消剩余连接，验证连接安全释放与等待者归零
	t.Logf("取消剩余长轮询请求并验证连接释放...")
	cancelClients()

	for attempt := 0; attempt < 50; attempt++ {
		time.Sleep(50 * time.Millisecond)
		if limiter.Stats().ActiveWaiters == 0 {
			break
		}
	}
	endStats := limiter.Stats()
	t.Logf("清理后状态: ActiveWaiters=%d (期望 0), ActiveInflight=%d", endStats.ActiveWaiters, endStats.ActiveInflight)
	if endStats.ActiveWaiters != 0 {
		t.Fatalf("取消后 ActiveWaiters 必须归零，实际为: %d", endStats.ActiveWaiters)
	}

	// 验证所有客户端 goroutine 退出，断言无残留
	clientWgDone := make(chan struct{})
	go func() {
		clientWg.Wait()
		close(clientWgDone)
	}()
	select {
	case <-clientWgDone:
		t.Logf("全部 2000 个客户端 goroutine 均已安全退出，无 goroutine 泄漏。")
	case <-time.After(3 * time.Second):
		t.Fatalf("超时！仍有客户端 goroutine 未正常退出！")
	}
}

// TestConcurrencyScale_SimulatedTaskQuotaBoundary 使用 490 SQL + 10 POST 验证任务准入边界：
// 1. 单主体活跃任务上限 (ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ=500) 在真实 HTTP POST 接口下的严格准入与 429 拦截；
// 2. 独立主体配额物理隔离；
// 3. 终态任务释放名额后允许继续创建；
// 4. 单任务长轮询等待者上限 (ICLOUD_HME_MAX_WAITERS_PER_KEY=8) 与任务配额的独立生效。
func TestConcurrencyScale_SimulatedTaskQuotaBoundary(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	tempDir := t.TempDir()

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_prod_01", RealEmail: "prod@gmail.com", Status: "active"}},
	}
	fb.onGetMailboxEndpointFingerprint = func(accountID string) (string, bool) {
		return "fp_prod_01", true
	}
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}

	eventBus := mail.NewEventBus(time.Minute)
	syncWorker := NewMailSyncWorker(fb, st, eventBus, time.Second)

	// 生产环境 Compose 真实默认配置值:
	// ICLOUD_HME_MAX_GLOBAL_ACTIVE_VREQ=2000
	// ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ=500
	// ICLOUD_HME_MAX_WAITERS_GLOBAL=4000
	// ICLOUD_HME_MAX_WAITERS_PRINCIPAL=2000
	// ICLOUD_HME_MAX_WAITERS_PER_KEY=8
	// ICLOUD_HME_MAX_INFLIGHT_GLOBAL=128
	// ICLOUD_HME_MAX_INFLIGHT_PRINCIPAL=64
	prodLimiterCfg := RequestLimiterConfig{
		MaxWaitersGlobal:        4000,
		MaxWaitersPerPrincipal:  2000,
		MaxWaitersPerKey:        8,
		MaxInflightGlobal:       128,
		MaxInflightPerPrincipal: 64,
	}
	limiter := NewRequestLimiter(prodLimiterCfg)

	vService := NewVerificationService(fb, st, eventBus, syncWorker)
	vService.SetRequestLimiter(limiter)
	vService.maxGlobal = 2000
	vService.maxPerPrincipal = 500

	r := gin.New()
	r.Use(requireExternalV2Auth("", st, limiter))
	v2 := r.Group("/api/external/v2")
	srv := &Server{
		store:          st,
		verifyService:  vService,
		eventBus:       eventBus,
		requestLimiter: limiter,
	}
	v2.POST("/verification-requests", srv.externalV2CreateVerificationRequestHandler)
	v2.GET("/verification-requests/:request_id", srv.externalV2GetVerificationRequestHandler)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	httpServer := &http.Server{Handler: r}
	go func() { _ = httpServer.Serve(ln) }()
	defer httpServer.Close()

	baseURL := fmt.Sprintf("http://%s", ln.Addr().String())
	transport := &http.Transport{
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 1000,
		MaxConnsPerHost:     1000,
		DisableKeepAlives:   false,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
	defer client.CloseIdleConnections()

	// 1. 创建两个独立 API Token
	tok1, _ := st.CreateToken("prod_worker_1", store.ScopeVerify, "")
	tok2, _ := st.CreateToken("prod_worker_2", store.ScopeVerify, "")

	ctx := context.Background()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	expStr := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339)

	_, _ = st.DB().ExecContext(ctx, `INSERT OR IGNORE INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_prod_01', 'acc_prod_01', 'prod@gmail.com', 'active', '[]', ?, ?)`, nowStr, nowStr)

	// 2. 为 tok1 预置前 490 个有效活跃任务，并准备第 491~501 个别名租约
	const quota = 500
	for i := 1; i <= 490; i++ {
		allocID := fmt.Sprintf("prod_alloc_%04d", i)
		vreqID := fmt.Sprintf("prod_vreq_%04d", i)
		alias := fmt.Sprintf("prod_alias_%04d@icloud.com", i)
		_, _ = st.DB().ExecContext(ctx, `INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, status, allocated_at) VALUES (?, ?, 'acc_prod_01', 'token', ?, 'allocated', ?)`, allocID, alias, tok1.ID, nowStr)
		_, _ = st.DB().ExecContext(ctx, `INSERT INTO verification_requests (request_id, principal_kind, principal_id, lease_id, alias_email, status, baseline_provider, baseline_mailbox, baseline_uidvalidity, baseline_uid, created_at, expires_at) VALUES (?, 'token', ?, ?, ?, 'ready', 'imap', 'INBOX', 1, 100, ?, ?)`, vreqID, tok1.ID, allocID, alias, nowStr, expStr)
	}
	for i := 491; i <= quota+1; i++ {
		allocID := fmt.Sprintf("prod_alloc_%04d", i)
		alias := fmt.Sprintf("prod_alias_%04d@icloud.com", i)
		_, _ = st.DB().ExecContext(ctx, `INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, status, allocated_at) VALUES (?, ?, 'acc_prod_01', 'token', ?, 'allocated', ?)`, allocID, alias, tok1.ID, nowStr)
	}

	// 为 tok2 准备 1 个别名租约
	_, _ = st.DB().ExecContext(ctx, `INSERT INTO alias_allocations (allocation_id, alias_email, account_id, owner_kind, owner_id, status, allocated_at) VALUES ('tok2_alloc', 'tok2@icloud.com', 'acc_prod_01', 'token', ?, 'allocated', ?)`, tok2.ID, nowStr)

	// 3. tok1 通过真实 HTTP POST 接口连续创建 10 个任务 (到达 500 满额)
	for i := 491; i <= quota; i++ {
		allocID := fmt.Sprintf("prod_alloc_%04d", i)
		body := fmt.Sprintf(`{"lease_id":"%s"}`, allocID)
		req, _ := http.NewRequest("POST", baseURL+"/api/external/v2/verification-requests", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+tok1.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("tok1 创建第 %d 个任务失败: err=%v, code=%d", i, err, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	// 4. tok1 尝试通过 HTTP POST 创建第 501 个任务 -> 必须被单主体 500 配额拒绝，返回 429 TOO_MANY_REQUESTS 且带 Retry-After
	body501 := fmt.Sprintf(`{"lease_id":"prod_alloc_%04d"}`, quota+1)
	req501, _ := http.NewRequest("POST", baseURL+"/api/external/v2/verification-requests", bytes.NewBufferString(body501))
	req501.Header.Set("Authorization", "Bearer "+tok1.Token)
	req501.Header.Set("Content-Type", "application/json")
	resp501, err := client.Do(req501)
	if err != nil {
		t.Fatalf("req501 failed: %v", err)
	}
	defer resp501.Body.Close()

	if resp501.StatusCode != http.StatusTooManyRequests {
		bodyBytes, _ := io.ReadAll(resp501.Body)
		t.Fatalf("超出单主体配额期望 429, 实际得到: %d, body: %s", resp501.StatusCode, string(bodyBytes))
	}
	if resp501.Header.Get("Retry-After") == "" {
		t.Fatalf("429 响应头必须携带 Retry-After")
	}
	var errResp apiResp
	_ = json.NewDecoder(resp501.Body).Decode(&errResp)
	if errResp.Code != "TOO_MANY_REQUESTS" {
		t.Fatalf("429 错误码期望 TOO_MANY_REQUESTS, 实际为: %s", errResp.Code)
	}
	t.Logf("tok1 创建第 501 个任务被正确拦截: 429 TOO_MANY_REQUESTS (Retry-After: %s)", resp501.Header.Get("Retry-After"))

	// 5. 独立主体 tok2 尝试创建任务 -> 必须成功返回 200 OK，证明不同主体配额完全隔离
	bodyTok2 := `{"lease_id":"tok2_alloc"}`
	reqTok2, _ := http.NewRequest("POST", baseURL+"/api/external/v2/verification-requests", bytes.NewBufferString(bodyTok2))
	reqTok2.Header.Set("Authorization", "Bearer "+tok2.Token)
	reqTok2.Header.Set("Content-Type", "application/json")
	respTok2, err := client.Do(reqTok2)
	if err != nil || respTok2.StatusCode != http.StatusOK {
		t.Fatalf("tok2 创建任务失败: err=%v, code=%d", err, respTok2.StatusCode)
	}
	_ = respTok2.Body.Close()
	t.Logf("独立主体 tok2 成功创建任务，不受 tok1 满额影响")

	// 6. 终态释放验证：将 tok1 的第 1 个任务置为 succeeded 终态，释放 1 个活跃名额
	_, _, _ = st.CompleteVerificationRequestResult(ctx, "prod_vreq_0001", store.VerificationCompletion{Code: "123456", MatchedEventRef: "ev_prod_01"})

	// tok1 再次尝试创建第 501 个任务 -> 应当成功返回 200 OK
	reqRetry, _ := http.NewRequest("POST", baseURL+"/api/external/v2/verification-requests", bytes.NewBufferString(body501))
	reqRetry.Header.Set("Authorization", "Bearer "+tok1.Token)
	reqRetry.Header.Set("Content-Type", "application/json")
	respRetry, err := client.Do(reqRetry)
	if err != nil || respRetry.StatusCode != http.StatusOK {
		t.Fatalf("名额释放后 tok1 创建任务应当成功: err=%v, code=%d", err, respRetry.StatusCode)
	}
	var retryResp struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(respRetry.Body).Decode(&retryResp)
	createdReqID := retryResp.Data.RequestID
	_ = respRetry.Body.Close()
	t.Logf("终态释放后 tok1 成功补建任务: %s", createdReqID)

	// 7. 单任务长轮询等待者上限保护 (ICLOUD_HME_MAX_WAITERS_PER_KEY=8)
	// 验证等待者配额与任务配额是完全不同维度的保护机制
	clientCtx, cancelWaiters := context.WithCancel(context.Background())
	defer cancelWaiters()

	var waiterWg sync.WaitGroup
	waiterWg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer waiterWg.Done()
			req, _ := http.NewRequestWithContext(clientCtx, "GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=15", baseURL, createdReqID), nil)
			req.Header.Set("Authorization", "Bearer "+tok1.Token)
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}

	for attempt := 0; attempt < 50; attempt++ {
		time.Sleep(20 * time.Millisecond)
		if limiter.Stats().ActiveWaiters == 8 {
			break
		}
	}
	if limiter.Stats().ActiveWaiters != 8 {
		t.Fatalf("挂起等待者期望 8, 实际 %d", limiter.Stats().ActiveWaiters)
	}

	// 发起第 9 个长轮询 -> 超出单任务 8 个上限，返回 429 TOO_MANY_WAITERS_PER_KEY
	req9, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/external/v2/verification-requests/%s?timeout=15", baseURL, createdReqID), nil)
	req9.Header.Set("Authorization", "Bearer "+tok1.Token)
	resp9, err := client.Do(req9)
	if err != nil {
		t.Fatalf("req9 failed: %v", err)
	}
	defer resp9.Body.Close()
	if resp9.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("超出单任务等待者上限期望 429, 实际 %d", resp9.StatusCode)
	}
	var errResp9 apiResp
	_ = json.NewDecoder(resp9.Body).Decode(&errResp9)
	if errResp9.Code != "VERIFY_WAITER_LIMIT" {
		t.Fatalf("错误码期望 VERIFY_WAITER_LIMIT, 实际 %s", errResp9.Code)
	}
	t.Logf("单任务第 9 个长轮询被正确拒绝: 429 VERIFY_WAITER_LIMIT (Retry-After: %s)", resp9.Header.Get("Retry-After"))

	cancelWaiters()
}
func TestConcurrency_IdempotencyAndLimiterScenarios(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	tempDir := t.TempDir()

	st, err := store.NewStore(tempDir)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}
	defer st.Close()

	fb := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_01", RealEmail: "acc_01@gmail.com", Status: "active"}},
	}
	fb.onGetMailboxEndpointFingerprint = func(accountID string) (string, bool) {
		return "fp_01", true
	}
	fb.onGetMailboxBoundaryContext = func(ctx context.Context, accountID, folder string) (string, uint32, uint32, error) {
		return "imap", 1, 100, nil
	}

	eventBus := mail.NewEventBus(time.Minute)
	syncWorker := NewMailSyncWorker(fb, st, eventBus, time.Second)

	limiterCfg := RequestLimiterConfig{
		MaxWaitersGlobal:        100,
		MaxWaitersPerPrincipal:  50,
		MaxWaitersPerKey:        2, // 每键最多 2 个
		MaxInflightGlobal:       20,
		MaxInflightPerPrincipal: 10,
	}
	limiter := NewRequestLimiter(limiterCfg)

	vService := NewVerificationService(fb, st, eventBus, syncWorker)
	vService.SetRequestLimiter(limiter)

	r := gin.New()
	r.Use(requireExternalV2Auth("", st, limiter))
	v2 := r.Group("/api/external/v2")
	srv := &Server{
		store:          st,
		verifyService:  vService,
		eventBus:       eventBus,
		requestLimiter: limiter,
	}
	v2.POST("/verification-requests", srv.externalV2CreateVerificationRequestHandler)
	v2.GET("/verification-requests/:request_id", srv.externalV2GetVerificationRequestHandler)

	tok, err := st.CreateToken("scenario_worker", store.ScopeVerify, "")
	if err != nil {
		t.Fatalf("create token failed: %v", err)
	}

	ctx := context.Background()

	// 准备 allocation
	nowStr := time.Now().UTC().Format(time.RFC3339)
	_, _ = st.DB().ExecContext(ctx, `INSERT OR IGNORE INTO accounts (id, name, real_email, status, tags, created_at, updated_at) VALUES ('acc_01', 'acc_01', 'acc_01@gmail.com', 'active', '[]', ?, ?)`, nowStr, nowStr)

	_, _ = st.DB().ExecContext(ctx, `
		INSERT INTO alias_allocations (
			allocation_id, alias_email, account_id, owner_kind, owner_id, status, allocated_at
		) VALUES ('lease_test_01', 'test_alias@icloud.com', 'acc_01', 'token', ?, 'allocated', ?)
	`, tok.ID, nowStr)

	t.Run("A22: 创建幂等性-丢响应恢复与冲突校验", func(t *testing.T) {
		body := []byte(`{"lease_id":"lease_test_01"}`)
		idempotencyKey := "idem-key-999"

		// 第一次调用创建
		req1, _ := http.NewRequest("POST", "/api/external/v2/verification-requests", bytes.NewReader(body))
		req1.Header.Set("Authorization", "Bearer "+tok.Token)
		req1.Header.Set("Idempotency-Key", idempotencyKey)
		w1 := httptest.NewRecorder()
		r.ServeHTTP(w1, req1)

		if w1.Code != http.StatusOK {
			t.Fatalf("第一次创建期望 200, 实际 %d: %s", w1.Code, w1.Body.String())
		}
		var resp1 map[string]interface{}
		_ = json.Unmarshal(w1.Body.Bytes(), &resp1)
		data1 := resp1["data"].(map[string]interface{})
		reqID1 := data1["request_id"].(string)

		// 第二次调用：同键、同参数重放 -> 应恢复 200 且返回完全一致的 request_id
		req2, _ := http.NewRequest("POST", "/api/external/v2/verification-requests", bytes.NewReader(body))
		req2.Header.Set("Authorization", "Bearer "+tok.Token)
		req2.Header.Set("Idempotency-Key", idempotencyKey)
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, req2)

		if w2.Code != http.StatusOK {
			t.Fatalf("重放创建期望 200, 实际 %d: %s", w2.Code, w2.Body.String())
		}
		var resp2 map[string]interface{}
		_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
		data2 := resp2["data"].(map[string]interface{})
		reqID2 := data2["request_id"].(string)

		if reqID1 != reqID2 {
			t.Fatalf("幂等恢复未返回原任务ID！req1=%s, req2=%s", reqID1, reqID2)
		}

		// 第三次调用：同键、不同参数（使用不同 lease_id） -> 应返回 409 IDEMPOTENCY_CONFLICT
		_, _ = st.DB().ExecContext(ctx, `
			INSERT INTO alias_allocations (
				allocation_id, alias_email, account_id, owner_kind, owner_id, status, allocated_at
			) VALUES ('lease_test_02', 'test_alias2@icloud.com', 'acc_01', 'token', ?, 'allocated', ?)
		`, tok.ID, nowStr)

		conflictBody := []byte(`{"lease_id":"lease_test_02"}`)
		req3, _ := http.NewRequest("POST", "/api/external/v2/verification-requests", bytes.NewReader(conflictBody))
		req3.Header.Set("Authorization", "Bearer "+tok.Token)
		req3.Header.Set("Idempotency-Key", idempotencyKey)
		w3 := httptest.NewRecorder()
		r.ServeHTTP(w3, req3)

		if w3.Code != http.StatusConflict {
			t.Fatalf("冲突参数期望 409, 实际 %d: %s", w3.Code, w3.Body.String())
		}
	})

	t.Run("A01: 单任务等待者限制与取消释放", func(t *testing.T) {
		reqRecord, _ := st.GetVerificationRequestByIdempotencyKey(ctx, "token", tok.ID, "idem-key-999")
		if reqRecord == nil {
			t.Fatalf("未找到前置任务")
		}

		// limiter 限制 MaxWaitersPerKey 为 2
		clientCtx, cancelClient := context.WithCancel(context.Background())
		defer cancelClient()

		// 挂起第 1 个连接
		w1Done := make(chan struct{})
		go func() {
			req, _ := http.NewRequestWithContext(clientCtx, "GET", fmt.Sprintf("/api/external/v2/verification-requests/%s?timeout=10", reqRecord.RequestID), nil)
			req.Header.Set("Authorization", "Bearer "+tok.Token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			close(w1Done)
		}()

		// 挂起第 2 个连接
		w2Done := make(chan struct{})
		go func() {
			req, _ := http.NewRequestWithContext(clientCtx, "GET", fmt.Sprintf("/api/external/v2/verification-requests/%s?timeout=10", reqRecord.RequestID), nil)
			req.Header.Set("Authorization", "Bearer "+tok.Token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			close(w2Done)
		}()

		time.Sleep(100 * time.Millisecond)

		// 发起第 3 个连接 -> 应当被单键 2 个等待者限制挡住，返回 429
		req3, _ := http.NewRequest("GET", fmt.Sprintf("/api/external/v2/verification-requests/%s?timeout=10", reqRecord.RequestID), nil)
		req3.Header.Set("Authorization", "Bearer "+tok.Token)
		w3 := httptest.NewRecorder()
		r.ServeHTTP(w3, req3)

		if w3.Code != http.StatusTooManyRequests {
			t.Fatalf("超出单键等待者限制应返回 429, 实际为 %d: %s", w3.Code, w3.Body.String())
		}

		// 客户端取消其中一个连接
		cancelClient()
		<-w1Done
		<-w2Done

		time.Sleep(100 * time.Millisecond)

		// 取消释放后，新连接应当能够成功申请进入
		req4, _ := http.NewRequest("GET", fmt.Sprintf("/api/external/v2/verification-requests/%s?timeout=0", reqRecord.RequestID), nil)
		req4.Header.Set("Authorization", "Bearer "+tok.Token)
		w4 := httptest.NewRecorder()
		r.ServeHTTP(w4, req4)

		if w4.Code != http.StatusOK {
			t.Fatalf("取消释放后新请求应当通过，实际为 %d: %s", w4.Code, w4.Body.String())
		}
	})
}

// TestRequestBodyReadTimeout_RejectsSlowBody 验证慢速请求体超出读取时限时被物理超时拒绝 (PR-CONCURRENCY T1)
func TestRequestBodyReadTimeout_RejectsSlowBody(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(bodyLimitMiddleware())
	r.POST("/test/body", func(c *gin.Context) {
		var body map[string]interface{}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 模拟已超过 deadline 的慢速 body reader
	expiredBody := &readTimeoutReader{
		rc:       io.NopCloser(bytes.NewReader([]byte(`{"hello":"world"}`))),
		deadline: time.Now().Add(-1 * time.Second), // 读前已超时
	}

	req, _ := http.NewRequest("POST", "/test/body", nil)
	req.Body = expiredBody
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("超时的请求体读取应当失败，实际状态码: %d", w.Code)
	}
}
