# 当前工作区系统审查

日期：2026-10-02。基线：Git HEAD `178ff25030e6525e8bf616989f3ecb2824de119d`，包含审查开始时已有的全部未提交修改。

## 结论与范围

发现 4 个可复现问题：2 个 P1 取码正确性问题、2 个 P2 状态与响应问题。不能因现有测试通过就认定所有任务边界已覆盖。

本轮检查了路由与鉴权、库存认领及远程出号、取码创建/交付、共享收件箱扫描、IMAP/HME 连接池、账号配置与登录、数据库版本及部署模板。重点复核现有并发修订后的边界。没有修改业务实现、提交、推送或部署；保留所有既有工作区修改，没有操作真实数据或真实账号。

所有新问题都通过临时 SQLite 和测试后端/HTTP handler 复现，每项连续运行三次。模拟邮箱只控制网络返回和调用顺序，实际调用生产 Worker、Store 与 handler。它们证明本地逻辑缺陷，不证明线上发生率。

## R01 — P1：历史扫描覆盖被沿用到新任务，可能漏掉重建后的验证码

位置：`internal/server/mail_sync.go:880`，以及 `:948`、`:1111` 的覆盖记录更新。

检查点按物理邮箱保存 `NextUID`，但 `AliasBaselines` 只保留别名曾经覆盖过的最小基线。回扫仅发生在新基线小于历史基线，或别名从未出现过时。历史任务结束后的同名新任务，基线通常更大，因此不会触发必要回扫。

确定性顺序：

1. A 的旧任务基线为 10，收码成功；扫描游标推进到 21，覆盖记录为 A=10。
2. B 的任务仍活跃。Worker 下一轮先快照活跃别名，只包括 B。
3. A 的新任务基线为 21，在 Worker 读取下一轮邮箱边界前完成入库；新邮件成为 UID 21。
4. 当前轮仍按 B 的别名集合扫描到 UID 30，跳过 A 的 UID 21，游标变成 31。
5. 下一轮包含 A 新任务，但 21 不小于历史值 10，因此不回扫。A 新任务停留在 ready，验证码为空。

诊断输出：

```text
new generation missed UID 21: status=ready code=""
checkpoint=&{NextUID:31 AliasBaselines:map[a@icloud.com:10 b@icloud.com:10]}
```

该顺序不依赖客户端在 POST 结束前发送验证码；Worker 的活跃任务快照可以先于新任务入库，邮箱边界读取可以晚于入库和来信。因为 A 在失败轮没有被观察，该邮件也没有被发布为 A 的内存事件，不能依赖缓存补救。

建议：覆盖记录必须反映当前观察任务/基线的实际覆盖，不能把历史最小基线等同于后来任务的覆盖。新任务或观察集合变化后，对尚未证明已覆盖的基线恢复必要回扫。保持扫描固定上界与落库后推进游标规则。

验收：固定上述时序，调用正式创建链路并在成功响应后提供新邮件；下一轮应完成新任务。另覆盖同名新任务在两次 Worker 轮次之间替换、共享邮箱多个母号、持久化任务没有 GET 订阅者的场景。

## R02 — P1：切换物理收件邮箱后，旧任务可接受另一邮箱的历史验证码

位置：`internal/server/verification_service.go:403`、`internal/server/mail_sync.go:842`、`internal/server/account_handlers.go:255`。

持久化任务只有 provider、INBOX、UIDVALIDITY 与 UID 边界，没有记录实际收件箱身份。收件邮箱更新和解除绑定只清理 MailReadService 缓存，没有使原收件箱上的活跃任务失效。Worker 虽然按端点指纹隔离扫描检查点，完成任务仍只核对上述四类基线字段。

UIDVALIDITY 的唯一性作用于同一个 mailbox 的代际，不能拿两个不同物理收件箱的相同数值证明其 UID 可比较。

确定性复现：旧任务在邮箱一取得 UIDVALIDITY=1、UIDNEXT=100；切到邮箱二，其 UIDVALIDITY 也为 1。邮箱二存在昨天发来的目标别名邮件，UID=150、验证码 777777。Worker 使用邮箱二的新指纹扫描后，旧任务被成功完成为该历史验证码。

```text
old task accepted another inbox's historical code: status=succeeded code="777777"
```

此探针替换上游邮箱视图和指纹，没有经过真实 SetMailbox 的网络认证；配置 handler 和 Manager 的调用链检查确认目前没有取码任务失效操作。真实提供商会否出现相同 UIDVALIDITY、用户会否在任务活跃时切换邮箱，未做线上验证。

建议：任务基线应绑定物理收件箱身份，或者在物理收信来源发生改变时使原来源任务显式失效；同时阻止在途旧扫描/基线采集把旧来源结果写入更新后的配置。仅清空缓存或检查点不足以解决此问题。

