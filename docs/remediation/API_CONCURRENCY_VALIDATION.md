# API 并发与收信可靠性修复验收

日期：2026-10-02。涵盖第四轮审查 U01–U04，以及后续共享收件箱热路径、取码索引、代理首包和部署配置优化。验证环境为 Windows amd64 / Go 1.27.0 / CGO_ENABLED=0，以及 WSL Ubuntu Linux amd64 / Go 1.26.0 / GCC 15.2.0 / CGO_ENABLED=1；均使用当前项目 modernc SQLite WAL 与 go-imap v1.2.1。

**结论：本轮确定缺陷已修复，Linux 完整全库 `-race` 和 Windows 全库 `-short` 均通过。正式服务装配在本机协议上游下完成 4×500 和单 Token 2000 两个至少 60 秒的混合负载场景；单 Token 场景额外完成 2000 封集中收码及配额恢复。Linux 子进程真实 SIGTERM 验证通过。真实 Gmail、Apple、生产 Compose/反向代理运行和独立服务资源容量仍待验收。当前修改只在本地工作区，未提交、推送或部署。**

历史审查报告保留原失败记录。本报告以最后一次全库日志为准，区分模拟测试、本机协议与信号测试、生产验证，不沿用此前 Windows 非 short 测试的计数或时延。

## 1. 修复与证据

