# OrbitBeacon 服务端部署与运维

本文面向生产环境部署、配置、反向代理、CDN、数据库接入与版本升级。

- Agent 部署与运维：[docs/agent.md](../docs/agent.md)
- 存储高可用、故障切换、备份与恢复：[docs/storage-and-backup.md](../docs/storage-and-backup.md)
- 系统架构与安全边界：[docs/architecture.md](../docs/architecture.md)

---

## 1. 生产部署拓扑与目录规范

```text
+-------------+   HTTPS / WSS (443)   +----------------------+   HTTP (127.0.0.1:8080)   +--------------------+
| 浏览器/Agent | --------------------> | Caddy / Nginx        | ------------------------> | orbitbeacon-server |
+-------------+  (TLS 终结 + 限流)     | (保留 Host, XFF,     |   (无缓冲 SSE, WS 透传)    +----+----------+----+
                                      |  WS Upgrade)         |                                |          |
                                      +----------+-----------+                                v          v
                                                 | (可选静态资源)                       [data_dir]    [MySQL]
                                                 v                                      0700 目录    (可选)
                                      +----------------------+
                                      | 独立静态 CDN (可选)    |
                                      | /_app/assets/*?v=... |
                                      +----------------------+
```

### 数据目录（`data_dir`）权限要求

- 须位于支持文件锁的本地可写文件系统，每个目录仅运行一个服务端实例（**切勿强行删除 `server.lock`、`-wal`/`-shm` 或恢复文件**）。
- 目录须归运行用户所有且权限为 **`0700`**；内部数据库文件与 `secrets.key` 为同用户 **`0600`** 普通文件。
- 路径及所有上级目录不得经过符号链接，上级目录须归 `root` 或运行用户所有（如在 `/tmp` 等公共目录需有 sticky 位）。

---

## 2. 配置优先级与参数参考

配置优先级：**命令行参数 > `ORBITBEACON_*` 环境变量 > YAML 文件 > 默认值**（YAML 禁止重复键、未知键与 `null`；环境变量名使用大写下划线，如 `ORBITBEACON_PUBLIC_URL`）。

| YAML 键 | 默认值 | 说明 |
| --- | --- | --- |
| `listen_addr` | `127.0.0.1:8080` | HTTP 监听地址（公网请接 HTTPS 反向代理） |
| `data_dir` | `./data` | 数据持久化目录（须满足 `0700` 与锁要求） |
| `public_url` | `http://localhost:8080` | 浏览器访问地址（见下方校验规则） |
| `max_nodes` | `500` | 节点数量上限（含待注册与已禁用节点） |
| `trusted_proxies` | `[]` | 反向代理 IP / CIDR 列表（禁止填 `/0`） |
| `geoip_lookup` | `true` | 是否向 `ipinfo.io` 查询未手填地区的 Agent 公网 IP |
| `geoip_token` / `geoip_token_file` | `""` | 可选 ipinfo token（互斥；密钥文件须为 `0600` 普通文件） |
| `web_update_interval_seconds` | `10` | 网页状态推送与轮询周期（`2–60` 秒，重启生效） |
| `metrics_listen` | `""` | 独立 Prometheus `/metrics` 与 `/metrics/nodes` 监听（无鉴权，勿暴露公网） |
| `access_log` | `false` | 请求访问日志（健康检查仅记 5xx，不记 Query/Cookie/密钥） |
| `allow_insecure_webhooks` | `false` | 允许向本机回环 IP 发送 HTTP Webhook（须同时开启下一项） |
| `notification_allow_private_networks` | `false` | 允许通知访问内网地址（云元数据地址始终拦截） |
| `database` | `{}` | 默认本地 SQLite；MySQL 与主备配置见第 5 节 |

### `public_url` 与 `trusted_proxies` 关键规则

```text
客户端请求 ─[ 直连 IP ∈ trusted_proxies? ]─┬─ 否 ──▶ 取直连 IP (忽略 X-Forwarded-For)
                                         └─ 是 ──▶ 从右向左跳过可信代理, 取首个不可信 IP
```

- **Origin 校验**：`public_url` 须填浏览器实际访问的根地址（仅协议 + 域名 + 可选端口，不支持子路径，如 `https://beacon.example.com`）。浏览器访问地址须与之完全一致，否则登录与写操作会被拒绝。
- **会话 Cookie（`orbitbeacon_session`）**：`HttpOnly`、`SameSite=Lax`、`Path=/`；仅当 `public_url` 为 `https://` 时启用 `Secure`。

