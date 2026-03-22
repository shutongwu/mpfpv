# mpfpv 架构与核心逻辑文档

## 1. 项目概述

mpfpv 是一个纯用户态、单二进制的组网工具，用 Go 编写。核心场景是无人机与地面站通过云服务器中继通信。它将两个功能合并到一个程序中：

- **多路径 UDP 冗余**：类似 engarde，通过多张物理网卡同时发送相同数据包，对端去重，实现链路级容错。
- **TUN 虚拟 IP 组网**：类似 WireGuard 的虚拟网络层，通过 TUN 设备创建虚拟 IP 网络。

替代了原先需要同时部署 engarde + WireGuard 的方案。

**设计决策**：不加密。`teamKey` 仅用于配对校验（SHA-256 前 8 字节），不提供安全保证。

## 2. 架构总览

### 星形拓扑

所有流量经服务器中转，客户端之间不直连：

```
                      ┌──────────────────────┐
                      │      Cloud Server     │
                      │   UDP :9800 (relay)   │
                      │   TUN mpfpv0          │
                      │   10.99.0.254/16      │
                      └───┬──────┬──────┬─────┘
                          │      │      │
              ┌───────────┘      │      └───────────┐
              │                  │                  │
    ┌─────────┴─────────┐ ┌─────┴───────┐ ┌────────┴────────┐
    │  Linux Client      │ │ ARM Client  │ │ Windows Client  │
    │  (ubuntu-dev)      │ │ (radxa)     │ │ (ground station)│
    │  eth0 + wlan0      │ │ wlan0       │ │ Wi-Fi           │
    │  10.99.0.1         │ │ 10.99.0.3   │ │ 10.99.0.2       │
    └────────────────────┘ └─────────────┘ └─────────────────┘
```

### 数据流

```
发送方向：
[App] → TUN read → 封装 8B header → multipath Send → UDP WriteToUDP × N张网卡

服务器处理：
UDP recv → DecodeHeader → Dedup → srcIP 校验 → routeTable 查 dstIP → sendToClient

接收方向：
UDP recv (central socket / per-NIC socket) → Dedup → strip header → TUN write → [App]
```

### 组件关系

```
cmd/mpfpv/main.go
    │
    ├─ mode=server → server.New() → server.Run()
    │                    │
    │                    ├─ protocol (编解码/去重)
    │                    ├─ tunnel   (TUN 设备)
    │                    └─ web      (HTTP API)
    │
    └─ mode=client → client.New() → client.Run()
                         │
                         ├─ protocol  (编解码/去重)
                         ├─ tunnel    (TUN 设备)
                         ├─ transport (多路径发送器)
                         │     ├─ InterfaceWatcher (网卡发现)
                         │     ├─ createBoundUDPConn (SO_BINDTODEVICE)
                         │     └─ MultiPathSender (冗余/故障转移)
                         └─ web       (HTTP API)
```

## 3. 协议设计

### 3.1 8 字节 UDP 封装头

每个 UDP 包的前 8 字节是固定头，4 字节对齐（ARM 友好）：

```
Byte:  0        1        2    3       4    5    6    7
    ┌────────┬────────┬─────────────┬─────────────────────┐
    │ Flags  │Reserved│ ClientID    │ Sequence Number     │
    │V=1|Type│Priority│ (big-end)   │ (big-endian uint32) │
    └────────┴────────┴─────────────┴─────────────────────┘
```

- **Flags（Byte 0）**：高 4 位 = 版本号（固定 `0x10`），低 4 位 = 消息类型
  - `0x00` = Data（数据包）
  - `0x01` = Heartbeat（心跳）
  - `0x02` = HeartbeatAck（心跳响应）
- **Reserved（Byte 1）**：最低位 = Priority 标志（预留），其余为 0
- **ClientID（Bytes 2-3）**：uint16，客户端标识。服务器自身使用 `clientID=0`
- **Seq（Bytes 4-7）**：uint32，per-clientID 递增序列号，用于去重

编解码采用零拷贝直接操作 byte slice：

```go
func EncodeHeader(buf []byte, h *Header) {
    buf[0] = (h.Version & 0xF0) | (h.Type & 0x0F)
    buf[1] = 0
    if h.Priority { buf[1] = 0x01 }
    binary.BigEndian.PutUint16(buf[2:4], h.ClientID)
    binary.BigEndian.PutUint32(buf[4:8], h.Seq)
}
```

