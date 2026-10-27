# 许可证与依赖基线

本文件记录当前仓库的发布边界，不把参考项目的许可证自动延伸到 OrbitBeacon。

## 项目许可证

OrbitBeacon 本身使用仓库根目录 `LICENSE` 中的 GNU GPL-3.0。分发源码或二进制时必须同时提供该许可证及适用的第三方版权/许可通知。

## Go 模块基线

`go.mod` 与 `go.sum` 是版本和完整模块图的唯一来源。当前直接依赖及其模块随附许可证为：

| 模块 | 版本 | 许可证/通知 |
|---|---:|---|
| `github.com/shirou/gopsutil/v4` | `v4.26.9` | BSD-3-Clause（模块 `LICENSE`） |
| `golang.org/x/crypto` | `v0.58.0` | BSD-3-Clause（模块 `LICENSE`） |
| `golang.org/x/term` | `v0.47.0` | BSD-3-Clause（模块 `LICENSE`） |
| `modernc.org/sqlite` | `v1.61.0` | BSD-3-Clause（模块 `LICENSE`）；所包含 SQLite 代码/文档按 `LICENSE-SQLITE` 为 Public Domain；其余嵌入内容按模块 `LICENSE-3RD-PARTY.md` |

当前显式间接依赖包含 `modernc.org/libc v1.79.0`。它使用 BSD-3-Clause，并带有自己的 `LICENSE-3RD-PARTY.md`；该通知不能因 `modernc.org/sqlite` 的 BSD-3-Clause 主许可证而省略。其余间接模块以各模块发布的许可证和版权文件为准，不得在没有重新核对通知的情况下新增、升级或打包。

### 发布与变更规则

1. 新增或升级 Go/npm 依赖时，提交必须同时说明精确版本、许可证和是否把代码/资源编入发布物。
2. `go.mod` 中的间接依赖也属于发布审查范围；`go mod verify` 只验证内容完整性，不等价于许可证审查。
3. 发布物必须保留项目 `LICENSE`、直接依赖许可证以及实际编入二进制的传递第三方通知；modernc 的 SQLite/libc 通知按上表对应模块文件同步。
4. 目标 Svelte/Node 版本尚未进入当前仓库；在创建 `web/` 和 lockfile 前，不把方案中的前端版本、peer 约束或许可证当作已引入依赖。进入后必须单独生成 npm 依赖许可证清单。
5. ServerStatus-Rust、Nezha、Komari、Beszel 只用于设计比较，不是代码或依赖来源；其 Apache/WTFPL/MIT 等许可证不改变本项目 GPL-3.0 基线。

## 可重复核对

```sh
go mod verify
go list -m all
grep -n '^\(require\|replace\)' go.mod
```

发布前还需要对最终构建图执行许可证扫描，并将扫描结果与发布物一同归档；本文件不声称已经完成完整的传递依赖法律审计。
