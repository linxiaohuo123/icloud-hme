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
  - 物理邮件删除（无论是 IMAP 还是 WebMail）：已全面关闭，等待 PR-02 建立可靠邮件引用模型与 UID EXPUNGE 支持后重新评估。
  - 历史流水物理删除：已暂停，等待 PR-03 独立库存防重模型分离。
  - 外部令牌直接现场并发批量创建：已暂停，等待 PR-05 写入恢复状态机。

---

### 5. 提交/推送/部署状态

- **提交状态**：本地工作区修改就绪，遵循“不得直接推送 main、不得覆盖未提交已有改动”的铁律，等待人工核验或创建特性分支。
- **推送状态**：未推送。
- **部署状态**：未自动部署，未连接生产数据库与生产环境。

---

### 6. 下一轮 PR-02 任务展望

- 主题：**邮件身份、详情契约及安全删除**
- 任务重点：
  1. 详情包装结构规范化（保留 `{success, data: {message: ...}}`，前端使用 `MessageDetailResponse` 严格读取）；
  2. 引入规范化邮件引用（`MessageRef`），包含 `provider`、`account_id`、`mailbox`、`uid_validity`、`uid`，杜绝跨文件夹/跨账号覆盖与串信；
  3. 前端缓存竞态消除（切换账号/文件夹取消未完成请求）；
  4. 批量拉取响应对齐（每项保留 `requested_ref`，明确成功与失败，杜绝拿最近邮件冒充成功）；
  5. 安全物理删除能力探针与 RFC 4315 UID EXPUNGE 支持核验。
