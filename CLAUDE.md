# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**mpfpv** is a pure-userspace, single-binary networking tool (Go) for drone-to-ground-station communication via a cloud relay server. It combines multi-path UDP redundancy (inspired by engarde) with built-in TUN-based virtual IP networking — replacing the need for both engarde and WireGuard.

Star topology: all traffic routes through the server. No peer-to-peer direct connections (by design, though the architecture reserves room for future P2P).

**No encryption by design** — this is intentional, not an oversight. The `teamKey` mechanism is only for pairing validation (prevents accidental cross-connect), not security.

## Build & Run

```bash
# Build (once go.mod exists)
go build -o mpfpv ./cmd/mpfpv/

# Cross-compile (CGO_ENABLED=0 required for static binaries)
make linux-arm        # OpenIPC drone boards
make linux-arm64      # Radxa etc.
make linux-amd64      # cloud server
make windows-amd64    # ground station
make darwin-arm64     # macOS ground station

# Run
./mpfpv -config mpfpv.yml          # mode determined by config file
./mpfpv --mode server              # or via CLI flag

# Test
go test ./...
go test ./internal/protocol/...    # single package
go test -run TestDedup ./internal/protocol/
```

## Architecture

### Binary modes
Single binary, two modes selected by config `mode: client | server`.

### Protocol
Custom 8-byte UDP header (4-byte aligned for ARM):
- Byte 0: Flags (version + message type: Data/Heartbeat/HeartbeatAck)
- Bytes 2-3: Client ID (uint16)
- Bytes 4-7: Sequence number (uint32, per-clientID)
- Bytes 8-N: Raw IP packet payload

### Key packages (planned layout)

| Package | Role |
|---------|------|
| `cmd/mpfpv/` | Entry point, dispatches by mode |
| `internal/client/` | Client main loop — TUN read → encapsulate → multi-NIC send; UDP recv → dedup → TUN write |
| `internal/server/` | Server main loop — session/route table management, heartbeat handling, packet forwarding |
| `internal/tunnel/` | TUN device abstraction with platform-specific implementations (`_linux`, `_windows`, `_darwin`) |
| `internal/transport/` | Multi-NIC discovery (netlink or polling), `SO_BINDTODEVICE` binding, multi-path sender, receiver+dedup |
| `internal/protocol/` | Header codec, sliding-window dedup bitmap, constants/types |
| `internal/config/` | YAML config parsing |
| `internal/web/` | Embedded Web UI (`go:embed`) + JSON API for runtime status/control |

### Data flow
- **Client uplink**: App → TUN → mpfpv encapsulate (add 8-byte header) → send via all NICs (redundant) or best NIC (failover) → server
- **Server forwarding**: UDP recv → dedup (per-clientID sliding window) → validate inner IP src matches registered virtualIP → read dest IP → route table lookup → encapsulate → send to target client (respecting their sendMode)
- **Client downlink**: UDP recv → dedup → strip header → write to TUN → App

### Concurrency model
- Server: G1 UDP recv loop, G2 TUN read loop, G3 timeout cleanup timer
- Client: G1 TUN read→send, G2 UDP recv→TUN write, G3 NIC discovery (netlink/poll), G4 heartbeat timer, GN per-NIC receiver goroutine
- Shared state: `sync.RWMutex` for route/session tables; per-clientID dedup bitmaps (no lock contention); `sync.Pool` for packet buffers

### Multi-path strategy
- **redundant**: every packet sent via all NICs; max reliability, bandwidth × N
- **failover**: best NIC only (by heartbeat RTT), auto-switch on failure with anti-flap protection

### Heartbeat design
- Data packets serve as keepalive when traffic flows; heartbeats only fire after idle threshold (default 200ms)
- In failover mode, heartbeats always go through ALL NICs (to maintain RTT measurements), only Data packets follow single-NIC rule
- Server learns routes exclusively from heartbeats, never from data packet inner IPs

## Agent Team Structure

本项目由 agent team 协作开发，结构如下：

### 项目负责人（主 agent）
- 定义模块间接口契约（Go interface）
- 协调各组进度、分配任务、解决阻塞
- Code review、架构决策
- 最终集成交付

### 三个开发组

| 组 | 负责包 | 开发职责 | 组内 QA 职责 |
|---|---|---|---|
| **协议/传输组** | `protocol/`, `transport/` | 头编解码、去重位图、buffer pool、多网卡发现、SO_BINDTODEVICE、多路径发送、failover RTT | 丢包/乱序模拟、窗口边界、网卡热插拔测试 |
| **服务端组** | `server/`, `config/` | 会话管理、路由表、heartbeat 处理、转发、超时清理、IP 分配持久化、配置解析 | 并发会话、超时清理、teamKey 校验、路由正确性测试 |
| **客户端/TUN组** | `client/`, `tunnel/` | TUN 三平台实现、客户端主循环、heartbeat 定时、收发包 | TUN 读写 mock、heartbeat 状态机、自动分配流程测试 |

### 总 QA（独立角色，兼 Web UI 开发）
- **集成测试**：多 client + server 端到端全链路
- **故障注入**：拔网卡、杀进程、服务器重启恢复
- **硬件验证**：OpenIPC / 树莓派 / Windows / macOS 部署
- **性能基准**：视频流延迟、切换耗时、GC 毛刺
- **Web UI**：`internal/web/` JSON API + 嵌入式单页 HTML
- **回归测试**：每个 Phase 交付后全量回归

### 组内 QA vs 总 QA 分工

| | 组内 QA | 总 QA |
|---|---|---|
| 范围 | 单模块内 | 跨模块 + 端到端 |
| 测试类型 | 单元测试、接口 mock | 集成测试、真机测试、故障注入 |
| 时机 | 开发同步 | Phase 交付节点 |

## Reference Code

`可参考项目/engarde/` contains the engarde source. Key files to reference:
- `cmd/engarde-client/main.go` — multi-NIC discovery + forwarding logic
- `cmd/engarde-client/udpconn_bindtodevice.go` — `SO_BINDTODEVICE` syscall
- `cmd/engarde-server/main.go` — client tracking + broadcast reply
- `engarde.yml.sample` — config format
- `Makefile` — cross-compile targets

## Tech Stack

| Item | Choice |
|------|--------|
| Language | Go |
| TUN library | `golang.zx2c4.com/wireguard/tun` |
| Logging | `github.com/sirupsen/logrus` |
| Config | `gopkg.in/yaml.v3` |
| NIC binding | `SO_BINDTODEVICE` (Linux), address binding (Windows/macOS) |

## Platform Notes

- Linux NIC detection: prefer netlink (`RTM_NEWADDR`/`RTM_DELADDR`) for <10ms detection; fallback to 200ms polling
- `SO_BINDTODEVICE` requires root or `CAP_NET_RAW`
- TUN on OpenIPC may need kernel `CONFIG_TUN=y`
- Default TUN MTU: 1300
- Dedup window: 4096 packets (covers ~3.9s at 5Mbps video with 2-NIC redundancy)
