# iCloud Hide My Email API 文档

## 概述

HTTP JSON API，所有接口均采用标准 JSON 格式交互。

### 成功响应

```json
{
  "success": true,
  "data": {}
}
```

### 失败响应

```json
{
  "success": false,
  "code": "VALIDATION_ERROR",
  "message": "参数错误"
}
```

### 稳定错误码清单

| 错误码 | HTTP 状态码 | 含义说明 |
| :--- | :--- | :--- |
| `AUTH_REQUIRED` | 401 | 缺少身份凭证或会话已过期 |
| `INVALID_CREDENTIALS` | 401 | 管理员登录密码错误 |
| `CSRF_INVALID` | 403 | 浏览器 Session 模式下缺少有效 `X-CSRF-Token` |
| `ACCOUNT_NOT_FOUND` | 404 | 指定的 iCloud 账号不存在 |
| `ACCOUNT_IDENTITY_MISMATCH` | 409 | 新 Cookie 或密码登录对应另一 Apple 账号，原账号和库存保持不变 |
| `COOKIE_SAVED_INVALID` | 422 | 新 Cookie 已保存，但 Apple 会话校验失败；账号状态已更新，需刷新状态 |
| `COOKIE_VALIDATION_FAILED` | 422 | 自动登录所得 Cookie 未通过 Apple 校验，原有凭据未改变 |
| `VALIDATION_ERROR` | 400 | 请求体或 Query 参数校验失败 |
| `ALIAS_LIMIT_REACHED` | 400 | 单账号活跃别名已达 Apple 750 上限，网关层自动熔断拦截 |
| `OTP_REQUIRED` | 409 | iCloud 账号登录需要双重认证 (2FA) 验证码 |
| `OTP_INVALID` | 400 | 2FA 验证码必须是 6 位数字 |
| `OTP_EXPIRED` | 400 | 2FA 登录任务已失效或超时 |
| `AUTH_TASK_EXPIRED` | 502 | Camoufox 登录任务已在代理端失效 |
| `CAMOUFOX_TRANSPORT_INVALID` | 503 | 跨主机 Camoufox 通信未使用 HTTPS |
| `CAMOUFOX_UNAVAILABLE` | 503 | 已配置的 Camoufox 代理未通过连通性或浏览器探活检查 |
| `CAMOUFOX_AUTH_FAILED` | 502 | Camoufox 通信令牌无效或无权访问代理 |
| `OTP_REJECTED` | 401 | Apple 拒绝了提交的 2FA 验证码 |
| `RATE_LIMITED` | 429 | 触发安全流控或账号配额超限 (响应含 `Retry-After` 头) |
| `VERIFY_TIMEOUT` | 408 | 验证码长轮询等待超时 (指定时间内未收到目标邮件) |
| `UPSTREAM_UNAUTHORIZED` | 401 | iCloud 上传凭据 (Cookie/Token) 已被 Apple 吊销或失效 |
| `UPSTREAM_FAILURE` | 502 | Apple 网关异常或 IMAP 连接超时拒绝 |
| `PROXY_CHECK_FAILED` | 502 | 代理服务器不可达、认证失败或无法建立外部隧道 |
| `INTERNAL_ERROR` | 500 | 服务端底层存储或系统不可用 |
| `INCOMPLETE_EXPORT` | 502 | 全量别名导出时至少一个账号读取失败，未生成导出文件 |

### 安全与鉴权约定

1. **自动化无状态鉴权（推荐注册机与外部系统使用）**：
   - 配置环境变量 `ICLOUD_HME_API_KEY`（或 `ICLOUD_PRIME_API_TOKEN`）。
   - 在请求头携带 `Authorization: Bearer <API_KEY>` 或 `X-API-Key: <API_KEY>`。
   - **完全无需登录 Cookie，免除一切 CSRF 校验**，专为高并发注册机、微服务无缝调用设计。
   - 该 Key 等同管理员权限，**请勿下发给第三方**；对外发放请改用第 2 条的作用域令牌。
2. **外部接入令牌鉴权（分销与业务隔离）**：
   - 通过中台 `/api/tokens` 签发的令牌（如 `am_xxxxxx`），直接在请求头携带 `Authorization: Bearer <TOKEN>`。
   - 系统会自动记录该 Token 分销的出号流水，用于计量审计。
   - **令牌按作用域授权（最小权限）**，详见下方「作用域模型」。
   - URL 查询参数 `token` / `api_key` 仅在 `GET /mail/code*`、`/mail/view*`、`/mail/raw*`、`/api/mail/code*`、`/api/mail/view*`、`/api/mail/raw*` 和 `GET /api/verify-code` 的已注册路由有效。管理接口、写接口及所有外部 v2 接口必须使用请求头凭据。
   - 主服务访问日志省略查询参数，避免记录直链凭据；反向代理访问日志需采用相同的隐藏策略。
3. **作用域模型 (Scopes)**：

   | 作用域 | 授权范围 | 覆盖端点与限制 |
   | --- | --- | --- |
   | `allocate` | 出号 (仅限库存池) | 外部 v2 接口 `POST /api/external/v2/allocate` 及兼容出号端点 `/api/allocate`、`/api/quick-create`、`/api/alias/lease`、`/api/external/v1/allocate`。<br>**外部令牌严禁指定母号 account_id，严禁现场远程建号 (mode=create)，仅限认领可用库存**。 |
   | `verify` | 关联租约取码 | `POST /api/external/v2/verification-requests`、`GET /api/external/v2/verification-requests/:id`、`GET /api/verify-code`、`/api/external/v1/verify-code`。<br>**仅限提取归属于该令牌的别名验证码；严禁调用管理员 inbox/messages/mailboxes 接口翻看全局邮件**。 |
   | `admin` | 全部管理面 | 现场远程建号 (`POST /api/create`、`/create/batch`)、账号维护、全局收件箱 (`/api/inbox*`、`/api/messages*`、`/api/mailboxes`)、业务标识、令牌管理、流水、调度、系统设置、代理检测、`POST /api/reload`。 |

   - `POST /api/tokens` 未显式传 `scopes` 时，**默认只发放 `allocate,verify`**，即对外令牌无法触达任何管理面接口，也无法读取或收割其它令牌。
   - 需要管理员级令牌时显式传 `"scopes": "admin"`。
   - 升级前已存在的历史令牌自动获得 `admin`，行为不变。
   - 浏览器管理员会话（Cookie）隐式拥有全部作用域。
   - 作用域不足时返回 `403 SCOPE_DENIED`。
4. **Web 控制台鉴权**：
   - 经由 `POST /api/auth/login` 获取会话 Cookie `hme_session`。
   - 会话 Cookie 属性：`Path=/; HttpOnly; SameSite=Strict`；启用 TLS 时自动置为 `Secure`。
   - 浏览器写操作（POST / PUT / PATCH / DELETE）必须在请求头回传 `X-CSRF-Token`。
5. **秘密零外泄原则**：
   - 任何接口响应**均不回显** `cookies`、`app_password`、`proxy` 密文字符串，仅向客户端暴露 `has_cookies`、`has_app_password`、`has_proxy` 等健康布尔标记。
   - `GET /api/tokens` **只回显令牌掩码**（如 `am_1a2b****(35位)`），令牌本体仅在创建响应中出现一次，避免管理台被读取后批量收割令牌。
