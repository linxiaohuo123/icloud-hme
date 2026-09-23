# icloud-hme 收件箱性能优化：Gemini 执行指令

## 任务与版本
仓库：linxiaohuo123/icloud-hme
本次静态审查基线：33870ff07e7a5c6ebfeadcb95672b6d97dfbd426
任务：改善“账号工作台 → 收件箱与验证码”的首屏、验证码补全、切换 Tab 和刷新延迟。
这不是资产/租约重构，不重新开启全项目安全整改。不得把代码注释中的“毫秒级”“0ms”当成测量结果。

先读取当前 main 与部署版本；若已变化，核对下面性能链路是否仍存在，不覆盖新提交。
创建一个 perf/inbox-loading 分支，一个 PR，连续完成，最终只做一次集中审查。
本轮可实现有针对性的计时和改动；不自动合并、部署、访问生产凭据或更换网络出口。
测试环境缺少真实 Apple 账号时，完成本地受控协议测试和实现，明确线上耗时仍未验证。

## 已核对的代码事实
1. web/src/components/inbox/InboxTableView.tsx：
   - fixedAccount 场景也通过 fetchAccountsDeduped 获得账号元数据，再由 accountCapabilityReady 放行 inbox。
   - 独立请求 /api/mailboxes；/api/inbox 默认 limit=20、days=7，UI folder=all。
   - /api/inbox 返回后，才对前最多 50 封缺 preview/body 的邮件 POST /api/messages。
   - batch 返回后合入结果，前端才对正文进行 OTP 提取。
   - moduleMessageCache 只缓存详情，组件 result 初始仍是 null。
   - 调整 alias/folder/limit/days 会触发 effect；点查询又增加 retryKey，可能重复查询。
   - 前端会 abort inbox/batch fetch，但这不能单独证明服务端的 batch 网络工作被取消。
2. web/src/pages/AccountWorkspace.tsx：
   - activeTab 条件渲染，切出 inbox 后其组件卸载，切回重新初始化。
   - 父层已请求 /api/accounts/:id，且传入 aliases；没有将 AccountSummary 直接用于子层能力判断。
   - 别名加载并非 inbox 完成的必经阻塞项，不要错误改造成 Promise.all 等待所有别名。
3. internal/server/mail_read_service.go：
   - ListInbox 仅透传 ListInboxContext，没有列表快照缓存或同键 in-flight 合并。
   - 详情缓存是 10 分钟、1000 条，不能当作列表缓存。
4. internal/server/backend_mail.go 与 internal/mail/client_fetch.go：
   - IMAP 批量正文已按 mailbox/UIDVALIDITY 分组调用 UID FETCH，不是 20 次逐封请求。
   - 正文读取使用 BODY.PEEK[]，默认请求完整 MIME，不是有字节上限的协议级摘要。
   - GetFullBatchInFolderWithValidity 会将 full.Preview 设为 full.Body。
5. internal/server/mail_handlers.go + mail_read_service.go：
   - batch 响应同时包含 messages[] 和 items[].message；
     同一封正文可能同时出现在两处的 body 与 preview 中。
   - 前端当前主要消费 messages，不能假定四份逻辑文本经过压缩后仍是四倍网络流量。
6. internal/mail/pool.go：
   - 原生 iCloud IMAP 同账号串行（sem 容量 1），maxConns=50 是全局池容量，不是每账号 50 并发。
   - 每次复用前 Ping/NOOP，冷连接包含拨号、TLS、Login。
   - 不可通过移除同连接互斥来“优化”。
7. internal/mail/client.go / client_fetch.go：
   - all 会解析目录，再逐文件夹 SELECT/FETCH；独立 /mailboxes 又执行 LIST。
   - 默认无 alias、无 SinceUID 的列表是取最近 limit，再本地按日期过滤；
     缩短 7 天不是必然减少上游工作量。
   - 有 alias 的查询另外有多标头 UID SEARCH 和最多 30 封最近邮件 fallback；
     不把这条路径套到截图“全部别名”的场景。
8. Context：
   - listInboxHandler 已经把 HTTP context 传入 backend，不要重复声称它完全没传。
   - GetMessages/GetMessage/ListMailboxes 的生产调用仍经 WithMailClient(context.Background())。
   - 对后面这些路径补真正 context 贯穿，不是加 goroutine 假取消。
9. internal/account/client_factory.go：
   - 自定义转寄 IMAP MailboxConfig 路径每次 NewClient/Connect/Disconnect，和原生 iCloud 池不同。
   - 先确认生产实际走哪条路径，不能从截图“IMAP”推断具体主机、代理或是否连接复用。
10. internal/server/mail_sync.go：
   - 仅有活跃验证码订阅时同步；与页面读信可竞争同账号池。
   - 页面自动刷新关闭不代表外部取码客户端停止工作。

## 第一阶段：先建立真实分段耗时，随后连续实施
不要先加 Redis、换数据库、提高并发或降低安全保证。

