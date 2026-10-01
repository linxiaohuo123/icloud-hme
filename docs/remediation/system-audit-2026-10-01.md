# 全系统审查记录（2026-10-01）

## 结论与范围

初始审查复现了 4 个缺陷，并发现反向代理配置风险与开发依赖安全告警。后续按要求修复；当前修复与验证记录见文末。以下发现保留初始根因和复现证据。

审查基线：`fe73194904f81254530cf117b186ee313333f8e0` 加当前工作区已有未提交修改。检查覆盖桌面前端与接口调用、Go 服务、SQLite 库存与恢复流程、Camoufox 登录生命周期、通知、停机、Compose/反向代理范本和依赖。

所有故障复现使用临时 SQLite、虚构凭据和本地 mock 上游；未调用真实 Apple 创建或修改接口，未修改 `.env`、运行数据或 Master Key。原有未提交修改和 staged 删除均保留。移动端不在审查范围。

## 已复现缺陷

### F1 / P1：通知网络错误暴露 URL 中的凭据

- 位置：`internal/notify/notify.go:352`、`:366`、`:368`、`:267`、`:294`。
- 触发：已配置通知渠道，在 DNS、连接、TLS 等网络失败时发送消息。
- 根因：Telegram token 被拼入 URL；`http.Client.Post` 返回的 `url.Error` 带完整 URL，`postJSON` 直接包装，随后写入日志或作为测试通知错误返回前端。飞书 webhook、Bark device key 使用同一函数，存在相同错误传播路径。
- 影响：日志及错误响应可能包含完整推送凭据。已证明代码路径可泄露，未核验生产日志是否实际包含凭据。
- 复现：mock RoundTripper 返回网络错误，以虚构 `AUDIT_FAKE_TELEGRAM_SECRET` 作为 token，确认返回错误与 dispatch 日志同时包含该字符串。
- 临时探针：`TestAuditProbeNotificationSecretsInFailureLog`，通过。
- 修复方向：在通知 HTTP 边界生成安全错误，保留渠道、错误类别和必要诊断，去除带秘密的 URL；同时覆盖日志与测试通知响应。

### F2 / P2：补货成功后的库存写入失败无法自动恢复

- 位置：`internal/server/backend_alias.go:322`；`internal/server/server.go:182`、`:189`；`internal/store/reserve_intent.go:161`；`internal/store/inventory.go:335`、`:359`。
- 触发：上游 Generate/Reserve 已成功，但调度补货的 `AddInventoryAlias` 写入失败。
- 根因：创建流程先把 Reserve intent 标记为 `succeeded`，补货流程随后才写入可用库存。恢复查询只扫描 `prepared/reserve_sent/outcome_unknown`，不再处理该成功 intent；调度补货也没有绑定远程分配 operation。远端列表刷新仅补入 `unknown/synced`，无法恢复可分配资格。
- 影响：实际创建已消耗 Apple 额度，调度却记为失败；该邮箱无法自动作为补货库存出号。邮箱仍可从远端刷新看到并由管理员手动处理，未复现重复分配。
- 复现步骤：
  1. 使用本地 HME mock 成功返回 Generate、Reserve 和 List。
  2. 临时 SQLite trigger 只拒绝 `alias_inventory` INSERT。
  3. 调度结果为 created=0、failed=1，但 Reserve intent 已为 succeeded。
  4. 移除 trigger，运行 reconciliation，返回空且库存仍不存在。
  5. 刷新列表后库存为 unknown/synced，认领返回 `NO_AVAILABLE_INVENTORY`。
- 临时探针：`TestAuditProbeReplenishmentInventoryFailureCannotRecover`，通过。
- 修复方向：为补货保留可持久恢复的业务归属，确保成功创建但尚未入库的结果能够幂等恢复为 available；恢复不得重新调用 Reserve。

### F3 / P2：登录请求断开后，遗留 OTP 任务阻挡下一次登录

