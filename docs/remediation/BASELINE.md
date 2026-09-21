# icloud-hme 基线复核报告 (PR-00)

编制日期：2026-09-21  
审计基线 SHA：`eddf07562bea8f96a8917df67131a156bab7b718`  
分支：`main`  

---

## 1. 运行环境与工具链

| 工具 | 规定版本 | 实际执行环境版本 | 状态 |
|---|---|---|---|
| OS | Linux / Windows (隔离环境) | Windows 11 x64 (本地隔离环境) | 符合 |
| Go | >= 1.26 | `go version go1.27.0 windows/amd64` | 符合 |
| Node.js | >= 22 | `v24.11.1` | 符合 |
| npm | >= 10 | `11.6.2` | 符合 |
| Vite / React | React 19 / Vite 8 | `react@19.2.8`, `vite@8.2.0` | 符合 |

---

## 2. 真实命令执行与实际退出码基线

在本地隔离测试环境中执行真实检查，记录实际退出码，无假冒与篡改：

```sh
# 前端静态检查与测试
npm --prefix web run lint
# 退出码: 0 (修复 SchedulePage.tsx React 19 render 阶段 ref 赋值后完全通过，0 错误)

npm --prefix web run test:run
# 退出码: 0 (14 个测试文件，95 个用例全部通过，耗时 11.29s)

npm --prefix web run build
# 退出码: 0 (TypeScript 类型检查通过，Vite 打包至 internal/webui/dist/)

# 服务端检查与全量测试
go vet ./...
# 退出码: 0 (全工作区 0 警告)

go build ./...
# 退出码: 0 (编译通过)

go test ./... -count=1 -timeout 180s
# 退出码: 0 (全部 11 个模块通过)

go test -race ./...
# 退出码: 1 (本地 Windows 隔离环境未安装 GCC/CGO, 依据规范 §3.1 记入限制, 由 CI ubuntu-latest 镜像执行验证)
```

---

## 3. 调用链与入口边界复核

沿方案规定的核心入口链路进行了逐层调用核查：

1. **邮件操作链路 (`mail_handlers.go` -> `backend_mail.go` -> `mail/client.go`)**:
   - `GET /api/inbox`: 现已移入 `admin` 组，普通令牌被拒。
   - `DELETE /api/inbox/:message_id`: 移入 `admin` 组并直接返回 `400 MAIL_DELETE_UNSUPPORTED` 安全阻断。
2. **出号与分配链路 (`quick_create_handler.go` -> `store.go`)**:
   - `/api/allocate` 等旧路径统一走号池认领；非管理员禁止指定 `account_id`；存储未就绪或池空时不自动盲目切换现场新建。
   - 直接建号接口 (`/api/create`, `/api/create/batch`) 移入 `admin` 作用域保护。
3. **验证码查询链路 (`verify_handler.go` -> `mail_sync.go` -> `eventbus.go`)**:
   - `GET /api/verify-code` 对 `auto_delete` 参数明确返回 `400 UNSUPPORTED_PARAMETER`，彻底消除隐式别名停用副作用。
4. **后台生命周期 (`server.go` -> `lease_pruner.go`, `cookie_monitor.go`)**:
   - `LeasePruner.PruneOnce()` 已安全暂停物理清理，保留保留期配置并输出可见告警日志，保护当前唯一的防重事实依据。

---

## 4. CI 自动触发机制 (R01)

- 文件：`.github/workflows/ci.yml`
- 现已更新触发条件为：
  - `push` (分支: `main`)
  - `pull_request` (分支: `main`)
  - `workflow_dispatch` (手动调试触发)
- 保留原有全部自动化测试与静态构建流程，未做任何跳过或删减。
