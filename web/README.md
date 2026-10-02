# web/ — icloud-hme 管理控制台前端

React + TypeScript + Vite 单页应用。构建产物输出到 `../internal/webui/dist`，由 Go 服务通过 `//go:embed` 内嵌并提供单页回退，生产环境无需单独部署前端。

## 常用命令

```bash
npm ci              # 安装依赖
npm run dev         # 开发服务器，/api 代理到 http://127.0.0.1:8081 (需先启动后端)
npm run check       # 发布前检查：lint + 单元测试 + 类型检查与构建
npm run build       # 仅构建，输出到 ../internal/webui/dist
npm run test        # vitest 监听模式
```

## 目录

- `src/api/`：请求客户端 (CSRF 注入、401 拦截) 与 API 类型，类型需与 `internal/server` 契约保持一致
- `src/auth/`：管理员会话探测、登录与登出
- `src/pages/`、`src/components/`：页面与组件
- `src/test/`：测试工具与 MSW 模拟服务

## 说明

- 打包脚本 (`scripts/package-*.ps1`、`build.sh`) 构建前会清理 `internal/webui/dist` 中的旧资源，保留受 git 跟踪的 `placeholder.txt`，保证未构建前端时 Go 包仍可编译。
- 本目录的 `go.mod` 只是模块边界，用于阻止根模块的 `go ... ./...` 扫描 `node_modules` 中第三方附带的 Go 源码，与前端构建无关。
