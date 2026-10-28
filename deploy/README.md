# Server 部署

提供 **systemd** 和 **Docker Compose** 两种方式。以下命令从仓库根目录执行；Agent 安装见 [Agent 指南](../docs/agent.md)。

## 部署前

- 准备 Linux 主机、可用域名和可信 HTTPS（或自行分发私有 CA）。
- SQLite 数据必须放在支持文件锁的本地文件系统，不要共用数据目录运行多个 Server。
- 默认由同机反向代理终结 TLS，业务端口 `8080` 不对公网开放。
- `public_url` 填浏览器实际访问的 HTTPS 源站，包含非默认端口。配置优先级为 CLI > `ORBITBEACON_*` 环境变量 > JSON > 默认值；未知 JSON 键会报错。

配置示例分别为 [`systemd/server.json`](systemd/server.json) 和 [`docker/server.json`](docker/server.json)。原生示例监听 `127.0.0.1:8080`，容器监听 `0.0.0.0:8080`，但只发布到宿主机回环地址；数据目录均为 `/var/lib/orbitbeacon`。

## systemd

### 安装、初始化

已有匹配架构的 `build/orbitbeacon-server` 可跳过编译；Go 版本以 [`go.mod`](../go.mod) 为准：

```sh
mkdir -p build
GOTOOLCHAIN=go1.27.3 CGO_ENABLED=0 go build -trimpath \
  -o build/orbitbeacon-server ./cmd/orbitbeacon-server

# 已有该账号时跳过 useradd。
sudo useradd --system --user-group --home /var/lib/orbitbeacon \
  --shell /usr/sbin/nologin orbitbeacon
sudo install -D -o root -g root -m 0755 \
  build/orbitbeacon-server /usr/local/bin/orbitbeacon-server
sudo install -D -o root -g orbitbeacon -m 0640 \
  deploy/systemd/server.json /etc/orbitbeacon/server.json
sudo install -D -o root -g root -m 0644 \
  deploy/systemd/orbitbeacon-server.service \
  /etc/systemd/system/orbitbeacon-server.service
sudo install -d -o orbitbeacon -g orbitbeacon -m 0700 /var/lib/orbitbeacon
sudoedit /etc/orbitbeacon/server.json
```

至少修改 `public_url`。若更改数据目录，需同步调整 unit 的可写路径和目录权限。unit 不授予 capabilities；推荐由代理绑定 `443`，不要直接改成低端口。

```sh
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server bootstrap \
  --config /etc/orbitbeacon/server.json --username admin
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-server
sudo journalctl -u orbitbeacon-server -n 100 --no-pager
```

按提示输入管理员密码；已有管理员时不能重复初始化。初始化、备份和恢复使用服务账号，除 `backup --online` 外均应在停服后操作同一数据目录。

## Docker Compose

Compose 使用 GHCR 镜像，不在部署机编译。当前 Compose 的回退标签为 `0.1.0`，**不随 `VERSION` 自动更新**；请显式选择实际已发布的版本并持久保存环境变量，不依赖 `latest`。

```sh
export ORBITBEACON_PUBLIC_URL=https://beacon.your-domain.com
export ORBITBEACON_IMAGE_TAG="$(cat VERSION)"  # 先确认该版本已发布
# 私有包需先执行 docker login ghcr.io。
docker compose -f deploy/docker/compose.yaml pull

docker compose -f deploy/docker/compose.yaml run --rm --no-deps \
  --entrypoint /usr/local/bin/orbitbeacon-server orbitbeacon \
  bootstrap --config /etc/orbitbeacon/server.json --username admin
docker compose -f deploy/docker/compose.yaml up -d
docker compose -f deploy/docker/compose.yaml ps
docker compose -f deploy/docker/compose.yaml logs --tail=100 orbitbeacon
```

仅首次部署执行 `bootstrap`。数据卷固定名为 `orbitbeacon-data`，不要执行 `down -v` 或删除该卷。容器以 `65532:65532` 运行；改用 bind mount 时，数据目录须归该身份所有、权限为 `0700`。

`deploy/docker/server.json` 在构建时复制进镜像，并非运行时挂载。修改仓库中的该文件不会改变现有容器；额外设置需在 Compose 的 `environment` 中传入对应 `ORBITBEACON_*`，或只读挂载完整配置到 `/etc/orbitbeacon/server.json`。宿主机 `export` 的变量只有被 Compose 引用或显式传入时才会进入容器。环境或镜像标签变更后执行 `up -d` 重建容器。

镜像构建文件为 [`build/server.Dockerfile`](../build/server.Dockerfile)；发布流水线使用根目录 `VERSION`，发布逻辑见 [quality.yml](../.github/workflows/quality.yml)。流水线配置不代表对应版本已在 GHCR 或 GitHub Release 可用。

## HTTPS 与反向代理

选择 [`proxy/Caddyfile`](proxy/Caddyfile) 或 [`proxy/nginx.conf`](proxy/nginx.conf)，先修改域名和证书路径。Caddy 示例自动申请证书，要求 Caddy 2.10.0+；nginx 示例需要已有证书。

