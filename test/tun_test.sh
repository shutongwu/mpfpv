#!/bin/bash
# tun_test.sh — End-to-end TUN integration test using Linux network namespaces.
#
# Requires root privileges. Creates 3 network namespaces (ns-server, ns-clientA,
# ns-clientB) connected by veth pairs, starts mpfpv in each, and verifies that
# clientA can ping clientB's virtual IP through the VPN tunnel.
#
# Usage:
#   sudo bash test/tun_test.sh
#
# The script cleans up all namespaces and processes on exit (including on failure).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY="$PROJECT_ROOT/mpfpv"
CONFIG_DIR="$SCRIPT_DIR/tun_configs"

# Namespace and veth names.
NS_SERVER="ns-mpfpv-server"
NS_CLIENT_A="ns-mpfpv-clientA"
NS_CLIENT_B="ns-mpfpv-clientB"

VETH_S_A="veth-sa"   # server side of pair A
VETH_A_S="veth-as"   # clientA side of pair A
VETH_S_B="veth-sb"   # server side of pair B
VETH_B_S="veth-bs"   # clientB side of pair B

# Physical-layer IPs (used for UDP transport between namespaces).
IP_S_A="192.168.100.1"   # server end of link to clientA
IP_A_S="192.168.100.2"   # clientA end of link to server
IP_S_B="192.168.101.1"   # server end of link to clientB
IP_B_S="192.168.101.2"   # clientB end of link to server

# Virtual IPs (assigned by mpfpv via TUN).
VIP_SERVER="10.99.0.254"
VIP_CLIENT_A=""   # auto-assigned, will be read from logs
VIP_CLIENT_B=""   # auto-assigned, will be read from logs

# PIDs of background mpfpv processes.
PID_SERVER=""
PID_CLIENT_A=""
PID_CLIENT_B=""

# Temp files for logs.
LOG_SERVER=$(mktemp /tmp/mpfpv-server.XXXXXX.log)
LOG_CLIENT_A=$(mktemp /tmp/mpfpv-clientA.XXXXXX.log)
LOG_CLIENT_B=$(mktemp /tmp/mpfpv-clientB.XXXXXX.log)

# ---------- cleanup -----------------------------------------------------------

cleanup() {
    echo "--- Cleaning up ---"

    # Kill mpfpv processes.
    for pid in $PID_SERVER $PID_CLIENT_A $PID_CLIENT_B; do
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done

    # Delete namespaces (also removes veth pairs).
    for ns in $NS_SERVER $NS_CLIENT_A $NS_CLIENT_B; do
        ip netns del "$ns" 2>/dev/null || true
    done

    # Remove temp log files.
    rm -f "$LOG_SERVER" "$LOG_CLIENT_A" "$LOG_CLIENT_B"

    # Remove IP pool file if created.
    rm -f /tmp/mpfpv_tun_test_pool.json
}

trap cleanup EXIT

# ---------- pre-flight checks ------------------------------------------------

if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: This script must be run as root."
    exit 1
fi

# Build the binary if it doesn't exist or is older than the source.
echo "--- Building mpfpv binary ---"
(cd "$PROJECT_ROOT" && go build -o "$BINARY" .)

if [ ! -x "$BINARY" ]; then
    echo "ERROR: Failed to build mpfpv binary."
    exit 1
fi

# ---------- create namespaces and veth pairs ----------------------------------

echo "--- Creating network namespaces ---"
ip netns add "$NS_SERVER"
ip netns add "$NS_CLIENT_A"
ip netns add "$NS_CLIENT_B"

echo "--- Creating veth pairs ---"

# Pair A: server <-> clientA
ip link add "$VETH_S_A" type veth peer name "$VETH_A_S"
ip link set "$VETH_S_A" netns "$NS_SERVER"
ip link set "$VETH_A_S" netns "$NS_CLIENT_A"

# Pair B: server <-> clientB
ip link add "$VETH_S_B" type veth peer name "$VETH_B_S"
ip link set "$VETH_S_B" netns "$NS_SERVER"
ip link set "$VETH_B_S" netns "$NS_CLIENT_B"

echo "--- Configuring IP addresses ---"

# Server namespace: two interfaces.
ip netns exec "$NS_SERVER" ip addr add "$IP_S_A/24" dev "$VETH_S_A"
ip netns exec "$NS_SERVER" ip link set "$VETH_S_A" up
ip netns exec "$NS_SERVER" ip addr add "$IP_S_B/24" dev "$VETH_S_B"
ip netns exec "$NS_SERVER" ip link set "$VETH_S_B" up
ip netns exec "$NS_SERVER" ip link set lo up

