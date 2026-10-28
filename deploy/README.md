# 部署

Server 提供两种部署方式，命令均从仓库根目录执行：

- **systemd**：原生 Linux 服务，配置和数据由主机管理。
- **Docker Compose**：容器运行，数据保存在固定名称的 Docker volume。

## 文件布局

```text
VERSION                           当前项目版本，发布流水线的唯一版本来源
build/server.Dockerfile          Docker 镜像构建文件
deploy/docker/compose.yaml       Compose 定义
deploy/docker/server.json         容器配置
deploy/proxy/Caddyfile           Caddy TLS 反向代理示例
deploy/proxy/nginx.conf          nginx TLS 反向代理示例
deploy/systemd/server.json       systemd 配置
deploy/systemd/*                 systemd 服务、定时备份 unit 与脚本
```

前置条件：Linux、可信 HTTPS 或私有 CA、支持 SQLite 锁的本地文件系统。服务默认只监听本机 HTTP，由反向代理终结 HTTPS；不要直接暴露 `8080`。

`public_url` 必须填写浏览器实际访问的 HTTPS 源站。配置优先级为：CLI > `ORBITBEACON_*` 环境变量 > JSON > 默认值。未知配置键会报错。

运维观测默认关闭。设置 `"access_log": true`（或 `ORBITBEACON_ACCESS_LOG=true`、`--access-log`）后，每个非健康检查请求会写一条结构化访问日志；状态码为 5xx 的健康检查也会记录。日志不包含查询字符串、Cookie、Authorization 或接入 token。

设置 `"metrics_listen": "127.0.0.1:9464"`（或 `ORBITBEACON_METRICS_LISTEN`、`--metrics-listen`）后，Server 在独立 HTTP 监听器的 `/metrics` 提供 Prometheus 文本格式指标。默认空值表示不启动指标监听器，主业务监听器始终不提供 `/metrics`。该监听器不使用主监听器的 TLS 配置，建议只绑定回环地址或受保护的运维网络，并由采集端直接访问；关闭服务时它与主监听器一起优雅停止。

主要序列为 `orbitbeacon_http_requests_total{method,route,status_class}`、`orbitbeacon_http_request_duration_seconds`、`orbitbeacon_http_requests_in_flight`、`orbitbeacon_public_streams`、`orbitbeacon_agent_reports_total{reason}`、`orbitbeacon_login_rejections_total{reason}`、`go_goroutines`、`go_memstats_heap_alloc_bytes` 和 `orbitbeacon_build_info{version}`。报告和登录拒绝原因按 HTTP 结果归类，其中 `unavailable` 合并服务繁忙和停机期间的 503。

## systemd

### 1. 编译和安装

已有匹配架构的 `build/orbitbeacon-server` 可跳过编译：

```sh
mkdir -p build
GOTOOLCHAIN=go1.27.3 CGO_ENABLED=0 go build -trimpath -o build/orbitbeacon-server ./cmd/orbitbeacon-server
```

```sh
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

已存在 `orbitbeacon` 账户时不要重复创建。编辑配置时至少修改 `public_url`；保留 `127.0.0.1:8080` 和 `/var/lib/orbitbeacon`，除非同时调整 unit 和目录权限。

unit 已移除全部 capabilities；若改为直接绑定 1024 以下端口，需要单独授予 `CAP_NET_BIND_SERVICE`，并同步调整 `CapabilityBoundingSet` 和 `AmbientCapabilities`。推荐继续由反向代理绑定 443。

### 2. 初始化和启动

```sh
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server bootstrap \
  --config /etc/orbitbeacon/server.json --username admin
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-server
```

按提示输入管理员密码。已有管理员时不能重复 `bootstrap`。初始化、备份和恢复必须使用服务账户；除 `backup --online` 外，服务运行时不要操作同一数据目录。

```sh
sudo systemctl status orbitbeacon-server
sudo journalctl -u orbitbeacon-server -n 100 --no-pager
```

## Docker Compose

设置运行时配置并拉取 GHCR 镜像：

```sh
export ORBITBEACON_PUBLIC_URL=https://beacon.your-domain.com
# 私有 GHCR 包需要先执行：docker login ghcr.io
docker compose -f deploy/docker/compose.yaml pull
```

镜像地址默认固定为 `ghcr.io/mr-monkeyray/orbit-beacon:0.1.0`，与当前根目录 `VERSION` 一致。Compose 只拉取镜像，不在主机执行 build；`ORBITBEACON_IMAGE_TAG` 可显式改为其他已发布的版本标签或 SHA 标签。不要在生产环境依赖浮动的 `latest`。

首次部署初始化并启动：

```sh
docker compose -f deploy/docker/compose.yaml run --rm --no-deps \
  --entrypoint /usr/local/bin/orbitbeacon-server orbitbeacon \
  bootstrap --config /etc/orbitbeacon/server.json --username admin