| 事项 | 最终实现 | 回归证据 |
|---|---|---|
| U01：SQLite 读取快照升级失败 | CompleteMatchingVerificationRequests 的事务第一条语句改为带全部匹配条件的 UPDATE … RETURNING。保留状态、期限、别名、provider、mailbox、UIDVALIDITY、baseline UID 判定；读完并关闭结果集，提交成功后才返回可发布的权威结果。不靠扩大 busy_timeout 或无条件重试掩盖错误。 | TestCompletionSurvivesConcurrentResultWriter 调度真实 SQLite 及另一正常生产 CAS，两个任务均完成。针对性命令连续 3 次通过；完整协议收码也通过。 |
| U02：IMAP 期限被库清空 | Client.SetDeadline 同步设置 go-imap Timeout，Login 也设置命令预算。直接 Disconnect 最多等待 2 秒 LOGOUT，失败强制终止。池关闭在账号操作退出后物理断开闲置连接并 join 回收协程，不逐连接等待 LOGOUT。 | 命令黑洞、LOGOUT 黑洞、并发 Pool.Close 和回收协程退出回归通过。 |
| U02：停机与慢正文 | Run 收到信号或监听失败后先停止准入、广播取消，HTTP Shutdown 与资源清理共享 10 秒总体预算。首次等待超时后清理继续，后续调用等待同一 closeDone。main 移除重复关闭 defer。正文取消通过到期读取期限打断 Read；取消后保留到期期限，防止 net/http 排空正文再次挂起。 | TestShutdownInterruptsBlockedRequestBody 使用真实 TCP 慢 POST，在预算内完整收尾；原关闭重试、兼容长轮询、创建停机回归通过。协议容量测试停掉全部 2000 个 120 秒长轮询并 join 客户端。 |
| U04：UUID 数字误判认证 | 优先认证哨兵；传输错误中的 URL 不参加认证文本判断。历史 WebMail/密码登录文本只识别明确 HTTP 状态字段和明确会话失效语义。移除任意 401/403 和 auth complete 子串判断。 | UUID、代理端口、request_id 含 401/403/421 的网络故障保持 502；明确认证状态与哨兵保持 401。取消、期限、身份错误和访问拒绝原契约通过。批量预检连续 3 次及全库通过。 |
| 管理员路由与取消遗漏 | 管理员路由查询采用独立 5 秒预算；幂等查询、准入和提交传播取消/超时。email 查询区分取消与超时，POST/GET 不再把原始 context 错误兜底成 500。 | 取消创建返回 499；原 DB 故障、基线预算和停机回归通过。 |
| 后台保留容量弱测试 | 改用可应答的本机 IMAP；断言回调执行、SELECT 成功、返回 7 封及操作名额归零。虚构账号测试不再拨号公网。 | 原 IMAPSerialWaiters 与 ForegroundPins 两项正式回归通过。 |
| U03：容量装配与统计 | 保留并明确命名模拟测试；正式 Config/Server/Manager、实际 HTTP/IMAP、全部任务经 POST、Worker.Start 真实周期及两种 60 秒混合负载。分批建立等待连接并等待实际准入，不提高 128/64 短阶段配额；无应用层隐藏重试，分别记录拨号、请求、解码、状态和预期取消。 | 见第 2 节和最后一次 Linux 全库 race 日志。 |
| 到期完成吞数据库错误 | CompleteVerificationRequestResult 直接返回 ExpireVerificationRequest 的权威 CAS 记录与错误，不忽略过期写入失败，也不重复读出可能变化的结果。 | TestCompletion_ExpiredCASFailureIsReturned 通过真实 SQLite 触发器复现错误并验证恢复；TestCompletion_ExpiredCASReturnsTerminalWinner 保留成功胜出者的验证码、链接与事件引用。 |
| HTTP CONNECT 丢失隧道首包 | 握手后继续通过原 bufio.Reader 读取，保留 HTTP 响应解析时已预读的 IMAP greeting；连接的写入、关闭和期限仍使用原 net.Conn。 | TestDialHTTPConnectPreservesBufferedTunnelBytes 使用同一 TCP 写入合并 HTTP 响应头与 IMAP greeting，并验证反向 PING。旧实现确定性读超时，修复后及最终全库通过。 |
| 共享邮箱重复解析收件人 | 每封邮件只解析一次结构化收件人，用活跃别名集合查找；覆盖兼容分发、增量元数据筛选和正文分发，避免对每个别名反复解析同一封邮件。 | TestMailSync_SharedInboxMultipleStructuralRecipients 验证多个收件人、大小写、重复地址、正文提及隔离、真实母号 MessageRef 与原始消息不被改写。 |
| 取码热查询缺索引、批量基线未归一化 | 数据库升级为 v11：归一化别名/状态/期限索引，以及主体/状态/期限覆盖索引。批量基线查询与完成路径统一使用 LOWER(TRIM(alias_email))。 | TestVerificationRequestHotQueryPlans 核验真实 SQLite 查询计划；TestVerificationBatchBaselinePreservesNormalizedAliases 覆盖历史混合大小写/空白；TestV10VerificationIndexUpgrade 覆盖事务升级、注入失败回滚、重启数据与迁移前备份。 |
| Linux 停机缺真实信号证据 | Linux 构建标签测试在独立子进程调用生产 Server.Run，空账号库启动全部后台引擎，发送真实 SIGTERM。 | TestLinuxSIGTERM_RunDrainsWaiterAndPreservesStore 验证已接纳 HTTP 等待取消、进程正常退出、资源归零、Store 关闭与重开后的任务状态保留。 |

IMAP 库 Timeout 是单命令预算；父 Context 的取消/期限仍通过关闭物理连接约束整个阶段，不能把命令 Timeout 说成跨所有命令不变的绝对期限。

v10→v11 索引创建、替换和版本写入在同一事务内，失败回滚；启动前使用现有一致性快照机制。迁移不修改取码记录字段或 API 数据格式。

共享邮箱分发的一次 Windows 诊断样本（40 封邮件 / 2000 活跃别名，`BenchmarkMailSyncSharedInboxRecipients -benchtime=1x`）：

| 指标 | 修改前 | 修改后 |
|---|---:|---:|
| 每轮分发时间 | 31.7365ms | 2.1384ms |
| 分配字节 | 10,687,080 | 419,448 |
| 分配次数 | 562,648 | 1,613 |

这是单次本地兼容分发路径诊断，没有重复统计置信区间，也不包含公网、生产数据库吞吐或全部后台负载，不能作为生产性能承诺。

## 2. 正式协议容量测试

测试：`TestConcurrencyScale_ProductionProtocol2000Tasks60Seconds` 和 `TestConcurrencyScale_SinglePrincipal2000TasksBurst`，源码 `internal/server/concurrency_protocol_test.go`。

