# 存储架构、MySQL 高可用与加密备份

本文介绍 OrbitBeacon 的双域存储引擎、事务租约模型、MySQL 异步主备切换、人工恢复及加密备份恢复机制。

---

## 1. 双域存储与租约模型

OrbitBeacon 将数据严格划分为两个逻辑域，各持独立连接池与读写锁边界：

| 逻辑域 | SQLite 默认文件 | MySQL | 核心职责 |
| --- | --- | --- | --- |
| **Main（业务域）** | `orbitbeacon.db` | 预建业务 database | 账户、会话、节点资产、身份、设置、流量、探测、告警、审计、通知、备份任务 |
| **History（历史域）** | `metrics.db` | 预建历史 database | 节点 1m/5m/1h 指标聚合、分盘使用率、服务检查 5m 聚合桶 |

### 租约、世代（Generation）与写门控流程

```text
HTTP 请求 / 后台任务
   │
   ▼
[db.Acquire(ctx, Reader/Writer)] ──(绑定当前 Generation 租约 Lease)──┐
   │                                                                │
   ├── 读请求: 仅使用绑定世代的 Reader 连接池                          │
   └── 写请求: 进入域内串行 writeGate ──▶ BeginWrite (含 Guard 探针)    │
                                           │                        │
                         +-----------------+-----------------+      │
                         v                                   v      │
                  COMMIT 正常返回                     COMMIT 结果未知  │
                  释放 gate & 租约                    (503 commit_unknown)
                                                             │      │
                                                             v      │
                                                   冻结准入, 查事务 UUID
                                                   确认已提交或已回滚   │
   <────────────────────────────────────────────────────────────────┘
```

- **世代固定**：普通请求从认证到响应固定同一世代；长连接（WS/SSE）仅在认证、读取、发送时持租约，空闲等待时释放。
- **排空机制**：切换数据库或停机前取消旧世代 context，并等待正在进行的查询、语句、驱动回滚彻底结束；**普通请求超时或客户端取消不会误判为数据库故障**。
- **SQLite 默认加固**：启用 WAL、`foreign_keys=ON`、`synchronous=FULL`、`immediate` 写事务，文件权限严格为 `0600`。
- **MySQL 传输安全**：TCP 省略 `tls` 为明文（仅限受控内网）；配置 `tls: {}` 启用 TLS 1.2+ 且失败不回退明文；Unix socket 不使用 TLS。

---

## 2. MySQL 主备与故障切换

仅在配置 `database.standby` 时激活 HA 复制与控制库；单库部署不创建控制文件及复制任务。

### 2.1 异步行日志复制拓扑

```text
+-------------------------------------------------------------------+
|                        orbitbeacon-server                         |
|                                                                   |
|    +------------------+                    +------------------+   |
|    |   Main 业务域     |                    |  History 历史域   |   |
|    +--------+---------+                    +--------+---------+   |
|             |                                       |             |
|   +---------+---------+                   +---------+---------+   |
|   v                   v                   v                   v   |
| [Primary] ──行日志─▶ [Standby]         [Primary] ──行日志─▶ [Standby]
|  (Active)   (异步)   (只收日志)         (Active)   (异步)   (只收日志)
+-------------------------------------------------------------------+
             ^                                       ^
             +-----------------+---------------------+
                               |
                               v
                 +---------------------------+
                 |  data_dir/storage-control |
                 |  (.db 本地权威与状态控制库) |
                 +---------------------------+
```

- **域名独立**：Main 与 History 可分别由不同端点担任 Active，非活动副本不承担读分流。
- **事务级行日志**：源事务按表白名单捕获完整行镜像（After-Image），目标按原事务与序号重放；单事务上限 **64 MiB**（超限返回 `409 storage_transaction_too_large`），每域日志保留 **24 小时 / 256 MiB**。
- **冲突保护**：遇到主键/唯一键/外键冲突或日志被淘汰，目标立即回滚并撤销完整副本资格，需重新同步，绝不用覆盖写掩盖分叉。
- **约束红线**：Main 与 History 的两端不得指向同一 Server UUID + database；初始化时 Standby 必须为空库；绑定后不可擅自删除 `storage-control.db`、直接删 Standby 配置或由 DBA 手动改写业务表。