```sh
# Caddy：修改示例后安装、校验、重载。
sudo install -D -o root -g root -m 0644 deploy/proxy/Caddyfile /etc/caddy/Caddyfile
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy

# 或 nginx：该文件需由 nginx 的 http 块包含。
sudo install -D -o root -g root -m 0644 \
  deploy/proxy/nginx.conf /etc/nginx/conf.d/orbitbeacon.conf
sudo nginx -t
sudo systemctl reload nginx
```

两个示例均限制请求体为 1 MiB、保留 Host 并设置转发头，且发送含 `includeSubDomains` 的 HSTS。只有确认域名及其子域名均支持 HTTPS 后才保留该设置。代理终结 TLS 时由代理设置 HSTS；`X-Forwarded-Proto` 不替代 `public_url` 的 Origin 校验。

Server 默认不信任转发头。原生同机代理可设置：

```json
"trusted_proxies": ["127.0.0.1", "::1"]
```

容器中需填写 **Server 实际看到的代理源 IP** 或最小必要 CIDR，不能照抄宿主机回环地址，也不要信任整个共享网络。环境变量 `ORBITBEACON_TRUSTED_PROXIES` / CLI `--trusted-proxies` 接受逗号分隔列表；禁止 `/0`。Server 只对可信直连代理解析 `X-Forwarded-For`，从右向左跳过可信代理，取第一个不可信地址；无有效结果则回退直连 IP。入口代理必须覆盖或正确追加真实客户端地址。

公开页的 `/api/public/v1/stream` 使用 SSE，连接最长 15 分钟，按客户端 IP 最多 4 条、全局最多 200 条；建流失败时页面退回 10 秒轮询。Server 发送 `X-Accel-Buffering: no`，nginx 不要忽略或覆盖它；也可为该路径显式设置 `proxy_buffering off`。Caddy 无需额外关闭 SSE 缓冲。不配置可信代理会让访客共用代理 IP 的流数和限流额度。nginx 示例另有登录限流，Caddy 示例没有，需由边缘服务或额外模块补充。

## 检查与日常观测

```sh
curl --fail https://beacon.your-domain.com/healthz/ready
# 原生安装还可使用 CLI；私有 CA 加 --ca-file /安全路径/ca.pem。
/usr/local/bin/orbitbeacon-server health --url https://beacon.your-domain.com
```

readiness 仅证明 Server 可用，不证明 Agent 已上报。明文健康检查只接受 `http://127.0.0.1:8080` 或 `http://[::1]:8080`，不接受 `localhost`；远程检查用 HTTPS。

可选 Server 配置：

| JSON 键 | 默认值 | 用途 |
| --- | --- | --- |
| `access_log` | `false` | 结构化访问日志；健康检查仅在 5xx 时记录，不记录查询字符串、Cookie、Authorization 或接入 token |
| `metrics_listen` | `""` | 独立 HTTP 监听器的 `/metrics`，例如 `127.0.0.1:9464`；主监听器不提供该接口 |
| `allow_insecure_webhooks` | `false` | 允许字面回环地址的 HTTP webhook |
| `notification_allow_private_networks` | `false` | 允许通知访问内网 SMTP / webhook；元数据地址始终拒绝 |

以上键均有对应的大写 `ORBITBEACON_*` 环境变量和连字符形式 CLI flag。指标监听器不使用主服务的 TLS，须绑定回环或受保护的运维网络；Compose 还需自行配置采集可达的网络或端口。