### 装配与负载

- 正式 server.New、真实 Manager/backend、完整路由/鉴权/生命周期中间件、真实 SQLite WAL 与 MailSyncWorker.Start。
- 10 个虚构母号共享一个本机 IMAP 收件箱；基线 SELECT/EXAMINE、UID SEARCH、元数据/正文 FETCH、持久化与 HTTP 交付均走实际实现。fixture 仅替代上游邮箱及协议服务器。
- 初始 2000 个任务全部经 POST 创建。默认配置场景为 4 个 Token 各 500；单 Token 场景为同一 Token 2000。SQL 只准备母号、租约分配和路由前置数据，不插入 verification_requests。
- 同一批 100 别名两轮收到新验证码，完成后经 POST 重建下一代任务，恢复 2000 等待者。混合阶段累计成功创建 2200 个任务，恢复后的活跃任务为 2000。
- 两个场景都至少运行 60 秒混合即时 GET 与前台 IMAP 边界查询；后台周期收码继续进展。另有 20 个客户端取消并复用原任务重连；要求至少 100 次短请求和 25 次前台 IMAP，不用短窗口代替完整场景。
- 默认配置场景第 501 个主体任务返回 429 TOO_MANY_REQUESTS，独立主体随后正常创建；第 2001 个全局任务返回 503 SERVER_BUSY。单 Token 场景验证第 2001 个主体任务、等待者被拒绝。同任务第 9 个等待者返回 429 VERIFY_WAITER_LIMIT。均核验 Retry-After: 2；跨主体读取返回 404。
- 单 Token 场景额外同时提供 2000 封邮件，全部 HTTP 结果核验验证码和真实母号；任务和等待者归零后，再用真实 POST 新建任务并建立一个等待者，证明配额恢复。
- 默认配置场景最后保留 2000 个等待者，单 Token 场景最后保留上述一个新等待者，调用生产共用 shutdownHTTP；HTTP、worker、池和 Store 真实收尾、客户端 goroutine join 后才报告成功。

正常稳态快照中等待者均为 2000。邮件完成/补建和主动取消/重连期间按计划短暂下降，随后检查恢复；不宣称所有瞬间都恰有 2000 个连接。

### 配置

4×500 场景的显式 Config 与当前 main/Compose 默认一致；单 Token 场景只调整活跃任务配额。两者均保留相同的短阶段和等待者配额。

| 配置名 | 默认 / 4×500 | 单 Token 2000 | 含义 |
|---|---:|---:|---|
| ICLOUD_HME_MAX_INFLIGHT_GLOBAL | 128 | 128 | 全局短阶段 |
| ICLOUD_HME_MAX_INFLIGHT_PRINCIPAL | 64 | 64 | 单主体短阶段 |
| ICLOUD_HME_MAX_GLOBAL_ACTIVE_VREQ | 2000 | 4000 | 全局活跃任务 |
| ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ | 500 | 2000 | 单 Token 活跃任务 |
| ICLOUD_HME_MAX_WAITERS_GLOBAL | 4000 | 4000 | 全局等待连接 |
| ICLOUD_HME_MAX_WAITERS_PRINCIPAL | 2000 | 2000 | 单主体等待连接 |
| ICLOUD_HME_MAX_WAITERS_PER_KEY | 8 | 8 | 同任务/邮箱等待键 |

### 最新全库实测

数字来自最后一次 Linux `CGO_ENABLED=1 go test -race ./... -count=1 -timeout 600s -json` 的 `race-final.jsonl`，没有用较快的独立运行替代全库记录。race 检测会增加开销，以下数字不能当作生产吞吐或公网时延。

