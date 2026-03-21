# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**mpfpv** 是一个纯用户态、单二进制的组网工具（Go），用于无人机与地面站通过云服务器中继通信。整合了多路径 UDP 冗余（类似 engarde）和 TUN 虚拟 IP 组网（类似 WireGuard），替代了同时需要 engarde + WireGuard 的方案。

**星形拓扑**：所有流量经服务器中转，客户端之间不直连。

**不加密**：这是设计决策。`teamKey` 仅用于配对校验（防误连），不是安全机制。

## Build & Run

```bash
# 本地构建
go build -o mpfpv ./cmd/mpfpv/

# 交叉编译（全部目标）
make all                  # = server + client(x86/arm/windows)
make server               # build/server/mpfpv-linux-amd64
make client-x86           # build/client/x86/mpfpv-linux-amd64
make client-arm           # build/client/arm/mpfpv-linux-arm64
make client-windows       # build/client/windows/mpfpv-windows-amd64.exe
make client-windows-gui   # build/client/windows/mpfpv-gui.exe (WebView2 GUI)
make client-darwin        # build/client/darwin/mpfpv-darwin-arm64
make client-mipsle        # build/client/mipsle/mpfpv-linux-mipsle (OpenIPC)

# 运行
./mpfpv -config mpfpv.yml -v     # -v 开启 debug 日志
./mpfpv -version                 # 查看版本号

# 测试
go test ./...                                    # 单元测试
go test -tags integration -v ./test/...          # 集成测试
go test -run TestDedup ./internal/protocol/      # 单个测试
```

## 构建产物目录结构

```
build/
├── server/
│   └── mpfpv-linux-amd64           # 服务器（仅 Linux amd64）
└── client/
    ├── x86/mpfpv-linux-amd64       # Linux x86 客户端
    ├── arm/mpfpv-linux-arm64       # ARM64 客户端（Radxa 等）
    └── windows/
        ├── mpfpv-windows-amd64.exe # Windows 命令行客户端
        └── mpfpv-gui.exe           # Windows GUI 客户端（WebView2）
```

## Architecture

### 两种入口

| 入口 | 说明 |
|------|------|
| `cmd/mpfpv/` | 命令行入口，按 config `mode` 分发到 server 或 client |
| `cmd/mpfpv-gui/` | Windows GUI 入口，使用 WebView2 原生窗口内嵌 Web UI，支持连接/断开/配置 |

### 协议

自定义 8 字节 UDP 封装头（4 字节对齐，ARM 友好）：

```
Byte 0:     Flags (高4位=Version1, 低4位=Type: Data/Heartbeat/HeartbeatAck)
Byte 1:     Reserved
Bytes 2-3:  Client ID (uint16, big-endian)
Bytes 4-7:  Sequence Number (uint32, big-endian, per-clientID 递增)
Bytes 8-N:  Payload (原始 IP 包 或 Heartbeat/Ack 结构)
```

Heartbeat payload（固定 16 字节 + 可变长设备名）：
- 前 16 字节：VirtualIP(4) + PrefixLen(1) + SendMode(1) + Reserved(2) + TeamKeyHash(8)
- 16 字节之后：UTF-8 设备名字符串（可选，向后兼容）

### 包结构

| Package | 职责 |
|---------|------|
| `internal/protocol/` | 8 字节头编解码、Heartbeat/Ack 编解码、滑动窗口去重(bitmap)、TeamKeyHash |
| `internal/config/` | YAML 配置解析、校验、默认值、SaveConfig |
| `internal/tunnel/` | TUN 设备抽象 + 平台实现（Linux: 纯 syscall; Windows: wintun+go:embed; macOS: 桩） |
| `internal/transport/` | 网卡发现(白名单+轮询)、SO_BINDTODEVICE、多路径发送器(redundant/failover)、RTT 跟踪 |
| `internal/server/` | 服务端：UDP 监听、Heartbeat/会话管理、路由表、数据转发、IP 自动分配(持久化)、超时清理 |
| `internal/client/` | 客户端：心跳循环、收发包、TUN 读写、多路径/单网卡模式切换、设备名+machineID |
| `internal/web/` | 嵌入式 Web UI（go:embed HTML）+ JSON API（客户端/服务端/GUI 控制） |

### 数据流

```
[App] → TUN → client encapsulate(+8B header) → UDP multipath → [Server]
[Server] → UDP recv → dedup → validate srcIP → route lookup → forward to target client
[Target Client] → UDP recv → dedup → strip header → TUN write → [App]
```

服务器自身 TUN 流量使用 clientID=0。

### 设备识别与 IP 分配

- **clientID** = FNV-1a hash(hostname + machineID)，自动生成，同机器永远一致
  - Linux machineID: `/etc/machine-id`
  - Windows machineID: 注册表 `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`
- **virtualIP** 由服务器统一分配，客户端不配置
- **设备名**（hostname）随 Heartbeat 发送，服务端显示用
- IP 分配持久化到 `ip_pool.json`：`[{clientID, ip, name}]`，同一设备重连拿到同一 IP

### 网卡发现（白名单模式）