分别测量：
- 首次打开到邮件标题可见；
- 到首个验证码可见；
- 到当前页需要的验证码/预览完成或明确失败；
- 点开一个未缓存详情；
- 切出再切回同账号同筛选；
- 手动刷新直到确认拿到新结果（不能只记录旧快照可见时间）。

浏览器记录 method、路径模板、status、timing、transfer/decoded size、
initiator、canceled、请求次数、Performance long tasks。
TTFB 包含网络和服务端等待，不能仅凭 TTFB 判断为数据库慢。
HAR 默认脱敏未必去掉邮件内容；不得上传完整 HAR、OTP、正文、Cookie、Token。
优先输出只有时间和大小的表。

服务端加入可关闭/可采样计时：
request_id、路由模板、匿名账号标识、queue_wait_ms、conn_reused、
dial_ms、tls_ms、login_ms、noop_ms、list_ms、select_ms、
search_ms、fetch_header_ms、fetch_body_ms、mime_parse_ms、
serialize_ms、response_bytes、cache_hit、fetched_count、cancel_reason。
原生 IMAP 需在自己的拨号/命令边界计时，net/http/httptrace 不能自动测原生 IMAP。
不打开含原始 LOGIN/正文的协议 debug，不公开 pprof。

分开测：
- INBOX / all；
- 10 / 20 封；
- 冷连接 / 热连接；
- 首次 / 切回 / 刷新；
- 全部别名 / 指定别名；
- 小文本 / 带大附件邮件；
- 无外部取码订阅 / 一个已有订阅。
小样本如实报告 sample_count；不以两次请求宣称可靠 p95。
允许测试邮箱上的小规模只读测量，不做生产压测或自动修改代理。

## 第二阶段：第一批低侵入改进
按测量结果选主导问题，不强行全做。

A. 复用元数据与恢复列表
- fixedAccount 从父层可靠 AccountSummary 获取能力，保留能力未就绪保护。
- 目录使用账号/凭据版本隔离的短缓存或延迟加载，不抢在邮件列表前占同账号连接。
- 同账号同筛选切回时恢复内存中的列表快照，显示“上次更新/更新中”；
  正确清理过期状态、账号切换、退出登录、权限/凭据变化。
- 列表快照缓存与完整正文缓存分离；不把已有全文 Map 当成列表已缓存。
- 同一查询使用规范化 query key，包括账号、provider/能力、folder、alias、days、limit；
  请求取消或报错不能写入可复用成功缓存。
- 同时命中 cache + 后台 revalidate 是旧数据可见更快，不可声称邮件同步已经实时完成。

B. 一个用户意图只发一轮查询
- 选定一种交互：筛选立即生效并将按钮定义为刷新，
  或筛选只更新 draft、查询时提交。
- 不让“修改筛选 + 点击查询”触发重复的同语义任务。
- 刷新不能与仍在运行的正文补全无限重叠。
- 单账号不做高频 poll；页面不可见时暂停无意义自动刷新。
- 详情和预览独立 loading/error；正文失败不要静默伪装为“没有验证码”。

C. 不要把当前页全部正文绑在最慢一封上
- 保留先展示列表；不要简单把 /api/inbox 全局改成 body=true。
- 只处理当前可见页中尚未补全的邮件；首批大小（例如 4–6）作为实验参数，不是既定性能结论。
- 同账号采用小批顺序补全，每批到达即可更新，给其他请求释放调度机会；
  不在一个 IMAP 会话上同时并发 SELECT/FETCH。
- 标题已可识别 OTP 的先显示，但不能把“标题无关键字”当成永远不读取正文的理由。
- 服务端对已缓存项可立即利用，合并相同 in-flight 读任务时：
  某个调用方取消不能中止仍有调用方需要的任务，
  全部调用方离开才取消共享底层工作，并有总超时。
- 失败、限流、取消均保持明确语义和有界重试。

D. 真正贯穿取消
- 将 HTTP context 传入 GetMessage/GetMessages/ListMailboxes 的生产网络路径。
- 取消必须覆盖等待连接、执行、解析；握手未能即时取消的范围明确报告。
- 已取消任务不能继续长时间占用同账号连接。
- 保留健康连接复用；不把正常完成后的 watcher 竞态当成理由随意关闭别的请求正在用的连接。
- 不借机重写整套网络客户端。

## 第三阶段：只有测量证明有必要，再做协议/队列改进
A. 轻量预览
- UI 预览与完整详情分开，避免首屏为了几位 OTP 拉完整带附件 MIME。
- 按 BODYSTRUCTURE/文本 MIME part 选择内容，使用 BODY.PEEK 与有界读取。
- 支持纯文本、HTML、multipart/alternative、嵌套 MIME、base64、quoted-printable、多字节字符。
- 原始 MIME 直接截前 N KB 可能截不到正文或破坏编码，不能把截断解析失败说成“无验证码”。
- preview/body_complete/truncated 等状态清楚；需要时受控读取后续文本部分；
  点击详情仍走完整详情路径。
