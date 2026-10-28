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

按提示输入管理员密码；已有管理员时不能重复初始化。初始化和恢复使用服务账号并先停服；导出及计划备份由运行中的 Server 执行。

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

镜像构建文件为 [`build/server.Dockerfile`](../build/server.Dockerfile)。[`ci.yml`](../.github/workflows/ci.yml) 在 `main` push 且全部检查通过后调用 [`release.yml`](../.github/workflows/release.yml)，按根目录 `VERSION` 发布 amd64/arm64 镜像、Linux Server/Agent 包及 `SHA256SUMS`；版本镜像与两项 Release 资产齐全时跳过。PR、其他分支和 tag push 不发布；流水线配置不代表产物已可用。

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

## 导出与备份计划

管理员进入 **Settings → Data operations**：Export 发起后台导出，Backup plans 管理计划，Task history 查看结果。受理后可以离开页面；铃铛会显示完成或失败，下载必须手动点击，不会自动下载。管理员共享任务、下载和密码；viewer 的铃铛仅显示告警，各用户已读状态独立。备份结果不通过 Webhook、Telegram 或邮件外发。

### 归档与密码

- Configuration、Monitoring、Events 三类独立勾选，至少一类，默认仅 Configuration。
- 配置含用户密码哈希、Agent 持久身份、通知/对象存储凭据及匹配的 `secrets.key`；不含机器部署配置、TLS 证书或可执行文件，请另存这些文件。
- 监控含已落库的节点观测、流量、探测和指标历史；事件含审计、告警、已结束投递、站内通知及已结束任务历史，不带历史备份密码。登录会话、接入票据、待执行任务、待发送通知和旧下载产物不归档。
- 每包为 `.tar.gz.age`，有独立的 16 位随机大小写字母/数字密码。管理员仅在详情中主动 Reveal、Copy 或 Download recovery info 时获取密码；须把每包凭据另存到安全位置，密码不会随远程包上传，也没有主密码托管。
- 手动导出文件有效 24 小时；任务、站内通知及密码保留 90 天，任务和密码从任务完成时计期。原机丢失或密码过期且未另存时无法恢复。计划包只显示本地路径或目标 bucket 内的准确 Object key，不拼造下载 URL，也不提供网页下载副本。
- 只备份已落库数据，不暂停采集或强制冲刷内存；主库与指标库各自一致，不保证跨库同一时刻。选择 Monitoring 时历史库不可读会使任务失败，不以空历史冒充成功。

### 计划、时区与本地目录

每条计划使用一个本地或对象存储目标，支持多个计划；新建默认停用。Cron 为五段，默认 `0 3 * * *`，不接受秒字段、`@daily`、时区前缀或命令。按 **Server 进程的 `time.Local`** 执行，不是浏览器时区；页面展示 Server 时区和各次执行的当地时间。修改系统/容器时区后需重启 Server，容器不自动继承宿主时区。错过多个周期只补一次；同计划已有任务时不堆积。任务活动期间不能编辑、停用或删除计划。

本地路径须为预先创建的绝对路径，由服务运行用户所有、`0700`、无符号链接且可写；Server 不自动改权限。每个计划写入自己的 `orbitbeacon-<plan_id>/`，默认保留最近 7 份，可设 1–1000；仅成功后清理该计划的旧包。删除计划不删除已保存备份，清理失败会单独警告。

- systemd 默认仅允许写入 `/var/lib/orbitbeacon`。使用其他路径时，先准备目录所有权/权限，再用 unit drop-in 为该路径增加精确的 `ReadWritePaths=`；不要放宽为全盘可写。
- Compose 使用现有数据卷内的路径，或显式挂载额外可写目录并设为 `65532:65532` 所有、`0700`。不修改只读根文件系统或依赖小容量 tmpfs；staging 使用 data-dir。

### 对象存储

统一使用 AWS S3 SDK 和显式填写的静态凭据，不读取主机默认凭据链；支持以下配置，远程只上传，保留期由云端生命周期管理：

| Provider | Endpoint / Region / 寻址 |
| --- | --- |
| COS | `https://cos.<region>.myqcloud.com`；region 必填，bucket 含 APPID，virtual-host |
| OSS | 推荐 `https://s3.oss-<region>.aliyuncs.com`，region 必填，virtual-host；自定义域须已绑定 bucket 且配置 TLS，按该域直接寻址 |
| R2 | `https://<account>.r2.cloudflarestorage.com`；region 为 `auto`，path-style |
| AWS S3 | 默认 `https://s3.<region>.amazonaws.com`，region 必填，virtual-host |
| S3 compatible | endpoint、region、bucket 必填，默认 path-style |