6. **DNS Rebinding 与本地环回安全默认配置**：
   - 默认绑定 `127.0.0.1:8081`。当监听未授权的外部 IP 时，网关层强制拦截非法公网 Host 探测，防止恶意网站发起 DNS 重绑定内部穿透。
   - 通知 Webhook 等可配置出站地址默认**拒绝环回 / 私有网段 / 链路本地（含 `169.254.169.254` 云元数据）**，阻断盲 SSRF。确有内网自建 Webhook 需求时设置 `ICLOUD_HME_ALLOW_PRIVATE_WEBHOOK=true` 显式放行。

---

## 认证端点

### 1. 登录 (Web 控制台)

```http
POST /api/auth/login
Content-Type: application/json

{"password": "管理员密码"}
```

**成功响应：** 设置 `hme_session` Cookie
```json
{
  "success": true,
  "data": {
    "csrf_token": "a1b2c3d4e5f6...",
    "expires_at": "2026-08-05T22:00:00+08:00"
  }
}
```

### 2. 查询当前会话

```http
GET /api/auth/session
Cookie: hme_session=...
```

**成功响应：**
```json
{
  "success": true,
  "data": {
    "csrf_token": "a1b2c3d4e5f6...",
    "expires_at": "2026-08-05T22:00:00+08:00"
  }
}
```

### 3. 退出登录

```http
POST /api/auth/logout
Cookie: hme_session=...
X-CSRF-Token: <token>
```

**成功响应：** 清除 Cookie
```json
{
  "success": true,
  "data": {
    "logged_out": true
  }
}
```
撤销记录会持久化；若写库失败，接口返回 500 且不会清除 Cookie，客户端应提示重试。
首次升级到持久撤销版本时，旧版管理员 Cookie 会失效，需要重新登录一次。
现有 v2 数据库启动时自动迁移到 v3，创建会话撤销表，并在迁移前生成数据库备份。

---

## 账号管理端点

### 4. 获取账号列表

```http
GET /api/accounts
Authorization: Bearer <API_KEY>
```

**响应：**
```json
{
  "success": true,
  "data": [
    {
      "id": "acc_1",
      "name": "主力一号",
      "real_email": "owner@example.com",
      "icloud_email": "owner@icloud.com",
      "host": "icloud.com",
      "status": "active",
      "alias_total": 85,
      "alias_active": 82,
      "has_cookies": true,
      "has_app_password": true,
      "has_proxy": false,
      "schedule_protected": false,
      "tags": ["default", "reg_pool_a"],
      "last_validated": "2026-09-20T12:00:00+08:00",
      "status_message": "",
      "created_at": "2026-08-01T09:00:00+08:00"
    }
  ]
}
```

`schedule_protected` 是后端根据账号名称和保护标签计算的只读布尔值。为 `true` 时，账号不参与自动、普通手动或全员强制补货；前端据此标记保护状态并排除自动补货统计。修改账号名称或标签后的响应会重新计算该值。

### 5. 添加新账号

```http
POST /api/accounts
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "name": "新号",
  "icloud_email": "newbie@icloud.com",
  "host": "icloud.com",
  "proxy": "socks5://127.0.0.1:10808",
  "cookies": "X-APPLE-WEBAUTH-TOKEN=abc; X-APPLE-WEBAUTH-USER=def"
}
```

- `name`：必填，1-64 字符
- `icloud_email`：必填，合法邮箱格式
- `host`：`icloud.com` 或 `icloud.com.cn` (默认 `icloud.com`)
- `proxy`：可选，支持 `http://`, `https://`, `socks5://`, `socks5h://`, `socks4://`
- `cookies`：可选，支持原始 Cookie 字符串或 JSON 对象格式

### 6. 编辑账号基本信息

```http
PATCH /api/accounts/:id
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "name": "重命名账号",
  "host": "icloud.com.cn"
}
```

### 7. 更新账号出口代理

```http
PUT /api/accounts/:id/proxy
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "proxy": "socks5://user:pass@192.168.1.100:1080"
}
```
*传空字符串 `{"proxy": ""}` 表示解除并清空代理。*

### 8. 更新 iCloud 网页 Cookie

```http
PUT /api/accounts/:id/cookies
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "cookies": "X-APPLE-WEBAUTH-TOKEN=...; X-APPLE-WEBAUTH-USER=..."
}
```
*支持纯文本 Cookie 串或标准 JSON 字典对象。*
*已验证身份的账号只能更新同一 Apple 身份的 Cookie；校验失败时可能返回 `COOKIE_SAVED_INVALID`，此时 Cookie 和错误状态已保存。*

### 9. 设置 IMAP 专用密码 (App Password)

```http
POST /api/accounts/:id/password
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "icloud_email": "owner@icloud.com",
  "app_password": "abcd-efgh-ijkl-mnop"
}
```
*提交后服务端连接 `imap.mail.me.com:993` 验证登录和收件箱访问，预检最多等待 30 秒；验证通过后入库。*

### 10. 绑定收件搜索邮箱

```http
PUT /api/accounts/:id/mailbox
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "provider": "qq",
  "email": "owner@qq.com",
  "imap_host": "imap.qq.com",
  "imap_port": 993,
  "authorization_code": "邮箱 IMAP 授权码"
}
```
服务端验证 IMAP 登录和 `INBOX` 访问，预检最多等待 30 秒。修改已绑定邮箱时，只有邮箱地址、服务器和端口都不变，才可留空 `authorization_code` 以沿用原授权码。此操作不会修改 Apple“隐藏邮箱”的转发目标。

```http
DELETE /api/accounts/:id/mailbox
Authorization: Bearer <API_KEY>
```
解绑后清除本系统的外部 IMAP 配置，读信回到已配置的 iCloud IMAP 或 WebMail；Apple 转发目标不会改变。

### 11. iCloud 账号密码直接登录 (获取 Cookie)

```http
POST /api/accounts/:id/login
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "password": "账号密码",
  "otp_code": "123456"
}
```
- 请求体必须在 `password` 与 `otp_code` 中二选一；`otp_code` 必须是 6 位数字。若 Apple 要求双重认证，接口返回 `409 OTP_REQUIRED`；Camoufox 模式下响应的 `data.task_id` 标识本次登录任务。客户端输入收到的 6 位验证码后，仅提交带有 `otp_code` 的请求即可。
- 未配置 Camoufox URL 和令牌时使用原生 SRP；已配置代理但不可用时返回 `503 CAMOUFOX_UNAVAILABLE`，不会自动切换登录方式。主服务重启后旧 Camoufox 验证码返回 `OTP_EXPIRED`。
- 自动登录所得 Cookie 通过 Apple 会话校验后才会替换账号原有凭据；校验失败返回 `422 COOKIE_VALIDATION_FAILED`。

取消 Camoufox 双重认证任务时，使用 `OTP_REQUIRED` 响应中的任务 ID：

```http
POST /api/accounts/:id/login/cancel
Authorization: Bearer <API_KEY>
Content-Type: application/json

{"task_id":"task_..."}
```

响应 `data.cancelled` 表示该任务的取消请求已被代理接受；若代理不可达，接口返回错误并保留任务以供重试。

### 12. 删除账号

```http
DELETE /api/accounts/:id
Authorization: Bearer <API_KEY>
```

---

## 核心出号与作业端点

### 13. 指定账号单次创建别名

```http
POST /api/create
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1",
  "label": "GitHub注册"
}
```

**响应：**
```json
{
  "success": true,
  "data": {
    "email": "fresh_user_8912@icloud.com",
    "label": "GitHub注册",
    "created_at": "2026-09-20T14:20:10+08:00",
    "account_id": "acc_1",
    "audit_recorded": true
  }
}
```
`audit_recorded=false` 表示 Apple 已创建该别名，但本地出号流水写入失败；不要重复创建，应记录返回的邮箱并排查存储故障。