验收：两个不同邮箱使用相同 UIDVALIDITY，旧任务不得读取新邮箱历史邮件；切换期间在途创建和扫描也不得混用来源。普通授权码轮换仍应保持明确的业务语义。

## R03 — P2：批量 UIDVALIDITY 失效未归一化数据库中的邮箱值

位置：`internal/store/inventory_vreq.go:856`。

批量基线与完成查询使用 `LOWER(TRIM(alias_email))`，但批量代际失效仍使用 `alias_email IN (...)`。入参被转小写去空白，数据库历史值没有同步归一化。现有 v11 索引迁移也没有修改历史记录字段。

历史任务的 `alias_email="  Target@iCloud.com  "`、UIDVALIDITY=7；以 `target@icloud.com` 触发 UIDVALIDITY=8 的批量失效时，更新 0 行，任务仍为 ready。基线查询却能找到它。没有 GET 订阅者时，Worker 无法正确把该任务收敛为 invalidated，任务会继续占据有效期内的容量，并被反复扫描。

```text
UIDVALIDITY 7 -> 8 was ignored: affected=0 status=ready
```

建议：批量失效与基线/完成路径使用相同邮箱归一化表达式，继续利用现有归一化索引；不要以额外清洗所有历史数据代替修正不一致的查询语义。

验收：大小写与前后空白的历史值，以及普通规范值，均被同一批量操作正确失效；当前代际任务不得被误伤。

## R04 — P2：兼容取码接口停机时返回空的 HTTP 200

位置：`internal/server/verify_handler.go:133`、`:135`。

兼容接口等待期间，HTTP 请求 Context 与服务 Context 的取消分支都直接 return，不写响应。服务取消会经 requestTrackingMiddleware 联动已接纳请求；对仍连接的客户端，net/http 可以因此产生 HTTP 200 和空正文，调用方收到的不是可识别的停机错误。

诊断调用生产 handler 的服务取消分支，观察到：

```text
shutdown falsely looks successful: HTTP 200 body=""
```

影响 `/api/verify-code` 及复用该 handler 的 mail/code 等兼容入口。v2 的 VerificationService 有明确 SERVER_SHUTTING_DOWN 错误，不能把其验证结果推广到兼容接口。

建议：服务取消对仍连接的请求返回明确 503 和 JSON 错误码。请求 Context 同时因服务取消而结束时也要优先识别服务停机，避免 select 随机分支继续产生空 200；客户端主动断连保留取消语义。

验收：真实 TCP HTTP 等待者已准入后取消服务，检查状态和错误码；分别覆盖全部兼容路由和客户端主动取消。现有探针只覆盖 handler，不宣称验证了代理或 Docker 停机。

## 本轮验证

- `go vet ./...`：通过。
- `npm run check`：通过，29 个测试文件 / 262 项测试，包含 lint、TypeScript 检查及 Vite 构建。
- Camoufox：项目虚拟环境运行 `python -m unittest test_main -q`，22 项测试通过。初次尝试 pytest 时环境无 pytest，随后按实际 unittest 测试结构执行，没有新增依赖；未运行有头浏览器流程探测脚本。
- `docker compose config --quiet`：通过，仅证明配置可解析。
- `git diff --check`：通过。
- 新增四项正确性探针：`-count=3` 全部稳定失败。
- `go test ./... -count=1 -timeout 600s`：退出码 0，13 个有测试包通过；server 包 189.921s，未使用 short，包含两个正式协议容量场景。这是 Windows 普通测试，不是 race 检测；不等同于线上容量验收。

> 原始诊断日志与 `.go.txt` 探针副本已于 2026-10-02 清理，不再随仓库保留；对应问题均已转为正式回归测试。R01、R02、R04 对应 `internal/server/mailbox_source_regression_test.go`，R03 对应 `internal/store/generation_invalidation_test.go`：

```powershell
go test ./internal/server ./internal/store -run "MailboxChange|RecreatedAlias|OldMailboxBatch|Compatibility|GenerationInvalidationNormalizesStoredAlias" -count=3 -v -timeout 120s
```

## 未验证项与交付边界

本轮未执行 race 检测、Linux SIGTERM、生产 Docker build/stop/restart、真实 Apple/Gmail 登录与投递、长时间生产容量测试。现有验收报告的 Linux race 历史结论未作为本轮现场验证使用。没有读取或改写生产数据库、`.env` 或 Master Key。

本轮结果是代码审查及可复现诊断材料，不是修复完成或已部署声明。修复优先级：R01/R02 取码正确性，其次 R03/R04 状态与交付错误。

## 后续修复

本报告保留审查时的原始发现与失败证据。四项问题的本地修复与最终验收见 [SYSTEM_REPAIR_2026_10_02.md](SYSTEM_REPAIR_2026_10_02.md)。
