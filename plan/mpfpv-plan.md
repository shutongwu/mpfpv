# mpfpv 实现方案

## Context

团队做无人机业务，无人机与地面站通过云服务器通信。当前用 Tailscale（单网卡）或 engarde+WireGuard（多网卡），痛点：
- 断网重连慢（Tailscale）
- 多网卡冗余需要额外跑 engarde
- WireGuard 内核模块在 OpenIPC 等精简系统上难部署

**目标**：写一个纯用户态的单二进制组网工具，整合 engarde 的多路径冗余能力和 WireGuard 的虚拟 IP 能力，去掉加密开销。

---

## 架构概览

```
无人机 (client)                   云服务器 (server)                 地面站 (client)
┌──────────────┐                 ┌──────────────┐                ┌──────────────┐
│  App Layer   │                 │              │                │  App Layer   │
│(WebRTC, MAV) │                 │              │                │(GCS, Video)  │
│      │       │                 │              │                │      │       │
│  TUN 10.99.0.1                 │  TUN 10.99.0.254              │  TUN 10.99.0.2
│      │       │                 │      │       │                │      │       │
│  mpfpv-client│                 │  mpfpv-server│                │  mpfpv-client│
│   ┌─┬─┐      │                 │      │       │                │      │       │
│   5G   5G    │                 │   公网 IP     │                │   单网卡     │
└──────────────┘                 └──────────────┘                └──────────────┘
    │ │  │                            │                               │
    └─┴──┴──── UDP 多路径 ────────────┴────────── UDP 单路径 ─────────┘
```

星形拓扑：所有流量经服务器中转。客户端之间不直连。
后续可扩展 P2P 直连（UDP 打洞），当前版本不实现，但架构上预留：transport 层抽象为 Peer 接口，未来加 P2P 路径只需新增一种 Peer 实现，不影响上层。

---

## 技术选型

| 项 | 选择 | 理由 |
|---|---|---|
| 语言 | Go | 交叉编译方便、TUN 库成熟、单二进制、engarde 参考代码可复用 |
| TUN 库 | `golang.zx2c4.com/wireguard/tun` | 跨平台（Linux/Windows/macOS）、久经考验、仅 TUN 抽象不依赖 WireGuard 协议 |
| 日志 | `github.com/sirupsen/logrus` | 同 engarde |
| 配置 | `gopkg.in/yaml.v3` | 同 engarde，升级到 v3 |
| 二进制 | 单二进制 | `mpfpv --mode server` 或 `mpfpv --mode client`，配置决定模式 |

---

## 协议设计

UDP 封装头 **8 字节**（4 字节对齐，避免 ARM 非对齐访问性能惩罚）：

```
Byte 0:     Flags (高4位=版本v1, 低4位=类型)
Byte 1:     Reserved (高7位保留, 最低1位=priority, v1暂不使用, 为未来按优先级分策略预留)
Bytes 2-3:  Client ID (uint16, 最多65535个客户端; 服务器自身使用 clientID=0)
Bytes 4-7:  Sequence Number (uint32, 每个 clientID 独立递增, 用于去重)
Bytes 8-N:  Payload (原始 IP 包)
```

### 消息类型（Flags 低 4 位）
- `0x00` **Data** — IP 数据包
- `0x01` **Heartbeat** — 心跳（合并了原 Register 和 Keepalive）
- `0x02` **HeartbeatAck** — 服务器回复心跳（可携带分配的虚拟 IP）

### Heartbeat payload 格式（固定 16 字节，4 字节对齐）
```
Bytes 0-3:   Virtual IP (4 字节 IPv4, 网络字节序; 自动分配模式下首次填 0.0.0.0)
Byte 4:      Prefix Length (uint8, 如 24; 自动分配模式下填 0)
Byte 5:      Send Mode (uint8, 0x00=redundant, 0x01=failover)
Bytes 6-7:   Reserved (2 字节, 置零)
Bytes 8-15:  Team Key Hash (8 字节, teamKey 的前 8 字节 SHA-256 摘要, 用于配对校验)
```