### 14. 智能一键出号 / 分销分配 (号池优先 / 注册机首选推荐)

```http
POST /api/quick-create
# 兼容别名路由：
# POST /api/alias/lease
# POST /api/allocate
# POST /api/external/v1/allocate
Authorization: Bearer <API_KEY> # 或 Bearer <EXTERNAL_TOKEN>
Content-Type: application/json

{
  "tag": "reg_pool_a",
  "label": "某平台注册任务",
  "mode": "pool"
}
```

**请求参数说明：**
- `tag` (可选，字符串)：指定业务标签。优先分配绑定该标签的母号资产；如未指定则匹配通用号池，杜绝跨业务串号。
- `label` (可选，字符串)：别名备注标签。
- `mode` (可选，字符串，默认 `"pool"`)：
  - `"pool"`（默认）：号池优先。管理员请求在号池耗尽时尝试 Apple 实时建号；普通外部令牌仅能从号池认领，耗尽时返回 `503 POOL_EMPTY`。
  - `"pool_only"`：**严格仅用号池**。仅从预存就绪号池中认领，绝不实时调用 Apple 上游；若号池耗尽立即返回 `503 POOL_EMPTY`（附带 `Retry-After: 60`），保护注册机免受上游风控与阻塞。
  - `"create"`：管理员强制实时建号。绕过预存号池，直接调用 Apple 上游 API 创建全新别名（受账号小时配额限制）。

**出号机制与核心优势：**
- **本地号池预存提取**：后台定时任务（Schedules/Jobs）在平时按 Apple 单账号约 5 个/小时限制平稳补货，注册机高峰期直接从本地号池认领可用库存，有效平抑突发建号瓶颈。
- **无需指定 `account_id`**：底层调度引擎结合号池优先策略与 Round-Robin 算法自动轮询分配。
- **业务标签亲和隔离 (`tag`)**：优先分配打上指定业务标签的专属母号；若无则自动匹配通用号池，严禁跨业务串号。
- **并发原子防重**：底层 `ClaimInventoryAlias` 在 SQLite 事务中认领库存并写入分配与流水，阻止同一别名重复出号。
- **自动审计与流水落库**：使用外部接入令牌发起调用时，自动记录该 Token、分配的别名、出号来源 (`pool`/`created`)、业务标签至数据库。
- **存量资产充分复用**：单号达到 750 上限后虽无法新建，但其存量预置别名仍可划入号池供外部业务认领。

**响应：**
```json
{
  "success": true,
  "data": {
    "email": "auto_hunter_99@icloud.com",
    "account_id": "acc_1",
    "label": "某平台注册任务",
    "tag": "reg_pool_a",
    "source": "pool",
    "created_at": "2026-09-20T14:25:00+08:00"
  }
}
```
- `source`: 出号来源，`"pool"`（从本地预存号池认领）或 `"created"`（触发 Apple 上游即时创建）。

### 15. 批量创建别名

```http
POST /api/create/batch
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1",
  "count": 3,
  "label_prefix": "批量任务"
}
```
- `count`：1–5（单次请求强制钳制在 5 个以内，规避 Apple 突发风控）。

**响应：**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "requested": 3,
    "created_count": 3,
    "skipped_count": 0,
    "created": [
      {"email": "user_alpha@icloud.com", "label": "批量任务 1", "created_at": "2026-09-20T14:20:10Z"},
      {"email": "user_beta@icloud.com", "label": "批量任务 2", "created_at": "2026-09-20T14:20:11Z"},
      {"email": "user_gamma@icloud.com", "label": "批量任务 3", "created_at": "2026-09-20T14:20:12Z"}
    ]
  }
}
```
若某个已创建别名的流水写入失败，响应会额外包含 `audit_failed` 邮箱数组；这些邮箱已经在 Apple 创建，不应重复申请。

### 16. 自动化创建作业生命周期 (Create Jobs)

系统提供一套完整的声明式作业门面，便于外部系统管理自动补货计划：

#### 16.1 查询作业列表
```http
GET /api/create/jobs
Authorization: Bearer <API_KEY>
```
**响应：**
```json
{
  "success": true,
  "data": [
    {
      "id": "job_acc_1",
      "account_id": "acc_1",
      "label_prefix": "AutoBot",
      "mode": "duration",
      "status": "running",
      "duration_hours": 12,
      "created_count": 4,
      "created_at": "2026-09-20T08:00:00+08:00",
      "updated_at": "2026-09-20T14:20:00+08:00"
    }
  ]
}
```

`next_run_at` 暂不返回：执行时刻受调度轮询、时间窗口和配额限制，当前服务无法给出准确预测。任务配置读取或写入失败时接口返回 `500 PERSISTENCE_ERROR`。

#### 16.2 创建或更新作业
```http
POST /api/create/jobs
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1",
  "label_prefix": "AutoBot",
  "mode": "duration",
  "duration_hours": 12
}
```
- `mode` 支持：
  - `"continuous"`：持续均匀补货，每小时平摊创建（受小时安全配额约束）。
  - `"duration"`：指定运行 `duration_hours` 小时后自动完成并收敛停机。
  - `"time_window"`：在 `start_time` 与 `end_time`（格式 `"HH:MM"`）窗口内作业。

#### 16.3 暂停作业
暂停或删除会阻止本轮尚未开始的账号补货；已经向 Apple 发出的建号请求仍可能完成，并计入本小时配额。
```http
POST /api/create/jobs/job_acc_1/pause
Authorization: Bearer <API_KEY>
```
**响应：**
```json
{
  "success": true,
  "data": {
    "id": "job_acc_1",
    "status": "paused"
  }
}
```

#### 16.4 恢复作业
```http
POST /api/create/jobs/job_acc_1/resume
Authorization: Bearer <API_KEY>
```
**响应：**
```json
{
  "success": true,
  "data": {
    "id": "job_acc_1",
    "status": "running"
  }
}
```

#### 16.5 删除 / 重置作业
```http
DELETE /api/create/jobs/job_acc_1
Authorization: Bearer <API_KEY>
```
**响应：**
```json
{
  "success": true,
  "data": {
    "id": "job_acc_1",
    "deleted": true
  }
}
```

---

## 邮件读取与验证码提取端点

### 17. 查询收件箱邮件列表

```http
GET /api/inbox?account_id=acc_1&alias=target@icloud.com&folder=all&limit=20&days=7&body=1
Authorization: Bearer <API_KEY>
```

**参数说明：**
- `account_id`：必填，母账号 ID。
- `alias`：可选，指定别名过滤。IMAP 先按收件人标头搜索，再核验结构化收件人；QQ/网易只搜索标准 `To`，并回扫最近邮件以兼容转寄重写。WebMail 搜索结果也需有可核验的收件人字段。
- `folder`：可选，默认 `all`（顺序检索 `INBOX` 与 `Junk` 垃圾箱，再按时间合并）；亦可指定单文件夹如 `INBOX`。
- `limit`：可选，1–100，默认 20。
- `days`：可选，1–90，默认 7 天。
- `body`：可选，传入 `1` 或 `true` 时拉取匹配邮件正文；默认只返回摘要元数据。WebMail 只提供邮件预览，不保证完整正文。

**响应：**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "target@icloud.com",
    "folder": "all",
    "count": 1,
    "method": "imap",
    "messages": [
      {
        "id": "1042",
        "from": "Service <service@verify.com>",
        "to": "target@icloud.com",
        "subject": "您的注册验证码",
        "date": "2026-09-20T14:35:10+08:00",
        "preview": "您的验证码是 958204，有效期 10 分钟...",
        "folder": "INBOX",
        "unread": true,
        "body": "<html><body>您的验证码是 <b>958204</b></body></html>",
        "content_type": "text/html"
      }
    ]
  }
}
```

