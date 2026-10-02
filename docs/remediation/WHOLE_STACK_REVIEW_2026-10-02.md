# 全栈审查：2026-10-02

> 修复状态（2026-10-03）：F01–F06 已完成本地修复和回归验证，见 [全栈修复报告](WHOLE_STACK_BUGFIXES_2026-10-03.md)。以下保留发现时的行为与复现证据。

## 结论与边界

当前工作区仍有 **6 项已复现问题**，其中 2 项直接影响自动化取码或创建配额。上一轮已修的 7 类前端问题不计入本次发现。

本次检查前端请求、后端 handler、账号与邮件来源、MIME/OTP、号池分配、幂等操作、取码状态机、数据库迁移/备份、调度、认证、通知、Camoufox 代理和 Compose 配置。通过临时 SQLite、本机 HTTP 模拟端点及 MSW 验证，未操作真实 Apple 账号、真实号池、生产数据库或外部通知渠道。

本轮只新增审查报告和归档的复现证据；未修复业务代码，未提交、推送或部署。既有未提交修改保持原样。

## 已确认发现

| 编号 | 优先级 | 问题 | 实际影响 |
| --- | --- | --- | --- |
| F01 | P1 | MIME 纯文本/HTML 拼接破坏验证码候选边界 | 邮件已收到却无法完成取码，worker 越过该邮件 |
| F02 | P1 | 配额释放没有绑定原预留小时 | 旧请求失败可释放新小时的预留额度，造成超配额创建 |
| F03 | P2 | 通知 HTTP 重定向绕过内网 URL 校验 | 已配置通知 URL 可把服务端请求和通知内容带到内网 |
| F04 | P2 | 创建业务标识接受已有 ID 并执行 UPSERT | POST 返回成功，但已有业务标识被覆盖 |
| F05 | P2 | 业务标识改名未处理母号标签引用 | 修改脚本为新 tag 后，已有可用库存无法按新 tag 领取 |
| F06 | P2 | 业务标识编辑没有隔离弹窗保存会话 | 旧保存响应关闭新弹窗，新草稿丢失 |

### F01：后端 multipart/alternative 导致验证码丢失

位置：`internal/mail/mime.go:415`、`internal/mail/sniffer.go:73`、`internal/server/mail_sync.go:1069`、`internal/server/mail_sync.go:1165`。

真实 MIME 解码输入：

```text
text/plain: Your verification code is 482019
text/html:  <p>482019</p><p>Expires in ten minutes</p>
```

`bodyPreview` 将两种格式合为：

```text
Your verification code is 482019

482019
Expires in ten minutes
```

`digitRunRegex` 允许数字之间包含换行，跨越 MIME 格式边界抓到两个相同验证码，形成 12 位候选并拒绝。`ExtractOTP("Verification code", full.Preview)` 实际返回 `nil`。

worker 使用该 Preview 嗅探 OTP；结果为空时跳过邮件，并正常推进扫描 checkpoint。使用同一实测 Preview 连续调用实际 worker 两次，得到：

```text
status=ready code="" scan_calls=1 nextUID=13
```

目标邮件 UID 是 12。第二次轮询未重新扫描，任务仍无法完成；既有任务继续等待，最终可能过期。这是后端解析和扫描路径的问题，前端嗅探器修复未覆盖该路径。

修复方向：保留纯文本、HTML 的内容字段边界，对各字段提取候选并统一仲裁，保留真实多验证码冲突拒绝及分组验证码支持。增加真实 MIME 到 worker 持久化终态的回归。

证据：`mail_probe_test.go.txt`、`mail-probe.log`、`server_probe_test.go.txt` 的 worker 测试及 `worker-probe.log`。

### F02：旧小时失败释放新小时额度

位置：`internal/store/schedule.go:167`、`internal/store/schedule.go:203`；实际调用者为 `internal/server/backend_alias.go:128` 及批量创建失败路径。

`TryReserveQuota` 返回允许/剩余额度，未向调用者返回预留所属窗口；`ReleaseQuota(accountID, count)` 仅查看数据库当前窗口。触发时序：

