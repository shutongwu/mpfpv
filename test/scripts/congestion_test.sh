#!/bin/bash
# mpfpv 5G 网络拥堵模拟测试脚本
# 用法: sudo testnet        (交互式菜单)
#       sudo testnet <编号>  (直接运行指定测试)
#
# 核心思路: 每个测试都轮流对每张卡施加劣化，另一张卡保持正常
# 绝不使用 ip link down / USB 操作，只用 tc netem + tbf
# 需要在 radxa 上以 root 执行

# === 配置 ===
PING_TARGET="10.99.0.254"
PING_FALLBACK="10.99.0.2"
PING_COUNT=20
PING_INTERVAL=0.3
LOCAL_API="http://127.0.0.1:9801"
SERVER_API="http://114.55.58.24:9801"
PHASE_SETTLE=3
LOG_DIR="/tmp/congestion_test_$$"

# === 颜色 ===
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

info()   { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()     { echo -e "${GREEN}[PASS]${NC} $*"; }
fail()   { echo -e "${RED}[FAIL]${NC} $*"; }
warn()   { echo -e "${YELLOW}[WARN]${NC} $*"; }
header() { echo ""; echo -e "${BOLD}========================================${NC}"; echo -e "${BOLD} $*${NC}"; echo -e "${BOLD}========================================${NC}"; }
sep()    { echo -e "${CYAN}  ----------------------------------------${NC}"; }

# === 结果收集 ===
declare -a RESULTS=()
declare -a NICS=()
NIC_A=""
NIC_B=""
MONITOR_PID=""

# ============================================================
# 安全清理
# ============================================================
cleanup() {
    [ -n "$MONITOR_PID" ] && kill "$MONITOR_PID" 2>/dev/null && wait "$MONITOR_PID" 2>/dev/null
    jobs -p 2>/dev/null | xargs -r kill 2>/dev/null
    wait 2>/dev/null

    echo ""
    info "清理: 移除所有 tc 规则..."
    local all_nics
    all_nics=$(ip -o link show up | awk -F': ' '{print $2}' | grep -E '^eth|^enx|^enp|^ens|^wlan|^wlp|^usb')
    for nic in $all_nics; do
        tc qdisc del dev "$nic" root 2>/dev/null
    done

    sleep 1
    if ping -c 2 -W 3 "$PING_TARGET" &>/dev/null; then
        ok "清理验证: ping $PING_TARGET 成功，网络已恢复"
    elif ping -c 2 -W 3 "$PING_FALLBACK" &>/dev/null; then
        ok "清理验证: ping $PING_FALLBACK 成功，网络已恢复"
    else
        fail "警告: 清理完成但 ping 失败！请手动检查: tc qdisc show"
    fi
    rm -rf "$LOG_DIR"
}
trap cleanup EXIT INT TERM

# ============================================================
# 依赖检查
# ============================================================
check_deps() {
    [ "$(id -u)" != "0" ] && { fail "请用 sudo 运行"; exit 1; }
    command -v tc &>/dev/null || { fail "缺少 tc 命令"; exit 1; }
    modprobe sch_netem 2>/dev/null
    modprobe sch_tbf 2>/dev/null
    pgrep -f 'mpfpv' > /dev/null || { fail "mpfpv 未运行！"; exit 1; }
    ip addr show mpfpv0 &>/dev/null || { fail "mpfpv0 不存在！"; exit 1; }
    mkdir -p "$LOG_DIR"
}

# ============================================================
# 网卡检测
# ============================================================
detect_nics() {
    local api_result
    api_result=$(curl -s --max-time 2 "$LOCAL_API/api/interfaces" 2>/dev/null)
    if [ $? -eq 0 ] && [ -n "$api_result" ] && echo "$api_result" | python3 -c "import sys,json; json.load(sys.stdin)" &>/dev/null; then
        NICS=($(echo "$api_result" | python3 -c "import sys,json; [print(i['name']) for i in json.load(sys.stdin)]" 2>/dev/null))
    fi
    [ ${#NICS[@]} -eq 0 ] && NICS=($(ip -o link show up | awk -F': ' '{print $2}' | grep -E '^enx|^usb|^eth'))
    [ ${#NICS[@]} -lt 2 ] && { fail "需要至少 2 张网卡才能轮流测试"; exit 1; }

    NIC_A="${NICS[0]}"
    NIC_B="${NICS[1]}"
    info "检测到网卡: $NIC_A / $NIC_B"
}

select_ping_target() {
    if ping -c 2 -W 2 "$PING_TARGET" &>/dev/null; then
        info "Ping 目标: $PING_TARGET"
    elif ping -c 2 -W 2 "$PING_FALLBACK" &>/dev/null; then
        PING_TARGET="$PING_FALLBACK"
        info "Ping 目标: $PING_TARGET"
    else
        fail "无法 ping 通目标，请检查 mpfpv"; exit 1
    fi
}

# ============================================================
# tc 辅助
# ============================================================
clear_nic() { tc qdisc del dev "$1" root 2>/dev/null; }
clear_all() { for nic in "${NICS[@]}"; do tc qdisc del dev "$nic" root 2>/dev/null; done; }

# 施加 netem + tbf
apply_congestion() {
    local nic="$1" delay="$2" jitter="$3" jitter_corr="$4"
    local loss="$5" loss_corr="$6" rate="$7" reorder="${8:-0%}"

    tc qdisc del dev "$nic" root 2>/dev/null
    local cmd="tc qdisc add dev $nic root handle 1: netem delay $delay $jitter $jitter_corr distribution normal loss $loss $loss_corr"
    [ "$reorder" != "0%" ] && cmd+=" reorder $reorder 50%"
    eval "$cmd" 2>/dev/null || { warn "netem 失败: $nic"; return 1; }

    if [ -n "$rate" ] && [ "$rate" != "-" ]; then
        local burst=1600 limit=3000
        local rate_num; rate_num=$(echo "$rate" | grep -oP '^\d+')
        [ -n "$rate_num" ] && [ "$rate_num" -gt 1000 ] 2>/dev/null && burst=4000 && limit=12000
        tc qdisc add dev "$nic" parent 1:1 handle 10: tbf rate "$rate" burst "$burst" limit "$limit" 2>/dev/null
    fi
}

apply_netem_only() {
    local nic="$1" delay="$2" jitter="$3" jitter_corr="$4" loss="$5" loss_corr="$6"
    tc qdisc del dev "$nic" root 2>/dev/null
    tc qdisc add dev "$nic" root netem delay "$delay" "$jitter" "$jitter_corr" distribution normal loss "$loss" "$loss_corr" 2>/dev/null
}

# ============================================================
# mpfpv 状态快照
# ============================================================
snapshot_interfaces() {
    curl -s --max-time 2 "$LOCAL_API/api/interfaces" 2>/dev/null | python3 -c "
import sys,json
try:
    for i in json.load(sys.stdin):
        s=i.get('status','?'); r=i.get('rtt','-'); a=' *' if i.get('isActive') else ''
        print(f'  {i[\"name\"]:20s} status={s:8s} RTT={r:>10s}{a}')
except: print('  (API 不可用)')
" 2>/dev/null
}

monitor_start() {
    local logfile="$1"; > "$logfile"
    ( local prev=""
      while true; do
        local snap; snap=$(curl -s --max-time 1 "$LOCAL_API/api/interfaces" 2>/dev/null | python3 -c "
import sys,json
try:
    d=json.load(sys.stdin); print(' | '.join(f'{i[\"name\"]}={i.get(\"status\",\"?\")},RTT={i.get(\"rtt\",\"-\")}' for i in d))
except: print('?')
" 2>/dev/null)
        [ -n "$snap" ] && [ "$snap" != "$prev" ] && echo "[$(date +%H:%M:%S)] $snap" >> "$logfile" && prev="$snap"
        sleep 1
      done
    ) &
    MONITOR_PID=$!
}
monitor_stop() { [ -n "$MONITOR_PID" ] && kill "$MONITOR_PID" 2>/dev/null && wait "$MONITOR_PID" 2>/dev/null; MONITOR_PID=""; }

show_monitor_log() {
    local logfile="$1"
    [ -f "$logfile" ] || return
    info "路径状态变化:"
    while IFS= read -r line; do echo "  $line"; done < "$logfile"
    rm -f "$logfile"
}

# ============================================================
# Ping 测试
# ============================================================
do_ping_test() {
    local label="$1" pass_thr="${2:-5}" warn_thr="${3:-15}"
    local pingfile="$LOG_DIR/ping_${RANDOM}.txt"
    ping -c "$PING_COUNT" -i "$PING_INTERVAL" -W 3 "$PING_TARGET" > "$pingfile" 2>&1

    local received lost loss_pct
    received=$(grep -c 'bytes from' "$pingfile" 2>/dev/null || echo 0)
    lost=$((PING_COUNT - received))
    loss_pct=$((lost * 100 / PING_COUNT))
    local rtt; rtt=$(grep -oP 'rtt.*= \K[0-9.]+/[0-9.]+/[0-9.]+' "$pingfile" 2>/dev/null | head -1)
    [ -z "$rtt" ] && rtt="-/-/-"
    local avg_rtt; avg_rtt=$(echo "$rtt" | cut -d'/' -f2)

    if [ "$loss_pct" -le "$pass_thr" ]; then ok "$label: 丢包=${loss_pct}%, RTT=${rtt}ms"
    elif [ "$loss_pct" -le "$warn_thr" ]; then warn "$label: 丢包=${loss_pct}%, RTT=${rtt}ms"
    else fail "$label: 丢包=${loss_pct}%, RTT=${rtt}ms"
    fi
    RESULTS+=("${label}|${PING_COUNT}|${received}|${lost}|${loss_pct}|${avg_rtt:-N/A}")
    rm -f "$pingfile"
}

start_bg_ping() {
    local count="$1" interval="$2"
    local pingfile="$LOG_DIR/bgping_${RANDOM}.txt"
    ping -c "$count" -i "$interval" -W 3 "$PING_TARGET" > "$pingfile" 2>&1 &
    echo "$! $pingfile"
}

analyze_bg_ping() {
    local pingfile="$1" label="$2" pass_thr="${3:-10}" warn_thr="${4:-20}"
    local received lost loss_pct count
    count=$(grep -c '' "$pingfile" 2>/dev/null || echo 0)  # just for safety
    received=$(grep -c 'bytes from' "$pingfile" 2>/dev/null || echo 0)
    # get actual count from ping summary
    local actual_count; actual_count=$(grep -oP '\d+(?= packets transmitted)' "$pingfile" 2>/dev/null || echo "$received")
    [ "$actual_count" -eq 0 ] 2>/dev/null && actual_count=1
    lost=$((actual_count - received))
    loss_pct=$((lost * 100 / actual_count))
    local rtt; rtt=$(grep -oP 'rtt.*= \K[0-9.]+/[0-9.]+/[0-9.]+' "$pingfile" 2>/dev/null | head -1)
    [ -z "$rtt" ] && rtt="-/-/-"
    local avg_rtt; avg_rtt=$(echo "$rtt" | cut -d'/' -f2)

    if [ "$loss_pct" -le "$pass_thr" ]; then ok "$label: 丢包=${loss_pct}%, RTT=${rtt}ms"
    elif [ "$loss_pct" -le "$warn_thr" ]; then warn "$label: 丢包=${loss_pct}%, RTT=${rtt}ms"
    else fail "$label: 丢包=${loss_pct}%, RTT=${rtt}ms"
    fi
    RESULTS+=("${label}|${actual_count}|${received}|${lost}|${loss_pct}|${avg_rtt:-N/A}")
    rm -f "$pingfile"
}

# ============================================================
# 测试 1: 基准
# ============================================================
test_baseline() {
    header "基准测试（双网卡正常）"
    clear_all; sleep 1
    info "mpfpv 路径状态:"; snapshot_interfaces; echo ""
    do_ping_test "基准-双网卡正常" 0 3
}

# ============================================================
# 测试 2: 轮流断网
# ============================================================
test_blackout() {
    header "轮流断网（95%丢包 + 3s延迟）"

    for nic in "${NICS[@]}"; do
        local other
        [ "$nic" = "$NIC_A" ] && other="$NIC_B" || other="$NIC_A"
        sep
        info "断网 $nic，保留 $other"
        apply_netem_only "$nic" 3000ms 500ms 25% 95% 95%
        sleep "$PHASE_SETTLE"

        info "施加后状态:"; snapshot_interfaces

        # 等 mpfpv 检测到（5次心跳miss ≈ 5s）
        info "等待 mpfpv 检测 (~6s)..."
        sleep 6
        info "检测后状态:"; snapshot_interfaces

        do_ping_test "断网 $nic" 5 10

        clear_nic "$nic"
        sleep 3
        info "恢复后状态:"; snapshot_interfaces
        echo ""
    done
    clear_all
}

# ============================================================
# 测试 3: 轮流限速
# ============================================================
test_ratelimit() {
    header "轮流限速（渐进：5M → 1M → 200K → 50K）"

    local rates=("5mbit" "1mbit" "200kbit" "50kbit")
    local descs=("5Mbit" "1Mbit" "200Kbit" "50Kbit")

    for nic in "${NICS[@]}"; do
        local other
        [ "$nic" = "$NIC_A" ] && other="$NIC_B" || other="$NIC_A"
        sep
        info "限速 $nic，保留 $other"

        for idx in "${!rates[@]}"; do
            local rate="${rates[$idx]}"
            local desc="${descs[$idx]}"
            info "  $nic → $desc"
            # 纯限速，不加额外延迟和丢包
            tc qdisc del dev "$nic" root 2>/dev/null
            tc qdisc add dev "$nic" root handle 1: tbf rate "$rate" burst 1600 limit 3000 2>/dev/null
            sleep "$PHASE_SETTLE"

            do_ping_test "限速${desc} $nic" 5 15
        done

        clear_nic "$nic"
        sleep 2
        echo ""
    done
    clear_all
}

# ============================================================
# 测试 4: 轮流丢包
# ============================================================
test_loss() {
    header "轮流丢包（渐进：5% → 15% → 30% → 60%）"

    local losses=("5%" "15%" "30%" "60%")
    local corrs=("50%" "70%" "80%" "90%")

    for nic in "${NICS[@]}"; do
        local other
        [ "$nic" = "$NIC_A" ] && other="$NIC_B" || other="$NIC_A"
        sep
        info "丢包 $nic，保留 $other"

        for idx in "${!losses[@]}"; do
            local loss="${losses[$idx]}"
            local corr="${corrs[$idx]}"
            info "  $nic → ${loss}丢包 (相关性${corr})"
            tc qdisc del dev "$nic" root 2>/dev/null
            tc qdisc add dev "$nic" root netem loss "$loss" "$corr" 2>/dev/null
            sleep "$PHASE_SETTLE"

            do_ping_test "丢包${loss} $nic" 5 15
        done

        clear_nic "$nic"
        sleep 2
        echo ""
    done
    clear_all
}

# ============================================================
# 测试 5: 轮流高延迟+抖动
# ============================================================
test_latency() {
    header "轮流高延迟（渐进：50ms → 200ms → 500ms → 2000ms）"

    local delays=("50ms" "200ms" "500ms" "2000ms")
    local jitters=("30ms" "100ms" "300ms" "1000ms")

    for nic in "${NICS[@]}"; do
        local other
        [ "$nic" = "$NIC_A" ] && other="$NIC_B" || other="$NIC_A"
        sep
        info "加延迟 $nic，保留 $other"

        for idx in "${!delays[@]}"; do
            local delay="${delays[$idx]}"
            local jitter="${jitters[$idx]}"
            info "  $nic → ${delay}±${jitter}"
            tc qdisc del dev "$nic" root 2>/dev/null
            tc qdisc add dev "$nic" root netem delay "$delay" "$jitter" 25% distribution normal 2>/dev/null
            sleep "$PHASE_SETTLE"

            do_ping_test "延迟${delay} $nic" 5 15
        done

        clear_nic "$nic"
        sleep 2
        echo ""
    done
    clear_all
}

# ============================================================
# 测试 6: 轮流综合拥堵（延迟+丢包+限速）
# ============================================================
test_congestion() {
    header "轮流综合拥堵（延迟+丢包+限速同时施加）"

    local levels=(light moderate heavy near_disconnect)
    local level_names=("轻微" "中度" "严重" "近断连")

    for nic in "${NICS[@]}"; do
        local other
        [ "$nic" = "$NIC_A" ] && other="$NIC_B" || other="$NIC_A"
        sep
        info "综合拥堵 $nic，保留 $other"

        for idx in "${!levels[@]}"; do
            local level="${levels[$idx]}"
            local name="${level_names[$idx]}"

            case "$level" in
                light)           apply_congestion "$nic" 50ms 30ms 25% 2% 50% 5mbit 0% ;;
                moderate)        apply_congestion "$nic" 150ms 80ms 30% 10% 75% 1mbit 5% ;;
                heavy)           apply_congestion "$nic" 500ms 300ms 25% 30% 80% 200kbit 10% ;;
                near_disconnect) apply_congestion "$nic" 2000ms 1000ms 25% 60% 90% 50kbit 15% ;;
            esac

            info "  $nic → ${name}拥堵"
            sleep "$PHASE_SETTLE"

            do_ping_test "${name}拥堵 $nic" 5 15
        done

        clear_nic "$nic"
        sleep 2
        echo ""
    done
    clear_all
}

# ============================================================
# 测试 7: 轮流突发 + 恢复
# ============================================================
test_spike() {
    header "轮流突发拥堵（正常→突然heavy→恢复）"

    for nic in "${NICS[@]}"; do
        local other
        [ "$nic" = "$NIC_A" ] && other="$NIC_B" || other="$NIC_A"
        sep
        info "突发拥堵 $nic，保留 $other"

        local monfile="$LOG_DIR/spike_${nic}.txt"
        monitor_start "$monfile"

        # 后台 ping 35s
        local bg_result; bg_result=$(start_bg_ping 35 1.0)
        local ping_pid=$(echo "$bg_result" | awk '{print $1}')
        local pingfile=$(echo "$bg_result" | awk '{print $2}')

        info "  正常运行 8s..."
        sleep 8

        info "  突发 heavy → $nic"
        apply_congestion "$nic" 500ms 300ms 25% 30% 80% 200kbit 10%
        sleep 12

        info "  恢复 $nic"
        clear_nic "$nic"
        sleep 10

        wait "$ping_pid" 2>/dev/null || true
        monitor_stop

        analyze_bg_ping "$pingfile" "突发 $nic" 10 20
        show_monitor_log "$monfile"
        echo ""
    done
    clear_all
}

# ============================================================
# 测试 8: 双卡同断验证（确认 tc 生效）
# ============================================================
test_both_blackout() {
    header "双卡同断验证（两张卡同时 100%丢包 5秒）"
    clear_all; sleep 1

    info "先测基准 ping..."
    do_ping_test "同断前基准" 0 3

    echo ""
    warn ">>> 即将同时断掉两张卡 5 秒，视频/ping 应完全中断 <<<"
    sleep 2

    # 后台 ping 持续 15s（断前5s + 断中5s + 恢复5s）
    local bg_result; bg_result=$(start_bg_ping 30 0.5)
    local ping_pid=$(echo "$bg_result" | awk '{print $1}')
    local pingfile=$(echo "$bg_result" | awk '{print $2}')

    info "正常运行 5s..."
    sleep 5

    info ">>> 双卡同断! $NIC_A + $NIC_B 100%丢包 <<<"
    for nic in "${NICS[@]}"; do
        tc qdisc del dev "$nic" root 2>/dev/null
        tc qdisc add dev "$nic" root netem loss 100% 2>/dev/null
    done

    sleep 5

    info ">>> 恢复双卡 <<<"
    clear_all

    sleep 5
    wait "$ping_pid" 2>/dev/null || true

    analyze_bg_ping "$pingfile" "双卡同断5s" 10 50

    # 检验：断 5 秒应该丢至少 8-10 个包（0.5s间隔 → ~10个包在断网期间）
    # 如果丢包为 0 说明 tc 没生效
    local recv; recv=$(echo "${RESULTS[-1]}" | cut -d'|' -f5)
    if [ "$recv" -le 2 ] 2>/dev/null; then
        warn "丢包极少，tc 可能未生效！检查 tc qdisc show"
    else
        ok "tc 验证通过：断网期间确实产生了丢包"
    fi
}

# ============================================================
# 系统状态
# ============================================================
test_status() {
    header "系统状态检查"
    pgrep -f 'mpfpv' > /dev/null && ok "mpfpv 运行中 (PID: $(pgrep -f 'mpfpv' | head -1))" || { fail "mpfpv 未运行！"; exit 1; }

    if ip addr show mpfpv0 &>/dev/null; then
        ok "TUN: mpfpv0, IP: $(ip -4 addr show mpfpv0 | grep -oP 'inet \K[\d./]+')"
    fi

    echo ""; info "mpfpv 路径状态:"; snapshot_interfaces

    echo ""; info "服务器客户端:"
    curl -s --max-time 2 "$SERVER_API/api/clients" 2>/dev/null | python3 -c "
import sys,json
try:
    for c in json.load(sys.stdin):
        print(f'  ID={c[\"clientID\"]:5d}  {c[\"virtualIP\"]:12s}  {c.get(\"deviceName\",\"?\"):20s}  addrs={c[\"addrCount\"]}  online={c[\"online\"]}')
except: print('  (不可用)')
" 2>/dev/null

    echo ""; info "tc 规则:"
    local clean=true
    for nic in "${NICS[@]}"; do
        local r; r=$(tc qdisc show dev "$nic" 2>/dev/null | grep -v "noqueue\|fq_codel\|pfifo_fast\|mq")
        [ -n "$r" ] && warn "  $nic: $r" && clean=false
    done
    $clean && ok "  无残留 tc 规则"
}

# ============================================================
# 结果汇总
# ============================================================
print_summary() {
    [ ${#RESULTS[@]} -eq 0 ] && return
    echo ""
    echo -e "${BOLD}=================================================================${NC}"
    echo -e "${BOLD}                    测 试 结 果 汇 总${NC}"
    echo -e "${BOLD}=================================================================${NC}"
    printf "%-26s %5s %5s %5s %7s %10s\n" "场景" "总包" "收到" "丢失" "丢包率" "平均RTT"
    echo "-----------------------------------------------------------------"

    local p=0 w=0 f=0
    for entry in "${RESULTS[@]}"; do
        IFS='|' read -r label total recv lost loss_pct avg_rtt <<< "$entry"
        local color="$GREEN"
        if [ "$loss_pct" -gt 20 ] 2>/dev/null; then color="$RED"; ((f++))
        elif [ "$loss_pct" -gt 5 ] 2>/dev/null; then color="$YELLOW"; ((w++))
        else ((p++)); fi
        local rs; [ "$avg_rtt" = "N/A" ] || [ -z "$avg_rtt" ] && rs="N/A" || rs="${avg_rtt}ms"
        printf "%-26s %5s %5s %5s ${color}%6s%%${NC} %10s\n" "$label" "$total" "$recv" "$lost" "$loss_pct" "$rs"
    done
    echo "-----------------------------------------------------------------"
    echo -e "  ${GREEN}PASS: ${p}${NC}  ${YELLOW}WARN: ${w}${NC}  ${RED}FAIL: ${f}${NC}  总计: ${#RESULTS[@]}"
    echo -e "${BOLD}=================================================================${NC}"
}

# ============================================================
# 执行
# ============================================================
run_test() {
    case "$1" in
        1|baseline)   test_status; test_baseline ;;
        2|blackout)   test_status; test_baseline; test_blackout ;;
        3|ratelimit)  test_status; test_baseline; test_ratelimit ;;
        4|loss)       test_status; test_baseline; test_loss ;;
        5|latency)    test_status; test_baseline; test_latency ;;
        6|congestion) test_status; test_baseline; test_congestion ;;
        7|spike)      test_status; test_baseline; test_spike ;;
        8|verify)     test_status; test_both_blackout ;;
        9|all)
            test_status; test_baseline
            test_blackout; test_ratelimit; test_loss
            test_latency; test_congestion; test_spike
            test_both_blackout ;;
        0|q) echo "退出"; exit 0 ;;
        *) warn "无效选项: $1"; return 1 ;;
    esac
}