### 18. 单封邮件详情读取

提供两种等价且互补的路由格式，适应不同前端与自动化客户端：

WebMail 只返回不完整的邮件预览 (`body_complete=false`)。列表命中的邮件详情会缓存 10 分钟；缓存过期后，超过最新 100 封的 WebMail 邮件可能无法再通过详情接口找到。IMAP 详情不受此限制。

#### 方式 A：扁平风格
```http
GET /api/inbox/1042?account_id=acc_1
Authorization: Bearer <API_KEY>
```
**响应：**
```json
{
  "success": true,
  "data": {
    "id": "1042",
    "folder": "INBOX",
    "from": "service@verify.com",
    "to": "target@icloud.com",
    "subject": "您的注册验证码",
    "date": "2026-09-20T14:35:10+08:00",
    "body": "您的验证码是 958204",
    "content_type": "text/plain",
    "account_id": "acc_1",
    "method": "imap",
    "cached": false
  }
}
```

#### 方式 B：嵌套包裹风格
```http
GET /api/messages/1042?account_id=acc_1
# 也支持复合格式：GET /api/messages/INBOX:1042?account_id=acc_1
Authorization: Bearer <API_KEY>
```
**响应：**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "message": {
      "id": "1042",
      "folder": "INBOX",
      "from": "service@verify.com",
      "to": "target@icloud.com",
      "subject": "您的注册验证码",
      "date": "2026-09-20T14:35:10+08:00",
      "body": "您的验证码是 958204",
      "content_type": "text/plain"
    },
    "method": "imap",
    "cached": false
  }
}
```

### 19. 批量拉取邮件正文 (带 10 分钟缓存)

```http
POST /api/messages
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1",
  "messages": [
    {"folder": "INBOX", "uid": "1042"},
    {"folder": "INBOX", "uid": "1043"}
  ]
}
```
- 单次最多批量拉取 50 封邮件，支持单条 IMAP `UidFetch` 指令批量聚合，自动载入服务端 10 分钟只读内存缓存。
- 单封邮件正文超限、损坏或缺失时，`data.items` 按原请求顺序保留对应的 `error`；成功读取的邮件仍出现在 `data.messages` 中，并且只有成功正文进入缓存。网络、取消等整批失败仍返回错误，不伪装为逐项成功。

### 20. 删除邮件（当前不支持）

```http
DELETE /api/inbox/1042?account_id=acc_1&folder=INBOX
Authorization: Bearer <API_KEY>
```
- 当前接口返回 `400 MAIL_DELETE_UNSUPPORTED`，不会删除邮件或清除缓存。

### 21. 查询邮箱文件夹列表

```http
GET /api/mailboxes?account_id=acc_1
Authorization: Bearer <API_KEY>
```
**响应：**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "folders": [
      {"name": "INBOX", "role": "inbox", "flags": ["\\Seen"]},
      {"name": "Junk", "role": "junk", "flags": []}
    ]
  }
}
```

### 22. 验证码提取与对外直出链接 (开箱即用)

```http
# 方式 1: 程序一行 GET 取验证码 (支持通过 ?token= 传参，免请求头)
GET /api/verify-code?email=target@icloud.com&token=<TOKEN>&timeout=30
# 简写别名：GET /mail/code?email=target@icloud.com&token=<TOKEN>&timeout=30
# 方式 2: 网页可视化查信直链 (浏览器点开即看，沙盒隔离渲染)
# GET /mail/view?email=target@icloud.com&token=<TOKEN>
# 方式 3: 邮件纯正文直链 (Raw 文本输出，支持 &format=html)
# GET /mail/raw?email=target@icloud.com&token=<TOKEN>
# 兼容外部分销路由：
# GET /api/external/v1/verify-code?email=target@icloud.com&timeout=30
Authorization: Bearer <API_KEY> # 或 Bearer <EXTERNAL_TOKEN>
```

**参数说明**：
- `email`（必填，亦兼容 `alias`）：待收件的别名地址。
- `timeout`（可选）：最大挂起秒数，默认 30 秒，上限 120 秒。超时返回 `408 VERIFY_TIMEOUT`。
- 长轮询在交付验证码前再次验证原请求的令牌凭据；令牌轮换、撤销或过期后，在途旧请求返回 `401 REVOKED_TOKEN`，不会消费该验证码事件。
- `fresh` 或 `nocache`（可选）：布尔值，默认 `false`。传 `true` 时仅跳过本地近期内存缓存，**但不是严格的 IMAP 邮件基线保证**。需要严格基线保证的新客户端请使用 v2 端点。
- `auto_delete`：**明确不支持并会被拒绝**。传入 `auto_delete=true` 或 `1` 会直接返回 `400 UNSUPPORTED_PARAMETER` 错误。依据 RFC 9110 规范，HTTP GET 必须具备安全/无副作用语义，严禁通过 GET 查询操作导致别名被隐式停用。
- `/mail/view` 与 `/mail/raw` 支持 IMAP 正文和 WebMail 不完整预览。IMAP 默认查询 INBOX 最近 30 天；WebMail 不承诺按天数筛选，也不保证完整正文。
  - 普通外部令牌使用分配记录中的母号读取邮件；显式 `account_id` / `account` 与分配账号不一致时返回 `404 RESOURCE_NOT_FOUND`，无法通过此参数跨账号读取。
  - 查信页通过内嵌的外部 CSS/JavaScript 资源加载样式与交互，保持全局 CSP 对内联脚本和事件处理器的限制。`/mail/raw?format=html` 使用独立 CSP sandbox，允许内联排版样式，阻断脚本、表单提交、同源存储与外部资源加载。
  - `/mail/raw` 默认返回最新邮件；可传 `message_id` 选择该别名最近 20 封中的邮件，不存在时返回 `404 MESSAGE_NOT_FOUND`。HTML 预览使用 `format=html&frame=1`，仅允许同源页面嵌入，仍保留独立 sandbox 隔离。
  - 查信页分别展示验证码和激活链接；只有激活链接的邮件也会显示打开链接按钮，不显示空验证码复制按钮。

> **关于别名停用与配额说明**：
> - 停用别名必须由具备管理员权限的会话显式调用管理接口 `POST /api/aliases/:id/deactivate`；普通 `allocate,verify` 令牌不具备停用接口权限，且系统不提供外部令牌的租约停用能力。
> - 在 Apple 侧停用别名仅代表停止该别名的邮件转发，**不保证**上游必定释放创建总额度，系统严禁声称可通过停用实现无限循环创建。
> - 项目中的 `MaxAliasesPerAccount`（750）为本系统的内部安全防护与观测阈值，并非 Apple 官方的 SLA 承诺。

**命中成功响应：**
```json
{
  "success": true,
  "data": {
    "email": "target@icloud.com",
    "code": "849201",
    "magic_link": "https://auth.example.com/confirm?token=xyz",
    "subject": "【某平台】您的验证码为 849201",
    "from": "no-reply@example.com",
    "date": "2026-09-20T14:38:00+08:00",
    "account_id": "acc_1"
  }
}
```

### 22.1 推荐方案：v2 规范化出号与权威取码链路 (新客户端首选)

针对注册机、自动化客户端以及高可靠业务系统，推荐使用具备**显式幂等保障、主体资源隔离与严格 IMAP 邮件基线**的 v2 接口套件：

