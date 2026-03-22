#!/bin/bash
set -e

# mpfpv installer
# Usage: sudo ./install.sh <server|client> [config-file]

MODE=${1:-client}
CONFIG=${2:-}
INSTALL_DIR=/opt/mpfpv
BINARY=./mpfpv

if [ "$(id -u)" != "0" ]; then
    echo "Error: run as root (sudo ./install.sh ...)"
    exit 1
fi

if [ ! -f "$BINARY" ]; then
    echo "Error: mpfpv binary not found in current directory"
    exit 1
fi

echo "Installing mpfpv ($MODE mode)..."

# Create install directory
mkdir -p "$INSTALL_DIR"

# Copy binary
cp "$BINARY" "$INSTALL_DIR/mpfpv"
chmod +x "$INSTALL_DIR/mpfpv"

# Copy config
if [ -n "$CONFIG" ] && [ -f "$CONFIG" ]; then
    cp "$CONFIG" "$INSTALL_DIR/mpfpv.yml"
elif [ ! -f "$INSTALL_DIR/mpfpv.yml" ]; then
    if [ "$MODE" = "server" ]; then
        cp "$(dirname "$0")/server.yml" "$INSTALL_DIR/mpfpv.yml" 2>/dev/null || \
        cat > "$INSTALL_DIR/mpfpv.yml" << 'EOF'
mode: server
teamKey: "change-me"

server:
  listenAddr: "0.0.0.0:9800"
  virtualIP: "10.99.0.254/24"
  subnet: "10.99.0.0/24"
  mtu: 1400
  ipPoolFile: "/opt/mpfpv/ip_pool.json"
  webUI: "0.0.0.0:9801"
EOF
    else
        cat > "$INSTALL_DIR/mpfpv.yml" << 'EOF'
mode: client
teamKey: "change-me"

client:
  serverAddr: "your-server:9800"
  sendMode: redundant
  mtu: 1400
  webUI: "0.0.0.0:9801"
EOF
    fi
    echo "Config created at $INSTALL_DIR/mpfpv.yml — edit teamKey and serverAddr!"
fi

# Install systemd service
cp "$(dirname "$0")/mpfpv.service" /etc/systemd/system/mpfpv.service 2>/dev/null || \
cat > /etc/systemd/system/mpfpv.service << 'EOF'
[Unit]
Description=mpfpv multipath VPN
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/mpfpv/mpfpv -config /opt/mpfpv/mpfpv.yml
Restart=always
RestartSec=3
WorkingDirectory=/opt/mpfpv
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable mpfpv

echo ""
echo "Installed to $INSTALL_DIR"
echo "Version: $($INSTALL_DIR/mpfpv -version)"
echo ""
echo "Commands:"
echo "  sudo systemctl start mpfpv     # start"
echo "  sudo systemctl stop mpfpv      # stop"
echo "  sudo systemctl restart mpfpv   # restart"
echo "  sudo systemctl status mpfpv    # status"
echo "  sudo journalctl -u mpfpv -f    # logs"
echo ""
echo "Config: $INSTALL_DIR/mpfpv.yml"
echo "Web UI: http://localhost:9801"
