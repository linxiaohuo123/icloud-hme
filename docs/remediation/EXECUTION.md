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

