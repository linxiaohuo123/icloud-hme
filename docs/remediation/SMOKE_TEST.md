# icloud-hme 真实环境上线前 Smoke Test 20 步验收清单 (SMOKE_TEST.md)

> 本文档规范在部署至生产/预发真实环境前的冒烟验证流程。
> **前置条件**：系统已部署至测试目标机，已完成 Stage 0 数据库副本迁移验收（`scripts/release-validate-db.sh` 通过）。

---

## 严格执行顺序 (1 ~ 20 步)

真实上线前必须严格按照以下顺序执行冒烟验证（前置依赖未满足严禁跳步）：

1. **启动服务**
   - 使用生产配置启动服务二进制。
   - 观察标准输出日志无 Panic，表结构与 DDL 迁移正常完成，SQLite WAL 模式与外键约束正常启用。

2. **登录 admin**
   - 访问管理前端或管理接口执行 `POST /api/auth/login`。
   - 确认获得合法 Admin Session / Cookie。

3. **查看账号列表**
   - 调用 `GET /api/accounts`。
   - 确认所有母号加载成功，凭据状态标记正常（Active / HasCookies / HasAppPassword），无敏感密码与 Cookie 回显。

4. **读取一个测试账号收件箱**
   - 调用 `GET /api/inbox?account_id=<test_account_id>`。
   - 读取该测试账号收件箱消息列表。

5. **验证 INBOX MessageRef 正常**
   - 核验返回数据中的每封邮件实体，其邮件身份严格符合生产 identity 规范：
     - **IMAP 模式**：`provider` + `account_id` + `mailbox` + `uid_validity` + `uid`
       - `account_id` 正确对应母号；
       - `mailbox=INBOX`；
       - `uid_validity > 0`；
       - `uid > 0`；
       - `canonical message_ref` 可以成功解析并与这些字段完全一致。
     - **WebMail 模式**：`provider` + `account_id` + `thread_id`。

6. **创建/准备一个明确的测试库存 alias**
   - 在该测试母号下，通过管理界面或补货流程准备一个明确可用的测试别名（`status=available`）。

7. **external v2 allocate 领用**
   - 使用业务 API Token 调用 `POST /api/external/v2/allocate`（请求头携带 `Idempotency-Key: smoke-test-key-01`）。
   - 确认成功领用该别名，记录返回的 `lease_id` 与 `email`。

8. **相同 Idempotency-Key 重放**
   - 使用完全相同的请求体与 `Idempotency-Key: smoke-test-key-01` 再次调用 `POST /api/external/v2/allocate`。

9. **确认返回相同 allocation**
   - 核验返回与步骤 7 完全相同的 `lease_id`、`email` 与分配时间戳，未触发二次扣减，号池水位保持稳定。

10. **创建 verification request**
    - 使用业务 Token 调用 `POST /api/external/v2/verification-requests`，请求体传入步骤 7 返回的 `lease_id`。

11. **确认 baseline_ready**
    - 核验接口返回 200，状态为 `pending` 或 `ready`，且返回了采集到的 IMAP `baseline_uid` 与 `baseline_uidvalidity`。

12. **发送测试验证码邮件**
    - **CRITICAL**：必须在步骤 11 确认基线游标建立完成后，才向该别名邮箱发送测试验证码邮件（杜绝测试邮件先于基线到达）。

13. **获取验证码**
    - 调用 `GET /api/external/v2/verification-requests/:id?timeout=15`。
    - 确认在超时时间内成功捕获并提取到新发验证码。

14. **重复 GET 同 request**
    - 再次调用 `GET /api/external/v2/verification-requests/:id`。

15. **必须返回同一个 code**
    - 核验返回状态为终态 `succeeded`，提取到的验证码与步骤 13 完全一致。

16. **重启服务**
    - 向当前运行进程发送 `SIGTERM` 触发优雅关机，确认 Worker 收敛并关闭数据库后重新启动服务。

17. **同 allocation 不得再次发放**
    - 重启后，其他 Token 或无幂等键的请求调用 allocate，绝不能再次分配该别名。

18. **同 verification terminal result 不得改变**
    - 重启后，再次 GET 步骤 10 的 `request_id`，结果必须依然为 `succeeded` 终态且 `code` 不变。

19. **查看 stats**
    - 调用 `GET /api/system/stats`。
    - 确认号池已用/可用统计口径与数据库实际数据严格对齐。

20. **审计日志无泄漏**
    - 检查全量服务日志与控制台输出，确认无任何明文 Cookie、Authorization、API Token、OTP 验证码及完整邮件正文泄露。

---

## 验收结论判定

- **全量 PASS**：20 个步骤无跳步、无报错、无数据异常。
- **BLOCKER**：任何一步出现非预期状态、数据冲突或敏感信息暴露，立即中止上线，触发回滚。