- 位置：`internal/server/account_handlers.go:299`；`internal/server/backend.go:805`、`:808`、`:818`、`:864`、`:496`、`:752`。
- 触发：登录尚在轮询时刷新页面或断开 HTTP 请求，代理随后返回 otp_required。
- 根因：handler 调用的 `LoginAccount` 不接收请求 Context；轮询和 sleep 独立运行。断开请求后仍持久化 OTP 任务，但前端无法接收包含 task_id 的响应。
- 影响：重新登录返回 `AUTH_IN_PROGRESS`，用户无法继续刚才的验证码流程。已有 3 分钟 OTP 超时回收，因此不是永久卡死；正常代理取消成功时会释放任务。
- 复现：本地 Camoufox mock 阻塞任务查询；取消真实 HTTP 客户端 Context 后释放 OTP 响应。确认任务仍被持久化、未发送 DELETE、新登录返回 AUTH_IN_PROGRESS；最后显式 Cancel 清理。
- 临时探针：`TestAuditProbeDisconnectedCamoufoxLoginLeavesOTPTask`，通过。
- 修复方向：为登录请求明确取消/恢复契约；当前同步接口应贯穿 Context，并在请求取消后正确清理未交付任务。

### F4 / P2：停机超时后的再次关闭返回伪成功

- 位置：`internal/server/server.go:410`、`:411`、`:499`、`:506`、`:512`、`:517`。
- 触发：第一次 `CloseContext` 等待后台任务超时，随后后台任务退出，再调用 CloseContext。
- 根因：`sync.Once` 包裹整个关闭流程；第一次超时消耗 once，却未关闭 backend 和 Store。第二次调用不执行流程，局部 err 默认 nil，直接返回成功。
- 影响：调用方得到成功结果，但底层连接仍未关闭，停机重试语义不成立。未复现数据损坏。
- 复现：临时 Store 和 fakeBackend，阻塞 autoSyncWg；第一次 15ms 超时，释放任务后使用 Background 重试。返回 nil，但 backend close 标志仍为 false，Store Ping 仍成功。
- 临时探针：`TestAuditProbeCloseTimeoutRetryReportsSuccessWithoutCleanup`，通过。
- 修复方向：将一次性发出停止信号与等待/最终关闭分开，使后续调用能等待或继续完成关闭，并返回真实结果。

## 配置与依赖风险

### D1：Nginx 范本超时短于登录流程等待上限

`deploy/nginx.conf:61` 配置全局 `proxy_read_timeout 130s`，账号登录没有单独覆盖；`internal/server/backend.go:552` 的初次登录轮询上限为 210 秒。慢登录在持续 130 秒未返回数据时会被此范本提前断开，可能放大 F3。

已确认配置不匹配，未在生产 Nginx 复现 504，也未确认生产是否使用此范本。Caddy 范本不具有相同全局 130 秒限制。建议根据登录链路总预算配置该路由的超时。

### D2：默认 Nginx access log 可能记录查询凭据

该 Nginx 范本未定义脱敏 access_log。若部署沿用默认 combined 格式中的 `$request`，分享直链 query token/API key 会被记录。此为条件性风险，实际生产日志配置未知。

Go Gin 已启用 `SkipQueryString: true`，此风险不适用于当前 Go access log。建议在代理层采用不含查询字符串的 URI 日志格式。

### D3：前端开发依赖有已知安全告警

`npm audit --json` 报告 6 个受影响 package 条目：4 high、2 moderate、0 critical；`npm audit --omit=dev --json` 为 0。全部属于开发、构建或测试依赖，不能据此断言部署的 Go 服务可被利用。

受影响链路包含 brace-expansion、js-yaml、undici、nanoid，以及 vitest/@vitest/mocker。当前 vitest 为 4.1.10，审计给出的修复版本范围包含 4.1.11。建议更新并验证 lockfile；升级需结合当前工具版本兼容性。

### D4：依赖审计的已验证上限

- Go：`go run golang.org/x/vuln/cmd/govulncheck@latest ./...` 未发现当前代码可触达或已导入包中的漏洞；另报告 3 个当前未调用的 required-module 漏洞。
- Python：对 Camoufox agent 当前 `.venv/Lib/site-packages` 运行 pip-audit，60 个依赖、0 个已知漏洞、0 个跳过项。Docker 构建按 requirements 重新解析依赖，当前本地环境结果不等同于生产镜像清单。
- npm：生产依赖审计为 0；开发依赖告警如 D3。
- CI：Go/前端 CI 与主服务镜像 workflow 仅手动触发属于已有明确选择，不列为新 bug；Camoufox 镜像 workflow 的构建成功也不能替代 Python 测试。

## 验证记录

