# iCloud Hide My Email 本地管理工具

[English](#english) | 中文

通过逆向 iCloud Web 接口和 IMAP 邮件协议，实现 Apple iCloud 隐藏邮箱别名的创建、列出和邮件收取功能。内置中文管理界面（React 单页应用，随二进制内嵌分发）。

## 功能特性

- ✅ **中文管理界面** — 浏览器访问 `http://localhost:8081` 即开即用
- ✅ **创建 HME 别名** — 自动生成 iCloud 隐藏邮箱地址
- ✅ **列出所有别名** — 查看账号下的所有 HME 别名
- ✅ **收取邮件** — 通过 IMAP 或 Web API 读取发到 HME 别名的邮件
- ✅ **双路径读信** — 邮件读取优先走 IMAP (App Password),无 App Password 时回退 Web API (Cookie)
- ✅ **多账号管理** — 支持多个 iCloud 账号并行管理
- ✅ **双认证模式** — Cookie (创建别名 + 读邮件回退) 和 App Password (IMAP 优先)
- ✅ **Cookie 健康监控** — 后台周期校验账号会话，凭据失效自动标记 error 并让调度器跳过，杜绝出号链路静默停摆
- ✅ **系统设置 + 通知推送** — 管理台「系统设置」页配置飞书/Bark/Telegram 渠道，Cookie 失效/恢复、配额水位告警实时送达手机与 IM
- ✅ **安全模型** — 单管理员会话、CSRF 校验、登录限流、响应脱敏

## 快速开始

### 1. 安装

#### 方式一：下载二进制发布版（推荐）

从 [GitHub Releases](https://github.com/linxiaohuo123/icloud-hme/releases) 下载对应平台的二进制文件：

| 平台 | 文件 |
|---|---|
| Linux x86_64 | `icloud-hme_linux_amd64` |
| Linux ARM64 | `icloud-hme_linux_arm64` |
| macOS Intel | `icloud-hme_darwin_amd64` |
| macOS Apple Silicon | `icloud-hme_darwin_arm64` |
| Windows x86_64 | `icloud-hme_windows_amd64.exe` |

```bash
# 示例：Linux 下直接运行（必须设置管理员密码与凭据主密钥）
export ICLOUD_HME_ADMIN_PASSWORD='change-this-before-running-2026'
export ICLOUD_HME_MASTER_KEY='$(openssl rand -base64 32)'
chmod +x icloud-hme_linux_amd64
./icloud-hme_linux_amd64
```

#### 方式二：Docker

```bash
# 拉取镜像
docker pull ghcr.io/linxiaohuo123/icloud-hme:latest

# 运行（将本机 data 目录挂载进去）
docker run -d \
  --name icloud-hme \
  -p 8081:8081 \
  -v /path/to/data:/app/data \
  -e ICLOUD_HME_ADMIN_PASSWORD='change-this-before-running-2026' \
  -e ICLOUD_HME_MASTER_KEY='your-32-byte-base64-master-key' \
  ghcr.io/linxiaohuo123/icloud-hme:latest
```

> ⚠️ 上面的密码与密钥仅为示例，**不可照抄**。密码请设为至少 8 字符的强密码，Master Key 必须为严格 32 字节的 Base64 字符串（可通过 `openssl rand -base64 32` 生成）。

镜像支持 `linux/amd64` 和 `linux/arm64` 双架构，自动适配。

#### 方式三：源码编译（需要 Go 1.26+ 与 Node.js 22.12+ 双工具链）

```bash
# 前置要求: Go 1.26+、Node.js 22.12+
git clone https://github.com/linxiaohuo123/icloud-hme.git
cd icloud-hme

# 一键构建（安装前端依赖 → 前端测试 → 前端构建 → Go 测试 → 编译）
./build.sh

# 或者手动分步构建
npm --prefix web ci
npm --prefix web run build
go build -o icloud-hme .
```

### 2. 安全配置（必读）

管理界面与 API 均需要管理员登录，升级后所有 API 都必须先通过 `POST /api/auth/login` 获取会话：

| 环境变量 | 说明 | 默认 |
|---|---|---|
| `ICLOUD_HME_ADMIN_PASSWORD` | 管理员密码，**必填**，至少 8 字符，且不得使用仓库模板里的占位值 | 无（缺失时拒绝启动） |
| `ICLOUD_HME_MASTER_KEY` | 凭据根密钥，**必填**，严格 32 字节 Base64 编码，用于加密凭据 | 无（缺失时拒绝启动） |
| `ICLOUD_HME_MASTER_KEY_FILE` | 凭据根密钥文件路径（支持 Docker Secret / systemd credentials） | 无 |
| `ICLOUD_HME_API_KEY` | 自动化 API Key，**等同管理员权限**，请勿下发给第三方 | 无（不启用） |
| `ICLOUD_HME_ADDR` | HTTP 监听地址 | `127.0.0.1:8081`（仅本机） |
| `ICLOUD_HME_SESSION_TTL` | 会话有效期 | `12h`（范围 `15m`–`168h`） |
| `ICLOUD_HME_SECURE_COOKIE` | 通过 TLS 反向代理部署时设为 `true` | `false` |
| `ICLOUD_HME_COOKIE_MONITOR_INTERVAL` | Cookie 健康监控周期 | `30m`（范围 `5m`–`24h`） |
| `ICLOUD_HME_COOKIE_THROTTLE` | Cookie 校验的账号间节流；留空自动摊平到监控周期内 | 自动（范围 `50ms`–`5m`） |
| `ICLOUD_HME_STARTUP_SYNC_INTERVAL` | 启动预热账号间提交间隔；留空自动摊平到约 10 分钟 | 自动（范围 `50ms`–`5m`） |
| `ICLOUD_HME_MAIL_POLL_INTERVAL` | 取码长轮询期间的邮件轮询周期 | `2s`（范围 `500ms`–`1m`） |
| `ICLOUD_HME_LEASE_RETENTION` | 已用别名流水保留期，支持 `180d` 写法；清理不影响别名路由表 | 永久保留 |
| `ICLOUD_HME_ALLOW_PRIVATE_WEBHOOK` | 允许通知 Webhook 指向内网地址 | `false`（默认拒绝内网，防盲 SSRF） |

> **Breaking Change**：未设置 `ICLOUD_HME_ADMIN_PASSWORD` 或 `ICLOUD_HME_MASTER_KEY` 将拒绝启动；
> 原有匿名 API 调用将收到 `401 AUTH_REQUIRED`。管理员会话只存内存，进程重启即失效。

> **上线前四条硬性安全检查**
>
> 1. **口令必须替换**。`your_strong_password_here`、`admin123456` 等模板占位值会被启动校验直接拒绝。
> 2. **Master Key 必须安全备份**。V2 SQLite 中：Apple Cookies、App Password、Mailbox credentials、Proxy credential、Notify secrets 使用 AES-256-GCM 认证加密存储；API Token 只保存不可逆哈希；Master Key 独立于数据库保存。**Master Key 丢失后将无法恢复任何已加密凭据！**
> 3. **历史备份安全处置**。pre-V2 / pre-migration 的历史备份文件可能包含明文凭据，必须安全保管，生产升级稳定确认无误后加密归档或安全销毁。
> 4. **必须由 TLS 反代暴露**。程序自身不提供 HTTPS；直接以 HTTP 暴露到公网时，管理员口令与会话 Cookie 均为明文传输。
>    建议保持 `ICLOUD_HME_ADDR=127.0.0.1:8081` 并用 Nginx/Caddy 终止 TLS，同时设置 `ICLOUD_HME_SECURE_COOKIE=true`。
>    程序会以 `0700` 创建目录、`0600` 收紧 SQLite 及其 WAL/SHM 文件；**Windows 部署需自行收紧目录 ACL**。

> **对外分发令牌请用作用域**：`GET /api/tokens` 仅回显掩码，`POST /api/tokens` 未指定 `scopes` 时默认只发放
> `allocate,verify`（出号 + 取码），这类令牌**无法触达账号/令牌/设置等管理面**。详见 `API.md` 的「作用域模型」。

### 3. 配置账号

在程序 `data/` 目录下创建 `accounts.json`（参考仓库内 `accounts.json.template`）：

```json
{
  "accounts": {
    "acc_1": {
      "id": "acc_1",
      "name": "主号",
      "real_email": "owner@example.com",
      "icloud_email": "owner@icloud.com",
      "cookies": {
        "X-APPLE-WEBAUTH-TOKEN": "token_value",
        "X-APPLE-WEBAUTH-USER": "v=1:s=1:d=22789132008"
      },
      "host": "icloud.com",
      "proxy": "http://user:pass@host:port",
      "app_password": "xxxx-xxxx-xxxx-xxxx",
      "status": "active"
    }
  }
}
```

> **提示:** 也可以通过管理界面的「账号」页面动态添加账号，无需手动编辑 JSON 文件。`cookies`、`app_password`、`proxy` 都是可选的。

### 4. 启动服务

```bash
# 二进制方式（默认 data 目录）
export ICLOUD_HME_ADMIN_PASSWORD='your-strong-password'
./icloud-hme_linux_amd64

# 指定端口和数据目录
./icloud-hme_linux_amd64 -addr :9090 -data ./my_data

# 调试模式（启用请求日志）
./icloud-hme_linux_amd64 -debug

# 查看完整参数
./icloud-hme_linux_amd64 -h
```

服务默认监听 `:8081`。浏览器打开 `http://localhost:8081` 进入管理界面（账号 / 别名 / 收件箱）。完整 API 契约见 [API.md](API.md)。

## API 接口

> **认证**：除 `POST /api/auth/login` 与 `GET /api/auth/session` 外，所有 `/api` 接口都需要管理员会话 Cookie（`hme_session`）；非 GET/HEAD/OPTIONS 请求还需携带 `X-CSRF-Token` 请求头。完整契约与 curl 示例见 [API.md](API.md)。

### 核心接口

#### 创建 HME 别名

```bash
POST /api/create

# 请求体
{
  "account_id": "acc_1",      # 必填: 账号 ID
  "label": "注册某网站"        # 可选: 别名标签
}

# 响应
{
  "success": true,
  "data": {
    "email": "xyz123@icloud.com",
    "label": "注册某网站",
    "created_at": "2024-01-15T10:30:00Z",
    "account_id": "acc_1"
  }
}
```

#### 读取邮件

```bash
GET /api/inbox?account_id=acc_1&alias=xyz123@icloud.com&limit=20&days=7

# 参数说明:
#   account_id - 必填: 账号 ID
#   alias      - 可选: 只读取发到该别名的邮件
#   limit      - 可选: 返回邮件数量 (默认 20)
#   days       - 可选: 查找最近几天的邮件 (默认 7,仅 IMAP 模式)

# 响应
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "count": 2,
    "method": "imap",
    "messages": [
      {
        "id": "1042",
        "from": "noreply@example.com",
        "to": "xyz123@icloud.com",
        "subject": "欢迎注册",
        "preview": "感谢您的注册...",
        "date": "2026-07-09T14:32:10+08:00"
      }
    ]
  }
}

# 读取方式 (自动选择):
#   method: "imap"    — 通过 App Password 认证 (优先)
#   method: "web_api" — 通过 Cookie 认证,无需 App Password (回退)
```

### 账号管理接口

#### 列出所有账号

```bash
GET /api/accounts

# 响应
{
  "success": true,
  "data": [
    {"id": "acc_1", "name": "主号"},
    {"id": "acc_2", "name": "副号"}
  ]
}
```

#### 添加账号

**简化版（cookies 可选）:**

```bash
POST /api/accounts

# 请求体
{
  "name": "新账号",
  "host": "icloud.com",           # 可选
  "proxy": "http://..."           # 可选
}

# 响应 - 状态为 pending,需登录
{
  "success": true,
  "data": {
    "id": "acc_xxx",
    "name": "新账号",
    "status": "pending"
  }
}
```

**完整版（带 Cookie）:**

```bash
POST /api/accounts

# 请求体
{
  "name": "新账号",
  "cookies": "{\"x-apple-session-token\":\"token_value\"}",  # JSON 或 Header 格式
  "host": "icloud.com",           # 可选
  "proxy": "http://..."           # 可选
}

# 响应
{
  "success": true,
  "data": {
    "id": "acc_3",
    "name": "新账号",
    "status": "active"
  }
}
```

#### 账号登录（获取 Cookie）

```bash
POST /api/accounts/:id/login

# 请求体
{
  "password": "用户的常规iCloud密码",  # 不是 App Password
  "otp_code": "123456"                  # 可选,2FA 验证码
}

# 响应
{
  "success": true,
  "data": {
    "id": "acc_1",
    "cookies": {
      "x-apple-session-token": "...",
      "X-APPLE-WEBAUTH-TOKEN": "..."
    }
  }
}
```

#### 删除账号

```bash
DELETE /api/accounts/:id

# 响应
{
  "success": true,
  "data": {"id": "acc_3"}
}
```

#### 设置 App Password

```bash
POST /api/accounts/:id/password

# 请求体
{
  "icloud_email": "your_email@icloud.com",
  "app_password": "xxxx-xxxx-xxxx-xxxx"
}

# 响应
{
  "success": true,
  "data": {
    "id": "acc_1",
    "icloud_email": "your_email@icloud.com"
  }
}
```

### 别名管理接口

#### 列出所有别名

```bash
GET /api/aliases?account_id=acc_1

# 响应
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "count": 15,
    "aliases": [
      {
        "email": "xyz123@icloud.com",
        "label": "注册某网站",
        "created_at": "2024-01-15T10:30:00Z"
      }
    ]
  }
}
```

#### 停用别名

```bash
POST /api/aliases/:id/deactivate

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

#### 激活别名

```bash
POST /api/aliases/:id/reactivate

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

#### 删除别名

```bash
DELETE /api/aliases/:id

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123"
  }
}
```

## 认证方式

### 方式一: Cookie 认证 (推荐,功能最完整)

Cookie 认证可实现所有功能:创建别名、读取邮件、管理别名。

**适用范围:**
- 创建/停用/激活/删除 HME 别名 ✅
- 读取邮件 (通过 iCloud Web API,无需 App Password) ✅

**获取 Cookie:**

1. 使用浏览器登录 [icloud.com](https://www.icloud.com) 或 [icloud.com.cn](https://www.icloud.com.cn) (国区)
2. 打开浏览器开发者工具 (F12)
3. 进入 Application → Cookies
4. 导出全部 Cookie 为 `{"key":"value"}` 格式的 JSON

**关键 Cookie (必需):**
- `X-APPLE-WEBAUTH-TOKEN` — 认证 token
- `X-APPLE-WEBAUTH-USER` — 含 dsid (`v=1:s=1:d=22789132008`)
- `X-APPLE-WEBAUTH-HSA-TRUST` — 设备信任 token
- `X-APPLE-DS-WEB-SESSION-TOKEN` — 会话 token

**注意:** 导出的 Cookie 值不要包含多余的引号或转义字符。

### 方式二: App Password 认证 (IMAP,优先读邮件)

App Password 用于 IMAP 读取邮件,是邮件读取的优先路径 (支持服务端按收件人搜索)。

**生成 App Password:**

1. 登录 [appleid.apple.com](https://appleid.apple.com)
2. 进入 "登录和安全" → "App 专用密码"
3. 生成新密码,用于此工具

### 邮件读取双路径

`GET /api/inbox` 自动选择读取方式:

1. **优先: IMAP (App Password)** — 设置了 App Password 时使用,支持服务端按收件人 (`TO`) 搜索
2. **回退: Web API (Cookie)** — 无 App Password 或 IMAP 失败时,通过 `mccgateway` 端点读取,本地按别名过滤

响应中包含 `"method": "web_api"` 或 `"method": "imap"` 字段,标识实际使用的读取方式。

## 项目架构

```
icloud-hme/
├── main.go                 # 入口: 读取安全配置、加载账号、启动服务
├── web/                    # 前端工程 (React + TypeScript + Vite)
│   └── src/                #   管理界面源码
├── accounts.json           # 账号配置文件 (自动生成)
├── go.mod
└── internal/
    ├── account/
    │   ├── manager.go      # 多账号管理器 (持久化、客户端工厂)
    │   └── public.go       # 公开 DTO (Summary) 与输入校验
    ├── auth/
    │   ├── manager.go      # 管理员会话 + CSRF
    │   └── limiter.go      # 登录失败限流
    ├── hme/
    │   ├── client.go       # iCloud HME Web 客户端 (Cookie 认证)
    │   └── auth.go         # SRP 登录 (账号密码 + 2FA 获取 Cookie)
    ├── mail/
    │   ├── client.go       # IMAP 邮件客户端 (App Password 认证)
    │   └── web_client.go   # Web 邮件客户端 (Cookie 认证,无需 App Password)
    ├── server/
    │   ├── server.go       # 路由分组 (认证 + CSRF)
    │   ├── backend.go      # 业务接口与 Manager 适配器
    │   ├── auth.go         # 登录/会话/退出 handler 与中间件
    │   ├── account_handlers.go  # 账号管理 handler
    │   └── middleware.go   # 安全响应头、请求上限
    └── webui/
        └── embed.go        # 内嵌前端资源 + SPA fallback
```

### 核心模块

- **account.Manager**: 管理多个 iCloud 账号,负责配置持久化和客户端创建
- **hme.Client**: 封装 iCloud HME Web API,支持 Cookie 认证
- **hme.auth**: SRP 协议登录,支持账号密码 + 可选 2FA
- **mail.Client**: IMAP 邮件客户端 (App Password,优先读邮件)
- **mail.WebClient**: 通过 iCloud Web API (mccgateway) 读取邮件,无需 App Password
- **server.Server**: HTTP API 服务 + 管理界面静态资源

## 技术栈

- **Go 1.26+** / **Gin** — HTTP 框架
- **React 19 + TypeScript + Vite 8** — 管理界面
- **go-imap** — IMAP 协议实现
- **tls-client** — TLS 指纹模拟 (绕过 iCloud 反爬)

## 常见问题

### Q: 创建别名返回 401/403 错误?

**A:** Cookie 已过期，需要重新获取。iCloud Cookie 有效期通常为 24 小时。

### Q: 读取邮件返回超时?

**A:** 检查网络连接，确保可以访问 `imap.mail.me.com:993`。

### Q: 如何查看某个别名收到了哪些邮件?

**A:** 调用 `GET /api/inbox?account_id=acc_1&alias=your_alias@icloud.com`

### Q: 支持同时管理多个 iCloud 账号吗?

**A:** 支持，在 `accounts.json` 中配置多个账号即可，每个账号有独立的 `id`。

## 开发指南

### 本地开发

```bash
# 前端开发模式 (vite dev server, /api 代理到 :8081)
npm --prefix web ci
npm --prefix web run dev

# 后端开发模式
export ICLOUD_HME_ADMIN_PASSWORD='your-strong-password'
go run main.go -debug

# 前端检查 (lint + test + build)
npm --prefix web run check

# 完整构建 (含前端)
./build.sh

# 交叉编译
GOOS=linux GOARCH=amd64 go build -o icloud-hme .
GOOS=windows GOARCH=amd64 go build -o icloud-hme.exe .
```

### 发布

推送 `v*` tag 到 GitHub 自动触发 CI：

```bash
git tag v0.2.0 && git push origin --tags
```

Actions 会自动构建多平台二进制、Docker 镜像（`ghcr.io/linxiaohuo123/icloud-hme`）并创建 Release。

### 代码规范

- 代码注释使用中文
- 错误信息返回给用户时使用中文
- API 响应格式统一: `{success: bool, data: any, message: string}`

## 许可证

MIT License

---
## 社区

友情链接：[LINUX DO](https://linux.do)

## English

A local management tool for Apple iCloud Hide My Email (HME) aliases, supporting creation, listing, and email reading through reverse-engineered iCloud Web API and IMAP protocol. Ships with a built-in Chinese management UI (React SPA embedded in the single binary).

### Features

- Built-in management UI at `http://localhost:8081`
- Create HME aliases automatically
- List all aliases for an account
- Read emails sent to HME aliases via IMAP or Web API
- Manage multiple iCloud accounts
- Dual authentication: Cookie and App Password
- Cookie health monitor: periodic session checks mark dead cookies as error so the scheduler skips them automatically
- Settings panel with push notifications (Feishu / Bark / Telegram) for cookie expiry, recovery and alias quota alerts
- Security: AES-256-GCM encrypted credentials storage, single-admin session, CSRF checks, login rate limiting, redacted API responses

### Quick Start

#### Option 1: Binary (GitHub Releases)

Download the latest binary from [GitHub Releases](https://github.com/linxiaohuo123/icloud-hme/releases):

| Platform | File |
|---|---|
| Linux x86_64 | `icloud-hme_linux_amd64` |
| Linux ARM64 | `icloud-hme_linux_arm64` |
| macOS Intel | `icloud-hme_darwin_amd64` |
| macOS Apple Silicon | `icloud-hme_darwin_arm64` |
| Windows x86_64 | `icloud-hme_windows_amd64.exe` |

```bash
# Linux example (admin password and master key are REQUIRED)
export ICLOUD_HME_ADMIN_PASSWORD='change-this-before-running-2026'
export ICLOUD_HME_MASTER_KEY='$(openssl rand -base64 32)'
chmod +x icloud-hme_linux_amd64
./icloud-hme_linux_amd64
```

#### Option 2: Docker

```bash
docker pull ghcr.io/linxiaohuo123/icloud-hme:latest

docker run -d \
  --name icloud-hme \
  -p 8081:8081 \
  -v /path/to/data:/app/data \
  -e ICLOUD_HME_ADMIN_PASSWORD='change-this-before-running-2026' \
  -e ICLOUD_HME_MASTER_KEY='your-32-byte-base64-master-key' \
  ghcr.io/linxiaohuo123/icloud-hme:latest
```

> The password and master key above are only examples — do NOT copy them. Use a strong password (min 8 chars) and a 32-byte Base64 master key.

#### Option 3: Build from source (Go 1.26+ and Node.js 22.12+)

```bash
git clone https://github.com/linxiaohuo123/icloud-hme.git
cd icloud-hme

# One-shot build (frontend deps → frontend test → frontend build → Go test → binary)
./build.sh

# Or step by step
npm --prefix web ci
npm --prefix web run build
go build -o icloud-hme .
```

### Configuration

| Env var | Description | Default |
|---|---|---|
| `ICLOUD_HME_ADMIN_PASSWORD` | Admin password, **required**, min 8 chars | none (refuses to start) |
| `ICLOUD_HME_SESSION_TTL` | Session TTL | `12h` (range `15m`–`168h`) |
| `ICLOUD_HME_SECURE_COOKIE` | Set `true` when deployed behind TLS | `false` |
| `ICLOUD_HME_COOKIE_MONITOR_INTERVAL` | Cookie health monitor interval | `30m` (range `5m`–`24h`) |

> **Breaking change (v0.3+)**: without `ICLOUD_HME_ADMIN_PASSWORD` the server refuses to start; all API endpoints now require login (`401 AUTH_REQUIRED`). Admin sessions are in-memory only and are lost on restart.

Create `data/accounts.json` (see `accounts.json.template`) and start the server (default port `:8081`). Open `http://localhost:8081` to use the management UI. Full API contract: [API.md](API.md).
