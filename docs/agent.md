# Linux Agent 接入

本文用现有的 CLI 和管理 API 完成一台 Linux 节点的首次接入。Web 控制台还没有创建接入凭证的向导，所以接入流程由命令完成。

## 准备

- 已按[主 README](../README.md) 启动 Server，并创建管理员。
- Agent 机器为 Linux，且已有匹配 OS/架构的 `orbitbeacon-agent` 二进制。
- 操作机有 Bash、curl、jq。安装二进制和策略需要 sudo，运行 Agent 不需要。
- Agent **不能以 root 运行**。下面以当前普通用户作为运行身份；如果改用专用账号，命令、UID/GID 和文件所有者都要一起改，不能只改 policy 里的数字。

下面的步骤先按同机体验来写。远程节点只需把 `https://localhost:8443` 换成从该节点能访问的真实域名，并确保证书匹配；安装和注册要在每台 Agent 上执行。不要把 Server 私钥复制给 Agent，也不要用 `curl -k` 跳过证书校验。

## 一条命令安装（专用系统账号）

可用 [安装脚本](../deploy/install-agent.sh) 代替下面的手动安装和 systemd 配置。先按第 2 节在可信管理机签发 token，再安全传到 Agent 机器。脚本需要 root、Linux amd64/arm64、`awk`、`curl`、`tar`、`sha256sum`、`install` 和 `runuser`（缺失时用 `su`）；离线安装不需要 curl。账号创建支持 `useradd` 或 BusyBox 的 `adduser`/`addgroup`。

从**包含此脚本的固定发布 tag**下载到本地并检查，再运行；不要直接把网络脚本管道送进 root shell。以下版本仅为示例：

```sh
VERSION=0.1.0
curl --fail --silent --show-error --location --proto '=https' \
  --proto-redir '=https' --tlsv1.2 \
  -o install-agent.sh \
  "https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v$VERSION/deploy/install-agent.sh"
less install-agent.sh
sudo -v  # 先完成 sudo 认证，避免密码提示与 token 的标准输入冲突
# TOKEN 已从安全来源读入当前 shell；不要启用 set -x，也不要写进命令参数。
printf '%s\n' "$TOKEN" | sudo sh ./install-agent.sh \
  --server https://monitor.example.com --version "$VERSION"
unset TOKEN
```

也可用 `--token-file /安全路径/enrollment.token`，或在终端直接运行后按隐藏提示输入。私有 CA 加 `--ca-file /安全路径/ca.pem`；省略时使用系统信任。`--server` 只接受 HTTPS 源站（可带端口，不可带子路径、账号或查询参数）。脚本通过 HTTPS 下载 release 包和 `SHA256SUMS`，校验后只取当前架构的 Agent；校验和用于检查下载完整性，不是独立发布签名。离线主机可用 `--binary /安全路径/orbitbeacon-agent-linux-amd64` 替代下载，此时 `--version` 可省略；本地二进制的来源和架构需自行核对。

安装后，二进制位于 `/usr/local/bin/orbitbeacon-agent`；root 所有的 policy 和可选 CA 位于 `/etc/orbitbeacon/`，`install_mode` 为 `native`。状态目录 `/var/lib/orbitbeacon-agent` 权限为 `0700`，归专用非 root 账号 `orbitbeacon-agent` 所有；注册凭证以 `0600` 保存并由该账号执行 `enroll`。失败时保留 `enrollment.token`，不要先删再重试；检查 URL、CA、网络和有效期后，可按脚本提示以该账号重试（详细诊断见第 3 节）。注册子进程的输出不展示，以免意外泄露 token。成功后启用并启动 systemd 服务；没有运行中的 systemd 时，脚本打印以前台方式运行的命令。

重复运行可升级二进制并重启服务（须先升级 Server：旧版 Server 会忽略流量字段且不返回确认，Agent 最多持续重发 24 个小时桶）；已有 `identity.json` 且未提供 token 时跳过注册，保留身份和状态。升级时仍需传相同 `--server` 和所需 `--ca-file`，并显式关闭标准输入，避免等待输入：

