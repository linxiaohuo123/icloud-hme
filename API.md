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
| `VALIDATION_ERROR` | 400 | 请求体或 Query 参数校验失败 |
| `ALIAS_LIMIT_REACHED` | 400 | 单账号活跃别名已达 Apple 750 上限，网关层自动熔断拦截 |
| `OTP_REQUIRED` | 409 | iCloud 账号登录需要双重认证 (2FA) 验证码 |
| `OTP_INVALID` | 401 | 提交的 2FA 验证码错误或已过期 |
| `RATE_LIMITED` | 429 | 触发安全流控或账号配额超限 (响应含 `Retry-After` 头) |
| `VERIFY_TIMEOUT` | 408 | 验证码长轮询等待超时 (指定时间内未收到目标邮件) |
| `UPSTREAM_UNAUTHORIZED` | 401 | iCloud 上传凭据 (Cookie/Token) 已被 Apple 吊销或失效 |
| `UPSTREAM_FAILURE` | 502 | Apple 网关异常或 IMAP 连接超时拒绝 |
| `PROXY_CHECK_FAILED` | 502 | 代理服务器不可达、认证失败或无法建立外部隧道 |
| `INTERNAL_ERROR` | 500 | 服务端底层存储或系统不可用 |

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
3. **作用域模型 (Scopes)**：

   | 作用域 | 授权范围 | 覆盖端点 |
   | --- | --- | --- |
   | `allocate` | 出号 | `POST /api/create`、`/api/create/batch`、`/api/quick-create`、`/api/alias/lease`、`/api/allocate`、`/api/external/v1/allocate` |
   | `verify` | 取码读信 | `GET /api/verify-code`、`/api/external/v1/verify-code`、`/api/inbox*`、`/api/messages*`、`/api/mailboxes` |
   | `admin` | 全部管理面 | 账号、别名维护、业务标识、令牌、流水、调度、系统设置、代理检测、`POST /api/reload` |

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
   - 任何接口响应**绝对不回显** `cookies`、`app_password`、`proxy` 密文字符串，仅向客户端暴露 `has_cookies`、`has_app_password`、`has_proxy` 等健康布尔标记。
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
      "tags": ["default", "reg_pool_a"],
      "last_validated": "2026-09-20T12:00:00+08:00",
      "status_message": "",
      "created_at": "2026-08-01T09:00:00+08:00"
    }
  ]
}
```

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
*提交后服务端将实时连接 `imap.mail.me.com:993` 发起原子握手验证，验证通过后安全入库。*

### 10. 绑定收件搜索邮箱

```http
PUT /api/accounts/:id/mailbox
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "mailbox": "alias_owner@icloud.com"
}
```

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
- 若 Apple 要求双重认证，接口返回 `409 OTP_REQUIRED`；客户端输入收到的 6 位验证码重新提交带有 `otp_code` 的请求即可。

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
    "account_id": "acc_1"
  }
}
```

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
  - `"pool"`（**默认推荐**）：**号池优先**。毫秒级从后台预先按限速积攒的就绪别名池中原子认领出号；若号池耗尽，则自动无缝降级触发 Apple 实时建号。
  - `"pool_only"`：**严格仅用号池**。仅从预存就绪号池中认领，绝不实时调用 Apple 上游；若号池耗尽立即返回 `503 POOL_EMPTY`（附带 `Retry-After: 60`），保护注册机免受上游风控与阻塞。
  - `"create"`：**强制实时建号**。绕过预存号池，直接调用 Apple 上游 API 创建全新别名（受每小时 5 个配额限制）。

**出号机制与核心优势：**
- **零延迟提取 (~1ms)**：后台定时任务（Schedules/Jobs）在平时按 Apple 5个/小时限制平稳囤号，注册机高峰期直接从号池原子提取，彻底打破 5个/小时的瞬时瓶颈。
- **无需指定 `account_id`**：底层自动化调度引擎结合号池优先策略与 Round-Robin 算法秒级分配。
- **业务标签亲和隔离 (`tag`)**：优先分配打上指定业务标签的专属母号；若无则自动匹配通用号池，严禁跨业务串号。
- **并发原子防重**：底层 SQLite/WAL 事务排他锁 (`ClaimPoolAlias`) 保证千并发抢号绝无并发碰撞或重复出号。
- **自动审计与流水落库**：使用外部接入令牌发起调用时，自动记录该 Token、分配的别名、出号来源 (`pool`/`created`)、业务标签至数据库。
- **突破 750 别名上限**：单号达到 750 限制后虽无法新建，但其存量预置别名仍可自由划入号池被注册机认领。

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
- `source`: 出号来源，`"pool"`（从预存号池秒级认领）或 `"created"`（触发 Apple 上游即时创建）。

### 15. 批量创建别名

```http
POST /api/create/batch
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1",
  "count": 3,
  "note": "批量任务"
}
```
- `count`：1–5（单次请求强制钳制在 5 个以内，规避 Apple 突发风控）。

