# 服务器部署

以下命令从仓库根目录执行。Agent 安装见 [Agent 指南](../docs/agent.md)，日志格式见[日志约定](../docs/conventions.md#日志)。

| Agent 路径 | 属主与权限 |
| --- | --- |
| `/opt/orbit-beacon/` | root:orbitbeacon-agent，`0750` |
| `/opt/orbit-beacon/bin/` | orbitbeacon-agent:orbitbeacon-agent，`0750`；内含 `orbitbeacon-agent` |
| `/opt/orbit-beacon/etc/` | root:orbitbeacon-agent，目录 `0750`、文件 `0640`；agent.json、install-policy.json、ca.pem |
| `/opt/orbit-beacon/state/` | orbitbeacon-agent:orbitbeacon-agent，`0700`；identity、enrollment.token、run.lock 等 |

## 数据目录

- 使用支持文件锁、可写的本地文件系统；每个数据目录只运行一个服务器。
- 数据目录归运行用户所有，权限 `0700`；数据库、SQLite sidecar 与 `secrets.key` 为同用户所有的 `0600` 普通文件。
- 路径不得经过符号链接；祖先目录须归 root 或运行用户所有；组或其他用户可写时须有 sticky 保护。不要直接使用权限不符的仓库目录。
- 遇到 `data_dir_in_use` 先查占用进程；不要删除 `server.lock`、SQLite sidecar 或恢复 journal 强行启动。

## 配置参考

**CLI > `ORBITBEACON_` 环境变量 > JSON > 默认值**。JSON 由 `--config` 指定，未知键、null、错误类型或无效值会报错。环境变量用大写键名，CLI 用连字符，例如 `ORBITBEACON_MAX_NODES=100`、`--max-nodes 100`。Agent 本地参数也使用此优先级，固定安装策略另行校验。

| JSON 键 | 默认值 | 含义 |
| --- | --- | --- |
| `listen_addr` | `127.0.0.1:8080` | HTTP 业务监听地址 |
| `data_dir` | `./data` | 持久化目录，遵守[目录要求](#数据目录) |
| `public_url` | `http://localhost:8080` | 浏览器可信源，见下文 |
| `max_nodes` | `500` | 正整数，包含待注册、禁用节点；不是性能承诺 |
| `trusted_proxies` | `[]` | 可信代理 IP / CIDR，见下文 |
| `web_update_interval_seconds` | `10` | 整数 2–60 秒，网页推送合并与 HTTP 轮询周期；重启生效，不改变 Agent 采集周期 |
| `metrics_listen` | 空字符串 | 独立 HTTP `/metrics`（服务自身）与 `/metrics/nodes`（节点）监听，主监听不提供；无认证，节点指标含节点名、地区、服务商、磁盘路径和网卡名，须绑定回环或受保护网络 |
| `access_log` | `false` | 访问日志；健康检查仅记录 5xx，不记录查询字符串、Cookie、Authorization 或注册票据 |
| `allow_insecure_webhooks` | `false` | 允许字面回环地址的 HTTP Webhook，见[通知网络](#通知网络) |
| `notification_allow_private_networks` | `false` | 允许内网通知目标，见[通知网络](#通知网络) |
| `database` | `{}` | 默认 SQLite；MySQL 使用完整 JSON 对象，见[数据库](#数据库) |

`public_url` 与 Origin：

- 使用真实 IP、域名或不同端口时须显式设置，与浏览器规范化后的协议、主机和端口精确匹配；DNS 名转小写，IPv6 规范化，HTTP 80 / HTTPS 443 省略。
- 只接受绝对 HTTP / HTTPS URL，不含凭据、查询或片段；路径不参与 Origin 比较。页面与 API 同源，不支持子路径部署。
- 不从请求 Host 或转发头推断可信源，`X-Forwarded-Proto` 不参与此判断。
- Agent 与 `health --url` 只接受根 URL；HTTP 不能搭配 `--ca-file`。

`trusted_proxies` 与客户端地址：

- 仅直连来源可信时解析 `X-Forwarded-For`，从右向左跳过可信代理，取首个不可信地址；缺失、畸形或全可信时回退直连 IP。
- JSON 使用数组；环境变量和 CLI 使用逗号分隔列表；禁止 `/0`。
- 同机原生代理可用 `["127.0.0.1", "::1"]`。容器填写服务器实际看到的代理源 IP 或最小 CIDR，避免信任整个共享网络；入口代理须覆盖或正确追加真实客户端地址。

会话 Cookie 为 `orbitbeacon_session`：host-only（无 Domain）、`HttpOnly`、`SameSite=Lax`、`Path=/`；仅 HTTPS `public_url` 启用 `Secure`，不受代理到服务器的协议或转发头影响。

### 初始化凭据

[快速开始](../README.md#2-启动)说明初始账户。需要自定义时，在首次 `serve` 前设置 `ORBITBEACON_ADMIN_USERNAME`、`ORBITBEACON_ADMIN_PASSWORD`：

- 可单项覆盖，未设置的一项保留默认；显式空值或无效凭据拒绝启动。密码为 8–1024 个 UTF-8 字节，不裁剪空格。
- 已有用户时忽略两变量，不重置密码或新增账户；初始化变量不写入 JSON 或备份。
- systemd 用 `[Service]` drop-in 的 `Environment=` / `EnvironmentFile=`，限制凭据文件读取权限；不要把密码放入命令参数。Compose 已显式转发两变量，显式空值也会传入。

## systemd

配置示例为 [`systemd/server.json`](systemd/server.json)。已有对应架构的二进制可跳过编译；Go 版本见 [go.mod](../go.mod)。

```sh
mkdir -p build
CGO_ENABLED=0 go build -trimpath -o build/orbitbeacon-server ./cmd/orbitbeacon-server
# 账户已存在时跳过 useradd。
sudo useradd --system --user-group --home /var/lib/orbitbeacon \
  --shell /usr/sbin/nologin orbitbeacon
sudo install -D -o root -g root -m 0755 \
  build/orbitbeacon-server /usr/local/bin/orbitbeacon-server
sudo install -D -o root -g orbitbeacon -m 0640 \
  deploy/systemd/server.json /etc/orbitbeacon/server.json
sudo install -D -o root -g root -m 0644 \
  deploy/systemd/orbitbeacon-server.service /etc/systemd/system/orbitbeacon-server.service
sudo install -d -o orbitbeacon -g orbitbeacon -m 0700 /var/lib/orbitbeacon
sudoedit /etc/orbitbeacon/server.json
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-server
sudo journalctl -u orbitbeacon-server -n 100 --no-pager
```

- 启动前按[配置参考](#配置参考)修改访问源及初始化变量；直接远程 HTTP 还需调整监听并限制网络访问。
- 数据目录变更时同步修改 unit 的 `ReadWritePaths=` 和目录权限。
- unit 不授予 capabilities；服务器在 `8080` 后接代理，低端口由代理绑定。

## Docker Compose

[`compose.yaml`](docker/compose.yaml) 使用 GHCR 镜像；回退标签为 `0.1.0`，不随 `VERSION` 自动更新。显式选择已发布版本并持久保存环境变量。

```sh
export ORBITBEACON_PUBLIC_URL=http://localhost:8080
export ORBITBEACON_IMAGE_TAG="$(cat VERSION)"
# 私有包需先执行 docker login ghcr.io 认证。
docker compose -f deploy/docker/compose.yaml pull
docker compose -f deploy/docker/compose.yaml up -d
docker compose -f deploy/docker/compose.yaml ps
docker compose -f deploy/docker/compose.yaml logs --tail=100 orbitbeacon
```

- 容器监听 `0.0.0.0:8080`，仅发布至宿主机 `127.0.0.1:8080`；直接远程 HTTP 需显式调整端口发布并限制访问。
- 数据卷固定名 `orbitbeacon-data`；不要执行 `down -v` 或删除卷。运行身份为 `65532:65532`，bind mount 须匹配此身份及[目录权限](#数据目录)。
- [`docker/server.json`](docker/server.json) 构建时复制入镜像；编辑仓库副本不影响现有容器。额外配置须在 Compose `environment` 中传入，或只读挂载完整配置到 `/etc/orbitbeacon/server.json`。
- 宿主机变量只有被 Compose 引用或显式传入才进入容器；环境或镜像变更后用 `up -d` 重建。指标采集另配可达网络或端口。
- 本地镜像构建见 [`build/server.Dockerfile`](../build/server.Dockerfile)。

## HTTPS 与反向代理

- 服务器仅提供 HTTP，不生成或加载入站 TLS 证书。HTTP 不加密密码、会话、节点凭据或上报；仅用于本机或受控网络，公网由外置代理提供可信 HTTPS。
- 代理终结 TLS，业务端口不对公网开放；按[配置参考](#配置参考)设置访问源、代理信任与 Cookie。
- 证书须受客户端信任；私有 CA 配置浏览器信任，Agent 和健康检查使用 `--ca-file`，不跳过校验。
- 选择 [Caddyfile](proxy/Caddyfile) 或 [nginx.conf](proxy/nginx.conf)，修改域名与证书路径。Caddy 自动申请证书，所需版本见示例注释；nginx 需已有证书，示例由 `http` 块包含。

```sh
sudo install -D -o root -g root -m 0644 deploy/proxy/Caddyfile /etc/caddy/Caddyfile
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy
# 也可改为安装 nginx 示例。
sudo install -D -o root -g root -m 0644 deploy/proxy/nginx.conf /etc/nginx/conf.d/orbitbeacon.conf
sudo nginx -t
sudo systemctl reload nginx
```

- 示例均限制请求体 1 MiB、保留 Host 并设置转发头；HSTS 含 `includeSubDomains`，仅在所有子域名支持 HTTPS 时保留。
- 保持 WS 升级、SSE 无缓冲及长连接超时；nginx 示例已配置，不要覆盖 `X-Accel-Buffering: no`。连接额度见[实时传输](../docs/architecture.md#实时传输)，错误代理配置可能让访客共用额度。
- nginx 示例有登录限流，Caddy 须另配边缘限流。

## 日常检查

```sh
curl --fail http://localhost:8080/healthz/ready
/usr/local/bin/orbitbeacon-server health --url http://localhost:8080
# 私有 CA 需追加 --ca-file /secure/path/ca.pem。
/usr/local/bin/orbitbeacon-server health --url https://beacon.your-domain.com
```

- `/healthz/live` 只检查进程，`/healthz/ready` 检查接流状态与当前业务域可读写，均无需认证；历史单独故障不代表业务域失效。它们不证明 Agent 已上报，另查节点最近上报。
- 排障记录响应 `code` 与 `request_id`，不发送 Cookie、密码或凭据。日志字段见[约定](../docs/conventions.md#日志)。

## 数据库

省略 `database` 或使用 `{}` 保持本地 SQLite。MySQL 使用两个预建 database，业务和历史分别存储；`data_dir` 仍须持久化目录锁、加密 key、归档工作文件及恢复决定。当前开发基准为 MySQL 26.10.0，启动按事务、持久化及触发器所需能力检查，不按版本字符串设门槛。

```json
{
  "database": {
    "primary": {
      "driver": "mysql",
      "network": "tcp",
      "address": "db.example:3306",
      "user": "orbitbeacon",
      "password_file": "/run/secrets/database-password",
      "main_database": "orbitbeacon",
      "history_database": "orbitbeacon_history"
    }
  }
}
```

- `ORBITBEACON_DATABASE` 整体替换 JSON 对象，不与文件中的 endpoint 或 failover 字段深合并。不提供携带数据库密码的 flag；`password_file` 必须归运行用户所有、`0600`、非空且不超过 64 KiB，只移除一个末尾 LF。
- TCP 省略 `tls` 使用明文，仅限本机或受控网络；数据库凭据与业务数据不受传输加密保护。显式 `tls: {}` 启用 TLS 1.2+，校验系统根 CA 与 address 主机名；可用 `ca_file` 和 `server_name` 指定私有 CA 与验证名称。配置 TLS 后，证书、主机名或握手失败均拒绝连接，不跳过校验，也不回退明文。本机 Unix socket 使用 `network: "unix"` 与绝对 socket 路径，不带 `tls`。
- 私有 CA 示例：在 endpoint 中加入 `"tls": {"server_name": "db.example", "ca_file": "/etc/orbitbeacon/mysql-ca.pem"}`，并将 CA 文件只读挂载到相同路径。
- `network` 默认 `tcp`；database 名只接受 ASCII 字母/下划线开头、随后字母数字下划线，最长 64 字节，两个名称不得相同。默认名称如上。SQLite endpoint 不接受 MySQL 字段或自定义路径。
- 应用账户只需两个 database 内的 `SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, DROP, INDEX, REFERENCES, TRIGGER`；不需 SUPER 或全局 CREATE DATABASE。仅此 Server 写入应用 schema，不运行第二个使用不同 data_dir 的 Server，也不要由 DBA 直接修改业务行。
- MySQL 必须可写并使用支持事务的 InnoDB，`innodb_flush_log_at_trx_commit=1`；启用 binlog 时 DBA 须设 `sync_binlog=1` 与 `log_bin_trust_function_creators=1`。Server 校验部署条件，不擅自更改全局变量。所有连接固定 5 秒连接、读、写超时。
- COMMIT 结果未知返回 `503 storage_commit_unknown`，先查询当前状态，不直接重发写请求。主库故障不返回旧缓存伪造成功；历史库不能读取时，包含 Monitoring 的归档失败。
- 保留并备份与数据库匹配的 `data_dir/secrets.key`。已有 TOTP、通知或备份加密数据而 key 缺失时拒绝启动，不生成替代 key；恢复原 key 或使用完整、可验证的配置归档恢复。
- systemd 沿用 `/etc/orbitbeacon/server.json`，secret 放在 `ProtectHome` 之外的 `/etc/orbitbeacon` 或 `/run`。Docker 的配置、secret 和 CA 另作只读挂载，secret 属主为 `65532:65532` 且 `0600`；不因容器读不到而放宽权限。默认 Compose 不添加 MySQL 服务。

Docker 可用 override 文件补充现有服务：

```yaml
services:
  orbitbeacon:
    command: ["serve", "--config", "/etc/orbitbeacon/server.json"]
    volumes:
      - /etc/orbitbeacon/server.json:/etc/orbitbeacon/server.json:ro
      - /etc/orbitbeacon/database-password:/run/secrets/database-password:ro
```

远程开发地址、database 名与 Redis DB 索引见 [远程开发环境](../temp/README.md)。凭据只保存在私有文件中，不写入文档或命令行。

## 通知网络

- 默认拒绝通知访问内网、回环及元数据目标；拨号时检查最终 IP，HTTP 不使用代理或重定向。
- 内网 SMTP / Webhook 需开启 `notification_allow_private_networks`；字面回环 HTTP Webhook 还须同时开启 `allow_insecure_webhooks`，元数据地址始终禁止。
- HTTPS 和 SMTP 校验证书；SMTP 非 465 端口要求 STARTTLS。
- 控制台可管理通知通道、测试投递与查看记录；Agent 探测使用不同策略，见[服务探测](../docs/agent.md#服务探测)。

### Webhook 签名验证

配置 secret 后，接收方读取 `X-OrbitBeacon-Timestamp` 的 Unix 秒数字符串，建议拒绝与当前时间相差超过 5 分钟的请求。将该头的原始字符串、一个英文句点 `.` 和未经解析或重新编码的原始请求体依次拼接，以 secret 为密钥计算 HMAC-SHA256。`X-OrbitBeacon-Signature` 格式为 `v1=<小写十六进制摘要>`；拒绝其他前缀，并使用常量时间比较。未配置 secret 时只有时间戳头，没有签名头。

以下 Go 片段置于返回 `bool` 的验证函数中；`r` 为请求、`body` 为原始请求体字节、`secret` 为共享密钥：

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

## 导出与备份

在“设置 → 数据操作 / Settings → Data operations”发起导出、配置计划，在“任务历史 / Task history”查看结果。受理后可离开页面；手动下载，不自动下载。任务与恢复密码由所有管理员共享，通知可靠性见[告警与通知](../docs/architecture.md#告警与通知)。

### 归档范围

| 类别 | 内容 |
| --- | --- |
| 配置 / Configuration | 密码哈希、各账户改密要求、节点持久身份、规则、计划、通知及对象存储凭据、匹配的 `secrets.key`、全站状态页开关 |
| 监控 / Monitoring | 已落库节点观测、流量、探测与指标历史 |
| 事件 / Events | 审计、告警、已结束投递、站内通知及已结束任务历史；不含历史备份密码 |

- 至少选择一类，默认仅配置。不归档登录会话、注册票据、待执行任务、待发送通知、旧下载产物，以及机器部署配置、TLS 证书、可执行文件；这些部署文件另存。
- 配置 settings 仅包含全站状态页开关、API 令牌全局限额及存储故障通知订阅；各项校验结构和值，不接受任意键。配置恢复整体替换这些设置；仅监控 / 事件恢复保留当前值。缺失状态页开关时默认开启。
- 每包为 `.tar.gz.age`，独立 16 位随机大小写字母/数字密码；在详情主动显示、复制或下载恢复信息。密码不随远程包上传，没有主密码托管。
- 另存每包密码；原机丢失或密码记录过期且未另存时无法恢复。下载与密码有效期见[存储与保留](../docs/architecture.md#存储与保留)，快照边界见[备份](../docs/architecture.md#备份)。

### 计划

- 每计划一个本地或对象存储目标，可建多个计划；新建默认停用，任务活动期间不能编辑、停用或删除。
- Cron 为五段，默认 `0 3 * * *`；不接受秒字段、`@daily`、时区前缀或命令。按服务器进程 `time.Local` 执行，页面显示服务器时区；更改系统/容器时区后重启，容器不自动继承宿主时区。
- 错过多个周期只补一次，同计划已有任务时不堆积。没有连接测试、任务取消或重试按钮；失败后重新导出或等下一周期。
- 结果未知时核对对象位置、大小与哈希，先确认是否已写入。

### 备份目标

本地目标：

- 预建绝对路径，归运行用户所有、`0700`、可写且无符号链接；服务器不自动改权限。每计划写入 `orbitbeacon-<plan_id>/`，默认保留最近 7 份，可设 1–1000；仅成功后清理该计划旧包。
- 删除计划不删除已有包；清理失败单独警告。systemd 在数据目录外备份时，精确增加 `ReadWritePaths=`；Compose 用数据卷或另挂可写目录，不依赖只读根目录或小容量 tmpfs，staging 使用 data-dir。
- 计划包仅显示本地路径或准确 Object key，不提供网页下载副本或拼造 URL。

对象存储使用显式静态凭据，不读取主机默认凭据链；只上传，保留期由云端生命周期管理。

| Provider | Endpoint / Region / 寻址 |
| --- | --- |
| COS | `https://cos.<region>.myqcloud.com`；region 必填，bucket 含 APPID，virtual-host |
| OSS | 推荐 `https://s3.oss-<region>.aliyuncs.com`；region 必填，virtual-host；自定义域须绑定 bucket 并配置 TLS，按该域寻址 |
| R2 | `https://<account>.r2.cloudflarestorage.com`；region 为 `auto`，path-style |
| AWS S3 | 默认 `https://s3.<region>.amazonaws.com`；region 必填，virtual-host |
| S3 compatible | endpoint、region、bucket 必填，默认 path-style |

- Endpoint 为 HTTPS 源站，可带端口，不含路径、凭据、查询或片段；prefix 为安全相对路径。TLS 至少 1.2，校验证书，不使用 HTTP 代理或重定向。
- 默认拒绝私网；目标显式允许私网后仍禁止回环、链路本地、非全局单播及元数据地址，最终 bucket 域名也校验 DNS/IP。
- 凭据加密保存，编辑不回填；更换 provider 须提交匹配 endpoint、region 与完整新凭据。正式使用前验证上传及恢复，不承诺供应商全部特性或自定义域兼容性。

## 恢复

1. 停止服务器，以服务账号操作相同数据目录。归档格式要求见[升级](#升级)。
2. 密码文件归运行用户所有、`0600`，含该包密码，可带换行；省略 `--password-file` 则终端无回显输入，不接受参数或环境变量明文密码。

```sh
sudo -u orbitbeacon /usr/local/bin/orbitbeacon-server restore --config /etc/orbitbeacon/server.json \
  --input /backup/orbitbeacon-....tar.gz.age \
  --replace --password-file /backup/password.txt
```

- 目标已有业务数据须加 `--replace`；空目录可省略，但包内须含配置。所选类别整体替换，未选类别保留，不逐条合并；稳定 ID 依赖不满足则整次拒绝。
- 配置恢复后计划全部停用；事件不重放任务或投递，目标原活动工作标为恢复中断。开关处理见[归档范围](#归档范围)。
- `prepared` 决定回滚完整旧状态，`committed` 决定只完成新状态。SQLite 使用原子文件组安装；含 MySQL 时各域分别在 guard 事务内替换，并与 key 共用本地恢复决定。目标不可达时保留日志并拒绝服务，不启动半恢复状态。成功输出 `.restore-<id>/old/` 原副本位置，确认正常后再妥善处理。
- 再启动并按[日常检查](#日常检查)验证 readiness、登录、节点上报及通知凭据。
- Compose 先停服务，再用一次性 `compose run` 将归档和密码文件只读挂载，使用相同数据卷及运行身份执行恢复。

## 升级

原生 Agent 可通过控制台按版本远程升级，操作与回滚见[远程升级](../docs/agent.md#远程升级)。安装器的 `--update-base-url URL` 固定 HTTPS 发布源，写入 root 所有的策略；未配置时不开放 upgrade。下载、切换与回滚均以 Agent 用户运行，root 所有的 `libexec/orbitbeacon-agent-rollback` 由无 `+` 前缀的 `ExecStartPre=-` 调用；`etc/` 不可由 Agent 写入。管理员绝不能运行 `sudo orbitbeacon-agent`。

`SHA256SUMS` 与二进制同源，只防传输损坏，不能防发布源入侵；没有签名验证。不接受此信任边界的节点应从本地 `remote_commands` 移除 `upgrade`。

当前主库、历史库及归档要求 schema baseline **2**。旧 baseline 1 数据库和归档明确拒绝，不提供自动迁移或清空。使用新数据目录与空的专用 MySQL schema；升级前保留旧数据库、key、归档密码和原二进制。结构调整直接更新可信原生建表 SQL。

以下步骤仅适用于 schema 相同的版本更新；跨 baseline 不直接复用旧数据目录或旧归档。

1. 完成所需类别备份，另存密码、部署配置和证书，停止服务器。
2. systemd 替换二进制；Compose 设置目标镜像标签，执行 `pull orbitbeacon`、`up -d orbitbeacon`。
3. 启动并检查 readiness、登录和节点上报，再升级 Agent。

先升级服务器，再升级 Agent；升级或降级前核对数据库及归档兼容性，遥测字段兼容不代表完整协议兼容。
