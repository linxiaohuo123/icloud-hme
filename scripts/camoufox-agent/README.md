# Camoufox 登录代理

该服务按请求启动独立浏览器，取得 Apple Cookie 后由 Go 主服务校验会话和账号身份，通过后才替换并保存凭据。最多同时运行 2 个登录任务；每个任务最长 8 分钟，结果保留 2 分钟。Go 首阶段最多等待 210 秒，留出慢页面及中国区重新登录时间。

登录代理先处理验证码、区域跳转与“信任此浏览器”，再确认浏览器的 `accountLogin` 请求启用了 `extended_login: true`、响应已完成且返回账号身份、无需继续双重认证且 `hsaTrustedBrowser: true`，最后提取 Apple 会话 Cookie。仅出现 TOKEN、WEB-ID 或 TRUST Cookie 不代表最终会话已经建立；缺少完整握手时会报错，不导出中间态凭据。新版结构保留 Cookie 的域、路径、有效期、安全属性及同名 Cookie；分区 Cookie 暂不支持，遇到时明确拒绝导出。保持登录不保证固定有效期，Apple 仍可撤销会话。

新登录握手发出或切换区域时会立即清除旧就绪状态；旧请求的迟到响应不能恢复它，读取 Cookie 期间发生新握手也不会导出旧结果。“信任”按钮可见但点击失败时继续等待，不跳过信任步骤。

## Docker Compose

在项目根目录的现有 `.env` 中新增 `ICLOUD_HME_CAMOUFOX_TOKEN`，值使用独立随机令牌，例如 `openssl rand -hex 32` 的输出。保留已有 Master Key 与数据库文件。生产环境首次部署优先拉取 GHCR 预构建镜像，然后运行：

```bash
docker compose pull camoufox-agent
docker compose build icloud-hme
docker compose up -d
docker compose logs --tail=50 camoufox-agent
```

Camoufox agent 镜像由 `.github/workflows/camoufox-agent.yml` 独立构建，只有 `scripts/camoufox-agent/` 变化或手动触发时才重新构建。修改主服务代码不会重新安装系统库或下载浏览器内核。

如果 GHCR 不可达、镜像权限未配置或需要本地调试，可显式使用仓库内的 Dockerfile 兜底：

```bash
docker compose build camoufox-agent
docker compose up -d camoufox-agent icloud-hme
```

主服务与代理通过 Compose 内部网络通信；代理不发布宿主机端口。主服务设置页可以检查代理就绪状态。

代理启动时会实际启动一次浏览器验证运行环境，之后空闲时每 120 秒刷新；失败时每 15 秒重试。`/health` 只返回最近一次探针结果，不会因健康检查请求额外启动浏览器。登录任务运行期间暂停后台探针。

## Windows 本地运行

`start_agent.bat` 和项目根目录的 `start_camoufox_agent.bat` 只会从项目根目录 `.env` 读取 `ICLOUD_HME_CAMOUFOX_TOKEN`，也可以在进程环境中预先设置同名变量。缺少令牌时脚本直接退出。Agent 默认只监听 `127.0.0.1:8089`。

跨主机连接时，主服务的 `ICLOUD_HME_CAMOUFOX_URL` 必须使用 HTTPS，并在防火墙或 VPN 中限制来源；需要双向认证时同时配置 `ICLOUD_HME_CAMOUFOX_CA_FILE`、`ICLOUD_HME_CAMOUFOX_CLIENT_CERT_FILE` 和 `ICLOUD_HME_CAMOUFOX_CLIENT_KEY_FILE`。只有确认链路位于加密专用 VPN 后，才允许显式设置 `ICLOUD_HME_CAMOUFOX_ALLOW_INSECURE=true` 使用 HTTP。

账号 HTTP/HTTPS 代理可使用 `http://user:pass@host:port` 格式，Agent 会把认证信息单独交给浏览器。Camoufox 所用浏览器不支持带用户名密码的 SOCKS 代理，此类配置会明确报代理配置错误。

## 内部接口

