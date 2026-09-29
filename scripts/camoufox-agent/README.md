# Camoufox 登录代理

该服务按请求启动独立浏览器，取得 Apple Cookie 后由 Go 主服务写入并校验账号身份。最多同时运行 2 个登录任务；每个任务最长 8 分钟，结果保留 2 分钟。Go 首阶段最多等待 210 秒，留出慢页面及中国区重新登录时间。

## Docker Compose

在项目根目录的现有 `.env` 中新增 `ICLOUD_HME_CAMOUFOX_TOKEN`，值使用独立随机令牌，例如 `openssl rand -hex 32` 的输出。保留已有 Master Key 与数据库文件。然后运行：

```bash
docker compose up -d --build
docker compose logs --tail=50 camoufox-agent
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
