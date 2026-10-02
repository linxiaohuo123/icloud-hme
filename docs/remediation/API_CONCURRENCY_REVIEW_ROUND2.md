# API 并发优化第二轮审查

审查日期：2026-10-01。HEAD：178ff25030e6525e8bf616989f3ecb2824de119d。审查对象为 Gemini 本轮未提交改动，包括业务代码、配置、测试和验收报告。实际工具链：Go 1.27.0 windows/amd64，CGO_ENABLED=0。

结论：本轮确实修复了上次的多个问题，原有八项复现及新增两个启动检查全部通过；但仍存在终态重放错误、热点账号耗尽全局操作名额、关闭后客户端重建、停机时 HTTP 请求未退出便关闭存储等问题。当前不能根据验收报告宣布正确性全部达标或稳定支持 2000—4000 生产任务。建议先完成下列修复，再验收部署条件。

本次只新增本报告。未修改业务代码、Gemini 测试或原验收报告；未提交、推送或部署。新增复现测试使用 Go overlay 放在临时目录，未进入项目工作区；全部使用临时数据库、虚构凭据和本机 HTTP，没有访问真实 Apple/Gmail 或生产数据。

## 1. 上轮 R01—R10 的实际状态

| 上轮编号 | 本轮复查结论 |
|---|---|
| R01 历史库升级失败 | 原 v9 升级场景已修复；v10 缺索引启动恢复、重复启动测试通过。不能据此代替所有迁移失败回滚场景验收。 |
| R02 指纹泄露代理密码 | 原日志指纹不再包含明文代理密码，原复现通过。 |
| R03 共享邮箱引用属于代表母号 | 已为真实目标母号生成引用，原跨母号引用解析复现通过。完整真实 IMAP 与后续详情读取容量场景仍未验证。 |
| R04 同键不同 lease 返回 500 | 主体+键锁已经接入，原真实 HTTP 并发复现通过：一个成功、一个 409，只读取一次基线。 |
| R05 过期/终态重放标为可发送 | baseline_ready 的原问题已修复，但新到期收敛忽略 CAS 结果与错误，见 S01。 |
| R06 后台类别未接通 | 后台标记已沿生产调用传递，新增 Backend 保留容量测试通过；操作名额顺序和 HTTP 忙契约仍有问题，见 S02/S07。 |
| R07 SQL 错误传播 | 批量基线故障停止批次、凭据 DB 故障不变成撤销，原复现通过；租约查询和新重放路径仍吞错，见 S01/S06。 |
| R08 HME 生命周期 | 关闭后新 acquire 已拒绝；已借出排队者仍能在 Close 返回后重建客户端，drop 等待不能取消，见 S03。 |
| R09 真实容量证据 | 2000 等待者已改用真实 HTTP TCP，测试通过；主收信仍为 fakeBackend、任务仍直接插库，验收结论继续超出证据，见 S08。 |
| R10 活跃任务配置 | main → Config → Service 和 Compose 已接通这两个任务配额；本机 Compose 解析得到全局 2000、单主体 500。基线/上游候选限制仍主要为代码常量，报告的容量推荐未完成生产等效验证。 |

附加项中，Cookie 准入已检查 BindPrincipal 错误；共享 checkpoint 已改用物理邮箱指纹；API.md 已补幂等契约。创建阶段期限、停机验证及文档准确性仍未闭环。

## 2. 剩余问题

### S01 — P1：到期重放伪造 expired，忽略真实 CAS 胜者和持久化失败（已复现）

位置：internal/store/inventory_vreq.go:716、745—746。

GetVerificationRequestByIdempotencyKey 读取旧快照后执行 ExpireVerificationRequest，却丢弃返回的权威记录、CAS 是否成功及 error，并直接把旧快照的 Status 改成 expired。

两项隔离复现：

1. 在临时库注入过期更新失败。真实 HTTP 重放仍返回 200、status=expired，但数据库记录仍是 ready。
2. 使用 SQLite 触发器确定性模拟读快照后成功 CAS 已胜出。数据库为 succeeded、code=123456；重放却返回 expired、空 code。

第二项没有证明数据库的成功记录被覆盖：数据库 CAS 保住了成功记录，错误发生在重放返回的状态与数据。

修复：使用 ExpireVerificationRequest 返回的权威记录；CAS 输给成功终态时返回成功终态；存储失败显式向上返回错误；不凭旧快照修改状态。保持原 request_id、基线和有效期。

### S02 — P1：同一个账号的排队请求占满全局上游名额（IMAP/HME 均已复现）