```sh
sudo sh ./install-agent.sh --server https://monitor.example.com \
  --version "$VERSION" < /dev/null
```

卸载前先在控制台撤销节点凭证，再执行 `sudo sh ./install-agent.sh --uninstall` 并输入 `yes`；自动化可加 `--yes`。这会停止并禁用服务，删除 unit、二进制、policy、CA 副本和**整个状态目录**，保留系统账号；本地删除不会撤销服务端凭证。没有 systemd 时须先停止前台进程。

## 1. 安装二进制和固定策略

在仓库根目录以普通用户执行。`id -u` 和 `id -g` 都必须不为 0。

```bash
AGENT_UID="$(id -u)"
AGENT_GID="$(id -g)"
DEMO_DIR="$HOME/orbitbeacon-demo"
sudo install -o root -g root -m 0755 \
  build/orbitbeacon-agent /usr/local/bin/orbitbeacon-agent
sudo install -d -o root -g root -m 0755 /etc/orbitbeacon
sudo install -d -o "$AGENT_UID" -g "$AGENT_GID" -m 0700 \
  /var/lib/orbitbeacon-agent
sudo install -o root -g root -m 0644 \
  "$DEMO_DIR/server-cert.pem" /etc/orbitbeacon/ca.pem
sudo tee /etc/orbitbeacon/install-policy.json > /dev/null <<EOF
{
  "server_url": "https://localhost:8443",
  "ca_file": "/etc/orbitbeacon/ca.pem",
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

这里复制的是 README 本机证书的公开部分。使用系统已信任的正式证书时，可以省略复制 CA 的命令，并把 `ca_file` 改为 `""`；使用私有 CA 时，复制签发 CA 的公开 PEM。

| 字段 | 要求 |
| --- | --- |
| `server_url` | HTTPS 源站，只允许空路径或 `/`，不能带账号、查询参数或片段 |
| `ca_file` | 空字符串表示使用系统信任；否则为绝对路径、root 所有的 PEM 文件，祖先目录也必须 root 所有且不可被组/其他用户写入 |
| `state_dir` | Agent 用户所有的私有绝对目录，存放 `identity.json` 和 `desired.json` |
| `expected_uid` / `expected_gid` | 实际运行进程的非 root UID/GID，必须同时匹配 |
| `expected_account` | 可为空；非空时还会核对系统账号名 |
| `install_mode` | `foreground`、`native` 或 `docker`；这里只运行前台进程，不会安装服务 |

policy 文件和从 `/` 开始的每个祖先目录都必须归 root 所有，组/其他用户不可写（root 所有的 sticky 临时目录是例外），并且不能经过符号链接。不要把 policy 放进普通用户的仓库或家目录。CA 文件也必须归 root 所有，并放在 root 所有的安全目录中；`/tmp` 这类组/其他用户可写的位置不行。state 目录和凭证属于运行用户，不要用 `sudo orbitbeacon-agent ...` 运行它们。

普通 Agent 配置文件不是必需的，只传 `--install-policy` 即可。如果额外使用 `--config agent.json`，文件可以是 `{}`；任何受保护字段只要与 policy 不一致，即使会被其他来源覆盖，也会报 `install_policy_conflict`。同时检查并移除冲突的 `ORBITBEACON_` 环境变量。

## 2. 登录 API、创建节点并签发凭证

以下命令在**同一个 Bash 会话**执行。管理员密码通过标准输入交给 curl，不进入命令行参数。临时目录里会有会话 Cookie 和短期凭证，应视为敏感信息。

```bash
set -euo pipefail
umask 077
ORIGIN='https://localhost:8443'
TLS=(--cacert /etc/orbitbeacon/ca.pem)
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
  -H 'Content-Type: application/json' \
  --data-binary '{"name":"my-first-node"}' \
  "$ORIGIN/api/v1/nodes" > "$WORK/node.json"