### HeartbeatAck payload 格式（固定 8 字节）
```
Bytes 0-3:  Assigned Virtual IP (4 字节, 静态模式下回显客户端 IP, 自动分配模式下为服务器分配的 IP)
Byte 4:     Prefix Length (uint8)
Byte 5:     Status (uint8, 0x00=OK, 0x01=teamKey 不匹配, 0x02=clientID 冲突)
Bytes 6-7:  Reserved (2 字节, 置零)
```

### 配对机制
客户端和服务器通过 **teamKey** 配对：
- 两端配置文件中设置相同的 `teamKey: "myteam2024"`
- 客户端在 Heartbeat 中发送 teamKey 的 SHA-256 前 8 字节
- 服务器校验匹配后才接受该客户端，不匹配则回 Status=0x01 拒绝
- 不是加密认证，只防误连（符合"不需要加密"的需求）

### Heartbeat 设计要点
- 服务器收到 Heartbeat 时，先校验 teamKey，再检查 clientID：
  - teamKey 不匹配 → 拒绝，HeartbeatAck Status=0x01
  - clientID 已被另一个 virtualIP 注册（静态模式下两设备配了相同 clientID）→ 拒绝，HeartbeatAck Status=0x02，日志告警
  - virtualIP 为 0.0.0.0 → 自动分配模式，服务器分配 IP 并回 HeartbeatAck
  - 正常 → 创建/更新会话和路由表
- 服务器重启后最多 1 秒自动恢复所有路由（下一次 Heartbeat 到达）。
- **路由表只从 Heartbeat 学习**，不从 Data 包的内层 IP 隐式学习。避免配置错误的客户端污染路由表。
- 收到 Data 包时，如果源 clientID 的内层 IP 源地址与注册的 virtualIP 不匹配，丢弃并告警。

### 序列号规则
- 每个发送方（每个 clientID）维护自己的递增 seq 计数器，从 0 开始
- 服务器转发数据包时**不改** seq，原样转发
- 服务器自身发出的包（HeartbeatAck、服务器 TUN 产生的流量）使用 clientID=0 和独立的 seq 计数器

### 去重
接收端维护 per-clientID 的滑动窗口位图，默认 **4096** 包（可配置）。
- 5Mbps 视频 ≈ 520 pkt/s，2 网卡冗余 = 1040 pkt/s，4096 窗口覆盖 ~3.9 秒
- 覆盖 5G 切基站时的延迟飙升场景（即使 3 卡 1560 pkt/s 也有 ~2.6 秒余量）

### 两层去重
- **服务器收上行包时去重**：无人机 N 张网卡各发 1 份（通常 N=2），服务器去重后只保留 1 份写入 TUN / 转发给目标客户端
- **客户端收下行包时去重**：服务器冗余回包给客户端 N 个地址，客户端去重后只写 1 份到 TUN

去重位图按 clientID 维度隔离（每个 clientID 有独立的 seq 空间）。

---

## 多路径策略

两种模式，用户在配置文件中全局选择，**不按服务/端口区分**：

1. **redundant**（冗余模式）：每个包通过所有网卡发送，最大可靠性，带宽 = 流量 × 网卡数
2. **failover**（故障转移模式）：走最优单网卡，故障时自动切换

接口发现：Linux 优先 netlink 事件驱动，兜底轮询 `net.Interfaces()`（间隔 200ms）。Linux 上用 `SO_BINDTODEVICE` 绑定接口，Windows/macOS 用地址绑定。

### 服务器回包策略
服务器回包跟随客户端模式（客户端通过 Heartbeat 上报 sendMode）：
- 客户端 redundant → 服务器向该客户端所有已知源地址各发一份
- 客户端 failover → 服务器只向最近活跃的地址发