#### 步骤 1：规范化出号认领 (POST /api/external/v2/allocate)
必须携带 `Idempotency-Key` 请求头（防止网络抖动导致的重复出号）：
```http
POST /api/external/v2/allocate
Authorization: Bearer <TOKEN>
Idempotency-Key: task_unique_key_001
Content-Type: application/json

{
  "tag": "default",
  "label": "AutoTask"
}
```
**响应：**
```json
{
  "success": true,
  "data": {
    "lease_id": "alloc_6f8b2a1c...",
    "allocation_id": "alloc_6f8b2a1c...",
    "email": "fresh_alias@icloud.com",
    "alias_email": "fresh_alias@icloud.com",
    "account_id": "acc_1",
    "operation_id": "op_9c72e1...",
    "source": "pool",
    "status": "allocated",
    "allocated_at": "2026-09-23T10:00:00Z"
  }
}
```
> **字段说明**：
> - `lease_id` / `allocation_id`：租约唯一标识（两者值相同，指向同一数据库分配记录，用于后续取码绑定）。
> - `allocated_at`：别名真实租约分配时间戳（RFC3339 UTC 格式）。注：历史 v1 的 `tag` 与 `created_at` 字段在 v2 契约中不再提供。
> - 若相同幂等键传参不一致返回 `409 IDEMPOTENCY_CONFLICT`；底层别名分配出现状态或唯一性冲突返回 `409 ALLOCATION_CONFLICT`。

#### 步骤 2：创建取码意图并锁定邮件基线 (POST /api/external/v2/verification-requests)

配置了原生或外部 IMAP 时，基线读取失败返回 `502 UPSTREAM_FAILURE` 并保留实际 IMAP 故障；只有 Cookie/WebMail 可用的账号返回 `400 CAPABILITY_UNSUPPORTED`。

严格取码同步中，完整邮件正文的超限、MIME 或编码损坏会按账号、文件夹、UIDVALIDITY 与 UID 记录逐项错误，并继续处理其他邮件；失败正文不会完成取码任务。缺失正文、网络错误或结果持久化失败仍保留当前页游标等待重试。

使用出号时返回的 `allocation_id`（即 `lease_id`）建立取码任务。系统将自动原子采集目标母号 IMAP 的最新 `UIDVALIDITY` 与 `UIDNEXT` 基线：
```http
POST /api/external/v2/verification-requests
Authorization: Bearer <TOKEN>
Content-Type: application/json

{
  "lease_id": "alloc_6f8b2a1c..."
}
```
**响应：**
```json
{
  "success": true,
  "data": {
    "request_id": "vreq_8a3d1e4f...",
    "lease_id": "alloc_6f8b2a1c...",
    "alias_email": "fresh_alias@icloud.com",
    "status": "ready",
    "expires_at": "2026-09-23T10:10:00Z"
  }
}
```

#### 步骤 3：在外部目标网站触发发送验证码邮件
调用第三方注册/登录接口向 `fresh_alias@icloud.com` 发送验证码。

#### 步骤 4：长轮询获取验证码 (GET /api/external/v2/verification-requests/:id)

事件消费前与结果交付前均重新验证原请求凭据；等待期间令牌轮换、撤销或过期后返回 `401 TOKEN_REVOKED`。轮换保留任务归属，新凭据可继续查询原任务；消费事件前发现凭据失效时，不会消费该事件。

```http
GET /api/external/v2/verification-requests/vreq_8a3d1e4f...?timeout=30
Authorization: Bearer <TOKEN>
```
**响应：**
```json
{
  "success": true,
  "data": {
    "request_id": "vreq_8a3d1e4f...",
    "lease_id": "alloc_6f8b2a1c...",
    "alias_email": "fresh_alias@icloud.com",
    "status": "succeeded",
    "code": "654321",
    "magic_link": "",
    "message_ref": "ev_msg_1002"
  }
}
```

---

## 别名维护与管理端点

### 23. 获取账号下别名列表

`account_id=all` 或留空时聚合全部账号。响应额外包含 `complete` 和 `failed_accounts`；`complete=false` 时 `count` 仅表示已成功读取的别名数，不能视为全量。

```http
GET /api/aliases?account_id=acc_1
Authorization: Bearer <API_KEY>
```

**响应：**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "count": 1,
    "aliases": [
      {
        "email": "fresh_user_8912@icloud.com",
        "anonymousId": "anon_8912",
        "label": "GitHub注册",
        "note": "测试",
        "active": true,
        "createdAt": "2026-09-20T14:20:10Z"
      }
    ]
  }
}
```

### 24. 导出别名 (CSV / JSON)

```http
GET /api/aliases/export?account_id=all&format=csv
Authorization: Bearer <API_KEY>
```
- `account_id`：母账号 ID，传入 `all` 或留空可合并导出系统中所有账号的别名；任何账号读取失败时返回 `502 INCOMPLETE_EXPORT`，不输出不完整文件。
- `format`：`csv`（默认，带 UTF-8 BOM，Excel 直接双击不乱码）或 `json`。

### 25. 编辑别名标签与备注

```http
PATCH /api/aliases/:id
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1",
  "label": "新标签",
  "note": "追加业务备注"
}
```
- `:id` 为别名的 `anonymousId`。

### 26. 批量更新别名

```http
POST /api/aliases/batch-update
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1",
  "anonymous_ids": ["anon_1", "anon_2"],
  "label": "批量修改标签",
  "note": "批量备注"
}
```
- 单次最多批量修改 100 个别名。
- 响应中的 `succeeded` 和 `failed` 分别列出成功与失败的别名 ID。若 Apple 已处理修改但本地账号会话未同步，响应还会包含 `last_error`；此时应刷新列表核对结果。

### 27. 停用别名

停用、重新激活与删除接口均拒绝非法 JSON 或字段类型错误，返回 `400 VALIDATION_ERROR` 且不执行操作；空请求体仍可通过查询参数 `account_id` 指定账号。

```http
POST /api/aliases/:id/deactivate
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

### 28. 重新激活别名

```http
POST /api/aliases/:id/reactivate
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

### 29. 物理删除别名

```http
DELETE /api/aliases/:id
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1"
}
```

### 29.1 存量别名受控激活入池 (Promote to Pool)

将未被消费过的存量别名（处于 unknown 沉睡状态）受控激活为可用号池库存（available）。严格排除私人大号与受保护账号。

```http
POST /api/aliases/promote-to-pool
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1",  // 可选：指定激活账号；为空表示所有非保护活跃账号
  "limit": 500            // 可选：批次上限；0 或负数表示全部
}
```

**成功响应 (200 OK)**:
```json
{
  "success": true,
  "data": {
    "promoted_count": 242,
    "available_aliases": 496,
    "dormant_aliases": 0
  }
}
```

---

## 网络诊断与代理端点

### 30. 代理连通性测试

```http
POST /api/proxy/check
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "proxy": "socks5://127.0.0.1:10808"
}
```
- 支持协议：`http://`, `https://`, `socks5://`, `socks5h://`, `socks4://`。
- 服务端建立代理握手并向外部权威 IP 端点发起测速探测。

**响应：**
```json
{
  "success": true,
  "data": {
    "ok": true,
    "latency_ms": 128,
    "message": "出口 IP: 104.28.19.22"
  }
}
```

---

## 业务中台治理端点

### 31. 业务标识 (Tags)

用于在母账号池中划分业务领域（如区分不同游戏、不同海外电商渠道）：

- `GET /api/tags`：列出所有业务标识
- `POST /api/tags`：创建业务标识 `{"tag": "reg_pool_a", "name": "注册A组"}`
- `PATCH /api/tags/:id`：部分更新业务标识名称、标签、状态和描述。未提交字段保持现值；`{"description":""}` 清空描述，省略或传 `null` 保持原描述。创建时间和最近活动时间由服务端保留。
- `DELETE /api/tags/:id`：删除业务标识