1. 请求 A 在整点前预留一个额度，仍在网络调用或等待账号锁。
2. 整点后，请求 B 预留新小时的一个额度，数据库进入新窗口。
3. A 失败，调用 `ReleaseQuota(accountID, 1)`。
4. 释放操作减掉 B 所属新窗口的预留计数。

临时数据库按等价时间窗口模拟整点切换，设置每小时额度 5，新窗口预留一个后失败释放旧请求，实际剩余值为 **5**，正确值应为 **4**。本次未等待真实整点，模拟只改变旧预留行的窗口字段，其余使用生产 Store 方法。

后端在进入账号写互斥锁之前预留额度，旧操作网络阶段与新窗口预留可以重叠，因此该时序存在真实调用路径。新请求若成功，系统仍认为其未消耗额度，可额外放行创建。

修复方向：预留返回窗口标识，失败释放通过原窗口条件更新；单条、批量、取消与未知结果路径统一使用该预留凭据。

证据：`store_probe_test.go.txt`、`go-probes.log`。

### F03：通知出站校验仅覆盖初始 URL

位置：`internal/server/netguard.go:27`、`internal/server/settings_handlers.go:122`、`internal/notify/notify.go:94`、`internal/notify/notify.go:383`。

保存通知配置时拒绝内网 URL，但发送器使用默认跟随重定向的 `http.Client`。初始公开地址返回 HTTP 307 并指向环回地址后，没有再次应用出站策略。

测试通过真实设置和测试通知 handler，将 `http://8.8.8.8/hook` 配置为逻辑公开地址；测试专用 transport 将该地址映射到本机模拟重定向服务，未访问 8.8.8.8。模拟服务返回 307 到第二个本机端点；未开启内网放行开关时，第二端点收到一次转发请求，API 还返回：

```json
{"results":[{"channel":"feishu","ok":true}]}
```

限制：此入口需要管理员配置通知 URL，或已配置的外部端点发生重定向；未确认未认证攻击者可直接使用该入口。DNS 解析变化还存在静态风险，但本次仅把已复现的重定向绕过列为确认结论。

修复方向：发送时对重定向和实际连接目标应用同一出站策略，保留显式内网放行配置的语义。

证据：`server_probe_test.go.txt`、`go-probes.log`。请求只到本机测试服务器。

### F04：POST 创建覆盖已有业务标识

位置：`internal/server/hub_handlers.go:61`、`internal/server/hub_handlers.go:80`、`internal/store/tag.go:63`。

handler 直接反序列化完整 `BusinessTag`，只在 ID 为空时生成新 ID。重名预检检查 tag 字符串，无法阻止客户端传入已有 ID 和新 tag；`SaveTag` 的 `ON CONFLICT(id) DO UPDATE` 随后覆盖旧记录。

复现：先保存 `id=tag_existing, tag=original, description=keep me`，再 POST 同 ID 的 `replacement`。返回 HTTP 200，库中只剩一条 replacement 记录，原说明被覆盖。

限制：管理接口受 admin 权限保护；默认前端创建不发送 ID。错误导入、重试代码或其他管理 API 调用者能够触发。这是创建契约与数据完整性问题，无已确认的普通令牌越权。

修复方向：创建 DTO 只接收可编辑字段，服务端生成 ID/创建时间，创建持久化使用 INSERT 并显式处理冲突。

证据：`server_probe_test.go.txt`、`go-probes.log`。

### F05：修改 tag 后母号路由仍引用旧字符串

位置：`internal/server/hub_handlers.go:111`、`internal/server/hub_handlers.go:135`、`internal/server/quick_create_handler.go:124`；前端 `web/src/pages/BusinessTagsPage.tsx:260`。

编辑弹窗同时修改 tag 和 name，并提示用户修改外部脚本参数。后端只修改 `business_tags` 记录；账号 `accounts.tags` 仍存储旧 tag。号池路由按账号标签字符串匹配新请求，不读取业务标识 ID。

复现：账号绑定 `oldbiz`，库存有 active/available 别名；PATCH 标识为 `newbiz` 成功。读取实际账号行仍是 `["oldbiz"]`；把该实存标签作为 Backend 摘要输入实际分配服务，按 `newbiz + pool_only` 领取返回 `pool empty`。