### Failover 最优网卡判定
- **主要指标**：每个网卡最近 N 次心跳的 RTT 滑动平均值
- **辅助指标**：心跳丢失率（连续 K 次心跳无回应标记为 down）
- **切换条件**：当前网卡 RTT 均值连续超过次优网卡的阈值倍数时切换
- **防乒乓**：切换后至少保持 T 秒不切回，除非新网卡也 down
- 具体参数（N、K、T、阈值）实现时根据实测调整

### 网卡发送失败处理
发包时某个 socket 写失败：静默跳过继续发其他网卡，标记该网卡为 suspect。下次轮询周期验证网卡状态，不可用则移除。不因单个网卡报错阻塞整条发送链路（复用 engarde 的 `toDelete` 模式）。

---

## 客户端与服务器职责

### 客户端（极简）
- TUN 读出的所有包 → 一律封装发给服务器
- 从服务器收到的所有包 → 去重后一律写入 TUN
- **不维护路由表**，不需要知道虚拟网络中有哪些其他节点

### 服务器
- 维护路由表：`虚拟IP → clientID`
- 维护客户端地址表：`clientID → {[]源地址, sendMode, lastSeen}`
- 收到上行数据包：去重 → 校验内层 IP 源地址与注册 virtualIP 一致（不一致则丢弃+告警）→ 读 IP 头目标地址 → 查路由表 → 转发给目标 clientID（或写入自己的 TUN）
- 收到下行 TUN 包：读 IP 头目标地址 → 查路由表 → 封装发给目标 clientID
- 超时清理（两级）：
  - **addrTimeout**（默认 5 秒）：单个 srcAddr 超时移除，不再向其回包。但只要还有活跃地址就不删除 clientID 会话
  - **clientTimeout**（默认 15 秒）：clientID 所有地址均无活动 → 删除整个会话（源地址列表 + 路由条目）；若使用自动 IP 分配，IP 映射保留 5 分钟避免短暂断网后重连拿到不同 IP
- 离线节点流量：v1 默默丢弃。后续改进可回复 ICMP Destination Unreachable

---

## 虚拟 IP 分配

支持两种方式：
- **静态**：配置文件写死 `virtualIP: "10.99.0.1/24"`
- **自动**（推荐）：客户端只需配 clientID 和 serverAddr，服务器根据 clientID 分配固定虚拟 IP，通过 HeartbeatAck 返回。分配表持久化到本地 JSON 文件，服务器重启后加载，同一 clientID 始终拿到相同 IP。
  - 分配逻辑完全在服务器端，客户端零开销
  - 新设备接入只需配一个 clientID，不用操心 IP 冲突

---

## 保活与重连

### 混合保活：数据包优先，心跳兜底
- **有数据流时**：数据包充当保活，服务器收到任何包（数据/心跳）都更新 clientID 的源地址映射和 lastSeen。零额外开销。
- **空闲时**：客户端在超过 N ms（默认 200ms，可配置）没有发过数据包时，才发一次 Heartbeat。避免空闲超时被踢。
- **网卡变化时**：检测到新网卡/IP 变化，立即通过新网卡发一次 Heartbeat（让服务器即时学到新地址）。
- **首次连接 / 服务器重启恢复**：Heartbeat 携带 virtualIP + sendMode，服务器从 Heartbeat 重建路由表。

### Failover 模式下的心跳特殊规则
Failover 模式下数据包只走最优网卡，非活跃网卡没有数据流经过，无法探测 RTT。因此：
- **心跳始终通过所有网卡发送**（每秒一次），不受"空闲才发心跳"的限制
- 只有 **Data 包**遵循"只走最优网卡"的规则
- 心跳包很小（8+8=16 字节/网卡/秒），开销可忽略
- 这确保了非活跃网卡始终有 RTT 数据，切换时能可靠选出次优网卡

