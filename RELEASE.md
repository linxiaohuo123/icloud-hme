# iCloud HME 发布与升级手册 (Release Runbook)

## V1 → V2 升级流程 (Upgrade Runbook)

适用于从旧版本 (Schema V1，含明文凭据/Token) 升级到当前版本 (Schema V2，AES-256-GCM 凭据加密与 Token 哈希存储)。

1. **停止运行中的旧服务**：
   ```bash
   systemctl stop icloud-hme
   # 或 docker compose down
   ```

2. **执行离线只读一致性备份**：
   使用新版本二进制执行 non-mutating 离线备份（绝对不会修改或提前迁移源数据库）：
   ```bash
   ./icloud-hme -data ./data -backup ./pre-upgrade-v1.db
   ```

3. **妥善保管备份文件**：
   将 `pre-upgrade-v1.db` 保存到安全、受访问控制的独立位置（注意：此备份包含历史明文凭据）。

4. **配置 Master Key**：
   生成严格 32 字节 (256-bit) Base64 编码的 Master Key 并配置到环境变量或 `.env` 中：
   ```bash
   # 生成密钥
   openssl rand -base64 32
   # 写入配置
   echo "ICLOUD_HME_MASTER_KEY=<生成的主密钥>" >> /etc/icloud-hme.env
   ```
   > ⚠️ **安全红线**：Master Key 独立于数据库存储，请务必将其持久化备份至密码管理器或安全密钥库中。如果 Master Key 丢失，升级后的 V2 加密凭据将永久无法恢复！

5. **启动新版本服务**：
   ```bash
   systemctl start icloud-hme
   # 或 docker compose up -d
   ```

6. **自动完成数据迁移 (V1 → V2 Migration)**：
   服务启动时将自动执行：
   - 提取并重构 `api_tokens` 为不可逆 SHA-256 哈希存储；
   - 使用 AES-256-GCM + AAD 认证加密账号凭据 (Cookies / App Password / Mailbox / Proxy) 与通知配置；
   - 执行 WAL checkpoint 与页面重整 (VACUUM)，抹除磁盘上的历史明文残留；
   - 将数据库版本标记为 `user_version = 2`。

7. **健康检查确认**：
   检查就绪探针确保服务正常就绪：
   ```bash
   curl -fsS http://127.0.0.1:8081/readyz
   ```

8. **确认账号凭据正常读取**：
   登录管理员后台，检查各账号列表与状态，确保凭据解密正常且无告警。

9. **确认存量 Legacy Token 仍可通过认证**：
   使用迁移前的外部 API Token 调用测试接口，确认历史令牌依然能够正常通过认证。

10. **生产稳定后安全处置历史备份**：
    在生产环境稳定运行确认无误后，对包含历史明文凭据的 `pre-upgrade-v1.db` 以及 `data/backups/pre-migrate-v1-to-v2-*.db` 副本进行加密归档或安全销毁。

---

## 回滚指南 (Rollback Runbook)

如果 V2 数据库已完成迁移，**绝对不能**直接将 V2 数据库交由旧版 V1 二进制运行（V1 程序无法解密 V2 凭据，且缺少明文 token 列）。

若必须降级回旧版本，请按以下步骤操作：

1. **停止 V2 服务**：
   ```bash
   systemctl stop icloud-hme
   # 或 docker compose down
   ```

2. **恢复迁移前的 V1 备份**：
   使用新版或旧版工具将备份还原至数据目录，例如使用新版恢复命令：
   ```bash
   ./icloud-hme -data ./data -restore ./pre-upgrade-v1.db
   ```
   或直接使用文件替换：
   ```bash
   cp ./pre-upgrade-v1.db ./data/icloud_hme.db
   rm -f ./data/icloud_hme.db-wal ./data/icloud_hme.db-shm
   ```

3. **启动旧版 binary / 容器**：
   ```bash
   # 启动旧版服务
   systemctl start icloud-hme
   ```
