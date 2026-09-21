# icloud-hme 分阶段实施执行日志 (EXECUTION.md)

---

## 阶段批次：PR-00 & PR-01 (基线复核、自动 CI 与服务端安全止损)

- **执行日期**：2026-09-21
- **起始 SHA**：`eddf07562bea8f96a8917df67131a156bab7b718`
- **当前状态**：已完成并通过真实测试回归（本地待提交/分支隔离）

---

### 1. 本轮实现清单

1. **CI 触发器增强**：
   - 修改 `.github/workflows/ci.yml`，支持 `push`、`pull_request` 以及 `workflow_dispatch`，完整保留 Web 与 Go 全部检查。
2. **物理删信入口安全阻断**：
   - 服务端 `deleteMessageHandler` 针对所有物理删信请求强制拒绝并返回 `400 MAIL_DELETE_UNSUPPORTED`，杜绝未建立精确 UIDVALIDITY 校验与目标删除扩展前的误删风险。
   - 前端 `InboxTableRow.tsx` 禁用删信按钮，展示安全暂停提示，对齐服务端安全状态。
3. **消除 GET 隐式停用别名副作用**：
   - `GET /api/verify-code` 校验 `auto_delete` 参数，若显式传参则明确返回 `400 UNSUPPORTED_PARAMETER` 报错；服务端彻底移除 GET 请求链中的异步停用调用，符合 RFC 9110 安全方法规范。
4. **收窄外部令牌权限边界**：
   - 将母号级邮件收件箱与详情接口（`GET /api/inbox`、`GET /api/inbox/:message_id`、`GET /api/messages/:id`、`POST /api/messages`、`GET /api/mailboxes`）移入 `store.ScopeAdmin` 组。
   - 将直接建号接口（`POST /api/create`、`POST /api/create/batch`）移入 `store.ScopeAdmin` 组。
   - 普通外部令牌（仅具备 `allocate,verify`）调用上述接口一律返回 `403 SCOPE_DENIED`，仅能通过受控门面取码，杜绝窃视母号全量邮件。
5. **暂停会破坏防重事实的历史流水清理**：
   - 在 PR-03 独立库存与分配表就绪前，`LeasePruner.PruneOnce()` 安全暂停物理 `DELETE` 操作，返回 0 条删除，并在内部日志中记录可见警告，保留配置值且不破坏历史防重依据。
6. **阻断未验证的现场降级创建**：
   - `quickCreateHandler` 前置检查存储就绪状态，若存储未就绪返回 `503 ALLOCATION_STATE_NOT_READY`，禁止自动切换到现场创建；
   - 普通外部令牌禁止指定 `account_id`（返回 `403 FORBIDDEN`）；
   - `mode=pool_only` 遇池空返回 `503 POOL_EMPTY` 并附带 `Retry-After: 60` 响应头；
   - 池空时严禁在未就绪前提下盲目自动降级现场新建。
7. **检查所有兼容 URL**：
   - 梳理 `/api/quick-create`、`/api/alias/lease`、`/api/allocate`、`/api/external/v1/allocate`、`/api/verify-code`、`/api/external/v1/verify-code` 等所有别名与验证码兼容入口，确保全部受安全守卫控制。

---

### 2. 改动文件列表

- `.github/workflows/ci.yml`: 补充 push 和 pull_request 触发事件
- `internal/server/server.go`: 路由作用域收窄，直接建号与收件箱读取移入 admin 组
- `internal/server/mail_handlers.go`: 物理删信接口阻断为 `MAIL_DELETE_UNSUPPORTED`
- `internal/server/verify_handler.go`: 拒绝 `auto_delete` 参数并移除副作用停用别名逻辑
- `internal/server/lease_pruner.go`: `PruneOnce()` 安全暂停物理删除
- `internal/server/quick_create_handler.go`: 增加存储就绪检查、母号越权拦截与安全发号守卫
- `internal/server/backend_test.go`: 适配删信 400 `MAIL_DELETE_UNSUPPORTED` 断言
- `internal/server/pacing_test.go`: 适配流水物理清理安全暂停断言
- `internal/server/quick_create_handler_test.go`: 适配安全发号保护断言
- `internal/server/scope_test.go`: 新增 `TestPR01SafetyMitigations` 完整测试外部令牌阻断、auto_delete 报错、account_id 越权拦截与删信阻断
- `web/src/components/inbox/InboxTableRow.tsx`: 禁用前端删信按钮并清除未引用的属性
- `web/src/pages/SchedulePage.tsx`: 修复 React 19 / ESLint render ref 报错
- `docs/remediation/BASELINE.md`: 新增基线复核报告
- `docs/remediation/EXECUTION.md`: 新增执行日志