### 32. 外部接入令牌 (APITokens)

用于向外部下游系统提供免密分销出号接入（**仅 `admin` 作用域可访问**）：

- `GET /api/tokens`：列出所有外部接入令牌。**令牌本体以掩码形式返回**，请以创建响应为准妥善保存。
- `POST /api/tokens`：创建外部令牌。
  - 请求体：`{"name": "下游合作方A", "scopes": "allocate,verify", "expires_in_days": 7}`
  - 服务端自动生成高强度随机串（`crypto/rand`）；非空自定义 `token` 返回 `400 CUSTOM_TOKEN_NOT_ALLOWED`。
  - `expires_in_days` 必须是 JSON 整数；也可使用 RFC3339 字符串 `expires_at` 指定过期时间。非法 JSON 或字段类型错误返回 `400 VALIDATION_ERROR`，不会创建令牌。空请求体保留默认创建行为；不指定过期时间时令牌无到期时间。
  - `scopes` 留空时默认为最小权限 `allocate,verify`；需要管理面能力时显式传 `admin`。
  - 响应 `data` 中一次性返回明文 `token`，之后无法再次读取。
- `DELETE /api/tokens/:id`：销毁吊销该接入令牌

### 33. 已用别名流水审计 (Leases)

记录全站每一个被分配出的别名流水与归属 Token（定时调度产出同样入账，`token_name` 为 `scheduler`）：

- `GET /api/leases?alias=...&tag=...&status=...&limit=20&offset=0`：`alias` 同时搜索别名邮箱和账号 ID，分页检索出号流水与交付状态（`limit` 上限 500；查询故障返回 500）
- `PATCH /api/leases/:id/status`：更新流水状态 `{"status": "completed"}`（仅接受 `completed` / `leased` / `abandoned`）

### 34. 定时调度配置与运行大盘 (Schedules)

- `GET /api/schedule/configs`：列出所有账号的定时调度策略
- 调度配置、业务标识和令牌列表查询遇到数据库错误时返回 `500 PERSISTENCE_ERROR`，不会把故障当作空列表或配额耗尽。
- `PUT /api/schedule/configs/:account_id`：**部分更新**调度配置，只覆盖请求体中显式出现的字段：
  `enabled`、`hourly_quota`(1-500)、`alias_label`、`mode`(`always`/`daily_window`/`duration`)、`start_time`、`end_time`、`duration_hours`、`started_at`。
  - 非空时段必须为 `HH:mm`，非空 `started_at` 必须为 RFC3339，时长不能为负数或超出可表示范围。
  - 启用后合并配置必须有效：`daily_window` 要求完整的开始/结束时间（支持跨午夜）；`duration` 要求正整数时长，启动时间为空时由服务端初始化。
  - 合并与校验在同一配置锁内完成，校验失败返回 `400 VALIDATION_ERROR`，不保存任何字段、不改变已用配额。已有非法配置仍可通过 `{"enabled":false}` 安全暂停。
  - `/api/create/jobs/:id/resume` 使用同一启用校验，不能绕过校验启动尚未填写完整的停用配置。
  - 前端同一账号的保存按操作顺序执行，只提交变化字段并采用响应中的完整配置。轮询等待上一轮结束，保存期间仍可更新日志和运行状态，旧配置响应不能覆盖保存结果；所有保存完成前禁止手动补货。
  - 持续时长到期表示停止自动补货，不等于 `enabled=false`。界面保留真实启用开关，并提供独立的重新计时入口；关闭开关才暂停普通手动补货，全员强制补货仍包含暂停账号。
  > `current_hour_count` / `last_hour_window` 是服务端配额仲裁状态，**不接受客户端提交**（即使提交也被忽略），避免把已扣减的小时配额回退成旧值。
- `POST /api/schedule/run-now`：立即触发一次补货。
  前端触发时淘汰旧状态请求，提交完成后重新读取真实状态。`triggered=true` 表示请求已提交，实际执行结果以运行状态和日志为准；已有轮次执行中时调度器会防重入并记录跳过。
  默认**只跑已启用定时任务的账号**（`scope=enabled_only`）；需要连未启用账号一起强推时传 `?all=true`（`scope=all_accounts`）。
- `GET /api/schedule/logs`：读取最新 100 条环形调度运行日志
- `GET /api/schedule/status`：查询调度器运行概览统计

---

## 系统设置与通知端点

### 35. 读取通知配置

```http
GET /api/settings/notify
Authorization: Bearer <API_KEY>
```

### 36. 保存通知配置

```http
PUT /api/settings/notify
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "feishu_webhook": "https://open.feishu.cn/open-apis/bot/v2/hook/xxx",
  "bark_url": "https://api.day.app/your-device-key",
  "telegram_token": "123456:AAF...",
  "telegram_chat": "123456789",
  "event_kinds": {
    "cookie_expired": true,
    "cookie_recovered": true,
    "quota_low": true
  },
  "quota_threshold": 450,
  "resend_minutes": 360
}
```
- 支持飞书、Bark、Telegram 独立或并发推送。
- `quota_threshold`：当账号活跃别名达到该水位时主动发送告警通知。
- 配置立即保存至 SQLite settings 表，全自动热加载生效。

### 37. 发送测试通知

```http
POST /api/settings/notify/test
Authorization: Bearer <API_KEY>
```
**响应：**
```json
{
  "success": true,
  "data": {
    "results": [
      {"channel": "feishu", "ok": true},
      {"channel": "bark", "ok": true}
    ]
  }
}
```

---

## 系统运维端点

### 38. 磁盘配置热重载
```http
POST /api/reload
Authorization: Bearer <API_KEY>
```
- 重新解析加载磁盘上的 `data/accounts.json`。
- 自动重置 IMAP 连接池与别名内存缓存，支持外部脚本修改 JSON 文件后热重载生效。

### 39. 运行时可观测性水位 (System Stats)

```http
GET /api/system/stats
Authorization: Bearer <API_KEY>
```

**仅 `admin` 作用域可访问**（受限令牌返回 `403 SCOPE_DENIED`）。用于在几千账号规模下判断容量与水线，刻意零依赖（不引入 Prometheus 客户端），需要时序采集时在外层接 exporter。

```json
{
  "success": true,
  "data": {
    "uptime_seconds": 86400,
    "goroutines": 42,
    "process": {
      "heap_alloc_bytes": 52428800,
      "heap_sys_bytes": 134217728,
      "gc_cycles": 812,
      "gc_pause_total_ns": 15234000
    },
    "store": {
      "accounts": 2000,
      "alias_routes": 400000,
      "leases": 1850000,
      "db_size_bytes": 268435456
    },
    "alias_pool": {
      "total_active_aliases": 535,
      "consumed_aliases": 34,
      "available_aliases": 254,
      "dormant_aliases": 242,
      "apple_quota_remaining": 215
    },
    "message_cache_entries": 137,
    "engines": {
      "mail_subscribers": false,
      "cookie_monitor_interval": "30m0s",
      "scheduler": { "running": false, "interval_seconds": 300, "last_run_at": "2026-09-20T23:40:00+08:00" },
      "lease_pruner": { "enabled": true, "retention": "4320h0m0s", "recent_logs": ["23:45:01 已清理 12000 条…"] }
    }
  }
}
```

**运维关注点**：