docker compose -f deploy/docker/compose.yaml up -d
```

GitHub Actions 从根目录 `VERSION` 读取版本。在 `main` 分支发布 `latest`、版本标签和 SHA 标签，同时创建带 Linux Server/Agent 多架构二进制的 GitHub Release。若该版本的版本镜像和 Release 包都已存在，后续同版本构建会跳过推送；需要发布新内容时先更新 `VERSION`。修改 `public_url` 不需要重新构建镜像，只需更新环境变量并重建容器。

```sh
docker compose -f deploy/docker/compose.yaml ps
docker compose -f deploy/docker/compose.yaml logs --tail=100 orbitbeacon
```

数据卷固定为 `orbitbeacon-data`，不要执行 `down -v` 或删除该卷。容器使用 distroless `nonroot`（UID/GID `65532:65532`）；使用 bind mount 时，目录须由该身份拥有且权限为 `0700`。

## TLS 反向代理

示例位于 [`proxy/Caddyfile`](proxy/Caddyfile) 和 [`proxy/nginx.conf`](proxy/nginx.conf)，都在代理上终结 TLS，并通过 HTTP/1.1 转发到 `127.0.0.1:8080`。部署前把示例域名和证书路径换成真实值，同时确保 Server 的 `public_url` 与浏览器实际访问的 HTTPS 源站完全一致。

Caddy 会自动申请受信任证书。示例使用 [`request_body`](https://caddyserver.com/docs/caddyfile/directives/request_body) 限制请求体，需要 Caddy 2.10.0 或更新版本。安装示例后先验证，再重载：

```sh
sudo install -D -o root -g root -m 0644 deploy/proxy/Caddyfile /etc/caddy/Caddyfile
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

nginx 示例需要已有证书：

```sh
sudo install -D -o root -g root -m 0644 \
  deploy/proxy/nginx.conf /etc/nginx/conf.d/orbitbeacon.conf
sudo nginx -t
sudo systemctl reload nginx
```

两个示例都会保留 `Host` 并设置 `X-Forwarded-For`、`X-Forwarded-Proto`，限制请求体为 1 MiB，配置合理的上游超时并发送 HSTS。确认该域名只提供 HTTPS 后再启用 HSTS；`includeSubDomains` 也会影响所有子域名。

公开状态页的 `/api/public/v1/stream` 是最长 15 分钟的 SSE 连接。nginx 应对该路径设置 `proxy_buffering off`；Server 同时发送 `X-Accel-Buffering: no`，未覆盖该响应头时也可依赖它关闭 nginx 缓冲。Caddy 默认会流式转发 SSE，无需额外配置。每个客户端 IP 最多 4 条、全局最多 200 条流，超出时页面自动退回 10 秒轮询；反向代理部署需配置下文的 `trusted_proxies`，否则所有访客共用代理 IP 的 4 条名额。

Server 的 `trusted_proxies` 默认 `[]`，不信任任何转发头，此时登录和接入限流按代理直连 IP 计算。同机代理可在 JSON 中设置 `"trusted_proxies": ["127.0.0.1", "::1"]`；容器部署应填写 Server 实际看到的代理 IP 或最小必要 CIDR，不要信任整个共享网络。`ORBITBEACON_TRUSTED_PROXIES` 和 `--trusted-proxies` 接受逗号分隔列表，空字符串清空列表；非法 IP/CIDR 和 `/0` 全网前缀会报错。

只有直连地址属于可信代理时，Server 才从右向左检查 `X-Forwarded-For`，跳过可信代理并使用第一个不可信 IP；缺失、包含非法地址或全部可信时回退到直连地址。代理必须覆盖客户端提交的转发头，或正确追加经过验证的客户端地址；`X-Forwarded-Proto` 不影响 Origin 校验或 HSTS。Server 仅在自身终结 TLS 时发送 `Strict-Transport-Security: max-age=31536000`；代理终结 TLS 时由代理设置 HSTS。入口代理仍应对登录请求限流；nginx 示例对 `POST /api/v1/session` 使用了更严格的独立限流区。Caddy 示例没有按客户端实现该限制，生产部署应在 Caddy 前的边缘服务或额外 Caddy 限流模块中补充。

## 验证

```sh
curl --fail https://beacon.your-domain.com/healthz/ready
/usr/local/bin/orbitbeacon-server health \
  --url https://beacon.your-domain.com
```

`healthz/ready` 只表示 Server 可用，不表示 Agent 已接入。

本机和容器内也可执行 `orbitbeacon-server health --url http://127.0.0.1:8080`（IPv6 使用 `http://[::1]:8080`）。明文 HTTP 健康检查仅接受这两个回环 IP 字面量，不接受 `localhost` 或其他主机；远程检查继续使用 HTTPS，私有 CA 可通过 `--ca-file` 指定。