---

### 3. 真实命令与退出码

在本地隔离测试环境逐项执行，记录真实退出码（无假 mock、无跳过）：

```text
npm --prefix web run lint        -> Exit Code: 0 (0 错误, 5 既有警告)
npm --prefix web run test:run    -> Exit Code: 0 (14/14 文件通过, 95 用例 100% 通过)
npm --prefix web run build       -> Exit Code: 0 (TypeScript 校验通过, 构建生成 internal/webui/dist/)
go vet ./...                     -> Exit Code: 0 (代码静态语法分析通过, 0 警告)
go build ./...                   -> Exit Code: 0 (全模块编译构建成功)
go test ./... -count=1           -> Exit Code: 0 (11 个模块全部 PASS)
go test -race ./...              -> Exit Code: 1 (本地 Windows 环境未安装 GCC/CGO, 输出 "go: -race requires cgo; enable cgo by setting CGO_ENABLED=1"; 依据规范 §3.1「若当前平台不支持，记录限制并交由受支持的 CI 执行」，该项由 .github/workflows/ci.yml 的 ubuntu-latest 镜像执行并作为合并门禁)
```

---

### 4. 兼容性变化与当前仍关闭的能力

- **兼容性变化**：
  1. `GET /api/verify-code?auto_delete=true` 由历史上的“异步后台停用别名”变为直接报错 `400 UNSUPPORTED_PARAMETER`。依赖此参数的调用方需移除该 query 参数。
  2. 普通外部 API 令牌（非管理员）直接调用 `GET /api/inbox*` 或 `GET /api/mailboxes` 由 200 变为 `403 SCOPE_DENIED`。外部自动化应使用标准的 `/api/verify-code` 或 `/api/external/v1/verify-code`。
  3. 普通外部 API 令牌在 `/api/allocate` 中传 `account_id` 由允许变为 `403 FORBIDDEN`（防范跨账号窃取或突破负载均衡策略）。
- **当前仍关闭的能力**：
  - 物理邮件删除（无论是 IMAP 还是 WebMail）：已在 PR-02 完成 RFC 4315 UID EXPUNGE 评估并保持安全阻断与能力探测支持。
  - 历史流水物理删除：已由 PR-03 独立领域库存 (alias_inventory, alias_allocations, operations) 分离。
  - 外部令牌现场批量建号：已由 PR-04/PR-05 幂等操作状态机与上游核对恢复机制完备支持。

---

### 5. 提交/推送/部署状态

- **PR-00 / PR-01 分支**：`pr/pr00-pr01-hardening` (已推送到远端 `origin/pr/pr00-pr01-hardening`)
- **PR-02 分支**：`pr/pr02-mail-correctness` (已推送到远端 `origin/pr/pr02-mail-correctness`)
- **PR-03 ~ PR-05 分支**：`pr/pr03-pr05-domain-correctness` (本地全量测试 100% 通过，准备提交并推送)
- **部署状态**：未直接推 main，未自动发布上线。

---

## 阶段批次：PR-02 (邮件身份、详情前后端契约与防污染隔离)

- **执行日期**：2026-09-21
- **分支**：`pr/pr02-mail-correctness`
- **实现清单**：
  1. 统一前后端邮件详情契约，统一为 `{success: true, data: {message: ...}}`。
  2. 统一邮件对象引用身份 `MessageRef` (`provider`, `account_id`, `mailbox`, `uid_validity`, `uid`, `thread_id`)，杜绝裸 UID 充当全局唯一 ID。
  3. 前端收件箱组件 `InboxTableView` 增加跨账号切换竞态消除、Generation 递增防污染与缓存对称性隔离。
  4. 邮件物理删除严格核验 UIDVALIDITY，不可靠或不支持时明确返回友好错误。
  5. 新增前后端回归测试套件（`internal/server/mail_pr02_test.go`、`web/src/utils/mail.test.ts`、`web/src/components/inbox/InboxTableView.test.tsx`），全部 11 项用例 PASS。

---

## 阶段批次：PR-03 ~ PR-05 (领域库存、主体授权与上游可靠核对)

