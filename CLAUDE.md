# CLAUDE.md

## 项目简介

mpfpv — 无人机多路径冗余组网工具（Go），通过云服务器中继，整合 UDP 多路径冗余 + TUN 虚拟 IP。

详细架构见 `docs/architecture.md`。

## 构建 & 测试

```bash
go build -o mpfpv ./cmd/mpfpv/
make all          # 交叉编译全部目标
go test ./...     # 单元测试
```

## 部署

| 节点 | IP | 虚拟 IP | 说明 |
|------|-----|---------|------|
| 阿里云 ECS | 114.55.58.24 | 10.99.0.254 | 服务器 UDP :9800, Web :9801 |
| Radxa Zero3 | 100.97.145.40 (Tailscale) | 10.99.0.3 | 无人机端 |
| Windows PC | — | 10.99.0.2 | 地面站 |

**禁止**在 PVE 宿主机（192.168.1.100）上部署。

通过隧道更新 radxa 时用原子命令，不能先 kill mpfpv 再执行后续操作。

## Commit 规范

中文，格式：`feat:` / `fix:` / `refactor:` / `test:` / `docs:` + 描述

## 参考代码

`可参考项目/engarde/` — engarde 源码，多路径发送的参考实现。
