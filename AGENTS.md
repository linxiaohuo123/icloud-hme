# 项目环境与运维规范

## 部署模式
- **服务器生产环境默认使用 Docker Compose 部署**。
- 编排文件: `docker-compose.yml`
- 核心服务容器:
  - `icloud-hme`: 主服务容器（Go 编译产物，Alpine 运行时，UID 10001）
  - `icloud-camoufox-agent`: 反检测无头浏览器自动化上号服务容器（Python 3.11，纯无头模式，shm 2GB）
- 挂载持久化路径: `./data:/app/data` (存储 `icloud_hme.db`)
- **权限安全提示**: Linux 服务器首次启动前需确保数据目录属主属于容器用户:
  ```bash
  chown -R 10001:10001 ./data
  ```

## 标准更新与重启命令
```bash
git pull origin main
docker compose up -d --build
docker compose logs -f --tail=50
```

## 健康检查验证
```bash
# 1. 主服务健康检查
curl -i http://127.0.0.1:8081/livez

# 2. Camoufox 反检测代理健康检查
docker compose exec camoufox-agent python3 -c "import os,urllib.request; req=urllib.request.Request('http://127.0.0.1:8089/health', headers={'X-Camoufox-Token': os.environ['ICLOUD_HME_CAMOUFOX_TOKEN']}); print(urllib.request.urlopen(req, timeout=12).read().decode())"
```