### 3.2 Heartbeat payload（固定 16 字节 + 可变长设备名）

```
Byte:  0  1  2  3   4        5         6    7      8 ─ 15       16+
    ┌──────────────┬────────┬─────────┬──────────┬────────────┬──────────┐
    │ VirtualIP(4) │PrefixL │SendMode │ReplyPort │TeamKeyHash │DeviceName│
    │              │ (1)    │ (1)     │ (2,BE)   │  (8 bytes) │(variable)│
    └──────────────┴────────┴─────────┴──────────┴────────────┴──────────┘
```

- **VirtualIP**：客户端请求的虚拟 IP。新客户端发送 `0.0.0.0` 表示请求自动分配
- **SendMode**：`0x00`=redundant，`0x01`=failover
- **ReplyPort**：中央接收 socket 端口号。服务器将回包同时发到源端口和此端口，确保数据能送达 NIC 无关的接收 socket
- **TeamKeyHash**：SHA-256(teamKey) 的前 8 字节
- **DeviceName**：UTF-8 字符串，16 字节之后的可变长部分（向后兼容，旧版客户端不发此字段）

### 3.3 HeartbeatAck payload（固定 8 字节）

```
Byte:  0  1  2  3   4        5       6    7
    ┌──────────────┬────────┬───────┬──────────┐
    │AssignedIP(4) │PrefixL │Status │Reserved  │
    └──────────────┴────────┴───────┴──────────┘
```

- **Status**：`0x00`=OK，`0x01`=TeamKey 不匹配，`0x02`=ClientID 冲突

### 3.4 TeamKeyHash 的作用

```go
func ComputeTeamKeyHash(teamKey string) [8]byte {
    sum := sha256.Sum256([]byte(teamKey))
    var out [8]byte
    copy(out[:], sum[:8])
    return out
}
```

不是加密，仅用于配对校验——防止不同"队伍"的设备误连到同一服务器。服务器收到 Heartbeat 后对比 hash，不匹配则回 `AckStatusTeamKeyMismatch` 并拒绝注册。

### 3.5 数据包完整生命周期

以客户端 A ping 客户端 B 为例：

```
1. 客户端 A 的应用层发出 ICMP echo → 操作系统路由到 TUN 设备 mpfpv0

2. tunReadLoop 从 TUN read 出原始 IP 包
   ├─ 检查 IPv4 版本 (buf[0]>>4 == 4)
   ├─ 改写内层源 IP 为自己的 virtualIP（防止 OS 选错源地址）
   ├─ 重算 IPv4 header checksum
   └─ 封装 8B header (Version1 | TypeData | clientID_A | seq++)

3. multipath.Send(packet)
   ├─ redundant: 串行 WriteToUDP 到每张活跃网卡的 bound socket
   └─ failover: 只写入 RTT 最优的一张网卡

4. 服务器 UDP recv loop 收包
   ├─ DecodeHeader → TypeData
   ├─ IsDuplicate(clientID_A, seq) → 若重复则丢弃
   ├─ 验证 session 存在 + 内层 srcIP == 注册的 virtualIP
   ├─ 读内层 dstIP → 查 routeTable → 找到 clientID_B
   └─ sendToClient(clientID_B, rawPacket)
       ├─ redundant: 写到 B 的所有已知源地址 + ReplyPort 地址
       └─ failover: 只写到 B 最近活跃的地址

5. 客户端 B 收包（central recv socket 或 per-NIC recv socket）
   ├─ DecodeHeader → TypeData
   ├─ IsDuplicate(clientID_A, seq) → 去重
   └─ tunDev.Write(payload) → 写入 TUN

6. 客户端 B 的操作系统从 TUN 收到 ICMP echo → 生成 ICMP reply → 反向走同样流程
```

## 4. 多路径冗余（核心）

### 4.1 网卡发现机制

采用**白名单模式**，只识别物理网卡：

```go
// Linux 物理网卡前缀
"eth", "enp", "ens", "eno", "enx",    // 有线
"wlan", "wlp", "wlx",                  // 无线
"usb"                                   // USB 网络适配器

// Windows 名称关键字
"Wi-Fi", "WiFi", "Wireless", "WLAN", "Ethernet", "以太网", "USB"

// macOS
"en*"
```

