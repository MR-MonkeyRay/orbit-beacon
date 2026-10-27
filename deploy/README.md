# 部署

Server 提供两种部署方式，命令均从仓库根目录执行：

- **systemd**：原生 Linux 服务，配置和数据由主机管理。
- **Docker Compose**：容器运行，数据保存在固定名称的 Docker volume。

## 文件布局

```text
build/server.Dockerfile          Docker 镜像构建文件
deploy/docker/compose.yaml       Compose 定义
deploy/docker/server.json         容器配置
deploy/systemd/server.json       systemd 配置
deploy/systemd/*.service         systemd unit
```

前置条件：Linux、可信 HTTPS 或私有 CA、支持 SQLite 锁的本地文件系统。服务默认只监听本机 HTTP，由反向代理终结 HTTPS；不要直接暴露 `8080`。

`public_url` 必须填写浏览器实际访问的 HTTPS 源站。配置优先级为：CLI > `ORBITBEACON_*` 环境变量 > JSON > 默认值。未知配置键会报错。

## systemd

### 1. 编译和安装

已有匹配架构的 `build/orbitbeacon-server` 可跳过编译：

```sh
mkdir -p build
CGO_ENABLED=0 go build -trimpath -o build/orbitbeacon-server ./cmd/orbitbeacon-server
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

### 2. 初始化和启动

```sh
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server bootstrap \
  --config /etc/orbitbeacon/server.json --username admin
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-server
```

按提示输入管理员密码。已有管理员时不能重复 `bootstrap`。初始化、备份和恢复必须使用服务账户；服务运行时不要操作同一数据目录。

```sh
sudo systemctl status orbitbeacon-server
sudo journalctl -u orbitbeacon-server -n 100 --no-pager
```

## Docker Compose

设置运行时配置并拉取 GHCR 镜像：

```sh
export ORBITBEACON_PUBLIC_URL=https://beacon.your-domain.com
export ORBITBEACON_IMAGE_TAG=latest
# 私有 GHCR 包需要先执行：docker login ghcr.io
docker compose -f deploy/docker/compose.yaml pull
```

镜像地址默认为 `ghcr.io/mr-monkeyray/orbit-beacon:latest`。Compose 只拉取镜像，不在主机执行 build；`ORBITBEACON_IMAGE_TAG` 可改为 GitHub Actions 发布的 SHA 标签。

首次部署初始化并启动：

```sh
docker compose -f deploy/docker/compose.yaml run --rm --no-deps \
  --entrypoint /usr/local/bin/orbitbeacon-server orbitbeacon \
  bootstrap --config /etc/orbitbeacon/server.json --username admin
docker compose -f deploy/docker/compose.yaml up -d
```

GitHub Actions 在 `main` 分支发布 `latest`，每次 push 也发布 SHA 标签。修改 `public_url` 不需要重新构建镜像，只需更新环境变量并重建容器。

```sh
docker compose -f deploy/docker/compose.yaml ps
docker compose -f deploy/docker/compose.yaml logs --tail=100 orbitbeacon
```

数据卷固定为 `orbitbeacon-data`，不要执行 `down -v` 或删除该卷。容器使用 distroless `nonroot`（UID/GID `65532:65532`）；使用 bind mount 时，目录须由该身份拥有且权限为 `0700`。

## 验证和维护

```sh
curl --fail https://beacon.your-domain.com/healthz/ready
/usr/local/bin/orbitbeacon-server health \
  --url https://beacon.your-domain.com
```

`healthz/ready` 只表示 Server 可用，不表示 Agent 已接入。

- systemd 数据目录：`/var/lib/orbitbeacon`
- Compose 数据卷：`orbitbeacon-data`，挂载到 `/var/lib/orbitbeacon`
- `backup` 只包含 SQLite 数据库，不包含 `secrets.key`、配置或 TLS 证书。
- 备份、恢复前先停服，并保存整个数据目录及权限；不要只复制运行中的 SQLite 主文件。

systemd 备份示例：

```sh
sudo systemctl stop orbitbeacon-server
sudo install -d -o orbitbeacon -g orbitbeacon -m 0700 /var/lib/orbitbeacon-backups
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server backup \
  --config /etc/orbitbeacon/server.json \
  --output /var/lib/orbitbeacon-backups/snapshot.db
sudo install -o orbitbeacon -g orbitbeacon -m 0600 \
  /var/lib/orbitbeacon/secrets.key \
  /var/lib/orbitbeacon-backups/secrets.key
sudo systemctl start orbitbeacon-server
```

恢复前先保存现有数据：

```sh
sudo systemctl stop orbitbeacon-server
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server restore \
  --data-dir /var/lib/orbitbeacon \
  --input /var/lib/orbitbeacon-backups/snapshot.db
sudo systemctl start orbitbeacon-server
```

Compose 的备份和恢复使用同一 `docker compose run --entrypoint ...` 方式，目标路径为容器内 `/var/lib/orbitbeacon`。不要删除 WAL/SHM 文件绕过锁或恢复错误。

反向代理示例和 Agent 接入见[主 README](../README.md)与[Linux Agent 指南](../docs/agent.md)。
