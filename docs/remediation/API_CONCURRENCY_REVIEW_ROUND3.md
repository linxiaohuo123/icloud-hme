# API 并发与收信可靠性第三轮审查

审查时间：2026-10-01 至 2026-10-02（Asia/Shanghai）。HEAD：178ff25030e6525e8bf616989f3ecb2824de119d，分支 main。对象为 Gemini 最新未提交代码、正式回归测试、容量测试与验收报告。

结论：前两轮的 20 项缺陷回归全部通过，修复有实际进展，但“S01–S08 全部闭环”和“生产等效容量已验证”仍不成立。本轮额外隔离检查复现了停机提前关闭 Store、兼容取码连接无法取消且清理不再继续、前台 pin 占满后台池条目、email 创建路径吞数据库错误、批量邮件池满错误映射遗漏。部署建议还使用不存在的环境变量，新增配额测试将任务配额误当成等待者配额。

本轮仅新增本报告。六项审查检查使用 Go overlay 放在临时目录，没有改写业务代码或 Gemini 的正式测试。审查开始时 50 个已有修改/未跟踪文件的 SHA256 全部保持一致。未提交、推送或部署；未读取生产数据库、访问真实 Gmail/Apple、发送邮件或消耗别名额度。

## 1. S01–S08 复核状态

| 编号 | 本轮证据与结论 |
|---|---|
| S01 | 权威 CAS 返回记录及错误已接入；成功终态胜出和过期更新失败两项回归通过。原缺陷已修复。 |
| S02 | IMAP/HME 排队不再占活跃网络名额；原两项回归通过。但 IMAP 条目没有后台保留，见 T03，不能宣称所有独立账号/后台进展均有保障。 |
| S03 | drop 等待 Context 和关闭后防重建两项回归通过。新增 closing 状态检查有不同锁保护的读写，见 T07；并发竞态检查未执行。 |
| S04 | 空闲 v2 长轮询的服务取消已接入，原回归通过。HTTP POST、现有兼容取码路径和清理完成条件仍有缺陷，见 T01/T02。 |
| S05 | 基线有 10 秒、提交有 5 秒阶段预算；原“基线必须携带 deadline”检查通过。该测试没有验证整个 POST 各分支的超时行为；email handler 和管理员路由查找仍直接使用父 Context。 |
| S06 | lease_id 主路径已区分存储故障，原回归通过。email handler 在进入 Service 前仍把故障变成 404，见 T04。 |
| S07 | 响应工具已支持 Retry-After，基线/分页/列表等忙哨兵与 HME 适配有改进。批量正文读取仍漏 ErrIMAPPoolBusy，真实 HTTP 复现见 T05。 |
| S08 | 2000 TCP、客户端 join、100 个结果和跨主体 404 均实跑通过。但生产配置不等效、主 IMAP 仍为回调、Worker 没有 Start、全测试仅 1.24 秒，见 T06。未形成生产稳定容量证据。 |

## 2. 需要修复的缺陷

### T01 — P1：HTTP 请求还在执行，CloseContext 已报告成功并关闭 Store（已复现）

位置：internal/server/server.go:332、444、529、544；internal/server/verification_service.go:205、678；internal/server/middleware.go:42。

Run 在 HTTP Shutdown 超时后继续 CloseContext，没有强制关闭剩余连接或等待所有 handler。CloseContext 仅检查 ActiveWaiters，忽略 ActiveInflight 和未计入等待者的请求阶段。注入的 serverCtx 只在 v2 查询末尾 select 中被监听，没有覆盖 POST 和 HTTP 请求体读取。

两项本机真实 HTTP 复现：

1. 基线阶段仍在执行，缩短 HTTP drain 至 30ms。CloseContext 返回 nil，ActiveInflight=1、ActiveWaiters=0，Store Ping 为 `sql: database is closed`。释放基线后 POST 返回 500。
2. **保持生产默认 10 秒 drain**。发送合法 POST 的部分请求体，剩余正文仍处于已有 15 秒读取预算内。10 秒 Shutdown 到期后 CloseContext 返回 nil，ActiveInflight=1，Store 已关闭；补齐请求体后 POST 返回 500。该复现不依赖缩短生产 HTTP drain。

