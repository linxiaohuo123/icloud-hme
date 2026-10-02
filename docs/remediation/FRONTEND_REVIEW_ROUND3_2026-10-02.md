# 前端第三轮深度审查（2026-10-02）

修复状态：本报告记录修复前证据，相关 7 类问题已在 [修复报告](FRONTEND_BUGFIXES_ROUND3_2026-10-02.md) 中完成本地修复和验证。

## 结论

第二轮确认的 5 类问题仍能复现；本轮新增确认 2 类，共 7 类。新增问题集中在编辑表单的生命周期及别名说明的读写契约。审查使用真实 AccountsPage、AccountWorkspace 组件、MSW 隔离请求、实际 Go 别名解析器和 HME HTTP 客户端，没有操作真实 Apple 账号或生产环境。

本轮没有修改业务代码，只新增审查报告和忽略目录下的证据。上轮优化不是全站无缺陷的保证；验证码跨字段拼接的问题已在第二轮通过修改前快照证实是上轮修改引入的回归。

| 优先级 | 问题 | 触发与影响 | 证据 |
| --- | --- | --- | --- |
| P1 | 验证码跨字段数字拼接 | HTML 正文只含 `482019`，后端 preview 同样为 `482019`；前端当成 12 位数字串而拒绝，详情验证码消失 | 第二轮真实 MIME 字段和 UI 探针，本轮复验 |
| P2 | HTML 可见文本处理不完整 | head/title 里的数字干扰候选；数字实体在详情中显示为源码 | 第二轮真实 MIME 字段和 UI 探针，本轮复验 |
| P2 | 异步弹窗旧响应关闭新表单 | 创建别名或保存 A 的 App 密码后取消重开，旧成功回调关闭新表单、清掉草稿 | 本轮真实父页面集成探针进一步确认 |
| P2 | 代理检测结果属于旧输入 | 检测 A 期间改成 B，A 的成功结果仍显示在 B 下方 | 第二轮 UI 探针，本轮复验 |
| P2 | 业务标识成功回调清空后续草稿 | 提交 first 后编辑 second-draft，first 成功清空新草稿 | 第二轮真页面探针，本轮复验 |
| P2 | 账号编辑因父页面重渲染重置 | 列表刷新清掉草稿，并把未完成请求的保存锁解除，允许并行第二次 PATCH | 本轮 3 个真实 AccountsPage 探针 |
| P2 | 别名说明无法回读，名称修改也提交空说明 | Go 列表 DTO 丢掉 note；编辑框初始化为空；保存名称时仍发送 `note: ""`，实际 HME 客户端原样转发 | 本轮真工作台探针及 Go 协议对照 |

前 5 项完整根因、条件和边界见 [第二轮报告](FRONTEND_REVIEW_ROUND2_2026-10-02.md)。数字实体样本有完整后端 preview 时仍可提取验证码，不能把正文展示问题夸大为必然漏码。

## 新增问题一：账号编辑草稿和在途保存锁被刷新重置

位置：`web/src/pages/AccountsPage.tsx:449`、`web/src/components/AccountFormDialog.tsx:40-49`。

AccountsPage 在 JSX 中每次重新创建 `editing` 对象。AccountFormDialog 的初始化 effect 依赖整个 `[open, editing]`，因此父页面任何重渲染都可能被误认成一个新编辑会话：重置名称、邮箱、区域、错误，并执行 `setSubmitting(false)`。仅稳定依赖对象身份或只添加按钮 disabled，不能解决生命周期根因。

已验证的真实页面路径：

1. 打开账号编辑，名称输入 `Unsaved draft`；调用实际账号缓存失效入口，列表 GET 完成后，名称变回 `Account a`。
2. 编辑后发起延迟 PATCH，按钮已经显示禁用的“保存中…”；账号更新触发父页面刷新，按钮重新变成可点击的“保存”。点击后，第一个请求尚未完成，第二个 PATCH 已发出。
3. 完全通过页面交互复现，无需探针手动发送更新事件：A 的 App 密码保存延迟 → 取消弹窗 → 打开 B 的账号编辑并输入 `Important B draft` → A 保存成功触发实际父回调及账号更新 → B 的草稿变回 `Account b`。

第 3 个路径说明第 1、2 个探针依赖的账号更新是当前系统里真实可产生的事件。后端 `internal/account/manager.go:319` 的 UpdateMetadata 按收到的字段保存，未提供请求版本仲裁。并行重复请求已证实；网络完成顺序造成最终字段覆盖是由此推导的后果，本轮未通过真实生产数据演示。