### 网卡变化检测
- **Linux**：优先用 netlink 事件驱动（`RTM_NEWADDR`/`RTM_DELADDR`），检测延迟 < 10ms
- **无 netlink 时**：兜底轮询 `net.Interfaces()`，间隔 200ms（可配置）。有 netlink 时不启动轮询 goroutine

### 重连时间
- 有 netlink + 有数据流：检测 ~0ms + 下一个数据包从新 IP 发出 ≈ **< 10ms**
- 有 netlink + 空闲：检测 ~0ms + 立即发心跳 + 1 RTT ≈ **10-30ms**
- 无 netlink（轮询兜底）：最坏 200ms + 1 RTT ≈ **200-230ms**
- **无握手、无状态机**，IP 变化后流量自动从新地址发出

---

## 并发模型

### 服务器 goroutine 结构
- G1: UDP 收包循环（收上行 → 去重 → 路由转发）
- G2: TUN 读循环（读本地包 → 查路由 → 封装下发）
- G3: 超时清理定时器
- 可选: Heartbeat 处理（可复用 G1）

### 客户端 goroutine 结构
- G1: TUN 读循环 → 封装 → 多网卡发送
- G2: UDP 收包循环 → 去重 → 写 TUN
- G3: 网卡发现（netlink 事件驱动 / 200ms 轮询兜底）
- G4: Heartbeat 定时发送（空闲兜底 + failover 下所有网卡定时探测）
- GN: 每网卡一个接收 goroutine（同 engarde 的 `wgWriteBack`）

### 共享数据并发控制
- 路由表、客户端地址表：`sync.RWMutex`（读多写少）
- 去重位图：per-clientID 独立实例，无锁竞争
- 网卡列表 `sendingChannels`：`sync.RWMutex`（同 engarde）

### Buffer 池
从 Phase 2 起引入 `sync.Pool` 复用包缓冲区（MTU 大小），避免高流量视频场景下 GC 延迟毛刺。

---

## MTU

默认 **1300**，可在每个节点配置文件中独立配置。服务器转发时不关心 MTU（原样转发 UDP 包），不需要两端一致。

---

## Web 管理页面

通过配置 `webUI: "127.0.0.1:9801"` 开启。单个 HTML 文件用 `go:embed` 打进二进制，原生 JS 轮询 JSON API，不需要前端构建工具。

### 客户端 Web UI
- **配置**：切换 sendMode（冗余/故障转移），运行时临时禁用/启用某张网卡，不需要重启
- **状态**：连接状态、虚拟 IP、clientID、当前 sendMode
- **网卡列表**：名称、物理 IP、状态（active/suspect/down）、RTT、failover 下标注当前活跃卡

### 服务端 Web UI
- **客户端列表**：clientID、虚拟 IP、sendMode、在线/离线、最后活跃时间、源地址数量
- **操作**：删除某个客户端的 IP 分配（下次连接重新分配）
- **路由表**：虚拟 IP → clientID 映射，方便排查问题

### JSON API（客户端）
- `GET /api/status` — 连接状态、虚拟 IP、sendMode
- `GET /api/interfaces` — 网卡列表及状态
- `POST /api/interfaces/{name}/disable` / `enable` — 临时禁用/启用网卡
- `POST /api/sendmode` — 切换 sendMode

### JSON API（服务端）
- `GET /api/clients` — 所有客户端列表及状态
- `GET /api/clients/{id}` — 单个客户端详情（源地址列表）
- `DELETE /api/clients/{id}` — 删除客户端 IP 分配
- `GET /api/routes` — 路由表

---

## 项目结构

