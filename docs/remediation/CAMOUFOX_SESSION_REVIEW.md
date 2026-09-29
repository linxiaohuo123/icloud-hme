# Camoufox 会话生命周期复查

核查日期：2026-09-29。对象：本地会话管理实现；本文的测试结果不代表生产部署结果。

## 结论

复查时确认结构化 Cookie、独立认证材料、加密持久化和有界恢复方向正确，并发现五个可复现的实现缺口；后续修复状态见下节。应先完善状态提交与错误分类，再考虑浏览器持久化等较重方案。不能由 Cookie 数量、extended_login 或 Expires 推断 Apple 服务端会话一定长期有效。

## 本轮修复状态

下表保留修复前的复现依据。五项现已在本地实现修复：认证检查点独立持久化、严格信任/身份响应检查、Agent 响应头轮换采集及 Go 国家信息同步、端点重查错误传播、按 setup 作用域计算期限。新增正式回归覆盖业务失败/取消、重启、人工换号、身份不一致、持久化失败、异常 JSON、迟到响应头及混合 Cookie 作用域。生产长期有效性仍未验证。

## 已验证的问题（修复前）

| 优先级 | 问题与证据 | 影响与建议 |
| --- | --- | --- |
| P1 | `internal/account/hme_pool.go` 的业务错误分支只调用 `saveRecoveryProgress`，随后销毁客户端。隔离测试向池注入轮换后的会话并返回业务错误，确认只保存冷却时间，认证材料仍是旧值。 | 认证已成功但后续列表超时、取消或业务失败时，可能丢失有效新凭据。是否使旧材料立即失效取决于 Apple。应在认证成功且身份确认后提交检查点，失败响应对 Cookie 的删除不能污染该检查点；不重放业务写操作。 |
| P1 | `internal/hme/client.go` 和 `session.go` 对 `hsaTrustedBrowser` 直接调用 Bool()。模拟 HTTP 200 `{}`，实际返回 ErrOTPRequired 并设置 RecoveryBlocked。 | 缺字段、null、错误类型与明确 false 被混为一谈；异常响应可使自动恢复永久停止，直到重新登录。先验证结构和字段类型；只有明确挑战或明确不受信任才判定 MFA，格式错误保留为协议错误。 |
| P1 | Agent `observe_account_login` 只保存 accountLogin 请求体里的认证材料，未读取成功响应的轮换认证头。模拟成功响应携带新 Session/Trust Token，仍导出请求中的旧值，all_headers 未被调用。 | 若 Apple 在这次响应轮换材料，会从导入时就保存旧材料。应读取当前成功握手响应头，有有效新值时覆盖请求值；保留请求代际检查。Go 已接收 Session/Trust Token 头，但尚未同步 Account-Country 头。真实账号是否发生这种轮换尚未验证。 |
| P2 | `internal/hme/alias.go` 的列表端点重查只处理 resolveErr==nil。模拟列表 403 后 validate 401，最终错误仍是第一次的访问拒绝。 | 真实认证错误被旧错误掩盖，上层不能按认证失败恢复。重查失败应传播该次错误；对于限流、访问拒绝、格式错误，需明确哪些确实值得重新发现端点。 |
| P2 | `internal/account/public.go` 对所有域、路径的同名核心 Cookie 取最早期限。给全球区账号加入已过期中国区同名 Cookie，公开到期值取到了中国区。 | 可能显示过期但实际请求凭据仍有效。展示应使用与实际 setup 请求一致的 Cookie 作用域选择规则；明确会话型 Cookie 的未知期限，避免把其他区域的期限当成当前会话期限。 |

测试性质：四项 Go 复现通过临时 `go test -overlay` 加载测试，不修改生产代码；一项 Python 使用模拟 Playwright 响应。这里的 PASS 表示观察到了所描述的缺陷，不能解读为缺陷已修复。凭据丢失测试隔离验证池的提交逻辑，不声称已模拟真实 Apple 的 token 吊销策略。

## 调研结果与方案取舍