| 指标 | 4×500 | 单 Token 2000 |
|---|---:|---:|
| 初始创建 2000 任务 | 42.992007866s | 50.377395664s |
| 混合负载窗口 | 60.123308252s | 60.012916623s |
| 满 2000 等待者的稳态快照 | 126 次 | 106 次 |
| 混合即时 GET / 前台 IMAP | 126 / 31 | 106 / 26 |
| 两轮交付 / 取消重连 | 200 / 20 | 200 / 20 |
| 两轮交付观察时延 p50 | 1.385526079s | 984.038472ms |
| 同口径 p99 / max | 1.646683142s / 1.652167984s | 1.327716512s / 1.333030655s |
| 两轮阶段 IMAP Status / SEARCH / FETCH | 2325 / 4 / 8 | 2323 / 4 / 8 |
| 额外 2000 封集中收码 p50 / p99 / max | 本场景不执行 | 7.080226694s / 13.204464717s / 13.296760632s |
| HTTP 调用 / 实际 TCP 拨号 | 4557 / 2028 | 4539 / 2028 |
| 拨号失败 / HTTP 失败 / 解码失败 / 非预期状态 | 0 / 0 / 0 / 0 | 0 / 0 / 0 / 0 |
| 主动取消（7 个重复等待者 + 20 个重连） | 27 | 27 |
| 停机退出响应 | 2000 个 503，0 个 499 | 1 个 503，0 个 499 |
| 停机后等待者 / 短阶段 / IMAP 连接与操作 | 全部 0 | 全部 0 |
| 客户端 join / Store 已关闭 | 已验证 | 已验证 |
| 测试耗时 | 110.62s | 131.04s |

明确停机响应和预期配额拒绝单独核验，不计为非预期失败；测试另允许服务取消级联产生的明确 499 REQUEST_CANCELED 并分别计数。无关状态或失败仍使验收失败。

时延起点是整批邮件在 fixture 互斥保护下可读，终点是测试逐项观察到 HTTP 结果，并完成解码结果的验证码比对及 MessageRef 母号校验；包含轮询、分发和按结果顺序检查的等待，不含公网投递，也不是每个 HTTP 响应实际到达时间或所有业务端点的成功请求 p99。Status 包含 2200 次任务基线和混合前台操作，不能写成单纯后台 EXAMINE 次数。没有测独立服务 RSS/FD，不把同进程 Go 堆写成服务器内存容量。

### 协议边界

注入已连接的本机明文 TCP IMAP 客户端；Manager、Pool、命令、扫描、存储和分发真实执行。所有端点为 127.0.0.1，意外重连也不会访问公网。**未验证生产 TLS、DNS、Gmail 认证、公网限制或邮件投递。**

只启动收信 Worker；未启动通知、自动补货和 Cookie 监控等无关引擎，不能冒充全部生产后台负载同时运行的容量证明。

### 独立的 Linux SIGTERM 回归

`TestLinuxSIGTERM_RunDrainsWaiterAndPreservesStore` 在独立测试子进程启动正式 `Server.Run`，空账号库保证不连接真实 Apple/Gmail。管理员 stats 确认一个真实 HTTP 长轮询已被接纳后，发送操作系统 SIGTERM；验证明确取消响应、进程正常退出、等待者/短阶段/池归零、Store 关闭。重开临时数据库后，未完成任务仍为 ready，已完成任务的验证码和事件引用完整保留。

最终全库中本测试通过；从发送信号至收尾并完成重开校验为 1.068259656s（整个父测试 1.46s）。`TestLinuxSIGTERM_HelperProcess` 在父进程中跳过是预期行为，由父测试通过子进程真正执行。此处验证一个等待者的真实信号；2000 等待者通过容量测试的共用 shutdownHTTP 路径验证，不能混称为“SIGTERM 2000 连接已实测”。仍未验证 Docker Engine/Compose 的生产 stop 与重启。

## 3. 模拟测试的覆盖

