# 项目环境与运维规范

## 部署模式
- **服务器生产环境默认使用 Docker Compose 部署**。
- 编排文件: `docker-compose.yml`
- 容器名称: `icloud-hme`
- 挂载持久化路径: `./data:/app/data` (存储 `icloud_hme.db`)

## 标准更新与重启命令
```bash
git pull origin main
docker compose up -d --build
docker compose logs -f --tail=50
```

## 健康检查验证
```bash
curl -i http://127.0.0.1:8081/livez
```
