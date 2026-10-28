# Linux Agent 接入

推荐流程：**控制台创建节点并签发凭证 → 安装脚本注册并启动 → 控制台确认上报**。Server 部署见[部署指南](../deploy/README.md)。

## 准备

- Server 已启动并创建管理员；Agent 能访问其 HTTPS 源站，证书受系统信任或有私有 CA 的公开 PEM。
- 安装脚本支持 Linux amd64 / arm64，需 root 安装、非 root 运行；支持 `useradd` 或 BusyBox 的 `adduser` / `addgroup`，并需 `runuser` 或 `su`。
- 下载需 `curl`、`tar`、`sha256sum`，另需常用 POSIX 工具、`awk`、`install`。离线提供二进制时不执行下载和解包。
- 不要复制 Server 私钥、跳过证书校验，或把管理员 Cookie 传到 Agent 主机。

## 1. 签发接入凭证

管理员登录控制台，创建节点，打开节点详情的 **Enroll agent** 页签，选择 **Issue enrollment ticket**。凭证原文只显示一次，关闭弹窗后无法找回。

- 单节点凭证：有效 15 分钟、1 次认领；已有节点直接签发，不要重复创建资产。
- 分组批量凭证：在设置的分组列表签发，有效 30 分钟、最多 100 次认领，仍受 Server 节点容量限制。
- 撤销接入 ticket 只阻止后续认领和该 ticket 的重试，**不会撤销已经接入的节点凭证**。

通过已核验主机身份的 SSH/SCP 等安全通道，把 token 交给 Agent 主机。不要写入命令参数、shell 历史、公共日志或仓库。

## 2. 安装并启动

从与部署版本匹配、**确实包含脚本的固定 tag** 下载脚本，检查后运行；不要把网络脚本直接管道给 root shell。以下在仓库根目录读取版本；非源码部署请手动设置已发布版本：

```sh
VERSION="$(cat VERSION)"
curl --fail --silent --show-error --location --proto '=https' \
  --proto-redir '=https' --tlsv1.2 -o install-agent.sh \
  "https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v$VERSION/deploy/install-agent.sh"
less install-agent.sh
sudo sh ./install-agent.sh \
  --server https://beacon.your-domain.com --version "$VERSION"
```

直接在终端运行时会隐藏输入 token。也可使用 `--token-file /安全路径/enrollment.token`；自动化通过标准输入传入，不启用 `set -x`，并提前完成 sudo 认证：

```sh
sudo -v
# TOKEN 已从安全来源读入当前 shell。
printf '%s\n' "$TOKEN" | sudo sh ./install-agent.sh \
  --server https://beacon.your-domain.com --version "$VERSION"
unset TOKEN
```

私有 CA 加 `--ca-file /安全路径/ca.pem`；只传公开 CA，不传私钥。`--server` 只接受 HTTPS 源站，可带端口，不能带账号、子路径、查询或片段。脚本下载 release 包及 `SHA256SUMS` 并校验完整性，**不是独立的发布签名验证**。离线安装用 `--binary /安全路径/orbitbeacon-agent-linux-amd64` 替代下载，`--version` 可省略；来源和架构自行核对。

脚本创建专用账号 `orbitbeacon-agent`，固定实际 UID/GID，不保证是 `1000:1000`。安装路径：

| 路径 | 内容与权限 |
| --- | --- |
| `/usr/local/bin/orbitbeacon-agent` | root 所有的可执行文件 |
| `/etc/orbitbeacon/install-policy.json` | root 所有的安装策略，模式 `native` |
| `/etc/orbitbeacon/ca.pem` | 可选 CA 副本 |
| `/var/lib/orbitbeacon-agent` | Agent 账号所有，`0700`；身份和状态不应共享 |
| `/etc/systemd/system/orbitbeacon-agent.service` | 非 root systemd 服务 |

脚本以 Agent 账号执行 `enroll`。成功后启动服务；systemd 未运行时只打印前台启动命令，需自行管理进程。注册失败保留状态目录内的 `enrollment.token`，不会启用服务；不要先删除凭证再重试，可检查 URL、CA、网络和有效期后执行：

```sh
sudo -u orbitbeacon-agent /usr/local/bin/orbitbeacon-agent enroll \
  --install-policy /etc/orbitbeacon/install-policy.json \
  --token-file /var/lib/orbitbeacon-agent/enrollment.token
```

重试成功后，以原来的 `--server`、版本和 CA 参数重跑安装脚本，并加 `< /dev/null`，让脚本补装并启动服务，不再提供 token。

注册成功会移除该 token 文件，但不会删除原先通过 `--token-file` 提供的源文件或管理机副本，需另行清理。若提示注册成功但清理失败，应核对服务端结果后删除残留文件，不能认为认领已回滚。

## 3. 确认运行结果

