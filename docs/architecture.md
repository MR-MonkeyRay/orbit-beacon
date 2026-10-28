# OrbitBeacon 架构

本文描述当前代码中的运行时边界和数据流。OrbitBeacon 由一个中心服务器、多个 Linux 非 root agent、SQLite 数据库，以及服务器内嵌的管理控制台和公开状态页组成。

## 组件

### Agent

- `cmd/orbitbeacon-agent`：读取管理员固定的安装策略和 agent 配置，校验运行 UID/GID，打开持久身份与配置后启动运行时。
- `internal/agent/collect`：采集系统、CPU、负载、内存、交换空间、磁盘、网络和运行时间等指标。
- `internal/agent/service`：把采集器、持久身份、远端配置和 HTTPS 客户端连接起来。调度器维护一个采集循环和一个上报 worker；它不保存离线队列，只保留最新不可变快照，并把请求或退避期间产生的通知合并到该快照。
- `internal/agent/identity`：持久化安装身份、agent bearer 凭据、`run_generation` 和每次运行的 `session_id`，用于阻止旧进程继续上报。

### Server

- `cmd/orbitbeacon-server`：加载配置，锁定并准备数据目录，打开数据库，组装服务、HTTP/HTTPS listener、可选 metrics listener、保留清理循环和关闭流程。
- `internal/server/auth`：用户、角色、登录、会话、密码变更和 CSRF。
- `internal/server/nodes`：节点、分组、元数据、期望配置、凭据撤销及公开字段设置；公开可见性变更会递增版本号。
- `internal/server/enrollment`：单节点和批量注册票据、认领、恢复与撤销；注册完成后使公开投影失效。
- `internal/server/ingest`：校验 agent 身份与报告顺序，维护各节点最新报告和公开指标的内存状态，按需持久化节点状态。
- `internal/server/management`：用户之外的管理资源，当前包括分组和审计事件查询、清理。
- `internal/server/audit`：把管理变更的审计事件写入发起变更的同一 SQLite 事务；没有用户或系统 actor 的内部操作不生成事件。
- `internal/server/webui`：通过 Go `embed` 提供已构建的管理控制台和公开页静态资源。

## 数据流

```text
Linux host                          OrbitBeacon server
+-----------+     +-----------+      +-------------------------+
| collect   | --> | scheduler | ---> | Agent / enrollment API  |
| metrics   |     | latest    | HTTPS| bearer / ticket         |
+-----------+     +-----------+      +------------+------------+
                        ^                        |
                        | desired config         v
                        +------------------ +-----------+
                                            | ingest    |
                                            +-----+-----+
                                                  |
                              +-------------------+---------------+
                              | latest reports    | coalesced state
                              v                   v
                           memory            SQLite (WAL)
                              |                   ^
                              |                   | management API
                              |                   | session / CSRF
                              |                   +--- Console
                              |                   |
                              |               read-only pool
                              v                   v
                           Public API / visibility cache
                                      |
                                      v
                               Public status page
```

### 采集与上报

1. agent 调度器按当前远端配置启动采集，每次采集有 3 秒超时，并生成递增 `seq` 的协议报告。
2. 容量为 1 的通知通道唤醒上报 worker。上报进行中或退避期间的新采样会覆盖旧快照，因此恢复后发送最新数据，而不是回放历史队列。
3. HTTPS 客户端用节点 bearer 凭据提交报告；服务器返回已接受序号、服务器时间，以及需要时的期望配置。
4. server 在读取请求体前做凭据准入；随后校验协议、报告内容、节点状态、凭据、`run_generation`、`session_id` 和序号。禁用节点被拒绝，旧运行实例或冲突会话被隔离。

### 最新状态与持久化

`ingest.Service` 为活跃节点维护带锁的内存状态，包括最新完整报告、接收时间、公开指标快照、凭据身份栅栏和限流状态。`Latest` 读取的是该内存快照；SQLite 不保存每份历史指标报告。