此外，v2 长轮询的 Waiter release 比最后的凭据交付复查更早执行；ActiveWaiters=0 本身也不等于全部数据库访问完成。

修复要求：把已接受请求的整个 handler 生命周期纳入停机协调；停止新业务、使请求与服务取消联动、处理 HTTP drain 超时后的连接收尾，再等待请求及后台任务完成，最后关闭池和 Store。不能仅增加等待者轮询时长或机械扩大 10 秒预算。

验收：保留两个复现条件；在途 POST/请求体/v2 交付阶段不会在 Store 关闭后继续使用它。正常停机只在清理完成时报告成功。

### T02 — P1：兼容取码连接收不到停机取消；清理超时后永久停止（已复现）

位置：internal/server/verify_handler.go:124；internal/server/server.go:453、529–541；相关路由注册见 internal/server/server.go:626。

`verifyCodeHandler` 只监听邮件、计时器和客户端 Context，没有监听服务生命周期。同一 handler 仍供 `/api/verify-code`、`/api/external/v1/verify-code` 和 mail/code 路由使用。这些路径已接入 RequestLimiter，所以旧接口未退出会使新的停机检查一直看到等待者。

CloseContext 内部等待 5 秒后设置 closeErr 并直接 return；defer 仍关闭 closeDone。closeOnce 随后使重复 Close 只读这个错误，永远不会继续关闭 Backend/Store。日志所谓“推迟底层存储关闭”实际没有后续清理流程。

真实 HTTP 复现：

- 兼容接口持有一个 120 秒等待者；服务取消后 5 秒仍为 ActiveWaiters=1。
- 第一次 CloseContext 返回“尚有活跃长轮询连接”，Store Ping 成功，说明存储未关闭。
- 显式取消客户端后 ActiveWaiters=0。
- 再次 CloseContext 仍返回同一错误，Store Ping 仍成功，清理没有继续。

修复要求：覆盖当前所有真实取码路由的取消；清理完成 channel 只能代表真实清理完成。调用方等待超时与后台清理生命周期分离，不能在一次超时分支结束唯一清理流程。Run/main 的进程退出预算也必须与此协调，不能承诺进程退出后协程还能继续。

验收：现有路由统一取消；首次等待超时后请求退出，清理仍会完成；重复 Close 正确等待并返回最终结果。Linux SIGTERM/容器 stop 仍需单独验收。

### T03 — P1：只保留活跃操作槽，前台 pin 仍能占满全部 IMAP 条目（已复现）

位置：internal/mail/pool.go:51、250、438–475；方案第 6 节 T3、第 8 节 T6，以及第 10 节争用测试要求。

`reservedForSync` 只在 acquireActiveOp 中限制前台操作数量，getOrCreateWithServerAndPin 不接收类别，也没有前台占用不同条目的边界。S02 顺序调整后，等账号串行锁的前台请求会先 pin 条目；全部条目被 pin 时，后台即使还有保留的操作槽也取不到新条目。

隔离复现：池硬容量 3、活跃上限 3、后台保留 1；三个不同邮箱的前台请求正在等待各自串行锁。此时 entries=3、active=0、foregroundActive=0，独立后台邮箱仍收到 ErrIMAPPoolBusy，无法进入网络操作准入。测试仅使用本机端口与取消 Context。

生产默认池容量 50、同步保留 5 时，方案要求前台不同被借用条目最多占 45，不能只限制前台活跃操作为 27。闲置条目可以安全驱逐；同条目多个引用不应重复计数。用户当前若所有母号只使用同一物理 Gmail，则这个“多个不同池键占满”的复现并不证明该部署已经出现饱和，但混合邮箱场景的保护没有实现。

修复要求：在现有条目引用和准入临界区落实后台可借用条目容量，保持单物理邮箱串行和池总硬上限。不要通过额外连接或扩容掩盖保留规则缺失。

### T04 — P2：email 形式的创建 POST 仍把数据库故障返回为 404（已复现）

位置：internal/server/external_v2_handlers.go:218–230。

Service 的 lease_id 查询已修复，但 handler 的 email → allocation_id 查询仍把所有错误合并为“未找到或无权”。普通 Token 在进入修复后的 Service 前就得到 404；现有正式回归只发送 lease_id，没有覆盖此分支。该 handler 查询也没有独立短阶段预算。

