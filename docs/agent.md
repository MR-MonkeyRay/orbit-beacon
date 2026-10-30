# Linux Agent 注册

流程：**创建节点并签发注册票据 → 安装并注册 → 确认上报**。术语见[约定](conventions.md)，服务器部署与网络信任见[部署指南](../deploy/README.md#https-与反向代理)。

## 准备

- 服务器已启动，Agent 可访问其源站；管理员已完成初始化改密。
- 安装脚本支持 Linux amd64 / arm64，需 root 安装、非 root 运行；支持 `useradd` 或 BusyBox 的 `adduser` / `addgroup`，并需 `runuser` 或 `su`。
- 下载需 `curl`、`tar`、`sha256sum`，另需常用 POSIX 工具、`awk`、`install`。离线提供二进制时不执行下载和解包。
- 不要复制服务器私钥、跳过证书校验，或把管理员 Cookie 传到 Agent 主机。

## 注册票据

在节点详情“注册 Agent / Enroll Agent”选择“签发注册票据 / Issue enrollment ticket”；已有节点直接签发，不重复创建。注册票据只显示一次，关闭前复制。

- 单节点注册票据：有效 15 分钟，仅可认领一次。
- 批量注册票据：在“设置 → 分组 / Settings → Groups”签发，有效 30 分钟、最多 100 次认领，仍受服务器节点容量限制。
- 吊销注册票据只阻止后续认领与该票据的重试；已注册 Agent 保留节点凭据。

通过已核验主机身份的 SSH/SCP 等安全通道，把注册票据交给 Agent 主机。不要写入命令参数、shell 历史、公共日志或仓库。

## 安装与启动

使用与服务器匹配、已发布且包含脚本的固定版本；检查后运行，不要把网络脚本管道给 root shell。源码部署可读取 `VERSION`，否则手动设置版本：

```sh
VERSION="$(cat VERSION)"
curl --fail --silent --show-error --location --proto '=https' \
  --proto-redir '=https' --tlsv1.2 -o install-agent.sh \
  "https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v$VERSION/deploy/install-agent.sh"
less install-agent.sh
sudo sh ./install-agent.sh \
  --server https://beacon.your-domain.com --version "$VERSION"
```

终端运行时隐藏输入注册票据。也可使用 `--token-file /安全路径/enrollment.token`；自动化通过标准输入传入，不启用 `set -x`，并提前完成 sudo 认证：

```sh
sudo -v
# TOKEN 已从安全来源读入当前 shell。
printf '%s\n' "$TOKEN" | sudo sh ./install-agent.sh \
  --server https://beacon.your-domain.com --version "$VERSION"
unset TOKEN
```

- `--server` 与 CA 要求见[配置参考](../deploy/README.md#配置参考)和 [HTTPS](../deploy/README.md#https-与反向代理)；私有 CA 加 `--ca-file /安全路径/ca.pem`。
- 脚本与发布包下载仅用 HTTPS；`SHA256SUMS` 校验不是独立签名验证。
- 离线安装用 `--binary /安全路径/orbitbeacon-agent-linux-amd64`，可省略版本，自行核对来源与架构。

脚本创建专用账号 `orbitbeacon-agent`，固定实际 UID/GID，不保证是 `1000:1000`。安装路径：

| 路径 | 内容与权限 |
| --- | --- |
| `/usr/local/bin/orbitbeacon-agent` | root 所有的可执行文件 |
| `/etc/orbitbeacon/install-policy.json` | root 所有的安装策略，模式 `native` |
| `/etc/orbitbeacon/agent.json` | 本地运行参数；新装为 `{}`，root:orbitbeacon-agent、`0640`；重装保留，拒绝符号链接 |
| `/etc/orbitbeacon/ca.pem` | 可选 CA 副本 |
| `/var/lib/orbitbeacon-agent` | Agent 账号所有，`0700`；身份和状态不应共享 |
| `/etc/systemd/system/orbitbeacon-agent.service` | 非 root systemd 服务 |

脚本以 Agent 账号注册，成功后启动服务；没有运行中的 systemd 时打印前台启动命令。失败保留 state 中的 `enrollment.token`，不启用服务。修复 URL、CA、网络或票据问题后重试：

```sh
sudo -u orbitbeacon-agent /usr/local/bin/orbitbeacon-agent enroll \
  --install-policy /etc/orbitbeacon/install-policy.json \
  --config /etc/orbitbeacon/agent.json \
  --token-file /var/lib/orbitbeacon-agent/enrollment.token
```

重试成功后，以原来的 `--server`、版本和 CA 参数重跑安装脚本，并加 `< /dev/null`，让脚本补装并启动服务，不再提供注册票据。

成功后删除 state 中的注册票据文件，但源文件和管理机副本须自行清理。若提示清理失败，先核对注册结果，不能假定认领已回滚。

## 运行检查

```sh
sudo systemctl status orbitbeacon-agent
sudo journalctl -u orbitbeacon-agent -n 100 --no-pager
sudo -u orbitbeacon-agent /usr/local/bin/orbitbeacon-agent doctor \
  --install-policy /etc/orbitbeacon/install-policy.json --config /etc/orbitbeacon/agent.json --json
```

`doctor` 检查配置、身份文件、HTTP/HTTPS 连通性和采集（HTTPS 同时验证证书），**不注册、不上报、不验证节点凭据**；`authentication: "not_checked"` 是预期结果。

在控制台查看最近上报与样本，“Agent 设置 / Agent settings”查看实际运行参数；状态含义见[状态与历史](architecture.md#状态与历史)，公开节点步骤见[快速开始](../README.md#6-公开节点)。

常见问题：

| 现象 | 处理 |
| --- | --- |
| `runtime_identity_mismatch` | 用 policy 指定的非 root UID/GID 运行，不能直接 `sudo orbitbeacon-agent run` |
| `install_policy_conflict` | 检查额外 `--config` 和 `ORBITBEACON_*`，按[安装策略校验](#本地运行参数与-reload)消除冲突 |
| `401` / `403 node_disabled` | 检查节点是否禁用或凭据已吊销；按[身份变更规则](architecture.md#状态与历史)启用或重新签发注册票据 |
| `409 identity_conflict`，退出码 3 | 同一身份被更新进程取代；unit 禁止自动重启争抢。克隆机器必须独立 state 并分别注册 |
| 网络或其他 HTTP 错误 | Agent 退避重试，记录首次失败、至少间隔 5 分钟的连续失败摘要及恢复；样本处理见[数据流](architecture.md#数据流) |

`run` 独占 state 中的 `run.lock`。不要删除或替换正在使用的锁；退出后留下空锁文件是正常的。`identity.json` 包含节点凭据，不要手改或复制给另一台机器。`enroll` 不传 `--token-file` 仅恢复已有凭据，不会取得新的授权。

## 本地运行参数与 reload

五项参数在 `run`、`enroll`、`doctor` 中一致，合并顺序见[配置参考](../deploy/README.md#配置参考)。安装策略固定信任根、身份与目录；每个来源独立检查，后续覆盖不能隐藏冲突。

| 参数 | 默认值 | 范围 / 选择 |
| --- | --- | --- |
| `report_interval_seconds` | `10` | 5–60 秒，采集与正常上报共用周期 |
| `disk_interval_seconds` | `60` | 60–3600 秒 |
| `interfaces` | `[]` | 最多 8 项；空数组使用默认路由网卡 |
| `disk_paths` | `[]` | 最多 8 个绝对路径；空数组使用默认根路径 |
| `probe_targets` | `[]` | 最多 5 项；空数组不探测 |

选择还受 JSON 转义后 64 KiB 报告预算约束。

环境变量为 `ORBITBEACON_REPORT_INTERVAL_SECONDS` 等大写参数名；flag 使用 `--report-interval-seconds` 等连字符形式。列表必须是 JSON 数组字符串，例如 `ORBITBEACON_INTERFACES='["eth0"]'` 或 `--probe-targets '["https://example.com/a,b"]'`，不按逗号拆分。

修改后执行 `sudo systemctl reload orbitbeacon-agent`（前台进程用 `kill -HUP <PID>`）。reload 重读原 JSON，沿用启动时环境和显式 flags，不重读 policy；有效变更在采样边界完整应用并重采，不重置身份或累计流量，坏配置保留旧值。检查日志 `configuration_reloaded` / `configuration_unchanged` / `configuration_reload_rejected` 和下一报告；systemctl 成功只代表信号送达。

## 升级与卸载

按[升级顺序](../deploy/README.md#升级)检查服务器后，下载并检查目标版本的脚本，更新 `VERSION`，再执行：

```sh
sudo sh ./install-agent.sh --server https://beacon.your-domain.com \
  --version "$VERSION" < /dev/null
```

已有 `identity.json` 且没有传入注册票据时，脚本保留身份与本地 `agent.json`，跳过注册，替换二进制后重启服务。仍须传相同 `--server` 和所需 `--ca-file`；省略 CA 参数会使新 policy 改用系统信任。

卸载前先在控制台吊销节点凭据或删除节点；对应 API 为 `DELETE /api/v1/nodes/{id}/credential`。然后：

```sh
sudo sh ./install-agent.sh --uninstall
# 输入 yes 确认；自动化可加 --yes。
```

卸载会移除 unit、二进制、policy、本地配置、CA 副本和**全部状态目录**，保留系统账号；没有 systemd 时须先手动停止进程。仅删除本地文件不会使服务器上的节点凭据失效。

## 手动前台安装

仅在不使用安装脚本时采用。以下在 Agent 主机以普通用户执行，UID/GID 均不得为 0；准备匹配架构的 `build/orbitbeacon-agent`：

```sh
AGENT_UID="$(id -u)"
AGENT_GID="$(id -g)"
sudo install -o root -g root -m 0755 \
  build/orbitbeacon-agent /usr/local/bin/orbitbeacon-agent
sudo install -d -o root -g root -m 0755 /etc/orbitbeacon
sudo install -d -o "$AGENT_UID" -g "$AGENT_GID" -m 0700 /var/lib/orbitbeacon-agent
sudo tee /etc/orbitbeacon/install-policy.json > /dev/null <<EOF
{
  "server_url": "https://beacon.your-domain.com",
  "ca_file": "",
  "state_dir": "/var/lib/orbitbeacon-agent",
  "expected_uid": $AGENT_UID,
  "expected_gid": $AGENT_GID,
  "expected_account": "",
  "install_mode": "foreground"
}
EOF
sudo chown root:root /etc/orbitbeacon/install-policy.json
sudo chmod 0644 /etc/orbitbeacon/install-policy.json
printf '%s\n' '{}' | sudo tee /etc/orbitbeacon/agent.json > /dev/null
sudo chown "root:$AGENT_GID" /etc/orbitbeacon/agent.json
sudo chmod 0640 /etc/orbitbeacon/agent.json
```

CA 使用限制见[配置参考](../deploy/README.md#配置参考)。私有 CA 由 root 安装为 `/etc/orbitbeacon/ca.pem`（`0644`），并填写 policy 的 `ca_file`。policy、CA 及祖先目录须 root 所有且不可被组/其他用户写入，无符号链接；不要放在家目录或 `/tmp`。state 按[安装路径](#安装与启动)的要求归运行用户所有。

通过安全通道接收注册票据，保存为当前用户所有的 `0600` 普通文件；路径无符号链接，祖先仅 root 或运行用户所有且不可被组/其他用户写入（安全 sticky 临时目录例外）。以普通用户执行：

```sh
TOKEN_SOURCE=/安全路径/enrollment.token
install -m 0600 "$TOKEN_SOURCE" /var/lib/orbitbeacon-agent/enrollment.token
orbitbeacon-agent enroll --install-policy /etc/orbitbeacon/install-policy.json \
  --config /etc/orbitbeacon/agent.json \
  --token-file /var/lib/orbitbeacon-agent/enrollment.token
orbitbeacon-agent doctor --install-policy /etc/orbitbeacon/install-policy.json --config /etc/orbitbeacon/agent.json --json
orbitbeacon-agent run --install-policy /etc/orbitbeacon/install-policy.json --config /etc/orbitbeacon/agent.json
```

Ctrl+C 或 SIGTERM 停止。额外的 Agent JSON 配置非必需。若需要 systemd 长期运行，优先使用安装脚本；自行配置 [unit](../deploy/systemd/orbitbeacon-agent.service) 时必须同步账号、state 所有权、policy 的 UID/GID 和 `install_mode: "native"`，不能只复制 unit。`install_mode` 本身不会安装服务。

## API 签发

- 在可信管理机登录，取得会话与 CSRF，按 [OpenAPI](../api/openapi.yaml) 调用 `POST /api/v1/enrollments`。
- 单节点请求为 `{"node_id":"节点 UUID","mode":"single"}`；批量为 `{"group_id":"分组 UUID","mode":"batch"}`，不能同时提供两个 ID。
- `token` 字段通过安全通道传给安装步骤；妥善清理临时 Cookie 与票据副本，结束后注销会话。Origin 规则见[配置参考](../deploy/README.md#配置参考)。

## 流量累计

网卡选择与网速一致（显式 `interfaces` 或默认路由网卡），按内核收发计数差累计到 UTC 小时桶。新网卡先建基线；重置后从当前计数继续累计。每次启动使用新 epoch，已结算桶在后续采样后移除，确认语义见[状态与历史](architecture.md#状态与历史)。

状态只在内存中：停机期间不计入、重启不补算，最多保留 24 个小时桶，超出时丢弃最旧桶。首次成功读取网卡计数器前发送空列表；后续读取失败不新增计数，已有未结算值仍可重发。流量计费边界见[状态与历史](architecture.md#状态与历史)。

## 服务探测

在 Agent 本地配置中设置 `probe_targets` 并 reload，目标须唯一，每个不超过 1024 字节、不得含空白。目标会向控制台用户暴露，**不要放秘密**。探测从 Agent 网络发起，可访问其可达的内部服务；应限制本地配置写入权限和 Agent 网络出口。

| 目标 | 行为 |
| --- | --- |
| `tcp://host:port` | 仅测 TCP 建连；端口 1–65535，IPv6 如 `tcp://[::1]:443` |
| `http://host/path` / `https://host/path` | GET 至响应头的耗时；低于 400（包括 3xx）为成功，不跟随重定向，不保存 Cookie，最多丢弃 64 KiB 响应体；HTTPS 校验证书并使用系统信任，不使用安装策略的私有 CA |
| `icmp://host` | ICMP Echo 往返耗时；不能带端口、路径或查询；仅 Linux 非特权 datagram socket 支持，其他系统为 `unsupported` |

每 60 秒执行一次，单次超时 5 秒，最多并发 5 个；reload 后新增目标立即开始检查。报告携带该次采样目标集合中最近完成的结果，未完成的省略，删除目标后停止携带；结果去重见[状态与历史](architecture.md#状态与历史)。结果不包含响应体或原始错误信息。

HTTP 探测不使用 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`，不能依赖代理做出口限制。Linux ICMP 要求 Agent 的组 ID 位于 `net.ipv4.ping_group_range`；用 `sysctl net.ipv4.ping_group_range` 检查，按主机或容器策略配置允许范围。权限不足报告 `unsupported`，不会提权，也不需要 `CAP_NET_RAW`。

更多字段和错误码见 [OpenAPI](../api/openapi.yaml)。