```
mpfpv/
├── cmd/
│   └── mpfpv/
│       └── main.go                # 入口，按 mode 分发
├── internal/
│   ├── client/
│   │   └── client.go              # 客户端主逻辑
│   ├── server/
│   │   └── server.go              # 服务端主逻辑
│   ├── tunnel/
│   │   ├── tun.go                 # TUN 接口定义
│   │   ├── tun_linux.go           # Linux TUN 实现
│   │   ├── tun_windows.go         # Windows TUN (wintun)
│   │   └── tun_darwin.go          # macOS TUN (utun)
│   ├── transport/
│   │   ├── multipath.go           # 多网卡发送（复用 engarde 模式）
│   │   ├── receiver.go            # 接收 + 去重
│   │   ├── iface.go               # 网卡发现
│   │   ├── iface_bindtodevice.go  # Linux SO_BINDTODEVICE
│   │   └── iface_generic.go       # Windows/macOS 地址绑定
│   ├── protocol/
│   │   ├── packet.go              # 头编解码
│   │   ├── dedup.go               # 滑动窗口去重
│   │   └── types.go               # 常量和类型
│   ├── web/
│   │   ├── handler.go             # HTTP handler + JSON API
│   │   └── static/
│   │       └── index.html         # 嵌入式单页 HTML (go:embed)
│   └── config/
│       └── config.go              # YAML 配置解析
├── go.mod
├── Makefile                       # 交叉编译
├── mpfpv.yml.sample               # 示例配置
└── 可参考项目/engarde/             # 参考代码（已有）
```

---

## 示例配置

```yaml
mode: client
teamKey: "myteam2024"              # 与服务器配对，防误连

client:
  clientID: 1
  virtualIP: "10.99.0.1/24"        # 留空则由服务器自动分配
  serverAddr: "203.0.113.1:9800"
  sendMode: redundant              # redundant | failover
  mtu: 1300                        # TUN MTU，默认 1300
  dedupWindow: 4096                # 去重窗口大小，默认 4096
  excludedInterfaces:
    - "lo"
    - "docker0"
  webUI: "127.0.0.1:9801"          # 留空则不启用 Web 管理
```

```yaml
mode: server
teamKey: "myteam2024"              # 与客户端配对

server:
  listenAddr: "0.0.0.0:9800"
  virtualIP: "10.99.0.254/24"
  subnet: "10.99.0.0/24"           # 自动分配用的地址池
  clientTimeout: 15                # 秒，clientID 整体超时
  addrTimeout: 5                   # 秒，单个源地址超时
  dedupWindow: 4096
  mtu: 1300
  ipPoolFile: "ip_pool.json"       # 自动分配 IP 持久化文件
  webUI: "0.0.0.0:9801"            # 服务端 Web 管理
```

---

## 实施分阶段

### Phase 1：协议层 + 基础通信（无 TUN，纯 UDP 转发验证）

**公共模块：**
- Go module 初始化、项目骨架
- `protocol/types.go`：8 字节 header 常量定义
- `protocol/packet.go`：header 编解码（Encode/Decode，零拷贝写入预分配 buffer）
- `protocol/dedup.go`：滑动窗口去重（per-clientID，默认 4096，可配置）
- `config/config.go`：YAML 配置解析（客户端配置 + 服务器配置）
- `sync.Pool` buffer 池（MTU 大小预分配）

**服务器端 `server/server.go`：**
- 单端口 UDP 监听（`0.0.0.0:9800`）
- 收包主循环：解析 header → 按 clientID 分发
- Heartbeat 处理：校验 teamKey → 解析 virtualIP + sendMode → 创建/更新会话 → 回 HeartbeatAck
- 客户端会话管理：`clientID → {virtualIP, []srcAddr, sendMode, lastSeen}`
- 路由表：`virtualIP → clientID`
- 上行数据转发：去重 → 校验内层 IP → 读 IP 包目标地址 → 查路由表 → 封装发给目标客户端
- 超时清理：定时扫描，addrTimeout 清理单个地址，clientTimeout 清理整个会话