复现：临时库将 alias_allocations 表改名，发送正常 Token 的 `POST {"email":"target_alias@icloud.com"}`，实际得到 404 RESOURCE_NOT_FOUND，而非 500 INTERNAL_ERROR。

修复要求：区分明确无记录、数据库错误、取消和超时；email 与 lease_id 路径共用正确的查询/错误处理，并保留规范化 lease 与幂等指纹契约。管理员回退也应继承有限查询预算。

### T05 — P2：批量邮件正文读取遗漏池满忙映射（已通过真实 HTTP 复现）

位置：internal/server/backend_mail.go:517–529；internal/server/mail_read_service.go:716；internal/server/mail_handlers.go:169。

GetMessagesContext 只识别 ErrUpstreamBusy，没有识别 ErrIMAPPoolBusy。条目池满时被转换成 502 UPSTREAM_FAILURE，响应没有 Retry-After。MailSync 的聚合正文读取也使用这一 Backend 方法。

复现通过真实 Manager、IMAP Pool 和 `/api/messages` HTTP handler：一个本机 net.Pipe IMAP 客户端正在借用唯一条目，读取另一账号邮件得到 **502 UPSTREAM_FAILURE，Retry-After 为空**。在进入网络前即被池容量拒绝，没有连接真实 Apple。

修复要求：将具体池满错误与操作忙错误统一优先识别，返回 503 SERVER_BUSY 和 Retry-After: 2；增加批量路径的 HTTP 回归，不仅断言 Backend 的 Data。

### T06 — P1：部署参数写错、配额测试不等效、S08 容量结论超过证据（配置核对及实跑）

位置：docs/remediation/API_CONCURRENCY_VALIDATION.md:19、105、141；internal/server/concurrency_scale_test.go:74、151、209、301、322、471、605、671。

#### 6.1 不存在的环境变量不会提高配额

报告推荐的两个名称没有被 main.go 解析，也没有透传进 Compose：

```text
ICLOUD_HME_VERIFY_MAX_ACTIVE_REQUESTS_PER_PRINCIPAL
ICLOUD_HME_VERIFY_MAX_ACTIVE_REQUESTS_GLOBAL
```

实际任务配额参数是：

```text
ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ
ICLOUD_HME_MAX_GLOBAL_ACTIVE_VREQ
```

修改报告里的错误名称后重建，默认单主体 500 任务限制仍不变。不要新增别名配置来迁就错误文档，直接纠正文档和测试。

本机当前 Compose 解析结果（只提取非敏感参数）：

| 类型 | 全局 | 单主体 | 参数 |
|---|---:|---:|---|
| 活跃取码任务 | 2000 | 500 | MAX_GLOBAL_ACTIVE_VREQ / MAX_PER_TOKEN_ACTIVE_VREQ |
| 长轮询等待者 | 4000 | 2000 | MAX_WAITERS_GLOBAL / MAX_WAITERS_PRINCIPAL |
| 短阶段在途请求 | 128 | 64 | MAX_INFLIGHT_GLOBAL / MAX_INFLIGHT_PRINCIPAL |

等待者单键上限为 8，stop_grace_period 为 30s。这些数值是配置边界，不是生产容量证据。

若单 Token 确需 2000 活跃任务，正确名称的候选配置例如：

```dotenv
ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ=2000
ICLOUD_HME_MAX_GLOBAL_ACTIVE_VREQ=4000
```

该示例只解释正确配置名，不代表 4000 档位通过，也不是本轮部署建议；实际全局值按主体数量和实测容量确定。

#### 6.2 “生产等效”测试测的是手工改小的等待者配额

新测试把等待者设成全局 2000、单主体 500，将 inflight 设成 256/128，并直接插入单主体 501 个任务。这与生产的等待者 4000/2000、inflight 128/64 不符，也绕过了任务创建上限。

第 501 个 GET 被拒绝，证明的是该测试手工设置的 waiter limit，不证明生产“第 501 个长轮询被拒绝”。生产 500 限制控制创建仍活跃的任务，等待者限制控制挂起的连接，必须分别验收。还需要通过正式 Config/Server 装配和 POST 创建验证任务配额，不能以字段赋值和 SQL 预置称为生产装配。