| 指标 | 水位含义 |
| --- | --- |
| `alias_pool.available_aliases` | **核心号池就绪水位**：外部注册机当前可立即领取的空闲别名总数 |
| `alias_pool.dormant_aliases` | **沉睡资产数**：处于 unknown 且干净未消费的别名数，可随时调 `promote-to-pool` 激活入池 |
| `alias_pool.total_active_aliases` | 所有活跃账号在 Apple 远端处于激活状态的别名总数 |
| `alias_pool.consumed_aliases` | 历史已被外部注册机或业务领用消费的别名总数 |
| `alias_pool.apple_quota_remaining` | 距离 Apple 单母号 750 物理上限剩余可现场新建额度（已自动排除私人大号） |
| `goroutines` | 数千为正常；持续数万说明有泄漏（派生 goroutine 未退出） |
| `heap_alloc_bytes` | 主要来自别名缓存（账号数 × 别名数）。2000×200 约 150–200 MB |
| `store.leases` | 未配置 `ICLOUD_HME_LEASE_RETENTION` 时会持续累积增长 |
| `store.alias_routes` | 应接近别名总量；明显偏低说明路由自愈尚未覆盖全部账号 |
| `message_cache_entries` | 上限 1000，接近上限说明详情缓存正在被填满 |

---

## 客户端接入示例

### 场景一：注册机接入脚本 (v2 规范化链路，推荐标准方案)

```bash
#!/usr/bin/env bash
BASE="http://127.0.0.1:8081"
TOKEN="your_external_token_here"
IDEMP_KEY="task_$(date +%s)_$RANDOM"

# 1. 规范化出号认领 (显式幂等键，直接从号池认领就绪资产)
ALLOC=$(curl -s -X POST "$BASE/api/external/v2/allocate" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $IDEMP_KEY" \
  -H "Content-Type: application/json" \
  -d '{"tag":"default","label":"AutoRegBot"}')

EMAIL=$(echo "$ALLOC" | jq -r '.data.email')
LEASE_ID=$(echo "$ALLOC" | jq -r '.data.allocation_id')
echo "获取到别名: $EMAIL (租约 ID: $LEASE_ID)"

# 2. 建立持久化取码意图 (锁定 IMAP 邮件基线)
VREQ=$(curl -s -X POST "$BASE/api/external/v2/verification-requests" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"lease_id\":\"$LEASE_ID\"}")

VREQ_ID=$(echo "$VREQ" | jq -r '.data.request_id')
echo "已建立取码任务: $VREQ_ID (基线就绪)"

# 3. 调用目标网站发起注册 / 请求发送验证码...
# curl -X POST "https://example.com/register" -d "email=$EMAIL"

# 4. 长轮询等待验证码 (基于基线等待新邮件到达，无历史旧邮件串扰)
echo "等待验证码到达..."
RESULT=$(curl -s "$BASE/api/external/v2/verification-requests/$VREQ_ID?timeout=60" \
  -H "Authorization: Bearer $TOKEN")

CODE=$(echo "$RESULT" | jq -r '.data.code')
MAGIC=$(echo "$RESULT" | jq -r '.data.magic_link')
echo "捕获验证码: $CODE, 激活链接: $MAGIC"
```

---

## 外部自动化 API (v2 规范化契约)

专为外部自动化客户端与多主体调用设计的规范化接口：

### 1. 幂等出号: `POST /api/external/v2/allocate`
- **请求头**：
  - `Authorization: Bearer <TOKEN>` (必填)
  - `Idempotency-Key: <UUID>` (外部令牌**强制必填**，管理员可选)
- **请求体 JSON**：
  ```json
  {
    "tag": "default",
    "label": "reg_task_1",
    "mode": "pool"
  }
  ```
  - `mode`: `"pool"` (默认，号池优先；管理员空池时可实时建号)、`"pool_only"` (严格仅号池)、`"create"` (管理员强制实时建号)。普通外部令牌严禁指定 `"create"`，空池时不会实时建号；
  - `tag`: 业务标签，必须在令牌允许的 `allowed_tags` 内；
  - `account_id`: 外部令牌**严禁指定母号** (防跨号定向盗领)。
- **请求指纹规范 (F04)**：
  系统使用包含 `version: 2, tag, account_id, label, mode` 的规范结构体计算 SHA-256 指纹 (`v2:<hex>`)，杜绝拼接碰撞与 mode 歧义。历史旧格式自动向下兼容回放。
- **响应状态码与格式**：
  - **200 OK**：出号成功 (或同键同参数成功回放)
    ```json
    {
      "success": true,
      "data": {
        "operation_id": "op_xxxxxx",
        "lease_id": "alloc_xxxxxx",
        "allocation_id": "alloc_xxxxxx",
        "email": "alias@icloud.com",
        "alias_email": "alias@icloud.com",
        "account_id": "acc_xxxxxx",
        "source": "pool",
        "allocated_at": "2026-09-24T00:00:00Z",
        "status": "allocated"
      }
    }
    ```
  - **202 Accepted (Pending)**：操作处理中
    ```json
    {
      "success": true,
      "data": {
        "operation_id": "op_xxxxxx",
        "status": "pending"
      }
    }
    ```
  - **409 Conflict (`IDEMPOTENCY_CONFLICT`)**：
    相同 `Idempotency-Key` 使用了不同的请求参数（包括 tag、label、mode 等），严格拒绝。
  - **503 Service Unavailable (`POOL_EMPTY`)**：
    库存池暂无可用别名，响应头包含 `Retry-After: 60`。
    **【池空同键重试契约 (F05)】**：池空时操作记录为可证明无分配副作用的失败。当下游客户端在等待 `Retry-After` 后（或管理员补货后），**允许且推荐使用原相同 Idempotency-Key 进行重试**，补货后重试将原子转入成功分配并返回 200 OK。

### 2. 操作状态查询: `GET /api/external/v2/operations/:operation_id`
- **请求头**：`Authorization: Bearer <TOKEN>` (仅允许操作归属主体查询)
- **无幽灵 ID 保证 (F06)**：所有对外发放的 `operation_id`（包括有键与无键请求）均已在底层 `operations` 表真实持久化，杜绝 404 幽灵记录。
- **响应示例** (200 OK)：
  ```json
  {
    "success": true,
    "data": {
      "operation_id": "op_xxxxxx",
      "principal_kind": "token",
      "principal_id": "tok_xxxxxx",
      "operation_kind": "v2_allocate",
      "idempotency_key": "uuid_key",
      "state": "succeeded",
      "candidate_email": "alias@icloud.com",
      "result_ref": "alloc_xxxxxx",
      "created_at": "2026-09-24T00:00:00Z",
      "updated_at": "2026-09-24T00:00:00Z"
    }
  }
  ```

### 场景二：Python 自动化封装示例 (v2 规范化链路)

