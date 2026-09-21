# icloud-hme 发布与运维部署指南 (RELEASE.md)

---

## 1. 核心架构变更与行为变化 (Breaking Changes)

本次 PR-00 ~ PR-08 重构完成了底层高可用、安全隔离与协议规范化改造，主要行为变化如下：

1. **权限严格最小化**：
   - 外部业务 API 令牌（默认包含 `allocate,verify` 作用域）**禁止**直接调用管理员级建号与收件箱读取接口（`POST /api/create`、`GET /api/inbox`、`GET /api/mailboxes`）。调用将返回 `403 SCOPE_DENIED`。
   - 外部系统取码必须通过受控的 `/api/verify-code` 或外部规范 v2 接口。
2. **GET 请求安全无副作用**：
   - `GET /api/verify-code?auto_delete=true` 不再支持。传此参数将明确返回 `400 UNSUPPORTED_PARAMETER`，严禁通过 GET 请求产生异步停用别名的副作用。
3. **外部出号单一号池与阻断现场建号**：
   - 外部令牌请求出号仅能从本地可用库存中认领。当号池耗尽时，返回 `503 POOL_EMPTY` 并带 `Retry-After: 60`，不再允许外部令牌隐式触发主账号对 Apple 的远程批量建号。
4. **外部 v2 强幂等契约**：
   - `POST /api/external/v2/allocate` 对外部令牌强制校验 `Idempotency-Key` 请求头，缺失返回 400；相同幂等键传不同参数返回 `409 IDEMPOTENCY_CONFLICT`。
5. **取码基于 IMAP 基线游标**：
   - `POST /api/external/v2/verification-requests` 建立取码任务时，强校验 `lease_id` 归属，单 lease 存在活跃任务返回 409 冲突；自动采集 IMAP `UIDVALIDITY` 与 `UIDNEXT` 基线。
   - 若上游主号为 WebMail 且不支持单邮件游标，显式返回 `400 CAPABILITY_UNSUPPORTED`。

---

## 2. 生产环境建议配置

| 配置项 | 推荐值 | 说明 |
| :--- | :--- | :--- |
| `MAIL_POLL_INTERVAL` | `2s` | 后台收信轮询间隔（无活跃订阅者时自动静默） |
| `COOKIE_MONITOR_INTERVAL` | `30m` | Cookie 健康状态巡检周期 |
| `COOKIE_MONITOR_THROTTLE` | `1s` | 巡检各主号间的平摊节流，防止突发风控 |
| `MAX_GLOBAL_ACTIVE_VREQ` | `1000` | 全局活跃取码任务数上限（超限返回 503 SERVER_BUSY） |
| `MAX_PER_TOKEN_ACTIVE_VREQ` | `50` | 单 Token 活跃取码任务数上限（超限返回 429 TOO_MANY_REQUESTS） |

---

## 3. 优雅停机生命周期 (Graceful Shutdown)

系统严格遵循以下生命周期顺序平稳关闭：
1. **停止接收新请求**：通知 Gin 引擎与服务 Context 取消；
2. **等待后台 Worker 收敛**：
   - `MailSyncWorker` 等待单轮批次拉取完成；
   - `Scheduler` 等待在途补货轮次收敛；
   - `CookieMonitor` 等待单轮校验完成；
3. **关闭长连接池**：释放底层 `mail.Pool` (IMAP) 与 `HMEClientPool` (HTTP/TLS Client)；
4. **关闭持久化存储**：最后关闭 SQLite 数据库句柄，确保无在途脏写与无未落盘数据。

---

## 4. 灰度演练与已知限制

1. **WebMail 模式限制**：
   WebMail 仅具备 thread digest 概要，无法支持基于 UID 的严格单邮件游标。需严格 fresh 取码的业务应为母号配置 App Password 启用 IMAP 模式。
2. **物理删信暂停**：
   服务端对物理删信保持 `400 MAIL_DELETE_UNSUPPORTED` 安全阻断，保护历史邮件事实。
3. **发布演练建议**：
   - 灰度发布单个节点，观察 10 分钟：
     - 检查 SQLite 数据库无锁死（busy handler 自动等待 5s）；
     - 检查 `pool_only` 出号稳定，上游 Apple 调用计数为 0；
     - 检查邮件同步在有订阅者时即时唤醒，无订阅者时静默零请求。
