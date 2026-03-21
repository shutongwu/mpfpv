#!/bin/bash
# mpfpv 多路径冗余测试脚本
# 用法: sudo bash failover_test.sh [测试项]
# 测试项: all | ping | failover | single | status | degrade | stress
# 需要在 radxa 上以 root 执行

# set -e  # 不用 set -e，避免 tc/ping 非零退出码终止脚本

# === 配置 ===
PING_TARGET="10.99.0.2"      # Windows 客户端虚拟 IP
PING_COUNT=20
PING_INTERVAL=0.5
SERVER_API="http://114.55.58.24:9801"
LOCAL_API="http://127.0.0.1:9801"

# === 颜色 ===
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[PASS]${NC} $*"; }
fail()  { echo -e "${RED}[FAIL]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }

# === 工具函数 ===
get_nics() {
    curl -s "$LOCAL_API/api/interfaces" 2>/dev/null | \
        python3 -c "import sys,json; [print(f'  {i[\"name\"]:20s} {i[\"localIP\"]:16s} {i[\"status\"]:8s} RTT={i[\"rtt\"]}') for i in json.load(sys.stdin)]" 2>/dev/null || \
        echo "  (API 不可用)"
}

get_server_info() {
    curl -s "$SERVER_API/api/clients" 2>/dev/null | \
        python3 -c "
import sys,json
for c in json.load(sys.stdin):
    print(f'  clientID={c[\"clientID\"]:5d}  {c[\"virtualIP\"]:12s}  {c[\"deviceName\"]:20s}  addrs={c[\"addrCount\"]}  online={c[\"online\"]}')
" 2>/dev/null || echo "  (服务器 API 不可用)"
}

check_mpfpv() {
    if pgrep -f 'mpfpv -config' > /dev/null; then
        ok "mpfpv 进程运行中 (PID: $(pgrep -f 'mpfpv -config'))"
    else
        fail "mpfpv 未运行！"
        exit 1
    fi
}

check_tun() {
    if ip addr show mpfpv0 &>/dev/null; then
        local ip=$(ip -4 addr show mpfpv0 | grep -oP 'inet \K[\d./]+')
        ok "TUN 设备 mpfpv0 正常, IP: $ip"
    else
        fail "mpfpv0 TUN 设备不存在"
    fi
}

# 精确 ping 测试，返回丢包率
do_ping() {
    local target=$1 count=$2 interval=$3 label=$4
    info "$label: ping $target x$count (间隔 ${interval}s)"
    local result
    result=$(ping -c "$count" -i "$interval" -W 2 "$target" 2>&1)
    local loss=$(echo "$result" | grep -oP '\d+(?=% packet loss)')
    local rtt=$(echo "$result" | grep -oP 'rtt.*= \K[0-9./]+' || echo "N/A")
    if [ "$loss" = "0" ]; then
        ok "丢包率 0%, RTT(min/avg/max) = ${rtt}ms"
    elif [ "$loss" -lt 10 ]; then
        warn "丢包率 ${loss}%, RTT = ${rtt}ms"
    else
        fail "丢包率 ${loss}%, RTT = ${rtt}ms"
    fi
    return "$loss"
}

# === 测试 1: 状态检查 ===
test_status() {
    echo ""
    echo "========================================"
    echo " 测试: 系统状态检查"
    echo "========================================"
    check_mpfpv
    check_tun
    echo ""
    info "本地网卡状态:"
    get_nics
    echo ""
    info "服务器客户端列表:"
    get_server_info
    echo ""
    info "Socket RECV-Q:"
    ss -ulnp 2>/dev/null | grep mpfpv | awk '{printf "  %-50s RECV-Q=%s\n", $5, $2}'
}

# === 测试 2: 基础 ping ===
test_ping() {
    echo ""
    echo "========================================"
    echo " 测试: 基础 ping 连通性"
    echo "========================================"
    do_ping "$PING_TARGET" "$PING_COUNT" "$PING_INTERVAL" "双网卡 redundant"
}

# === 测试 3: 单网卡故障转移 ===
test_failover() {
    echo ""
    echo "========================================"
    echo " 测试: 拔网卡故障转移"
    echo "========================================"

    # 获取当前网卡列表
    local nics=($(ip -o link show up | awk -F': ' '{print $2}' | grep -E '^eth|^enx|^wlan|^usb'))
    if [ ${#nics[@]} -lt 2 ]; then
        warn "只有 ${#nics[@]} 个网卡，跳过故障转移测试"
        return
    fi

    info "当前活跃网卡: ${nics[*]}"
    local test_nic="${nics[0]}"
    local keep_nic="${nics[1]}"
    info "将模拟断开: $test_nic, 保留: $keep_nic"

    # 阶段 1: 双网卡 baseline
    info "--- 阶段 1: 双网卡 baseline ---"
    do_ping "$PING_TARGET" 10 0.5 "双网卡"

    # 阶段 2: 断开一个网卡，持续 ping
    info "--- 阶段 2: 断开 $test_nic ---"
    info "开始后台 ping..."
    ping -c 30 -i 0.5 -W 2 "$PING_TARGET" > /tmp/failover_ping.txt 2>&1 &
    local ping_pid=$!

    sleep 3
    info "正在断开 $test_nic ..."
    ip link set "$test_nic" down

    sleep 10
    info "正在恢复 $test_nic ..."
    ip link set "$test_nic" up

    sleep 5
    wait $ping_pid 2>/dev/null || true

    # 分析结果
    local total=$(grep -c 'bytes from\|timeout\|Unreachable' /tmp/failover_ping.txt 2>/dev/null || echo 0)
    local received=$(grep -c 'bytes from' /tmp/failover_ping.txt 2>/dev/null || echo 0)
    local lost=$((total - received))

    echo ""
    info "故障转移结果:"
    info "  总包数: $total, 收到: $received, 丢失: $lost"
    if [ "$total" -gt 0 ]; then
        local loss_pct=$((lost * 100 / total))
        if [ "$loss_pct" -le 5 ]; then
            ok "丢包率 ${loss_pct}% (故障转移基本无感)"
        elif [ "$loss_pct" -le 20 ]; then
            warn "丢包率 ${loss_pct}% (有短暂中断)"
        else
            fail "丢包率 ${loss_pct}% (故障转移不理想)"
        fi
    fi

    echo ""
    info "Ping 详细输出:"
    cat /tmp/failover_ping.txt | grep -E 'bytes from|timeout|Unreachable|statistics|received|rtt'
    rm -f /tmp/failover_ping.txt
}

# === 测试 4: 单网卡性能 ===
test_single() {
    echo ""
    echo "========================================"
    echo " 测试: 逐个网卡单独测试"
    echo "========================================"

    local nics=($(ip -o link show up | awk -F': ' '{print $2}' | grep -E '^eth|^enx|^wlan|^usb'))
    if [ ${#nics[@]} -lt 2 ]; then
        warn "只有 ${#nics[@]} 个网卡，跳过单网卡测试"
        do_ping "$PING_TARGET" "$PING_COUNT" "$PING_INTERVAL" "单网卡"
        return
    fi

    for nic in "${nics[@]}"; do
        info "--- 测试只用 $nic ---"
        # 关掉其他网卡
        local others=()
        for other in "${nics[@]}"; do
            if [ "$other" != "$nic" ]; then
                ip link set "$other" down
                others+=("$other")
            fi
        done
        sleep 3

        do_ping "$PING_TARGET" 10 0.5 "仅 $nic"

        # 恢复
        for other in "${others[@]}"; do
            ip link set "$other" up
        done
        sleep 3
    done

    info "--- 恢复双网卡 ---"
    do_ping "$PING_TARGET" 10 0.5 "恢复双网卡"
}

# === 测试 5: 连续切换压力测试 ===
test_stress() {
    echo ""
    echo "========================================"
    echo " 测试: 连续切换压力测试 (30秒)"
    echo "========================================"

    local nics=($(ip -o link show up | awk -F': ' '{print $2}' | grep -E '^eth|^enx|^wlan|^usb'))
    if [ ${#nics[@]} -lt 2 ]; then
        warn "需要至少 2 个网卡"
        return
    fi

    info "后台 ping 30 秒，每 5 秒切换一次网卡..."
    ping -c 60 -i 0.5 -W 2 "$PING_TARGET" > /tmp/stress_ping.txt 2>&1 &
    local ping_pid=$!

    for i in 1 2 3 4 5 6; do
        local nic="${nics[$(( (i-1) % ${#nics[@]} ))]}"
        sleep 2
        info "  第${i}次: 断开 $nic"
        ip link set "$nic" down
        sleep 3
        info "  第${i}次: 恢复 $nic"
        ip link set "$nic" up
    done

    wait $ping_pid 2>/dev/null || true

    local total=$(grep -c 'bytes from\|timeout\|Unreachable' /tmp/stress_ping.txt 2>/dev/null || echo 0)
    local received=$(grep -c 'bytes from' /tmp/stress_ping.txt 2>/dev/null || echo 0)
    local lost=$((total - received))
    local loss_pct=0
    [ "$total" -gt 0 ] && loss_pct=$((lost * 100 / total))

    echo ""
    info "压力测试结果: 总包=$total 收到=$received 丢失=$lost 丢包率=${loss_pct}%"
    if [ "$loss_pct" -le 10 ]; then
        ok "压力测试通过 (丢包 ${loss_pct}%)"
    else
        fail "压力测试丢包过高 (${loss_pct}%)"
    fi

    grep -E 'statistics|received|rtt' /tmp/stress_ping.txt
    rm -f /tmp/stress_ping.txt
}

# === 测试 6: 网络劣化冗余测试（模拟 5G 信号差） ===
tc_cleanup() {
    local nics=($(ip -o link show up | awk -F': ' '{print $2}' | grep -E '^eth|^enx|^wlan|^usb'))
    for nic in "${nics[@]}"; do
        tc qdisc del dev "$nic" root 2>/dev/null
    done
}

test_degrade() {
    echo ""
    echo "========================================"
    echo " 测试: 网络劣化冗余（模拟 5G 信号差）"
    echo "========================================"

    local nics=($(ip -o link show up | awk -F': ' '{print $2}' | grep -E '^eth|^enx|^wlan|^usb'))
    if [ ${#nics[@]} -lt 2 ]; then
        warn "只有 ${#nics[@]} 个网卡，跳过劣化测试"
        return
    fi

    # 确保清理残留 tc 规则
    tc_cleanup
    trap tc_cleanup EXIT

    info "当前活跃网卡: ${nics[*]}"
    info ""

    # --- 阶段 1: 双网卡正常 baseline ---
    info "=== 阶段 1: 双网卡正常 baseline ==="
    do_ping "$PING_TARGET" 20 0.2 "双网卡正常"

    # --- 阶段 2: 一个网卡 100% 丢包（模拟完全断网） ---
    info ""
    info "=== 阶段 2: ${nics[0]} 100% 丢包（另一个正常） ==="
    tc qdisc add dev "${nics[0]}" root netem loss 100%
    sleep 1
    do_ping "$PING_TARGET" 20 0.2 "${nics[0]} 全丢，${nics[1]} 正常"
    tc qdisc del dev "${nics[0]}" root
    sleep 1

    # --- 阶段 3: 一个网卡高延迟+高丢包（模拟信号差） ---
    info ""
    info "=== 阶段 3: ${nics[0]} 延迟 500ms + 50% 丢包 ==="
    tc qdisc add dev "${nics[0]}" root netem delay 500ms loss 50%
    sleep 1
    do_ping "$PING_TARGET" 20 0.2 "${nics[0]} 劣化，${nics[1]} 正常"
    tc qdisc del dev "${nics[0]}" root
    sleep 1

    # --- 阶段 4: 两个网卡都劣化（模拟双链路都差） ---
    info ""
    info "=== 阶段 4: 两个网卡都 30% 丢包 ==="
    tc qdisc add dev "${nics[0]}" root netem loss 30%
    tc qdisc add dev "${nics[1]}" root netem loss 30%
    sleep 1
    do_ping "$PING_TARGET" 20 0.2 "双网卡各 30% 丢包（冗余应大幅降低实际丢包）"
    tc_cleanup
    sleep 1

    # --- 阶段 5: 轮流劣化（模拟信号交替变差） ---
    info ""
    info "=== 阶段 5: 轮流劣化压力测试 (30 秒) ==="
    ping -c 60 -i 0.5 -W 2 "$PING_TARGET" > /tmp/degrade_ping.txt 2>&1 &
    local ping_pid=$!

    for i in 1 2 3 4 5 6; do
        local nic="${nics[$(( (i-1) % ${#nics[@]} ))]}"
        info "  第${i}次: ${nic} → 100% 丢包"
        tc qdisc add dev "$nic" root netem loss 100% 2>/dev/null || \
            tc qdisc change dev "$nic" root netem loss 100%
        sleep 3
        info "  第${i}次: ${nic} → 恢复正常"
        tc qdisc del dev "$nic" root 2>/dev/null
        sleep 2
    done

    wait $ping_pid 2>/dev/null || true

    local total=$(grep -c 'bytes from\|timeout\|Unreachable' /tmp/degrade_ping.txt 2>/dev/null || echo 0)
    local received=$(grep -c 'bytes from' /tmp/degrade_ping.txt 2>/dev/null || echo 0)
    local lost=$((total - received))
    local loss_pct=0
    [ "$total" -gt 0 ] && loss_pct=$((lost * 100 / total))

    echo ""
    info "轮流劣化结果: 总包=$total 收到=$received 丢失=$lost 丢包率=${loss_pct}%"
    if [ "$loss_pct" -le 5 ]; then
        ok "冗余优秀 (丢包 ${loss_pct}%)"
    elif [ "$loss_pct" -le 15 ]; then
        warn "冗余一般 (丢包 ${loss_pct}%)"
    else
        fail "冗余不理想 (丢包 ${loss_pct}%)"
    fi
    grep -E 'statistics|received|rtt' /tmp/degrade_ping.txt
    rm -f /tmp/degrade_ping.txt

    # --- 阶段 6: 恢复 baseline ---
    info ""
    info "=== 阶段 6: 恢复双网卡正常 ==="
    tc_cleanup
    sleep 2
    do_ping "$PING_TARGET" 20 0.2 "恢复正常"
}

# === 主入口 ===
if [ "$(id -u)" != "0" ]; then
    echo "请用 sudo 运行此脚本"
    exit 1
fi

case "${1:-all}" in
    status)   test_status ;;
    ping)     test_status; test_ping ;;
    failover) test_status; test_failover ;;
    single)   test_status; test_single ;;
    stress)   test_status; test_stress ;;
    degrade)  test_status; test_degrade ;;
    all)
        test_status
        test_ping
        test_degrade
        ;;
    *)
        echo "用法: sudo bash $0 [status|ping|failover|single|stress|degrade|all]"
        exit 1
        ;;
esac

echo ""
echo "========================================"
echo " 测试完成"
echo "========================================"