范围：此问题需要先绑定业务标签，再通过当前编辑功能修改业务键。新建标识或只改说明不触发。历史流水和操作也保存业务键字符串，重命名时应明确其展示与审计口径。

修复方向：为被引用的业务键建立显式改名规则。最小方案是拒绝直接重命名被引用 tag 并返回明确错误；如保留重命名能力，需事务迁移母号引用、同步内存摘要，并明确历史记录处理，验证改名后实际领号。

证据：`server_probe_test.go.txt`、`go-probes.log`。

### F06：旧编辑请求关闭新业务标识草稿

位置：`web/src/pages/BusinessTagsPage.tsx:260`、`:274`、`:1106`、`:1134`。

保存期间仅禁用提交按钮，输入、取消和关闭仍可用。成功响应无条件 `setEditingTag(null)`；finally 无条件清 `savingEdit`，没有目标/会话校验。

MSW 延迟 A 的 PATCH，用户点击取消、打开 B 并修改草稿。释放 A 响应后，新 B 弹窗被关闭。探针前置步骤及成功提示均完成，断言在响应后检查新弹窗时失败，位置为归档探针第 40 行。

修复方向：沿用当前已有 `useDialogSession` 保存锁与会话隔离，旧完成只刷新数据，活跃编辑会话决定弹窗交互。增加取消/重开、目标切换及过期 finally 回归。

证据：`frontend_probe.test.tsx.txt`、`frontend-probe.json`。

## 验证与现有覆盖

本次新探针共 **7 个测试**，均在正确行为断言处失败，对应上面的 **6 项问题**；F01 分别验证真实 MIME 解码和实际 worker 的持久化/扫描行为。探针已经从正式测试目录移出，归档源码以 `.go.txt` / `.tsx.txt` 结尾，不影响正常构建。

归档目录：`build/whole-stack-audit-2026-10-02/`，属于忽略的本地证据，未上传。

本轮现有回归验证：

- account/mail/store/scheduler/notify 的相关迁移、备份、邮箱来源、身份、取消、配额、调度、补货回归通过，输出保存在 `core-regressions.log`。
- server 的认证路由矩阵、分配幂等、跨主体隔离、取码扫描、共享邮箱聚合、来源切换、取消、通知内网初始地址拦截、元数据缓存等选定回归通过，输出保存在 `server-regressions.log`。
- 认证包全量 **19 个测试**通过，输出保存在 `auth-regressions.jsonl`。core 的正则筛选未选中该包，已单独补跑。
- 前端选定的 **3 个测试文件、98 个测试**通过：业务标识现有行为、上一轮弹窗回归及 OTP 嗅探，结果为 `frontend-regressions.json`。
- Camoufox 代理 **22 个测试**通过，测试模拟浏览器/握手/任务 API，未使用真实 Apple 登录。
- `git diff --check` 通过，只有既有 CRLF 转换提示。

此前同工作区已经完成 Go 全仓测试、vet 和前端 `npm run check`。本轮未改业务代码，因此未重复完整构建；新增触发场景证明现有测试通过不等于上述场景正确。

## 额外文档偏差

`API.md:394` 宣称请求业务标签无匹配账号时自动回退公共号池。但 `selectPoolAccounts` 对非 default tag 明确返回专属匹配集合，无匹配直接为空；对应现有回归要求禁止回退。应修正文档，区分库存领取和管理员实时创建候选选择，不把严格隔离的行为描述成通用回退。

## 未验证范围

- 未检查生产服务器当前代码、容器状态、持久卷权限、反代配置、备份可恢复性或真实收信延迟；Compose 文件静态检查不能替代生产验证。
- 未进行真实 Apple Cookie 长时间可用性、Camoufox 真实登录或提供方 HTTP 行为验证。
- 未跑 Linux 专属信号/容器测试；本机 Windows 未检测到 GCC/Clang，未运行 Go race 检测。
- 本轮未做完整浏览器视觉验收或依赖漏洞数据库扫描。
- 对未发现新缺陷的模块，仅能确认本次阅读及选定测试范围，没有“整套系统零 bug”的证据。

建议修复顺序：F01 取码、F02 配额，随后处理 F03 出站策略与 F04/F05 业务标识数据契约，最后补齐 F06 编辑会话。