最小修复方向：按打开会话和编辑目标身份初始化草稿，避免无关父重渲染重置；保存锁独立于表单初始化，并使异步完成回调只作用于所属会话。补上上述真页面回归测试。

## 新增问题二：别名补充说明缺少完整读写闭环

位置：`internal/hme/alias.go:23,467`、`web/src/api/types.ts:46`、`web/src/components/EditAliasDialog.tsx:34,42,62`、`internal/server/alias_handlers.go:268`、`internal/server/backend_alias.go:840`、`internal/hme/alias.go:387`。

实际调用链：

`上游 metaData.note → parseAliasList/Alias DTO 丢弃 → 前端 Alias 无 note → 编辑框置空 → PATCH 显式提交 note:"" → handler/managerBackend 原样传递 → HME updateMetaData 显式发送 note:""`。

Go 探针向实际解析器输入带 `metaData.note="Existing important note"` 的合法列表。序列化结果为：

```json
[{"email":"note-test@icloud.com","anonymousId":"note-test","label":"Existing label","active":true}]
```

该真实 DTO 保存为 fixture，供工作台 UI 探针直接消费。打开“修改别名备注”后，说明框为空；只将名称改为 `Renamed label` 并保存，实际前端请求为：

```json
{"account_id":"a","label":"Renamed label","note":""}
```

Go 实际 HME HTTP 客户端对隔离的本地 httptest 服务发送：

```json
{"anonymousId":"note-test","label":"Renamed label","note":""}
```

已确认：现有说明无法正常回读，用户仅改名称也会提交空说明，链路没有“保持已有说明”的语义。Apple 对空 note 的实际存储处理本轮没有联网验证；不能声称已经观察到生产说明被删除。按照项目现有 updateMetaData 全量字段写入实现，覆盖已有说明是明确风险。

最小修复方向：完整透传当前说明并正确初始化编辑框，让名称修改保留已有说明、用户明确清空时才清空。不能仅在前端省略 note：当前 Go 请求 DTO 用 string，缺失字段仍变成空字符串；若采用局部更新，需要整个协议链路都区分缺失与显式空值。

## 复验及证据

| 验证 | 本轮结果 |
| --- | --- |
| 第二轮隔离探针 | 11/11 在预期正确行为断言处失败；5 类缺陷及限定边界仍存在 |
| 第三轮隔离探针 | 6/6 在预期正确行为断言处失败；3 个账号编辑探针、2 个真实父页面弹窗探针、1 个说明探针 |
| Go 说明协议对照 | `go test ./internal/hme -run TestFrontendAudit3AliasNoteContract -count=1 -v` 通过；确认 DTO 丢字段及真实出站空 note |
| 常规相关回归 | 6 个测试文件、71 项全部通过：AccountsPage、AccountWorkspace、SchedulePage 两文件、InboxTableView、useAccounts |
| 空白和冲突标记检查 | `git diff --check` 通过；已有 CRLF/LF 提示不是测试失败 |

本轮复验的调度保存队列、在途保存与立即执行互斥、旧轮询结果隔离、收件箱切号/正文请求隔离路径，没有确认新增缺陷。收件箱物理删除按钮及保留期清理当前停用，不把对应静态风险当成活跃故障。

第二轮全量 `npm run check` 的 30 个文件、306 项测试及 lint/typecheck/build 已通过；本轮未修改业务源码，因此只重跑以上 71 项相关测试，没有重复全量构建、Go 全仓测试或 vet。常规通过与隔离探针失败并不矛盾：现有回归测试没有覆盖这组具体错误路径。

证据在 Git 忽略目录 `build/frontend-audit-2026-10-02/`：

- `frontend.round3.repro.test.tsx`：第三轮前端探针源码；复制至 `web/src/test/` 后，在 web 目录运行。
- `probe-round3-results.json`、`probe-round3-summary.json`：6 个失败断言。
- `probe-round2-recheck-results.json`：第二轮 11 个探针的本轮复验。
- `round3-real-alias-fixture.json`：实际 Go 别名解析序列化结果。
- `backend-round3-alias-note.test.go.txt`：Go 解析及出站请求对照源码；复制为 internal/hme 下的临时 `_test.go` 可运行。
- `round3-targeted-regression-results.json`：71 项常规回归结果。

所有临时失败测试已从正式源码目录移除，源码与结果保留在证据目录，不影响常规测试。

## 交付范围

本轮只完成分析和本地证据整理，7 类问题仍待修复。保留用户原有工作区修改，没有提交、推送或部署；没有读写 `.env`、生产数据库或密钥，没有调用真实 Apple 创建/删除、领取号池或发送通知。JS DOM 交互及本地协议验证不等同于生产运行和完整浏览器视觉验收。
