# API 并发优化改动审查

审查日期：2026-10-01。HEAD：`178ff25030e6525e8bf616989f3ecb2824de119d`；审查对象为当前未提交改动及新增测试、配置和验收报告。实际本机工具链为 `go1.27.0 windows/amd64`，`CGO_ENABLED=0`。

结论：实现方向可保留，但当前改动不满足部署条件。历史 v9 数据库升级无法启动，共享邮箱结果引用归属错误、日志泄露代理密码和任务幂等缺陷已复现；后台资源保留未接入生产调用。现有报告不能证明真实 2000 个 HTTP 连接或完整收信容量。

本次审查只新增本报告。业务代码及原有修改保持；临时复现测试已从项目移除，测试源码与原始结果保存在下文指定目录。

## 1. 优先修复的问题

### R01 — P1：历史 v9 数据库升级后无法启动（已复现）

位置：`internal/store/store.go:489`、`internal/store/schema.go:788`。

v9→v10 迁移增加创建幂等字段和唯一索引，却未建立新的 `idx_vreq_status_exp`。终态校验要求该索引存在。新库通过 v0 初始化已有索引，因此现有新库测试通过；真实 v9 数据库升级失败。

复现：在隔离数据库中移除 v10 新字段/索引，还原 `user_version=9`，关闭后用当前 `NewStore` 重新打开，报错：

```text
schema validation failed: index idx_vreq_status_exp is missing
```

迁移事务已经把版本写成 10，单纯重启仍无法补齐索引。修复应在 v9→v10 启动迁移事务内建立全部 v10 必需索引，并让中间版本校验只要求该版本实际拥有的索引。补充真实历史结构、重复启动及迁移失败回滚测试，不能仅把新库的版本号改低当作历史库覆盖。

### R02 — P1：共享收件箱异常日志输出代理密码（已复现）

位置：`internal/server/backend_mail.go:707`、`internal/server/backend_mail.go:720`、`internal/server/mail_sync.go:772`。

端点“指纹”实际是拼接字符串，包含完整 `proxy` URL。网络/同步失败日志输出完整指纹；代理 URL 带 `user:password@host` 时密码原文进入日志。

使用完全虚构的代理凭据和注入的 IMAP 失败，已捕获日志中的虚构明文代理密码。修复应将规范化端点配置生成不可逆摘要作为内部身份，日志只输出安全摘要或脱敏字段。不能打印摘要的输入，也不要通过关闭用户代理解决。

### R03 — P1：共享读取后的 MessageRef 仍属于代表母号（已复现）

位置：`internal/server/mail_sync.go:1004`、`internal/server/mail_sync.go:1017`、`internal/server/mail_sync.go:1036`。

代码为事件的 `AccountID` 改用真实母号，但落库的 `MatchedEventRef` 和事件 `EventID` 仍使用代表母号读取所得的 `fullMsg.MessageRef`。

复现：用 acc-a 读取共享邮箱，邮件属于 acc-b 的别名。任务成功落盘后，按 acc-b 解析返回引用，报错：

```text
account mismatch: ref belongs to acc-a, request for acc-b
```

`MailReadService.GetMessageDetail` 会把此情况映射为 403。这次复现证明的是引用归属错误，未据此声称验证码数字已经错投。

修复应在确认物理邮件身份及别名归属后，为每个目标母号生成正确的引用，落库、发布、去重和 HTTP 返回使用一致身份；不要就地修改多个别名共享的正文对象。无基线查询的共享批次仍直接使用代表母号发布，也需保持真实归属。增加引用解析和后续详情读取断言，不能只检查事件的 AccountID。

### R04 — P1：同一创建键并发请求不同 lease 返回 500（已复现）

位置：`internal/server/verification_service.go:255`、`internal/store/inventory_vreq.go:219`。

目前只有 lease 锁，没有主体+创建键锁。相同主体/键、不同 lease 可同时查不到历史任务并分别读取基线。第二次插入违反创建键唯一索引，SQLite 原始错误没有转换成预期幂等冲突，服务捕获 `ErrIdempotencyConflict` 的恢复分支无法处理它。

通过两个真实本机 HTTP 请求及基线屏障复现：返回 200 和 500；读取基线两次。预期为一个成功、一个明确 409，冲突请求不重复读取基线。

修复按方案固定主体+创建键锁→lease 锁顺序，锁内重查，并在最终事务中正确仲裁创建键、参数及配额。只识别相关创建键唯一冲突，不能把所有约束失败归为幂等冲突。

### R05 — P1：过期和终态任务重放仍返回 baseline_ready=true（已复现）

