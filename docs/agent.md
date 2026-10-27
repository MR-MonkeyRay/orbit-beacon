# Linux Agent 接入

本文用现有的 CLI 和管理 API 完成一台 Linux 节点的首次接入。Web 控制台还没有创建接入凭证的向导，所以接入流程由命令完成。

## 准备

- 已按[主 README](../README.md) 启动 Server，并创建管理员。
- Agent 机器为 Linux，且已有匹配 OS/架构的 `orbitbeacon-agent` 二进制。
- 操作机有 Bash、curl、jq。安装二进制和策略需要 sudo，运行 Agent 不需要。
- Agent **不能以 root 运行**。下面以当前普通用户作为运行身份；如果改用专用账号，命令、UID/GID 和文件所有者都要一起改，不能只改 policy 里的数字。

下面的步骤先按同机体验来写。远程节点只需把 `https://localhost:8443` 换成从该节点能访问的真实域名，并确保证书匹配；安装和注册要在每台 Agent 上执行。不要把 Server 私钥复制给 Agent，也不要用 `curl -k` 跳过证书校验。

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
  "install_mode": "foreground",
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

用 Ctrl+C 或 SIGTERM 停止。仓库目前没有 Agent 安装器或 Agent systemd unit；需要长期运行时，用自己的服务管理器以同一非 root 身份、同一 policy 和持久化 state 目录启动。`install_mode` 不是安装命令。

## 4. 如何判断结果

- `enroll` 成功只证明服务端接受了该安装身份，不证明后续指标上报成功。
- 注册后管理节点状态仍显示 `unknown`，配置应用确认没有管理端展示接口。显式公开的节点可在首页查看最近180秒上报状态及四项基础资源；未公开节点不会出现。不要用“页面没有图表”推断注册失败，也不要用“进程在运行”证明服务端已收到数据。
- 在节点详情页保存上报间隔等期望配置后，Agent 会在正常上报交互中把它保存到 state 下的 `desired.json`。可以检查该文件的 revision 和配置值确认本地持久化，但这不等于服务端已收到应用确认。
- 不要手工改写或共享 `identity.json`，它包含长期节点凭证。克隆机器必须使用独立 state 并分别接入，否则会发生进程所有权冲突。
- `enroll` 不带 `--token-file` 只会恢复已有节点凭证，不会创建新的接入授权；凭证丢失时需要管理员重新签发 ticket。
- `409 identity_conflict` 表示该进程已被更新的运行身份取代，会停止采集上报。其他网络错误会退避重试；断网期间不积压历史样本。
- `update --version ...` 当前明确报不可用。本指南不提供远程命令执行或网页升级能力。

更多请求字段和错误码见 [OpenAPI](../api/openapi.yaml)。