**客户端 `client/client.go`：**
- 单 socket 连接服务器
- Heartbeat 定时发送（携带 virtualIP + sendMode + teamKey hash）
- 等待 HeartbeatAck 确认注册成功
- 收包循环：解析 header → 去重 → 准备好给上层（Phase 2 接 TUN）
- 发包接口：接收上层数据 → 封装 header → 发送到服务器

**入口 `cmd/mpfpv/main.go`：**
- 解析配置文件和命令行参数
- 按 mode 启动 server 或 client

**验证**：启动 1 个 server + 2 个 client，用测试代码从 client A 发 UDP 数据，经 server 转发，client B 收到。验证 Heartbeat 注册、teamKey 校验、去重、超时清理均工作正常。

---

### Phase 2：TUN 集成（虚拟 IP 通信）

**公共模块：**
- `tunnel/tun.go`：TUN 设备接口定义（Read/Write/Close/Name）
- `tunnel/tun_linux.go`：Linux TUN 创建 + IP 配置（`ip addr add` + `ip link set up`）
- `tunnel/tun_windows.go`：Windows wintun 创建 + IP 配置（`netsh`）
- `tunnel/tun_darwin.go`：macOS utun 创建 + IP 配置（`ifconfig`）

**服务器端改动：**
- 启动时创建 TUN 设备，配置虚拟 IP（如 10.99.0.254/24）
- 新增 TUN 读循环 goroutine：TUN 读 IP 包 → 读目标地址 → 查路由表找 clientID → 封装 header → 按客户端 sendMode 发送
- 上行数据处理改动：去重后的 IP 包 → 判断目标地址：
  - 目标是服务器自己 → 写入服务器 TUN
  - 目标是其他客户端 → 查路由表转发
- 虚拟 IP 自动分配：客户端 Heartbeat 中 virtualIP 为 0.0.0.0 时，从子网池分配，通过 HeartbeatAck 返回

**客户端改动：**
- 启动时创建 TUN 设备，配置虚拟 IP
- 新增 TUN 读循环 goroutine：TUN 读 IP 包 → 封装 header → 发给服务器
- 收包处理改动：去重后的 IP 包 → 写入 TUN
- 自动分配模式：首次 HeartbeatAck 中拿到分配的 IP → 配置 TUN

**验证**：2 台 Linux 机器（或 1 台机器 + 1 个网络命名空间），分别运行 client，能 `ping` 通对方虚拟 IP。从一个 client 的虚拟 IP `curl` 另一个 client 上的 HTTP 服务。

---

### Phase 3：多网卡冗余

**客户端改动（主要工作在客户端）：**
- `transport/iface.go`：网卡发现
  - Linux：优先 netlink 事件驱动（即时检测）
  - 兜底：轮询 `net.Interfaces()`，间隔 200ms
  - 过滤排除列表、loopback、link-local
  - 检测新增/移除/IP 变化的网卡
- `transport/iface_bindtodevice.go`（Linux）：`SO_BINDTODEVICE` 绑定网卡，复用 engarde 的 syscall 实现
- `transport/iface_generic.go`（Windows/macOS）：地址绑定
- `transport/multipath.go`：多网卡发送器
  - 维护 `map[ifname]*Path`（每个网卡一个 UDP socket + 元数据）
  - redundant 模式：遍历所有 Path 发同一个包，单个 socket 写失败静默跳过+标记 suspect
  - failover 模式：选 RTT 最优的 Path 发送
  - RTT 探测：利用 Heartbeat/HeartbeatAck 计算每网卡 RTT 滑动平均
  - 切换逻辑：当前网卡 RTT 持续超标或 down → 切到次优，防乒乓保护
- 每个活跃网卡一个接收 goroutine（同 engarde 的 `wgWriteBack` 模式）：收包 → 去重 → 写 TUN
- Heartbeat 改为经所有网卡发送

**服务器端改动：**
- 客户端地址表扩展：同一个 clientID 可有多个 srcAddr
- 回包策略实现：
  - 客户端 sendMode=redundant → 向该客户端所有已知 srcAddr 各发一份
  - 客户端 sendMode=failover → 只向最近活跃的 srcAddr 发