IP 过滤规则（排除非物理地址）：
- `169.254.0.0/16`：link-local
- `100.64.0.0/10`：CGNAT / Tailscale
- `10.99.0.0/16`：mpfpv 自身虚拟 IP 段

`InterfaceWatcher` 优先尝试 netlink 事件驱动（当前未实现，返回 false），回退到 200ms 轮询。每次扫描比较新旧集合，检测新增、移除和地址变更（地址变更视为 remove + add）。

### 4.2 SO_BINDTODEVICE 绑定

Linux 上使用 `SO_BINDTODEVICE` 将 socket 绑定到特定物理网卡，确保数据包走指定路径：

```go
func createBoundUDPConn(localAddr net.IP, ifaceName string) (*net.UDPConn, error) {
    s, _ := syscall.Socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP)
    syscall.SetsockoptInt(s, SOL_SOCKET, SO_REUSEADDR, 1)
    syscall.SetsockoptString(s, SOL_SOCKET, SO_BINDTODEVICE, ifaceName)
    // 关键：50ms 发送超时，防止拔网卡时 WriteToUDP 长时间阻塞
    syscall.SetsockoptTimeval(s, SOL_SOCKET, SO_SNDTIMEO, &Timeval{Sec: 0, Usec: 50000})
    syscall.Bind(s, &localSockAddr)
    // ...
}
```

`SO_SNDTIMEO = 50ms` 是关键设计：redundant 模式串行写入每张网卡，如果某张网卡拔掉了，没有发送超时会导致 `WriteToUDP` 阻塞数秒，拖慢其它健康网卡的发送。50ms 超时确保最坏情况下 N 张网卡的总延迟 = N × 50ms。

### 4.3 redundant vs failover 模式

**redundant（冗余）**：每个数据包通过所有活跃/可疑网卡各发一份。

```go
func (m *MultiPathSender) sendRedundantLocked(data []byte) error {
    sentCount := 0
    for _, p := range m.paths {
        if p.Status == PathDown { continue }  // 跳过已确认挂掉的
        if _, err := p.Conn.WriteToUDP(data, m.serverAddr); err != nil {
            p.Status = PathSuspect  // 写失败标记为可疑，但不立即放弃
            continue
        }
        sentCount++
    }
    if sentCount == 0 { return errors.New("all paths failed") }
    return nil
}
```

**failover（故障转移）**：只走 RTT 最优的一张网卡，故障时自动切换。

```go
func (m *MultiPathSender) sendFailoverLocked(data []byte) error {
    p := m.paths[m.activePath]
    if p == nil {
        // 没有活跃路径，找任意非 Down 的
        for _, pp := range m.paths { ... }
    }
    _, err := p.Conn.WriteToUDP(data, m.serverAddr)
    return err
}
```

心跳始终通过**所有网卡**发送（`SendAll`），无论何种模式，确保每条路径都被 RTT 探测。

### 4.4 串行直接 WriteToUDP 的设计决策

redundant 模式不用 channel 和 per-NIC goroutine 发送，而是在调用者 goroutine 内串行循环 `WriteToUDP`。原因：

1. **零分配**：不需要把每个包复制到 channel，消除 per-packet goroutine 和 GC 开销
2. **可控延迟**：`SO_SNDTIMEO=50ms` 限制了单张网卡的最大阻塞时间
3. **简单性**：不需要管理发送 goroutine 的生命周期和关闭顺序

### 4.5 收发分离：中央接收 socket + per-NIC 发送 socket

```
发送路径：
┌───────────┐   WriteToUDP    ┌─────────┐
│ per-NIC   │ ──────────────→ │ Server  │
│ bound     │  SO_BINDTODEVICE│         │
│ socket    │                 │         │
└───────────┘                 └─────────┘
  eth0:rand_port               :9800
  wlan0:rand_port

接收路径：
┌─────────┐   回包到源端口   ┌───────────┐
│ Server  │ ──────────────→ │ per-NIC   │  ← NAT 环境下必须走这个
│         │                 │ recv loop │
│         │  回包到 ReplyPort ┌───────────┐
│         │ ──────────────→ │ central   │  ← 非 NAT 环境或本地网络
│         │                 │ recv sock │
└─────────┘                 └───────────┘
                             0.0.0.0:recv_port
```