```sh
sudo systemctl status orbitbeacon-agent
sudo journalctl -u orbitbeacon-agent -n 100 --no-pager
sudo -u orbitbeacon-agent /usr/local/bin/orbitbeacon-agent doctor \
  --install-policy /etc/orbitbeacon/install-policy.json --json
```

`doctor` 检查配置、身份文件、HTTPS 连通性和采集，**不注册、不上报、不验证节点凭证**；`authentication: "not_checked"` 是预期结果。

在控制台确认：

- 节点的在线状态、最近上报时间及最新样本；在线窗口为 180 秒，管理列表的持久化时间可能约滞后 30 秒。`enroll` 成功或进程存在都不等于上报成功。
- 修改 **Agent configuration** 后查看 `pending` / `applied` / `error` 及版本号；配置经下一次上报下发，随后上报应用结果。本地 `desired.json` 存在只证明落盘，不能代替服务端确认。
- 首页仅显示管理员显式公开的节点；没有出现在首页不代表接入失败。公开设置独立于内部名称和元数据。

常见问题：

| 现象 | 处理 |
| --- | --- |
| `runtime_identity_mismatch` | 用 policy 指定的非 root UID/GID 运行，不能直接 `sudo orbitbeacon-agent run` |
| `install_policy_conflict` | 检查额外 `--config` 和 `ORBITBEACON_*`；受保护字段必须与 policy 完全一致，后续覆盖不能隐藏冲突 |
| `401` / `403 node_disabled` | 检查凭证是否撤销、节点是否被禁用；启用节点会保留原凭证，撤销后需新 ticket |
| `409 identity_conflict`，退出码 3 | 同一身份被更新进程取代；unit 禁止自动重启争抢。克隆机器必须独立 state 并分别接入 |
| 网络或其他 HTTP 错误 | Agent 退避重试，记录首次失败、至少间隔 5 分钟的连续失败摘要及恢复；断网不积压历史样本 |

`run` 独占 state 中的 `run.lock`。不要删除或替换正在使用的锁；退出后留下空锁文件是正常的。`identity.json` 包含长期凭证，不要手改或复制给另一台机器。`enroll` 不传 `--token-file` 仅恢复已有凭证，不会取得新的授权。

## 升级与卸载

**先升级 Server，再升级 Agent。** 遥测报告忽略新增未知字段，但仍校验已知指标、来源和正文大小；注册、恢复及管理写入继续严格拒绝未知字段。不支持的协议版本仍会拒绝。先检查 Server readiness，再运行经过检查的新版本安装脚本：

```sh
sudo sh ./install-agent.sh --server https://beacon.your-domain.com \
  --version "$VERSION" < /dev/null
```

已有 `identity.json` 且没有传入 token 时，脚本保留身份并跳过注册，替换二进制后重启服务。仍须传相同 `--server` 和所需 `--ca-file`；省略 CA 参数会使新 policy 改用系统信任。Agent 的 `update --version ...` 当前不可用，控制台不提供远程升级、终端或任意命令执行。

卸载前先在控制台撤销节点凭证或删除节点；对应撤销 API 为 `DELETE /api/v1/nodes/{id}/credential`。然后：

```sh
sudo sh ./install-agent.sh --uninstall
# 输入 yes 确认；自动化可加 --yes。
```

卸载会移除 unit、二进制、policy、CA 副本和**全部状态目录**，保留系统账号；没有 systemd 时须先手动停止进程。本地删除不会使服务端凭证失效。

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
```

私有 CA 先以 root 安装为 `/etc/orbitbeacon/ca.pem`（`0644`），再将 policy 的 `ca_file` 设为该绝对路径。policy、CA 及各级祖先目录必须 root 所有、组/其他用户不可写，路径不得经过符号链接，不能放进家目录或 `/tmp`。state 目录归运行用户、权限 `0700`。

将通过安全通道收到的 token 保存为当前用户所有的 `0600` 普通文件；token 和祖先路径不得经过符号链接，祖先只允许 root 或运行用户所有且不可被组/其他用户写入（安全 sticky 临时目录例外）。下面将 `TOKEN_SOURCE` 改为该文件的绝对路径，以普通用户执行：

```sh
TOKEN_SOURCE=/安全路径/enrollment.token
install -m 0600 "$TOKEN_SOURCE" /var/lib/orbitbeacon-agent/enrollment.token
orbitbeacon-agent enroll --install-policy /etc/orbitbeacon/install-policy.json \
  --token-file /var/lib/orbitbeacon-agent/enrollment.token
