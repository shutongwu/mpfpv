# 2026-03-21 多路径冗余修复记录

## 背景

Windows 客户端（地面站）通过 mpfpv 隧道连接 radxa（无人机），经阿里云服务器中继。
Windows 报告心跳正常但数据不通，radxa 有双网卡（eth0 + USB 4G）但拔网卡时视频卡顿 2-3 秒。

## 修复的问题

### 1. Windows 客户端数据包被服务器丢弃（源 IP 不匹配）

**现象**: 心跳正常，ping 不通。

**根因**: Windows 上有 Clash Meta TUN 适配器，导致经过 mpfpv0 TUN 的包源 IP 被 OS 选为 `198.18.0.1` 而非虚拟 IP `10.99.0.2`。服务器 srcIP 校验不通过。

**修复**: 客户端 TUN 读包后、发送前强制改写内部 IP 包的源地址为 virtualIP，并重算 IPv4 checksum。

**提交**: `470a011`

### 2. 客户端重连后数据包被 dedup 误判

**现象**: 客户端重启后心跳正常但数据不通。

**根因**: 客户端重连后 seq 从 0 重新计数，但服务器和其他客户端的 dedup 窗口保留了旧 maxSeq（几千），新数据包被判为"太旧"或"重复"丢弃。心跳不经过 dedup 所以不受影响。

**修复**:
- 服务器: session 超时移除时调用 `dedup.Reset(clientID)` 清除去重状态（`470a011`）
- dedup 通用修复: seq 回退超过窗口大小时视为发送端重启，自动 reset bitmap（`1326af4`）

### 3. 拔网卡时视频卡顿 2-3 秒（故障转移而非冗余）

**现象**: 双网卡 redundant 模式下拔掉一个网卡，视频卡顿 2-3 秒后恢复。表现像故障转移而非冗余。

**根因分析过程**:

| 尝试 | 方案 | 结果 | 原因 |
|------|------|------|------|
| 1 | recvLoop 重试不退出 | 仍卡顿 | 问题不在接收 |
| 2 | 中央接收 socket（收发分离） | NAT 环境收不到包 | 中央 socket 无 NAT 映射 |
| 3 | 保留 per-NIC recvLoop + 中央 socket | 仍卡顿 | 问题在发送方向 |
| 4 | 并行 goroutine 发送 | 延迟尖刺 1781ms | per-packet goroutine/channel/GC 开销太大 |
| 5 | 串行发送（仿 engarde） | 仍卡顿 2-3s | WriteToUDP 在死网卡上阻塞 |
| 6 | SO_SNDTIMEO 50ms | 仍卡 50ms | 治标不治本 |
| 7 | removePath 先关 socket 再拿锁 | `ip link down` 仍丢包 | 内核移除接口影响全局路由 |
| 8 | **每网卡独立发送 goroutine + channel** | **通过** | 彻底解耦 |

**最终方案**: 每个网卡一个常驻 `pathSendLoop` goroutine，`sendRedundantLocked` 只往各 path 的 `sendCh` 写包（非阻塞，满则丢弃）。任何一个网卡的 `WriteToUDP` 阻塞只影响自己的 goroutine，其他网卡完全独立不受影响。

**提交**: `8a578b0`

### 4. 协议扩展: ReplyPort（中央接收 socket）

心跳 payload 的 Reserved 2 字节改为 `ReplyPort`，客户端创建一个不绑网卡的中央 UDP socket 用于接收。服务器回包时同时发到原端口（NAT 兼容）和 ReplyPort。拔网卡时中央 socket 不受影响。

**提交**: `72c46f2`, `43d164f`

### 5. SO_SNDTIMEO 安全网

`createBoundUDPConn` 创建 socket 时设置 `SO_SNDTIMEO=50ms`，防止极端情况下 `WriteToUDP` 长时间阻塞。

**提交**: `445cccf`

## 架构对比: engarde vs mpfpv

### 发送方向

| | engarde | mpfpv (修复后) |
|---|---|---|
| 发送模式 | 主线程串行 WriteToUDP | 每网卡独立 goroutine + sendCh |
| 阻塞影响 | 死网卡阻塞影响后续网卡 | 各网卡完全隔离 |
| Per-packet 开销 | 零 | channel 写入（非阻塞） |

### 接收方向

| | engarde | mpfpv (修复后) |
|---|---|---|
| 接收 socket | per-NIC | per-NIC + 中央 socket |
| NIC 移除影响 | 只影响被移除的 NIC | per-NIC recvLoop 重试 + 中央 socket 兜底 |

### 接口管理

| | engarde | mpfpv (修复后) |
|---|---|---|
| 检测方式 | 1s 轮询 | 200ms 轮询 |
| 移除操作 | 先关 socket 再删 map | 同（先关 socket 解除阻塞） |

## 测试结果

使用 `test/failover_test.sh degrade`（tc netem 模拟网络劣化）:

| 测试场景 | 丢包率 | 延迟 |
|---------|--------|------|
| 双网卡正常 | 0% | 19ms |
| 一个网卡 100% 丢包 | 0% | 34ms |
| 一个网卡 500ms 延迟 + 50% 丢包 | 0% | 32ms |
| 双网卡各 30% 丢包 | 0% | 22ms |
| 轮流 100% 丢包 × 6 次 | 0% (60/60) | 26ms |

## 完整提交列表

```
41f6335 test: 添加多路径冗余测试脚本（failover/degrade/stress）
8a578b0 feat: 每网卡独立发送 goroutine，彻底解耦多路径发送
445cccf fix: removePath 先关 socket 再拿锁，和 engarde 一致
ee3fba1 fix: redundant 发送改回串行零分配
7a9ee34 fix: redundant 模式并行发送，避免死网卡阻塞
43d164f fix: 保留 per-NIC recvLoop 兼容 NAT
72c46f2 feat: 收发分离，加中央接收 socket
7b41e83 fix: recvLoop 瞬断时重试而非退出
1326af4 fix: dedup 检测到 seq 回退超过窗口大小时自动 reset
470a011 fix: 修复客户端重连后数据包不通的两个问题
```