位置：internal/account/hme_pool.go:395、408；internal/mail/pool.go:250、264。

当前实现先申请全局活跃操作名额，再等同账号/同物理邮箱的串行锁。尚未执行网络操作的排队请求已经占用名额。与方案 T3 明确规定的“账号串行完成后、网络开始前取得操作名额”相反。

复现：

- HME：一个账号正在执行，其余 15 个请求只在等同账号锁，activeOps 已达 16。一个独立账号也被 ErrHMEOpBusy 拒绝。
- IMAP：两个调用只等共享邮箱信号量，已占满测试配置的全部前台操作名额。独立邮箱被 ErrUpstreamBusy 拒绝，没有开始网络操作。

这会把热点账号排队扩大成其他账号的服务拒绝。用户多母号共用 Gmail 的拓扑尤其需要区分“等待物理邮箱锁”和“正在执行网络操作”。

修复：条目安全借用/pin → 可取消的账号串行 → 活跃操作准入 → 网络操作；释放顺序与关闭协议一致。调整后还必须保留后台条目容量和操作容量，不能通过增加同邮箱客户端实例绕过串行约束。混合测试同时检查实际执行数量、排队数量与独立账号进展。

### S03 — P1：HME Close 后排队借用还能重建客户端；drop 等待无法取消（已复现）

位置：internal/account/hme_pool.go:153—156、201、231、389—428。

closed 检查只覆盖新 acquire。Close 等待当前条目锁、关闭客户端后就返回，未等待所有已 pin 的借用者退出。排队者后续拿到旧条目锁时没有复查 closing/closed，仍可为已脱离池管理的条目重建客户端。

通过真实 Manager.WithHMEClientContext 调用及并发屏障复现：
Close 已返回，排队回调仍执行，旧条目再次持有非 nil 客户端，queuedError 为 nil。测试结束显式关闭该孤立客户端，避免临时测试泄漏。

另外，同键旧条目正在 drop 时 acquire 使用裸 <-closedDone。30ms 截止的请求在 120ms 观察窗口内仍不返回，直到旧操作结束才退出。

修复：关闭与已借出引用共同管理最终完成；等条目锁后检查关闭状态，避免关闭完成后重建；同键 closing 等待接受 Context；关闭完成后所有借用者已退出或已明确拒绝。补排队借用、drop/取消、Close/取消和重复关闭测试。

### S04 — P1：停机时长轮询未退出，存储已关闭（已通过本机 HTTP 复现）

位置：internal/server/server.go:333、340、447、532；internal/server/verification_service.go 的长轮询等待。

Run 先 HTTP Shutdown，再 CloseContext。Shutdown 到期仅记录错误，没有取消/强制收尾尚存连接；随后 s.cancel 只作用于后台生命周期，现有 HTTP 请求 Context 未接入该生命周期。CloseContext 可以关闭 Store，而长轮询仍挂起。

复现使用真实 HTTP 请求、相同 Handler，以及与 Run 一致的 Shutdown → CloseContext 顺序。为缩短测试，把 HTTP 等待预算设为 30ms：CloseContext 返回 nil、Store Ping 报 database is closed，ActiveWaiters 仍为 1；显式取消客户端才归零。

生产 Run 的 10 秒预算会延后发生时点，不能解决取消链断开。此测试不是实际 Linux SIGTERM 进程验证。

修复：停止接纳工作时取消已接受等待请求；等待 HTTP handler 和后台操作收尾，再关闭池和 Store。Shutdown 超时需要明确收尾路径。区分清理超时与清理成功，验证真实进程退出码和资源完成状态。

### S05 — P2：创建 POST 只有部分短阶段预算，基线/提交阶段未完成（调用验证与静态确认）

位置：internal/server/verification_service.go:305、312、322、355。

幂等锁、部分查询和 lease 锁已有 5 秒 Context，但：
基线槽等待、锁/槽后二次准入、基线网络及最终原子提交仍使用原请求 Context。

真实 HTTP POST 的 Backend 基线回调已确认 ctx.Deadline 不存在。HTTP 的 15 秒请求体读取 deadline 仍存在，但它不是独立的建立基线预算或 DB 提交预算，不能据此将阶段期限验收标为完成。

修复：按方案给建立基线阶段有限预算，覆盖锁/槽等待和网络；纯 DB/提交阶段设置自己的有限预算并保留父请求取消。阶段超时正确传播，不能机械将 5 秒期限套到后续 120 秒长轮询。补资源持续占用但客户端不取消的测试。

