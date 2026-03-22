# mpfpv

无人机多路径冗余组网工具。通过云服务器/自建服务器中继，整合 UDP 多路径冗余和 TUN 虚拟 IP 组网，单二进制、纯用户态、零依赖。

## 特性

- **多路径冗余**：多张物理网卡同时发送相同数据包，对端去重，链路级容错
- **故障切换**：自动选择最优路径，故障秒级切换，5 秒防乒乓冷却
- **虚拟 IP 组网**：TUN 设备创建虚拟网络，应用层完全透明
- **星形拓扑**：所有流量经服务器中转，支持 NAT 穿透
- **Web 管理界面**：嵌入式 Web UI，实时查看设备状态、每网卡 RTT 和带宽
- **服务端 IP 管理**：自动分配虚拟 IP，支持在线编辑和持久化
- **MTU 自动同步**：服务端配置 MTU，客户端自动跟随
- **跨平台**：Linux (x86/ARM64/MIPS)、Windows (CLI + GUI)、macOS (桩)

## 快速开始

### 服务端

```bash
tar xzf mpfpv-server-*.tar.gz && cd mpfpv-server-*
sudo ./install.sh server
# 编辑 /opt/mpfpv/mpfpv.yml，设置 teamKey
sudo systemctl start mpfpv
```

### 客户端

```bash
tar xzf mpfpv-client-*.tar.gz && cd mpfpv-client-*
sudo ./install.sh client
# 编辑 /opt/mpfpv/mpfpv.yml，设置 teamKey 和 serverAddr
sudo systemctl start mpfpv
```

### 管理

```bash
sudo systemctl status mpfpv    # 查看状态
sudo journalctl -u mpfpv -f    # 查看日志
```

Web UI：`http://<IP>:9801`

## 构建

```bash
go build -o mpfpv ./cmd/mpfpv/   # 本地构建
make all                          # 交叉编译全部目标
go test ./...                     # 运行测试
```

## 架构

```
[App] → TUN → client (+8B header) → UDP multipath → [Server] → forward → [Client] → TUN → [App]
```

星形拓扑，所有流量经服务器中转。自定义 8 字节 UDP 封装头，滑动窗口去重。详见 [CLAUDE.md](CLAUDE.md)。

## 致谢

本项目的多路径发送设计受 [engarde](https://github.com/porech/engarde) 启发，感谢 engarde 项目提供的优秀参考实现。

## License

MIT
