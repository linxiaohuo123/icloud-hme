# 前端第二轮深度审查（2026-10-02）

修复状态：本报告记录修复前证据，相关 5 类问题已在 [修复报告](FRONTEND_BUGFIXES_ROUND3_2026-10-02.md) 中完成本地修复和验证。

## 结论

当前工作区确认 5 类可复现 bug。11 个隔离前端探针按正确行为断言均失败；其中 3 个 UI 探针直接消费 Go 实际 MIME 解析生成的字段。常规 30 个测试文件、306 项测试以及 lint/typecheck/build 全通过，现有覆盖尚未包含这些路径。

本轮只审查，未修改业务源码。上一轮修复的范围不是全站无 bug 的保证；下述第 1 项已通过修复前源码快照对照确认属于新引入的回归。

## 1. P1：摘要与正文的数字跨字段拼接，导致有效验证码消失

位置：`web/src/utils/sniffer.ts:28-39,105-115`，消费者 `web/src/components/inbox/MailDetailDialog.tsx:48` 及收件箱列表。

实际后端数据：HTML 正文 `<p>482019</p>`，主题 `Verification code`，`decodeFullMessageBody` 输出 preview=`482019`、body=`<p>482019</p>`；后端 `ExtractOTP` 提取 `482019`。

前端 `buildSniffContext` 在清洗前比较原始 body 和 preview，因此清洗后重复数字仍拼成 `Verification code\n482019\n482019`。嗅探器只区分主题和合并后的 body，数字 run 正则将后两个字段视为一个 12 位数字串并拒绝。详情弹窗的验证码栏消失。

修复前快照返回 `{code: "482019"}`，当前实现返回 null，已确认回归。另一个独立边界探针：preview=`Your code is 4820`、完整 body=`Your code is 482019.`，被识别为两个强候选而返回空；该输入的提供方产生条件本轮未验证。

修复方向：明确保持 subject/preview/body 的字段边界和来源，完整正文存在时定义摘要参与策略；不要让不同字段的数字 run 互相连接，同时保留同一正文内分行验证码支持与真实冲突拒绝。

## 2. P2：HTML 清洗不完整，污染验证码候选并破坏正文展示

位置：`web/src/utils/sniffer.ts:185-201`，消费者 `MailDetailDialog`。

- 实际 MIME 样本含 `<head><title>Your code is 123456</title></head>`，正文 `<p>Your code is 482019.</p>`。Go 清洗掉不可见 head/title 并正确提取 482019；前端保留 title 文字，新增了 123456 强候选，使验证码被歧义拒绝。即使 preview 已由后端清洗正确，重新处理原始 body 仍污染结果。
- 正文 `<p>Your code is &#52;&#56;&#50;&#48;&#49;&#57;.</p>`，后端正常解码为 482019。前端详情正文实际显示字面量 `&#52;&#56;...`。有后端完整 preview 时验证码仍可从 preview 提取，因此不能把此样本的完整 UI 路径误报为一定漏码；无解码 preview 的工具输入探针则确实漏码。

修复方向：对齐后端可见文本语义，处理数字/十六进制实体以及 head/title/noscript 等不可见区块；继续以 React 文本节点安全渲染，不引入原始 HTML 注入。

## 3. P2：其他异步弹窗仍缺少会话所有权保护

位置：`web/src/components/CreateAliasDialog.tsx:30-56,64-90`，`web/src/components/AppPasswordDialog.tsx:43-61,71-108`。真实父组件分别为 AccountWorkspace 持续挂载创建弹窗和 AccountsPage 按目标条件挂载凭据弹窗。

创建别名的复现：提交 First 后让响应延迟，取消、重开并输入 Second draft；旧响应成功后执行 setLabel('')/onCreated，关闭新弹窗并丢失新草稿。请求未被取消，服务端创建仍可能成功，关闭 UI 不代表撤回创建。

App 密码的复现：账号 A 保存期间取消，使旧组件卸载，再打开账号 B 并输入新草稿；A 的旧 onSaved 回调仍更新父组件目标状态，直接关闭 B 的新弹窗。探针使用与 AccountsPage 相同的条件挂载模式，未访问真实凭据接口。

修复方向：提交期间约束交互，并将回调所有权绑定到账号、打开会话和挂载生命周期。不能只对 CookieDialog 使用该保护，也不能认为组件卸载会自动取消 Promise 回调。

## 4. P2：代理连通性结果可能属于旧输入

位置：`web/src/components/ProxyDialog.tsx:41-55,105-108`。

输入代理 A，发起延迟检测；在检测过程中改成代理 B。onChange 清除旧结果，但 A 的响应返回后仍无条件 setCheckResult(res)，界面在 B 输入下显示 A 的“连通正常”。此时保存提交 B，会给用户错误的测试依据。

修复方向：检测结果绑定提交时的代理值及请求序号/弹窗会话，输入变化或新检测作废旧结果；或者明确锁定检测期间输入。错误响应也应遵循同一所有权规则。

## 5. P2：业务标识创建成功后清空后续草稿

位置：`web/src/pages/BusinessTagsPage.tsx:226-241,595-638`。

提交 first 并延迟 POST，输入框仍可编辑，将其改为 second-draft；旧请求成功后无条件 setNewTagName('')/setNewTagDesc('')，后续草稿消失。探针确认真正发起请求、完成响应并触发成功反馈后，输入值变为空。

修复方向：创建期间锁定相关表单与预设按钮，或只清除仍等于提交快照的字段。上轮 SettingsPage 的修复没有覆盖该表单。

## 验证与证据

- `npm run check`：30 个文件、306 项测试通过，ESLint 零错误/零警告，TypeScript 和生产构建通过。
- 临时前端探针：11/11 在正确行为断言处失败。原始结果、精简断言摘要和探针源码保存在忽略目录 `build/frontend-audit-2026-10-02/` 的 `probe-round2-results.json`、`probe-round2-summary.json`、`frontend.round2.repro.test.tsx`。
- `go test ./internal/mail -run TestFrontendAudit2 -count=1 -v`：4 个后端对照子样本和 3 个真实 MIME 样本全部通过。真实字段保存为 `round2-real-mime-fixtures.json`，Go 源码探针保存为 `backend-round2-parity.test.go.txt`。
- 临时前端/Go 测试已从正式源码目录移除，不将故意失败的审查探针纳入常规测试。
- 本轮未重跑 Go 全仓测试；未修改 Go 业务代码。
- `git diff --check`：通过。

## 已检查但不计入可触发缺陷的路径

收件箱旧删除目标和跨账号删除完成回调存在静态风险，但 InboxTableRow 的删除按钮当前 disabled，且规范 MessageRef 带账号并由后端校验。因此本轮没有把它们算作可触发的跨账号误删 bug。

UsedAliasesPage 总数缩小时页码与 offset 可能不一致，但当前 LeasePruner.PruneOnce 明确暂停物理清理，不能声称后台自动清理正在触发该故障。本轮不将纯构造的缩量输入算入确认结论。

## 边界

所有新异常来自虚构数据、隔离的 MSW 请求、前端组件以及实际 Go MIME 解析调用。未发送真实通知、未创建或删除 Apple 邮箱、未操作生产环境；未触碰 `.env`、数据或密钥。用户原有未提交修改保留，无提交、推送或部署。