### 2.2 自动故障切换与回切状态机

`database.failover` 默认参数：`failure_timeout: 30s`、`max_loss_window: 10s`、`recovery_stable: 60s`、`manual_after: 24h`。

```text
[Active 正常运行]
       │
       │ 确认连接失败 / 只读 / 持久写失败 (非业务约束错或客户端取消)
       ▼
[Suspect 冻结] ── 立即关闭域准入, 取消并排空旧世代, 持续观测 30s (failure_timeout)
       │
       ├── 若 30s 内原主恢复 ──▶ 核对在途事务 UUID 后继续服务
       │
       └── 若超时仍失败 ──▶ 检查 Standby: 是否完整 + 身份正确 + 可证明丢失 <= 10s?
                               │
                 +-------------+-------------+
                 | 是 (可证明)                | 否 (不可证明 / 失联超 24h)
                 v                           v
          [自动提升 Standby]           [转人工锁定 Manual]
          (写控制意图 + 新 Epoch)      (拒绝自动切换; 需人工介入)
                 │
                 ▼
          [旧主恢复流程] ── 解析原在途事务 ── 增量追平或重建 ── 连续可写稳定 60s ──▶ 停写追平水位后回切
```

- **公开接口表现**：切换冻结期间，匿名状态页与公开 API 返回 `503`（不复用旧快照），`/healthz/live` 保持存活；历史域故障不影响健康业务域保持 `ready`。

---

## 3. 人工恢复与存储清理维护

### 3.1 人工恢复流程（Web 或停服 CLI）

- **在线恢复**：业务库可认证时，管理员可通过控制台或 `/api/v1/storage` 触发预览与异步操作（API 令牌无权调用）。
- **离线恢复**：认证不可用时，**停止 Server** 后在宿主机以运行用户执行（`status` 可在运行中读取本地观测）：

```text
1. 查询当前状态  ──▶ orbitbeacon-server storage status --config /etc/orbitbeacon/server.yaml
2. 生成操作预览  ──▶ orbitbeacon-server storage preview --config ... --domain main --action <动作> --target <角色>
   (5 分钟有效)        └── 动作: rebuild (重建非活动副本) | resume (批准单次故障恢复) | promote (人工提升, 须接受丢失)
3. 确认并排队执行 ──▶ orbitbeacon-server storage run --config ... --operation <UUID> --confirm <domain/target/instance_uuid>
                       └── promote 须追加 --accept-data-loss
```

- **清退备库（改回单库）**：先确认当前主库健康且无待决操作，停服后执行 `orbitbeacon-server storage disable --config ... --confirm <cluster-id>`（保留两端数据），随后再从配置移除 `standby`。

### 3.2 手动清理与空间整理

在控制台 **设置 → 数据 → 存储与清理**（`/settings#data-storage`）执行：

| 操作 | 确认口令 | 行为与锁边界 |
| --- | --- | --- |
| **按类别清理** | `CLEAN` | 验证保留天数与类别，每类每次最多删 5000 行并小事务提交；**不删**账户、节点、配置、流量、活动告警、待投递通知或备份 |
| **空间整理** | `COMPACT` | 在 Write Gate 内于事务外运行，**阻塞应用写入**（超时上限 **30 分钟**，反向代理需放宽至 31 分钟）：<br>• **SQLite**：执行 `VACUUM` + WAL checkpoint<br>• **MySQL**：对所选当前活动逻辑域 InnoDB 表逐表 `OPTIMIZE TABLE`（逐表不可整体回滚，不通过应用行日志同步备库） |