NODE_ID="$(jq -er '.id' "$WORK/node.json")"

jq -n --arg node_id "$NODE_ID" '{node_id: $node_id}' | \
  curl -fsS "${TLS[@]}" -b "$WORK/cookies" \
    -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
    -H 'Content-Type: application/json' \
    --data-binary @- "$ORIGIN/api/v1/enrollments" > "$WORK/ticket.json"
jq -er '.token' "$WORK/ticket.json" > "$WORK/enrollment.token"
install -m 0600 "$WORK/enrollment.token" \
  /var/lib/orbitbeacon-agent/enrollment.token
printf '待接入节点 ID: %s\n' "$NODE_ID"
```

正式域名使用系统信任时，把 `ORIGIN` 改成实际地址，`TLS=()`；使用私有 CA 时保留 `--cacert` 并改成正确路径。`Origin` 必须与配置的 `public_url` 源站一致，包括非默认端口。

如果节点已经在 Web 中创建，可以从节点详情页 URL 取得 UUID，设置 `NODE_ID='实际 UUID'`，跳过创建节点的两条命令，再签发凭证。不要为同一台机器反复创建资产。

单节点凭证有效期为 **15 分钟**，原文只在创建时返回，列表和详情接口都不能找回。分组批量接入时，把请求体换成 `{"group_id":"分组 UUID"}`；凭证有效期 **30 分钟**、最多 **100 次**认领，仍受总节点容量限制。`node_id` 和 `group_id` 不能同时传。

远程节点不要把管理员会话 Cookie 复制过去。在可信管理机签发后，用已验证主机身份的 SSH/SCP 等安全通道传递 token，再由 Agent 运行用户把它安装成私有目录下的 `0600` 普通文件。token 文件和祖先目录不能经过符号链接；祖先必须由 root 或 Agent 用户所有，且组/其他用户不可写（安全 sticky 临时目录是例外）。

## 3. 注册、诊断并运行

仍以 policy 指定的普通用户执行：

```bash
orbitbeacon-agent enroll \
  --install-policy /etc/orbitbeacon/install-policy.json \
  --token-file /var/lib/orbitbeacon-agent/enrollment.token
orbitbeacon-agent doctor \
  --install-policy /etc/orbitbeacon/install-policy.json --json
```

注册成功会输出节点 ID 并移除 token 文件；注册失败时 token 会保留，不要先删凭证再重试。若提示“注册成功但清理失败”，先核对结果和文件路径，再安全删除残留文件；清理错误不代表服务端回滚。

`doctor` 检查配置、身份文件、HTTPS 连通性和本机采集，**不会验证节点凭证、注册或上报**；`authentication: "not_checked"` 是预期值，不是注册失败。

注册成功后可以注销临时 API 会话，并清理管理机上的秘密副本：

```bash
curl -fsS "${TLS[@]}" -b "$WORK/cookies" -X DELETE \
  -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
  "$ORIGIN/api/v1/session"
rm -f -- "$WORK/cookies" "$WORK/session.json" "$WORK/node.json" \
  "$WORK/ticket.json" "$WORK/enrollment.token"