# ClientA namespace.
ip netns exec "$NS_CLIENT_A" ip addr add "$IP_A_S/24" dev "$VETH_A_S"
ip netns exec "$NS_CLIENT_A" ip link set "$VETH_A_S" up
ip netns exec "$NS_CLIENT_A" ip link set lo up

# ClientB namespace.
ip netns exec "$NS_CLIENT_B" ip addr add "$IP_B_S/24" dev "$VETH_B_S"
ip netns exec "$NS_CLIENT_B" ip link set "$VETH_B_S" up
ip netns exec "$NS_CLIENT_B" ip link set lo up

# Verify physical connectivity.
echo "--- Verifying physical connectivity ---"
ip netns exec "$NS_CLIENT_A" ping -c 1 -W 1 "$IP_S_A" >/dev/null 2>&1 || {
    echo "ERROR: clientA cannot reach server over physical link"
    exit 1
}
ip netns exec "$NS_CLIENT_B" ping -c 1 -W 1 "$IP_S_B" >/dev/null 2>&1 || {
    echo "ERROR: clientB cannot reach server over physical link"
    exit 1
}
echo "Physical connectivity OK."

# ---------- start mpfpv processes ---------------------------------------------

echo "--- Starting mpfpv server ---"
ip netns exec "$NS_SERVER" "$BINARY" --config "$CONFIG_DIR/server.yml" \
    >"$LOG_SERVER" 2>&1 &
PID_SERVER=$!

echo "--- Starting mpfpv clientA ---"
ip netns exec "$NS_CLIENT_A" "$BINARY" --config "$CONFIG_DIR/clientA.yml" \
    >"$LOG_CLIENT_A" 2>&1 &
PID_CLIENT_A=$!

echo "--- Starting mpfpv clientB ---"
ip netns exec "$NS_CLIENT_B" "$BINARY" --config "$CONFIG_DIR/clientB.yml" \
    >"$LOG_CLIENT_B" 2>&1 &
PID_CLIENT_B=$!

# Wait for heartbeat registration to complete.
echo "--- Waiting 3 seconds for registration ---"
sleep 3

# ---------- determine virtual IPs --------------------------------------------

# ClientA and ClientB use auto-assignment (virtualIP is empty / 0.0.0.0).
# Parse the assigned IP from the client logs (logrus output).
VIP_CLIENT_A=$(grep -oP 'virtualIP=\K[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' "$LOG_CLIENT_A" | head -1 || true)
VIP_CLIENT_B=$(grep -oP 'virtualIP=\K[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' "$LOG_CLIENT_B" | head -1 || true)

if [ -z "$VIP_CLIENT_A" ]; then
    echo "ERROR: Could not determine clientA virtual IP from logs."
    echo "--- clientA log ---"
    cat "$LOG_CLIENT_A"
    exit 1
fi

if [ -z "$VIP_CLIENT_B" ]; then
    echo "ERROR: Could not determine clientB virtual IP from logs."
    echo "--- clientB log ---"
    cat "$LOG_CLIENT_B"
    exit 1
fi

echo "Server VIP:  $VIP_SERVER"
echo "ClientA VIP: $VIP_CLIENT_A"
echo "ClientB VIP: $VIP_CLIENT_B"

# ---------- test: ping from clientA to clientB --------------------------------

echo "--- Test: clientA pings clientB ($VIP_CLIENT_B) ---"
if ip netns exec "$NS_CLIENT_A" ping -c 3 -W 2 "$VIP_CLIENT_B"; then
    echo "PASS: clientA can ping clientB through the VPN tunnel."
else
    echo "FAIL: clientA cannot ping clientB."
    echo ""
    echo "--- Server log ---"
    cat "$LOG_SERVER"
    echo ""
    echo "--- ClientA log ---"
    cat "$LOG_CLIENT_A"
    echo ""
    echo "--- ClientB log ---"
    cat "$LOG_CLIENT_B"
    exit 1
fi

# ---------- test: ping from clientB to clientA --------------------------------

echo "--- Test: clientB pings clientA ($VIP_CLIENT_A) ---"
if ip netns exec "$NS_CLIENT_B" ping -c 3 -W 2 "$VIP_CLIENT_A"; then
    echo "PASS: clientB can ping clientA through the VPN tunnel."
else
    echo "FAIL: clientB cannot ping clientA."
    exit 1
fi

# ---------- test: ping from clientA to server ---------------------------------

echo "--- Test: clientA pings server ($VIP_SERVER) ---"
if ip netns exec "$NS_CLIENT_A" ping -c 3 -W 2 "$VIP_SERVER"; then
    echo "PASS: clientA can ping server through the VPN tunnel."
else
    echo "FAIL: clientA cannot ping server."
    exit 1
fi

echo ""
echo "========================================="
echo " All TUN integration tests PASSED"
echo "========================================="
