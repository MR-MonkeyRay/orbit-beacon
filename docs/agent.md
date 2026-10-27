# Linux Agent 接入指南

本指南使用仓库已有的 CLI 和管理 API，完成一个节点的首次接入。Web 控制台还没有接入凭证创建向导。

## 准备

- 已按 [README](../README.md) 启动 Server，并创建管理员。
- Agent 所在机器为 Linux；已具备与其 OS/架构匹配的 `orbitbeacon-agent` 二进制。
- 操作机器已有 Bash、curl、jq；安装二进制和策略时需要 sudo。以下不安装任何依赖。
- Agent **运行不能使用 root**。下面以当前普通用户作为运行身份；如使用专用账号，需把命令、UID/GID 和文件所有者一起改为该账号，不要仅修改 policy 数字。

下文先给出与 README 配套的**同机体验**步骤。远程接入时，在每台 Agent 上执行安装与注册；把 `https://localhost:8443` 改为从该节点能访问的真实域名，证书必须匹配它。不要使用 `curl -k`，也不要把 Server 私钥复制给 Agent。

## 1. 安装二进制和固定策略

在仓库根目录以普通用户执行。`id -u` 和 `id -g` 必须均不为 `0`。如果希望使用 `1000:1000`，先确认当前用户确实具有该 UID/GID。

```bash
AGENT_UID="$(id -u)"
AGENT_GID="$(id -g)"
sudo install -o root -g root -m 0755 \
  bin/orbitbeacon-agent /usr/local/bin/orbitbeacon-agent
sudo install -d -o root -g root -m 0755 /etc/orbitbeacon
sudo install -d -o "$AGENT_UID" -g "$AGENT_GID" -m 0700 \
  /var/lib/orbitbeacon-agent /var/lib/orbitbeacon-agent/run
sudo install -o root -g root -m 0644 \
  data/server-cert.pem /etc/orbitbeacon/ca.pem
sudo tee /etc/orbitbeacon/install-policy.json > /dev/null <<EOF
{
  "server_url": "https://localhost:8443",
  "ca_file": "/etc/orbitbeacon/ca.pem",
  "state_dir": "/var/lib/orbitbeacon-agent",
  "runtime_dir": "/var/lib/orbitbeacon-agent/run",
  "expected_uid": $AGENT_UID,
  "expected_gid": $AGENT_GID,
  "expected_account": "",
  "install_mode": "foreground",
  "trust_key_file": "/etc/orbitbeacon/update-public.pem"
}
EOF
sudo chown root:root /etc/orbitbeacon/install-policy.json
sudo chmod 0644 /etc/orbitbeacon/install-policy.json
```

这里复制的是 README 本机证书的**公开部分**。使用系统已信任的正式证书时，可以省略复制 CA 的命令，并把 `ca_file` 改为 `""`；使用私有 CA 时应复制签发 CA 的公开 PEM。

策略字段说明：

| 字段 | 要求 |
| --- | --- |
| `server_url` | HTTPS 源站，仅允许空路径或 `/`，不能带账号、查询参数或片段 |
| `ca_file` | 可为空以使用系统信任；否则为绝对路径、root 所有、不可被组/其他用户写入的普通 PEM 文件 |
| `state_dir` | 绝对本地路径，Agent 所有的私有目录；存储 `identity.json` 和 `desired.json` |
| `runtime_dir` | 本地运行目录；示例使用已创建的私有子目录 |
| `expected_uid` / `expected_gid` | 实际运行进程的非 root UID/GID，必须同时匹配 |
| `expected_account` | 可为空；非空时还会校验系统账号名 |
| `install_mode` | `foreground`、`native` 或 `docker`；此处只运行前台进程，不会自动安装服务 |
| `trust_key_file` | 配置校验要求非空本地路径；当前升级命令不可用，运行/接入不会读取该文件，示例不需要创建它 |

policy 文件必须归 root 所有，且从 `/` 开始的每个祖先目录都不能被普通用户写入或经过符号链接。不要把它放在普通用户的仓库/家目录中。CA 文件也有同样的安全祖先要求。state 目录和凭证则属于运行用户，不要通过 `sudo orbitbeacon-agent ...` 运行它们。

普通 Agent 配置文件不是必需的：只传 `--install-policy` 即可。若额外使用 `--config agent.json`，文件可以是 `{}`；任何受保护字段只要与 policy 不同，即使会被其他来源覆盖，也会报 `install_policy_conflict`。同时检查并移除冲突的 `ORBITBEACON_` 环境变量。