rmdir -- "$WORK"
unset CSRF ADMIN
```

随后以前台进程启动：

```bash
orbitbeacon-agent run --install-policy /etc/orbitbeacon/install-policy.json
```

用 Ctrl+C 或 SIGTERM 停止。`run` 会在整个运行期间独占 state 目录下的 `run.lock`（`0600`）；同一目录的第二个进程会立即报错，不会递增运行代次。不要删除或替换正在使用的锁文件；退出后保留的空锁文件不代表进程还在运行。state 目录必须由运行用户所有、权限为 `0700`，锁文件及目录路径不能经过符号链接。

### 流量累计

Agent 使用与网速指标相同的网卡选择（显式 `interfaces` 或默认路由网卡），对内核收发字节计数取差，按采样时刻归入 UTC 小时桶。新选中的网卡先建立基线；计数器重置后从当前值继续累计。每次进程启动生成独立随机 epoch，按 epoch 和小时发送累计值，收到服务端对相同值的确认且小时结束后已有新采样，才移除该桶。

流量状态仅保存在内存：Agent 停机期间的字节不会计入，重启不补算，也不会重复累计上一进程的字节。请先升级 Server 再升级 Agent；旧版 Server 忽略流量字段且不返回确认，Agent 会继续重发，最多保留最近 24 个未结算小时桶，超过时丢弃最旧的桶。平台无法读取网卡计数器时，流量列表为空。

### systemd 长期运行

仓库提供 [Agent unit](../deploy/systemd/orbitbeacon-agent.service)。切换前先停止前台进程；创建专用非 root 系统用户，将 state 目录及已有凭证交给该用户，并把 policy 的 `expected_uid`、`expected_gid` 改成实际 UID/GID，`install_mode` 改成 `native`：

```bash
sudo useradd --system --user-group --home-dir /var/lib/orbitbeacon-agent \
  --no-create-home --shell /usr/sbin/nologin orbitbeacon-agent
sudo install -d -o orbitbeacon-agent -g orbitbeacon-agent -m 0700 \
  /var/lib/orbitbeacon-agent
sudo chown -R --no-dereference orbitbeacon-agent:orbitbeacon-agent \
  /var/lib/orbitbeacon-agent
POLICY_TMP="$(mktemp)"
jq --argjson uid "$(id -u orbitbeacon-agent)" \
  --argjson gid "$(id -g orbitbeacon-agent)" \
  '.expected_uid = $uid | .expected_gid = $gid | .expected_account = "orbitbeacon-agent" | .install_mode = "native"' \
  /etc/orbitbeacon/install-policy.json > "$POLICY_TMP"
sudo install -o root -g root -m 0644 "$POLICY_TMP" \
  /etc/orbitbeacon/install-policy.json
rm -f -- "$POLICY_TMP"
```

尚未注册时，用 `sudo -u orbitbeacon-agent orbitbeacon-agent enroll ...` 执行本节注册命令；`doctor` 也必须以该用户运行。在仓库根目录安装并启用 unit：

```bash
sudo install -o root -g root -m 0644 \
  deploy/systemd/orbitbeacon-agent.service /etc/systemd/system/orbitbeacon-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-agent.service
```

unit 的 `User` / `Group` 必须与 policy 对应，`StateDirectory` 固定使用 `/var/lib/orbitbeacon-agent`。`install_mode` 只是运行模式声明，不会安装服务。可用 `journalctl -u orbitbeacon-agent.service` 查看日志。

## 4. 如何判断结果

- `enroll` 成功只证明服务端接受了该安装身份，不证明后续指标上报成功。
- 注册后管理节点状态仍显示 `unknown`，配置应用确认没有管理端展示接口。显式公开的节点可在首页查看最近180秒上报状态及四项基础资源；未公开节点不会出现。不要用“页面没有图表”推断注册失败，也不要用“进程在运行”证明服务端已收到数据。
- 在节点详情页保存上报间隔等期望配置后，Agent 会在正常上报交互中把它保存到 state 下的 `desired.json`。可以检查该文件的 revision 和配置值确认本地持久化，但这不等于服务端已收到应用确认。
- 不要手工改写或共享 `identity.json`，它包含长期节点凭证。克隆机器必须使用独立 state 并分别接入，否则会发生进程所有权冲突。
- `enroll` 不带 `--token-file` 只会恢复已有节点凭证，不会创建新的接入授权；凭证丢失时需要管理员重新签发 ticket。
- `409 identity_conflict` 表示该进程已被更新的运行身份取代，会停止采集上报并以退出码 **3** 退出（其他失败为 **1**，正常退出为 **0**）。unit 通过 `RestartPreventExitStatus=3` 阻止自动重启争抢身份；应为克隆机器准备独立 state 并重新接入。其他网络或 HTTP 错误会退避重试；首次失败、每隔至少 5 分钟的连续失败摘要和恢复会写入日志，断网期间不积压历史样本。
- `update --version ...` 当前明确报不可用。本指南不提供远程命令执行或网页升级能力。

### 卸载

先在控制台删除节点或撤销其节点凭证（撤销 API 为 `DELETE /api/v1/nodes/{id}/credential`），使已保存或复制的凭证失效。然后停止并禁用服务，再删除 state 目录、二进制和 policy；state 中的 `identity.json` 包含长期节点凭证，不能只删服务文件。

```bash
sudo systemctl disable --now orbitbeacon-agent.service
sudo rm -rf -- /var/lib/orbitbeacon-agent
sudo rm -f -- /usr/local/bin/orbitbeacon-agent /etc/orbitbeacon/install-policy.json \
  /etc/systemd/system/orbitbeacon-agent.service