```python
import uuid
import time
import requests

BASE = "http://127.0.0.1:8081"
HEADERS = {"Authorization": "Bearer your_token_here"}
TIMEOUT = 10  # 严禁无超时请求

def allocate_alias(tag="default", label="PyTask"):
    idemp_key = str(uuid.uuid4())
    url = f"{BASE}/api/external/v2/allocate"
    payload = {"tag": tag, "label": label, "mode": "pool"}
    
    while True:
        resp = requests.post(url, headers={**HEADERS, "Idempotency-Key": idemp_key}, json=payload, timeout=TIMEOUT)
        
        # 1. 成功分配
        if resp.status_code == 200:
            data = resp.json().get("data", {})
            return data["email"], data["allocation_id"]
        
        # 2. 处理中 (Pending)
        if resp.status_code == 202:
            op_id = resp.json().get("data", {}).get("operation_id")
            time.sleep(1)
            continue
        
        # 3. 池空临时不可用 (遵循 Retry-After 同键重试契约)
        if resp.status_code == 503 and resp.json().get("code") == "POOL_EMPTY":
            retry_after = int(resp.headers.get("Retry-After", 60))
            print(f"库存池为空，等待 {retry_after} 秒后同键重试...")
            time.sleep(min(retry_after, 5))  # 示例演示等待
            continue
            
        # 4. 其他不可重试错误 (如 409 参数冲突, 403 权限不足)
        resp.raise_for_status()

# 1. 幂等出号
email, lease_id = allocate_alias(tag="default", label="PyTask")
print(f"Allocated: {email} (lease: {lease_id})")

# 2. 创建取码意图并锁定 IMAP 基线
vreq_resp = requests.post(
    f"{BASE}/api/external/v2/verification-requests",
    headers=HEADERS,
    json={"lease_id": lease_id},
    timeout=TIMEOUT,
).json()
vreq_id = vreq_resp["data"]["request_id"]
print(f"Verification request ready: {vreq_id}")

# 3. 目标网站触发发信...

# 4. 获取验证码 (支持长轮询 timeout)
result = requests.get(
    f"{BASE}/api/external/v2/verification-requests/{vreq_id}",
    headers=HEADERS,
    params={"timeout": 30},
    timeout=35,
).json()

if result.get("success"):
    print(f"Code: {result['data']['code']}")
```

---

## 健康检查探针

为部署与容器环境提供无敏感数据的轻量级健康探测端点：

### 1. 存活探针: `GET /livez`
- **认证**：无需认证，公开访问
- **职责**：仅报告进程存活状态，无存储与磁盘 I/O，不访问底层数据库，严禁触碰外部 Apple 接口
- **成功响应** (200 OK)：
  ```json
  {"status": "ok"}
  ```

### 2. 就绪探针: `GET /readyz`
- **认证**：无需认证，公开访问
- **职责**：报告核心存储 (SQLite) 连通性，严格 2 秒超时与取消保护，解耦业务大锁，严禁触碰外部 Apple 接口，不泄露系统配置与凭据
- **成功响应** (200 OK)：
  ```json
  {"status": "ok"}
  ```
- **失败响应** (503 Service Unavailable)：
  ```json
  {"status": "unavailable", "error": "store ping failed"}
  ```

### 3. 端点状态报告与部署平台动作说明
- **端点只负责状态报告**：`/livez` 与 `/readyz` 仅如实反映当前进程与存储健康状态，本身不执行进程终止或流量重定向。
- **与部署平台解耦**：
  - **Docker / Compose**：Compose 的 `healthcheck` 测试探针若失败，Docker 仅将容器元数据标记为 `unhealthy`，**默认 `restart: unless-stopped` 策略不会因 unhealthy 自动重启容器**，亦不原生具备反代流量摘除能力；
  - **生产反代 / 编排器**：若需自动剔除故障节点或重启，需在反向代理（如 Nginx `upstream` 探活）或容器编排层中，将 `/readyz` 作为流量切离判据、将 `/livez` 作为容器重启判据；
  - **架构边界**：单体部署保留标准端点支持即可，**无需额外引入 Kubernetes 或复杂微服务基础设施**。

---

## 风控安全与架构限制

1. **RFC 6265 Set-Cookie 吊销与去重机制**：
   严格遵循 Cookie 标准，识别 `Max-Age<=0` 时自动从内存与存储中淘汰旧会话，防止 Apple WAF 判定 Cookie 冲突重放封禁。
2. **拟人化步长调度 (Human-like Pacing)**：
   自动计划任务通过动态计算当小时剩余时间与剩余额度，将创建请求随机平摊在 8–12 分钟间隔，避免整点突发请求暴露机器特征。
3. **750 别名硬顶熔断保护**：
   项目内置常量 `MaxAliasesPerAccount`（750）为本系统依据逆向观测与平台行为设定的安全防护阈值（非 Apple 官方 SLA 承诺）。网关层在出号与调度时实时核验，触顶时自动故障转移（Failover）至下一个健康账号，并在达到时返回 `400 ALIAS_LIMIT_REACHED`，严禁触碰 Apple 上游错误风控。
4. **SQLite WAL 原子配额仲裁锁**：
   单账号创建入口（单建、批量、一键出号、定时补货）由底层数据库执行强一致原子配额计数，超限直接返回 `429 RATE_LIMITED` 并附加 `Retry-After: 3600` 秒头。
5. **双向出口 IP 隧道对齐**：
   配置代理后，Web API 与 IMAP 握手长连接统一走同一代理节点出网，避免双 IP 出口分裂被风控标记。
6. **DNS Rebinding 免疫防御**：
   严格校验 Host 请求头与监听绑定网卡，公网环境未配置强鉴权密钥时拒绝跨网段暴露，消除本地特权逃逸。
7. **最小权限令牌与作用域隔离**：
   对外发放的令牌默认只有 `allocate,verify`，无法触达账号、令牌、设置等管理面，亦无法通过 `GET /api/tokens` 收割其它令牌（该接口仅回显掩码）。
8. **内网出站防护**：
   通知 Webhook 等可配置出站地址默认拒绝环回、私有网段与链路本地地址（含 `169.254.169.254`），消除盲 SSRF；如需内网自建 Webhook，设置 `ICLOUD_HME_ALLOW_PRIVATE_WEBHOOK=true`。
9. **凭据文件权限**：
   数据目录以 `0700` 创建，SQLite 库及其 WAL/SHM 边车文件收紧为 `0600`。**Windows 部署需自行收紧 ACL**（`os.Chmod` 在 Windows 上只映射只读位）。
10. **启动口令强校验**：
    管理员口令除长度下限外，还会拒绝仓库模板中的公开占位值（如 `your_strong_password_here`、`admin123456`），命中即拒绝启动。
11. **别名归属路由表 (`alias_routes`) 与取码链路**：
    取验证码时必须先确定别名属于哪个母号。系统维护一张 `alias_routes(email → account_id)` 路由表：
    - **出号写穿**：每次分配别名时同步登记归属；
    - **列表自愈**：任何一次拉取到账号别名列表（启动预热、GUI 浏览、手动刷新）都会批量登记该账号全部别名；
    - **一次性回填**：升级后首次启动会用历史出号流水回填路由表（带标记，不会重复全表扫描）。
    已登记别名通过路由表定向查找；只有在 Apple 侧手工创建、本系统从未见过的别名才会轮转探测可用账号。未知别名探测每轮总计最多发起 20 次账号查询，最多 5 个并发；监听仍活跃时，后续轮次继续探测，不缓存未命中结果。


### 浏览器会话诊断字段

账号 Summary 的可选 `session` 对象仅用于脱敏诊断：`trusted`、`captured_at`（Unix 秒）、`recovery_available`、`recovery_blocked`、`recovery_after`（Unix 秒）和 `token_expires_at`（Unix 秒或 null）。`token_expires_at` 仅统计适用于当前区域 setup 请求的未过期核心 Cookie；任一适用 Cookie 期限未知或无适用 Cookie 时返回 null，不混用其他域或路径的期限。旧 Cookie 导入账号不包含此对象。`trusted` 是最近确认的状态，`recovery_available` 只表示存在恢复材料；二者都不保证未来有效性。接口不会返回 Cookie 列表或认证 token。

自动恢复被临时冷却时返回 `SESSION_RECOVERY_DEFERRED`（503）；Apple 要求重新验证时返回 `SESSION_REAUTH_REQUIRED`（401），需要重新发起登录。恢复不会自动重放写操作。更新主服务时必须同步更新 Camoufox Agent；旧 Agent 缺少完整会话时会明确报错。
