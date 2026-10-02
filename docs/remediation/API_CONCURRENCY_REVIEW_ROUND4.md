# API 并发与收信可靠性第四轮审查

日期：2026-10-02。审查对象为 Gemini 最新未提交的工作区代码和第三轮修订验收报告，Git HEAD 为 `178ff25030e6525e8bf616989f3ecb2824de119d`。本轮仅审查、运行测试和保存诊断材料，没有修改业务实现、提交、推送或部署。

## 1. 结论

**修复有实质进展，但不能验收为“全部完成、全库测试通过、2000 任务稳定可上线”。**

1. 原 HTTP handler 未退出就关闭 Store、兼容取码不响应服务取消、清理流程首次超时后永远停止等缺陷已改善，原回归测试通过。
2. IMAP 前台不同条目的保留规则、email 查询数据库错误、批量取信忙状态映射、HME closing 原子同步已经落地；后台保留条目能完成本地 IMAP SELECT 的额外验证也通过。
3. 本轮原始 2000 长轮询测试实际失败一次；随后单独运行三次通过。固定时序探针证实后台完成事务仍会被正常并发写者击穿，不能把偶尔通过当作稳定。
4. IMAP LOGOUT 的底层超时会被当前 go-imap 库清空，连接池关闭仍有无界等待路径。
5. 全库测试实际失败；另有错误文本中的 UUID 数字被误当作认证状态码的问题。
6. T06 的正式装配、HTTP 建立全部任务、协议层 IMAP、真实 Worker 周期和持续容量证据仍未完成。报告继续超过实际测试覆盖范围。

本机诊断并不证明真实 Gmail 收信质量、Apple 接口容量或 Linux 容器停机情况。三个确定性失败探针使用临时数据库、本机 net.Pipe 和虚构错误，不使用生产数据或真实账号。

## 2. 第三轮修复逐项复核

| 原编号 | 当前结论 | 实际证据及边界 |
|---|---|---|
| T01 | 原 Store 过早关闭问题已通过回归 | 顶层 requestTrackingMiddleware 登记整个 handler；关停停止新增登记；CloseContext 等 reqWg 再关 Store。两个原 POST/慢请求体回归通过。慢正文测试中 CloseContext 可以超时，但 Store 保持打开，这正是原不变量修复。Read 前检查 Context 不能打断已经阻塞的 Read，不能声称正文立即取消。 |
| T02 | 原兼容取消和首次超时终结清理问题已修 | 兼容 handler 增加服务取消；唯一清理协程与调用方等待期限分离。原长轮询收尾测试通过。但真实池关闭及进程总体预算仍有 U02。 |
| T03 | 前台条目保留修复成立 | foregroundEntries/fgPinCount 按不同前台条目计数，同条目引用不重复计数。额外探针在两个前台等待者占用两条目时，后台借用第三条目并通过本地协议完成 SELECT，返回 7 封，重复三次通过。原正式测试只检查不是 ErrIMAPPoolBusy，后台实际连接超时也会通过，需要补强该测试。 |
| T04 | 普通 email 路径已修，细项未闭环 | email 查租约采用 5 秒阶段预算，真实 DB 故障回归返回 500。管理员路由回退仍使用无独立期限的父 Context，取消与超时也仍被合并为 504，见第 4 节。 |
| T05 | 已修 | GetMessagesContext 识别 ErrIMAPPoolBusy，真实 HTTP 回归返回 503 SERVER_BUSY 并带 Retry-After。 |
| T06 | 未闭环 | 配置名和任务/等待者概念已经修正，但装配、测试数据与稳态范围仍不符合要求，见 U03。 |
| T07 | 原字段同步问题已修，race 未验证 | closing 已全面使用 atomic.Bool 的 Load/Store；正式功能回归通过。本环境 race 命令因 CGO_ENABLED=0 无法执行。 |

## 3. 必须处理的问题

### U01 — P1：后台完成事务仍会被正常并发写入击穿