位置：`internal/server/verification_service.go:201`、`internal/server/external_v2_handlers.go:260`。

幂等命中直接返回历史行，没有收敛已经到期的 ready 状态；响应的 `baseline_ready` 固定为 true，成功、失效和过期终态也会被标为可发送。

通过真实本机 HTTP 创建任务，将隔离数据库中的任务设为已到期，再以相同键重放：返回 `status=ready`、`baseline_ready=true`。客户端据此发送验证码时，该任务已不能正常接收结果。

修复应保留原 request_id、基线和 expires_at，必要时用 CAS 收敛过期；只有有效 ready 任务返回可发送，终态返回 false，不延长期限、不覆盖已成功终态。补充 succeeded/expired/invalidated 及到期未清理行的重放测试。

### R06 — P1：后台保留槽位没有接入实际收信调用（静态确认）

位置：`internal/mail/pool.go:214`、`internal/account/client_factory.go:254`、`internal/server/backend_mail.go:553`。

`DoContextWithServerAndKind(..., true, ...)` 只存在定义；生产代码全部经 `DoContextWithServer`，固定传 false。MailSync 的边界、分页、正文读取也走这条路径。因此前台达到前台上限后，后台同样被当成前台拒绝，保留容量没有实际服务后台。单测直接调用 `acquireActiveOp(..., true)`，未覆盖调用链接线。

另外，新忙错误没有在生产 handler/backend 中进行语义识别：可能被映射成 502/能力不支持，原生收件箱查询还可能走 WebMail 回退。过载拒绝缺少对应的 Retry-After。

修复应把后台类别沿实际收信调用显式传入共享池，并将容量拒绝映射为可识别的忙状态。忙不能触发补连接或无预算的上游回退。测试必须通过生产 Backend 路径占满前台资源，验证后台仍能持续收信，而非只测池内部计数。

### R07 — P2：错误传播仍不完整（两项已复现，恢复路由静态确认）

位置：`internal/server/mail_sync.go:581`、`internal/server/verification_service.go:376`、`internal/server/mail_sync.go:1087`。

| 错误路径 | 实际行为 | 应修复为 |
|---|---|---|
| 批量基线查询失败 | 只记录 scanErr，仍按空基线走 legacy 读取；注入 SQL 查询失败后已观测到一次 legacy 网络读取 | 立即终止失败批次，保留任务/游标，禁止降级历史查询 |
| 服务内凭据复查 DB 失败 | 忽略 ValidateTokenPrincipalContext 的 error，返回 TOKEN_REVOKED/401；注入数据库读取故障已复现 | 区分凭据失效与存储失败/取消，返回可定位服务错误 |
| 冷启动路由查询失败 | FindAliasRoute/FindLeaseAccount 仍无 Context 且用 bool 表达结果；失败可被当成未知母号 | 在恢复热路径传递 Context 和真实错误，避免失败转盲探 |

本轮未故障注入证明以上基线失败已造成真实旧码交付；已证明的是错误后仍进入不应执行的历史读取。报告不能将 T4 全面错误传播标为完成。

### R08 — P2：HME 关闭与驱逐生命周期仍未闭环（关闭后借用已复现）

位置：`internal/account/hme_pool.go:135`、`internal/account/hme_pool.go:172`、`internal/account/hme_pool.go:191`。

HME 池没有 closed 状态；Close 后 acquire 仍可新建条目，已复现。Close 清空 map 后用 TryLock 跳过忙条目，后续 Close 无法重新找到被跳过的客户端。drop 在等待条目锁之前删除注册键，也未等待 pin，允许旧条目未退出时创建同键新条目。

这些关闭/drop 问题部分来自原实现，本次新增 pin 与硬容量没有完成方案要求的修复。修复需协调 closed、pin、drop、closing 和最终关闭完成状态；关闭后拒绝借用，忙条目必须最终收尾，同键旧实例退出前不允许新实例并行。补充确定性屏障测试，不以清空 map 代替资源关闭。

### R09 — P1：验收报告把 handler 模拟写成真实连接容量（静态确认）

位置：`internal/server/concurrency_scale_test.go:195`、`internal/server/concurrency_scale_test.go:259`、`docs/remediation/API_CONCURRENCY_VALIDATION.md:11`。

主测试使用 `httptest.NewRecorder()` 加 `r.ServeHTTP()`，没有真实 listener/client/socket。它证明了 2000 个模拟 handler 等待者，不证明 2000 个真实 HTTP 连接、FD、网络缓冲、反代或连接重建速率。

