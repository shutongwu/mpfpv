mpfpv 服务端 (Linux x86_64)
===========================

系统要求：
  - Linux x86_64 (Ubuntu/Debian/CentOS 等)
  - root 权限（需要创建 TUN 设备）
  - 开放 UDP 端口 9800（数据中继）
  - 开放 TCP 端口 9801（Web 管理界面，可选）

依赖：无（静态编译，零依赖）

安装：
  tar xzf mpfpv-server-*.tar.gz
  cd mpfpv-server-*
  sudo ./install.sh server

配置：
  编辑 /opt/mpfpv/mpfpv.yml
  - teamKey: 修改为你的团队密钥（客户端需一致）
  - listenAddr: 默认 0.0.0.0:9800
  - webUI: 默认 0.0.0.0:9801

启停：
  sudo systemctl start mpfpv
  sudo systemctl stop mpfpv
  sudo systemctl restart mpfpv
  sudo systemctl status mpfpv

日志：
  sudo journalctl -u mpfpv -f

Web 管理界面：
  http://<服务器IP>:9801
  可查看所有设备、在线状态、每网卡 RTT 和带宽

卸载：
  sudo systemctl stop mpfpv
  sudo systemctl disable mpfpv
  sudo rm /etc/systemd/system/mpfpv.service
  sudo rm -rf /opt/mpfpv