- 所有正文缓存/差集/结果合并使用 canonical MessageRef，
  不引入裸 UID、数组下标或邮箱地址作为全局邮件 ID。
- 消除 preview=整个 body、messages 和 items 重复嵌套全文造成的 JSON 冗余。
  先检查外部客户端兼容性；如调整响应，应兼容旧默认格式或使用明确的轻量模式，
  不为减字节擅自破坏 /api/messages 公开契约。
- 正文缓存按条数与总字节双边界管理；flags 可变，不把整封状态永久缓存。

B. 文件夹与连接
- 目录缓存去掉同一次页面的重复 LIST；不要删除 UIDVALIDITY 校验来减少 SELECT。
- NOOP 从“每次借用必做”改为有证据的空闲健康检查策略，
  或在确定请求未完成时安全重试只读命令；不要把不确定的写操作重试进去。
- 自定义 IMAP 是否纳入池，只在真实使用且建连开销明显时实施。
  池 key 至少区分服务器、端口、账号/用户名、凭据版本与代理等有效身份。
- 若 queue_wait 明显主导，再评估 OTP/交互读取的优先级，或严格有界的两条独立连接。
  不是简单把 sem 改大；必须是各自串行的不同会话。
- 总连接、每账号连接及并发都要有上限，避免让 Apple 承担放大请求。

## 不变量与禁止事项
- 不修改资产归属、租约、库存准入、token owner 或数据库 migration。
- 不放开邮件物理删除；不恢复 auto_delete。
- UI 缓存不是 v2 VerificationRequest 的时效保证。
- v2 保留 mailbox + UIDVALIDITY + inclusive UIDNEXT、收件人核验、撤销复查与 CAS。
- 授权发生在返回缓存之前；退出登录要清理会话范围的前端邮件缓存。
- 不用缓存污染、旧验证码回放或漏查垃圾箱换取所谓提速。
- 不能全局偷偷把 all 改为 INBOX；若加快速 INBOX 入口，要明确其范围差异。
- 不改代理出口、不增加生产负载、不请求真实私人账号测试。
- 不引入 Redis/Kafka/常驻全库邮件镜像；本轮不升级依赖、不做 UI 外观重构。
- 不做公网端口扫描，不请求截图中的服务尝试获取管理权限。

## 回归与验收
必须有真实生产函数/受控协议测试，而非只让 fakeBackend sleep：
1. 同键同一用户动作只有一轮逻辑请求；取消后旧账号响应不污染。
2. Tab 返回可恢复同账号快照，退出登录清除，跨账号不共享。
3. /mailboxes 不阻塞优先列表；目录缓存作用可验证。
4. 一封大附件不会阻止其它已完成预览显示。
5. 缓存命中不重复取同一完整正文，部分正文不冒充完整。
6. HTTP 取消后 /api/messages 的底层取信与队列等待确实结束。
7. 中文/日文/英文 OTP、HTML/纯文本/编码/MIME 回归。
8. UIDVALIDITY 变化、同 UID 不同账号/文件夹仍隔离。
9. 与外部取码并行时，原有 OTP 正确性与等待上限没有回退。
10. 合并 fetch 的多个调用方中取消一个，不影响其他调用方。
11. 记录 headers/body、上游字节、NOOP/LIST/SELECT/FETCH 次数与队列时间，
    保留优化前后同环境对照，而不是只贴 CI 绿灯。

完整门禁沿用仓库实际脚本：Go test/race/vet/build 与 Web lint/test/build。
本机不能跑的标 NOT_RUN，只以 final SHA 对应 Linux CI 补证。
性能目标必须注明环境和样本，不承诺无条件 0ms/秒开。
暖快照 200ms 以内可以作为测试环境目标，但不能代表实时新邮件可见延迟。

## 最终交付
只在完成或真实 blocker 时汇报，不连续发送“正在跑测试”。
报告包含：
- baseline/final SHA、改动文件、PR 与 CI；
- 每项优化对应的已测瓶颈；
- cold/warm/tab/OTP/detail 的前后耗时、样本数、payload 字节、上游命令数；
- 测试环境与真实 Apple 环境分别验证了什么；
- 未测项目明确“数据不足”；
- 兼容性影响；
- 本轮没有修改资产/权限/OTP 不变量，没有自动合并或部署。

## 核心代码位置索引
- web/src/components/inbox/InboxTableView.tsx
- web/src/pages/AccountWorkspace.tsx
- web/src/pages/workspace/WorkspaceInboxTab.tsx
- web/src/hooks/useAccounts.ts
- web/src/api/client.ts
- web/src/utils/sniffer.ts
- internal/server/mail_handlers.go
- internal/server/mail_read_service.go
- internal/server/backend_mail.go
- internal/server/mail_sync.go
- internal/account/client_factory.go
- internal/mail/client.go
- internal/mail/client_fetch.go
- internal/mail/pool.go
- internal/mail/mime.go

本提示词基于源码静态审查，不包含生产性能实测；
不能直接将其中“候选瓶颈”改写成“已经测得的性能根因”。
