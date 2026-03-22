# 2026-03-21 多路径冗余修复记录

## 背景

mpfpv 星形拓扑组网：Windows 地面站客户端 / radxa 无人机客户端，经阿里云服务器 (114.55.58.24) 中继通信。本次修复涉及三个核心问题：Windows 数据不通、拔网卡卡顿、收发耦合。

---

## 问题 1：Windows 客户端心跳正常但数据不通

### 现象

Windows 客户端 `heartbeat sent` 日志正常，服务器端能收到心跳并建立 session，但 `ping 10.99.0.x` 始终不通。

### 根因分析

**根因 1 — 源 IP 被 Clash Meta TUN 篡改：**

Windows 上同时运行了 Clash Meta 的 TUN 模式，其适配器持有 `198.18.0.1` 的 fake-ip 地址。操作系统为 mpfpv TUN（mpfpv0）选择源 IP 时，错误选中了 Clash 的 `198.18.0.1` 而非 mpfpv 的虚拟 IP `10.99.0.2`。服务器收到数据包后做 srcIP 校验（验证内层 IP 包源地址是否匹配 session 注册的 virtualIP），校验不通过直接丢弃。

**根因 2 — 客户端重连后 seq 被 dedup 误判为重复：**

客户端 session 超时后重连，seq 从 0 重新计数。但服务器端的 dedup 滑动窗口仍保留旧的 maxSeq（可能已经到了几千），新数据包的 seq=0/1/2... 被判定为"太旧"直接丢弃。心跳走独立的 Type 字段不经过 dedup，因此心跳不受影响——这解释了"心跳正常但数据不通"的诡异现象。

### 修复方案

1. **客户端强制改写源 IP**：TUN 读出包后、封装发送前，将内层 IP 包的源地址强制覆盖为 `virtualIP`，并重新计算 IPv4 header checksum。无论 OS 选了什么源 IP，发出去的包一定是正确的。

2. **服务器 session 移除时重置 dedup**：`removeSession()` 中调用 `dedup.Reset(clientID)`，清除该客户端的去重窗口状态。客户端重连时从 seq=0 开始不会被误判。

### 涉及文件

- `internal/client/client.go` — 源 IP 改写 + checksum 重算
- `internal/protocol/dedup.go` — 添加 `Reset(clientID)` 方法
- `internal/server/server.go` — session 移除时调用 dedup.Reset

---

## 问题 2：拔网卡后视频流卡顿 2-3 秒

### 现象

radxa 双网卡（eth0 + USB 4G）运行 redundant 模式，拔掉其中一个网卡后，视频流卡顿 2-3 秒才恢复。本应是冗余发送（任一网卡可用即不中断），实际表现却像故障转移。

### 排查过程

1. **最初发现锁顺序问题**：`removePath` 先拿 `WLock` 再关 socket，而 `sendRedundantLocked` 持有 `RLock` 调用 `WriteToUDP`。拔网卡时 `WriteToUDP` 可能阻塞，持有 RLock 不释放；`removePath` 等 WLock 等不到——形成死锁链。
   - **修复**：和 engarde 保持一致，先关 socket（`conn.Close()` 会使阻塞的 `WriteToUDP` 立刻返回 error）再拿锁。

2. **仍然卡顿**：发现 `sendCh`（容量 256）队列中积压了大量旧包。一个网卡故障期间，待发队列堆满；网卡恢复后需要先把积压的旧包全部发完，新数据才能进入队列——造成延迟尖刺。

3. **引入 per-path 独立发送 goroutine**：每个网卡一个常驻 `pathSendLoop` goroutine，`sendRedundantLocked` 只往各 path 的 `sendCh` 非阻塞写入（满则丢弃）。任何一个网卡的 `WriteToUDP` 阻塞只影响自己的 goroutine，其他网卡完全独立。

4. **最终方案**：去掉 sendCh 和 per-path goroutine，改为串行直接 `WriteToUDP`。

### 最终方案原理

实际场景中"拔网卡"有两种情况：

- **信号差但网卡还在**：`WriteToUDP` 是非阻塞的（UDP 只写入内核缓冲区就返回），不会卡住。
- **物理拔除网卡**：`SO_BINDTODEVICE` 绑定的 socket 调用 `WriteToUDP` 时，内核立刻返回 `ENODEV`，同样不阻塞。

因此串行发送就够了，每个 `WriteToUDP` 调用都是微秒级返回。加 `SO_SNDTIMEO=50ms` 作为兜底安全网，防止极端情况下的阻塞。