- TestConcurrencyScale_Simulated2000WaitersSharedInbox：真实 HTTP/SQLite、fakeBackend 回调、2000 SQL 任务、两次手工 syncOnce。50 个 POST 基准任务已清理，不再变成全局 2050 / 首主体 550。应用层重试已移除；HTTP 错误不再称为 DialErrors。时延为解码并确认终态，验证码随后统一校验。
- TestConcurrencyScale_SimulatedTaskQuotaBoundary：490 SQL + 10 POST 到达主体 500，验证拒绝、主体隔离、终态释放和第 9 个等待者。不能写成 500 个全部经 POST 或完整服务装配。
- 建立连接按小批次等待实际准入，失败路径也 join 客户端；不把瞬间建立速率超过 128/64 短阶段配额导致的预期拒绝当作挂起容量不足。
- 最终 Linux race 和 Windows short 全库均通过；完整装配与持续性由第 2 节独立验证。

历史失败和开发过程日志均保留，没有删除有效断言或重试筛选成功样本。

## 4. 验证命令

| 检查 | 结果 |
|---|---|
| Linux：CGO_ENABLED=1 go test -race ./... -count=1 -timeout 600s -json | 退出码 0；13 个有测试包、765 个顶层测试通过；无 fail / DATA RACE / panic，stderr 为空。server 包 404.873s，含两个正式 60 秒容量场景与真实 SIGTERM。仅子进程 helper 在父进程中按预期跳过。 |
| Windows：go test ./... -short -count=1 -timeout 180s -json | 退出码 0；13 个有测试包、760 个顶层测试通过；stderr 为空。short 跳过两个正式容量场景及 TestScaleCookieRoundCost、TestScaleMailSyncFanout，不冒充 Windows 完整非 short 验证。 |
| go vet ./...（Windows 与 Linux） | 两个平台均退出码 0，Linux 构建标签测试也纳入检查。 |
| git diff --check | 通过 |
| docker compose config --quiet | 通过；仅证明编排可解析 |
| Caddy v2.10.2 adapt --validate | 通过；适配后验证码 response_header_timeout 与 read_timeout 均为 150000000000ns。未启动实际代理或验证生产 TLS。 |
| Linux 测试副本与工作区 SHA256 对照 | 233 个 Go 源文件及 go.mod/go.sum，共 235 个条目完全一致；无缺失、差异或多余 Go 文件。 |

复现命令（Linux race 使用 GCC 和 CGO；源码副本位于 Linux ext4，测试只使用临时库与回环上游）：

```powershell
go test ./... -short -count=1 -timeout 180s -json
go test ./internal/store ./internal/mail ./internal/server -run 'TestCompletion_ExpiredCAS|TestVerificationRequestHotQueryPlans|TestVerificationBatchBaseline|TestV10VerificationIndexUpgrade|TestDialHTTPConnect|TestMailSync_SharedInboxMultipleStructuralRecipients' -count=3 -timeout 90s
```

```bash
CGO_ENABLED=1 go test -race ./... -count=1 -timeout 600s -json
CGO_ENABLED=1 go test -race ./internal/server -run '^TestConcurrencyScale_(ProductionProtocol2000Tasks60Seconds|SinglePrincipal2000TasksBurst)$' -count=1 -v -timeout 360s
CGO_ENABLED=1 go test -race ./internal/server -run '^TestLinuxSIGTERM_RunDrainsWaiterAndPreservesStore$' -count=1 -v -timeout 60s
go vet ./...
docker compose config --quiet
caddy adapt --config deploy/Caddyfile --adapter caddyfile --validate
```

最后一次验证日志目录：`C:/Users/42013/AppData/Local/Temp/icloud-hme-followup-a6a7579fb3cb418f956a511781ccc79a`。Linux 源码与工具目录：`/home/linxiaohuo/.cache/icloud-hme-followup-a6a7579f`。

最终证据：`race-final.jsonl` / `race-final.stderr`、`windows-final-short.jsonl` / stderr、`linux-source-comparison.json`、`caddy-final.json`。分发诊断为 `recipients-before.txt` / `recipients-after.txt`；`start-hashes.json`、`start-status.txt`、`start-changes.patch` 用于保护既有未提交工作。13 个测试包和顶层测试计数来自最终 JSON 事件，不含子测试重复计数。