- **中央接收 socket**：`net.ListenUDP("udp4", 0.0.0.0:0)`，不绑定任何网卡。好处：网卡拔插不影响接收 socket，不会丢包。客户端在 Heartbeat 的 `ReplyPort` 字段告知服务器此端口。
- **per-NIC 接收**：每张网卡的发送 socket 也启动一个 `perPathRecvLoop`，用于接收 NAT 映射回来的包（NAT 只记住源端口，回包必须走同一端口）。
- 两个路径都把包送入同一个 `recvCh` channel，客户端统一处理。

### 4.6 RTT 跟踪和路径状态管理

每条路径有三个状态：

| 状态 | 含义 | 转换条件 |
|------|------|----------|
| `PathActive` | 健康 | 收到 HeartbeatAck 后 UpdateRTT |
| `PathSuspect` | 写失败 | WriteToUDP 返回 error |
| `PathDown` | 已断开 | 连续 5 次心跳无回应 |

RTT 计算：滑动窗口 10 个样本取平均值。

```go
func (m *MultiPathSender) UpdateRTT(ifaceName string, rtt time.Duration) {
    p.rttSamples = append(p.rttSamples, rtt)
    if len(p.rttSamples) > 10 {
        p.rttSamples = p.rttSamples[len(p.rttSamples)-10:]
    }
    p.RTT = average(p.rttSamples)
    p.missCount = 0
    p.Status = PathActive
}
```

Failover 路径选择有 **5 秒防乒乓冷却**：

```go
func (m *MultiPathSender) selectBestPathLocked() string {
    // 找 RTT 最小的非 Down 路径
    // 如果当前路径没 Down 且距上次切换不到 5 秒，保持不变
    if curStatus != PathDown && time.Since(m.lastSwitch) < 5*time.Second {
        return m.activePath  // 冷却期内不切换
    }
    // ...
}
```

### 4.7 拔网卡时的行为

`removePath` 的顺序至关重要——先关 socket 再拿写锁：

```go
func (m *MultiPathSender) removePath(name string) {
    // 1. 读锁查找 path
    m.mu.RLock()
    p := m.paths[name]
    m.mu.RUnlock()

    // 2. 先关 socket（unblock 在 sendRedundantLocked 中持有 RLock 的 WriteToUDP）
    close(p.closed)    // 通知 perPathRecvLoop 退出
    p.Conn.Close()     // 使 WriteToUDP 立即返回 error

    // 3. 再拿写锁删除
    m.mu.Lock()
    delete(m.paths, name)
    m.mu.Unlock()
}
```

如果顺序反过来（先拿写锁），会死锁：`sendRedundantLocked` 持有读锁等待 `WriteToUDP` 返回，`removePath` 等待写锁，互相阻塞。这个设计与 engarde 一致。

## 5. 去重机制

### 5.1 滑动窗口 bitmap 原理

每个 clientID 独立维护一个 bitmap（默认 4096 位 = 64 个 uint64）：

```
bitmap[seq % windowSize / 64] 的第 (seq % windowSize % 64) 位
```

核心逻辑：

```go
func (d *Deduplicator) IsDuplicate(clientID uint16, seq uint32) bool {
    cs := d.clients[clientID]

    diff := int64(seq) - int64(cs.maxSeq)  // 考虑 uint32 回绕

    if diff > 0 {
        // seq 在 maxSeq 前面 → 滑动窗口向前，清除被移出的旧位
        cs.maxSeq = seq
        cs.setBit(seq)
        return false  // 新包
    }

    if diff == 0 {
        return true  // maxSeq 的精确重复
    }

    // diff < 0: seq 在 maxSeq 后面
    if -diff >= windowSize {
        // 太远了，可能是发送方重启 → 重置状态
        cs.maxSeq = seq
        cs.setBit(seq)
        return false
    }

    // 检查 bitmap
    if cs.getBit(seq) { return true }  // 已见过
    cs.setBit(seq)
    return false  // 窗口内的乱序新包
}
```

### 5.2 per-clientID 去重

去重器按 clientID 隔离状态。每个客户端有独立的 `maxSeq` 和 bitmap，互不干扰。加锁粒度：整个 Deduplicator 一把 `sync.Mutex`（clientID map 的并发访问）。

