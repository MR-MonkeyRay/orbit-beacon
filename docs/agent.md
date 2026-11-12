# Linux Agent 安装与运维

流程：**创建节点并签发密钥 → 安装并注册 → 确认上报**。术语见[约定](conventions.md)，服务器部署与网络信任见[部署指南](../deploy/README.md#https-与反向代理)。

## 准备

- 服务器已启动，Agent 可访问其源站；管理员已完成初始化改密。
- 安装器识别 CentOS/RHEL、Debian、Ubuntu、Alpine 及其衍生系统，未知 Linux 使用通用工具检测。支持 `x86_64` / `amd64` 与 `aarch64` / `arm64`，不提供 32 位 x86/ARM 发布包。
- 需 root 安装、专用非 root 账户运行；账户工具为 `useradd` / `groupadd` 或 BusyBox `adduser` / `addgroup`，注册需 `runuser` 或 `su`。下载需 `curl`、`sha256sum`、`awk`，另需常用 POSIX 工具及 `install`、支持 `mv -T` / `chown -h` 的工具；无需 `tar`。离线 `--binary` 不需要 curl。
- 不自动安装软件包。Debian/Ubuntu 可用 `apt-get install curl ca-certificates coreutils passwd util-linux procps`；CentOS/RHEL 用 `dnf install curl ca-certificates coreutils shadow-utils util-linux procps-ng`（旧系统用 `yum`）；Alpine 用 `apk add curl ca-certificates coreutils openrc`，账户和 `su` 可用 BusyBox。systemd 的 reload 还需 `procps` / `procps-ng` 提供 `/bin/kill`。
- 不要复制服务器私钥、跳过证书校验，或把管理员 Cookie 传到 Agent 主机。

## 密钥

新增节点后，复制命令，在主机执行，按“密钥（隐藏输入）：”提示粘贴密钥，再等待上报。密钥单独复制；“命令中包含密钥 / Include key in command”默认关闭。已有节点用“更多 → 注册 Agent / More → Enroll Agent”；分组用“部署 Agent / Deploy Agent”，每次部署一个 Agent。关闭弹窗会清空明文。

- 密钥为 32 字节随机数的 base64url 编码，服务端只存 SHA-256；有效 30 分钟，仅可成功激活一次，重复使用返回 `403 forbidden`。
- 重新签发或吊销密钥不撤销已部署 Agent 的节点凭据，也不影响其他密钥。历史密钥保留原期限与使用计数，已使用就不能再次激活。
- `--key` 会出现在进程列表和 shell 历史中，可用于一次性密钥；默认隐藏输入避免这类暴露。不要开 `set -x`，或把密钥放进公共日志、仓库。

## 安装与启动

控制台的一行命令默认不含密钥：通过 HTTPS 下载固定版本安装器，再用 `sudo` 执行。启用“命令中包含密钥”时追加 `--key '密钥'`。版本无效时只提供密钥；Git tag、架构二进制和 `SHA256SUMS` 须已发布，下载失败不改用 latest 或跳过校验。私有 CA 使用下方手动方式。

手动安装使用与服务器匹配、已发布且包含脚本的固定版本；检查后运行，不要把网络脚本直接管道给 root shell。源码部署可读取 `VERSION`，否则手动设置版本：

```sh
VERSION="$(cat VERSION)"
curl --fail --silent --show-error --location --proto '=https' \
  --proto-redir '=https' --tlsv1.2 -o install-agent.sh \
  "https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v$VERSION/deploy/install-agent.sh"
less install-agent.sh
sudo sh ./install-agent.sh \
  --server https://beacon.your-domain.com --version "$VERSION"
```

未提供密钥且没有已有身份时，安装器从 `/dev/tty` 隐藏提示输入。来源顺序：`--key` → `--token-file` → 非终端 stdin → 隐藏提示；`--key` 与 `--token-file` 互斥，`--key` 不可为空。非终端 stdin 为空时仍尝试终端；没有密钥或可用终端则报错。已有身份且未传密钥时不提示。

自动化可用 `--key '一次性密钥'`、`--token-file /安全路径/enrollment.token` 或 stdin；stdin 方式先完成 sudo 认证：

```sh
sudo -v
# TOKEN 已从安全来源读入当前 shell；printf 使用 shell 内建命令。
printf '%s\n' "$TOKEN" | sudo -n sh ./install-agent.sh \
  --server https://beacon.your-domain.com --version "$VERSION"
unset TOKEN
```

- `--server` 与 CA 要求见[配置参考](../deploy/README.md#配置参考)和 [HTTPS](../deploy/README.md#https-与反向代理)；私有 CA 加 `--ca-file /安全路径/ca.pem`。
- 脚本自动选择 `orbitbeacon-agent-linux-amd64` 或 `orbitbeacon-agent-linux-arm64`，只下载 Agent 二进制和 `SHA256SUMS`，校验后安装并赋予执行权限，无需 `tar`。新脚本需配套提供独立二进制附件的版本，不兼容旧压缩包发布；旧版本可手动解压后通过 `--binary` 安装。脚本与二进制下载仅用 HTTPS；`SHA256SUMS` 校验不是独立签名验证。
- 离线安装用 `--binary /安全路径/orbitbeacon-agent-linux-amd64`，可省略版本，自行核对来源与架构。

### 安装器参数

`sh ./install-agent.sh --help` 无需 root。参数使用 `--选项 值`。

| 参数 | 用途 / 默认值 |
| --- | --- |
| `--server URL` | 必填，HTTP(S) 根地址，不带路径、查询、片段或凭据 |
| `--version VERSION` | 固定发布版本，不带 `v`；在线安装必填 |
| `--binary FILE` | 本地非空二进制，替代下载；此时版本可省略 |
| `--key KEY` | 一次性密钥；会出现在进程列表和 shell 历史中，与 `--token-file` 互斥 |
| `--token-file FILE` | 密钥文件；未指定来源时从 stdin 或终端隐藏提示输入 |
| `--ca-file FILE` | HTTPS Server 的私有 CA；HTTP 不可使用 |
| `--update-base-url URL` | HTTPS 发布镜像根地址，重装默认保留；`''` 关闭远程升级 |
| `--uid N`、`--gid N` | 专用账户的非零数字 UID/GID；不传则自动分配。已有账户/组不匹配时拒绝，不迁移身份、不重编号 |
| `--service MODE` | `auto`：运行中的 systemd → 已安装的 OpenRC → 前台；可明确指定 `systemd`、`openrc`、`none` |
| `--no-start` | 完成注册及服务文件安装，但不启用、不启动、不重启服务；已运行的旧进程不会停止 |
| `--download-timeout N` | 单次下载尝试超时秒数，默认 `300`，范围 `1–3600`；连接超时 15 秒 |
| `--download-retries N` | 下载失败的额外重试次数，默认 `3`，范围 `0–10`；不影响 Agent 运行时升级下载 |
| `--uninstall`、`--yes` | 卸载并删除全部本地状态；`--yes` 仅用于跳过卸载确认 |
| `-h`、`--help` | 显示帮助 |

例如，在账号 ID 未被占用的新主机安装后先检查配置，再手动启动：

```sh
sudo sh ./install-agent.sh --server https://beacon.your-domain.com \
  --version "$VERSION" --uid 1000 --gid 1000 --no-start \
  --download-timeout 120 --download-retries 2
```

运行参数如网卡、磁盘路径、探测目标仍统一编辑 `agent.yaml`，见[本地运行参数](#本地运行参数与-reload)，不在安装器重复一套配置解析。

### 账户与服务


脚本创建专用账号 `orbitbeacon-agent`，默认自动分配并固定实际 UID/GID；需要 `1000:1000` 时显式传入 `--uid 1000 --gid 1000`，不复用其他用户名下的 ID。安装路径：

| 路径 | 内容与权限 |
| --- | --- |
| `/opt/orbit-beacon/` | root:orbitbeacon-agent，`0750` |
| `/opt/orbit-beacon/bin/` | orbitbeacon-agent:orbitbeacon-agent，`0750`；内含可替换的 `orbitbeacon-agent`，`0750` |
| `/opt/orbit-beacon/etc/` | root:orbitbeacon-agent，`0750`；文件 `0640`，Agent 只读 |
| `/opt/orbit-beacon/libexec/orbitbeacon-agent-rollback` | root:root，`0755`；以 Agent 用户运行的启动回滚脚本 |
| `/opt/orbit-beacon/etc/install-policy.yaml` | root:orbitbeacon-agent，`0640`；服务模式为 `native`，前台为 `foreground` |
| `/opt/orbit-beacon/etc/agent.yaml` | 带注释的本地运行配置；root:orbitbeacon-agent、`0640`；重装保留，拒绝符号链接 |
| `/opt/orbit-beacon/etc/ca.pem` | root:orbitbeacon-agent，`0640`；可选 CA 副本 |
| `/opt/orbit-beacon/state/` | orbitbeacon-agent:orbitbeacon-agent，`0700`；identity、enrollment.token、run.lock 等身份和状态不应共享 |
| `/etc/systemd/system/orbitbeacon-agent.service` | 非 root systemd 服务 |
| `/etc/init.d/orbitbeacon-agent` | 非 root OpenRC 服务，与 systemd 二选一 |
| `/opt/orbit-beacon/libexec/orbitbeacon-agent-supervise` | OpenRC 专用、root:root、`0755`；以 Agent 用户执行，转发 reload/停止信号，并在异常或升级退出后重启 |

不要以 root 运行 `orbitbeacon-agent`；需要切换用户时用 `sudo -u orbitbeacon-agent`。

脚本以 Agent 账号注册，成功后按所选服务管理器启用并启动（`--no-start` 除外）；`none` 不安装服务，仅打印前台启动命令。OpenRC 服务在每次启动前以 Agent 用户检查升级回滚，正常退出或身份冲突退出码 3 不重启，其他退出延迟 10 秒重启；日志送往系统 syslog，标签 `orbitbeacon-agent`，需主机运行 syslog 服务。注册失败保留 state 中的 `enrollment.token`，不启用服务。

只有确认尚未提交、密钥仍有效时，才带密钥重试：

```sh
sudo -u orbitbeacon-agent /opt/orbit-beacon/bin/orbitbeacon-agent enroll \
  --install-policy /opt/orbit-beacon/etc/install-policy.yaml \
  --config /opt/orbit-beacon/etc/agent.yaml \
  --token-file /opt/orbit-beacon/state/enrollment.token
```

可能已提交但响应丢失时，不再重放原密钥；以 Agent 用户确认已保存的节点凭据：

```sh
sudo -u orbitbeacon-agent /opt/orbit-beacon/bin/orbitbeacon-agent enroll \
  --install-policy /opt/orbit-beacon/etc/install-policy.yaml \
  --config /opt/orbit-beacon/etc/agent.yaml
```

恢复成功后，用原来的 `--server`、版本和 CA 参数重跑安装脚本，加 `< /dev/null` 补装并启动服务，不再传字符串或密钥文件。无密钥恢复不会删除残留 `enrollment.token`；确认恢复成功后自行清理。显式激活只有成功才安全删除密钥；清理失败不代表认领已回滚。原命令含旧字符串时重复执行仍返回 403，不会因本地已有身份而假装成功。

更换服务管理器前，先停止、禁用旧服务并删除对应 unit/init 文件，保留整个 `/opt/orbit-beacon` 后重装；安装器拒绝同时留下两种服务。systemd 可用 `systemctl disable --now orbitbeacon-agent` 后删除 unit 并 `systemctl daemon-reload`；OpenRC 用 `rc-service orbitbeacon-agent stop`、`rc-update del orbitbeacon-agent default` 后删除 init 文件。`--no-start` 不会自动迁移或停止旧服务。

## 运行检查

```sh
sudo systemctl status orbitbeacon-agent
sudo journalctl -u orbitbeacon-agent -n 100 --no-pager
sudo -u orbitbeacon-agent /opt/orbit-beacon/bin/orbitbeacon-agent doctor \
  --install-policy /opt/orbit-beacon/etc/install-policy.yaml --config /opt/orbit-beacon/etc/agent.yaml --json
```

Alpine/OpenRC 用 `sudo rc-service orbitbeacon-agent status`；手动启用并启动用 `sudo rc-update add orbitbeacon-agent default`、`sudo rc-service orbitbeacon-agent start`。日志查看系统 syslog（如 `/var/log/messages`）；`doctor` 命令相同，BusyBox 可用 `su -s /bin/sh orbitbeacon-agent -c '…'` 代替 `sudo -u`。

`doctor` 检查配置、身份文件、HTTP/HTTPS 连通性和采集（HTTPS 同时验证证书），**不注册、不上报、不验证节点凭据**；`authentication: "not_checked"` 是预期结果。

在控制台查看最近上报与样本，使用 `orbitbeacon-agent doctor` 查看本地运行参数；状态含义见[状态与历史](architecture.md#状态与历史)，公开节点步骤见[快速开始](../README.md#6-公开节点)。

常见问题：

| 现象 | 处理 |
| --- | --- |
| `runtime_identity_mismatch` | 用 policy 指定的非 root UID/GID 运行，不能直接 `sudo orbitbeacon-agent run` |
| `install_policy_conflict` | 检查额外 `--config` 和 `ORBITBEACON_*`，按[安装策略校验](#本地运行参数与-reload)消除冲突 |
| `401` / `403 node_disabled` | 检查节点是否禁用或凭据已吊销；按[身份变更规则](architecture.md#状态与历史)启用或重新签发密钥 |
| `409 identity_conflict`，退出码 3 | 同一身份被更新进程取代；systemd/OpenRC 均停止自动重启争抢。克隆机器必须独立 state 并分别注册 |
| 网络或其他 HTTP 错误 | Agent 退避重试，记录首次失败、至少间隔 5 分钟的连续失败摘要及恢复；样本处理见[数据流](architecture.md#数据流) |

`run` 独占 state 中的 `run.lock`。不要删除或替换正在使用的锁；退出后留下空锁文件是正常的。`identity.json` 包含节点凭据，不要手改或复制给另一台机器。`enroll` 不传 `--token-file` 仅恢复已有凭据，不会取得新的授权。

## 本地运行参数与 reload

以下参数在 `run`、`enroll`、`doctor` 中一致，合并顺序见[配置参考](../deploy/README.md#配置参考)。安装策略固定信任根、身份与目录；每个来源独立检查，后续覆盖不能隐藏冲突。

编辑 `/opt/orbit-beacon/etc/agent.yaml`；完整默认模板见 [`agent.yaml`](../deploy/systemd/agent.yaml)。例如，指定磁盘路径、网卡和 HTTP 探测，关闭温度采集及所有远程指令：

```yaml
report_interval_seconds: 10
disk_interval_seconds: 60
interfaces:
  - eth0
disk_paths:
  - /srv/data
probe_targets:
  - https://example.com/health
probe_expected_status:
  https://example.com/health: 204
remote_commands: []
disabled_features:
  - temperature
```

状态目录 `state_dir`、CA 路径 `ca_file`、服务器 `server_url` 和升级源 `update_base_url` 在 root 管理的 [`install-policy.yaml`](../deploy/systemd/install-policy.yaml) 中配置，不放入运行配置。修改安装策略需重启；变更状态目录须停服、迁移完整原状态并同步目录权限和 unit 的 `ReadWritePaths=`，不能创建空目录代替原身份。安装脚本仍使用固定安装布局；自定义策略路径时自行维护 unit，重跑安装脚本会重建安装策略。

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

环境变量为 `ORBITBEACON_REPORT_INTERVAL_SECONDS` 等大写参数名；`remote_commands` 和 `disabled_features` 不提供 flag，其余 flag 使用 `--report-interval-seconds` 等连字符形式。**配置文件使用 YAML 序列和映射；环境变量、CLI 的复合值仍使用 JSON**，例如 `ORBITBEACON_INTERFACES='["eth0"]'` 或 `--probe-targets '["https://example.com/a,b"]'`，不按逗号拆分。`probe_expected_status` 的 flag 示例为 `--probe-expected-status '{"https://example.com/health":204}'`。

修改后执行 `sudo systemctl reload orbitbeacon-agent`，OpenRC 用 `sudo rc-service orbitbeacon-agent reload`，前台进程用 `kill -HUP <PID>`。reload 重读原 YAML，沿用启动时环境和显式 flags，不重读 policy；有效变更在采样边界完整应用并重采，不重置身份或累计流量，坏配置保留旧值。检查日志 `configuration_reloaded` / `configuration_unchanged` / `configuration_reload_rejected` 和下一报告的指标；本地设置不随报告发送；服务 reload 命令成功只代表信号送达。

## 远程升级

控制台的节点“升级”或分组“升级全部”只指定版本号。分组会要求输入组名；已运行目标版本、已关闭或本地不允许的节点跳过。当前版本也必须是正式三段版本，目标必须严格更新；开发版、相同版本和降级返回 `rejected/downgrade`。

安装器把 `update_base_url` 写进 root 所有的安装策略，默认 `https://github.com/mr-monkeyray/orbit-beacon/releases/download`；可用 `--update-base-url URL` 指定 HTTPS 镜像，重装保留已有地址，空值关闭升级能力。安装器只自动读取自己生成的扁平 YAML 策略；若手工改用其他 YAML 写法，重装须显式传入 `--update-base-url URL`（关闭时传 `--update-base-url ''`）。这个值不能通过 `agent.yaml`、reload 或服务器修改，也不随报告上传。docker/foreground 不支持远程升级。

下载和自检在后台执行，期间继续上报；只支持 Linux amd64/arm64。下载使用系统证书及策略中的 `ca_file`，只允许 HTTPS（最多五次重定向）。二进制上限 64 MiB，清单上限 64 KiB。候选版本通过 SHA256 和 `version --json` 自检后，保留 `bin/orbitbeacon-agent.prev`，写入 `state/upgrade-pending`，再原子替换并以 75 退出。下载最多十分钟，并预留 7 分钟给重启、启动确认和结果上报；指令剩余不足 7 分钟就拒绝执行。

新版本第一次被服务器接受的上报才算升级成功，然后删除待确认标记。新版本崩溃，或运行 2 分钟仍没上报成功而自行退出，都算一次启动；第四次启动时，root 所有的回滚脚本以 Agent 用户运行，恢复 `.prev`，结果为 `rolled_back`。升级期间服务器长时间不可用也会触发回滚。`.prev` 在成功升级后保留，供人工恢复；意外中断返回 `interrupted`。回滚脚本绝不执行 `bin/` 里的文件；`etc/` 始终归 root，Agent 只读。

发布源信任边界和安装相同：`SHA256SUMS` 只防传输损坏，不防源被入侵。不接受时从本地 `remote_commands` 移除 `upgrade`。手工恢复先停服务，再以 Agent 用户检查二进制。

## 升级与卸载

从 JSON 配置升级时，先停止 Agent、备份配置，再将 `etc/agent.json` 和 `etc/install-policy.json` 分别复制为 `etc/agent.yaml` 和 `etc/install-policy.yaml`，保留 root 属主、Agent 组及 `0640` 权限。JSON 是 YAML 子集，原内容可直接加载，之后再整理为 YAML 块格式。安装器发现旧文件未迁移会在更改前中止；重装时显式传入原升级源 `--update-base-url URL`（原为空则传 `''`），保留整个 `state/`，**不要修改或删除 `identity.json`**。重跑新安装器会更新 unit 的配置路径；手工部署则同步修改 `--config`、`--install-policy` 并 `daemon-reload`。

按[升级顺序](../deploy/README.md#升级)检查服务器后，下载并检查目标版本的脚本，更新 `VERSION`，再执行：

```sh
sudo sh ./install-agent.sh --server https://beacon.your-domain.com \
  --version "$VERSION" < /dev/null
```

已有 `identity.json` 且没有传入密钥时，脚本保留身份与本地 `agent.yaml`，跳过注册，替换二进制后重启服务。仍须传相同 `--server` 和所需 `--ca-file`；省略 CA 参数会使新 policy 改用系统信任。

卸载前先在控制台吊销节点凭据或删除节点；对应 API 为 `DELETE /api/v1/nodes/{id}/credential`。然后：

```sh
sudo sh ./install-agent.sh --uninstall
# 输入 yes 确认；自动化可加 --yes。
```

卸载会停止并禁用已安装的 systemd/OpenRC 服务，移除服务文件、二进制、policy、本地配置、CA 副本和**全部状态目录**，保留系统账号；前台进程须先手动停止。仅删除本地文件不会使服务器上的节点凭据失效。

### 容器兼容性检查

从仓库根运行 `sh deploy/install-agent.docker-test.sh`，默认使用 Debian 12、Ubuntu 24.04、CentOS Stream 9、Alpine 3.21；可在命令后传镜像列表缩小范围。需要 Go、Docker 及可访问的镜像/软件仓库，仅构建当前宿主 amd64/arm64 架构。容器无特权，安装脚本和二进制只读挂载，安装写入一次性容器。

检查真实系统工具下的离线安装、账户权限、身份/配置保留、非 root 二进制运行和服务文件；用身份保留标记跳过注册，不覆盖真实注册、完整开机启动或跨架构执行。架构选择、下载校验、密钥保护及 OpenRC 退出行为由 `sh deploy/install-agent.test.sh` 覆盖。

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
sudo tee /opt/orbit-beacon/etc/install-policy.yaml > /dev/null <<EOF
server_url: "https://beacon.your-domain.com"
update_base_url: ""
ca_file: ""
state_dir: /opt/orbit-beacon/state
expected_uid: $AGENT_UID
expected_gid: $AGENT_GID
expected_account: ""
install_mode: foreground
EOF
sudo chown "root:$AGENT_GID" /opt/orbit-beacon/etc/install-policy.yaml
sudo chmod 0640 /opt/orbit-beacon/etc/install-policy.yaml
sudo install -o root -g "$AGENT_GID" -m 0640 \
  deploy/systemd/agent.yaml /opt/orbit-beacon/etc/agent.yaml
```

CA 使用限制见[配置参考](../deploy/README.md#配置参考)。私有 CA 由 root 安装为 `/opt/orbit-beacon/etc/ca.pem`（root:Agent 组、`0640`），并填写 policy 的 `ca_file`。policy、CA 及祖先目录须 root 所有且不可被组/其他用户写入，无符号链接；不要放在家目录或 `/tmp`。state 按[安装路径](#安装与启动)的要求归运行用户所有。

用安全通道接收密钥。下面的命令会隐藏读取密钥，保存为当前用户所有的 `0600` 普通文件；路径不能有符号链接，祖先目录只归 root 或运行用户，且不可被组/其他用户写入（安全 sticky 临时目录例外）。以普通用户一次性执行整个括号块，等提示后再粘贴密钥；不要逐行执行或开 `set -x`。子 shell 退出或收到 HUP/INT/TERM 会恢复回显，密钥不进入命令参数、shell 历史或导出的环境变量；写文件用 shell 内建 `printf`：

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
  /opt/orbit-beacon/bin/orbitbeacon-agent enroll --install-policy /opt/orbit-beacon/etc/install-policy.yaml --config /opt/orbit-beacon/etc/agent.yaml --token-file /opt/orbit-beacon/state/enrollment.token
)
```

注册成功后执行：

```sh
/opt/orbit-beacon/bin/orbitbeacon-agent doctor --install-policy /opt/orbit-beacon/etc/install-policy.yaml --config /opt/orbit-beacon/etc/agent.yaml --json
/opt/orbit-beacon/bin/orbitbeacon-agent run --install-policy /opt/orbit-beacon/etc/install-policy.yaml --config /opt/orbit-beacon/etc/agent.yaml
```

Ctrl+C 或 SIGTERM 停止。不传 `--config` 时使用默认运行参数。若需要 systemd 长期运行，优先使用安装脚本；自行配置 [unit](../deploy/systemd/orbitbeacon-agent.service) 时必须同步账号、state 所有权、policy 的 UID/GID 和 `install_mode: native`，不能只复制 unit。`install_mode` 本身不会安装服务。

## API 签发

- 在可信管理机登录，取得会话与 CSRF，按[接口契约（OpenAPI）](../api/openapi.yaml) 调用 `POST /api/v1/enrollments`。
- 单节点请求为 `{"node_id":"节点 UUID"}`；分组请求为 `{"group_id":"分组 UUID"}`，恰好提供一个目标；旧 `mode` 字段会被严格解码拒绝为 400。
- 签发响应中的密钥 `token` 仅显示一次，`server_version` 用于固定安装版本；列表与详情均不含这两个字段。妥善清理临时 Cookie 与密钥副本，结束后注销会话。Origin 规则见[配置参考](../deploy/README.md#配置参考)。

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

`probe_expected_status` 是仅保存在本地 YAML 配置中的映射，例如 `https://example.com/health: 204`：键须在 `probe_targets` 中且为 HTTP(S)，值须为 100–599 的整数。设置后仅精确匹配成功（含低于 400 的不匹配响应），未设置仍以低于 400 为成功；reload 生效，不随报告发送。

HTTPS 收到响应时携带叶证书到期时间 `tls_expires_at`，状态码不匹配时仍携带；握手失败（含过期证书）为 `tls` 且无到期时间。

每 60 秒执行一次，单次超时 5 秒，最多并发 5 个；reload 后新增目标立即开始检查。报告携带所有本地目标的最新结果，首次检查完成前标为 `pending`，不计入失败或可用率；删除目标后停止携带，但历史按正常保留策略清理。结果去重见[状态与历史](architecture.md#状态与历史)。结果不包含响应体、原始错误信息或证书主体。

HTTP 探测不使用 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`，不能依赖代理做出口限制。Linux ICMP 要求 Agent 的组 ID 位于 `net.ipv4.ping_group_range`；用 `sysctl net.ipv4.ping_group_range` 检查，按主机或容器策略配置允许范围。权限不足报告 `unsupported`，不会提权，也不需要 `CAP_NET_RAW`。

字段与错误码见[接口契约（OpenAPI）](../api/openapi.yaml)。

## 远程指令

远程指令只有 `upgrade`、`restart`、`stop`，都受本机 `remote_commands` 限制；本地禁用由 Agent 返回 `disabled_locally`，服务器不保存或修改 Agent 设置。

- `upgrade`：只接受严格 `MAJOR.MINOR.PATCH`（无前导零），有效期 20 分钟；下发时剩余不足 7 分钟按过期拒绝。
- `restart`：持久化执行标记后以 75 退出，由 systemd `Restart=on-failure` 拉起；同一 ID 不会再次重启。有效期 5 分钟。
- `stop`：持久化成功结果并立即采集、上报，确认成功或最多等 15 秒，再以 0 退出；恢复要人工 `systemctl start orbitbeacon-agent`。已启用服务在主机重启后也会恢复，正常 SIGTERM 行为不变。有效期 5 分钟。
- 指令状态在 `state/commands.json`（0600）；文件损坏或写入失败时继续监控，但停止执行指令，需人工修复，不能随意删除，否则丢失去重记录。
- 每次上报最多带最旧的两条待确认结果；已处理 ID 最多留 32 条，只淘汰已过期且结果已确认的记录。容量满时新指令返回 `rejected/failed`。

## 额外指标

磁盘 IO、PSI 和温度这三组指标不可用时为 null，不影响上报。磁盘 IO 只汇总 `/sys/block/<name>/device` 存在的物理整盘；PSI 使用 some avg60；温度取 (0, 150] °C 内最高值，每分钟后台刷新，缓存超过三分钟即丢弃。设置仅由本地手工编辑并 reload。
