# 最终回归与关键场景验收

验收时间：2026-10-27T03:35Z 起，工作树 `/data/monkeyray/workspace/orbit-beacon`。

## 已通过

| 范围 | 命令/场景 | 结果 |
|---|---|---|
| 格式、依赖完整性 | `gofmt -l cmd internal`、`go mod verify` | 通过；无未格式化文件，所有模块已验证 |
| 全量 Go 回归 | `go test -count=1 ./...` | 通过；Server、Agent、协议、存储、OpenAPI、bundle、健康、备份恢复测试全部通过 |
| 静态检查 | `go vet ./...` | 通过 |
| 竞态回归 | 固定 `golang:1.27.3-bookworm` 容器内 `CGO_ENABLED=1 go test -race -count=1 ./internal/server/... ./internal/agent/...` | 通过；server、storage、auth、enrollment、ingest、management、nodes、Agent采集/锁/identity/service 全部通过 |
| 双端构建 | `tmpdir=$(mktemp -d) && CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$tmpdir/orbitbeacon-server" ./cmd/orbitbeacon-server && CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$tmpdir/orbitbeacon-agent" ./cmd/orbitbeacon-agent && "$tmpdir/orbitbeacon-server" version --json && "$tmpdir/orbitbeacon-agent" version --json && rm -rf "$tmpdir"` | 通过；两个二进制均输出 Go 1.27.3、linux/amd64、development build metadata |
| Server端到端烟测 | 临时配置启动真实 Server，访问 `/healthz/ready`，发送 SIGTERM | 通过；返回 `{"status":"ready"}`，进程正常退出 |
| 部署JSON/Compose | `jq empty deploy/server.container.json deploy/server.systemd.json`；`docker compose -f deploy/compose.yaml config` | 通过 |
| systemd | 将 unit 的 `ExecStart` 指向实际临时构建二进制后 `systemd-analyze verify` | 通过；未修改宿主 `/usr/local/bin`。直接验证原始 unit 的唯一提示是目标安装路径尚不存在 |
| 容器构建 | `docker build -f deploy/Dockerfile -t orbitbeacon:verify2 .` | 通过；Go 1.27.3 构建阶段与 distroless nonroot 运行阶段均完成 |
| 容器运行 | `docker run -P orbitbeacon:verify2`，访问容器 `/healthz/ready`，检查容器用户 | 通过；返回 `{"status":"ready"}`，用户为 `nonroot:nonroot`。此前发现的数据目录 0700 问题已由独立提交 `1b1bbb7` 修复 |
| O16关键场景 | 一致性 `VACUUM INTO` backup、staged restore、WAL/SHM/journal 清理、schema/index/integrity 拒绝、同名并发输出、失败 staging 清理 | 通过；见 `cmd/orbitbeacon-server/backup_restore_test.go` |

主机直接运行 `CGO_ENABLED=1 go test -race ...` 因环境没有 `gcc` 未通过；同一固定 Go 1.27.3 Debian 容器内的竞态命令已通过，未把主机缺少编译器误报为代码失败。

## 尚未声称通过

- Windows/macOS/arm64/armv7/386 实机或 VM 平台验收。
- 50/500 节点长期资源预算、慢 SSE/断网负载、FD/线程增长和 WAL 峰值预算。
- 签名升级、回滚、断电逐持久化边界和无人值守服务恢复。
- 完整归档备份（数据库之外的 `secrets.key`、配置、信任公钥、manifest）及跨空目录 Agent 接回。
- 公开状态页隐私、探测/告警/通知闭环和目标 Svelte/npm 前端。
- 完整传递依赖许可证法律扫描；当前许可证边界见 `THIRD_PARTY_LICENSES.md`。
