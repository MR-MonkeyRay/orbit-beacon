# Linux Agent 安装、配置与运维

核心部署流：**创建节点并签发密钥 → 安装与注册 → 验证上报**。术语参见 [约定](conventions.md)，服务端部署与 HTTPS 代理见 [部署指南](../deploy/README.md)。

---

## 1. 生命周期与权限隔离模型

```text
+-------------------------------------------------------------------------+
|                    Agent 主机安全边界 (root 安装 / 非 root 运行)           |
|                                                                         |
|  /opt/orbit-beacon/           (root:orbitbeacon-agent  0750)            |
|  ├── etc/                     (root 只写, Agent 只读)                    |
|  │   ├── install-policy.yaml  0640 信任根: server_url / update_base_url |
|  │   ├── agent.yaml           0640 运行参数: 间隔 / 网卡 / 磁盘 / 探测     |
|  │   └── ca.pem               0640 可选私有 CA 证书                       |
|  ├── libexec/                 (root:root  0755)                         |
|  │   ├── orbitbeacon-agent-rollback   启动 4 次未上报自动回滚 .prev      |
|  │   └── orbitbeacon-agent-supervise  OpenRC 进程守护与信号转发          |
|  ├── bin/                     (orbitbeacon-agent  0750)                 |
|  │   ├── orbitbeacon-agent    可在线原子替换的二进制                     |
|  │   └── orbitbeacon-agent.prev  升级保留的上个版本                     |
|  └── state/                   (orbitbeacon-agent  0700 私有状态)        |
|      ├── identity.json        节点 256-bit 凭据与运行身份 (严禁跨机复制!) |
|      ├── commands.json        远程指令去重与待确认回执 (0600, 最多 32 条)|
|      ├── enrollment.token     未完成激活时的临时密钥 (成功后自动清理)     |
|      ├── upgrade-pending      待新进程首次上报确认的升级标记             |
|      └── run.lock             单实例文件锁                              |
+-------------------------------------------------------------------------+
```

- **信任边界**：Agent 本地以专用非 root 账户 `orbitbeacon-agent` 运行，通过 `install-policy.yaml` 强制校验 UID/GID、服务器地址与更新源；禁止直接 `sudo orbitbeacon-agent run`。

---

## 2. 环境准备与安装方式

### 2.1 前置依赖

- **系统与架构**：支持 Debian / Ubuntu / CentOS / RHEL / Alpine 及常见衍生发行版，仅提供 `linux/amd64` 与 `linux/arm64`（无 32 位包）。
- **基础工具**：root 权限、账户工具（`useradd`/`groupadd` 或 BusyBox `adduser`/`addgroup`）、`runuser`/`su`、`curl`（离线 `--binary` 可免）、`sha256sum`、`awk`、`install`（支持 `mv -T` / `chown -h`）；systemd 重载需 `/bin/kill`（`procps`/`procps-ng`）。
- **安全红线**：切勿复制服务端私钥、跳过 TLS 校验，或将管理员 Cookie 放在 Agent 主机。

### 2.2 一键与脚本安装

控制台新建节点后默认生成**不含明文密钥**的安装命令（有效期 30 分钟、仅能成功激活一次）；如确认环境安全也可开启“命令中包含密钥”追加 `--key`。

手动检查并安装（推荐做法，不直接管道执行未知脚本）：

```sh
VERSION="$(cat VERSION)" # 或显式指定固定版本号 (不带 v)
curl --fail --silent --show-error --location --proto '=https' \
  --proto-redir '=https' --tlsv1.2 -o install-agent.sh \
  "https://raw.githubusercontent.com/mr-monkeyray/orbit-beacon/v$VERSION/deploy/install-agent.sh"
less install-agent.sh
sudo sh ./install-agent.sh --server https://beacon.your-domain.com --version "$VERSION"
```

