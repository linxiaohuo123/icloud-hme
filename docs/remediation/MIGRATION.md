# icloud-hme 数据库迁移与历史兼容规范 (MIGRATION.md)

---

## 1. 架构目标与单一真相源

在 PR-03 ~ PR-08 改造之前，系统存在两套库存真相源（`lease_records` 与 `alias_inventory`），且出号逻辑与取码逻辑耦合了脆弱的裸 `token_name` 与共享缓存。

本次重构确立了**单一权威库存真相源**：
- **`alias_inventory`**：权威物理别名库存表（包含 `status`, `allocation_state`, `account_id`, `created_at` 等）。
- **`alias_allocations`**：权威别名认领/分配租约表（包含 `allocation_id`, `alias_email`, `owner_kind`, `owner_id`, `business_tag`, `allocated_at`, `status`）。
- **`operations`**：幂等异步操作记录表（通过 `(owner_kind, owner_id, idempotency_key)` 唯一索引保证不可变幂等流转）。
- **`verification_requests`**：持久化取码意图与状态机表（包含 `request_id`, `lease_id`, `baseline_uidvalidity`, `baseline_uid`, `code`, `status`）。
- **`lease_records`**：降级为只读审计兼容表，**严禁**再作为库存是否可用的依据。

---

## 2. DDL 迁移拓扑与执行顺序

根据 SQLite 事务语义与依赖关系，DDL 升级在 `internal/store/store.go` 中按拓扑分层执行：

1. **基础表构建**：
   - 创建 `accounts`, `aliases`, `tokens`, `sessions`, `lease_records`, `business_tags`。
2. **表结构增量平滑升级 (ALTER TABLE)**：
   - 执行 `PRAGMA table_info(table_name)` 检查列是否存在；
   - 若缺失，动态补齐 `tokens.scopes`（默认赋予 `allocate,verify`）等关键列。
3. **覆盖索引建立**：
   - `idx_tokens_token` (API Token 恒时查询索引)
   - `idx_lease_records_account_email`
   - `idx_lease_records_status_completed`
4. **领域表初始化 (`initInventorySchema`)**：
   - 创建 `alias_inventory`，建立 `idx_alias_inventory_status` (覆盖 `status, allocation_state, business_tag`)；
   - 创建 `alias_allocations`，建立 `idx_alias_allocations_owner` (覆盖 `owner_kind, owner_id`)；
   - 创建 `operations`，建立唯一索引 `idx_operations_idemp`；
   - 创建 `verification_requests`，建立索引 `idx_vreq_lease_id` 与 `idx_vreq_principal`。
5. **历史数据单事务原子回填 (`migrateInventory`)**：
   - **单事务包裹**：读取旧数据并在同一事务内写入，若失败立即回滚，绝不产生半同步状态。
   - **库存初始化**：将已有 `aliases` 同步插入 `alias_inventory`，默认 `allocation_state='available'`。
   - **分配与租约回填**：
     扫描 `lease_records` 中已领用的别名。针对每条历史流水：
     - 若 `token_name` 对应全局唯一的非重名 Token，则将 `owner_kind='token'`, `owner_id=tok.ID`；
     - 若 `token_name` 重名、已删除或为 `scheduler` 定时产出，**强制标记为 `owner_kind='token', owner_id='legacy_unknown'`**，彻底阻断新建同名 Token 越权继承历史别名！
     - 更新 `alias_inventory` 的 `allocation_state='allocated'`。
   - **出号路由回填**：
     从 `lease_records` 提取 `(account_id, email)` 并写入 `alias_routes`，消除服务重启后对上游母号的盲扫。

---

## 3. 兼容期过渡策略与防破坏保证

1. **出号路径完全收口**：
   - `/api/quick-create`, `/api/alias/lease`, `/api/allocate`, `/api/external/v1/allocate`, `/api/external/v2/allocate` 全部统一调用 `AliasAllocationService`；
   - 外部令牌在号池耗尽时，服务端统一返回 `503 POOL_EMPTY` 并附带 `Retry-After: 60` 响应头，彻底阻断远程隐式现场建号；
   - 即使手动清理或截断 `lease_records`，所有出号入口依然以 `alias_inventory` 与 `alias_allocations` 为准，历史别名绝对不会被二次重复发放。
2. **取码路径统一**：
   - 外部 v2 取码统一使用 `POST /api/external/v2/verification-requests` 采集基线游标，并在 `GET /api/external/v2/verification-requests/:id` 提取验证码；
   - 旧 `GET /api/verify-code` 保留作为兼容只读入口，但受主体归属核验强约束，且显式传 `auto_delete=true` 时返回 400 明确拒绝。

---

## 4. 回滚与数据备份建议

1. **备份**：
   升级前备份 `dataDir` 下的 `icloud_hme.db` 文件。
2. **回滚**：
   若需紧急回滚至旧版本，新增加的表（`alias_inventory`, `alias_allocations`, `operations`, `verification_requests`）对旧版本代码完全透明（旧版不访问这四张表），只需恢复备份数据库即可。

---

## 5. 中间态 Schema 自愈与 Backfill 保证 (P0-9)

针对从 PR-04 ~ PR-07 中间态版本升级的数据库：
1. **精准字段探测 (`ensureColumn`)**：
   - 彻底废除旧版无保护的 `_, _ = s.db.Exec("ALTER TABLE ...")` 盲吞错模式。
   - 使用 `PRAGMA table_info` 预先探测列是否存在，字段已存在时安全跳过；真正执行 ALTER 失败时阻断 Store 初始化，杜绝半迁移静默运行。
2. **`alias_allocations` 缺失 `account_id` 平滑补列与 Backfill**：
   - 当历史中间态缺少 `account_id` 时，自动补充 `account_id TEXT NOT NULL DEFAULT ''`；
   - 顺序从 `alias_inventory`、`alias_routes` 与 `lease_records` 权威关联回填真实母号 ID，保证通过 `GetPrincipalAllocation` 等接口查询时字段完整且不破坏所有权边界。
3. **`operations` 与 `verification_requests` 终态对齐**：
   - `operations` 幂等自愈补充 `request_hash`, `candidate_email`, `result_ref`, `error_code`；
   - `verification_requests` 自愈补充基线五要素字段与匹配引用；
   - 幂等建立覆盖索引 (`idx_alias_inv_acc_alloc`, `idx_alias_alloc_owner`, `idx_operations_lookup` 等)，确保全量查询走索引。