sudo systemctl daemon-reload
```

若使用前台进程或自定义路径，先停止对应进程，再删除实际路径。删除本地文件不会撤销服务端凭证。

更多请求字段和错误码见 [OpenAPI](../api/openapi.yaml)。


## 服务探测

先升级 Server，再升级 Agent 并配置 `probe_targets`。旧版 Server 会拒绝非空的 `probe_results`，也不能配置探测目标；Agent 仅在配置了目标时发送对应结果，未配置时省略该字段。降级 Server 前应先清空目标配置，并确认 Agent 已应用。

每台 Agent 最多配置 5 个唯一目标，每个目标最多 1024 字节，不能含空白。探测从 Agent 所在网络发起，每 60 秒执行一次，单次超时 5 秒，最多同时执行 5 个检查。目标配置变更后约 1 秒开始检查新增目标，不等待下一次指标采集或报告。每次报告携带当前目标最近一次完成的结果；尚未完成的目标暂时省略，移除目标立即从后续报告中去除。同一结果可能出现在多次报告中，Server 按 `checked_at` 去重。

- `tcp://host:port`：测量 TCP 建立连接的耗时，不检查应用协议。端口范围为 1–65535，IPv6 地址须使用方括号，例如 `tcp://[::1]:443`。
- `http://host/path`、`https://host/path`：发送 GET，测量收到响应头的耗时；状态码低于 400 为成功（包括 3xx），不跟随重定向。最多读取并丢弃 64 KiB 响应体，不保存 cookie；HTTPS 使用系统证书信任并验证证书。目标不能包含账号信息或 URL 片段。请求的 User-Agent 为 `OrbitBeacon-Probe/<版本>`。
- `icmp://host`：测量 ICMP Echo 往返耗时，不能指定端口、路径或查询参数；同时存在 IPv4 和 IPv6 时优先使用 IPv4。仅 Linux 支持，使用非特权 datagram ICMP socket，不需要 root 或 `CAP_NET_RAW`。其他系统报告 `unsupported`。

HTTP/HTTPS 探测直接访问目标，**绕过 HTTP 代理**，不会使用 `HTTP_PROXY`、`HTTPS_PROXY` 或 `ALL_PROXY`；Agent 向 Server 发送报告的连接策略与此独立。不要依赖代理完成探测目标的访问控制。

Linux ICMP 要求 Agent 的组 ID 位于 `net.ipv4.ping_group_range` 允许范围内。多数发行版由 systemd 设置该值；容器中可能需要检查 `sysctl net.ipv4.ping_group_range` 并为 Agent 的组配置允许范围（例如通过容器的 sysctl 配置）。权限不足（EACCES/EPERM）会报告 `unsupported`，不会提升权限。探测结果仅包含固定错误类别，不包含原始错误信息或响应体。