### 5.3 seq 回退检测和自动 reset

当 `seq` 比 `maxSeq` 落后超过整个窗口大小时（`-diff >= windowSize`），判定为发送方重启。此时清空 bitmap、重置 `maxSeq`，避免客户端重启后所有包都被误判为重复。

服务端在删除超时 session 时也会调用 `d.dedup.Reset(clientID)` 主动清理，确保客户端重连后状态干净。

## 6. 服务端逻辑

### 6.1 session 管理

```go
type Server struct {
    sessions   map[uint16]*ClientSession  // clientID → session
    routeTable map[[4]byte]uint16         // virtualIP → clientID
}

type ClientSession struct {
    ClientID   uint16
    VirtualIP  net.IP
    SendMode   uint8
    ReplyPort  uint16
    DeviceName string
    Addrs      map[string]*AddrInfo  // "IP:Port" → {Addr, LastSeen}
    LastSeen   time.Time
}
```

一个客户端可以有多个源地址（多张网卡 = 多个 NAT 映射 = 多个 `IP:Port`）。服务器通过 Heartbeat 学习源地址，通过 Data 包更新 `LastSeen`。

### 6.2 IP 自动分配和持久化

客户端不配置 virtualIP，发送 `0.0.0.0`，由服务器从 subnet（如 `10.99.0.0/16`）中分配：

```go
func (s *Server) allocateIP(clientID uint16, deviceName string) (net.IP, uint8) {
    // 1. 已分配过 → 返回旧 IP
    if ip, ok := s.ipPool[clientID]; ok { return ip }

    // 2. 遍历 subnet，跳过已用 IP 和服务器自身 IP
    for offset := 1; offset < maxHosts; offset++ {
        candidate := baseIP + offset
        if !used[candidate] {
            s.ipPool[clientID] = candidate
            s.saveIPPool()  // 持久化到 ip_pool.json
            return candidate
        }
    }
}
```

持久化文件 `ip_pool.json` 格式：

```json
[
  {"clientID": 12345, "ip": "10.99.0.1", "name": "ubuntu-dev"},
  {"clientID": 23456, "ip": "10.99.0.2", "name": "DESKTOP-QE166QH"}
]
```

同一设备（同 clientID）重连后拿到同一 IP。clientID 由 `FNV-1a(hostname + machineID)` 生成，确保同一台机器永远一致。

### 6.3 路由表和数据转发

路由表是 `virtualIP → clientID` 的映射，在 session 创建时写入，session 超时时删除。

服务端收到 Data 包后的转发逻辑：

```
handleData:
  1. dedup → 重复包丢弃
  2. session 查找 → 未知 clientID 丢弃
  3. 内层 IP 校验 → srcIP 必须等于注册的 virtualIP（防伪造）
  4. 读 dstIP → 查 routeTable
     ├─ dstIP == serverVirtualIP → TUN write（发给服务器自己）
     ├─ dstIP 在 routeTable → sendToClient（转发给目标客户端）
     └─ 不在 routeTable → 丢弃
```

`sendToClient` 根据目标客户端的 `SendMode` 决定发送策略：
- redundant → 写到该客户端的所有已知源地址
- failover → 只写到最近活跃的地址

同时，如果客户端提供了 `ReplyPort`，每次发送都会额外往 `IP:ReplyPort` 再发一份（best-effort），确保中央接收 socket 也能收到。

### 6.4 超时清理

`cleanupLoop` 每 1 秒运行一次：

```
对每个 session:
  1. 遍历所有源地址，删除 LastSeen > addrTimeout(5s) 的
  2. 如果所有地址都删光了 AND session.LastSeen > clientTimeout(15s):
     ├─ 删除 routeTable 条目
     ├─ 删除 session
     └─ dedup.Reset(clientID)
```

两层超时的设计意图：
- **addrTimeout (5s)**：单个源地址超时快速清理（比如一张网卡拔掉），但不影响整个 session
- **clientTimeout (15s)**：所有地址都没活动才真正删除 session，给客户端足够的重连窗口

### 6.5 srcIP 校验

```go
var srcIP [4]byte
copy(srcIP[:], payload[12:16])  // 内层 IPv4 源地址

if srcIP != registeredIP {
    // 丢弃：防止客户端伪造源 IP 向其他客户端注入流量
    return
}
```

