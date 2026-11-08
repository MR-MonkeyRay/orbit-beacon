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

终端运行时隐藏输入注册票据；先复制并运行安装命令，等提示出现后再粘贴票据。也可使用 `--token-file /安全路径/enrollment.token`；自动化通过标准输入传入，不启用 `set -x`，并提前完成 sudo 认证：

```sh
sudo -v
# TOKEN 已从安全来源读入当前 shell；printf 使用 shell 内建命令。
printf '%s\n' "$TOKEN" | sudo sh ./install-agent.sh \
  --server https://beacon.your-domain.com --version "$VERSION"
unset TOKEN
```

- `--server` 与 CA 要求见[配置参考](../deploy/README.md#配置参考)和 [HTTPS](../deploy/README.md#https-与反向代理)；私有 CA 加 `--ca-file /安全路径/ca.pem`。
- 脚本自动选择 `orbitbeacon-agent-linux-amd64` 或 `orbitbeacon-agent-linux-arm64`，只下载 Agent 二进制和 `SHA256SUMS`，校验后安装并赋予执行权限，无需 `tar`。新脚本需配套提供独立二进制附件的版本，不兼容旧压缩包发布；旧版本可手动解压后通过 `--binary` 安装。脚本与二进制下载仅用 HTTPS；`SHA256SUMS` 校验不是独立签名验证。
- 离线安装用 `--binary /安全路径/orbitbeacon-agent-linux-amd64`，可省略版本，自行核对来源与架构。

脚本创建专用账号 `orbitbeacon-agent`，固定实际 UID/GID，不保证是 `1000:1000`。安装路径：

| 路径 | 内容与权限 |
| --- | --- |
| `/opt/orbit-beacon/` | root:orbitbeacon-agent，`0750` |
| `/opt/orbit-beacon/bin/` | orbitbeacon-agent:orbitbeacon-agent，`0750`；内含可替换的 `orbitbeacon-agent`，`0750` |
| `/opt/orbit-beacon/etc/` | root:orbitbeacon-agent，`0750`；文件 `0640`，Agent 只读 |
| `/opt/orbit-beacon/libexec/orbitbeacon-agent-rollback` | root:root，`0755`；以 Agent 用户运行的启动回滚脚本 |
| `/opt/orbit-beacon/etc/install-policy.json` | root:orbitbeacon-agent，`0640`；安装策略，模式 `native` |
| `/opt/orbit-beacon/etc/agent.json` | 本地运行参数；新装为 `{}`，root:orbitbeacon-agent、`0640`；重装保留，拒绝符号链接 |
| `/opt/orbit-beacon/etc/ca.pem` | root:orbitbeacon-agent，`0640`；可选 CA 副本 |
| `/opt/orbit-beacon/state/` | orbitbeacon-agent:orbitbeacon-agent，`0700`；identity、enrollment.token、run.lock 等身份和状态不应共享 |
| `/etc/systemd/system/orbitbeacon-agent.service` | 非 root systemd 服务 |

不要用 sudo 运行 orbitbeacon-agent。

脚本以 Agent 账号注册，成功后启动服务；没有运行中的 systemd 时打印前台启动命令。失败保留 state 中的 `enrollment.token`，不启用服务。修复 URL、CA、网络或票据问题后重试：

```sh
sudo -u orbitbeacon-agent /opt/orbit-beacon/bin/orbitbeacon-agent enroll \
  --install-policy /opt/orbit-beacon/etc/install-policy.json \
  --config /opt/orbit-beacon/etc/agent.json \
  --token-file /opt/orbit-beacon/state/enrollment.token
```

重试成功后，以原来的 `--server`、版本和 CA 参数重跑安装脚本，并加 `< /dev/null`，让脚本补装并启动服务，不再提供注册票据。

需要更换票据时，以策略指定的非 root Agent 用户运行下方[手动前台安装](#手动前台安装)中的隐藏读取注册命令。不要把新票据填进命令或 `printf` 字面量。

成功后删除 state 中的注册票据文件，但源文件和管理机副本须自行清理。若提示清理失败，先核对注册结果，不能假定认领已回滚。

## 运行检查

```sh
sudo systemctl status orbitbeacon-agent
sudo journalctl -u orbitbeacon-agent -n 100 --no-pager
sudo -u orbitbeacon-agent /opt/orbit-beacon/bin/orbitbeacon-agent doctor \
  --install-policy /opt/orbit-beacon/etc/install-policy.json --config /opt/orbit-beacon/etc/agent.json --json
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

以下参数在 `run`、`enroll`、`doctor` 中一致，合并顺序见[配置参考](../deploy/README.md#配置参考)。安装策略固定信任根、身份与目录；每个来源独立检查，后续覆盖不能隐藏冲突。

| 参数 | 默认值 | 范围 / 选择 |
| --- | --- | --- |
| `report_interval_seconds` | `10` | 5–60 秒，采集与正常上报共用周期 |
| `disk_interval_seconds` | `60` | 60–3600 秒 |
| `interfaces` | `[]` | 最多 8 项；空数组使用默认路由网卡 |
| `disk_paths` | `[]` | 最多 8 个绝对路径；空数组使用默认根路径 |
| `probe_targets` | `[]` | 最多 5 项；空数组不探测 |
| `probe_expected_status` | `{}` | HTTP(S) 目标到预期状态码（100–599 整数）的本地映射 |
| `remote_commands` | `["upgrade","restart","stop"]` | 允许的远程指令；native 仅在安装策略配置了升级地址时支持 upgrade，docker 仅 restart，foreground 仅 stop，取交集生效，`[]` 全部关闭 |
| `disabled_features` | `[]` | 关闭 disk、network、traffic、probes、disk_io、pressure、temperature 中的指定功能，默认全部启用；disk 同时关闭 disk_io，network 同时关闭 traffic，probes 清空探测目标 |

选择还受 JSON 转义后 64 KiB 报告预算约束。

环境变量为 `ORBITBEACON_REPORT_INTERVAL_SECONDS` 等大写参数名；`remote_commands` 和 `disabled_features` 不提供 flag，其余 flag 使用 `--report-interval-seconds` 等连字符形式。列表必须是 JSON 数组字符串，例如 `ORBITBEACON_INTERFACES='["eth0"]'` 或 `--probe-targets '["https://example.com/a,b"]'`，不按逗号拆分。`probe_expected_status` 使用 JSON 对象，例如 `--probe-expected-status '{"https://example.com/health":204}'`。

修改后执行 `sudo systemctl reload orbitbeacon-agent`（前台进程用 `kill -HUP <PID>`）。reload 重读原 JSON，沿用启动时环境和显式 flags，不重读 policy；有效变更在采样边界完整应用并重采，不重置身份或累计流量，坏配置保留旧值。检查日志 `configuration_reloaded` / `configuration_unchanged` / `configuration_reload_rejected` 和下一报告；systemctl 成功只代表信号送达。

## 远程升级

控制台节点的「升级」或分组的「升级全部」只指定版本号；分组需输入组名，已运行目标版本、已关闭或本地不允许的节点跳过。当前版本必须也是正式三段版本，目标必须严格更新；开发版、相同版本和降级返回 `rejected/downgrade`。

安装器将 `update_base_url` 写入 root 所有的安装策略，默认 `https://github.com/mr-monkeyray/orbit-beacon/releases/download`，可用 `--update-base-url URL` 指定 HTTPS 镜像，重装时保留已有地址；空值关闭升级能力。该值不能通过 `agent.json`、reload 或服务器修改，也不随报告上传。docker/foreground 不支持远程升级。

下载和自检在后台执行，期间继续上报；仅支持 Linux amd64/arm64。下载使用系统证书及策略中的 `ca_file`，仅允许 HTTPS（包括最多五次重定向）。二进制上限 64 MiB，清单上限 64 KiB。候选版本经 SHA256 校验和 `version --json` 自检后，保留 `bin/orbitbeacon-agent.prev`，写入 `state/upgrade-pending`，再原子替换并以 75 退出。下载最多十分钟，并为重启与首次上报预留 90 秒；不足 90 秒拒绝执行。

新版本首次被服务器接受的上报才算升级成功，并删除待确认标记。新版本崩溃，或运行 2 分钟仍未上报成功而自行退出，都计一次启动；第四次启动时，root 所有的回滚脚本以 Agent 用户运行并恢复 `.prev`，结果为 `rolled_back`。升级期间服务器长时间不可用同样会触发回滚。`.prev` 在成功升级后保留供人工恢复；意外中断返回 `interrupted`。回滚脚本绝不执行 `bin/` 中的文件，`etc/` 始终归 root 所有且 Agent 只读。

`SHA256SUMS` 与二进制来自同一发布源，只防传输损坏，不能防发布源被入侵；没有签名验证。不接受此信任边界的节点应从本地 `remote_commands` 移除 `upgrade`。管理员绝不能运行 `sudo orbitbeacon-agent`；手工恢复也须先停止服务，再以 Agent 用户检查二进制。

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
sudo install -d -o root -g "$AGENT_GID" -m 0750 /opt/orbit-beacon /opt/orbit-beacon/etc
sudo install -d -o "$AGENT_UID" -g "$AGENT_GID" -m 0750 /opt/orbit-beacon/bin
sudo install -d -o "$AGENT_UID" -g "$AGENT_GID" -m 0700 /opt/orbit-beacon/state
# 安装根不可由 Agent 写入；不在 bin 内写临时文件，也不跟随目标链接。
AGENT_TMP=$(sudo mktemp /opt/orbit-beacon/.agent.XXXXXXXX)
sudo install -o "$AGENT_UID" -g "$AGENT_GID" -m 0750 build/orbitbeacon-agent "$AGENT_TMP"
sudo mv -fT -- "$AGENT_TMP" /opt/orbit-beacon/bin/orbitbeacon-agent
sudo tee /opt/orbit-beacon/etc/install-policy.json > /dev/null <<EOF
{
  "server_url": "https://beacon.your-domain.com",
  "update_base_url": "",
  "ca_file": "",
  "state_dir": "/opt/orbit-beacon/state",
  "expected_uid": $AGENT_UID,
  "expected_gid": $AGENT_GID,
  "expected_account": "",
  "install_mode": "foreground"
}
EOF
sudo chown "root:$AGENT_GID" /opt/orbit-beacon/etc/install-policy.json
sudo chmod 0640 /opt/orbit-beacon/etc/install-policy.json
printf '%s\n' '{}' | sudo tee /opt/orbit-beacon/etc/agent.json > /dev/null
sudo chown "root:$AGENT_GID" /opt/orbit-beacon/etc/agent.json
sudo chmod 0640 /opt/orbit-beacon/etc/agent.json
```

CA 使用限制见[配置参考](../deploy/README.md#配置参考)。私有 CA 由 root 安装为 `/opt/orbit-beacon/etc/ca.pem`（root:Agent 组、`0640`），并填写 policy 的 `ca_file`。policy、CA 及祖先目录须 root 所有且不可被组/其他用户写入，无符号链接；不要放在家目录或 `/tmp`。state 按[安装路径](#安装与启动)的要求归运行用户所有。

通过安全通道接收注册票据。以下命令隐藏读取票据并保存为当前用户所有的 `0600` 普通文件；路径无符号链接，祖先仅 root 或运行用户所有且不可被组/其他用户写入（安全 sticky 临时目录例外）。以普通用户一次性执行整个括号块，等待隐藏读取后再粘贴票据并回车，不要逐行执行或启用追踪。子 shell 在退出或收到 HUP、INT、TERM 时恢复回显，票据不进入命令参数、shell 历史或导出的环境变量；写文件使用 shell 内建 `printf`：

```sh
(
  set +x
  set -e
  unset token
  trap 'stty echo' 0
  trap 'exit 1' HUP INT TERM
  install -m 0600 /dev/null /opt/orbit-beacon/state/enrollment.token
  stty -echo
  IFS= read -r token
  stty echo
  printf '%s\n' "$token" > /opt/orbit-beacon/state/enrollment.token
  unset token
  /opt/orbit-beacon/bin/orbitbeacon-agent enroll --install-policy /opt/orbit-beacon/etc/install-policy.json --config /opt/orbit-beacon/etc/agent.json --token-file /opt/orbit-beacon/state/enrollment.token
)
```

注册成功后执行：

```sh
/opt/orbit-beacon/bin/orbitbeacon-agent doctor --install-policy /opt/orbit-beacon/etc/install-policy.json --config /opt/orbit-beacon/etc/agent.json --json
/opt/orbit-beacon/bin/orbitbeacon-agent run --install-policy /opt/orbit-beacon/etc/install-policy.json --config /opt/orbit-beacon/etc/agent.json
```

Ctrl+C 或 SIGTERM 停止。额外的 Agent JSON 配置非必需。若需要 systemd 长期运行，优先使用安装脚本；自行配置 [unit](../deploy/systemd/orbitbeacon-agent.service) 时必须同步账号、state 所有权、policy 的 UID/GID 和 `install_mode: "native"`，不能只复制 unit。`install_mode` 本身不会安装服务。

## API 签发

- 在可信管理机登录，取得会话与 CSRF，按[接口契约（OpenAPI）](../api/openapi.yaml) 调用 `POST /api/v1/enrollments`。
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
| `dns://host` | DNS 地址解析耗时；至少一个地址为成功，仅接受主机，不含端口、路径、查询、凭据或片段 |
| `icmp://host` | ICMP Echo 往返耗时；不能带端口、路径或查询；仅 Linux 非特权 datagram socket 支持，其他系统为 `unsupported` |

`probe_expected_status` 是仅保存在本地的 JSON 对象，例如 `{"https://example.com/health":204}`：键须在 `probe_targets` 中且为 HTTP(S)，值须为 100–599 的整数。设置后仅精确匹配成功（含低于 400 的不匹配响应），未设置仍以低于 400 为成功；reload 生效，不随报告发送。

HTTPS 收到响应时携带叶证书到期时间 `tls_expires_at`，状态码不匹配时仍携带；握手失败（含过期证书）为 `tls` 且无到期时间。

每 60 秒执行一次，单次超时 5 秒，最多并发 5 个；reload 后新增目标立即开始检查。报告携带该次采样目标集合中最近完成的结果，未完成的省略，删除目标后停止携带；结果去重见[状态与历史](architecture.md#状态与历史)。结果不包含响应体、原始错误信息或证书主体。

HTTP 探测不使用 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`，不能依赖代理做出口限制。Linux ICMP 要求 Agent 的组 ID 位于 `net.ipv4.ping_group_range`；用 `sysctl net.ipv4.ping_group_range` 检查，按主机或容器策略配置允许范围。权限不足报告 `unsupported`，不会提权，也不需要 `CAP_NET_RAW`。

字段与错误码见[接口契约（OpenAPI）](../api/openapi.yaml)。

远程指令仅有 `upgrade`、`restart`、`stop`，最终由本机 `remote_commands` 许可决定；服务器不能修改 Agent 设置。`upgrade` 只接收严格的 `MAJOR.MINOR.PATCH` 目标版本（无前导零），有效期 20 分钟，下发时剩余不足 7 分钟按过期拒绝；restart/stop 有效期 5 分钟。
`restart` 在持久化执行标记后以退出码 **75** 结束，由 systemd 的 `Restart=on-failure` 拉起；同一 ID 不会再次重启。
`stop` 持久化成功结果并立即采集、上报，确认成功或最多等待 15 秒后以 **0** 退出；恢复须人工 `systemctl start orbitbeacon-agent`，已启用服务在主机重启后也会恢复。正常 SIGTERM 行为不变。
指令状态保存在 `state/commands.json`（0600）；损坏或写入失败时继续监控但停止执行指令，需人工检查修复，不能随意删除以免丢失去重记录。
每次上报最多发送最旧的两条待确认结果；已处理 ID 最多保留 32 条，只淘汰已过期且结果已确认的记录。容量满时新指令返回 `rejected/failed`。

Linux 上这三组指标不可用时为 null，不影响上报。磁盘 IO 只汇总 `/sys/block/<name>/device` 存在的物理整盘；PSI 使用 some avg60；温度取 (0, 150] °C 内最高值，每分钟后台刷新，缓存超过三分钟即丢弃。设置仅由本地手工编辑并 reload。