| 验证 | 结果与范围 |
| --- | --- |
| `go test ./... -count=1` | 本轮通过 |
| `go vet ./...` | 本轮通过 |
| Camoufox agent `python -m unittest discover -v` | 本轮 22 项通过 |
| `docker compose config --quiet` | 本轮通过，仅证明编排配置可解析 |
| 4 项临时故障探针 | 全部通过，证明当前错误行为可复现，不表示缺陷已修复 |
| 桌面 Chrome 1440×900 | 隔离 mock 服务登录、导航、主要页面、下拉菜单及 Escape 冒烟通过；登录后未见 console 错误 |
| CSP / React inline styles | 原疑点已排除，实际 CSSOM 样式和菜单定位正常 |
| `npm run check` | 上一轮同一工作区通过：29 文件、262 项测试，以及 lint/typecheck/build；本轮未修改前端，未重复执行 |
| Go/npm/Python 漏洞审计 | 结果如 D3、D4 |

既有全量测试通过不覆盖上述故障窗口。临时探针、浏览器快照和隔离服务停止标记在审查收尾时清理；本文保存复现步骤与代码位置。

## 未验证事项与建议顺序

真实 Apple 登录、会话长期有效性、HME 服务行为、实际 IMAP/转发投递、Linux 容器运行、生产数据库和线上代理配置均未由本轮 mock 或本地测试证明。未读取生产日志，未进行生产写操作。

修复建议顺序：F1 通知错误脱敏；F2 补货持久化恢复；F3 登录取消与 D1 代理超时；F4 停机重试；随后更新开发依赖并确认 D2 生产日志格式。

## 修复记录（2026-10-01）

- F1：通知错误使用安全展示文本并保留可检查的 cause，日志和测试通知响应不输出 URL、凭据或上游错误文本。回归覆盖 Telegram、飞书与 Bark，包含 transport 主动回显秘密的情形。
- F2：schema v9 持久化补货用途；库存、路由与完成引用原子提交，成功但未入库的补货仍可恢复。回归覆盖 Reserve 前落盘、库存失败、刷新后重启恢复、禁止再次 Reserve、路由失败回滚和已分配状态保持。历史用途不明记录保持隔离，不能自动补猜用途。
- F3：登录 handler 传入请求 Context；Camoufox 健康检查、OTP 提交、轮询和等待响应取消，断开后清理任务。任务创建握手在 70 秒 HTTP 超时内取得 ID 后执行回收，避免取消时丢失代理任务 ID；回归覆盖创建期间和轮询期间断开。原生 SRP 路径的网络取消能力不在本次改动范围。
- F4：启动一次清理，调用方按各自 Context 等待同一个完成结果；等待超时后清理继续，后台任务结束后才关闭连接与 Store。回归覆盖超时后重试、多调用方和仅关闭一次。
- D1/D2：Nginx 登录路由单独设置 330 秒超时；两个 server 块均启用不含 query/Referer 的 access log 格式。README 同步预算与配置包含方式。
- D3：更新前端锁文件并将 Vitest 最低版本升为 4.1.11；npm 完整审计为 0。声明 Node 22.22.2+（22.x）、24.15+（24.x）或 26+；本机默认 Node 24.11.1 低于开发依赖要求，前端验证使用临时 Node 24.15.0，未修改系统 Node。

修复后验证：

| 检查 | 结果 |
| --- | --- |
| `go test ./... -count=1` | 全量通过；历史 Cookie 迁移测试使用 `CurrentSchemaVersion` 并保留无损数据与备份断言 |
| `go vet ./...` | 通过 |
| 新增故障回归 | 通知凭据、补货失败与重启、事务回滚、v8/v9 迁移、登录两阶段取消、停机重试均通过 |
| 前端 `npm run check`（Node 24.15.0） | lint、262 项测试、typecheck、Vite 构建通过 |
| npm 完整及 `--omit=dev` 审计 | 均为 0 个已知漏洞 |
| Camoufox agent unittest | 22 项通过 |
| `docker compose config --quiet` | 通过 |
| unstaged / staged `git diff --check` | 通过 |

生产数据未修改，本文记录本地修复与验证，GitHub 发布以提交记录为准，未部署服务器。Docker 引擎未运行且没有本机 Nginx，容器与代理实际运行未验证；本机 CGO 关闭且无 GCC，未执行 Go race 检查；真实 Apple 和 IMAP 仍需独立上线验证。