- **密钥输入优先级**：`--key`（会暴露于进程列表与 shell 历史）→ `--token-file` → 非终端 stdin → `/dev/tty` 隐藏提示输入。
- **自动化 stdin 示例**（先 `sudo -v` 避免密码提示吞掉 stdin）：
  ```sh
  sudo -v
  printf '%s\n' "$TOKEN" | sudo -n sh ./install-agent.sh \
    --server https://beacon.your-domain.com --version "$VERSION"
  unset TOKEN
  ```
- **API 签发密钥**：管理员会话可通过 `POST /api/v1/enrollments`（传 `{"node_id":"<UUID>"}` 或 `{"group_id":"<UUID>"}`）签发一次显示的 `token` 与 `server_version`。

### 2.3 安装脚本常用参数

| 参数 | 用途与默认行为 |
| --- | --- |
| `--server URL` | 必填，HTTP(S) 根地址（无路径、查询或凭据） |
| `--version VERSION` | 固定发布版本（不带 `v`）；在线安装必填 |
| `--binary FILE` | 离线模式：使用本地二进制，无需 `curl` 与 `--version` |
| `--key KEY` / `--token-file FILE` | 传入一次性注册密钥（两者互斥） |
| `--ca-file FILE` | 私有 CA 证书路径（HTTPS 专用） |
| `--update-base-url URL` | 固定更新 HTTPS 根源；重装省略时保留原配置 |
| `--uid N` / `--gid N` | 指定非零 UID/GID；已有账户不匹配时拒绝执行 |
| `--service MODE` | `auto`（默认: systemd → OpenRC → 前台）/ `systemd` / `openrc` / `none` |
| `--no-start` | 仅安装与注册，不启用/重启服务 |
| `--download-timeout N` / `--download-retries N` | 下载单次超时（默认 300s, 1–3600）与额外重试次数（默认 3, 0–10） |
| `--uninstall` / `--yes` | 停止服务并清除程序、配置与 **state 全部状态**（保留系统账号） |

---

## 3. 本地配置与热重载 (`SIGHUP`)

配置优先级：**CLI Flags > `ORBITBEACON_*` 环境变量 > YAML 文件 > 默认值**（安装策略 `install-policy.yaml` 独立校验、不受覆盖）。

### `agent.yaml` 运行参数

编辑 `/opt/orbit-beacon/etc/agent.yaml`（默认模板见 [`agent.yaml`](../deploy/systemd/agent.yaml)）：

| 参数 | 默认值 | 范围与说明 |
| --- | --- | --- |
| `report_interval_seconds` | `10` | `5–60` 秒，主机采集与正常上报共用周期 |
| `disk_interval_seconds` | `60` | `60–3600` 秒，磁盘容量/inode 采样周期 |
| `interfaces` | `[]` | 最多 8 项；空数组自动选用默认路由网卡 |
| `disk_paths` | `[]` | 最多 8 个绝对路径；空数组选用根路径 `/` |
| `probe_targets` | `[]` | 最多 5 项（TCP/HTTP/HTTPS/DNS/ICMP） |
| `probe_expected_status` | `{}` | 本地映射：HTTP(S) 目标 → 精确预期状态码 (`100–599`) |
| `remote_commands` | `["check_update","upgrade","restart","stop"]` | 本地权限白名单，与 `install_mode` 取交集；`[]` 全关 |
| `disabled_features` | `[]` | 可关 `disk`/`network`/`traffic`/`probes`/`disk_io`/`pressure`/`temperature` |

> [!NOTE]
> - 环境变量 / CLI 传递数组或映射时须用 JSON 字符串（如 `ORBITBEACON_INTERFACES='["eth0"]'`）；总上报体受 JSON 转义后 **64 KiB** 预算约束。
> - 关闭 `disk` 联动关闭 `disk_io`；关闭 `network` 联动关闭 `traffic`；关闭 `probes` 会清空探测目标。

### 热重载（不中断上报与累计流量）

```sh
sudo systemctl reload orbitbeacon-agent      # systemd
sudo rc-service orbitbeacon-agent reload   # OpenRC
kill -HUP <PID>                            # 前台进程
```

