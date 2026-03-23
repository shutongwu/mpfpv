# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目背景

本目录用于在 RunCam WiFiLink 2（OpenIPC 固件）上部署 mpfpv 组网软件，替代原有的 WFB-ng WiFi 广播方案，改用双 4G USB 网卡 + mpfpv 多路径冗余通过服务器中继传输视频和遥控数据。

## 硬件信息

| 项目 | 值 |
|------|-----|
| 设备 | RunCam WiFiLink 2（空中端） |
| SoC | SigmaStar SSC338Q，双核 Cortex-A7 |
| 架构 | armv7l |
| 内存 | 91MB（视频编码后可用 ~65MB） |
| Flash | 8MB SPI NOR（overlay 5.4MB 可用） |
| SD 卡 | 16GB，挂载在 /mnt/sd，用于存放 mpfpv 等额外软件 |
| 内核 | Linux 4.9.84 |
| 固件 | OpenIPC（基于 Buildroot，BusyBox init，无 systemd） |
| SSH | root@192.168.1.190，密码 12345，Dropbear |
| Web UI | http://192.168.1.190（root / 12345） |
| WiFi | 已拆除，改用 USB Hub 接 4G 网卡 |

## Build & Deploy

```bash
# 构建 ARM32 二进制（从父目录执行）
cd .. && make client-arm32
# 产物：build/client/arm/mpfpv-linux-arm

# SSH 访问设备
sshpass -p '12345' ssh -o StrictHostKeyChecking=no root@192.168.1.190

# 部署二进制到设备 SD 卡
sshpass -p '12345' scp -O -o StrictHostKeyChecking=no \
  ../build/client/arm/mpfpv-linux-arm root@192.168.1.190:/mnt/sd/mpfpv/mpfpv

# 远程执行命令
sshpass -p '12345' ssh -o StrictHostKeyChecking=no root@192.168.1.190 "<命令>"
```

## 设备上的目录结构

```
/mnt/sd/mpfpv/           # SD 卡，主工作目录
  ├── mpfpv              # ARM32 二进制
  └── mpfpv.yml          # 客户端配置
/etc/init.d/             # BusyBox init 启动脚本位置
```

## 当前状态

- **majestic**（视频编码）：运行中，使用硬件 ISP，CPU 占用极低
- **wfb_tx/wfb_rx/wfb_tun**：WFB-ng 进程，WiFi 已拆无用，需关闭
- **msposd**：OSD 叠加（MAVLink 串口），按需保留
- **TUN 驱动**：/dev/net/tun 存在，可用
- **USB**：Bus 001，已识别 USB Hub

## 目标

1. 关闭 WFB-ng 相关服务（S98wifibroadcast、S98vtun）
2. 配置 4G USB 网卡驱动（RNDIS/CDC-ECM），确认内核模块是否存在
3. 部署 mpfpv ARM32 客户端到 SD 卡（/mnt/sd/mpfpv/）
4. 配置 majestic RTP 推流到 mpfpv 虚拟 IP
5. MAVLink 透传（串口 → UDP → mpfpv 隧道）
6. CRSF 遥控信号网络传输
7. 编写 init.d 启动脚本（无 systemd，用 BusyBox init）

## 关键约束

- **Flash 空间极小**（5.4MB），所有大文件放 SD 卡（/mnt/sd/）
- **无 systemd**，启动脚本放 /etc/init.d/，用 BusyBox init 语法
- **无包管理器**（无 apt/opkg），内核模块需手动部署或交叉编译
- **不要动 majestic 配置**，除非视频推流目标地址必须修改
- **设备 IP 可能变化**（DHCP），操作前先确认 192.168.1.190 是否可达
- mpfpv 服务器：cloudfpv.top:9800（家庭 PVE VM 192.168.1.197）
