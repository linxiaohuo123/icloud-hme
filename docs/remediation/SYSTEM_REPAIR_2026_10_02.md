# 系统审查修复验收

日期：2026-10-02。对应原始报告 `SYSTEM_REVIEW_2026_10_02.md` 的 R01–R04。

## 结果

四项已复现缺陷已修复，新增回归测试覆盖真实 Worker、VerificationService、SQLite 与 TCP HTTP handler。修复仅在本地工作区；保留既有未提交修改，未提交、推送、部署或操作真实账号、数据库、`.env`、Master Key。

| 问题 | 最终行为 | 回归证据 |
| --- | --- | --- |
| R01：重建任务漏码 | 扫描覆盖绑定当前活跃任务集合及当前基线；新增任务和观察集合变化触发必要回扫，回收历史覆盖 | 正式创建服务在 Worker 快照之后入库，同别名的新 UID 21 在下一轮成功持久化，且无 GET 订阅者 |
| R02：换收件邮箱串历史码 | 任务持久化物理来源及基线账号；完成 CAS、事件边界、发布去重与精确消费均隔离来源；基线、分页、正文使用冻结配置 | 不同邮箱相同 UIDVALIDITY 时旧任务失效；在途旧扫描不能完成/失效后来创建的新来源任务；基线采集中切换配置不会混用来源；GET 可独立发现来源变化 |
| R03：历史 alias 失效遗漏 | 批量 UIDVALIDITY 失效与查询/完成统一使用 `LOWER(TRIM(alias_email))`，来源过滤防止旧邮箱扫描影响新来源 | 带大小写及空白的历史 alias 正确更新为 invalidated，跨来源代际失效不误伤 |
| R04：停机空 HTTP 200 | 已接纳的兼容取码等待者在服务取消时返回 `503 SERVER_SHUTTING_DOWN`；请求 Context 同时取消时也识别停机 | 六个兼容路由真实 TCP HTTP 等待后取消服务，验证状态码/错误码；客户端主动取消正常释放订阅 |

配置快照测试还发现原 `copyAccount` 共享 `Mailbox` 指针，已补齐该配置的深拷贝。邮箱来源由规范化地址、IMAP 主机与端口构成，不包含明文凭据。连接指纹额外包含授权码及代理；仅轮换授权码/代理不会使同邮箱任务失效。

来源失效只更新配置快照之前已观察到的任务 ID，避免用旧批次的来源全量更新别名而误伤新任务。配置切换的发现发生在后台扫描或 GET 来源复查；已按冻结旧来源完成并持久化的终态保留。

## 数据库升级与兼容

数据库版本升至 v12，新字段 `baseline_source`、`baseline_account_id` 仅供内部使用，外部 JSON 不增加这些字段。

启动时先保留迁移前一致性备份，再事务升级。来源未知的旧 `pending` / `ready` 记录标记为 `invalidated`，客户端需要重新创建任务。`succeeded`、`expired`、`invalidated` 终态与原结果保留。迁移失败时列、状态及版本全部回滚；成功后重启幂等。

失效继续使用已有 `409 UIDVALIDITY_CHANGED`，消息明确包含物理来源变化，避免新增外部错误码。README 与 API 文档已同步。

历史中间态迁移测试同步了这一明确的新语义，并继续核验原别名/租约等业务字段及终态 CAS。索引迁移测试的人工降版 fixture 带已知来源，仍验证索引升级不改变其记录，真实无来源 v11 fixture 独立验证失效行为。

## 验证

- 全库 `go test ./... -count=1 -timeout 600s` 通过：server 194.021 秒，store 11.900 秒；包含已有并发规模场景。
- 关键修复、来源隔离、迁移与快照回归连续三轮通过。
- 全库通过后，收尾增加同 MessageRef 不同来源的精确消费隔离；最终 mail/server 相关模块的非规模回归通过（mail 4.301 秒、server 61.170 秒）。
- 最终 `go vet ./...`、`git diff --check` 通过。
- 原始失败探针、审查日志及中间修复失败记录已于 2026-10-02 清理，不再保留。

本次修复没有改变前端与 Camoufox 代码，因此未重复此前审查已通过的前端与 Python 验证。未运行 race 检查，未验证真实提供商行为、代理转发或服务器 Docker 停机；本地通过不代表已经部署上线。

## 同日后续修复与清理

在上述修复之后的复审中又处理了以下问题，均附带回归测试，并确认测试在撤掉修复后会失败：

| 问题 | 修复 | 回归测试 |
| --- | --- | --- |
| 无正文请求超过 15 秒被取消 | `bodyLimitMiddleware` 只对有正文请求设置连接读取期限；net/http 后台读超时不再误取消 GET/DELETE/空 POST | `body_deadline_regression_test.go` |
| 旧代际缓存事件或旧扫描边界失效新任务 | 事件代际不一致时以当前邮箱边界确认；Worker 代际失效仅作用于边界读取前已观察的任务 ID | `stale_generation_regression_test.go` |
| 旧事件占用订阅通道时有效验证码被挤掉 | 忽略旧事件后回查 `EventBus.CachedMatch` 与数据库终态；订阅时缓存可用验证码优先 | 同上，及 `internal/mail/eventbus_test.go` |
| 基线母号删除后任务持续 500 | 来源复查发现账号不存在时持久化失效，返回 409 | `stale_generation_regression_test.go` |
| 监听失败以退出码 0 退出 | `Run` 返回错误时非零退出 | 手工验证：端口占用时退出码 1 |
| 停机等待准入计数无上限 | handler 全部返回后最多等待 2 秒，泄漏时记录日志并继续关闭资源 | `shutdown_drain_regression_test.go` |
| 管理员会话与全局 API Key 共用限流桶 | 限流按凭据来源分桶，主体 ID 与资源归属不变 | `request_limits_test.go` |
| 不携带来源的旧完成接口静默失效 | 删除 `CompleteVerificationRequest` / `UpdateVerificationRequestResult` | 既有测试改用显式接口 |

清理：按审查轮次命名的测试文件与函数改为按功能命名，`EventBus` 订阅优先级与 `web/go.mod` 模块边界调整，`API.md` 补齐令牌轮换、单账号查询、Camoufox 状态与查信路径形式，存量文件统一 gofmt。`review_*/` 原始日志与探针副本已删除，`.gitignore` 保留该规则以防再次误提交。

验证：Windows 全量 `go test ./...`、`go vet ./...` 通过；Linux (WSL) `go test -race ./...` 全量通过且无数据竞争，含真实 SIGTERM 停机测试；前端 `npm run check` 通过；生产二进制冒烟与真实数据库副本 v8→v12 迁移通过。未验证真实 Apple/IMAP/代理/通知渠道。