位置：[后台匹配事务](../../internal/store/inventory_vreq.go:578)、[单任务 CAS](../../internal/store/inventory_vreq.go:332)、[长轮询事件处理](../../internal/server/verification_service.go:622)、[后台结果持久化](../../internal/server/mail_sync.go:1051)。

`CompleteMatchingVerificationRequests` 使用普通 BeginTx，先 SELECT 匹配 ID，再 UPDATE。它持有 Store.mu，但 `CompleteVerificationRequestResult` 不持有该锁；令牌活动刷盘及其他数据库写入也不是全部经这把锁串行。

后台已经建立 SQLite WAL 读取快照时，其他连接能够提交写入。随后将旧读取事务升级为写入事务，会立即失败为 SQLITE_BUSY_SNAPSHOT；另一个写者尚未释放写入权时也可能表现为 SQLITE_BUSY。配置 busy_timeout=5000 无法使失效的读取快照自动变成可写快照。

**原有压测实际失败：**

```text
[MailSync] 持久化验证码终态失败 (alias_0011@icloud.com UID=210): database is locked (5) (SQLITE_BUSY)
超时未收齐第 1 轮全部 100 个邮件到达交付结果 (已收 10/100)
FAIL TestConcurrencyScale_2000WaitersSharedInbox (11.04s)
```

**固定时序根因探针连续三次失败：**

1. 调用真实的 CompleteMatchingVerificationRequests，完成其 SELECT。
2. 在 SELECT 与 UPDATE 之间，调用真实 CompleteVerificationRequestResult 完成另一正常任务；该 CAS 成功提交。
3. 后台继续原 UPDATE，返回 `database is locked (517)`；目标任务仍为 ready，完成列表为空。

```text
otherWon=true otherErr=<nil> completionErr=database is locked (517)
sqliteCode=517 completed=0 targetStatus=ready
```

探针仅包装 SQLite 驱动安排调用顺序，没有模拟数据库失败或替换生产完成方法。它验证了代码中存在的具体竞争窗口；原压测日志只有错误码 5，因此不把那次自然失败强行写成错误码 517。

生产 Worker 会继续后续轮次，当前失败页也没有被直接推进游标，因此不能据此声称验证码永久丢失。但这会中断本轮分发、延迟后续任务并使容量测试不稳定。

**最小修复要求：**完成事务应在建立待写快照之前取得写入权。优先考虑单条带完整匹配条件的 UPDATE … RETURNING，正确读取并关闭结果集、提交后再发布事件；或使用明确取得写入权的事务。保留终态 CAS、期限、Mailbox/UIDVALIDITY 和权威结果语义。不要只消掉长轮询的重复写入、扩大 busy_timeout 或增加无条件 retry，因为其他并发写者仍会触发同一根因。

**验收：**正常后台完成与单任务完成、令牌活动刷盘、过期处理并发时，不发生快照升级失败；完整 HTTP 收码通过，多轮统计所有失败，不筛选成功的一次。诊断探针可以转成正式回归，但应按最终实现安排真实竞争，不机械依赖旧 SELECT 的文本。

### U02 — P1：IMAP 登出没有有效期限，停机仍可能卡住

位置：[连接池关闭](../../internal/mail/pool.go:386)、[Disconnect](../../internal/mail/client.go:311)、[Server 清理](../../internal/server/server.go:590)、[Manager.Close](../../internal/account/manager.go:110)、[main 退出](../../main.go:299)。依赖为 go-imap v1.2.1，见 [go.mod](../../go.mod:8)。

`Disconnect` 先 SetDeadline(now+90s)，然后调用 cli.Logout。项目没有设置底层 cli.Timeout；当前 go-imap 的 execute 在 Timeout=0 时会调用 SetDeadline(time.Time{})。因此，90 秒的外部 deadline 会被清空，LOGOUT 可以一直等待不应答的服务端。

**本地协议探针连续三次复现：**假 IMAP 发送正常 PREAUTH 欢迎语，读到生产 Pool.Close 发来的 LOGOUT，之后不应答。记录的最终 deadline 为零；Pool.Close 一直等待，手动关闭假服务端后才退出。

