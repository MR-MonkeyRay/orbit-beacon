# 服务端部署

本文提供 Linux 上的 systemd 与 Docker Compose 两种方式，二选一。以下命令从仓库根目录执行；项目概览和 Agent 接入见[主 README](../README.md)。

## 部署前准备

- 原生部署需要 Linux、systemd、sudo 权限，以及 Go 1.27.3 或兼容工具链；也可使用已准备好的、匹配服务器架构的 `bin/orbitbeacon-server`。容器部署需要 Docker Engine、Compose 插件及操作 Docker 的权限。
- 准备实际域名、DNS 和可信 HTTPS 证书。示例中的 `beacon.example.test` 是占位符，必须替换，不能直接用于公网签发证书。
- 数据必须放在支持 SQLite 锁的本地文件系统，例如 ext4、XFS 或 Btrfs；不要使用 NFS、CIFS 等网络卷，也不要用 tmpfs 保存需要持久化的数据。
- 默认只向宿主机回环地址开放 `8080`，由宿主机上的反向代理提供 HTTPS。部署文件不包含代理或证书申请配置；不要直接把 HTTP 端口暴露到公网。

### 配置与 HTTPS

两份 JSON 示例的关键配置如下：

| 配置 | systemd | Docker Compose |
| --- | --- | --- |
| 配置来源 | 安装到 `/etc/orbitbeacon/server.json` 的 [server.systemd.json](server.systemd.json) | 构建时写入镜像的 [server.container.json](server.container.json) |
| `listen_addr` | `127.0.0.1:8080` | `0.0.0.0:8080`，由 Compose 映射到宿主机 `127.0.0.1:8080` |
| `data_dir` | `/var/lib/orbitbeacon` | 容器内 `/var/lib/orbitbeacon` |
| `public_url` | 均需改为实际访问地址，例如 `https://beacon.your-domain.com`；使用独立域名根路径，不添加路径前缀 | 同左 |
| `max_nodes` | 默认 `500`，必须为正整数，包含待接入和禁用节点 | 同左 |

`public_url` 用于浏览器来源校验，不会自动启用 TLS。配置优先级为：显式 CLI 参数 > `ORBITBEACON_*` 环境变量 > JSON 文件 > 默认值；未知配置键会报错。提供的 Compose 文件没有向容器传入这些运行时环境变量，在宿主机设置变量不会自动覆盖容器配置。

若使用已安装并由 systemd 管理的 Caddy，可在其 Caddyfile 中加入以下站点，替换域名后重载 Caddy。公开域名的常规自动 HTTPS 部署需要正确 DNS，并允许外部访问代理的 `80/443` 端口。

```caddyfile
beacon.your-domain.com {
    reverse_proxy 127.0.0.1:8080
}
```

```sh
sudo systemctl reload caddy
```

其他反向代理同样应将该域名根路径的请求转发到 `http://127.0.0.1:8080`。这里的代理运行在宿主机；若代理也在容器中，不能把代理容器内的 `127.0.0.1` 当作宿主机，需要另行配置网络。当前服务不消费转发头，设置 `trusted_proxy_cidrs` 不会改变这一点。

也可成对设置 `tls_cert_file` / `tls_key_file` 让服务直接监听 TLS，但需自行调整监听地址、网络暴露和证书挂载，并确保服务用户可读证书。下述命令采用反向代理方案。

## 方式一：systemd

### 1. 安装二进制与配置

若已有匹配架构的 `bin/orbitbeacon-server`，跳过编译：

```sh
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -o bin/orbitbeacon-server ./cmd/orbitbeacon-server
```

以下账户命令适用于 Debian / Ubuntu 等提供 `useradd`、`/usr/sbin/nologin` 的系统。若已创建同名专用账户，不要重复创建。

```sh
sudo useradd --system --user-group --home /var/lib/orbitbeacon --shell /usr/sbin/nologin orbitbeacon
sudo install -D -o root -g root -m 0755 bin/orbitbeacon-server /usr/local/bin/orbitbeacon-server
sudo install -D -o root -g orbitbeacon -m 0640 deploy/server.systemd.json /etc/orbitbeacon/server.json
sudo install -D -o root -g root -m 0644 deploy/orbitbeacon-server.service /etc/systemd/system/orbitbeacon-server.service
sudo install -d -o orbitbeacon -g orbitbeacon -m 0700 /var/lib/orbitbeacon
sudoedit /etc/orbitbeacon/server.json
```