### 初始化凭据

首次空库启动会自动创建管理员 `orbitbeacon` / `orbitbeacon`（首次登录强制改密）。如需预设初始账号，可在首次启动前设置环境变量：
- `ORBITBEACON_ADMIN_USERNAME`、`ORBITBEACON_ADMIN_PASSWORD`（密码 `8–1024` 字节，保留首尾空格；已有用户时自动忽略，不可写入 YAML）。

---

## 3. 部署方式

### 3.1 systemd 原生部署（Linux）

```sh
# 1. 编译或下载发布二进制 (CGO_ENABLED=0)
mkdir -p build
CGO_ENABLED=0 go build -trimpath -o build/orbitbeacon-server ./cmd/orbitbeacon-server

# 2. 创建专用用户与安装文件
sudo useradd --system --user-group --home /var/lib/orbitbeacon --shell /usr/sbin/nologin orbitbeacon
sudo install -D -o root -g root -m 0755 build/orbitbeacon-server /usr/local/bin/orbitbeacon-server
sudo install -D -o root -g orbitbeacon -m 0640 deploy/systemd/server.yaml /etc/orbitbeacon/server.yaml
sudo install -D -o root -g root -m 0644 deploy/systemd/orbitbeacon-server.service /etc/systemd/system/orbitbeacon-server.service
sudo install -d -o orbitbeacon -g orbitbeacon -m 0700 /var/lib/orbitbeacon

# 3. 编辑配置并启动 (若修改 data_dir 须同步更新 unit 的 ReadWritePaths=)
sudoedit /etc/orbitbeacon/server.yaml
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-server
sudo journalctl -u orbitbeacon-server -n 100 --no-pager
```

### 3.2 Docker Compose 部署

[`docker/compose.yaml`](docker/compose.yaml) 默认拉取 GHCR 镜像，绑定宿主机 `127.0.0.1:8080`，以只读根文件系统及非 root 身份 `65532:65532` 运行。

```sh
export ORBITBEACON_PUBLIC_URL=https://beacon.your-domain.com
export ORBITBEACON_IMAGE_TAG="$(cat VERSION)"
docker compose -f deploy/docker/compose.yaml pull
docker compose -f deploy/docker/compose.yaml up -d
docker compose -f deploy/docker/compose.yaml logs --tail=100 orbitbeacon

# 本地从源码构建镜像:
docker build -f build/server.Dockerfile -t orbitbeacon-server:local .
```

> [!CAUTION]
> 数据卷名为 `orbitbeacon-data`，日常升级/重启切勿执行 `docker compose down -v`。若挂载自定义 `server.yaml` 与 `password_file`，须只读挂载并匹配 `65532:65532` 与 `0600` 权限。

---

## 4. HTTPS 反向代理与静态 CDN

### 4.1 反向代理要点（[Caddyfile](proxy/Caddyfile) / [nginx.conf](proxy/nginx.conf)）

- **必须保持**：原始 `Host`、`X-Forwarded-For`、**WebSocket Upgrade**、**SSE 无缓冲**（`X-Accel-Buffering: no`）及长连接超时。
- **空间整理超时**：若使用控制台存储空间整理（`COMPACT`），须将 `/api/v1/data-management/compact` 的上游响应超时放宽至 **31 分钟**（参考模板已配置）。
- **示例安装**：
  ```sh
  # Caddy (自动签发证书)
  sudo install -D -o root -g root -m 0644 deploy/proxy/Caddyfile /etc/caddy/Caddyfile
  sudo caddy validate --config /etc/caddy/Caddyfile && sudo systemctl reload caddy

  # 或 Nginx (需自备证书并在 server_name/limit_req 配置登录限流)
  sudo install -D -o root -g root -m 0644 deploy/proxy/nginx.conf /etc/nginx/conf.d/orbitbeacon.conf
  sudo nginx -t && sudo systemctl reload nginx
  ```

### 4.2 静态资源 CDN 部署顺序

仅建议加速带 `?v=<版本>` 的版本化静态资产 `/_app/assets/*`（构建配置见 [Web 开发指南](../web/README.md)）：

```text
[构建产物] ── 1. 先上传 _app/assets/ 至独立发布目录 (如 /releases/2026-11-07)
                     └── 响应头: Cache-Control: immutable (1y), CORS *, CORP cross-origin
              2. 再部署并重启同次构建的 orbitbeacon-server
                     └── HTML / API / WS / SSE 始终直连原站且禁止边缘缓存
```