`race-short.jsonl`、`race-full.jsonl`、`race-capacity-indexed.jsonl` 等保留早期失败诊断：包括 CONNECT 首包、模拟连接建立速率，以及旧 15 秒单主体窗口在 race 下无法完成工作量。没有删除正确断言或筛掉失败；最终两个协议场景统一使用 60 秒窗口，最终结论只采用 `race-final.jsonl`。第四轮旧日志另保留在 `C:/Users/42013/AppData/Local/Temp/icloud-hme-repair-round4-ca6d67711eb94fad925eeec7185d2b2e`。

## 5. 已验证、推断与未知

**已验证：**本地修复、正式装配、两个 2000 任务场景、单 Token 2000 封集中收码与配额恢复、多代基线、主体/母号隔离、混合负载、取消重连、Linux 真实信号收尾；v11 迁移/失败回滚/重启数据；完整 Linux race、Windows short、双平台 vet、差异检查、Compose 解析与 Caddy 配置校验。race 无报告不等于所有可能运行时序都已穷尽。

**推断：**同源聚合减少多母号重复扫描，收件人集合匹配减少本地重复解析，索引减少取码热查询遍历；公网往返较慢时应有收益。未测优化前公网基线，不给“公网延迟下降 90%”或指定吞吐承诺。

**未知/未验证：**真实 Gmail/Apple 的认证、TLS/DNS、实际投递延迟和提供商限制；生产 Compose 镜像构建/容器 stop 与重启、实际反向代理与 TLS；独立服务器 RSS/FD、持续创建吞吐、30 分钟/2 小时稳态，以及通知/补货/Cookie 监控等真实后台业务同时运行的容量。Gmail 配额须按当前官方每账号说明及实际响应核验，不能沿用“单 IP 15 个连接”；本机 fixture 不会触发提供商限制。

## 6. 部署说明

生产继续用现有 Docker Compose，保存 `.env`、`data` 与 `ICLOUD_HME_MASTER_KEY`。仓库代理模板验证码读取期限为 150 秒，覆盖业务最长 120 秒及存储/交付余量；Caddy 同时设置 `response_header_timeout 150s`，已在 v2.10.2 实际适配校验。部署时仍须按实际代理版本检查生效配置，模板校验不代表服务器已更新。

主服务 Compose 设置 `nofile.soft/hard=65535`。代理也要单独具备句柄容量：Nginx 主配置顶层设置 `worker_rlimit_nofile 65535;`，`events` 设置 `worker_connections 16384;`，执行 `nginx -t`；Caddy systemd 服务确认 `LimitNOFILE=65535`。每个 HTTP/1 长轮询通常同时占用客户端和上游连接，不能只按 2000 设置代理容量。用 `docker compose exec icloud-hme sh -c 'ulimit -n'` 核验实际容器软上限。

v2 GET 默认 timeout=0，兼容取码默认 30 秒，上限均为 120 秒；长轮询应显式传 timeout。测试显式 120 秒与运行了 60 秒是不同概念。

默认单 Token 为 500 活跃任务。单 Token 要承载 2000 时只设置现有变量，不增加新老 API 或配置别名：

```dotenv
ICLOUD_HME_MAX_PER_TOKEN_ACTIVE_VREQ=2000
ICLOUD_HME_MAX_GLOBAL_ACTIVE_VREQ=4000
```

等待连接默认单主体 2000、全局 4000；持久化任务、挂起连接、短阶段在途请求是不同预算。本轮单 Token 场景独立验证，仍不代表真实 Gmail 吞吐达标。

首次启动自动升级至 schema v11，并对旧数据库保存迁移前一致性快照。不要手动改 user_version，也不要让旧程序打开升级后的库。需要回退时使用迁移前备份和原 Master Key，并接受备份之后的数据回退；不要直接覆盖运行中的数据库。

HTTP/资源关闭共享 10 秒总体预算；Compose stop_grace_period 为 30 秒。清理成功才报告优雅停机，超时明确报错，调用方可继续等待同一清理流程。共用关闭方法及 Linux SIGTERM 已测，生产容器信号、代理和完整后台业务仍需服务器验收。部署后检查 `docker compose ps`、日志、`/readyz` 和实际资源限制，再逐步放量。