#### 6.3 主测试有效，但仍是短时模拟 Backend 能力测试

已验证：本机真实 TCP 2000 挂起、一次短 GET、100 个验证码匹配、100 个跨 Token 查询返回 404、取消后 Waiters 归零及 2000 个客户端 goroutine join。

未覆盖/指标限制：

- 主收信仍是 fakeBackend，没有假 IMAP 协议服务，没有经过真实 Manager、Pool、EXAMINE/UID FETCH、网络 MIME 解码。50 个 POST 的基线立即回调返回，不能称为真实 IMAP 基线吞吐。
- 2000 任务仍直接 SQL 插入，主测试配额高于生产；没有验证通过真实 POST 建立 2000 任务。
- Worker 没有 Start，仅手动执行一次 syncOnce，未验证生产默认 1 秒周期、多轮持续流量和取消重连。
- 本轮实跑全测试仅 **1.24 秒**，不足以称为稳定容量；没有 60 秒及以上稳态，也没有 4000 档位。
- deliveredAt 在 client.Do 返回后、JSON 解码前取值，衡量的是响应头收到时间。应在成功读取并验证响应体之后记录“收到验证码”的完成时间。
- 建连失败最多重试五次，错误计数只记最终失败；零最终错误不等于每一次建连尝试都零失败。
- Go 堆包含同进程客户端和服务端，不等于独立服务 RSS/FD；客户端 join 不证明所有服务/传输协程均无残留。
- 跨母号断言仍主要验证验证码与 allocation；未在主容量链路验证规范 MessageRef、真实母号详情读取及相应权限。
- 优化前 1800–2500ms 仍是估算，不能和模拟回调实测值构成同条件性能提升结论。EXAMINE/FETCH 次数需要协议级计数。

修复验收：保留这两项有用测试并如实命名；另补正式配置、任务 POST、假 IMAP 协议、真实 Worker 周期、混合/过载/停机场景及持续档位测试。环境不支持的项保留未验证，不把模拟结果补写成生产通过。

### T07 — P2：HME closing 标志由不同锁保护（静态确认，race 未执行）

位置：internal/account/hme_pool.go:227、267、271、447。

drop/Close 在 p.mu 下写 e.closing；WithHMEClientContextSession 在 entry.mu 下读 entry.closing。虽然 isClosed() 曾短暂取得 p.mu，但读 closing 发生在它释放之后；drop 可能在该读与后续 acquireActiveOp 之间更新标志，读写没有共同锁或完整 happens-before 关系。

修复要求：closed/closing 的检查与写入由同一池状态锁协调，或使用适当的原子状态；不增加第二份关闭状态。维持条目串行锁及既定锁顺序。原来的两个功能测试通过不能证明竞态不存在。

本轮 CGO_ENABLED=0，且 gcc/clang 不可用，`go test -race` 仍报 `-race requires cgo`。本项是代码锁关系确认，没有声称已通过 race 工具复现；修复后在支持环境运行 drop/Close/排队借用混合回归及 race。

## 3. 运维报告还需要纠正的事实