重载仅重读原 YAML 并沿用启动时环境变量与 Flags（不重读 `install-policy.yaml`）；坏配置保留旧值。通过日志 `configuration_reloaded` / `configuration_unchanged` / `configuration_reload_rejected` 确认结果。

---

## 4. 采集、流量累计与服务探测

### 4.1 流量累计与服务器 ACK

```text
Agent 内存状态 (重启建新 epoch, 不持久化, 最多保留 24 个 UTC 小时桶)
   │
   ├── 每个周期读取网卡 RX/TX 差值 ──▶ 累加至当前 UTC 小时桶 [epoch, period_start, rx, tx]
   │
   ├── 随 POST /report 发送未结算桶 ──▶ Server 在写事务入库并回传 acked_traffic
   │
   └── 收到 ACK 后仅当桶内计数未再增长时清理 (避免覆盖采样窗口内新增流量)
```

### 4.2 本地服务探测（每 60s 执行, 单次超时 5s, 最多并发 5 个）

| 目标格式 | 检测方式与成功判定 |
| --- | --- |
| `tcp://host:port` | TCP 建连耗时（端口 `1–65535`，IPv6 如 `tcp://[::1]:443`） |
| `http(s)://host/path` | GET 至响应头（丢弃 ≤ 64 KiB body，不重定、不用代理）；默认 `<400` 成功（可用 `probe_expected_status` 覆盖），HTTPS 校验证书并上报 `tls_expires_at` |
| `dns://host` | DNS 解析耗时，至少返回一个地址为成功（仅接受纯主机名） |
| `icmp://host` | Linux 非特权 datagram ping（需 GID 在 `sysctl net.ipv4.ping_group_range` 范围内，权限不足或其他系统报 `unsupported`） |

- **隐私提醒**：`probe_targets` 会展示在控制台，**严禁在 URL 中包含密钥或 Token**。

### 4.3 额外主机指标

- **磁盘 IO**：汇总 `/sys/block/<name>/device` 存在的物理整盘读写字节率与最高忙碌率。
- **PSI**：`some avg60`（CPU / 内存 / IO）。
- **温度**：`(0, 150] °C` 内最高有效传感器温度，每分钟后台刷新，缓存超 3 分钟丢弃。

---

## 5. 远程指令、更新检查与自动回滚

Server 仅下发四种固定无参数指令（状态与去重持久化于 `state/commands.json`，每次上报最多携带 2 条最旧待确认结果，已处理记录保留 32 条）：

| 指令 | 有效期 | 允许模式 | 执行行为 |
| --- | --- | --- | --- |
| `check_update` | 5 分钟 | 全部模式 | 查询本地 `update_base_url` 发现最新版本（只回传类型 `github`/`custom`、版本号与时间，禁止降级） |
| `upgrade` | 20 分钟 (余<7m拒绝) | 仅 `native` | 后台下载校验并执行原地替换，以退出码 **75** 重启；详见下方状态机 |
| `restart` | 5 分钟 | `native` / `docker` | 持久化已执行标记后以 **75** 退出，由服务管理器拉起 |
| `stop` | 5 分钟 | `native` / `foreground` | 持久化结果并触发立即上报（≤15s），随后以 **0** 退出，需人工 `start` 恢复 |

### 原地升级与 4 次失败自动回滚流程

```text
[Server 下发 upgrade] ──▶ 本地检查 update_base_url & remote_commands
                              │
                              ├── GitHub 源 ──▶ api.github.com/repos/.../releases/latest
                              └── 其他 HTTPS ──▶ {base}/latest.json
                              ▼
                   下载 {base}/vX.Y.Z/{SHA256SUMS, 二进制} (HTTPS ≤5 重定向, 限 64 MiB)
                              │
                              ├── 校验 SHA256 + `version --json` 自检通过
                              ├── 备份旧版为 bin/orbitbeacon-agent.prev + 写 state/upgrade-pending
                              └── 原子替换二进制并以 exit 75 退出 ──▶ systemd/OpenRC 拉起新进程
                                                                          │
                          +-----------------------------------------------+
                          v                                               v
               [2 分钟内首份上报成功]                          [崩溃或 2 分钟无有效上报]
               删除 upgrade-pending,                           第 1–3 次退出自动重试;
               保留 .prev 供手动应急                           第 4 次启动由 rollback 脚本恢复 .prev
                                                               ──▶ 回报 rolled_back!
```