- 地址超时清理：单个 srcAddr 超时移除，但只要还有活跃地址就不删除 clientID 会话

**验证**：
1. 客户端 2 张网卡，持续 `ping` 对端虚拟 IP，拔一张网卡，验证 0 丢包
2. 两张网卡都拔掉，验证流量中断；重新插上，验证 ≈1 秒恢复
3. failover 模式下，验证只走一张网卡，禁用后自动切换

---

### Phase 4：Web UI + 打磨与部署

**Web UI：**
- `web/handler.go`：JSON API 实现（客户端 API + 服务端 API）
- `web/static/index.html`：单页 HTML，原生 JS 轮询 API
- `go:embed` 打入二进制

**交叉编译：**
- `Makefile`：CGO_ENABLED=0 交叉编译目标
  - `linux-arm`（OpenIPC 无人机板）
  - `linux-arm64`（Radxa 等）
  - `linux-amd64`（云服务器）
  - `windows-amd64`（地面站）
  - `darwin-arm64`（macOS 地面站）

**服务器端：**
- IP 分配持久化（JSON 文件读写）
- 优雅退出：收到 SIGTERM/SIGINT → 停止收包 → 关闭 TUN → 退出
- 日志：客户端上下线、路由变更、异常事件

**客户端：**
- 优雅退出：关闭所有网卡 socket → 关闭 TUN → 退出
- 日志：网卡增删、Heartbeat 状态、模式切换事件

**公共：**
- 配置校验（IP 格式、clientID 范围、必填项检查）
- `-v` 版本输出（ldflags 注入）

**验证**：在实际目标硬件上部署测试
1. OpenIPC SS338：验证 TUN 可用性、二进制运行、多 5G 网卡冗余
2. 树莓派/Radxa：同上
3. Windows/macOS 地面站：验证 TUN 创建和基本通信
4. 端到端：无人机 WebRTC 视频流经虚拟 IP 到地面站，拔网卡测试无感切换

---

## 与 engarde 的关键差异

| | engarde | mpfpv |
|---|---|---|
| 虚拟 IP | 无（靠 WireGuard） | 内置 TUN |
| 客户端识别 | 按源 IP:port | 按 header 中的 clientID |
| 多客户端 | 每客户端一个端口 | 单端口复用 |
| 去重 | 无（靠 WireGuard） | 两层去重（服务器+客户端） |
| 服务器回包 | 广播所有地址 | 根据客户端 sendMode 决定 |
| 加密 | 无（靠 WireGuard） | 无（设计如此） |
| 协议开销 | 0 字节 | 8 字节 |
| 重连 | 继承 WireGuard | 无状态，≈1秒，服务器重启也自动恢复 |
| 二进制 | 2个 | 1个 |

---

## 关键参考文件

- `可参考项目/engarde/cmd/engarde-client/main.go` — 多网卡发现+转发核心逻辑
- `可参考项目/engarde/cmd/engarde-client/udpconn_bindtodevice.go` — SO_BINDTODEVICE 实现
- `可参考项目/engarde/cmd/engarde-server/main.go` — 服务端客户端追踪+回包广播
- `可参考项目/engarde/engarde.yml.sample` — 配置格式参考
- `可参考项目/engarde/Makefile` — 交叉编译参考

---

## 风险

1. **OpenIPC 无 TUN**：到硬件上验证 `ls /dev/net/tun`，如无则需重编内核加 `CONFIG_TUN=y`
2. **SO_BINDTODEVICE 需 root/CAP_NET_RAW**：无人机上通常 root 运行，地面站 Windows/macOS 用地址绑定不需特权
3. **NAT**：客户端主动连服务器，服务器有公网 IP，NAT 映射由客户端出站包自动建立，与 engarde 相同模型