编辑时替换 `public_url`，保留示例 `data_dir` 和 `listen_addr`，并按需设置 `max_nodes`。不要在已有部署上重新复制示例覆盖实际配置。

### 2. 先初始化，再启动

首次启动前，以**服务账户**创建第一个管理员；终端会提示输入密码，长度为 8–1024 个 UTF-8 字节。示例用户名 `admin` 可自行替换，不要把密码写入命令行或文档。

```sh
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server bootstrap \
  --config /etc/orbitbeacon/server.json --username admin
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-server
```

`bootstrap` 与 `serve` 会独占同一数据目录。若服务已经运行，先执行 `sudo systemctl stop orbitbeacon-server`；已有管理员时不能重复 bootstrap，它也不是密码重置命令。不要以 root 初始化后再改用普通账户运行，否则会触发数据所有权检查。

单元以 `orbitbeacon` 用户运行，`StateDirectory=orbitbeacon` 管理 `0700` 状态目录，并开启只读系统目录、私有临时目录和禁止提权等限制；安装和服务管理需要 sudo，服务进程不需要 root。

### 3. 查看状态与日志

```sh
sudo systemctl status orbitbeacon-server
sudo journalctl -u orbitbeacon-server -n 100 --no-pager
sudo systemctl restart orbitbeacon-server
```

修改 JSON 后重启服务；修改 unit 后还需先执行 `sudo systemctl daemon-reload`。

## 方式二：Docker Compose

### 1. 修改配置并构建镜像

先编辑 `deploy/server.container.json`，将 `public_url` 替换为实际 HTTPS 地址。保持 `listen_addr` 为 `0.0.0.0:8080`：容器内部若只监听回环地址，宿主机端口映射无法访问它。

```sh
docker compose -f deploy/compose.yaml build
```

配置通过 Dockerfile 复制进镜像，不是宿主机配置文件挂载；后续修改 JSON 也必须重新构建并重新创建服务容器。默认构建元数据为 `development` / `unknown`；需要发行版本信息时，可通过 `--build-arg VERSION=...`、`COMMIT=...`、`BUILD_DATE=...` 传入真实值。

### 2. 在同一数据卷中初始化，再启动

首次部署时尚未启动服务，直接执行以下命令。若服务已运行，先执行 `docker compose -f deploy/compose.yaml stop orbitbeacon`，避免数据目录锁冲突。

```sh
docker compose -f deploy/compose.yaml run --rm --no-deps \
  --entrypoint /usr/local/bin/orbitbeacon-server orbitbeacon \
  bootstrap --config /etc/orbitbeacon/server.json --username admin
docker compose -f deploy/compose.yaml up -d
```

请在交互终端输入管理员密码。镜像入口已经包含 `serve --config ...`，因此执行 bootstrap 必须像上面一样覆盖 `--entrypoint`，不能只在服务名后追加 `bootstrap`。一次性容器与长期运行的容器使用同一个 `orbitbeacon-data` 命名卷和相同 UID/GID。

```sh
docker compose -f deploy/compose.yaml ps
docker compose -f deploy/compose.yaml logs --tail=100 orbitbeacon
docker compose -f deploy/compose.yaml restart orbitbeacon
```

## 确认访问

服务和 HTTPS 代理启动后，替换以下域名再执行：

```sh
curl --fail https://beacon.your-domain.com/healthz/ready
```

原生二进制也提供带证书校验的健康命令；私有 CA 可追加 `--ca-file /path/to/ca.pem`：

```sh
/usr/local/bin/orbitbeacon-server health --url https://beacon.your-domain.com
```

就绪接口成功仅表示服务就绪，不代表 Agent 已接入。随后访问相同 HTTPS 地址，用初始化时创建的账户登录。

## 持久化与维护