**响应：**
```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "count": 3,
    "success": 3,
    "emails": [
      "user_alpha@icloud.com",
      "user_beta@icloud.com",
      "user_gamma@icloud.com"
    ],
    "errors": []
  }
}
```

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
      "next_run_at": "2026-09-20T14:30:00+08:00",
      "created_at": "2026-09-20T08:00:00+08:00",
      "updated_at": "2026-09-20T14:20:00+08:00"
    }
  ]
}
```

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
- `alias`：可选，指定别名过滤。底层采用 `To` / `Delivered-To` / `X-Original-To` / `Envelope-To` 四重头与全文深度检索，防止转寄重写漏信。
- `folder`：可选，默认 `all`（并发检索 `INBOX` 与 `Junk` 垃圾箱按时间合并倒序，杜绝验证邮件被分类拦截）；亦可指定单文件夹如 `INBOX`。
- `limit`：可选，1–100，默认 20。
- `days`：可选，1–90，默认 7 天。
- `body`：可选，传入 `1` 或 `true` 时直接在列表结果中**内联返回每封邮件的完整正文** (`body` 和 `content_type`)，避免多次往返调用。

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

#### 方式 B：Prime 包裹风格
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

### 20. 删除邮件

```http
DELETE /api/inbox/1042?account_id=acc_1&folder=INBOX
Authorization: Bearer <API_KEY>
```
- 执行后自动清除该邮件在服务端的内存缓存。

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

### 22. 极速提取验证码与激活链接 (注册机专属长轮询)

```http
GET /api/verify-code?email=target@icloud.com&timeout=30&auto_delete=true
# 兼容外部分销路由：
# GET /api/external/v1/verify-code?email=target@icloud.com&timeout=30&auto_delete=true
Authorization: Bearer <API_KEY> # 或 Bearer <EXTERNAL_TOKEN>
```

**核心工作机制：**
- **零延迟内存事件管道**：通过 `MailEventBus` 纯内存订阅，新邮件到达后**毫秒级推送到阻塞连接**直接返回，无须轮询数据库。
- **智能提取能力**：自动正则提取 4–8 位纯数字验证码与 Magic Link 激活确认链接。
- **`auto_delete=true` 自动闭环回收**：命中并返回验证码后，系统自动在后台异步停用该别名，即刻释放 Apple 750 别名配额，实现无需人工介入的无限循环注册！
- **参数说明**：
  - `email`（必填，亦兼容 `alias`）：待收件的别名地址。
  - `timeout`（可选）：最大挂起秒数，默认 30 秒，上限 120 秒。超时返回 `408 VERIFY_TIMEOUT`。
  - `auto_delete`（可选）：布尔值，默认 `false`。
  - `fresh` 或 `nocache`（可选）：布尔值，默认 `false`。传 `true` 时跳过本地近期缓存，严格等待最新抵达的邮件，适用于平台二次重发验证码场景。

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

---

## 别名维护与管理端点

### 23. 获取账号下别名列表

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
- `account_id`：母账号 ID，传入 `all` 或留空可合并导出系统中所有健康账号的全部别名。
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

### 27. 停用别名

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

### 29. 彻底删除别名

```http
DELETE /api/aliases/:id
Authorization: Bearer <API_KEY>
Content-Type: application/json