### S06 — P2：租约查询 DB 故障变成 404，管理员路径还会吞错恢复（已复现）

位置：internal/server/verification_service.go:229—267。

读取 allocation 失败后，代码可继续另一路查询；最终把 err != nil 和 alloc == nil 合并成 ErrVReqNotFound。管理员 email 路径还使用无 Context 的 FindAliasRoute，并可清除之前的真实 DB error。

在临时库将 alias_allocations 表改名，正常凭据的真实 HTTP POST 返回 404 RESOURCE_NOT_FOUND，而非存储服务错误。

修复：区分无记录、DB 故障及取消。只有明确无记录时进入必要兼容查找；真实错误向上返回，管理员路由恢复同样使用 Context/error 接口。不能将存储异常当成权限/资源不存在。

### S07 — P2：忙状态缺 Retry-After；池满和 HME 忙错误仍漏映射（HTTP 已复现，其他为静态确认）

位置：internal/server/backend_mail.go:194、591、682；internal/server/external_v2_handlers.go:248、304；internal/server/response.go；internal/server/backend.go:1116、1150。

Backend 为 ErrUpstreamBusy 返回 SERVER_BUSY，并在 Data 放 retry_after=2，但：
- response 工具不会把 Data 变成 HTTP Header；
- v2 创建/查询 handler 直接 failCode，连 Data 都丢弃；
- 新准入拒绝路径也没有相应 Retry-After；
- ErrIMAPPoolBusy 不在这些优先忙分支中；HME 的 ErrHMEOpBusy/ErrHMEPoolBusy 也未被相应错误适配识别。

真实 HTTP POST 已复现：503 SERVER_BUSY 的 Retry-After 为空。新增 Backend 饱和测试只断言 Data 字段，不能证明 HTTP Header。

API.md 中宣称“响应头 Retry-After: 2”和“连接池满也映射 503”的描述目前不符合实现。IMAP 条目池满还可能进入原有 502/能力错误或原生 WebMail 回退；本轮没有故障注入所有回退分支。

修复：所有具体忙哨兵优先识别，统一保留明确错误与响应 Header；入口/等待者/上游各类拒绝符合既定契约。用实际 HTTP 断言 status、code、Header、Data，以及没有额外网络回退。

### S08 — P1：容量报告仍超出证据，无法证明生产稳定 2000—4000 任务（源码核对与实跑）

位置：internal/server/concurrency_scale_test.go:44、73—74、130、196、285、425、449—451；docs/remediation/API_CONCURRENCY_VALIDATION.md。

本轮确有进步：主测试启动真实 TCP listener，真实 HTTP 客户端挂起 2000 个请求；本机复跑通过。但：

1. 主收信路径是 fakeBackend 回调，没有假 IMAP 协议服务，没有经过真实 Manager、IMAP Pool、EXAMINE/UID FETCH/MIME 网络解码。另一项两账号 Backend 测试验证了保留类别接线，不等于主容量测试经过完整 IMAP。
2. 2000 个任务仍直接 SQL 插入，期限 20 分钟；生产 POST 有效期为 10 分钟。50 次单独 POST 的基线是立即返回的回调，无法证明 2000 任务创建或真实 IMAP 基线吞吐。
3. 服务字段直接设 5000/3000；测试准入配置也高于生产默认，没有经过 main/Config 装配。当前 Compose 单主体活跃任务为 500，等待者 2000 不会将它自动提高。
4. Worker 未 Start，只有手工 syncOnce，未测正常 1 秒调度、持续流量或多轮进展。
5. 全测试本轮仅 1.24 秒，证明瞬时建立与短时挂起，不是稳态持续容量测试。没有 4000 档位验证。
6. 客户端 latency 从 GET 发起计时，包含邮件可用之前的等待和连接建立；不能称为邮件到达到交付的 p99。全程耗时从 mailDeliverStart 起算，但没有每封邮件到达和客户端完成时间的分布。
7. HeapAlloc 是客户端和服务端同一测试进程的 Go 堆，不等于服务端 RSS，也不包含内核 TCP 缓冲的完整占用。没有相应 RSS/FD 峰值。
8. 客户端对网络错误最多重试五次，只统计最终错误；当前零错误计数不能证明所有建连尝试零失败。
9. 归属大规模断言仍查原先插入的 allocation，没有在主测试中断言结果引用解析、详情读取或跨主体拒绝。
10. 取消后等待 ActiveWaiters 归零，但没有 join 全部客户端 goroutine，不能把它当作全部客户端/传输资源无残留证明。
11. 优化前 1800—2500ms、p50 2200ms 没有同条件可执行基准/原始结果支撑。
12. 报告所列十个 TestRxx_... 名称不在当前测试代码中。实际前缀测试为原八项加两个启动检查。报告将 R08 解释成正文隔离、R10 解释成停机，与审查的 HME 生命周期、任务配置不对应。