show_menu() {
    echo ""
    echo -e "${BOLD}========================================${NC}"
    echo -e "${BOLD}    mpfpv 5G 网络拥堵模拟测试${NC}"
    echo -e "${BOLD}========================================${NC}"
    echo ""
    echo -e "  ${CYAN}1${NC}  基准测试        双网卡正常状态基准"
    echo -e "  ${CYAN}2${NC}  轮流断网        每张卡轮流 95%丢包+3s延迟"
    echo -e "  ${CYAN}3${NC}  轮流限速        每张卡轮流 5M→1M→200K→50K"
    echo -e "  ${CYAN}4${NC}  轮流丢包        每张卡轮流 5%→15%→30%→60%"
    echo -e "  ${CYAN}5${NC}  轮流高延迟      每张卡轮流 50ms→200ms→500ms→2s"
    echo -e "  ${CYAN}6${NC}  轮流综合拥堵    每张卡轮流 延迟+丢包+限速"
    echo -e "  ${CYAN}7${NC}  轮流突发恢复    每张卡轮流 突然heavy再恢复"
    echo -e "  ${CYAN}8${NC}  ${RED}双卡同断验证${NC}    两张卡同时断5秒，确认tc生效"
    echo -e "  ${CYAN}9${NC}  全部测试        按顺序跑完 1-8"
    echo -e "  ${CYAN}0${NC}  退出"
    echo ""
}

# === 主入口 ===
check_deps
detect_nics
select_ping_target

if [ $# -gt 0 ]; then
    run_test "$1"
    print_summary
    echo -e "\n${BOLD} 测试完成${NC}"
    exit 0
fi

show_menu
while true; do
    echo -ne "${BOLD}请选择 [1-9, 0退出]: ${NC}"
    read -r choice
    [ -z "$choice" ] && continue
    [ "$choice" = "0" ] || [ "$choice" = "q" ] && { echo "退出"; exit 0; }
    if run_test "$choice"; then
        print_summary
        echo -e "\n${BOLD} 测试完成${NC}\n"
        RESULTS=()
        show_menu
    fi
done