为了减少写放大，普通连续报告可只更新内存。默认情况下，`last_seen` 的持久化间隔为 30 秒；新运行/会话、系统信息变化、配置确认或错误变化、恢复后的配置版本推进等情况会立即进入持久事务。ingest 的 SQLite 写入通过有界队列和单写者槽串行化；队列满时请求返回忙状态。

流量计数按 agent 进程随机 `epoch` 和 UTC 小时分桶。Agent 重发尚未确认的累计值；ingest 在每节点有界内存缓冲中按 `(epoch, period_start)` 保留接收与发送字节的最大值，并随上述约 30 秒持久事务或关闭排空一起用 `MAX` 写入，因此重发幂等且不会为每份报告增加一次数据库写。响应只确认最近一次成功提交后内存中已知的持久值；提交失败时待写值保留且不确认。确认也包括服务器因超过 35 天保留窗口而丢弃的过期条目，让 Agent 停止重发这些数据。Agent 重启时先单独暂存新运行的流量；只有事务内验证旧缓冲的凭据和已观察到的持久身份仍匹配，才会在同一事务中写入旧、新运行的待写计数并推进身份栅栏，提交后切换缓冲。

服务探测结果也沿用同一合并持久事务节奏及关闭排空，不为每份报告单独写库。每个节点最多保留 5 个目标槽，每个目标可排队 64 个检查结果；达到容量时沿用现有忙响应形成回压，不静默丢弃结果。首次使用某个目标时从 `probe_latest` 取得最后已计数的 `checked_at`，只有时间严格更新的结果才增加该检查时间所在 UTC 小时的 `probe_hourly` 检查/失败计数，重复上报不会重复计数；`probe_latest` 也只接受更新结果。事务写入前采用与流量相同的节点存在、启用状态、凭据轮换和旧 writer 身份栅栏。配置变更可能暂时落后，因此上报中已移除的目标仍可接收，但管理 API 只按当前配置顺序返回目标；配置 PATCH 在事务内清除已移除目标的最新与小时数据。该 API 读取持久数据，因此可能比上报滞后约 30 秒。

### 管理与注册

浏览器管理 API 使用用户会话和角色。节点、分组、用户、注册票据、公开设置等服务直接操作 SQLite；需要审计的变更将业务写入与审计事件放在同一事务中。

注册票据把 agent 安装身份绑定到节点凭据。节点身份变化会使匿名公开缓存失效。节点的远端配置以期望版本和已应用版本协调；agent 只在采集器实际采用新配置后确认对应版本。

### 公开状态页

匿名接口只选择状态可公开、已注册、具有凭据，并且显式启用且公开名称有效的节点。查询通过单独的 SQLite 只读连接池执行，不占用主写池；内存中的最新公开指标仍通过凭据、运行代次和会话身份栅栏与数据库记录匹配。

公开分页结果缓存 2 秒，最多 128 个分页键；并发缓存未命中会合并成一次查询。节点公开设置、启停、删除、凭据撤销或注册身份变化提交后，`nodes.Service` 增加公开可见性版本。缓存条目只有在版本仍匹配时才能命中，避免并发变更把旧页面标记成新数据。

公开页可用相同 `limit`/`offset` 参数连接 `/api/public/v1/stream`。SSE 首帧立即发送；已接受的 agent 上报和公开可见性版本变化通过“版本号 + 每次变更关闭并替换的 channel”广播，通知发布为 O(1) 且不会阻塞上报路径。变化帧最多每 2 秒一次，无变化时至少每 10 秒调用一次同一公开投影；流本身不跨帧保存结果，投影仍使用上述 2 秒缓存和并发未命中合并。公开设置变化会立即递增可见性版本使旧缓存失效，因此取消公开、禁用和删除会在下一帧体现。

每个客户端 IP 最多 4 条流，全局最多 200 条；连接最长 15 分钟。到期或服务器开始关闭时发送 `close` 事件并退出，使 HTTP 优雅关闭不必等待长连接；客户端断开、数据库读取或写入失败则直接结束。公开流使用 `no-store` 和 `X-Accel-Buffering: no`，访问日志在连接结束时记录完整请求。

## 存储与后台任务

