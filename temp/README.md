# 远程开发数据库

开发实例统一使用以下远端服务，不再启动本地 MySQL / Redis。旧 `orbitbeacon-ha-dev`、`orbitbeacon-temp` 及本目录的旧数据、证书和配置已清理。

| 服务 | 地址 | 账户 / index |
| --- | --- | --- |
| MySQL | `10.147.18.93:13306` | 应用账户 `orbitbeacon_dev`；管理员仅用于建库和隔离测试 |
| Redis | `10.147.18.93:16379` | DB `2`；本项目 key 前缀 `orbitbeacon:dev:` |

## MySQL 库名

以下四库已创建，应用账户仅有这些库内的 `SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, DROP, INDEX, REFERENCES, TRIGGER` 权限。

| 用途 | database |
| --- | --- |
| 主库：业务数据 | `orbitbeacon_dev_main` |
| 主库：指标历史 | `orbitbeacon_dev_history` |
| 备库：业务数据 | `orbitbeacon_dev_standby_main` |
| 备库：指标历史 | `orbitbeacon_dev_standby_history` |

主备位于同一远端实例，只隔离数据，不提供跨主机容灾。当前实例报告版本 `26.10.0`；按实际 InnoDB、事务及持久化能力校验，不固定版本号。TCP 默认不启用 TLS，仅用于受控开发网络；显式配置 `tls` 时严格验证证书，不自动降级。

## 配置与凭据

私有目录为 `~/.orbitbeacon-remote-development/`，目录 `0700`、文件 `0600`，不提交密码或带密码的连接 URI。

- `server-mysql.json`：开发服务器配置；数据目录为同目录下 `server-data`，保留其中的 `secrets.key`。
- `mysql-development.json`：库名、应用账户及密码文件路径。
- `server-ha-mysql.json`、`server-ha-sqlite.json`：后续主备开发配置；复用相同数据目录，不可并行启动。
- `console-credentials.json`：开发控制台地址、账户和已修改的密码，勿提交。
- `mysql-admin.json`、`mysql-admin.password`：隔离测试建库所用管理员配置和凭据。
- `mysql-app.password`：应用账户凭据。
- `redis.json`、`redis.password`：Redis 的地址、DB、key 前缀和凭据。

当前实际开发监听地址为 `http://127.0.0.1:18091`，已避开被占用的 `18090`。

```sh
go run ./cmd/orbitbeacon-server serve --config "$HOME/.orbitbeacon-remote-development/server-mysql.json"

ORBITBEACON_TEST_MYSQL=1 \
ORBITBEACON_TEST_MYSQL_CONFIG="$HOME/.orbitbeacon-remote-development/mysql-admin.json" \
go test -count=1 -timeout=20m ./internal/server/storage ./internal/server/history ./internal/server/backup
```

MySQL 集成测试每次创建四个 `orbitbeacon_test_<32位UUID>_{pm,ph,sm,sh}` 库及一个仅限这四库的临时应用账户。管理员只负责准备和清理，业务连接不用管理员权限；结束只删除本次创建的库和账户，不清空上述开发库、不修改共享实例全局参数。CI 的远程检查仅在主分支 push 且配置管理员 secrets 后执行；缺少连接材料时明确报告未执行。

Redis DB `2` 已通过认证和临时 key 读写；测试 key 已删除。当前应用没有 Redis 依赖，此 index 和前缀留作后续明确需要的开发用途。禁止 `FLUSHDB` / `FLUSHALL` 或清理其他项目前缀。