### 涉及文件

- `internal/transport/multipath.go` — 发送逻辑重构、SO_SNDTIMEO、removePath 锁顺序修复

---

## 问题 3：收发分离

### 问题

原来每个网卡的 UDP socket 同时负责收和发。拔掉网卡时，该 socket 的 `recvLoop` 也跟着断掉，导致从该路径返回的数据包丢失。

### 修复方案

引入**中央接收 socket**（不绑定任何网卡），专门用于接收服务器回包：

1. 客户端创建一个不绑网卡的 UDP socket，监听在 `ReplyPort` 上。
2. 心跳 payload 的 Reserved 2 字节复用为 `ReplyPort` 字段，告知服务器。
3. 服务器回包时**双发**：
   - 发到原来的源端口（保持 NAT 穿透兼容性）
   - 同时发到客户端的 `ReplyPort`（中央 socket）
4. 拔掉任何网卡，中央接收 socket 不受影响，回包始终能收到。

### 涉及文件

- `internal/transport/multipath.go` — 中央接收 socket 创建与管理
- `internal/client/client.go` — ReplyPort 注册、中央 socket 接收逻辑
- `internal/server/server.go` — 双发逻辑（原端口 + ReplyPort）

---

## 测试

### 测试脚本

编写了 `test/failover_test.sh`，支持以下模式：

| 模式 | 说明 |
|------|------|
| `status` | 查看当前多路径状态 |
| `ping` | 基础连通性测试 |
| `failover` | 完全禁用一个网卡，验证切换 |
| `single` | 单网卡基准测试 |
| `degrade` | 用 tc netem 模拟真实信号差场景（丢包/延迟），不改变接口状态 |
| `stress` | 压力测试 |

### 测试结果（degrade 模式）

使用 `tc netem` 模拟网络劣化，比 `ip link set down` 更接近真实场景：

| 测试场景 | 丢包率 | 延迟 |
|---------|--------|------|
| 双网卡正常 | 0% | ~19ms |
| 一个网卡 100% 丢包 | 0% | ~34ms |
| 一个网卡 500ms 延迟 + 50% 丢包 | 0% | ~32ms |
| 双网卡各 30% 丢包 | 0% | ~22ms |
| 轮流 100% 丢包 x6 次 | 0% (60/60) | ~26ms |

冗余发送效果符合预期：单网卡完全丢包时另一个无缝接管，双网卡各有丢包时冗余互补后实际零丢包。

---

## 部署

- **服务器**（114.55.58.24）：已更新并运行
- **radxa**（无人机端）：已通过 Tailscale 隧道更新
- **Windows 客户端**（地面站）：已更新

### 部署注意事项

通过 mpfpv 隧道 SSH 到远端设备更新 mpfpv 自身时，**不能直接 kill mpfpv 后再执行后续命令**——kill 的瞬间隧道断开，后续命令无法执行。必须使用原子命令：

```bash
# 正确做法：先上传新二进制，一条命令完成替换+重启
scp mpfpv-new radxa:/tmp/
ssh radxa 'mv /tmp/mpfpv-new /usr/local/bin/mpfpv && systemctl restart mpfpv'
```

---

## 经验教训

1. **通过隧道更新隧道程序本身**：必须用原子命令（mv + kill/restart 在同一条命令中），不能分步执行。
2. **测试网络故障用 tc netem**：`ip link set down` 会完全移除接口，与真实的"信号差"场景差距太大。`tc netem` 可以精确模拟丢包率、延迟、抖动，更接近真实无线环境。
3. **UDP WriteToUDP 的阻塞特性**：UDP socket 的 `WriteToUDP` 通常只写入内核发送缓冲区就返回，不等待对端确认。物理网卡移除时 `SO_BINDTODEVICE` socket 会立刻返回 `ENODEV`。因此串行发送在绝大多数场景下不会阻塞，不需要复杂的并发方案。
4. **dedup 窗口与重连的交互**：去重机制必须考虑发送端重启/重连导致 seq 回退的情况，否则重连后数据会被静默丢弃，而心跳正常会误导排查方向。
5. **Clash TUN 模式冲突**：Clash Meta 的 fake-ip TUN 模式（198.18.0.1）会影响其他 TUN 适配器的源 IP 选择，使用 mpfpv 时需要关闭 Clash 或为目标网段添加直连规则。