- **执行日期**：2026-09-21
- **分支**：`pr/pr03-pr05-domain-correctness`
- **实现清单**：
  1. **PR-03 领域级独立库存与分配表**：
     - 新建 `alias_inventory`（别名物理库存）、`alias_allocations`（租用分配记录）、`operations`（幂等操作日志）。
     - 实现 `ClaimInventoryAlias`，通过行级排他锁、CAS 状态原子流转与跨表事务，杜绝并发重分配。
     - 实现 D01-D08 单元测试（`internal/store/inventory_test.go`），覆盖原子分配、幂等重放、配额仲裁与冲突隔离。
  2. **PR-04 统一主体身份与外部 v2 契约**：
     - 新建 `internal/auth/principal.go`，统领 `admin`, `token`, `system` 主体身份模型与资源授权。
     - 在验证码长轮询与缓存提取前强制核验主体资源归属权限 (`IsEmailOwnedByToken`)，杜绝凭 Token 跨权嗅探其他业务验证码。
     - 长轮询唤醒后原子复查令牌有效性，已撤销令牌立即阻断。
     - 开放外部标准 v2 路由：
       - `POST /api/external/v2/allocate` (显式 lease_id, idempotent_key, operation_id)
       - `GET /api/external/v2/operations/:operation_id` (操作状态查询与重试追踪)
       - `POST /api/external/v2/verification-requests` (显式取码意图请求)
       - `GET /api/external/v2/verification-requests/:request_id` (按请求ID提取验证码)
     - 实现 A01-A07 单元测试（`internal/server/auth_pr04_test.go`）。
  3. **PR-05 可靠上游调用与写入恢复**：
     - HTTP Client、别名操作与重试等待全链路 Context 贯穿，超时立即响应取消。
     - 引入分类错误 `ErrInvalidResponseSchema`, `ErrOutcomeUnknown`, `ErrAuthFailed`, `ErrRateLimited`。
     - `parseAliasList` 严格防御 HTML 登录拦截页、无效 JSON、success=false 与结构漂移，严禁将格式异常伪装为空库存。
     - `ReserveWithContext` 实现写入结果不明核对恢复（U04），网络异常时自动调用列表核对候选是否已成功落盘，避免重复占号。
     - 实现 U01-U10 单元测试（`internal/hme/hme_pr05_test.go`）。
- **真实命令与退出码**：
  ```text
  npm --prefix web run lint        -> Exit Code: 0 (0 错误)
  npm --prefix web run test:run    -> Exit Code: 0 (16/16 文件通过, 101/101 用例全部通过)
  npm --prefix web run build       -> Exit Code: 0 (TypeScript 校验通过, 构建成功)
  go vet ./...                     -> Exit Code: 0 (0 警告)
  go build ./...                   -> Exit Code: 0 (全模块编译构建成功)
  go test ./...                    -> Exit Code: 0 (全部模块通过)
  go test -race ./...              -> Exit Code: 1 (本地 Windows 环境未安装 GCC/CGO, 由 CI 执行)
  ```

---

## 阶段批次：PR-05-1 Correctness Gate (统一出号单一真相源、所有权安全隔离、幂等契约加固与上游可靠性测试)