---

## 5. 数据库接入（SQLite 与 MySQL）

默认配置 `database: {}` 即使用 `data_dir` 下的本地 SQLite。切换至 MySQL 时须预建两个独立 database：

```yaml
database:
  primary:
    driver: mysql
    network: tcp             # 或 unix（此时不可配 tls）
    address: "db.example:3306"
    user: orbitbeacon
    password_file: /run/secrets/database-password   # 同用户 0600 文件，仅去末尾 1 个 LF
    main_database: orbitbeacon
    history_database: orbitbeacon_history
    # tls: {}                # 启用 TLS 1.2+（省略则为明文）；可选 ca_file / server_name
```

- **权限最小化**：仅需两库内的 `SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, DROP, INDEX, REFERENCES, TRIGGER`（无需 `SUPER` 或全局 `CREATE DATABASE`）。
- **引擎要求**：必须可写且使用事务型 InnoDB、`innodb_flush_log_at_trx_commit=1`；若启用 binlog 须配 `sync_binlog=1` 与 `log_bin_trust_function_creators=1`。
- **主备与运维**：异步 `database.standby` 配置、自动切换（`database.failover`）、`storage status/preview/run/disable`、数据清理及 `.tar.gz.age` 备份恢复均详见 [存储与备份手册](../docs/storage-and-backup.md)。

---

## 6. 通知网络策略与 Webhook 签名校验

- **出站安全**：默认拦截内网、回环及云元数据 IP（不使用代理或重定向）；HTTPS 与 SMTP 强制校验 TLS（非 465 端口强制 STARTTLS）。允许内网通知须开启 `notification_allow_private_networks`；向本机回环 IP 发送 HTTP Webhook 另须开启 `allow_insecure_webhooks`。
- **HMAC-SHA256 验签流程**（配置 secret 后附带 `X-OrbitBeacon-Timestamp` 与 `X-OrbitBeacon-Signature`）：

```text
签名原串 = "<X-OrbitBeacon-Timestamp>." + <未经重新编码的原始 HTTP Body>
校验逻辑 = 时间戳偏差 <= 300s && hmac.Equal(hexDecode(去 "v1=" 前缀), HMAC_SHA256(secret, 签名原串))
```

```go
ts, sig := r.Header.Get("X-OrbitBeacon-Timestamp"), r.Header.Get("X-OrbitBeacon-Signature")
sec, err := strconv.ParseInt(ts, 10, 64)
now := time.Now().Unix()
if err != nil || sec < now-300 || sec > now+300 || !strings.HasPrefix(sig, "v1=") { return false }
got, err := hex.DecodeString(strings.TrimPrefix(sig, "v1="))
if err != nil { return false }
mac := hmac.New(sha256.New, []byte(secret))
mac.Write([]byte(ts + "."))
mac.Write(body)
return hmac.Equal(got, mac.Sum(nil))
```

---

## 7. 日常健康检查与版本升级

### 健康检查

```sh
curl --fail http://localhost:8080/healthz/ready
/usr/local/bin/orbitbeacon-server health --url https://beacon.your-domain.com
# 使用私有 CA 时追加: --ca-file /secure/path/ca.pem
```

- `/healthz/live` 仅检查进程存活；`/healthz/ready` 检查接流状态与业务域可读写（历史域故障不影响业务域 ready）。

### 标准升级顺序

```text
1. 核对版本与 Schema 兼容 (当前: 主库 3 / 历史库 2)
   ├── 单机主库 schema 2 启动自动原地迁移为 3
   └── 已有 MySQL 复制元数据的 schema 2 须停服重播播种 (不可滚动升级)
2. 控制台导出完整加密备份 + 另存密码、YAML 配置、secrets.key 与旧二进制
3. 停止 Server ──▶ 替换 Server 二进制或拉取新镜像并启动
4. 验证 readiness、登录、节点上报及通知凭据
5. 再升级各主机 Agent (不可用新控制台远程升级未先经安装器迁移的旧格式 Agent)
```

> [!NOTE]
> 配置已全面统一为 YAML（JSON 是 YAML 子集，迁移旧 `server.json` 可直接复制为 `server.yaml` 并同步 `--config` 参数）。主库 3 归档与旧主库 2 归档不互通，回退时须恢复匹配的整套旧数据与旧程序。