1. **恢复材料和 Cookie 必须一起管理。** Pyicloud 的 token 登录发送 accountCountryCode、dsWebAuthToken、extended_login、trustToken，并单独判断受信任状态；session 层还从响应头更新材料。当前方向一致，但本项目应保留严格身份校验和安全提交边界，不能照搬其所有响应都直接落盘的策略。
2. **Cookie 期限和服务端会话寿命不同。** MDN 明确 Max-Age 优先于 Expires，未设置期限的是会话 Cookie。客户端保存一个会话 Cookie 并不会让服务器延长授权。icloudpd 文档提到约两个月的 MFA 周期，这是第三方项目在其业务下的描述，不是 Apple 对 HME 的保证。
3. **暂不采用每账号常驻浏览器。** 本机实际安装 camoufox 0.5.6，源码支持 persistent_context；Playwright 官方说明 storage_state 可保存 Cookie/localStorage/IndexedDB 等，但 sessionStorage 需另外处理。现有 Go API 是否依赖额外浏览器存储尚无证据。常驻浏览器会增加隔离、资源、加密和并发锁管理成本，应在修复确定性缺陷后再做少量账号对照实验。
4. **访问环境应观测，不能当成已确认根因。** 当前浏览器为 Firefox 系，Go 为 Chrome_146 TLS profile，UA 写 Chrome/147；clientId 在客户端重建时重新生成。这些差异存在，但没有证据证明 Apple 将会话绑定于其中任一项。建议先统一 Go 自身 profile/UA，并考虑保存稳定 clientId；跨浏览器方案须通过对照实验决定，不能仅替换 UA 冒充同一环境。
5. **不要增加无差别保活。** 当前默认 30 分钟校验，恢复固定冷却 5 分钟。适宜补充恢复原因、HTTP 状态、最后成功时间、是否发生材料轮换等脱敏观测，并使恢复遵守有效 Retry-After；没有数据前不要把所有账号改成每分钟登录。
6. **可复现部署仍需完善。** requirements 固定 camoufox，但 Playwright/FastAPI 等只有下限；本机本次版本分别为 Playwright 1.62.0、FastAPI 0.141.1、Pydantic 2.13.5，浏览器报告 152.0.4-beta.31。应记录并锁定经验证的构建组合，不能假定生产构建与本机一致，也不应直接把本机版本当成已验证生产锁文件。

## 实施顺序与剩余验收

1. 已实现：修复已验证认证检查点、响应结构判定、响应头轮换采集三项 P1；覆盖恢复成功后列表失败、请求取消、失败 Set-Cookie 删除、进程重载和人工换号竞争。
2. 已实现：修复端点重查错误传播和按实际作用域展示期限；补充混合区域、路径重复、未知期限、显式 MFA、异常 JSON 的回归。
3. 增加脱敏诊断与构建版本记录，再做部署后观察：登录后、1 小时、6 小时、24 小时、72 小时，包含重启和正常空闲。记录 validate/HME 只读结果、恢复次数、Cookie 更新与最后失败分类；不输出凭据，不用创建别名消耗配额。
4. 仅在上述仍出现快速失效且证据指向环境时，对照相同出口代理、稳定 clientId、浏览器持久化上下文。24/72 小时通过仅证明观察窗口可用，不能承诺长期有效。

## 来源与限制

- [Pyicloud token 登录实现（固定提交）](https://github.com/timlaing/pyicloud/blob/86c4bc90d5632bcaf7507e5335e127f7450a177a/pyicloud/base.py#L719)
- [Pyicloud 响应材料更新与持久化](https://github.com/timlaing/pyicloud/blob/86c4bc90d5632bcaf7507e5335e127f7450a177a/pyicloud/session.py#L191)
- [Pyicloud 认证头映射](https://github.com/timlaing/pyicloud/blob/86c4bc90d5632bcaf7507e5335e127f7450a177a/pyicloud/const.py#L10)
- [Playwright 官方认证状态文档](https://playwright.dev/python/docs/auth)
- [Camoufox async API 源码](https://github.com/daijro/camoufox/blob/main/pythonlib/camoufox/async_api.py)；另核对本机 0.5.6 的安装源码，确认 persistent_context 支持。
- [MDN Cookie 生命周期](https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/Cookies)
- [icloudpd 认证说明（固定提交）](https://github.com/icloud-photos-downloader/icloud_photos_downloader/blob/879c561240d993d748ddb4546f935090502b16d3/docs/authentication.md)

以上资料本轮实际读取。Camoufox 官网 usage 页面返回 HTTP 403，因此改读源码；没有将该页面作为成功获取的依据。未访问真实 Apple 账号、未检查生产日志或验证生产部署。没有 Apple 官方 HME 会话有效期契约可用于保证寿命。

## 请求发送链路追加复查

追加发现并用回归测试复现、修复三项问题：

- 旧 Cookie map 手写请求头与 jar 自动发送叠加，同名认证字段发送两次；上游 Max-Age=0 删除后，jar 仍能重新附加旧值。现已将手动导入和 SRP 完成后的业务发送收敛为 map 单一来源；结构化会话保留 scoped jar。测试使用虚构的等号、加号、斜杠及引号值，不使用用户真实凭据。
- HTTP 200 的明确 AUTHENTICATION_FAILED 正文未分类，无法进入既有认证恢复。现统一识别指定错误字段，并验证写操作不会自动重放。
- accountLogin 的 429 响应中较长 Retry-After 被固定五分钟恢复冷却覆盖。现保存两者中更晚的时间，避免提前重新请求。

这些测试证明实现缺陷及修复效果，不证明用户真实账号快速失效必由这些缺陷造成；未调用真实 Apple 账号接口。