服务器使用 `modernc.org/sqlite` 纯 Go 驱动。主连接池启用 WAL、外键、5 秒 busy timeout、`synchronous=FULL` 和 immediate 写事务，最多 3 个连接。迁移按版本在事务中执行，并记录到 `schema_migrations`；如果数据库版本高于当前二进制所知版本，启动会拒绝继续。

指标历史独立存于 `metrics.db`，复用相同的 securefs 权限验证、SQLite 设置和迁移机制，但拥有独立的 `schema_migrations`。主库写锁不会阻塞历史写入；历史只通过只读池检查节点是否存在。历史库版本过新或无法安全打开时记录警告并禁用历史，不阻止主服务启动。历史为尽力而为的数据，不包含在离线或在线 `backup` 中，`restore` 不替换已有历史；停服后移走历史库及 WAL/SHM 可从空历史重新开始。

已接受且非重复的上报通过容量 4096 的非阻塞缓冲区交给单个历史 writer；满时丢弃并增加 `orbitbeacon_history_samples_dropped_total`。只复制成功且新鲜的标量值；网络直接汇总各接口已有的每秒速率，不使用流量累计计数器。每节点每分钟计算 sum/count/max，在约 60 秒一次的独立事务中写入完成的分钟及其 5 分钟、1 小时 rollup，rollup 均值按样本数加权。主机重启的部分分钟用加法合并；未收到数据的区间不生成空点。写失败会重试，内存待写桶上限为 8192（达到上限会丢弃并计数）。1 分钟、5 分钟、1 小时数据分别保留 48 小时、14 天、90 天。每小时保留清理同时清除主库已不存在节点的所有历史和内存待写桶；节点删除与历史移除之间最多有一个清理周期，历史 API 始终先验证节点存在。

匿名公开查询使用另一个 `mode=ro`、`query_only=ON` 的只读池，最多 4 个连接。主库必须先完成迁移，再打开该池。

每小时运行一次保留清理：审计事件保留 90 天；过期注册票据及其重试收据额外保留 24 小时；过期会话按有限批次删除。历史清理通过主键索引逐节点查找孤儿，每个删除事务最多处理 1000 个桶，事务之间接收缓冲区中的样本；一轮可执行多个批次，总删除数没有固定上限。清理失败会记录日志，并在下一轮重试。

主库中的逐小时流量保留 35 天。整日完全超出窗口后，保留任务在同一事务内按节点和 UTC 日汇总所有 epoch/小时、累加写入 `traffic_daily` 并删除对应小时行，避免重复计数；每日汇总保留 400 天。流量 API 把仍在小时表中的数据与每日汇总合并，按节点 `billing_day` 计算 UTC 月度周期，并返回十进制字符串字节计数、配额和最近更新时间。每日汇总、周期计数和接收/发送合计的所有字节加法均在 Go 中按 `MaxInt64` 饱和，超过上限后返回上限值，不会因整数溢出阻塞保留清理。

服务探测的 `probe_latest` 保存每个节点/目标的最新结果，`probe_hourly` 保存按 UTC 小时累计的检查数、失败数和延迟总和。管理 API 的 24 小时窗口选择“大于等于当前 UTC 整点减 23 小时”的桶，不另设上界；逐小时探测数据保留 7 天，现有保留循环按与流量及告警清理相同的有限批次删除更早数据。

## 信任边界

- **主机与 agent 进程**：agent 假定安装策略和状态目录由部署时设定的账户边界保护；运行时拒绝 root、错误 UID/GID 或错误账户。主机本身被攻破后，agent 凭据和采集结果不再可信。
- **agent 与 server**：这是不可信网络边界。报告使用 HTTPS 和 bearer 凭据；server 不信任请求中的节点标识、顺序或运行身份，必须重新从凭据映射节点并检查 fencing 字段。
- **浏览器与管理 API**：浏览器输入和会话均不可信。认证、角色、Origin 和 CSRF 检查位于 HTTP 路由边界，管理服务只接收通过该边界传播的 actor 上下文。
- **匿名访问与内部状态**：公开 API 无会话；数据库查询筛选显式发布的节点，响应使用字段白名单，不直接返回内部节点记录或完整 agent 报告。
- **server 与本地持久化**：SQLite 文件和数据目录属于服务器信任根。打开数据库前会执行本地路径与文件安全检查；迁移和审计依靠 SQLite 事务保持原子性。
- **反向代理**：客户端地址只在配置的可信代理范围内接受转发信息；TLS 可以由 server 或受信任的前置代理终止。

