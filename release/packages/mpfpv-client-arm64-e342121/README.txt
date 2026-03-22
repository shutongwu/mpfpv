mpfpv 客户端 (Linux ARM64)
===========================

适用设备：Radxa Zero3、树莓派 4/5 (64位)、其他 ARM64 Linux 设备

系统要求：
  - Linux ARM64 (aarch64)
  - root 权限（需要创建 TUN 设备）
  - 至少一张可联网的物理网卡

依赖：无（静态编译，零依赖）

安装：
  tar xzf mpfpv-client-arm64-*.tar.gz
  cd mpfpv-client-arm64-*
  sudo ./install.sh client

配置：
  编辑 /opt/mpfpv/mpfpv.yml
  - teamKey: 与服务端一致
  - serverAddr: 服务器地址，如 "cloudfpv.top:9800" 或 "1.2.3.4:9800"
  - sendMode: "redundant"（多网卡冗余）或 "failover"（故障切换）

  可选：
  - excludedInterfaces: 排除不需要的网卡，如 ["lo", "docker0", "tailscale0"]
  - webUI: 客户端本地 Web 界面，默认 0.0.0.0:9801

启停：
  sudo systemctl start mpfpv
  sudo systemctl stop mpfpv
  sudo systemctl restart mpfpv
  sudo systemctl status mpfpv

日志：
  sudo journalctl -u mpfpv -f

多网卡说明：
  软件自动发现物理网卡（eth*/enp*/ens*/wlan*/usb* 等）。
  redundant 模式下每个包通过所有网卡各发一份，最大可靠性。
  failover 模式下只走延迟最低的网卡，故障自动切换。

卸载：
  sudo systemctl stop mpfpv
  sudo systemctl disable mpfpv
  sudo rm /etc/systemd/system/mpfpv.service
  sudo rm -rf /opt/mpfpv