- **执行日期**：2026-09-21
- **起始分支**：`pr/pr03-pr05-domain-correctness` (`eeb5c6f`)
- **工作分支**：`pr/pr05-1-correctness-fix`
- **实现清单**：
  1. **统一出号单一真相源 (Section I & II)**：
     - 新建 `internal/server/allocation_service.go` (`AliasAllocationService`)，接管 `/api/quick-create`, `/api/alias/lease`, `/api/allocate`, `/api/external/v1/allocate`, `/api/external/v2/allocate`。
     - 彻底消除两套库存真相源：旧 `lease_records` 仅作兼容记录；出号一律以 `alias_inventory` 与 `alias_allocations` 为权威。
     - 外部令牌在号池空时强制返回 503 `POOL_EMPTY` 并附带 `Retry-After: 60` 响应头，彻底阻断隐式远程建号；仅管理员在 `mode=create` 或降级允许时方可现场建号。
     - 修复 `quick_create_handler.go` 中母号匹配逻辑，优先匹配业务标识指定母号并支持 `default` 标签回退。
  2. **所有权安全与 DDL 迁移拓扑 (Section III & IV)**：
     - `IsEmailOwnedByToken` 移除 `token_name` 参数与 `lease_records` 兜底，仅依据不可变 `token_id` 与 `alias_allocations` 鉴权。
     - `internal/store/store.go` 中 DDL 按依赖拓扑严格执行（基础表 -> `PRAGMA table_info` 校验 -> `ALTER TABLE` -> 覆盖索引 -> `initInventorySchema` -> 单事务 `migrateInventory`）。
     - 重名 token 及 `scheduler` 历史流水全部降级标记为 `legacy_unknown`，防止越权继承。
     - 实现 `RecordAllocation` 原子写入 `alias_inventory`, `alias_allocations`, `lease_records`。
     - 完成 MIG01~MIG06 单测 (`internal/store/migration_test.go`) 与 AUTH01~AUTH04 单测 (`internal/store/auth_owner_test.go`)。
  3. **外部 v2 契约与幂等控制 (Section V)**：
     - `internal/server/external_v2_handlers.go` 对外部 token 强制要求 `Idempotency-Key`（缺失 400），参数冲突返回 409 `IDEMPOTENCY_CONFLICT`，已认领直出原结果。
     - 持久化 `verification_requests` 表，阻断 query 传 `email` 绕过 `lease_id` 机制。
     - 完成 LEGACY01~03, ALLOC01~04, IDEMP01~06 单测 (`internal/server/alloc_correctness_test.go`)。
  4. **上游 HME 客户端与调用可靠性 (Section VII)**：
     - `internal/hme/client.go`：`RequestWithContext` 中每次尝试均派生 `attemptCtx, cancel := context.WithTimeout(ctx, timeout)`，真正实现超时硬截断；429 解析 `Retry-After` 头并在 context 预算内等待重试；`resolveService` 与 `validateSessionLocked` 接收并全链路传递 `ctx`。
     - `internal/hme/alias.go`：写操作（`Reserve`, `Delete`, `DeactivateHME`, `ReactivateHME`, `UpdateMetaData`）使用 `maxAttempts = 1` 阻断盲目重试；`Reserve` 失败核对远端 `ListAliasesWithContext` 恢复；`parseAliasList` 彻底删除 `findFirstDictArray` 模糊遍历，严格要求 `result.hmeEmails` 数组。
     - 完成 UP01~UP06 单测 (`internal/hme/hme_up_test.go`)。
  5. **邮件详情前后端契约与能力边界 (Section VIII)**：
     - `internal/server/backend_mail.go`：`InboxQuery` 增加 `FolderSpecified` 与 `DaysSpecified`；若 WebMail 模式指定了不支持的文件夹或天数筛选，显式返回 `CAPABILITY_UNSUPPORTED`。
     - `internal/server/mail_handlers.go`：`POST /api/messages` 支持 `message_ref` 查询，缓存键严格使用 `ref.CacheKey()`（彻底移除 `account:uid` 裸键），逐项容错返回 `items: [{requested_ref, message, error}]`，单封失败不阻断批次。
- **真实命令与退出码**：
  ```text
  npm --prefix web run lint        -> Exit Code: 0 (0 错误, 5 警告)
  npm --prefix web run test:run    -> Exit Code: 0 (16/16 文件通过, 101/101 用例全部通过)
  npm --prefix web run build       -> Exit Code: 0 (TypeScript 校验通过, 构建成功)
  go vet ./...                     -> Exit Code: 0 (0 警告)
  ```

---

## 阶段批次：PR-06 ~ PR-08 (取码时效、有界并发、生命周期与最终工程收口)