{
  "account_id": "acc_1"
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
- `PATCH /api/tags/:id`：更新业务标识名称与标签
- `DELETE /api/tags/:id`：删除业务标识

### 32. 外部接入令牌 (APITokens)

用于向外部下游系统提供免密分销出号接入（**仅 `admin` 作用域可访问**）：

- `GET /api/tokens`：列出所有外部接入令牌。**令牌本体以掩码形式返回**，请以创建响应为准妥善保存。
- `POST /api/tokens`：创建外部令牌。
  - 请求体：`{"name": "下游合作方A", "scopes": "allocate,verify", "token": "am_custom_token"}`
  - `token` 留空时由服务端自动生成高强度随机串（`crypto/rand`）。
  - `scopes` 留空时默认为最小权限 `allocate,verify`；需要管理面能力时显式传 `admin`。
  - 响应 `data` 中一次性返回明文 `token`，之后无法再次读取。
- `DELETE /api/tokens/:id`：销毁吊销该接入令牌

### 33. 已用别名流水审计 (Leases)

记录全站每一个被分配出的别名流水与归属 Token（定时调度产出同样入账，`token_name` 为 `scheduler`）：

- `GET /api/leases?alias=...&tag=...&status=...&limit=20&offset=0`：分页多维检索出号流水与交付状态（`limit` 上限 500）
- `PATCH /api/leases/:id/status`：更新流水状态 `{"status": "completed"}`（仅接受 `completed` / `leased` / `abandoned`）

### 34. 定时调度配置与运行大盘 (Schedules)

- `GET /api/schedule/configs`：列出所有账号的定时调度策略
- `PUT /api/schedule/configs/:account_id`：**部分更新**调度配置，只覆盖请求体中显式出现的字段：
  `enabled`、`hourly_quota`(1-500)、`alias_label`、`mode`(`always`/`daily_window`/`duration`)、`start_time`、`end_time`、`duration_hours`、`started_at`。
  > `current_hour_count` / `last_hour_window` 是服务端配额仲裁状态，**不接受客户端提交**（即使提交也被忽略），避免把已扣减的小时配额回退成旧值。
- `POST /api/schedule/run-now`：立即触发一次补货。
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
| `goroutines` | 数千为正常；持续数万说明有泄漏（派生 goroutine 未退出） |
| `heap_alloc_bytes` | 主要来自别名缓存（账号数 × 别名数）。2000×200 约 150–200 MB |
| `store.leases` | 未配置 `ICLOUD_HME_LEASE_RETENTION` 时会无限增长 |
| `store.alias_routes` | 应接近别名总量；明显偏低说明路由自愈尚未覆盖全部账号 |
| `message_cache_entries` | 上限 1000，接近上限说明详情缓存正在被填满 |
- 自动重置 IMAP 连接池与别名内存缓存，支持外部脚本修改 JSON 文件后零停机生效。

---

## 客户端极速接入示例

### 场景一：注册机极速闭环脚本 (号池秒级出号 + 长期资产留存，推荐标准方案)

```bash
#!/usr/bin/env bash
BASE="http://127.0.0.1:8081"
API_KEY="your_admin_api_key_or_token"

# 1. 秒级认领邮箱 (优先从后台定时囤积的号池中认领，~1ms 零延迟，不触发 Apple 限速)
ALLOC=$(curl -s -X POST "$BASE/api/allocate" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"tag":"reg_pool_a","label":"AutoRegBot","mode":"pool"}')

EMAIL=$(echo "$ALLOC" | jq -r '.data.email')
SOURCE=$(echo "$ALLOC" | jq -r '.data.source')
echo "获取到别名: $EMAIL (出号来源: $SOURCE)"

# 2. 调用目标网站发起注册...
# curl -X POST "https://example.com/register" -d "email=$EMAIL"

# 3. 毫秒级长轮询验证码 (不传 auto_delete，别名作为永久资产留存，后续可随时收邮件/找回密码)
echo "等待验证码到达..."
CODE_RESP=$(curl -s "$BASE/api/external/v1/verify-code?email=$EMAIL&timeout=60" \
  -H "Authorization: Bearer $API_KEY")

CODE=$(echo "$CODE_RESP" | jq -r '.data.code')
MAGIC=$(echo "$CODE_RESP" | jq -r '.data.magic_link')

echo "捕获验证码: $CODE, 激活链接: $MAGIC"
```

> **注意：资产留存 vs 用完即抛**
> - **长期留存（默认/推荐）**：不要在取码请求中传 `auto_delete=true`。别名将永久保留在 iCloud 母号中，日后目标网站重置密码或发通知时随时可在系统内查收邮件。
> - **用完即抛（可选）**：仅当明确不需要该账号且欲释放母号 750 个上限时，才在 `verify-code` 中传入 `auto_delete=true`。

### 场景二：Python 极速自动化封装

```python
import requests

BASE = "http://127.0.0.1:8081"
HEADERS = {"Authorization": "Bearer your_api_key"}

# 1. 优先从号池提取就绪别名 (~1ms，并发无冲突)
resp = requests.post(
    f"{BASE}/api/allocate",
    json={"tag": "reg_pool_a", "label": "PyTask", "mode": "pool"},
    headers=HEADERS
).json()

email = resp["data"]["email"]
source = resp["data"]["source"]
print(f"Allocated: {email} (source: {source})")

# 2. 获取验证码 (长期保留别名资产，不传 auto_delete)
verify = requests.get(
    f"{BASE}/api/verify-code",
    params={"email": email, "timeout": 30},
    headers=HEADERS
).json()

if verify["success"]:
    print(f"Code: {verify['data']['code']}")
```

---

## 风控安全与架构限制

1. **RFC 6265 Set-Cookie 吊销与去重机制**：
   严格遵循 Cookie 标准，识别 `Max-Age<=0` 时自动从内存与存储中淘汰旧会话，防止 Apple WAF 判定 Cookie 冲突重放封禁。
2. **拟人化步长调度 (Human-like Pacing)**：
   自动计划任务通过动态计算当小时剩余时间与剩余额度，将创建请求随机平摊在 8–12 分钟间隔，彻底杜绝整点突发请求的机器特征。
3. **750 别名硬顶熔断保护**：
   单个 Apple ID 活跃别名官方上限为 750 个。网关层在出号与调度时实时核验，触顶时自动故障转移（Failover）至下一个健康账号，并在达到时返回 `400 ALIAS_LIMIT_REACHED`，严禁触碰 Apple 上游错误风控。
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
    因此归属解析是一次主键点查，**不会随账号数增长而退化**；只有在 Apple 侧手工创建、本系统从未见过的别名才会走有上限（20 个账号/轮）+ 负缓存的探测兜底。