1. **Gmail 的 15 限制不是已证实的单 IP 配额**。本轮读取 [Google 官方帮助](https://support.google.com/mail/answer/7126229?hl=en)，其中“Too many simultaneous connections”章节写的是：`up to 15 email clients at a time per account`。应按邮箱账号的同时客户端连接描述，不能改写为单 IP 15 TCP 或据此承诺不会封 IP。其他网络限制及真实投递仍未验证。
2. **默认等待时间不是统一 60 秒**。v2 GET 不传 timeout 时当前为 0，即即时读取；verifyCodeHandler 当前默认 30 秒。测试显式 timeout=60 不会改变接口默认值。
3. 验收报告要求反代 150s，而当前 deploy/Caddyfile 和 deploy/nginx.conf 使用 130s，须明确实际部署值及完整请求阶段预算。Compose 解析通过不验证 Nginx/Caddy 语法、运行或 HTTPS 长轮询。
4. nofile=65535、somaxconn=4096 和扩大本地端口范围没有实际服务器 FD/队列/端口压力证据。可作为容量规划候选，不应列为所有 2000 连接部署的无条件硬门槛，尤其不要照报告直接修改内核参数。

## 4. 本轮执行证据

| 验证 | 结果与边界 |
|---|---|
| go test ./... -count=1 | PASS；server 52.534s，包含根包、internal 包与 scripts。 |
| Test / Test | 共 20 项 PASS。 |
| go vet ./... | PASS。 |
| git diff --check | PASS。 |
| docker compose config --quiet | PASS；仅编排解析。 |
| TestConcurrencyScale_2000WaitersSharedInbox | PASS，1.24s；范围见 T06。 |
| TestConcurrencyScale_ProductionEquivalence_PrincipalQuota | PASS，0.43s；真实测量手工设置的等待者限制，非生产等效。 |
| 六项新增隔离检查 | 分别 FAIL，复现 T01–T05；不改正式测试断言。 |
| go test -race ./internal/account -run '^Test' -count=1 | 无法执行：-race requires cgo。 |
| Google 官方帮助 | 已只读核对，15 个客户端按 account 表述。 |
| 真实 Gmail/Apple、Linux SIGTERM、生产反代、服务器资源 | 未验证。 |

主容量本轮观测：50 个模拟基线 POST 35.4397ms；2000 等待者建立 752.6803ms；测试进程 Go 堆 74.75MiB；一次短 GET 1.36ms；100 个结果从标记邮件可用到收齐耗时 273.352ms；按响应头时间统计分发 p99=273.352ms；客户端完整请求耗时 p99=1.0013377s。不能写成真实 Gmail 时延或长期生产容量。

## 5. 隔离材料与复跑

临时目录：

```text
C:\Users\42013\AppData\Local\Temp\icloud-hme-round3-1a10e8d8ca464a8b8a1fbbc3db3d7a7d
```

包含 shutdown_busy_regression_test.go、pool_entry_reserve_test.go、overlay.json、workspace-start-hashes.json，以及 regression-results.txt、full-suite-results.txt、scale-results.txt、round3-probe-results.txt、round3-batch-results.txt、round3-default-drain-results.txt。

round3-probe-results.txt 的首次批量检查曾因虚构账号邮箱格式不符合 Manager 规则得到 400，此结果不是 T05 的证据。已修正临时测试的虚构邮箱格式并单独重跑；T05 的有效 502 证据在 round3-batch-results.txt。未修改业务规则或正式测试预期。

```powershell
go test -overlay 'C:\Users\42013\AppData\Local\Temp\icloud-hme-round3-1a10e8d8ca464a8b8a1fbbc3db3d7a7d\overlay.json' ./internal/server ./internal/mail -run '^Test' -count=1 -v -timeout 45s
```

检查名与有效结果文件：

| 检查 | 证据 |
|---|---|
| TestEmailAllocationDBFailureIsNot404 | round3-probe-results.txt |
| TestShutdownMustDrainHTTPCreateBeforeClosingStore | round3-probe-results.txt |
| TestDefaultDrainMustCoverSlowHTTPBody | round3-default-drain-results.txt；保持 10 秒默认 drain |
| TestLegacyWaiterShutdownAndCleanupMustComplete | round3-probe-results.txt |
| TestIMAPPoolBusyBatchHTTPMustReturn503 | round3-batch-results.txt |
| TestForegroundPinsMustKeepBackgroundEntryCapacity | round3-probe-results.txt |

临时目录中的 overlay 文件名与正式 ReReview 文件不同，不会重复定义已有测试。后续实现可将这些触发条件转为正式回归，保留真实业务断言；不能只复制现有 PASS 检查后宣布新分支闭环。

## 6. 建议实施顺序

先修 T01/T02 请求与存储生命周期，再落实 T03 后台条目保留及 T07 状态锁；补 T04/T05 的两条遗漏路径。最后纠正文档、真实配置装配和 T06 的验收口径，按目标档位补持续测试。

保留现有 Go/Gin/SQLite、v2 与实际兼容调用，不创建新旧两套 API，不新增外部服务。正确性修复与容量声明分别验收：完成已复现缺陷并通过验证后，可以再评估小流量部署；2000 生产任务的稳定声明需要对应负载证据。本轮代码、提交和服务器状态均未由本助手改变。