`/health`、`/login`、`/submit-otp`、`/tasks/{task_id}` 均要求 `X-Camoufox-Token` 请求头。任务状态返回实际登录的 `host`；Go 主服务负责轮询任务、提交 OTP，并在校验成功后一起保存 Cookie 与登录区域。用户关闭 OTP 弹窗时会取消任务，`DELETE /tasks/{task_id}` 会终止对应浏览器。不要将令牌或成功任务返回的 Cookie 写入日志。

主服务只持久化账号 ID、任务 ID、代理地址和创建时间。重启后会取消旧任务；代理暂时不可达时保留记录，后续登录或停机时重试。已配置代理但探活失败会直接报错，不会静默回退至 SRP。

## 会话保存与恢复（数据库 v8）

主服务和 Agent 必须一起更新。Agent 成功响应新增 `session`：版本、DSID、登录区域、受信任状态、捕获时间、结构化 Cookie 列表，以及对应 `dsWebAuthToken`、`trustToken`、`accountCountryCode` 的恢复材料。认证材料优先采用当前成功响应的 Session/Trust Token 和 Account-Country 响应头，缺失时沿用当前请求值；旧请求的迟到响应头不能覆盖它们。Cookie map 仅供兼容投影，不能表示所有作用域。

主服务对新版会话用标准 Cookie 作用域规则发送，避免跨域灌入及同名覆盖；旧手动 Cookie 导入保持原行为。完整会话作为版本化 JSON 保存在原有 `accounts.cookies` 加密列，继续使用现有 Master Key 和账号 AAD。升级到 v8 不改写旧 Cookie 数据，启动前自动备份；旧程序不能打开 v8 数据库。降级必须使用升级前备份并保留原 Master Key，不要手工降低 `user_version`。新版认证材料不允许写入历史明文 JSON 回退存储。

后台校验及失败后下一次借出前的校验，可以使用保存的认证材料执行一次 `accountLogin`，之后重新检查 DSID、信任状态及 HME 端点。只读列表在认证恢复成功后最多重试一次；创建、保留、删除等业务操作不会因恢复而自动重放。临时失败冷却 5 分钟，明确拒绝或要求验证码时禁止继续自动恢复，需人工重新登录；恢复限制也持久化。认证校验通过并核对账号身份后立即保存会话检查点；后续业务失败只保存恢复限制，不用失败响应的 Cookie 覆盖该检查点。缺失或错误类型的信任/身份字段按协议错误处理，不作为明确 MFA 永久阻断恢复。人工更新 Cookie 会清除旧认证材料。代理变更要求重新校验，已有浏览器会话不允许直接改写登录区域。

新版会话收到普通 HTTP 403（没有明确的 `AUTHENTICATION_FAILED` 错误码）时，报告访问被拒绝，不直接判定 Cookie 过期或触发自动恢复，避免将访问环境问题误当成认证失效。

公开账号响应新增脱敏 `session` 属性，包含 `trusted`（最后一次确认的信任状态）、`captured_at`、`recovery_available`（有恢复材料，不代表保证成功）、`recovery_blocked`、`recovery_after` 和 `token_expires_at`（当前 setup 请求可用核心 Cookie 的最早已知期限；任一适用 Cookie 为会话型、或无适用 Cookie 时为 null）。日志只记录到期秒数、是否有恢复材料和状态，不记录 Cookie 或 token 值。会话型 Cookie 不被伪造固定有效期；短效 Cookie 允许导入但保留真实到期属性，不宣称长效。观察真实 Apple 登录 24/72 小时后的可用性仍需部署后验证。

## 请求发送与认证错误边界

手动导入的 Cookie 字符串没有域/路径元数据，继续使用原有 map 兼容发送，但仅由该 map 构造一次请求头，不再让 Cookie jar 追加第二份。原生 SRP 登录在握手期间使用 jar，成功提取 Cookie 后交给 map；Camoufox 结构化会话始终由作用域 jar 发送。`Set-Cookie` 删除的旧值不能由另一份缓存重新附加。

HTTP 200 正文中的明确 `AUTHENTICATION_FAILED`（顶层 errorCode 或 error.errorCode/error.code）同样归类为认证失败。只读校验可以进入既有恢复流程，写操作不因此重放。恢复 accountLogin 遇到 429 时，冷却时间至少 5 分钟；有效 Retry-After 更长时采用上游时间，并沿用现有恢复状态持久化。