```text
effectiveDeadlineCleared=true pendingUntilForcedPeerClose=true
deadline=0001-01-01 00:00:00 +0000 UTC
```

该探针观察了 150ms 挂起及实际 deadline 清空，没有等待真实 30/90 秒，也没有执行 Linux SIGTERM。无界等待结论由实际 deadline 记录、当前依赖源码及关闭调用链共同支持。

Pool.Close 串行调用每个客户端 Disconnect。CloseContext 的调用方超时仅停止等待，底层清理仍会卡在 Backend.Close，Store 和 closeDone 尚未收尾。main 随后的 defer mgr.Close 会再次等待同一池关闭完成。因此，新增唯一清理协程没有消除该路径；Compose 的 30 秒 grace 到期后可能只能强杀。

Run 目前也先做 10 秒 HTTP Shutdown，之后才进入服务取消/CloseContext；长轮询会先耗尽 HTTP drain，而非在收到信号时立即取消所有业务。随后还有一次 10 秒 CloseContext 和 defer Close，不能称为单一总体预算。

**修复要求：**关停先停止业务准入、广播服务取消，再协调 HTTP drain 和资源清理；整个进程采用明确的剩余预算。在请求/后台退出后，对闲置连接使用有效的有限登出期限，超时物理断开连接并完成池关闭。可以直接强制关闭已无业务使用的连接，或正确设置库的命令期限并接入强制断开。不要仅扩大 Compose grace，不要在未清理完时宣称成功。

**验收：**LOGOUT 黑洞、慢请求体、120 秒长轮询和重复 Close 都能在预算内收尾；所有 handler/worker/关闭协程退出、Store 关闭后才报告成功。Linux 容器实际 SIGTERM 验证另列，不能由本机单测替代。

### U03 — P1：容量验收仍不等效，报告不能宣称 T06 完成

位置：[主压测装配](../../internal/server/concurrency_scale_test.go:74)、[50 个基准任务](../../internal/server/concurrency_scale_test.go:152)、[手动同步](../../internal/server/concurrency_scale_test.go:519)、[配额测试预置](../../internal/server/concurrency_scale_test.go:770)、[验收声明](../../docs/remediation/API_CONCURRENCY_VALIDATION.md:64)。

已做对的部分：修正了真正的配置名；改为 4 个 Token 分担任务；验证的是第 501 个活跃任务的 POST 准入；补了终态释放、独立主体和同任务第 9 个等待者拒绝；客户端时间点移到了 JSON 解码之后；补了第二轮 100 个结果。

**仍未完成的事实：**

- 自建 Gin、手工构造 Server、手工赋值 Service 配额，没有正式 Config/Server 构造器及完整中间件装配。
- 实际 Compose 和 main 的短阶段默认上限为 **128/64**，测试写成 **256/128**；本轮安全读取的 Compose 解析结果也为 128/64。报告却仍写“生产默认配置”。
- 主测试先为第一个 Token 通过 POST 建立 50 个活跃任务，随后 SQL 插入 2000 个活跃任务，未清理前者。按实际准备流程，全局为 **2050**、第一个主体为 **550**，已经超过测试声明的 2000/500 上限。SQL 预置绕过了生产 POST 准入，不能称为 4×500 的正常生产状态。
- 配额测试是 **490 个 SQL 预置 + 10 个 POST** 到达 500，报告写成“500 个全部通过 POST 创建”。这个测试能够证明准入边界，但不能证明 500 个完整创建流程。
- 底层仍为 fakeBackend 回调，源码明确没有假 IMAP 协议端口；两轮都是直接调用 syncOnce，没有 Worker.Start 的真实周期。
- 独立三次通过的整个测试仅 **1.47s / 1.45s / 1.41s**。GET 显式 timeout=60 不代表维持了 60 秒，没有持续稳态、取消重连、混合争用或超额档位。
- 客户端最多重试五次，但 DialErrors 仅计最终失败；短阶段内失败后重试成功的请求没有被统计为失败尝试。client.Do 错误也未区分 TCP dial 与其他 HTTP/读取错误，不能证明所有建连尝试均零失败。
- deliveredAt 在 JSON 解码后、验证码比对前记录，验证码比对发生在后面的统一断言。可以写“成功解码响应”，不能写成取点时已经完成验证码匹配。
- Go 堆包含同进程客户端和服务端；没有独立服务端 RSS/FD 数据。单次短请求成功不能证明持续混合流量无饿死。
- 报告仍将接口默认等待统一写成 60 秒；实际 v2 默认 0、兼容取码默认 30 秒。测试文件仍残留 Gmail“单 IP 15 连接”的错误说明，报告已改为 per-account，应同步纠正。