## 关闭顺序

收到 `SIGINT`/`SIGTERM`，或任一 HTTP server 返回错误后，server 按以下顺序停止：

1. 取消根 context，并先将 readiness 标记为不可用。
2. 在同一个 10 秒期限内并行关闭主 HTTP server 和可选 metrics server，等待在途 HTTP handler。
3. 等待所有 Serve goroutine 退出。
4. 用独立的 5 秒期限关闭并排空 ingest；关闭开始后不再接受新的上报处理。
5. 等待保留清理 goroutine 退出。
6. 用独立的 5 秒期限排空历史队列，刷新包含当前未完成分钟的所有待写桶。
7. `run` 返回时执行其余 defer：先释放后注册的 listener，再关闭历史读写池、匿名只读池、主数据库，最后释放数据目录锁。

关闭流程先等待 HTTP handler，再排空 ingest，并在保留清理退出后关闭数据库；HTTP 关闭超时会强制关闭连接，ingest 排空超时会记录告警。


## Alert rules and event history

`internal/server/alerts` evaluates enabled rules every 30 seconds against enrolled,
enabled nodes. Metrics come from identity-fenced `ingest.Service.Latest`, with
server reception age and source freshness checked; disk usage uses the maximum
fresh disk percentage. Missing metrics reset pending timers and do not fire or
resolve existing episodes. `probes_down` counts failed probe results in the latest
report from 0 through 5; no probe results is a real zero rather than missing data,
so removing all targets can resolve an active probe alert. Offline rules use the latest in-memory or persisted
presence, falling back to enrollment time, with a minimum of `nodes.OnlineWindow`.
Metric recovery uses hysteresis (2% of threshold; at least one percentage point
for percent metrics). `probes_down` uses count hysteresis bounded to 0..5.

Migration 10 adds rules and alert episodes to the main SQLite database. Generated
scope foreign keys cascade group/node rule deletion. Episodes keep IDs and rule/name
snapshots so history survives deletion and open events resolve on the next tick.
A unique partial index prevents duplicate firing episodes; open events reload on
each tick, including restart. Pending state is bounded by current rules × nodes.
Rule writes and actor audit entries share a transaction, including rule deletions
caused by deleting a node/group scope. Reads allow viewers;
mutations use the existing administrator, Origin and CSRF checks. Event pagination
reuses audit timestamp/UUID keysets. The existing retention loop removes resolved
episodes older than 90 days using resolution time. Alert events and terminal
notification deliveries each catch up in 500-row transactions for up to 2 seconds
per pass, releasing the SQLite writer between batches.

After commit, notifications enter a bounded queue consumed outside evaluation and
transactions. `Notifier.Notify` must honor context cancellation; this release uses
the durable `notify.Service`; successful sends update `last_notified_at`. Queue
saturation drops notifications and logs/counts the drops. Delivery and retry policy are owned by `internal/server/notify`. The evaluator and worker stop on the server context and
are joined before database close, alongside retention within the existing shutdown
budget (HTTP 10s, ingest drain 5s, history flush 5s; deployment grace 30s).

## 通知通道与持久投递

Migration 11 增加 `notification_channels`、`alert_rule_channels`、`notification_deliveries` 及持久限速窗口 `notification_rate_limits`。规则保存、通道路由变更与规则审计在同一事务；通道 create/update/delete 与各自审计也共用事务。测试投递先在短事务中提交审计尝试和限速额度，再进行不可回滚的出站发送。

`internal/server/secretbox` 从数据目录已验证的 32 字节 `secrets.key` 经 HKDF-SHA256（info `orbitbeacon notification channel v1`）派生 AES-256-GCM 密钥。格式为版本字节 1、随机 12 字节 nonce、密文和 tag；associated data 绑定通道 UUID，阻止密文跨行替换。公开 JSON 不包含密文或明文，仅有 `secret_set`；审计及日志只记录安全分类。备份中的密文必须用匹配的 `secrets.key` 解密。