修复验收：保留当前 HTTP 能力测试，但准确标注 fakeBackend 与直接插库范围；增加通过正式配置/任务 POST、真实后台周期及假 IMAP 协议服务的测试；测持续负载、混合热点/独立账号、过载与停机。记录正确时间起点、实际配置和平台资源指标。若环境不能测，明确留为未验证，不从模拟结果推导 Gmail/生产容量。

## 3. 本轮验证结果

| 检查 | 结果与边界 |
|---|---|
| go test ./... -count=1 | PASS；server 49.341s。证明当前已有测试通过，不覆盖新增失败条件。 |
| go vet ./... | PASS。 |
| git diff --check | PASS。 |
| docker compose config --quiet | PASS，仅配置解析。 |
| Compose 指定非敏感配置提取 | 全局任务 2000、单主体任务 500、全局等待者 4000、单主体等待者 2000、stop_grace_period 30s。没有运行生产容器。 |
| 原 Test 前缀 | 10 项 PASS：原 8 项加 v10 缺索引启动恢复、重复启动。 |
| 新增隔离检查 | 10 项 FAIL，分别复现上述剩余缺陷。失败是审查发现，不是修改了正式测试预期。 |
| 真实 HTTP 主容量复跑 | PASS；2000 等待者、短请求成功、取消后等待者归零。收信/任务建立范围见 S08。 |
| go test -race ... | 无法执行：Go 报 -race requires cgo；不得记为 race PASS。 |
| 真实 Gmail、实际 Linux SIGTERM、真实反代 HTTPS | 未验证。 |

主容量复跑的观测值：50 个回调基线 POST 用时 36.0093ms；2000 等待者建立 769.702ms；Go 堆 68.96MiB；goroutine 9 → 10006；即时状态 GET 1.0501ms；从标记邮件可用到收齐 100 个结果 263.2689ms；客户端从 GET 发起计算的 p99 1.0073901s。以上是该模拟 Backend 测试的本次结果，不能作为生产 Gmail 指标或长期容量。

## 4. 可复跑材料

临时目录：

C:\Users\42013\AppData\Local\Temp\icloud-hme-concurrency-rereview-936e78fd57b349439d42d4222486e3ad

其中包含：
- failure_mapping_regression_test.go、idempotency_replay_test.go、hme_pool_wait_test.go、pool_serial_wait_test.go
- overlay.json
- rereview-probe-results.txt、deadline-probe-results.txt
- existing-probe-results.txt、http-scale-results.txt
- workspace-start-hashes.json

在项目根目录复跑新增检查：

~~~powershell
go test -overlay "C:\Users\42013\AppData\Local\Temp\icloud-hme-concurrency-rereview-936e78fd57b349439d42d4222486e3ad\overlay.json" ./internal/server ./internal/store ./internal/account ./internal/mail -run '^Test' -count=1 -v
~~~

overlay 将临时测试映射成各包的虚拟测试文件，不需要复制/覆盖项目测试。后续可按这些输入条件转成正式回归。目录失效时按本文重建，不能跳过缺陷验收。

新增检查名：

~~~text
TestPOSTBaselineNeedsFiniteDeadline
TestExpiredReplayMustNotHideWriteFailure
TestAllocationDatabaseFailureIsNot404
TestUpstreamBusyCarriesHTTPRetryAfter
TestShutdownMustCancelAcceptedHTTPWaiters
TestExpiredReplayReturnsCASWinner
TestHMEDropWaitMustRespondToCancellation
TestHMECloseMustCoverQueuedBorrowers
TestHMEWaitingSameAccountMustNotSpendGlobalNetworkSlots
TestIMAPSerialWaitersMustNotSpendGlobalNetworkSlots
~~~

## 5. 建议的修复顺序

先处理 S01—S04 的状态和生命周期正确性，再补 S05—S07 的期限与错误契约，最后重做 S08 的证据与验收报告。修复范围内保留现有 v2、共享邮箱串行保证和任务安全边界，不新增平行服务或绕过配额的路径。

代码修复、容量达标、部署条件分别给出结论。不要用全部单测通过替代故障场景验证，也不要用能瞬时挂起 2000 个本机 HTTP 请求替代真实收信与持续业务容量验收。