## 7. 客户端逻辑

### 7.1 TUN 读写循环

`tunReadLoop` 从 TUN 设备读取应用层发出的 IP 包，封装后发送：

```go
func (c *Client) tunReadLoop(ctx context.Context) {
    // 等待 TUN 就绪（auto-assign 模式：收到第一个 HeartbeatAck 后才创建 TUN）
    <-c.tunReady

    buf := make([]byte, mtu + HeaderSize)
    for {
        n := tunDev.Read(buf[HeaderSize:])    // 在 header 后面读 IP 包
        if buf[HeaderSize]>>4 != 4 { continue } // 只处理 IPv4

        // 改写源 IP 为 virtualIP（防止 OS 选错源地址）
        copy(buf[HeaderSize+12:HeaderSize+16], virtualIP)
        recalcIPv4Checksum(buf[HeaderSize:])

        // 封装 header
        EncodeHeader(buf, &Header{Version1, TypeData, clientID, seq++})

        // 发送
        multipath.Send(buf[:HeaderSize+n])
    }
}
```

收到数据包后写入 TUN：

```go
func (c *Client) handleData(hdr Header, payload []byte) {
    if c.dedup.IsDuplicate(hdr.ClientID, hdr.Seq) { return }
    pkt := make([]byte, len(payload))
    copy(pkt, payload)  // 必须拷贝，caller 的 buf 会被复用
    c.tunDev.Write(pkt)
}
```

### 7.2 心跳循环

每 1 秒发送一次心跳，第一次立即发送：

```go
func (c *Client) heartbeatLoop(ctx context.Context) {
    c.sendHeartbeat()  // 立即发第一次
    ticker := time.NewTicker(1 * time.Second)
    for {
        select {
        case <-ticker.C:
            c.sendHeartbeat()
        case <-ctx.Done():
            return
        }
    }
}
```

心跳通过 `multipath.SendAll()` 发送——所有网卡都发，确保每条路径被服务器记录、每条路径的 RTT 被测量。

收到 HeartbeatAck 后：
1. 更新 virtualIP 和 prefixLen
2. 首次收到 OK → 创建 TUN 设备，关闭 `tunReady` channel 唤醒 `tunReadLoop`
3. 在 multipath 模式下，根据响应到达的路径更新 RTT

### 7.3 源 IP 改写

```go
if len(vip4) == 4 && !vip4.Equal(net.IPv4zero) {
    copy(buf[HeaderSize+12:HeaderSize+16], vip4)
    recalcIPv4Checksum(buf[HeaderSize : HeaderSize+n])
}
```

Windows 上其它 TUN 适配器（如 Clash Meta 的 fake-ip）可能导致 OS 选择错误的源地址。客户端在发送前强制改写内层 IPv4 的源 IP 为自己的 virtualIP，并重算 checksum，确保服务端的 srcIP 校验通过。

### 7.4 多路径/单网卡模式

`client.Run()` 的模式选择逻辑：

```
if bindInterface != "" → 单网卡模式（Windows GUI）
    net.ListenUDP 绑定到指定网卡地址
elif serverAddr 非 loopback → 多路径模式
    transport.NewMultiPathSender → Start
else → 单 socket 模式
    net.ListenUDP(nil) 不绑定
```

### 7.5 clientID 生成

```go
func stableClientID(name string) uint16 {
    h := fnv.New32a()
    h.Write([]byte(name))        // hostname
    h.Write([]byte(machineID())) // /etc/machine-id 或 Windows 注册表 GUID
    return uint16(h.Sum32()%64900) + 100  // 范围 [100, 65000)
}
```

Linux 读 `/etc/machine-id`，Windows 读 `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`。确保即使 hostname 相同，不同机器也有不同 clientID。

## 8. 并发模型

### 服务端 goroutine

| Goroutine | 职责 | 阻塞点 |
|-----------|------|--------|
| main (G1) | UDP recv loop | `conn.ReadFromUDP` (1s timeout) |
| G2 | TUN read loop | `tunDev.Read` |
| G3 | cleanup timer | `time.Ticker` (1s) |

锁策略：
- `sessionsLock (sync.RWMutex)`：读锁用于 handleData（高频），写锁用于 handleHeartbeat 和 cleanup
- `routeLock (sync.RWMutex)`：读锁用于路由查找，写锁用于 session 创建/删除
- `ipPoolLock (sync.Mutex)`：仅 allocateIP 和 loadIPPool 使用
- `seq (atomic uint32)`：服务器自身发包的序列号