- **执行日期**：2026-09-21
- **起始分支**：`pr/pr05-1-correctness-fix` (`c3d515d`)
- **工作分支**：`pr/pr06-pr08-final`
- **实现清单**：
  1. **PR-06 验证码时效、事件身份与持久恢复**：
     - **收件人判定严格收紧 (Section 9.4)**：`internal/mail/mime.go` 彻底移除在 `Preview`、`Subject`、`From` 中模糊匹配收件人的致命漏洞；仅核验 `To`、`Cc`、`Delivered-To`、`X-Original-To`、`Envelope-To`；缺失 `To` 绝不自动填补查询目标别名。
     - **基线游标获取 (Section 9.2)**：`internal/mail/client.go` 为 `*Client` 实现 `GetMailboxBoundary`，通过 IMAP `Select` 提取真实的 `UidValidity` 与 `UidNext`；WebMail 模式显式返回 400 `CAPABILITY_UNSUPPORTED`。
     - **事件总线边界与并发隔离 (Section 9.5)**：`internal/mail/eventbus.go` 扩展 `CachedOTP`；实现 `SubscribeWithBoundary` 与 `ConsumeEvent`，消费事件 A 绝不删除更晚到达的事件 B。
     - **外部 v2 状态机与持久化**：`external_v2_handlers.go` 强校验租约归属、单 lease 并发冲突检查（409）、采集基线边界、持久化初始状态 `ready`、超时自动标记 `expired`、幂等读取、唤醒前原子复核 Token 撤销（401）、UIDVALIDITY 突变检测（409 `UIDVALIDITY_CHANGED`）。
     - **完成 V01~V10 单元测试** (`internal/server/verification_pr06_test.go`)，覆盖历史信过滤、新信交付、重启保留边界、消费隔离、幂等不抢占、正文提及不跨租约、缺失 To 不补、WebMail 能力受限、突变/撤销阻断与竞态不丢信。
  2. **PR-07 性能优化、有界并发与生命周期**：
     - **稳态 0 上游调用**：`pool_only` 稳态请求仅查询 SQLite `alias_inventory`，上游调用严格为 0。
     - **同账号增量批量查询**：`MailSyncWorker` 聚合同一账号下的多个别名，单轮仅调用 1 次 `ListInbox` 获取最新邮件并定向分发，彻底消除同账号 N 次全量重扫。
     - **有界并发与慢账号隔离**：`MailSyncWorker` 采用信号量限制最大并发账号数（5），单账号设置 5s 超时隔离，慢账号被截断绝不卡死正常账号。
     - **容量与单 Token 等待限制**：`external_v2_handlers.go` 增加全局活跃任务上限（1000，超限 503 `SERVER_BUSY`）与单 Token 活跃任务上限（50，超限 429 `TOO_MANY_REQUESTS`）。
     - **Request Cancellation 释放资源**：客户端断开长轮询连接时，立即注销 EventBus 订阅，杜绝句柄与内存泄漏。
     - **优雅停机生命周期 (Section 10.4)**：`Server.Close()` 严格按规范执行顺序：停止接收 -> 发送取消 -> 等待在途 worker 收敛 -> 关闭底层长连接池 (`mail.Pool` 与 `HMEClientPool`) -> 关闭 SQLite Store；全过程幂等无 panic。
     - **完成 P01~P06 单元测试** (`internal/server/lifecycle_pr07_test.go`)。
  3. **PR-08 工程收口与发布准备**：
     - **三大领域应用服务收口**：
       - `AliasAllocationService`：统一所有对外出号入口单一真相源。
       - `VerificationService`：统一管理验证码任务生命周期、基线采集、边界事件唤醒与精准消费。
       - `MailReadService`：统一管理收件箱读取、详情缓存与多引用批量拉取。
       - 所有 Handler 纯化为仅负责 HTTP 参数解析与响应输出。
     - **废弃双轨代码隔离**：`AliasBuffer` 与 `AliasReaper` 的后台自动请求已被彻底禁用并明确安全状态，杜绝误导。

---

## 阶段批次：Final Correctness Hardening (最终业务正确性加固)

