# 远程开发数据库

默认使用 TLS，不在本地部署 MySQL / Redis。

| 服务 | TLS（当前开发） | 明文（另一测试端点） |
| --- | --- | --- |
| MySQL | `10.147.18.93:13307` | `10.147.18.93:13306` |
| Redis | `10.147.18.93:16380` | `10.147.18.93:16379` |

CA 为 `temp/server.crt`，私有配置使用其副本 `~/.orbitbeacon-remote-development/server.crt`；`server_name` 为 `10.147.18.93`。严格校验证书，不跳过校验、不自动降级。证书有效至 2029-02-09，更新证书时同步私有副本。

## database / index

| 用途 | MySQL database |
| --- | --- |
| 主库：业务 | `orbitbeacon_dev_main` |
| 主库：历史 | `orbitbeacon_dev_history` |
| 备库：业务 | `orbitbeacon_dev_standby_main` |
| 备库：历史 | `orbitbeacon_dev_standby_history` |

应用账户为 `orbitbeacon_dev`，权限限于四库。MySQL `13306` 与 `13307` 是两个独立实例，可用于异实例主备测试；同在一台主机，不提供跨主机容灾。**当前开发配置的主备四库仍全部位于 TLS 实例 `13307`，尚未切为异实例主备。** 原开发数据已完整迁入 `13307` 同名库；两个实例不自动同步，勿交替写入。`13306` 为明文端点，不能直接用于要求严格 TLS 的 TCP 配置。

Redis 使用 **DB `2`**、key 前缀 **`orbitbeacon:dev:`**。TLS 认证、临时 key 写入/读取/删除已验证。应用目前没有 Redis 依赖；禁止 `FLUSHDB` / `FLUSHALL` 或清理其他项目前缀。

## 私有配置与启动

配置位于 `~/.orbitbeacon-remote-development/`（目录 `0700`、文件 `0600`），密码只由文件引用，不提交带密码的 URI。

- `server-ha-mysql.json`：当前 TLS 主备配置；运行目录 `server-data-tls`，保留其中的 `secrets.key`、`storage-control.db`，不要混用旧目录 `server-data`。
- `mysql-development.json` / `mysql-app.password`：TLS 应用连接、四库名称与应用凭据。
- `mysql-admin.json` / `mysql-admin.password`：TLS 管理连接，仅供隔离测试库使用。
- `redis.json` / `redis.password`：TLS 连接、DB `2` 和前缀。
- `console-credentials.json`：控制台账户；`*-plain.json` 保留迁移前配置，仅用于明确回退。

```sh
go run ./cmd/orbitbeacon-server serve --config "$HOME/.orbitbeacon-remote-development/server-ha-mysql.json"
```

控制台：`http://127.0.0.1:18091`。MySQL 集成测试设置 `ORBITBEACON_TEST_MYSQL=1`、`ORBITBEACON_TEST_MYSQL_CONFIG="$HOME/.orbitbeacon-remote-development/mysql-admin.json"`，使用 `go test -p 1` 串行运行，避免耗尽远端连接。夹具只清理本次 `orbitbeacon_test_<UUID>_{pm,ph,sm,sh}` 四库和临时账户，不清空开发库。