### 客户端 goroutine

| Goroutine | 职责 | 阻塞点 |
|-----------|------|--------|
| G1 | TUN read → multipath.Send | `tunDev.Read` |
| G2 | UDP recv → dedup → TUN write | `recvCh` channel 或 `conn.ReadFromUDP` |
| G3 | heartbeat timer | `time.Ticker` (1s) |
| G4 (per-NIC) | per-NIC recv loop | `p.Conn.ReadFromUDP` |
| G5 | central recv loop | `recvConn.ReadFromUDP` |
| G6 | InterfaceWatcher polling | `time.Ticker` (200ms) |

锁策略：
- `MultiPathSender.mu (sync.RWMutex)`：读锁用于 Send/SendAll（数据面热路径），写锁用于 addPath/removePath
- `Path.mu (sync.Mutex)`：保护单条路径的 Status/RTT/missCount
- `Client.mu (sync.Mutex)`：保护 virtualIP/prefixLen 的读写
- `seq (atomic uint32)`：per-client 序列号
- `registered (atomic int32)`：是否已注册的 bool 标志

### 关键并发安全点

1. **removePath 的锁顺序**：先 Close socket（无锁），再拿写锁删除 map 条目。避免与 sendRedundantLocked（持读锁调用 WriteToUDP）死锁。
2. **recvCh channel (256 buffer)**：所有接收 goroutine 向同一个 channel 写入，客户端 G2 统一消费。
3. **dedup 不需要 per-clientID 锁**：因为单 goroutine 消费 recvCh，IsDuplicate 调用是串行的（但 Deduplicator 内部有 mutex 以防万一）。

## 9. Web UI & API

### 9.1 架构

嵌入式单页 HTML（`go:embed static/index.html`），原生 JavaScript，2 秒轮询 API。页面自动检测模式：先探测 `/api/connection-status`（GUI 模式），再探测 `/api/status`（普通客户端），否则回退到服务端模式。

### 9.2 服务端 API

| 方法 | 端点 | 功能 |
|------|------|------|
| GET | `/api/clients` | 所有客户端列表 |
| GET | `/api/clients/{id}` | 单客户端详情（含源地址列表） |
| DELETE | `/api/clients/{id}` | 删除客户端 session |
| GET | `/api/routes` | 路由表 |
| GET | `/api/server-config` | 服务器配置（teamKey, listenAddr） |
| POST | `/api/server-config` | 修改服务器配置（保存到 YAML） |

### 9.3 客户端 API

| 方法 | 端点 | 功能 |
|------|------|------|
| GET | `/api/status` | 连接状态、virtualIP、clientID、sendMode |
| GET | `/api/interfaces` | 网卡列表（状态/RTT/是否 active） |
| POST | `/api/sendmode` | 切换 redundant/failover |
| POST | `/api/interfaces/{name}/enable` | 启用网卡 |
| POST | `/api/interfaces/{name}/disable` | 禁用网卡 |

### 9.4 GUI 控制 API（仅 Windows GUI）

| 方法 | 端点 | 功能 |
|------|------|------|
| GET/POST | `/api/config` | 读写配置 |
| POST | `/api/connect` | 连接服务器 |
| POST | `/api/disconnect` | 断开连接 |
| GET | `/api/connection-status` | 连接状态 |
| GET | `/api/available-interfaces` | 可用物理网卡列表 |

### 9.5 接口设计

Server 和 Client 分别实现 `ServerAPI` 和 `ClientAPI` 接口，Web handler 通过接口调用：

```go
type ServerAPI interface {
    GetClients() []ClientInfo
    GetClient(id uint16) *ClientDetailInfo
    DeleteClient(id uint16) error
    GetRoutes() []RouteEntry
}

type ClientAPI interface {
    GetStatus() StatusInfo
    GetInterfaces() []InterfaceStatus
    SetInterfaceEnabled(name string, enabled bool) error
    SetSendMode(mode string) error
}
```

`GUIController` 接口是可选的，由 Windows GUI 入口（`cmd/mpfpv-gui/`）注入，提供 connect/disconnect/config 操作。