## 2. API 登录、创建节点并签发凭证

以下在 **同一个 Bash 会话**执行。使用管理员账号，密码通过标准输入交给 curl，不出现在命令行参数中。临时目录包含会话 Cookie 和短期凭证，应视为敏感信息。

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

正式域名使用系统信任时，把 `ORIGIN` 改为实际地址，`TLS=()`；私有 CA 则保持 `--cacert`，改为正确路径。`Origin` 必须与配置的 `public_url` 对应源站一致，包括非默认端口。

如果节点已在 Web 中创建，可从节点详情页 URL 取得 UUID，设置 `NODE_ID='实际 UUID'`，跳过创建节点的两条命令，继续签发凭证。不要为同一机器反复创建资产。

单节点凭证有效期为 **15 分钟**，原文仅创建时返回；列表和详情接口不能找回原文。分组批量接入使用 `{"group_id":"分组 UUID"}`，有效期 **30 分钟**、最多 **100 次**认领，仍受总节点容量限制。不要同时传 `node_id` 和 `group_id`。

远程节点不要把管理员会话 Cookie 复制过去。在可信管理机签发后，用已验证主机身份的 SSH/SCP 等安全通道传递 token，然后由 Agent 运行用户将其安装为私有目录下的 `0600` 普通文件。token 文件及祖先目录不能经过符号链接，祖先必须由 root 或 Agent 用户所有、不可被组/其他用户写入（安全 sticky 临时目录是例外）。

## 3. 注册、诊断并运行

仍以 policy 指定的普通用户执行：

```bash
orbitbeacon-agent enroll \
  --install-policy /etc/orbitbeacon/install-policy.json \
  --token-file /var/lib/orbitbeacon-agent/enrollment.token
orbitbeacon-agent doctor \
  --install-policy /etc/orbitbeacon/install-policy.json --json
```

注册确认成功后会输出节点 ID，并移除 token 文件。若注册失败，原 token 文件保留；不要先删凭证再重试。若提示“注册成功但清理失败”，先核对结果及文件路径，再安全删除残留文件；清理错误不表示服务端回滚。

`doctor` 检查配置、身份文件、HTTPS 连通性和本机采集，**不会验证节点凭证、注册或上报数据**；`authentication:"not_checked"` 是预期结果，不是注册失败。

注册成功后可注销临时 API 会话并清理管理机上的秘密副本：

```bash
curl -fsS "${TLS[@]}" -b "$WORK/cookies" -X DELETE \
  -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" \
  "$ORIGIN/api/v1/session"
rm -- "$WORK/cookies" "$WORK/session.json" "$WORK/node.json" \
  "$WORK/ticket.json" "$WORK/enrollment.token"
rmdir -- "$WORK"
unset CSRF ADMIN
```

如果跳过了节点创建，没有 `node.json`，清理时也应从列表中删去该文件名。随后以前台进程启动：

```bash
orbitbeacon-agent run --install-policy /etc/orbitbeacon/install-policy.json
```

使用 Ctrl+C 或 SIGTERM 停止。仓库目前没有 Agent 安装器或 Agent systemd unit；长期运行时应由你自己的服务管理器使用同一非 root 身份、同一 policy 和持久化 state 目录启动，不要把 `install_mode` 当作安装命令。

## 4. 如何判断结果

- `enroll` 成功证明服务端已接受该安装身份，并不证明随后有指标成功上报。
- 注册后 Web 节点状态当前为 `unknown`，最新指标、在线判定和配置应用确认都还没有管理端展示接口。不能以“页面没有图表”推断注册失败，也不能以“进程在运行”证明服务端已收到数据。
- 在节点详情页修改上报间隔等期望配置后，Agent 在正常报告交互中保存到 state 下的 `desired.json`；可以检查该文件的 revision 和配置值确认本地持久化，但这不等同于服务端已收到下一次应用确认。
- 不要手工改写或共享 `identity.json`，它包含长期节点凭证。克隆机器必须使用独立 state 并分别接入，否则会发生进程所有权冲突。
- `enroll` 不带 `--token-file` 仅恢复已有节点凭证，不会创建新的接入授权；丢失可用凭证时需要管理员重新签发 ticket。
- `409 identity_conflict` 表示该进程已被更新的运行身份取代，会停止采集上报。其他网络错误会退避重试；断网期间不积压历史样本。
- `update --version ...` 当前明确报不可用。此指南不提供远程命令执行或网页升级能力。

更多请求字段及错误码见 [OpenAPI](../api/openapi.yaml)。