任务由单事务直接插入，期限设为 20 分钟，未测生产创建任务/基线的持续速率。收码直接向 EventBus 发布已准备好的 OTP；syncWorker 未启动，因此 53ms 指标未经过 Gmail/IMAP、邮件扫描、正文读取或解析。归属断言读取的是提前写入的 allocation，不足以验证聚合引用归属。1900 个剩余请求没有取消并等待退出，stopClientCh 没有消费者。

报告所称“稳定支持 2000~4000”、真实端到端收码时延、450~800ms 优化前对照等，不由现有测试证明。应更正为实际验证范围，并补真实 HTTP 与假 IMAP 协议服务的完整路径、同条件前后基准、稳态持续测试、p95/p99、RSS/FD/连接、混合过载及停机验证。真实 Gmail 单独列未验证。

### R10 — P1：2000 任务候选配置没有接通 Compose（静态确认）

位置：`internal/server/external_v2_handlers.go:27`、`docker-compose.yml:40`。

生产默认活跃任务配额仍为全局 1000、单主体 50。新 Compose 只传入在途和等待者配置，没有传入 `ICLOUD_HME_MAX_GLOBAL_ACTIVE_VREQ`、`ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ`；在宿主机项目 .env 写入这两个参数不会自动传进容器。

主测试手工设置服务字段为 5000/3000并直接插入任务，绕过生产配置路径。提高等待者数不会提高允许建立的有效任务数。

修复应在启动阶段解析并校验任务、基线及上游候选配置，实际传入服务/池，并穿过 Compose 与 .env.example。先压测得到稳定档位，再推荐配置，不直接把 2000 写成已验证生产容量。

## 2. 其他需要核对的完成范围

- T2 的 5 秒短阶段期限目前主要用于鉴权和 GET 状态读取；创建 POST 的幂等查询、lease 查询、预检、键/基线槽等待和最终提交仍直接使用原请求 Context，没有按方案分开期限。支持取消不等于具备阶段截止时间。
- 聚合 checkpoint 仍以每轮从 map 中选出的代表母号为键（`mail_sync.go:781`），没有稳定使用物理邮箱身份。代表母号变化可重复扫描同一范围；需验证删除/换绑和物理邮箱变更时的游标隔离。
- Cookie 鉴权忽略 `tok.BindPrincipal("admin")` 错误（`auth.go:232`），单主体在途超限仍放行；API Key/Token 路径则正确检查拒绝。
- 新契约未同步到 API.md；Run/main 的真实进程停机和真实代理语法/HTTPS 链路尚未验证，不能仅凭 CloseContext 单测宣布 T9 全部完成。

## 3. 本轮验证证据

| 检查 | 结果与范围 |
|---|---|
| `go test ./... -count=1` | 现有全量测试通过；临时复现测试加入前执行 |
| `go vet ./...` | 通过 |
| `git diff --check` | 通过；Git 另有 LF/CRLF 转换提示，与空白错误不同 |
| `docker compose config --quiet` | 通过；只证明配置解析，不证明候选参数完整或容器运行 |
| `go test -race ...` | 未执行成功，当前 CGO_ENABLED=0，报 `-race requires cgo`；不能标为 race 通过 |
| 隔离复现检查 | 8 项失败，均复现了预期的真实缺陷；使用临时数据库、本机 HTTP、虚构凭据/假上游，不访问生产数据或真实 Apple/Gmail |

复现检查名：

```text
TestV9Upgrade
TestAggregatedMessageRefOwnership
TestExpiredIdempotencyReplay
TestConcurrentSameKeyDifferentLease
TestBaselineQueryErrorMustNotReadLegacyMail
TestProxyCredentialsInSharedInboxLog
TestCredentialDatabaseFailureIsNotRevocation
TestHMEAcquireAfterClose
```

测试源码和原始输出目录：

```text
C:\Users\42013\AppData\Local\Temp\icloud-hme-concurrency-review-6e9eadc3a894441aa38923929c7ab9c4
```

目录内为 `server_probe_test.go`、`store_probe_test.go`、`account_probe_test.go` 和 `probe-results.txt`。为复跑，可将三个源码分别复制回对应 internal 包并命名为唯一的 `_test.go` 文件，然后在项目根目录执行：

```bash
go test ./internal/store ./internal/server ./internal/account -run '^Test' -count=1 -v
```

验收应先修复 R01–R08 的正确性和资源保护，补足配置接线，再重做 R09 的容量验证和报告。现阶段不建议将这批改动直接部署到持有历史数据库的生产服务器。