- **systemd：**数据位于 `/var/lib/orbitbeacon`；**Compose：**数据位于固定名称 `orbitbeacon-data` 的卷，挂载到容器同一路径。不要让两种部署或多个实例共用同一数据目录，也不要把此固定卷名误当作按 Compose 项目隔离的卷名。
- 数据目录必须由运行用户拥有，权限严格为 `0700`；数据库、密钥和锁文件必须属于同一运行用户，且不可向组或其他用户开放。路径不能经过符号链接，祖先目录也会校验所有权和可写权限；不安全权限会被拒绝，而不是自动修复。
- 镜像使用 distroless `nonroot`（UID/GID `65532:65532`），根文件系统只读，移除全部 Linux capabilities；镜像为数据目录预设 `0700` 和所有权，首次创建的命名卷沿用该设置。已有卷必须保持相同权限。改用宿主机 bind mount 时，需自行准备 `65532:65532`、`0700` 的目录并满足祖先目录约束，不要用 `chmod 777` 绕过问题。
- `orbitbeacon.db` 与 `secrets.key` 都需保留。完整目录备份或迁移应先停止服务，再保存整个数据目录及其权限；不要只复制运行中的 SQLite 主文件，忽略 WAL 或密钥。配置和 HTTPS 证书应另行备份。
- `docker compose -f deploy/compose.yaml down` 不会删除命名卷；**不要执行 `down -v` 或删除 `orbitbeacon-data`，否则会丢失持久化数据。**更换镜像无需重新 bootstrap；构建后执行 `docker compose -f deploy/compose.yaml up -d` 重新创建容器。
- 遇到 `data_dir_in_use`，先确认没有其他 `serve`、`bootstrap`、备份或恢复命令占用目录；不要删除 `server.lock` 来绕过锁。遇到权限或文件系统拒绝，修正实际目录、运行用户或存储位置，不要关闭安全限制。

### 数据库备份与恢复

`backup` 生成一个一致的 SQLite 数据库文件，**不包含** `secrets.key`、JSON 配置或 TLS 证书；`--output` 的父目录必须存在，输出文件不能已经存在。`restore` 接收该数据库文件，并要求显式传入 `--data-dir`。两者都需要独占数据目录，先停服务，并以原来的运行用户执行。

systemd 示例（`snapshot.db` 每次需换成未使用的文件名）：

```sh
sudo systemctl stop orbitbeacon-server
sudo install -d -o orbitbeacon -g orbitbeacon -m 0700 /var/lib/orbitbeacon-backups
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server backup \
  --config /etc/orbitbeacon/server.json \
  --output /var/lib/orbitbeacon-backups/snapshot.db
sudo install -o orbitbeacon -g orbitbeacon -m 0600 \
  /var/lib/orbitbeacon/secrets.key /var/lib/orbitbeacon-backups/secrets.key
sudo systemctl start orbitbeacon-server
```

恢复会替换现有数据库，请先保存当前数据。下面示例用于原机器、保留原 `secrets.key` 的情况；迁移时还需安装与备份配套的密钥和实际配置，并恢复正确所有权。

```sh
sudo systemctl stop orbitbeacon-server
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server restore \
  --data-dir /var/lib/orbitbeacon \
  --input /var/lib/orbitbeacon-backups/snapshot.db
sudo systemctl start orbitbeacon-server
```

Compose 同样通过覆盖入口运行工具；以下备份暂存在同一个数据卷中，完成后仍需将备份及配套密钥转存到独立、安全的位置，不能把同卷副本当作灾难恢复备份。

```sh
docker compose -f deploy/compose.yaml stop orbitbeacon
docker compose -f deploy/compose.yaml run --rm --no-deps \
  --entrypoint /usr/local/bin/orbitbeacon-server orbitbeacon \
  backup --config /etc/orbitbeacon/server.json \
  --output /var/lib/orbitbeacon/snapshot.db
docker compose -f deploy/compose.yaml up -d
```

恢复时同样先停容器，再运行上述 `run` 入口，将子命令与参数改为 `restore --data-dir /var/lib/orbitbeacon --input /var/lib/orbitbeacon/snapshot.db`；成功后再 `up -d`。恢复工具会校验输入数据库和目标 WAL/SHM 状态，不是任意损坏数据库或 sidecar 文件的自动修复器；报错时保留现场，不要手动删除 WAL/SHM 后强行启动。

## 参考

- [Caddy：反向代理与自动 HTTPS](https://caddyserver.com/docs/quick-starts/reverse-proxy)
- [Docker Compose：一次性命令与覆盖入口](https://docs.docker.com/reference/cli/docker/compose/run/)
- [Docker：数据卷与持久化](https://docs.docker.com/engine/storage/volumes/)