管理员可在控制台管理通知通道、测试投递并查看日志。回环 HTTP webhook 必须同时开启两项通知选项；HTTPS 和 SMTP 校验证书，SMTP 的非 465 端口要求 STARTTLS。探测目标与通知通道不是同一出站策略，Agent 探测限制见 [Agent 指南](../docs/agent.md#服务探测)。

## 备份

备份必须同时保存 **数据库和匹配的 `secrets.key`**：

- `backup` / `backup --online` 只备份主 SQLite 数据库，输出文件不能已存在；配置、TLS 证书和密钥需另存。
- `secrets.key` 为 32 字节，包含通知凭据解密所需密钥；缺失或换错后，数据库中的加密凭据无法解密。数据库与密钥文件保持 `0600`，备份目录 `0700`，另做受保护的异机副本。
- 历史图表使用独立 `metrics.db`，不在上述备份中。1 分钟/5 分钟/1 小时数据分别保留 48 小时/14 天/90 天；恢复主库不会恢复或替换历史。历史库打不开时仅禁用历史，不阻止主服务启动。
- 在线备份无需停服；离线备份省略 `--online`，但必须先停止 Server。不要直接复制运行中的 SQLite 主文件，也不要删除 WAL/SHM 绕过锁错误。

### systemd 在线备份

```sh
BACKUP="/var/backups/orbitbeacon/$(date -u +%Y%m%dT%H%M%SZ)"
sudo install -d -o orbitbeacon -g orbitbeacon -m 0700 /var/backups/orbitbeacon "$BACKUP"
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server backup --online \
  --config /etc/orbitbeacon/server.json --output "$BACKUP/orbitbeacon.db"
sudo install -o orbitbeacon -g orbitbeacon -m 0600 \
  /var/lib/orbitbeacon/secrets.key "$BACKUP/secrets.key"
```

每日备份可安装仓库的脚本和 timer：

```sh
sudo install -D -o root -g root -m 0755 \
  deploy/systemd/orbitbeacon-backup /usr/local/sbin/orbitbeacon-backup
sudo install -D -o root -g root -m 0644 \
  deploy/systemd/orbitbeacon-backup.service /etc/systemd/system/orbitbeacon-backup.service
sudo install -D -o root -g root -m 0644 \
  deploy/systemd/orbitbeacon-backup.timer /etc/systemd/system/orbitbeacon-backup.timer
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-backup.timer
sudo systemctl start orbitbeacon-backup.service
sudo journalctl -u orbitbeacon-backup.service -n 100 --no-pager
```

timer 在主机本地时间 03:30 后随机延迟最多 15 分钟执行；脚本以 UTC 命名目录，删除 `/var/backups/orbitbeacon` 下超过 14 天的时间戳目录。按恢复目标调整保留期，并检查实际生成文件。

### Compose 在线备份

以下使用新的空目录，操作机需有 sudo；`compose cp` 要求主容器存在：

```sh
mkdir -m 700 ./orbitbeacon-backup
sudo chown 65532:65532 ./orbitbeacon-backup
docker compose -f deploy/docker/compose.yaml run --rm --no-deps \
  --volume "${PWD}/orbitbeacon-backup:/backup" \
  --entrypoint /usr/local/bin/orbitbeacon-server orbitbeacon \
  backup --online --config /etc/orbitbeacon/server.json --output /backup/orbitbeacon.db
sudo chown -R "$(id -u):$(id -g)" ./orbitbeacon-backup
docker compose -f deploy/docker/compose.yaml cp \
  orbitbeacon:/var/lib/orbitbeacon/secrets.key ./orbitbeacon-backup/secrets.key
chmod 600 ./orbitbeacon-backup/orbitbeacon.db ./orbitbeacon-backup/secrets.key
```

任何一步失败都不算完整备份；保留成功的数据库与对应密钥，再转移到受保护存储，不要提交到仓库。

## 恢复

先停服，并另存现有数据库和密钥。恢复必须使用备份时匹配的 `secrets.key`，不能只运行 `restore` 后继续使用新生成的密钥。

systemd：将 `BACKUP` 指向选定的备份目录：

```sh
sudo systemctl stop orbitbeacon-server
sudo install -o orbitbeacon -g orbitbeacon -m 0600 \
  "$BACKUP/secrets.key" /var/lib/orbitbeacon/secrets.key
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server restore \
  --data-dir /var/lib/orbitbeacon --input "$BACKUP/orbitbeacon.db"
sudo systemctl start orbitbeacon-server
```

Compose：下面用于 Linux 本机的 rootful Docker，备份位于 `./orbitbeacon-backup`，已有 `orbitbeacon-data` 卷。先将目录交给容器身份，再把匹配密钥安装到 Docker 返回的实际卷路径；远程或 rootless Docker 应在对应 daemon 主机完成卷文件操作，不要把该路径当成本机路径。

```sh
docker compose -f deploy/docker/compose.yaml stop orbitbeacon
sudo chown -R 65532:65532 ./orbitbeacon-backup
DATA_DIR="$(docker volume inspect --format '{{ .Mountpoint }}' orbitbeacon-data)"
sudo install -o 65532 -g 65532 -m 0600 \
  ./orbitbeacon-backup/secrets.key "$DATA_DIR/secrets.key"
docker compose -f deploy/docker/compose.yaml run --rm --no-deps \
  --volume "${PWD}/orbitbeacon-backup:/backup:ro" \
  --entrypoint /usr/local/bin/orbitbeacon-server orbitbeacon \
  restore --data-dir /var/lib/orbitbeacon --input /backup/orbitbeacon.db
sudo chown -R "$(id -u):$(id -g)" ./orbitbeacon-backup
docker compose -f deploy/docker/compose.yaml up -d orbitbeacon
```

恢复命令成功后再启动，并重新检查 HTTPS readiness、登录、节点上报和通知解密。失败时保持停服，排查错误，不要删除锁或 sidecar 文件强行继续。

## 升级

1. 先完成数据库、匹配密钥及配置备份，再停止 Server。
2. systemd 替换 `/usr/local/bin/orbitbeacon-server`；Compose 设置已发布的 `ORBITBEACON_IMAGE_TAG`，执行 `pull orbitbeacon` 和 `up -d orbitbeacon`。
3. 启动并检查 readiness，确认数据库迁移完成，再升级 Agent。

Server 严格拒绝未知上报字段，新 Agent 对旧 Server 可能得到 `400`，不能假设旧 Server 会忽略 `traffic`、`probe_results` 或 `system.agent_version`。启用新配置前先完成 Server 升级。降级前确认目标版本支持当前数据库格式；Server 不会打开更高版本数据库，必要时停服恢复升级前的完整备份。