orbitbeacon-agent doctor --install-policy /etc/orbitbeacon/install-policy.json --json
orbitbeacon-agent run --install-policy /etc/orbitbeacon/install-policy.json
```

Ctrl+C 或 SIGTERM 停止。额外的 Agent JSON 配置非必需。若需要 systemd 长期运行，优先使用安装脚本；自行配置 [unit](../deploy/systemd/orbitbeacon-agent.service) 时必须同步账号、state 所有权、policy 的 UID/GID 和 `install_mode: "native"`，不能只复制 unit。`install_mode` 本身不会安装服务。

## API 签发（自动化）

在可信管理机的同一个 Bash 会话执行，需要 curl、jq。也可从已有节点详情 URL 取得 UUID，设置 `NODE_ID` 后跳过创建节点。下面使用系统信任的正式证书；私有 CA 将 `TLS=()` 改为 `TLS=(--cacert /安全路径/ca.pem)`。

```bash
set -euo pipefail
umask 077
ORIGIN='https://beacon.your-domain.com'
TLS=()
WORK="$(mktemp -d)"
read -r -p '管理员用户名: ' ADMIN
read -r -s -p '管理员密码: ' PASSWORD
printf '\n'
printf '%s' "$PASSWORD" | jq -Rs --arg username "$ADMIN" \
  '{username: $username, password: .}' | \
  curl -fsS "${TLS[@]}" -c "$WORK/cookies" \
    -H "Origin: $ORIGIN" -H 'Content-Type: application/json' \
    --data-binary @- "$ORIGIN/api/v1/session" > "$WORK/session.json"
unset PASSWORD
CSRF="$(jq -er '.csrf_token' "$WORK/session.json")"

curl -fsS "${TLS[@]}" -b "$WORK/cookies" \
  -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
  -H 'Content-Type: application/json' --data-binary '{"name":"my-first-node"}' \
  "$ORIGIN/api/v1/nodes" > "$WORK/node.json"
NODE_ID="$(jq -er '.id' "$WORK/node.json")"
jq -n --arg id "$NODE_ID" '{node_id: $id, mode: "single"}' | \
  curl -fsS "${TLS[@]}" -b "$WORK/cookies" \
    -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H 'Content-Type: application/json' --data-binary @- \
    "$ORIGIN/api/v1/enrollments" > "$WORK/ticket.json"
jq -er '.token' "$WORK/ticket.json" > "$WORK/enrollment.token"
```

`Origin` 必须匹配 Server 的 `public_url` 源站。批量签发请求体用 `{"group_id":"分组 UUID","mode":"batch"}`，不能同时传 `node_id`。将 `$WORK/enrollment.token` 安全传给安装步骤；临时目录含 Cookie 和凭证，失败时也要妥善清理。用完后注销会话并删除副本：

```bash
curl -fsS "${TLS[@]}" -b "$WORK/cookies" -X DELETE \
  -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" "$ORIGIN/api/v1/session"
rm -f -- "$WORK/cookies" "$WORK/session.json" "$WORK/node.json" \
  "$WORK/ticket.json" "$WORK/enrollment.token"
rmdir -- "$WORK"
unset CSRF ADMIN
```

## 流量累计

网卡选择与网速一致（显式 `interfaces` 或默认路由网卡），按内核收发计数差累计到 UTC 小时桶。新网卡先建基线；重置后从当前计数继续累计。每次启动使用新 epoch，服务端按累计值确认，已结算桶在后续采样后移除。

状态只在内存中：停机期间不计入、重启不补算，最多保留 24 个未结算小时桶，超出时丢弃最旧桶。无法读取网卡计数器时发送空列表。这不是云厂商账单数据。

## 服务探测

管理员在节点 **Agent configuration** 设置 `probe_targets`，最多 5 个唯一目标，每个不超过 1024 字节、不得含空白。目标会向控制台用户暴露，**不要放秘密**。探测从 Agent 网络发起，可访问其可达的内部服务；应限制管理员权限和 Agent 网络出口。

| 目标 | 行为 |
| --- | --- |
| `tcp://host:port` | 仅测 TCP 建连；端口 1–65535，IPv6 如 `tcp://[::1]:443` |
| `http://host/path` / `https://host/path` | GET 至响应头的耗时；低于 400（包括 3xx）为成功，不跟随重定向，不保存 Cookie，最多丢弃 64 KiB 响应体；HTTPS 校验证书并使用系统信任，不使用 Server policy 的私有 CA |
| `icmp://host` | ICMP Echo 往返耗时；不能带端口、路径或查询；仅 Linux 非特权 datagram socket 支持，其他系统为 `unsupported` |

每 60 秒执行一次，单次超时 5 秒，最多并发 5 个；配置应用后新增目标通常约 1 秒开始检查。报告携带最近完成结果，未完成的省略，删除目标后停止携带；Server 按 `checked_at` 去重。结果不包含响应体或原始错误信息。

HTTP 探测不使用 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`，不能依赖代理做出口限制。Linux ICMP 要求 Agent 的组 ID 位于 `net.ipv4.ping_group_range`；用 `sysctl net.ipv4.ping_group_range` 检查，按主机或容器策略配置允许范围。权限不足报告 `unsupported`，不会提权，也不需要 `CAP_NET_RAW`。

更多字段和错误码见 [OpenAPI](../api/openapi.yaml)。