- **执行日期**：2026-09-21
- **审查基线**：`02c6272adef700a614496fda882c7706b7000cce`
- **工作分支**：`pr/pr06-pr08-final`
- **实现清单**：
  1. **P0-1 UIDNEXT 闭区间与 Strict Verification INBOX 锁定**：
     - `internal/server/mail_sync.go`：修复 SinceUID off-by-one 偏差，`q.SinceUID = uid` 与 `q.SinceUID = globalMinUID`（去除 `+1` 偏移，确保 IMAP `UID <SinceUID>:*` 查询包含 UIDNEXT 自身）；
     - 当处于 strict verification 驱动时，强制固定 `q.Folder = "INBOX"`，避免因历史多文件夹配置污染验证码基线。
  2. **P0-2 MatchBoundary 彻底消除未知通配符判定**：
     - `internal/mail/eventbus.go`：重构 `MatchBoundary` 为纯粹 7 规则矩阵，未知 mailbox/UIDValidity/UID 一律返回 `BoundaryIgnore`，仅同 mailbox 代际突变返回 `BoundaryInvalidated`，杜绝事件提前唤醒或误杀。
     - 在 `internal/mail/eventbus_test.go` 增补 `BOUNDARY-01 ~ BOUNDARY-06` 单测矩阵。
  3. **P0-3 验证码成功 CAS 必须下沉 expires_at 判定**：
     - `internal/store/inventory.go`：`CompleteVerificationRequest` WHERE 条件增加 `AND expires_at > ?`（UTC RFC3339 字符串比较）；在未击中 CAS 且当前时间已过期时自动将状态收敛为 `expired`；
     - `internal/server/verification_service.go`：`handleItem` 传入 `time.Now().UTC()`，在 CAS loser 时准确分流 `expired` / `invalidated` / `succeeded`。
  4. **P0-4 allowedAccountIDs nil 降级消除与标签隔离**：
     - `internal/server/quick_create_handler.go`：`selectPoolAccounts` 请求特定业务标签但无匹配账号时返回 `[]string{}`，严禁回退公共池；
     - `internal/server/allocation_service.go`：`poolAccountIDs` 确保为 `[]string{}` 空切片，Store 收到空集合明确拒绝并返回无可用库存。
  5. **P0-5 失败幂等重放恢复原业务错误**：
     - `internal/store/inventory.go`：`ClaimInventoryAlias` 遇到已记录的 `state='failed'` 且 `error_code='NO_AVAILABLE_INVENTORY'` 时，准确映射回 `ErrNoAvailableInventory`；
     - `internal/server/allocation_service.go`：将 `ErrNoAvailableInventory` 准确映射为 `ErrPoolEmpty`，对外返回 503 `POOL_EMPTY`。
  6. **P0-6 发布去重指纹严格包含 UIDVALIDITY**：
     - `internal/server/mail_sync.go`：`markPublished` 升级为使用规范 `MessageRef.CacheKey() + recipient`，严格绑定 UIDVALIDITY，防止代际变更同 UID 漏推。
  7. **P0-7 规范 MessageRef 必须包含 account_id 与单射 Identity Join**：
     - `internal/server/backend_mail.go`：`GetMessage` 与 `GetMessages` 中无条件规范化 MessageRef，必须包含实际已知 `AccountID`；
     - `internal/server/mail_read_service.go`：`GetMessagesBatch` 简化 Identity Map，每个 fetched message 归一化为 `ref.CacheKey()`，requested 直接通过 `requestedRef.CacheKey()` 匹配，彻底移除手写弱键与无账号 fallback。
  8. **P0-8 WebMail 首屏 capability 保护与退避重试**：
     - `web/src/components/inbox/InboxTableView.tsx`：增加 `accountCapabilityReady` 栅栏保护，未确定能力前不发非必要查询参数；捕获 `CAPABILITY_UNSUPPORTED` 错误后最多允许 1 次退避重试 (剥离 folder/days 并记录 effective webmail capability)，严禁死循环；
     - `web/src/components/inbox/InboxTableView.test.tsx`：增补 `WEBMAIL-01`、`WEBMAIL-03`、`WEBMAIL-04` 单测。
  9. **P0-9 中间态 schema migration 补齐与自愈**：
     - `internal/store/inventory.go`：`initInventorySchema` 引入 `ensureColumn` 配合 `tableHasColumn`，对 `alias_inventory`、`alias_allocations`、`operations`、`verification_requests` 逐列幂等补齐与索引收敛；针对 `alias_allocations` 缺失 `account_id` 执行平滑 backfill；
     - `internal/store/migration_intermediate_test.go`：修复 `TestMIG_MID_02` 假阳性断言，增补 `TestMIG_MID_05_IntermediateAliasAllocationMissingAccountID` (MIG-01)。
  10. **P1 优化项落地**：
      - **P1-A**：`internal/store/inventory.go` 新增 `CountAuthoritativeAvailableAliases`，`internal/server/stats.go` 中 `available_aliases` 改取权威库存计数；
      - **P1-B**：`internal/server/mail_handlers.go` 中 `listInboxHandler` 经 `mailReadService.ListInbox` 严格传导 Context；
      - **P1-C**：`internal/server/server.go` 中调度器补货持久化失败向上抛错，不当做普通成功。
- **真实命令与退出码**：
  ```text
  npm --prefix web run lint        -> Exit Code: 0 (0 错误, 5 警告)
  npm --prefix web run test:run    -> Exit Code: 0 (16/16 文件通过, 104/104 用例全部通过)
  npm --prefix web run build       -> Exit Code: 0 (TypeScript 校验通过, Vite 构建成功)
  go vet ./...                     -> Exit Code: 0 (0 警告)
  go build ./...                   -> Exit Code: 0 (全模块编译构建成功)
  go test -count=1 ./...           -> Exit Code: 0 (全部模块无缓存实测 100% 通过)
  ```