**验收要求：**保留并如实命名现有模拟/边界测试；另外经正式 Config/Server、真实 HTTP POST 创建目标任务、协议层假 IMAP 和真实 Worker.Start，验证至少 60 秒稳态、多轮收码、取消重连、短请求混合争用和超额拒绝。精确记录所有尝试与响应失败、实际任务数、等待者数和不同资源指标。不具备环境的项写未验证，不补写成已经通过，不新增 API 版本或配置别名来迁就报告。

### U04 — P2：错误文本中的 UUID 数字会被误识别为认证失效

位置：[错误分类](../../internal/server/backend.go:1176)、[isSessionError](../../internal/server/backend.go:1183)、[随机 clientID 与请求 URL](../../internal/hme/client.go:131)、[原有回归](../../internal/server/backend_test.go:997)。

原始 `go test ./... -count=1 -timeout 180s` 在 TestBatchUpdateAliasesReportsPreflightFailure 失败：应返回 UPSTREAM_FAILURE，实际返回“iCloud 会话失效,请更新 Cookie”。之后单独重复三次又通过，因此不将其描述为每次必现。

`isSessionError` 对整个 err.Error() 搜索任意 `401`、`403`、`421` 字符串。HME 的请求 URL 含随机 UUID clientId，而传输层错误可能包含该 URL。状态判断因此可能取决于 UUID 的内容。

固定 URL 探针使用同一个标准 url.Error（代理 Bad Gateway），只改变 UUID 尾部，连续三次复现：

```text
...000000000aaa -> 502 UPSTREAM_FAILURE
...000000000401 -> 401 UPSTREAM_UNAUTHORIZED
```

其中没有实际上游认证失败。该探针确定证明分类缺陷；全库那一次失败没有保留原始上游错误/UUID，因此随机 UUID 导致那一次失败属于有依据的推断，不冒充已抓取的原始错误证据。这是现有分类问题，不声称由本次优化新引入。

**修复要求：**优先使用 hme.ErrAuthFailed 等明确的错误类型和真实响应状态；传输失败及 5xx 保持对应故障分类。若必须保留历史文本识别，要识别确定的状态字段，而非整条字符串中任意数字。保留真实认证失效、身份错误、忙状态、取消、超时的原契约。

**验收：**带 401/403/421 片段的 UUID、代理地址、请求 ID 不改变网络故障分类；真实认证拒绝仍正确返回；原批量预检测试多次运行及全库测试均通过。

## 4. 未闭环的较小事项

1. [管理员路由回退](../../internal/server/verification_service.go:279) 仍调用 FindAliasRouteContext(ctx, …)，没有使用独立短阶段期限。请求取消已经能传播，但父请求没有 Deadline 时，等待 DB 连接仍没有这一步的明确上限。补有限预算并保持 DB 错误传播。
2. [email 查询的取消处理](../../internal/server/external_v2_handlers.go:225) 将 context.Canceled 和 DeadlineExceeded 合并成 504 DEADLINE_EXCEEDED，仍未满足第三轮要求的两类区分。沿当前统一错误契约处理，不伪装成功。
3. 原后台条目回归的 listener 不 Accept，后台 TLS 操作最终超时，而断言仍通过。应固化本轮可应答的协议探针，至少断言回调执行、操作成功、资源释放。注入已连接客户端的本轮 SELECT 探针不证明 TLS 建连，二者覆盖边界应写清。