## 升级

升级前停止服务并完成数据库与 `secrets.key` 备份。替换二进制或镜像标签后再启动，并通过 readiness 检查验证。Server 会拒绝打开由更高版本写入的数据库，因此降级前必须确认目标版本支持当前数据库格式，并保留升级前的完整备份。

先升级 Server，再升级 Agent。Server 严格拒绝上报中的未知字段，新版 Agent 新增的字段（如 `system.agent_version`、流量统计 `traffic` 或服务探测 `probe_results`）会被旧版 Server 以 400 拒绝，直到 Server 升级。流量统计和服务探测启用前，所有 Agent 升级都应先确认 Server 已完成数据库迁移并通过 readiness 检查。服务探测目标对所有控制台用户和只读 API 令牌可见，URL 不得包含秘密；启用 ICMP 探测时，还需在 Agent 主机为其非 root 运行组配置 `net.ipv4.ping_group_range`。

systemd：

```sh
sudo systemctl stop orbitbeacon-server
# 按下文备份数据库和 secrets.key，再替换二进制。
sudo install -o root -g root -m 0755 \
  build/orbitbeacon-server /usr/local/bin/orbitbeacon-server
sudo systemctl start orbitbeacon-server
/usr/local/bin/orbitbeacon-server health --url https://beacon.your-domain.com
```

Docker Compose：

```sh
docker compose -f deploy/docker/compose.yaml stop orbitbeacon
# 按下文备份 volume，然后把标签改为要升级到的已发布版本。
export ORBITBEACON_IMAGE_TAG=0.2.0
docker compose -f deploy/docker/compose.yaml pull orbitbeacon
docker compose -f deploy/docker/compose.yaml up -d orbitbeacon
docker compose -f deploy/docker/compose.yaml ps
curl --fail https://beacon.your-domain.com/healthz/ready
```

把 `ORBITBEACON_IMAGE_TAG` 持久保存在部署环境文件或服务管理配置中，后续升级时只改为经过验证的新版本标签。

## 备份与恢复

- systemd 数据目录：`/var/lib/orbitbeacon`
- Compose 数据卷：`orbitbeacon-data`，挂载到 `/var/lib/orbitbeacon`
- `backup` 只包含 SQLite 数据库，不包含 `secrets.key`、配置或 TLS 证书。
- 指标历史存于独立的 `metrics.db`，为尽力而为的数据，不包含在 `backup`（包括 `--online`）中。1 分钟/5 分钟/1 小时数据分别保留 48 小时/14 天/90 天；已删除节点的历史在每小时清理时移除。恢复不替换历史库；若已有 `metrics.db` 版本过新或无法安全打开，Server 记录警告并禁用历史，主服务照常启动。需要清空历史时，先停服，再移走 `metrics.db` 及其 WAL/SHM 文件后重启。
- 日常备份推荐使用 `backup --online`；服务可继续读写，无需停服。不要直接复制运行中的 SQLite 主文件。
- 每份数据库备份都必须单独复制与其匹配的 `secrets.key`，将两者保存在同一受保护目录并保持 `0600`；配置和 TLS 证书也应按你的密钥管理策略另行保存。
- 不带 `--online` 的离线备份仍可用于维护场景，但必须先停服；恢复也必须先停服。

systemd 在线备份示例：

```sh
sudo install -d -o orbitbeacon -g orbitbeacon -m 0700 /var/lib/orbitbeacon-backups
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server backup \
  --online \
  --config /etc/orbitbeacon/server.json \
  --output /var/lib/orbitbeacon-backups/snapshot.db
sudo install -o orbitbeacon -g orbitbeacon -m 0600 \
  /var/lib/orbitbeacon/secrets.key \
  /var/lib/orbitbeacon-backups/secrets.key
sudo chmod 0600 \
  /var/lib/orbitbeacon-backups/snapshot.db \
  /var/lib/orbitbeacon-backups/secrets.key
```

需要离线备份时，先停止服务并省略 `--online`；数据库和 `secrets.key` 仍须作为同一份备份一起保存：

```sh
sudo systemctl stop orbitbeacon-server
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server backup \
  --config /etc/orbitbeacon/server.json \
  --output /var/lib/orbitbeacon-backups/offline-snapshot.db
sudo install -o orbitbeacon -g orbitbeacon -m 0600 \
  /var/lib/orbitbeacon/secrets.key \
  /var/lib/orbitbeacon-backups/offline-secrets.key
sudo systemctl start orbitbeacon-server
```

### 定时 systemd 备份

仓库提供每日定时示例。脚本使用在线模式，在服务持续运行时把数据库和单独的 `secrets.key` 写入同一个 UTC 时间戳目录，并保持文件权限为 `0600`。脚本会删除 `/var/backups/orbitbeacon` 下超过 14 天的时间戳备份目录；仍应为该目录配置异机复制和加密策略，并按恢复目标调整保留期。