> [!WARNING]
> 若清理或整理遇到请求中断、`503 storage_commit_unknown` 或超时，先刷新核实当前状态，**禁止直接自动重试**。

---

## 4. 加密导出、计划备份与离线恢复

### 4.1 三大归档类别与隔离规则

| 类别 | 包含内容 | 不包含内容 |
| --- | --- | --- |
| **Configuration（配置）** | 管理员密码哈希、改密标记、节点持久身份、告警/静默规则、通知与对象存储凭据、备份计划、匹配的 `secrets.key`、全局状态页/令牌限额/存储通知偏好 | 登录会话、一次性激活密钥、API 令牌、待执行任务、待发送通知、部署 YAML、TLS 证书与二进制 |
| **Monitoring（监控）** | 已落库节点观测（`last_seen`/`system`/IP/Geo）、流量、探测与指标历史、服务最新结果与历史桶 | 仅驻留内存的最新报告快照 |
| **Events（事件）** | 审计日志、告警事件、已结束投递记录、站内通知及已结束任务历史 | 历史归档密码（密码单独展示/短时查看，不入归档） |

### 4.2 加密打包与原子恢复流水线

```text
[手动导出 / Cron 计划 (最多 32 个未完成任务, 单 Worker)]
   │
   ├── 1. 按表/列白名单抽取已落库数据至临时传输库 (不暂停采集)
   ├── 2. 流式 tar.gz + age 加密 (.tar.gz.age) + 独立 16 位随机字母数字密码
   └── 3. 写入本地目录 (每计划默认留 7 份) 或上传 S3/COS/OSS/R2 对象存储
```

```text
[离线恢复 (须停服以运行用户执行)]
orbitbeacon-server restore --config /etc/orbitbeacon/server.yaml \
  --input /backup/orbitbeacon-....tar.gz.age --replace --password-file /backup/password.txt

   ├── 1. 校验归档认证、清单 SHA256、DDL 与行值 (拒绝孤立 sidecar / 损坏文件)
   ├── 2. 持久化 prepared 决定 ──▶ 保留完整 old 备份 + 写入 new 镜像与 secrets.key
   ├── 3. 写入 committed 决定 ──▶ 原子切换文件组 (SQLite) 或 guard 事务逐域替换 (MySQL)
   └── 4. 异常处理: committed 前中断下次启动自动回滚至 old; committed 后中断仅继续完成 new
```

### 4.3 计划任务与对象存储参数

- **Cron 规则**：仅接受标准 5 段表达式（默认 `0 3 * * *`，按服务器 `time.Local` 执行），不支持秒字段、`@daily` 或时区前缀；同计划任务运行中不重叠堆积。
- **密码安全**：恢复密码不上传且无主密码托管，详情页仅主动请求显示（30 秒遮罩）。**原机丢失且未留存密码将永久无法解密归档**。
- **对象存储支持**（需显式静态凭据与 HTTPS 1.2+，禁用 HTTP 代理、重定向及内网/元数据 IP）：

| Provider | Endpoint 规范 | 寻址模式与必填项 |
| --- | --- | --- |
| **COS** | `https://cos.<region>.myqcloud.com` | virtual-host；`region` 必填，`bucket` 含 APPID |
| **OSS** | `https://s3.oss-<region>.aliyuncs.com` | virtual-host；`region` 必填（自定义域须绑定 bucket 并配 TLS） |
| **R2** | `https://<account>.r2.cloudflarestorage.com` | path-style；`region` 填 `auto` |
| **AWS S3** | `https://s3.<region>.amazonaws.com` | virtual-host；`region` 必填 |
| **S3 compatible** | 自定义 `https://...`（可带端口，无路径） | 默认 path-style；`endpoint`、`region`、`bucket` 必填 |

- **Schema 兼容**：当前主库 schema **3** / 历史库 **2**。单机 schema 2 启动时原地升级为 3；已有复制元数据的 schema 2 需停服重新播种；恢复包同样要求匹配主库 3 / 历史库 2。