> [!IMPORTANT]
> 更新源只能在 `install-policy.yaml` 中修改并重启生效（默认 `https://github.com/mr-monkeyray/orbit-beacon/releases/download`）。`SHA256SUMS` 仅校验传输完整性；若不信任上游二进制发布源，请在本地 `agent.yaml` 中将 `check_update` 与 `upgrade` 从 `remote_commands` 移除。

---

## 6. 诊断检查与常见排障

### 日常自检命令

```sh
# 1. 服务与日志
sudo systemctl status orbitbeacon-agent
sudo journalctl -u orbitbeacon-agent -n 100 --no-pager
# Alpine OpenRC: sudo rc-service orbitbeacon-agent status  (日志见 /var/log/messages)

# 2. 本地诊断 (校验配置/身份/连通性与采集; 不触发上报或校验节点凭据)
sudo -u orbitbeacon-agent /opt/orbit-beacon/bin/orbitbeacon-agent doctor \
  --install-policy /opt/orbit-beacon/etc/install-policy.yaml \
  --config /opt/orbit-beacon/etc/agent.yaml --json
```

### 常见故障速查

| 错误 / 现象 | 原因与处理方式 |
| --- | --- |
| `runtime_identity_mismatch` | 当前运行 UID/GID 与 `install-policy.yaml` 不一致；使用 `sudo -u orbitbeacon-agent` 切换运行用户 |
| `install_policy_conflict` | 环境变量或额外 `--config` 企图改写受保护的策略字段，检查冲突配置源 |
| `409 identity_conflict` (exit 3) | 同一节点凭据被更新进程覆盖；systemd/OpenRC 自动停止争抢。**克隆虚拟机必须先删除 `state/` 并重新注册** |
| `401` / `403 node_disabled` | 节点已在控制台禁用或凭据被吊销；重新启用或签发新密钥后 `enroll` |
| 注册中断 / 响应丢失恢复 | **不要重放旧密钥**！先执行无 `--token-file` 的 `enroll` 检查是否已保存凭据：<br>`sudo -u orbitbeacon-agent /opt/orbit-beacon/bin/orbitbeacon-agent enroll --install-policy ... --config ...`<br>若确认未成功且密钥仍有效，再追加 `--token-file /opt/orbit-beacon/state/enrollment.token` |
| 更换服务管理器 | 先停用并删除旧 unit/init 文件，保留 `/opt/orbit-beacon/` 目录后重跑 `install-agent.sh` |

---

## 7. 进阶：升级迁移、容器测试与手动前台安装

- **版本升级**：**先升级 Server，再升级 Agent**。保留完整 `state/identity.json` 与原参数，加 `< /dev/null` 重跑新版本 `install-agent.sh` 即可无缝保留身份与配置；从旧 JSON 配置或旧 `update_channel` 升级时，须先按安装器提示迁移为扁平 YAML 格式。
- **容器兼容性测试**：在仓库根运行 `sh deploy/install-agent.docker-test.sh`（需 Go 与 Docker），将在 Debian 12、Ubuntu 24.04、CentOS Stream 9、Alpine 3.21 无特权容器中验证离线安装、权限与服务文件生成；本地脚本单元测试运行 `sh deploy/install-agent.test.sh`。
- **手动前台安装**：不使用安装脚本时，可手动创建 `0750`/`0700` 目录、写入 `install_mode: foreground` 的 `install-policy.yaml`、通过 `stty -echo` 将密钥写入 `0600` 文件执行 `enroll`，随后用 `orbitbeacon-agent run` 前台启动。