## 5. 本轮验证记录

| 检查 | 实际结果 |
|---|---|
| Round3 / ReReview / ReviewProbe 正式回归 | 全部通过；同批次 2000 主压测失败，所以整条目标命令退出 1。 |
| TestConcurrencyScale_ProductionTaskQuota | 通过；真实验证准入边界，受 U03 的装配边界限制。 |
| TestConcurrencyScale_2000WaitersSharedInbox | 首次失败，SQLITE_BUSY，10/100 交付；随后单独 -count=3 全过，不能抹去失败。 |
| go test ./... -count=1 -timeout 180s | 退出 1；internal/server 因 TestBatchUpdateAliasesReportsPreflightFailure 失败（61.761s）。其余有测试包通过。 |
| 单独批量预检回归 -count=3 | 三次通过；证明有非确定性，不能代替全库一次失败的记录。 |
| 固定时序 SQLite 完成探针 -count=3 | 三次失败，SQLITE_BUSY_SNAPSHOT=517；并发 CAS 成功，目标仍 ready。 |
| LOGOUT 黑洞探针 -count=3 | 三次失败，有效 deadline 被清空。测试释放本机服务端后已完整回收。 |
| 保留条目实际 SELECT 探针 -count=3 | 三次通过，回调执行、返回 count=7、err=nil。 |
| UUID 与传输错误分类探针 -count=3 | 三次失败，数字 401 导致错误分类变化。 |
| go vet ./... | 通过。 |
| git diff --check | 通过。 |
| docker compose config --quiet | 通过；仅证明编排可解析。 |
| go test -race … | 未能执行：`-race requires cgo; enable cgo by setting CGO_ENABLED=1`。 |
| 真实 Gmail/Apple、Linux SIGTERM、服务器持续容量 | 本轮未验证。 |

原始命令与日志保存目录：

```text
C:/Users/42013/AppData/Local/Temp/icloud-hme-round4-a5c6b3b894884d1a8551a3983dee7ad4
```

关键日志为 targeted-results.txt、scale-repeat-results.txt、full-results.txt、preflight-repeat-results.txt、round4-final-probe-results.txt、race-results.txt。本轮保留了失败及后续通过结果，没有修改原测试断言或用缓存覆盖失败证据。

本轮开始已有工作区修改的 SHA256 快照为 workspace-start-hashes.json；审查结束核对了其中 54 个原文件，均未变化或缺失。本轮新增的持久化材料只有本报告及第 6 节诊断源文件。

## 6. 可复现诊断材料

> 原始诊断日志与 `.go.txt` 探针副本已于 2026-10-02 清理，不再随仓库保留；对应问题均已转为正式回归测试。以下回归测试覆盖本轮三类探针：

- SQLite 固定时序：`internal/store/completion_concurrency_test.go` (`TestCompletionSurvivesConcurrentResultWriter`)
- 保留条目与 LOGOUT：`internal/mail/pool_entry_reserve_test.go`、`internal/mail/shutdown_test.go` (`TestPoolCloseDoesNotWaitForLogout`)
- UUID 错误分类：`internal/server/auth_status_cancel_regression_test.go`

```powershell
go test ./internal/store ./internal/mail ./internal/server -run "CompletionSurvives|ForegroundPins|PoolCloseDoesNotWait|TransportIdentifiers|ExplicitAuthStatus" -count=1 -v
```

## 7. 后续顺序

先修 U01 数据库完成事务，再修 U02 停机底层等待和 U04 错误分类；补第 4 节两处期限/分类遗漏。随后完成 U03 的测试装配与持续验收，按真实执行记录重写验收报告。

在这些问题关闭前，结论应保持“已完成部分修复，仍有可复现可靠性缺陷，生产高并发容量未验收”。原修改仍未提交；GitHub 推送和生产部署需要各自实际执行与验证。