评估器在事件提交后把带 event ID 的通知交给既有有界队列。真实 Notifier 在短事务中为规则启用的通道生成 pending 记录，保存该次状态的 payload 快照以防恢复事件改写尚未投递的 firing 通知。四条发送 lane 按 due 索引处理，每通道最多一个 in-flight；进程重启重载 pending。网络/HTTP 5xx/429 和 SMTP 临时 4xx 最多尝试 5 次，退避 1、5、15、60 分钟；Retry-After 上限一小时。每通道持久窗口限制 20 次/分钟，积压达到 100 条时后续通知合并到无 in-flight lease 的摘要记录，重置新摘要的尝试次数并保留退避时间。至少一次语义意味着远端接受后、状态提交前崩溃可能重复投递；事件提交与 post-commit 入队间的崩溃及既有队列饱和仍可能丢失通知。

Webhook 和 SMTP 使用共同的安全拨号器：在拨号时解析，默认拒绝回环、RFC1918/ULA、链路本地、组播、未指定和元数据地址，并把检查通过的 IP 字面量直接交给 net.Dialer，杜绝第二次 DNS 查询绕过。HTTP transport 禁止代理和重定向，正文读取上限 4 KiB。TLS 1.2+ 校验证书，SMTP 必须 STARTTLS 或端口 465 隐式 TLS，凭据只在加密通道发送。Telegram 基础地址仅包内测试可替换。默认关闭的两个出站配置开关见部署文档。

终态投递记录由原 retention 循环按 updated_at 分批删除，保留 30 天；pending 不清除。shutdown 停止接收，取消出站连接，并使用最多 5 秒通知预算；无需等待 10 秒发送超时。

Webhook 完整 URL 在 migration 11 的独立 `url_ciphertext` 列加密（AAD 为通道 ID 加 `:url`），`config_json` 只保留源站形式的 `url_redacted` 与 `url_set`。省略 URL 的 PATCH 保留原密文；显式替换/清除 secret 不解密旧值，URL 与签名密钥可独立修复。解密失败返回 `409 secret_unreadable`，worker 使用正常退避与最多五次尝试，允许恢复正确密钥后继续，并每通道每小时仅记录一次安全诊断。出站策略同时检查过渡 IPv6 中嵌入的 IPv4；元数据地址不受允许私网开关豁免。

## API token authentication

`internal/server/auth/api_tokens.go` owns read-only automation credentials. Migration 12 adds `api_tokens`, an indexed SHA-256 hash of a 256-bit secret, a public UUID/display prefix, creator foreign key, and lifecycle timestamps. The secret uses `obt_` plus unpadded base64url and appears only in the creation response. Token management requires an administrator session; mutations retain Origin/CSRF checks and insert `api_token.create` / `api_token.revoke` audit rows in the same transaction. Creation serializes the global 50-active-token cap through an immediate SQLite transaction. Creator deletion cascades; password and role changes do not revoke independent API tokens.

The HTTP layer recognizes Authorization before session authentication, rejects query-string credentials, and restricts bearer requests to allowed management GET/HEAD operations with viewer-equivalent access. Account/session, user, enrollment and API token reads and notification channel test paths are excluded. Cookie credentials cannot elevate bearer access; bearer responses never set cookies. HEAD runs the allowed GET handler and discards its body. Each authentication consults SQLite for immediate expiry/revocation enforcement, with a constant-time hash comparison after the unique-index lookup. Bounded in-memory buckets reuse login limiter machinery (10/s, burst 30 per token; invalid-peer 1/s, burst 30, trusted proxy and IPv6 /64 handling).

A best-effort last-used update checks a one-minute threshold and temporarily disables busy waiting on its checked-out SQLite connection, restoring it before release; a busy writer skips the timestamp. Hourly retention deletes at most 128 rows whose expiry or revocation is older than 30 days. The admin-only Settings panel shows metadata, confirms revocation, and clears the one-time token dialog on close.