只使用真实物理网卡，不用黑名单：
- Linux: `eth*`, `enp*`, `ens*`, `eno*`, `enx*`, `wlan*`, `wlp*`, `wlx*`, `usb*`
- Windows: 名称含 `Wi-Fi`/`Ethernet`/`以太网`/`USB`/`WLAN`
- macOS: `en*`
- IP 段过滤：排除 `169.254/16`(link-local)、`100.64/10`(Tailscale/CGNAT)、`10.99/16`(自身虚拟IP)
- 非 IPv4 包过滤：TUN 读出的包检查 IP 版本字段，非 IPv4 直接丢弃

### Windows 客户端特殊处理

- **单网卡模式**：通过 `bindInterface` 配置项指定网卡，跳过多路径
- **GUI**（`cmd/mpfpv-gui/`）：使用 `jchv/go-webview2`（纯 Go，无 CGO）创建原生窗口
- **wintun.dll**：通过 `go:embed` 嵌入二进制，运行时自动释放到 exe 同目录
- **配置极简**：用户只需填服务器地址 + Team Key + 选网卡

### 多路径策略

- **redundant**：每个包通过所有活跃网卡各发一份，最大可靠性
- **failover**：只走 RTT 最优网卡，故障自动切换（5 秒防乒乓冷却）
- 心跳始终通过所有网卡发送（即使 failover 模式），保证 RTT 探测
- RTT 滑动窗口 10 个样本，连续 5 次 miss 标记 Down

### Web UI

嵌入式单页 HTML（`internal/web/static/index.html`），原生 JS，2 秒轮询 API。

**服务端 API**：
- `GET /api/clients` / `GET /api/clients/{id}` / `DELETE /api/clients/{id}`
- `GET /api/routes`
- `GET/POST /api/server-config`（修改 teamKey/listenAddr）

**客户端 API**：
- `GET /api/status` / `GET /api/interfaces`
- `POST /api/sendmode` / `POST /api/interfaces/{name}/{enable|disable}`

**GUI 控制 API**（仅 Windows GUI）：
- `GET/POST /api/config` / `POST /api/connect` / `POST /api/disconnect`
- `GET /api/connection-status` / `GET /api/available-interfaces`

### 并发模型

- **Server**: G1 UDP recv loop, G2 TUN read loop, G3 cleanup timer (1s)
- **Client**: G1 TUN read→send, G2 UDP recv→TUN write (或 multipath recv channel), G3 NIC discovery polling (200ms), G4 heartbeat timer (1s)
- 共享状态: `sync.RWMutex`(sessions/routes), per-clientID dedup bitmap(无锁竞争), `sync/atomic`(seq/registered)

### 超时机制

- **addrTimeout** (默认 5s)：单个源地址超时移除，但只要还有活跃地址不删 session
- **clientTimeout** (默认 15s)：所有地址均无活动才删除整个 session + 路由

## 当前部署环境

| 节点 | IP | 虚拟 IP | 说明 |
|------|-----|---------|------|
| 阿里云 ECS | 114.55.58.24 | 10.99.0.254 | 服务器，UDP :9800，Web UI :9801 |
| PVE Ubuntu VM | 192.168.1.197 | 10.99.0.1 | 测试客户端 (ubuntu-dev) |
| Radxa Zero3 | (Tailscale) | 10.99.0.3 | 无人机端客户端 (radxa-zero3) |
| Windows PC | (WLAN) | 10.99.0.2 | 地面站 GUI 客户端 (DESKTOP-QE166QH) |

**注意**：绝对不要在 PVE 宿主机（192.168.1.100）上部署，会干扰整个网络。用 PVE 内的 Ubuntu VM。

## 已知问题与待办

### 已知问题
- Windows TUN 稳定性偶发问题（偶尔 ping 丢包），疑似 wintun 驱动或防火墙干扰
- Windows 上 Clash TUN 模式会冲突（198.18.0.1 fake-ip），需关闭 Clash 或加直连规则
- 心跳和数据共用 seq 计数器，多播/广播包消耗 seq 导致有效数据 seq 跳号

### 待优化
- Linux netlink 事件驱动网卡检测（当前仅轮询）
- buffer pool (`sync.Pool`) 减少 GC
- HeartbeatAck 中继续打印 "registered" 已修复，但 debug 级别的 heartbeat sent 日志仍会每秒输出
- macOS TUN 实现（当前为桩）
- 考虑 Windows 端改为端口转发模式替代 TUN，彻底避免驱动/防火墙问题

## Tech Stack

| 项 | 选择 |
|----|------|
| 语言 | Go 1.23+ |
| TUN 库 | Linux: 纯 syscall; Windows: `golang.zx2c4.com/wireguard/tun` + wintun |
| Windows GUI | `github.com/jchv/go-webview2`（纯 Go，无 CGO） |
| 日志 | `github.com/sirupsen/logrus` |
| 配置 | `gopkg.in/yaml.v3` |
| 网卡绑定 | `SO_BINDTODEVICE` (Linux), 地址绑定 (Windows/macOS) |

## 参考代码

`可参考项目/engarde/` 包含 engarde 源码，关键参考文件：
- `cmd/engarde-client/main.go` — 多网卡发现 + 转发逻辑
- `cmd/engarde-client/udpconn_bindtodevice.go` — SO_BINDTODEVICE 实现
- `cmd/engarde-server/main.go` — 客户端追踪 + 广播回包

## Commit 规范

- Commit message 使用中文
- 标准格式：`feat:` / `fix:` / `refactor:` / `test:` + 中文描述