Endpoint 只允许 HTTPS 源站（可带端口），不能含路径、用户信息、查询或片段；prefix 使用安全相对路径。TLS 至少 1.2 并校验证书，不使用 HTTP 代理、不跟随重定向。默认拒绝私网；显式允许私网仍禁止回环、链路本地、非全局单播和元数据地址，最终 bucket 域名也执行 DNS/IP 校验。凭据仅加密保存，编辑时不回填；更换 provider 须提交匹配 endpoint、region 及完整新凭据。

已进行 profile 协议测试和真实 MinIO S3 兼容存储 smoke；COS、OSS、R2、AWS S3 四家实云尚未实测，不据此承诺全部自定义域或供应商特性可用。没有连接测试、任务取消或重试按钮；失败后可重新发起导出，计划等待下一周期。存储结果未知时先核对详情中的位置、大小与哈希，不要把它当作确定未写入。

## 恢复

先停止 Server，使用运行服务的同一用户执行。只接受归档格式 v1、主库 schema 17、指标库 schema 1 精确匹配的 `.tar.gz.age` 包；旧 schema 包（包括仅 Monitoring 的包）也拒绝，不接收裸 SQLite 或转换旧格式。密码文件必须归该用户所有、为 `0600` 普通文件，内容为该包的 16 位密码，可带一个换行。也可省略 `--password-file` 从终端无回显输入；不支持命令行明文密码或环境变量密码。

```sh
orbitbeacon-server restore --data-dir /var/lib/orbitbeacon \
  --input /backup/orbitbeacon-....tar.gz.age \
  --replace --password-file /backup/password.txt
```

语法仅为 `restore --data-dir PATH --input ARCHIVE [--replace] [--password-file FILE]`。目标已有业务数据时必须显式 `--replace`；真正空目录可省略，但包内须包含 Configuration。包内所选类别整体替换，未含类别保留，不逐条合并；缺失稳定 ID 依赖时整次拒绝，不按名称匹配或自动创建节点。配置包自带匹配密钥，恢复配置后计划全部停用；登录会话及接入票据不从包导入。恢复事件不重放任务或外发投递；保留目标事件时，原活动任务/待发送投递会标为恢复中断。

多文件恢复保持 data-dir 与 `server.lock` inode 不变，使用 `restore-journal.json` 安装主库、指标库和密钥。失败或下次启动发现 prepared 状态时，验证回滚副本后恢复完整旧文件组；committed 状态仅清理 journal，不回滚。成功后输出 `.restore-<id>/old/` 原文件组位置，不自动删除；自行确认恢复正常后妥善保管或处理。回滚副本缺失、损坏或 journal 无效会阻止启动。不要删除锁、journal 或 SQLite sidecar 强行继续。

成功后再启动，检查 HTTPS readiness、登录、节点上报和通知凭据。Compose 先停服务，再通过一次性 `compose run` 将包及密码文件只读挂载到容器，使用相同数据卷和运行身份执行上述命令。

## 升级

当前主库为 schema 17：仅空库直接初始化，已有库必须只有一条匹配的版本记录；旧库、缺失/混合版本记录或更高版本库均拒绝，不运行迁移，也不自动清理原文件。旧 schema 16 及其他旧格式不能通过备份恢复转换为当前格式，需使用新的私有数据目录；保留旧目录、旧包及原版本程序。

以下替换步骤仅适用于数据库 schema 不变的版本：

1. 先完成所需三类数据的加密备份，另存恢复密码、部署配置和证书，再停止 Server。
2. systemd 替换 `/usr/local/bin/orbitbeacon-server`；Compose 设置已发布的 `ORBITBEACON_IMAGE_TAG`，执行 `pull orbitbeacon` 和 `up -d orbitbeacon`。
3. 启动并检查 readiness，确认数据库格式匹配，再升级 Agent。

Server 严格拒绝未知上报字段，新 Agent 对旧 Server 可能得到 `400`，不能假设旧 Server 会忽略 `traffic`、`probe_results` 或 `system.agent_version`。启用新配置前先完成 Server 升级。降级前确认目标版本支持当前数据库格式；Server 不会打开更高版本数据库，加密归档也要求格式及 schema 版本精确匹配，不提供跨 schema 转换。