```sh
sudo install -D -o root -g root -m 0755 \
  deploy/systemd/orbitbeacon-backup /usr/local/sbin/orbitbeacon-backup
sudo install -D -o root -g root -m 0644 \
  deploy/systemd/orbitbeacon-backup.service \
  /etc/systemd/system/orbitbeacon-backup.service
sudo install -D -o root -g root -m 0644 \
  deploy/systemd/orbitbeacon-backup.timer \
  /etc/systemd/system/orbitbeacon-backup.timer
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-backup.timer
sudo systemctl list-timers orbitbeacon-backup.timer
```

首次启用后可手动运行一次并检查生成文件；该命令不会停止或重启服务：

```sh
sudo systemctl start orbitbeacon-backup.service
sudo journalctl -u orbitbeacon-backup.service -n 100 --no-pager
sudo find /var/backups/orbitbeacon -maxdepth 2 -type f -ls
```

### Docker volume 备份

下面的命令让一次性容器直接对 `orbitbeacon-data` volume 执行在线备份，主容器无需停止。数据库写入主机目录后，从运行中的主容器复制匹配的 `secrets.key`。不要省略该文件，它不包含在数据库备份中。

```sh
mkdir -p ./orbitbeacon-backup
chmod 700 ./orbitbeacon-backup
sudo chown 65532:65532 ./orbitbeacon-backup

docker compose -f deploy/docker/compose.yaml run --rm --no-deps \
  --volume "${PWD}/orbitbeacon-backup:/backup" \
  --entrypoint /usr/local/bin/orbitbeacon-server orbitbeacon \
  backup --online --config /etc/orbitbeacon/server.json \
  --output /backup/orbitbeacon.db

sudo chown -R "$(id -u):$(id -g)" ./orbitbeacon-backup
docker compose -f deploy/docker/compose.yaml cp \
  orbitbeacon:/var/lib/orbitbeacon/secrets.key \
  ./orbitbeacon-backup/secrets.key
chmod 600 ./orbitbeacon-backup/orbitbeacon.db ./orbitbeacon-backup/secrets.key
```

如果任一步失败，删除该次不完整的备份目录后重试。确认数据库与匹配的 `secrets.key` 都存在且权限为 `0600`，再将备份目录转移到受保护的存储；不要提交到仓库。

恢复前先保存现有数据：

```sh
sudo systemctl stop orbitbeacon-server
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server restore \
  --data-dir /var/lib/orbitbeacon \
  --input /var/lib/orbitbeacon-backups/snapshot.db
sudo systemctl start orbitbeacon-server
```

Compose 恢复同样要先停止主容器，再使用 `docker compose run --entrypoint ...`；恢复前将数据库备份和匹配的 `secrets.key` 放回受保护的数据 volume。不要删除 WAL/SHM 文件绕过锁或恢复错误。

Agent 接入见[主 README](../README.md)与[Linux Agent 指南](../docs/agent.md)。

## 通知通道出站配置

`server.json` 可增加 `"allow_insecure_webhooks": false`、`"notification_allow_private_networks": false`；环境变量为 `ORBITBEACON_ALLOW_INSECURE_WEBHOOKS`、`ORBITBEACON_NOTIFICATION_ALLOW_PRIVATE_NETWORKS`，CLI 为 `--allow-insecure-webhooks`、`--notification-allow-private-networks`，均默认关闭。优先级为 flag > env > JSON。内网 SMTP 或 HTTPS webhook 需显式允许内网；回环 HTTP webhook 必须同时打开两项。默认拒绝私网、回环、链路本地和元数据 IP（元数据地址即使允许内网也始终拒绝），解析结果在实际拨号时校验并固定，无重定向或代理绕行。Telegram 固定调用官方 HTTPS API。

出站连接最长 10 秒，TLS 1.2+ 使用系统信任根并校验证书。SMTP 除 465 使用隐式 TLS 外必须 STARTTLS，AUTH PLAIN 仅在 TLS 建立后发送。管理员通过 API 创建通道及规则 `channel_ids`，可同步测试并读取投递日志；参见 [OpenAPI](../api/openapi.yaml)。通知 worker 的关闭预算最多 5 秒，与 HTTP 10 秒、ingest 5 秒、history 5 秒共同保持在 30 秒服务停止宽限内。

保留数据目录的 32 字节 `secrets.key`（所属用户、0600 权限检查保持不变）。数据库内的通道凭据为 AES-256-GCM 密文，备份也包含密文；必须使用匹配的 `secrets.key` 才能恢复解密，密钥和备份应一起妥善保护。每通道 20 次/分钟、最多 5 次重试；积压合并为摘要，投递终态记录由现有保留循环分批清理（30 天）。
