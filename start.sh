#!/usr/bin/env bash
#
# AIStudio2API 服务器管理脚本
#
#   ./start.sh start     启动；没有可执行文件时自动从源码构建
#   ./start.sh stop      停止（先优雅退出，超时后强制结束并清理残留 Camoufox）
#   ./start.sh restart   重启
#   ./start.sh status    查看进程与生成服务状态
#   ./start.sh log       跟踪日志（Ctrl+C 只退出查看，不影响服务）
#   ./start.sh build     重新构建前端与 Go 程序，完成后 restart 生效
#   ./start.sh rotate    日志超过 50MB 时轮转（enable 后每小时自动执行）
#   ./start.sh enable    开机自启、每小时轮转日志、每分钟守护（写入当前用户 crontab）
#   ./start.sh disable   取消开机自启、日志轮转与守护
#   ./start.sh watchdog  守护检查一次（enable 后 cron 每分钟执行，平时没有输出）：进程不在或 /health 连续无响应时
#                        自动拉起，记录写在 logs/watchdog.log；./start.sh stop 之后不拉起，./start.sh start 后恢复
#   ./start.sh disk      查看账户目录与浏览器缓存的磁盘占用
#   ./start.sh clean-cache  清理没有 Worker 在用的账户浏览器缓存（运行中也可安全执行）
#   ./start.sh diag      诊断回复慢：系统资源、账号冷却、各阶段耗时与结论（MINUTES=120 可扩大统计窗口）
#   ./start.sh report    打包排查资料（诊断、最近日志、程序快照、系统信息，已脱敏）到 reports/，下载后发给开发者
#   ./start.sh trace     打包 /trace/ 排查路由记录的请求详情与重复回复统计到 reports/（含提示词与回复原文）
#
# 可选环境变量：
#   USE_CN_MIRROR=1   构建时使用 goproxy.cn 与 npmmirror 镜像
#   STOP_TIMEOUT=90   停止时等待优雅退出的秒数（默认 90，应大于程序内 SHUTDOWN_GRACE 的 60 秒）
#   START_WAIT=300    启动时等待生成服务进入 RUNNING 的最长秒数
#
# 守护的配置写在 run/watchdog.conf（格式同 .env，优先；程序不会改写它）或 .env，cron 只读这两个文件，每次检查都重新读取：
#   WATCHDOG_HEALTH_FAILS=5   /health 连续失败几次（每分钟一次）判定卡死并重启，0 表示不做卡死检查
#   WATCHDOG_GRACE=300        进程启动后多少秒内不做卡死判定
# 程序只从进程环境读取、写进 .env 无效的变量（CAMOUFOX_PATH、SHUTDOWN_GRACE、LANG/LC_ALL/LC_MESSAGES、TZ、
# http_proxy 等代理变量）cron 里看不到终端 export 的值：写进 run/watchdog.conf，拉起程序时进程环境里没有的从这里补上

set -uo pipefail

APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$APP_DIR" || exit 1
# 物理路径：目录经符号链接访问时（cron 与终端可能用不同写法），按两种路径都能认出本目录的进程
APP_DIR_P="$(pwd -P)"

APP_NAME="aistudio2api"
BIN="$APP_DIR/$APP_NAME"
BIN_P="$APP_DIR_P/$APP_NAME"
RUN_DIR="$APP_DIR/run"
LOG_DIR="$APP_DIR/logs"
PID_FILE="$RUN_DIR/$APP_NAME.pid"
LOG_FILE="$LOG_DIR/$APP_NAME.log"
LOG_MAX_BYTES=$((50 * 1024 * 1024))
STOP_TIMEOUT="${STOP_TIMEOUT:-90}"
START_WAIT="${START_WAIT:-300}"
CRON_MARKER="# aistudio2api-autostart"

# 守护：人为停止标记、start/stop/restart 与 watchdog 共用的锁、守护日志与状态文件
STOP_MARK="$RUN_DIR/stopped"
LOCK_FILE="$RUN_DIR/start.lock"
LOCK_WAIT=390 # 用户命令等锁的上限：STOP_TIMEOUT + START_WAIT
[[ "$STOP_TIMEOUT" =~ ^[0-9]+$ && "$START_WAIT" =~ ^[0-9]+$ ]] && LOCK_WAIT=$((STOP_TIMEOUT + START_WAIT))
WD_LOG="$LOG_DIR/watchdog.log"
WD_LOG_MAX_BYTES=$((5 * 1024 * 1024))
WD_HEALTH_STATE="$RUN_DIR/watchdog.health"
WD_LAUNCHES="$RUN_DIR/watchdog.launches"
WD_THROTTLE="$RUN_DIR/watchdog.throttle"
WD_DEFER="$RUN_DIR/watchdog.deferred"
WD_CONF="$RUN_DIR/watchdog.conf"
WD_LOOP_WINDOW=1800  # 统计连续崩溃的窗口：30 分钟
WD_LOOP_MAX=5        # 窗口内自动拉起达到 5 次即降频
WD_LOOP_INTERVAL=600 # 降频后每 10 分钟最多拉起一次
WD_BOOT_SETTLE=90    # 开机后这么多秒内守护不拉起，交给 @reboot 那一行
WD_HEALTH_WINDOW=150 # 健康检查失败计数的有效期：超过这么多秒没有检查（守护停用、cron 中断）就不算连续，从头计
LOCK_DIR="$RUN_DIR/start.lock.d" # 没有 flock 命令时改用的目录锁
LOCK_MODE=""         # 当前持有的锁：flock、dir 或空
SCRIPT_NAME="${BASH_SOURCE[0]##*/}"
# 程序（与它拉起的 Camoufox）只从进程环境读取的变量：拉起时进程环境里没有的，从 run/watchdog.conf 补上
LAUNCH_ENV_KEYS=(CAMOUFOX_PATH SHUTDOWN_GRACE LANG LC_ALL LC_MESSAGES TZ
    http_proxy https_proxy all_proxy no_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY)
WATCHDOG_MODE=0
STOP_SENT_QUIT=0
LAUNCH_PID=""
LAUNCH_OFFSET=0
LAUNCH_ENV=()
LAUNCH_ENV_SRC=""
LAUNCH_ENV_LANG=""
DRIFT_DOTENV=""
DRIFT_LAUNCH=""

if [[ -t 1 ]]; then
    C_G=$'\e[32m' C_Y=$'\e[33m' C_R=$'\e[31m' C_0=$'\e[0m'
else
    C_G='' C_Y='' C_R='' C_0=''
fi
info() { printf '%s[INFO]%s %s\n' "$C_G" "$C_0" "$*"; }
warn() { printf '%s[WARN]%s %s\n' "$C_Y" "$C_0" "$*" >&2; }
fail() { printf '%s[FAIL]%s %s\n' "$C_R" "$C_0" "$*" >&2; }
have() { command -v "$1" >/dev/null 2>&1; }

trim() {
    local s="$1"
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    printf '%s' "$s"
}

# dotenv_value KEY [FILE]：只读 .env（或 FILE）里的值（与程序解析一致：允许 export 前缀、BOM、引号后的行内注释），没有时为空
dotenv_value() {
    local key="$1" file="${2:-.env}" value="" line quote rest
    [[ -f "$file" ]] || return 0
    line="$(sed '1s/^\xEF\xBB\xBF//' "$file" 2>/dev/null | grep -E "^[[:space:]]*(export[[:space:]]+)?${key}[[:space:]]*=" | tail -n 1 | tr -d '\r')"
    if [[ -n "$line" ]]; then
        value="$(trim "${line#*=}")"
        quote="${value:0:1}"
        if [[ "$quote" == '"' || "$quote" == "'" ]]; then
            rest="${value:1}"
            value="${rest%%"$quote"*}"
        else
            value="$(trim "${value%% #*}")"
        fi
    fi
    printf '%s' "$value"
}

# env_value KEY DEFAULT：与程序相同的优先级，非空的进程环境变量优先，其次 .env
env_value() {
    local key="$1" default="${2-}" value=""
    if [[ -n "${!key-}" ]]; then
        value="${!key}"
    else
        value="$(dotenv_value "$key")"
    fi
    [[ -z "$value" ]] && value="$default"
    printf '%s' "$value"
}

listen_addr() { env_value LISTEN_ADDR "127.0.0.1:2048"; }
listen_port() { local a; a="$(listen_addr)"; printf '%s' "${a##*:}"; }
listen_host() {
    local a h
    a="$(listen_addr)"
    h="${a%:*}"
    h="${h#[}"
    h="${h%]}"
    printf '%s' "$h"
}

# 本机探测地址：通配或回环监听时走 127.0.0.1，否则只能走监听的具体 IP
probe_host() {
    local h
    h="$(listen_host)"
    case "$h" in
        "" | 0.0.0.0 | :: | localhost | 127.*) printf '127.0.0.1' ;;
        ::1) printf '[::1]' ;;
        *:*) printf '[%s]' "$h" ;;
        *) printf '%s' "$h" ;;
    esac
}

# 本机探测地址是回环时才能查询生成服务状态
probe_is_loopback() {
    case "$(probe_host)" in
        127.0.0.1 | "[::1]") return 0 ;;
        *) return 1 ;;
    esac
}

listen_is_public() {
    case "$(listen_host)" in
        localhost | 127.* | ::1) return 1 ;;
        *) return 0 ;;
    esac
}

# admin_token 读取程序生成的管理令牌（控制面 /api/ 必须携带）
admin_token() {
    [[ -f "$APP_DIR/.admin-token" ]] || return 0
    tr -cd '0-9a-fA-F' <"$APP_DIR/.admin-token"
}

http_get() {
    have curl || return 1
    # --noproxy：服务器上常全局设置 http_proxy，本机探测不能走代理
    curl -fsS -m 3 --noproxy '*' -H "X-Admin-Token: $(admin_token)" "http://$(probe_host):$(listen_port)$1" 2>/dev/null
}

service_state() {
    http_get /api/status | json_state
}

json_state() { sed -n 's/.*"state"[[:space:]]*:[[:space:]]*"\([A-Z_]*\)".*/\1/p'; }

# alive PID：进程存在且不是僵尸
alive() {
    local st
    st="$(ps -p "$1" -o stat= 2>/dev/null)"
    [[ -n "$st" && "$st" != Z* ]]
}

pid_by_path() {
    have pgrep || return 1
    local pid bin cwd
    # 逻辑路径与物理路径都认：经符号链接启动与按真实路径启动的命令行前缀不同
    for bin in "$BIN" "$BIN_P"; do
        pid="$(pgrep -f -- "^${bin}( |\$)" 2>/dev/null | head -n 1)"
        if [[ -n "$pid" ]]; then
            printf '%s' "$pid"
            return 0
        fi
        [[ "$BIN_P" == "$BIN" ]] && break
    done
    # 按 README 用 ./aistudio2api 直接启动的实例命令行是相对路径：按进程名查找，再用工作目录确认是本目录的实例
    # （/proc/PID/cwd 是物理路径）
    for pid in $(pgrep -x -- "$APP_NAME" 2>/dev/null); do
        cwd="$(readlink "/proc/$pid/cwd" 2>/dev/null)"
        if [[ -n "$cwd" && ("$cwd" == "$APP_DIR" || "$cwd" == "$APP_DIR_P") ]]; then
            printf '%s' "$pid"
            return 0
        fi
    done
    return 1
}

# pid_is_ours PID：PID 是本目录的 aistudio2api：存活、进程名相符，且工作目录是本目录（start.sh 总是先 cd 到本目录再拉起，
# 与 pid_by_path 的判定一致）。PID 文件里的旧 PID 可能被另一个部署目录的实例复用，只看进程名会把它当成本目录的。
# 读不到工作目录时（其他用户的进程、没有 /proc）只看进程名
pid_is_ours() {
    local cwd
    alive "$1" && [[ "$(ps -p "$1" -o comm= 2>/dev/null)" == "$APP_NAME" ]] || return 1
    cwd="$(readlink "/proc/$1/cwd" 2>/dev/null)" || return 0
    [[ -z "$cwd" || "$cwd" == "$APP_DIR" || "$cwd" == "$APP_DIR_P" ]]
}

find_pid() {
    local pid=""
    if [[ -f "$PID_FILE" ]]; then
        pid="$(tr -cd '0-9' <"$PID_FILE")"
        if [[ -n "$pid" ]] && pid_is_ours "$pid"; then
            printf '%s' "$pid"
            return 0
        fi
    fi
    pid_by_path
}

# owned_by_app PID：沿父进程链向上查找，存在仍在运行的 aistudio2api 进程（主程序或 setup --login）时返回 0
owned_by_app() {
    local pid="$1" depth parent
    for depth in 1 2 3 4 5 6 7 8; do
        parent="$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')"
        [[ -z "$parent" || "$parent" == 0 || "$parent" == 1 ]] && return 1
        [[ "$(ps -p "$parent" -o comm= 2>/dev/null)" == "$APP_NAME" ]] && return 0
        pid="$parent"
    done
    return 1
}

kill_orphan_camoufox() {
    have pgrep || return 0
    local pids="" pid
    # 只清理没有存活 aistudio2api 父进程的 Camoufox：同目录另一个实例或正在进行的 setup --login 的浏览器不受影响
    for pid in $(pgrep -f -- "^$APP_DIR/runtime/camoufox/" 2>/dev/null); do
        owned_by_app "$pid" || pids+="$pid "
    done
    pids="$(trim "$pids")"
    [[ -z "$pids" ]] && return 0
    warn "清理残留的 Camoufox 进程：$pids"
    # shellcheck disable=SC2086
    kill -KILL $pids 2>/dev/null || true
}

check_camoufox_deps() {
    have ldd || return 0
    local dir="$APP_DIR/runtime/camoufox" f out missing="" versions=""
    for f in "$dir/camoufox-bin" "$dir/libxul.so"; do
        [[ -f "$f" ]] || continue
        # libmozgtk.so、libmozsandbox.so 等是 Camoufox 自带的库，运行时从安装目录加载；
        # 检查时同样把安装目录加入搜索路径，只报告真正缺失的系统库
        out="$(LD_LIBRARY_PATH="$dir${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" ldd "$f" 2>&1)"
        missing+="$(printf '%s\n' "$out" | awk '/=> not found/ {print $1}') "
        versions+="$(printf '%s\n' "$out" | grep -oE "version .[A-Za-z_]+[0-9.]*. not found" |
            sed -E 's/version .([A-Za-z_]+[0-9.]*). not found/\1/') "
    done
    missing="$(trim "$(printf '%s' "$missing" | tr ' ' '\n' | sed '/^$/d' | sort -u | tr '\n' ' ')")"
    versions="$(trim "$(printf '%s' "$versions" | tr ' ' '\n' | sed '/^$/d' | sort -u | tr '\n' ' ')")"
    if [[ -n "$missing" ]]; then
        warn "Camoufox 缺少系统库：$missing"
        warn "Debian/Ubuntu：sudo apt install libgtk-3-0 libasound2 libnss3 libdbus-glib-1-2 libxtst6 libxrandr2 libgbm1 libxkbcommon0 libpango-1.0-0 libcairo2 libxcomposite1 libxdamage1 libxfixes3 fonts-liberation"
        warn "Ubuntu 24.04 及以后把 libasound2 换成 libasound2t64"
    fi
    if [[ -n "$versions" ]]; then
        warn "系统运行库版本过低，Camoufox 需要：$versions；请升级系统，或在 .env 设置 WAA_BACKEND=go"
    fi
}

check_api_key() {
    listen_is_public || return 0
    local key
    key="$(env_value PROXY_API_KEY "")"
    [[ -n "$key" && "$key" != "sk-onechat-fun-fun" ]] && return 0
    warn "LISTEN_ADDR=$(listen_addr) 对外监听，但 PROXY_API_KEY 未设置或为公开的默认密钥 sk-onechat-fun-fun：知道默认密钥的人都能调用 API、消耗你的账号额度！"
    warn "建议在 .env 中设置，例如：PROXY_API_KEY=$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')"
}

# rotate_log 日志超过上限时轮转：复制为 .1 后原地截断。
# 程序以追加方式写日志，截断后继续写在新文件开头，运行中也可以安全执行。
rotate_log() {
    [[ -f "$LOG_FILE" ]] || return 0
    local size
    size="$(stat -c %s "$LOG_FILE" 2>/dev/null || wc -c <"$LOG_FILE")"
    if ((size > LOG_MAX_BYTES)); then
        cp -f "$LOG_FILE" "$LOG_FILE.1" && : >"$LOG_FILE"
        info "日志已轮转：$LOG_FILE.1"
    fi
}

api_hint() {
    local h
    h="$(listen_host)"
    case "$h" in
        "" | 0.0.0.0 | ::) h="<服务器IP>" ;;
    esac
    printf 'http://%s:%s/v1' "$h" "$(listen_port)"
}

admin_hint() {
    local token pw
    token="$(admin_token)"
    if [[ -n "$token" ]]; then
        info "管理页面：http://$(probe_host):$(listen_port)/?admin_token=$token（打开一次后浏览器记住登录；令牌保存在 .admin-token，删除后重启即更换）"
    fi
    listen_is_public || return 0
    pw="$(env_value ADMIN_PASSWORD "")"
    if [[ -n "$pw" ]]; then
        info "远程管理：$(api_hint | sed 's#/v1$##')（浏览器弹出登录框：用户名随意，密码为 .env 中的 ADMIN_PASSWORD；也可以用上面的令牌地址并把主机换成服务器 IP）"
        ((${#pw} >= 12)) || warn "ADMIN_PASSWORD 少于 12 位，暴露在外网时建议使用更长的随机密码"
    else
        info "远程管理：把上面令牌地址中的主机换成服务器 IP 打开，或在 .env 设置 ADMIN_PASSWORD 用密码登录；未配置 HTTPS 时令牌与密码均为明文传输"
    fi
}

build() {
    local missing=() tool
    for tool in go node npm; do
        have "$tool" || missing+=("$tool")
    done
    if ((${#missing[@]})); then
        fail "源码构建需要 Go 1.25+、Node.js 22.13+ 或 24+、npm；缺少：${missing[*]}"
        return 1
    fi
    if [[ ! -f web/package-lock.json ]]; then
        fail "缺少 web/package-lock.json，源码不完整"
        return 1
    fi
    local node_major
    node_major="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null)"
    [[ "$node_major" =~ ^[0-9]+$ ]] || node_major=0
    ((node_major >= 22)) || warn "Node.js 版本为 $(node -v 2>/dev/null)，前端构建要求 22.13+ 或 24+"
    info "$(go version)"

    local npm_args=()
    if [[ "${USE_CN_MIRROR:-0}" == "1" ]]; then
        export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
        npm_args+=(--registry=https://registry.npmmirror.com)
        info "使用国内镜像：GOPROXY=$GOPROXY，npm registry=registry.npmmirror.com"
    fi

    # 依赖未变化时跳过 npm ci，重复构建快很多
    if [[ ! -d web/node_modules || web/package-lock.json -nt web/node_modules/.package-lock.json ]]; then
        info "安装前端依赖..."
        if ! (cd web && npm ci ${npm_args[@]+"${npm_args[@]}"}); then
            fail "前端依赖安装失败；网络不通可尝试 USE_CN_MIRROR=1 ./start.sh build"
            return 1
        fi
    else
        info "前端依赖未变化，跳过 npm ci"
    fi
    info "构建前端..."
    if ! (cd web && npm run build); then
        # npm run build = 严格类型检查 + 打包；类型检查不影响运行，失败时退回只打包
        warn "前端严格类型检查未通过，改为跳过类型检查直接打包（不影响运行；详情可在 web 目录执行 npm run typecheck）"
        if ! (cd web && ./node_modules/.bin/vite build); then
            fail "前端打包失败"
            return 1
        fi
    fi

    info "构建 Go 程序..."
    if ! go build -trimpath -o "$BIN.tmp" ./cmd/aistudio2api; then
        rm -f "$BIN.tmp"
        fail "Go 构建失败；Go 低于 1.25 时会自动下载工具链，网络不通可尝试 USE_CN_MIRROR=1 或安装 Go 1.25+"
        return 1
    fi
    # 先写临时文件再替换，运行中的进程不受影响
    mv -f "$BIN.tmp" "$BIN"
    chmod +x "$BIN"
    info "构建完成：$BIN"
}

wait_ready() {
    local pid="$1" offset="$2" deadline=$((SECONDS + START_WAIT)) state="" last="" auto newpid
    auto="$(env_value AUTO_START true | tr '[:upper:]' '[:lower:]')"
    if ! have curl; then
        warn "未安装 curl，跳过就绪检查；用 ./start.sh log 查看启动过程"
        return 0
    fi
    info "等待服务就绪（最长 ${START_WAIT}s；Ctrl+C 只退出等待，不影响服务）"
    while ((SECONDS < deadline)); do
        if ! alive "$pid"; then
            fail "进程已退出，最近日志："
            tail -n 30 "$LOG_FILE" >&2
            check_camoufox_deps
            # 不删 PID 文件：find_pid 会校验进程是否存活，守护据此记录“PID 文件里的进程 N 已不存在”并检查是否被 OOM 结束；
            # 等待期间不持锁，守护可能已经重新拉起，这时 PID 文件指向新进程
            if newpid="$(find_pid)" && [[ "$newpid" != "$pid" ]]; then
                warn "守护已自动重新拉起（PID $newpid），用 ./start.sh status 查看"
            fi
            return 1
        fi
        if ! probe_is_loopback; then
            if http_get /health >/dev/null; then
                info "管理监听已就绪；LISTEN_ADDR 绑定在具体网卡 IP，本机无法查询生成服务状态，请用 ./start.sh log 确认"
                return 0
            fi
        else
            state="$(service_state)"
            if [[ -n "$state" && "$state" != "$last" ]]; then
                info "生成服务状态：$state"
                last="$state"
            fi
            if [[ "$state" == "RUNNING" ]]; then
                info "API 已就绪：$(api_hint)"
                admin_hint
                return 0
            fi
            if [[ "$state" == "STOPPED" ]]; then
                if [[ "$auto" == "false" || "$auto" == "0" ]]; then
                    warn "AUTO_START=false，生成服务保持 STOPPED，需在管理页面手动启动"
                    return 0
                fi
                if tail -c +$((offset + 1)) "$LOG_FILE" 2>/dev/null | grep -qF "没有可用账户"; then
                    warn "没有可用账户：账户未载入、全部停用或凭据全部失效（程序会每隔几分钟自动重试）"
                    account_diagnose "$offset"
                    return 0
                fi
            fi
        fi
        sleep 2
    done
    warn "等待超时，服务仍在启动或反复失败，最近日志："
    tail -n 20 "$LOG_FILE" >&2
    check_camoufox_deps
    return 0
}

# account_diagnose OFFSET：没有可用账户时给出排查信息
account_diagnose() {
    local offset="$1" auth_dir loaded dirs d f
    auth_dir="$(env_value AISTUDIO_AUTH_STATES auth)"
    loaded="$(tail -c +$((offset + 1)) "$LOG_FILE" 2>/dev/null | grep -oE "运行时装配 \| 2/3 \|[^\"]*账户=[0-9]+" | tail -n 1 | grep -oE "[0-9]+$")"
    info "程序载入的账户数：${loaded:-未知}（账户目录：$auth_dir）"
    if [[ -d "$auth_dir" ]]; then
        dirs=0
        for d in "$auth_dir"/*/; do
            [[ -d "$d" ]] || continue
            dirs=$((dirs + 1))
            for f in account.json storage-state.json; do
                [[ -f "$d$f" ]] || warn "$d 缺少 $f"
            done
            if [[ -f "${d}account.json" ]] && grep -qE '"enabled"[[:space:]]*:[[:space:]]*false' "${d}account.json"; then
                warn "$d 已停用（account.json 中 enabled=false）"
            fi
            if [[ -f "${d}storage-state.json" ]] && grep -qE '"browser"[[:space:]]*:[[:space:]]*"chrome"' "${d}storage-state.json"; then
                warn "$d 是 Windows Chrome 导入的账户，Linux 上无法续签，请改用 setup --login 重新登录"
            fi
        done
        info "$auth_dir/ 下共有 $dirs 个账户目录"
    else
        warn "账户目录 $auth_dir 不存在；账户应放在 $APP_DIR/$auth_dir/<邮箱>/"
    fi
    local fails
    fails="$(tail -c +$((offset + 1)) "$LOG_FILE" 2>/dev/null | grep -oE "模型目录同步失败[^\"]*" | cut -c1-200 | head -n 5)"
    if [[ -n "$fails" ]]; then
        warn "账户同步错误（前 5 条）："
        printf '%s\n' "$fails" >&2
    fi
}

# launch_app LABEL：清理残留 Camoufox、写主日志分隔行并在后台拉起程序，用户的 start 与守护共用。
# 成功时返回 0，LAUNCH_PID 为程序 PID 并写入 PID 文件；进程启动后立即退出时返回 1，
# PID 文件写入拉起时的 PID（LAUNCH_PID），守护下一轮记录的原因指向这次退出的进程
launch_app() {
    local label="$1" launched pid run=()
    kill_orphan_camoufox
    mkdir -p "$RUN_DIR" "$LOG_DIR"
    rotate_log
    # 只从进程环境读取的变量：进程环境里没有的从 run/watchdog.conf 补上（cron 里看不到终端 export 的值），只显示键名
    launch_env_plan
    [[ -n "$LAUNCH_ENV_SRC" ]] && info "进程环境里没有、按 $WD_CONF 补上：$LAUNCH_ENV_SRC"
    [[ -n "$LAUNCH_ENV_LANG" ]] && info "进程环境里没有语言设置（cron 里通常如此），按系统默认补上 LANG=$LAUNCH_ENV_LANG"
    printf '\n===== %s start.sh %s =====\n' "$(date '+%F %T')" "$label" >>"$LOG_FILE"
    LAUNCH_OFFSET="$(stat -c %s "$LOG_FILE" 2>/dev/null || wc -c <"$LOG_FILE")"

    # 软上限提到硬上限：从终端与从 cron 拉起的进程打开文件数上限一致（cron 里的软上限常常只有 1024）
    ulimit -n "$(ulimit -Hn)" 2>/dev/null || true
    # 补上的变量经 env 命令只传给程序（env 再 exec 程序，PID 不变），不改 start.sh 自己的环境
    run=("$BIN" -open-ui=false)
    ((${#LAUNCH_ENV[@]})) && run=(env "${LAUNCH_ENV[@]}" "$BIN" -open-ui=false)
    # 9>&-：锁的文件描述符不传给程序，否则程序运行期间一直占着锁，守护与之后的 start/stop 都拿不到
    if have setsid; then
        nohup setsid "${run[@]}" >>"$LOG_FILE" 2>&1 </dev/null 9>&- &
    else
        nohup "${run[@]}" >>"$LOG_FILE" 2>&1 </dev/null 9>&- &
    fi
    launched=$!
    LAUNCH_PID="$launched"
    sleep 1
    if alive "$launched" && [[ "$(ps -p "$launched" -o comm= 2>/dev/null)" == "$APP_NAME" ]]; then
        pid="$launched"
    else
        pid="$(pid_by_path)"
    fi
    if [[ -z "$pid" ]]; then
        printf '%s\n' "$launched" >"$PID_FILE"
        return 1
    fi
    LAUNCH_PID="$pid"
    printf '%s\n' "$pid" >"$PID_FILE"
}

start() {
    local pid rc
    # 用户执行 start/restart：清空守护的连续拉起计数与降频
    rm -f "$WD_LAUNCHES" "$WD_THROTTLE" "$WD_DEFER"
    if pid="$(find_pid)"; then
        rm -f "$STOP_MARK"
        lock_release
        info "已在运行（PID $pid）"
        status_detail
        return 0
    fi
    if [[ ! -x "$BIN" ]]; then
        info "未找到可执行文件 $APP_NAME，开始从源码构建"
        build || return 1
    fi
    if [[ ! -f .env && -f .env.example ]]; then
        cp .env.example .env
        chmod 600 .env
        warn "未找到 .env，已从 .env.example 复制；修改后执行 ./start.sh restart"
    fi
    check_api_key
    check_camoufox_deps

    if ! launch_app start; then
        lock_release
        fail "进程启动后立即退出，最近日志："
        tail -n 30 "$LOG_FILE" >&2
        return 1
    fi
    # 拉起成功：删除人为停止标记，守护恢复照看；PID 文件已写好，等待就绪期间不再持锁（守护按启动宽限期不做判定）
    rm -f "$STOP_MARK"
    lock_release
    info "已启动（PID $LAUNCH_PID，打开文件数上限 $(ulimit -n)），日志：$LOG_FILE"
    wait_ready "$LAUNCH_PID" "$LAUNCH_OFFSET"
    rc=$?
    ((rc == 0)) && health_mark_ok "$LAUNCH_PID"
    # 开了守护时当场提示：这次拉起的进程环境里有、守护或开机自启拉起时会丢掉的变量（只列键名）
    if ((rc == 0)) && watchdog_in_cron && pid="$(find_pid)"; then
        env_drift_warn "$pid"
    fi
    return "$rc"
}

stop() {
    local pid i
    # 用户执行 stop：写人为停止标记，守护不再拉起，直到 start 成功拉起进程。
    # restart 不写：它从停止到拉起写好 PID 文件一直持锁，守护插不进来；中途被打断（SSH 掉线、Ctrl+C）或拉起后立即退出时，
    # 用户的本意仍是让程序运行，交给守护按正常规则拉起（连续失败有崩溃循环保护）。守护自己的重启也不写
    if [[ "$WATCHDOG_MODE" != 1 && "${cmd:-}" == stop ]]; then
        mkdir -p "$RUN_DIR"
        printf '%s 用户执行 stop\n' "$(date '+%F %T')" >"$STOP_MARK"
    fi
    if ! pid="$(find_pid)"; then
        info "未在运行"
        rm -f "$PID_FILE"
        kill_orphan_camoufox
        return 0
    fi
    info "正在停止（PID $pid）..."
    kill -TERM "$pid" 2>/dev/null
    for ((i = 0; i < STOP_TIMEOUT; i++)); do
        alive "$pid" || break
        sleep 1
    done
    if alive "$pid" && [[ "$WATCHDOG_MODE" == 1 ]]; then
        # 守护判定卡死后的重启：先发 SIGQUIT，程序没有处理它，Go 运行时把全部 goroutine 的栈写进主日志后退出，
        # 事后能看出卡在哪里；5 秒内仍未退出（例如被 SIGSTOP 暂停）再强制结束。用户执行的 stop 不变
        warn "${STOP_TIMEOUT}s 内未退出，发送 SIGQUIT（goroutine 栈写进主日志 $LOG_FILE）"
        kill -QUIT "$pid" 2>/dev/null && STOP_SENT_QUIT=1
        for ((i = 0; i < 5; i++)); do
            alive "$pid" || break
            sleep 1
        done
    fi
    if alive "$pid"; then
        if ((STOP_SENT_QUIT)); then
            warn "SIGQUIT 后 5s 仍未退出，强制结束"
        else
            warn "${STOP_TIMEOUT}s 内未退出，强制结束"
        fi
        kill -KILL "$pid" 2>/dev/null
        sleep 1
    fi
    rm -f "$PID_FILE"
    kill_orphan_camoufox
    info "已停止"
    if [[ "$WATCHDOG_MODE" != 1 && "${cmd:-}" == stop ]] && watchdog_in_cron; then
        info "已记录人为停止（$STOP_MARK），守护不会自动拉起；执行 ./start.sh start 后恢复"
    fi
}

status_detail() {
    info "监听：$(listen_addr)    日志：$LOG_FILE"
    if ! probe_is_loopback; then
        info "LISTEN_ADDR 绑定在具体网卡 IP，本机无法查询生成服务状态"
        return 0
    fi
    local body state accounts
    if ! body="$(http_get /api/status)"; then
        warn "管理接口暂无响应（可能仍在启动）"
        return 0
    fi
    state="$(printf '%s' "$body" | json_state)"
    accounts="$(printf '%s' "$body" | sed -n 's/.*"accounts"[[:space:]]*:[[:space:]]*\({[^}]*}\).*/\1/p')"
    info "生成服务：${state:-未知}    账户：${accounts:-未知}"
    admin_hint
}

status() {
    local pid
    if ! pid="$(find_pid)"; then
        info "未运行"
        watchdog_status
        return 3
    fi
    info "运行中（PID $pid，已运行 $(trim "$(ps -p "$pid" -o etime= 2>/dev/null)")）"
    status_detail
    watchdog_status "$pid"
}

# auth_directory 返回账户目录（与程序相同：环境变量优先，其次 .env，默认 auth）
auth_directory() { env_value AISTUDIO_AUTH_STATES auth; }

# disk_usage 账户越多、预热过的账户越多，各账户目录下的 camoufox-cache 累积越多（每个上限约 256MB）
disk_usage() {
    local auth_dir
    auth_dir="$(auth_directory)"
    [[ -d "$auth_dir" ]] || { fail "账户目录不存在：$auth_dir"; return 1; }
    info "账户目录：$auth_dir    总占用：$(du -sh "$auth_dir" 2>/dev/null | cut -f1)"
    info "其中浏览器缓存：$(du -sch "$auth_dir"/*/camoufox-cache 2>/dev/null | tail -n 1 | cut -f1)"
    info "所在分区：$(df -h "$auth_dir" | awk 'NR==2 {print "已用 " $3 " / 共 " $2 "（" $5 "），剩余 " $4}')"
    info "缓存最大的 5 个账户："
    du -s "$auth_dir"/*/camoufox-cache 2>/dev/null | sort -rn | head -n 5 |
        awk '{printf "    %6d MB  %s\n", $1 / 1024, $2}'
}

# clean_cache 清理没有 Worker 在用的账户浏览器缓存，释放磁盘。
# Worker 使用缓存时持有 camoufox-cache/.lock 的文件锁（与 flock 命令是同一种锁），
# 这里在持锁状态下清空，拿不到锁说明正在使用，直接跳过；服务运行中执行也安全。
# 被清理的账户下次启动 Worker 时会重新下载页面资源，首次启动稍慢
clean_cache() {
    local auth_dir d size freed=0 cleaned=0 skipped=0
    auth_dir="$(auth_directory)"
    [[ -d "$auth_dir" ]] || { fail "账户目录不存在：$auth_dir"; return 1; }
    have flock || { fail "缺少 flock 命令（util-linux），无法判断缓存是否在用，已放弃"; return 1; }
    info "清理前浏览器缓存：$(du -sch "$auth_dir"/*/camoufox-cache 2>/dev/null | tail -n 1 | cut -f1)"
    for d in "$auth_dir"/*/camoufox-cache; do
        [[ -d "$d" ]] || continue
        if ! flock -n "$d/.lock" true 2>/dev/null; then
            skipped=$((skipped + 1))
            continue
        fi
        size="$(du -sk "$d" 2>/dev/null | cut -f1)"
        if flock -n "$d/.lock" find "$d" -mindepth 1 -maxdepth 1 ! -name .lock -exec rm -rf {} + 2>/dev/null; then
            freed=$((freed + ${size:-0}))
            cleaned=$((cleaned + 1))
        else
            skipped=$((skipped + 1))
        fi
    done
    info "已清理 $cleaned 个账户的缓存，释放约 $((freed / 1024)) MB；$skipped 个正在使用，已跳过"
    info "清理后浏览器缓存：$(du -sch "$auth_dir"/*/camoufox-cache 2>/dev/null | tail -n 1 | cut -f1)"
}

# ============================ 守护（./start.sh watchdog，enable 后 cron 每分钟执行） ============================
#
# 判定规则（重启要重新预热全部 Worker，代价高，宁可漏判也不误判）：
#   - 有人为停止标记 run/stopped（./start.sh stop 写入，start 成功拉起进程后删除）时什么都不做
#   - 与 start/stop/restart 共用锁 run/start.lock：用户命令阻塞等锁，守护拿不到锁就跳过本轮
#   - 进程不在：记录原因与主日志末尾（已打码），以守护身份拉起（不构建，不写人为停止标记）
#   - 进程在：启动 WATCHDOG_GRACE 秒后 /health 连续 WATCHDOG_HEALTH_FAILS 次（每分钟一次）失败才重启，
#     成功一次就清零；这个进程在这个地址上应答过 /health 之后失败才计数，地址不对不会被当成卡死
#   - 30 分钟内已自动拉起 5 次就改为每 10 分钟最多拉起一次，用户执行 start/restart 时清空计数

# lock_acquire wait|try：拿 start/stop/restart 与守护共用的锁。有 flock 时锁 run/start.lock（文件描述符 9，进程退出即释放）；
# 没有 flock 时用目录锁 run/start.lock.d（见 lock_dir_take）。
# 返回 0 拿到锁；1 没拿到（try 时锁被占，或 wait 超时）；2 建不了锁目录，不加锁照常执行
lock_acquire() {
    local mode="$1" deadline rc
    mkdir -p "$RUN_DIR" 2>/dev/null
    if have flock; then
        exec 9>>"$LOCK_FILE" || return 1
        if flock -n 9; then
            LOCK_MODE=flock
            return 0
        fi
        if [[ "$mode" == try ]]; then
            exec 9>&-
            return 1
        fi
        info "另一个 start.sh 命令或守护的自动拉起正在进行，等待其完成（最长 ${LOCK_WAIT}s）..."
        if flock -w "$LOCK_WAIT" 9; then
            LOCK_MODE=flock
            return 0
        fi
        exec 9>&-
        fail "等待 ${LOCK_WAIT}s 仍未拿到锁（$LOCK_FILE），请稍后重试"
        return 1
    fi
    lock_dir_take
    rc=$?
    ((rc != 1)) && return "$rc"
    [[ "$mode" == try ]] && return 1
    info "另一个 start.sh 命令或守护的自动拉起正在进行，等待其完成（最长 ${LOCK_WAIT}s）..."
    deadline=$((SECONDS + LOCK_WAIT))
    while ((SECONDS < deadline)); do
        sleep 1
        lock_dir_take
        rc=$?
        ((rc != 1)) && return "$rc"
    done
    fail "等待 ${LOCK_WAIT}s 仍未拿到锁（$LOCK_DIR），请稍后重试"
    return 1
}

# lock_dir_take：没有 flock 时的目录锁。mkdir 是原子操作，建成功的拿到锁，目录里的 pid 记持锁的 start.sh，
# 正常退出（含 Ctrl+C、SSH 掉线）时由 EXIT trap 删除；持锁进程已不在（被 kill -9）时视为残留，清掉后重试一次。
# 返回 0 拿到锁；1 锁被占；2 建不了锁目录
lock_dir_take() {
    local holder m
    if mkdir "$LOCK_DIR" 2>/dev/null; then
        lock_dir_own
        return 0
    fi
    [[ -d "$LOCK_DIR" ]] || return 2
    holder="$({ tr -cd '0-9' <"$LOCK_DIR/pid"; } 2>/dev/null)"
    if [[ -n "$holder" ]]; then
        alive "$holder" && [[ "$(ps -p "$holder" -o args= 2>/dev/null)" == *"$SCRIPT_NAME"* ]] && return 1
    else
        # 刚建好目录、还没写 pid：10 秒后仍为空才当作残留
        m="$(file_mtime "$LOCK_DIR")"
        [[ "$m" =~ ^[0-9]+$ ]] && (($(date +%s) - m < 10)) && return 1
    fi
    rm -f "$LOCK_DIR/pid"
    rmdir "$LOCK_DIR" 2>/dev/null
    mkdir "$LOCK_DIR" 2>/dev/null || return 1
    lock_dir_own
}

lock_dir_own() {
    { printf '%s\n' "$$" >"$LOCK_DIR/pid"; } 2>/dev/null
    LOCK_MODE=dir
    trap lock_release EXIT
}

lock_release() {
    case "$LOCK_MODE" in
        flock)
            { flock -u 9; } 2>/dev/null
            exec 9>&-
            ;;
        dir)
            # 只删自己持有的锁目录
            if [[ "$({ tr -cd '0-9' <"$LOCK_DIR/pid"; } 2>/dev/null)" == "$$" ]]; then
                rm -f "$LOCK_DIR/pid"
                rmdir "$LOCK_DIR" 2>/dev/null
            fi
            trap - EXIT
            ;;
    esac
    LOCK_MODE=""
}

# user_lock：用户的 start/stop/restart 先拿锁；建不了锁目录时不加锁照常执行
user_lock() {
    lock_acquire wait
    (($? != 1))
}

# watchdog_env：cron 的 PATH 只有 /usr/bin:/bin，HOME 等也可能缺失；补全后拉起的程序与从终端启动时一致
watchdog_env() {
    PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${PATH:+:$PATH}"
    if [[ -z "${HOME-}" ]]; then
        HOME="$(getent passwd "$(id -u)" 2>/dev/null | cut -d: -f6)"
        [[ -n "$HOME" ]] || HOME=/
    fi
    export PATH HOME
}

# wd_setting KEY：守护配置在文件里的值（cron 只读文件）：run/watchdog.conf 优先（程序不会改写它），其次 .env；都没有时为空。
# 管理页保存服务配置时程序按自己的配置项重写 .env，会去掉 WATCHDOG_*，写在 run/watchdog.conf 里不受影响
wd_setting() {
    local v
    v="$(dotenv_value "$1" "$WD_CONF")"
    [[ -n "$v" ]] || v="$(dotenv_value "$1")"
    printf '%s' "$v"
}

# wd_value KEY：非空的进程环境变量优先（手动执行时可临时覆盖），其次 wd_setting
wd_value() {
    if [[ -n "${!1-}" ]]; then
        printf '%s' "${!1}"
    else
        wd_setting "$1"
    fi
}

wd_log() {
    { printf '%s [%s] %s\n' "$(date '+%F %T')" "$1" "$2" >>"$WD_LOG"; } 2>/dev/null
}

# wd_detail：把标准输入作为上一条记录的附加行写进守护日志（调用方先打码）
wd_detail() {
    { sed 's/^/    | /' >>"$WD_LOG"; } 2>/dev/null
}

wd_rotate_log() {
    [[ -f "$WD_LOG" ]] || return 0
    local size
    size="$(stat -c %s "$WD_LOG" 2>/dev/null || wc -c <"$WD_LOG")"
    if ((size > WD_LOG_MAX_BYTES)); then
        mv -f "$WD_LOG" "$WD_LOG.1"
    fi
    return 0
}

# redact：打码代理账号密码（scheme://user:pass@、user:pass@host、host:port:user:pass）、邮箱（保留前两位）、
# sk- 开头与 AIza 开头的密钥、Bearer/Basic 凭据、Google 登录 Cookie、查询参数里的 key/token、password/token/api-key 等键值，
# 以及 .admin-token、ADMIN_PASSWORD、PROXY_API_KEY 的原值；规则与 tools/report.sh 一致，宁可多打码。
# 账号密码段一直匹配到这一段里最后一个 @（密码里可能有 / 或 @）
redact() {
    local secrets cookies='SAPISID|APISID|SSID|HSID|SIDCC|SID|__Secure-[A-Za-z0-9-]+|NID'
    secrets="$(admin_token)"$'\n'"$(env_value ADMIN_PASSWORD "")"$'\n'"$(env_value PROXY_API_KEY "")"
    LC_ALL=C sed -E \
        -e 's#([A-Za-z][A-Za-z0-9+.-]*://)[^[:space:]"'"'"']*@#\1***@#g' \
        -e 's#(^|[^A-Za-z0-9._%+*-])[A-Za-z0-9._%+-]+:[^[:space:]"'"'"'/][^[:space:]"'"'"']*@#\1***:***@#g' \
        -e 's#(([0-9]{1,3}\.){3}[0-9]{1,3}|([A-Za-z0-9-]+\.)+[A-Za-z]{2,}):([0-9]{2,5}):[^:[:space:]"'"'"'/]+:[^[:space:]"'"'"']+#\1:\4:***:***#g' \
        -e 's#([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}#\1***@***#g' \
        -e 's#(^|[^A-Za-z0-9])sk-[A-Za-z0-9_-]{4,}#\1sk-***#g' \
        -e 's#AIza[0-9A-Za-z_-]{30,}#AIza***#g' \
        -e 's#([?&](key|token|access_token|api_key|admin_token)=)[^&[:space:]"'"'"'<]+#\1***#gI' \
        -e 's#(bearer[[:space:]]+)[A-Za-z0-9._~+/=-]{8,}#\1***#gI' \
        -e 's#(basic[[:space:]]+)[A-Za-z0-9+/=]{8,}#\1***#gI' \
        -e "s#((${cookies})=)[^;[:space:]\"']+#\\1***#gI" \
        -e "s#(\"name\"[[:space:]]*:[[:space:]]*\"(${cookies})\"[[:space:]]*,[[:space:]]*\"value\"[[:space:]]*:[[:space:]]*\")[^\"]*#\\1***#gI" \
        -e 's#(^|[^A-Za-z0-9_])((admin_token|x-admin-token|token|password|passwd|secret|x-goog-api-key|x-api-key|api-key|proxy_api_key|api_key|apikey)"?[=:][[:space:]]*"?)[^[:space:]"&,;]+#\1\2***#gI' |
        WD_SECRETS="$secrets" LC_ALL=C awk '
            BEGIN { n = split(ENVIRON["WD_SECRETS"], s, "\n") }
            {
                line = $0
                for (i = 1; i <= n; i++) {
                    if (length(s[i]) < 6) continue
                    out = ""
                    while ((p = index(line, s[i])) > 0) {
                        out = out substr(line, 1, p - 1) "***"
                        line = substr(line, p + length(s[i]))
                    }
                    line = out line
                }
                print line
            }'
}

# utf8_cut N：每行最多保留 N 个字符（按 UTF-8 计，不依赖 locale；cron 里通常没有 LANG），截断处以 … 结尾
utf8_cut() {
    LC_ALL=C awk -v n="$1" '
        BEGIN { for (i = 128; i < 192; i++) cont[sprintf("%c", i)] = 1 }
        length($0) <= n { print; next }
        {
            out = ""; c = 0; len = length($0)
            if (len > 4 * n + 4) len = 4 * n + 4
            for (i = 1; i <= len; i++) {
                ch = substr($0, i, 1)
                if (!(ch in cont) && ++c > n) break
                out = out ch
            }
            print (i <= length($0) ? out "…" : out)
        }'
}

# json_unescape：把 slog JSON 字符串值里常见的转义还原（引号、<>&、换行制表、反斜杠）
json_unescape() {
    LC_ALL=C sed -e 's/\\"/"/g' -e 's/\\u003c/</g' -e 's/\\u003e/>/g' -e 's/\\u0026/\&/g' -e 's/\\[nrt]/ /g' -e 's/\\\\/\\/g'
}

# json_time LINE：slog JSON 行的 time 字段，换成本地时间（date 不认时按原样）
json_time() {
    local t
    t="$(printf '%s\n' "$1" | LC_ALL=C sed -nE 's/^\{("[a-z_]+":"([^"\\]|\\.)*",)*"time":"([^"]*)".*/\3/p')"
    [[ -n "$t" ]] || return 0
    date -d "$t" '+%F %T' 2>/dev/null || printf '%s' "$t"
}

# goroutine_summary：从标准输入（主日志里一个进程的输出）最后一次 SIGQUIT 的栈里，按“等待原因 + 第一个非标准库函数”
# 统计 goroutine 个数（前 8 项）。大量 goroutine 停在同一个锁或调用上时，这里能直接看出卡在哪里
goroutine_summary() {
    LC_ALL=C awk '
        /^SIGQUIT: quit/ { split("", c); n = 0; in_g = 0; quit = 1; next }
        !quit { next }
        /^goroutine [0-9]+ .*\[.*\]:$/ {
            if (in_g) c[key]++
            r = $0; sub(/^[^[]*\[/, "", r); sub(/\]:$/, "", r)
            sub(/, locked to thread/, "", r); sub(/, [0-9]+ minutes?/, "", r)
            key = r; fn = ""; in_g = 1; n++; next
        }
        in_g && fn == "" && /^[a-z0-9.-]+\.[a-z]+\// {
            fn = $0; sub(/\([^()]*\)$/, "", fn); key = key "  " fn
        }
        END {
            if (in_g) c[key]++
            if (!n) exit
            print "SIGQUIT 时共 " n " 个 goroutine，按等待原因与第一个非标准库函数计数（前 8 项）："
            fflush()
            for (k in c) printf "%7d  %s\n", c[k], k | "sort -rn | head -n 8"
        }'
}

# wd_log_tail [exited|hung]：主日志里最近一次启动之后（最后一条 start.sh 分隔行之后的整段，即这个进程的输出）的线索，先打码再截短：
#   - 崩溃输出的开头：第一行 panic: / fatal error: / runtime: / SIGxxx: / unexpected fault 及其后 7 行
#     （fatal error 会打印全部 goroutine 的栈，远超 20 行，开头不能丢）；没有这类行时取最后一条 JSON 之后的开头 8 行
#   - 最后 20 行里的非 JSON 行
#   - 最后一条 level=ERROR 的时间、msg 与 error 字段（各取前 200 字；启动失败的具体原因在 error 字段里），以及最后一条记录的时间
#     （高并发下最后一条 ERROR 常常是几小时前与退出无关的单个请求失败，对照时间才不会误会）
#   - exited（进程不在，默认）时再推断退出方式：收到过停止信号的是被外部 SIGTERM/SIGINT 结束；
#     既没有崩溃输出也不是启动失败的，多半是被 SIGKILL 直接结束（OOM、kill -9）
wd_log_tail() {
    [[ -f "$LOG_FILE" ]] || return 0
    local kind="${1:-exited}" from=1 sep lines last="" msg="" err="" text etime="" lastrec ltime stopped=0 startfail=0 quit=0
    sep="$(LC_ALL=C grep -n -E '^===== [0-9]{4}-[0-9]{2}-[0-9]{2} .* start\.sh ' "$LOG_FILE" 2>/dev/null | tail -n 1 | cut -d: -f1)"
    [[ "$sep" =~ ^[0-9]+$ ]] && from=$((sep + 1))
    lines="$(tail -n "+$from" "$LOG_FILE" 2>/dev/null | LC_ALL=C awk '
        function blank(s) { return s ~ /^[ \t\r]*$/ }
        function isjson(s) { return s ~ /^[ \t]*\{/ }
        {
            ring[NR % 20] = $0
            if (!cs && $0 ~ /^(panic: |fatal error: |runtime: |SIG[A-Z]+: |unexpected fault)/) cs = NR
            if (cs && NR - cs < 8) head[NR - cs] = $0
            if (isjson($0)) lj = NR
            else if (NR - lj <= 8) after[NR - lj] = $0
        }
        END {
            n = NR
            ts = n - 19
            if (ts < 1) ts = 1
            # 开头一段：崩溃起点优先，否则最后一条 JSON 之后；与最后 20 行重叠的部分只在后面输出一次
            if (cs) { bs = cs; for (i = 0; i < 8 && bs + i <= n; i++) blk[bs + i] = head[i] }
            else { bs = lj + 1; for (i = 1; i <= 8 && lj + i <= n; i++) blk[lj + i] = after[i] }
            if (bs < ts) {
                for (i = bs; i < bs + 8 && i < ts; i++) if (!blank(blk[i]) && !isjson(blk[i])) print blk[i]
                if (ts > bs + 8) print "…（省略 " (ts - bs - 8) " 行）"
            }
            for (i = ts; i <= n; i++) { l = ring[i % 20]; if (!isjson(l) && !blank(l)) print l }
        }' | redact | utf8_cut 300)"
    # 卡死重启时程序收到过 SIGQUIT：栈的开头是信号处理线程、末尾是寄存器，都看不出卡在哪里，改为按 goroutine 汇总
    if [[ "$kind" == hung ]] && tail -n "+$from" "$LOG_FILE" 2>/dev/null | LC_ALL=C grep -E '^SIGQUIT: quit' >/dev/null; then
        lines="$(tail -n "+$from" "$LOG_FILE" 2>/dev/null | goroutine_summary | redact | utf8_cut 300)"
        quit=1
    fi
    last="$(tail -n "+$from" "$LOG_FILE" 2>/dev/null | LC_ALL=C grep -E '^\{("[a-z_]+":"([^"\\]|\\.)*",)*"level":"ERROR"' | tail -n 1)"
    if [[ -n "$last" ]]; then
        msg="$(printf '%s\n' "$last" | LC_ALL=C sed -nE 's/^\{("[a-z_]+":"([^"\\]|\\.)*",)*"msg":"(([^"\\]|\\.)*)".*/\3/p' |
            json_unescape | redact | utf8_cut 200)"
        err="$(printf '%s\n' "$last" | LC_ALL=C sed -nE 's/.*[{,]"error":"(([^"\\]|\\.)*)".*/\1/p' |
            json_unescape | redact | utf8_cut 200)"
    fi
    if [[ -n "$last" ]]; then
        etime="$(json_time "$last")"
    fi
    lastrec="$(tail -n "+$from" "$LOG_FILE" 2>/dev/null | LC_ALL=C grep -E '^\{' | tail -n 1)"
    ltime="$(json_time "$lastrec")"
    if tail -n "+$from" "$LOG_FILE" 2>/dev/null | LC_ALL=C grep -E '^\{.*"msg":"收到停止信号' >/dev/null; then
        stopped=1
    fi
    [[ "$lastrec" == *'"msg":"AIStudio2API 启动失败"'* ]] && startfail=1
    if ((quit)); then
        printf '卡住时的 goroutine（SIGQUIT 栈汇总，已打码）：\n%s\n' "$lines"
    elif [[ -n "$lines" ]]; then
        printf '主日志末尾的非 JSON 行（已打码）：\n%s\n' "$lines"
    fi
    text="$msg"
    [[ -n "$err" ]] && text="${text:+$text：}$err"
    [[ -n "$text" ]] && printf '最后一条 ERROR（%s已打码，msg 与 error 各取前 200 字）：%s\n' "${etime:+时间 $etime，}" "$text"
    [[ -n "$ltime" ]] && printf '这个进程在主日志里最后一条记录的时间：%s\n' "$ltime"
    if [[ "$kind" == hung ]]; then
        if ((quit)); then
            printf '完整的 goroutine 栈在主日志 %s（这个进程最后一段里 SIGQUIT: quit 之后）\n' "$LOG_FILE"
        elif ((STOP_SENT_QUIT)); then
            printf '%s\n' '发过 SIGQUIT，但主日志里没有 goroutine 栈（进程可能被暂停，或卡在内核调用里收不到信号）'
        fi
        return 0
    fi
    if ((stopped)); then
        printf '%s\n' '进程收到停止信号（SIGTERM 或 Ctrl+C）后退出：不是 ./start.sh stop（它会留下人为停止标记），可能是被打断的 ./start.sh restart、手动 kill 或其他程序'
    elif [[ -z "$lines" ]] && ((startfail == 0)); then
        printf '%s\n' '主日志里没有崩溃输出：多半被 SIGKILL 直接结束（系统 OOM、kill -9 或其他程序）'
    fi
    return 0
}

# oom_killed PID：内核日志（dmesg，读不到时 journalctl -k）里有这个 PID 的 aistudio2api 被 OOM 结束的记录时返回 0，
# 没有时返回 1，两处都读不到（非 root 且 dmesg_restrict=1 等）时返回 2
oom_killed() {
    local pattern="Killed process $1 \\($APP_NAME\\)|task=$APP_NAME,pid=$1([^0-9]|\$)" out=""
    have dmesg && out="$(dmesg 2>/dev/null)"
    if [[ -z "$out" ]] && have journalctl; then
        out="$(journalctl -k -q --no-pager -n 20000 2>/dev/null)"
    fi
    [[ -n "$out" ]] || return 2
    printf '%s\n' "$out" | grep -E "$pattern" >/dev/null
}

# file_mtime FILE：文件修改时间（epoch 秒）；GNU stat 不可用时用 BSD/macOS 的 stat -f，都不行时为空
file_mtime() {
    stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null
}

# fmt_time EPOCH：格式化为 HH:MM:SS（GNU date -d，BSD/macOS date -r），都不行时为空
fmt_time() {
    date -d "@$1" '+%T' 2>/dev/null || date -r "$1" '+%T' 2>/dev/null
}

# wd_hourly FLAG：同一类记录每小时最多写一次；FLAG 不存在或已超过一小时时更新它并返回 0
wd_hourly() {
    local m
    if [[ -f "$1" ]]; then
        m="$(file_mtime "$1")"
        [[ "$m" =~ ^[0-9]+$ ]] && (($(date +%s) - m < 3600)) && return 1
    fi
    { : >"$1"; } 2>/dev/null
    return 0
}

# older_than_boot FILE：文件早于本次开机（系统重启过）时返回 0
older_than_boot() {
    local btime mtime
    btime="$(awk '/^btime / {print $2}' /proc/stat 2>/dev/null)"
    mtime="$(file_mtime "$1")"
    [[ "$btime" =~ ^[0-9]+$ && "$mtime" =~ ^[0-9]+$ ]] && ((mtime < btime))
}

# proc_age PID：进程已运行的秒数（解析 ps 的 etime：[[dd-]hh:]mm:ss）
proc_age() {
    local et
    et="$(trim "$(ps -p "$1" -o etime= 2>/dev/null)")"
    [[ "$et" =~ ^(([0-9]+)-)?(([0-9]+):)?([0-9]+):([0-9]+)$ ]] || return 1
    printf '%d' $((10#${BASH_REMATCH[2]:-0} * 86400 + 10#${BASH_REMATCH[4]:-0} * 3600 + 10#${BASH_REMATCH[5]} * 60 + 10#${BASH_REMATCH[6]}))
}

# proc_env PID KEY：进程启动时环境里 KEY 的值（读不到时为空）
proc_env() {
    [[ -r "/proc/$1/environ" ]] || return 0
    tr '\0' '\n' <"/proc/$1/environ" 2>/dev/null | sed -n "s/^$2=//p" | tail -n 1
}

# env_only_keys PID：进程环境里有（非空）、但 .env 里没有或取值不同的配置键；只看 .env.example 里出现的键，只输出键名
env_only_keys() {
    local pid="$1" key value keys=""
    [[ -r "/proc/$pid/environ" && -f .env.example ]] || return 0
    for key in $(tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | sed -n 's/^\([A-Z][A-Z0-9_]*\)=.*/\1/p' | sort -u |
        grep -Fx -f <(grep -oE '^#?[[:space:]]*(export[[:space:]]+)?[A-Z][A-Z0-9_]*=' .env.example |
            sed -E 's/^#?[[:space:]]*(export[[:space:]]+)?//; s/=$//' | sort -u)); do
        value="$(trim "$(proc_env "$pid" "$key")")"
        [[ -n "$value" ]] || continue
        case "$key" in
            WATCHDOG_*) [[ "$(wd_setting "$key")" == "$value" ]] && continue ;;
            *) [[ "$(dotenv_value "$key")" == "$value" ]] && continue ;;
        esac
        keys+="$key "
    done
    trim "$keys"
}

# system_lang：系统默认语言（/etc/default/locale 或 /etc/locale.conf 里的 LANG；登录 shell 从这里取，cron 里没有）
system_lang() {
    local f v
    for f in /etc/default/locale /etc/locale.conf; do
        v="$(dotenv_value LANG "$f")"
        if [[ -n "$v" ]]; then
            printf '%s' "$v"
            return 0
        fi
    done
    return 0
}

# system_zone：系统时区（/etc/localtime 指向的 zoneinfo 名，或 /etc/timezone）；TZ 未设置时程序按它取
system_zone() {
    local t
    t="$(readlink /etc/localtime 2>/dev/null)"
    if [[ "$t" == *zoneinfo/* ]]; then
        printf '%s' "${t##*zoneinfo/}"
    elif [[ -f /etc/timezone ]]; then
        trim "$(head -n 1 /etc/timezone 2>/dev/null)"
    fi
}

# locale_of LC_ALL LC_MESSAGES LANG：与程序相同，取第一个有效值（去掉 .编码 与 @修饰，C/POSIX 视为未设置）
locale_of() {
    local v
    for v in "$@"; do
        v="$(trim "$v")"
        v="${v%%.*}"
        v="${v%%@*}"
        case "$v" in
            "" | C | POSIX | c | posix) continue ;;
        esac
        printf '%s' "$v"
        return 0
    done
    return 0
}

# launch_env_plan：拉起程序时，LAUNCH_ENV_KEYS 里进程环境中没有（或为空）的从 run/watchdog.conf 补上；
# 语言变量（LC_ALL、LC_MESSAGES、LANG）进程环境与 run/watchdog.conf 里都没有时按系统默认语言补 LANG。
# 结果放在 LAUNCH_ENV（KEY=VALUE，交给 env 命令）、LAUNCH_ENV_SRC（从 run/watchdog.conf 补上的键名）与 LAUNCH_ENV_LANG
launch_env_plan() {
    local key v lang=0
    LAUNCH_ENV=()
    LAUNCH_ENV_SRC=""
    LAUNCH_ENV_LANG=""
    for key in "${LAUNCH_ENV_KEYS[@]}"; do
        if [[ -n "${!key-}" ]]; then
            [[ "$key" == LC_ALL || "$key" == LC_MESSAGES || "$key" == LANG ]] && lang=1
            continue
        fi
        v="$(dotenv_value "$key" "$WD_CONF")"
        [[ -n "$v" ]] || continue
        [[ "$key" == LC_ALL || "$key" == LC_MESSAGES || "$key" == LANG ]] && lang=1
        LAUNCH_ENV+=("$key=$v")
        LAUNCH_ENV_SRC+="${LAUNCH_ENV_SRC:+ }$key"
    done
    if ((lang == 0)); then
        v="$(system_lang)"
        if [[ -n "$v" ]]; then
            LAUNCH_ENV+=("LANG=$v")
            LAUNCH_ENV_LANG="$v"
        fi
    fi
}

# env_drift PID：运行中进程启动时的环境与 cron 拉起时会得到的环境比较，只输出键名（不输出取值）：
#   DRIFT_DOTENV：.env.example 里的配置项，进程环境里有、.env 里没有或取值不同（应写进 .env）
#   DRIFT_LAUNCH：LAUNCH_ENV_KEYS，进程里的值与 cron 拉起时（run/watchdog.conf，语言与时区还有系统默认）不同；
#                 程序只从进程环境读取它们，写进 .env 无效（应写进 run/watchdog.conf）。
#                 语言按程序的取法比较（LC_ALL、LC_MESSAGES、LANG 的第一个有效值，决定新加账户的默认语言），时区按 TZ 或系统时区比较
env_drift() {
    local pid="$1" key value conf lang_keys="" cron_lang pz cz
    DRIFT_DOTENV="$(env_only_keys "$pid")"
    DRIFT_LAUNCH=""
    [[ -r "/proc/$pid/environ" ]] || return 0
    for key in "${LAUNCH_ENV_KEYS[@]}"; do
        case "$key" in
            LANG | LC_ALL | LC_MESSAGES)
                [[ -n "$(trim "$(proc_env "$pid" "$key")")" ]] && lang_keys+="${lang_keys:+ }$key"
                continue
                ;;
            TZ) continue ;;
        esac
        value="$(trim "$(proc_env "$pid" "$key")")"
        conf="$(trim "$(dotenv_value "$key" "$WD_CONF")")"
        [[ "$value" == "$conf" ]] || DRIFT_LAUNCH+="${DRIFT_LAUNCH:+ }$key"
    done
    cron_lang="$(dotenv_value LANG "$WD_CONF")"
    if [[ -z "$(dotenv_value LC_ALL "$WD_CONF")$(dotenv_value LC_MESSAGES "$WD_CONF")$cron_lang" ]]; then
        cron_lang="$(system_lang)"
    fi
    if [[ "$(locale_of "$(proc_env "$pid" LC_ALL)" "$(proc_env "$pid" LC_MESSAGES)" "$(proc_env "$pid" LANG)")" != \
        "$(locale_of "$(dotenv_value LC_ALL "$WD_CONF")" "$(dotenv_value LC_MESSAGES "$WD_CONF")" "$cron_lang")" ]]; then
        DRIFT_LAUNCH+="${DRIFT_LAUNCH:+ }${lang_keys:-LANG}"
    fi
    pz="$(trim "$(proc_env "$pid" TZ)")"
    pz="${pz#:}"
    [[ -n "$pz" ]] || pz="$(system_zone)"
    cz="$(trim "$(dotenv_value TZ "$WD_CONF")")"
    cz="${cz#:}"
    [[ -n "$cz" ]] || cz="$(system_zone)"
    [[ "$pz" == "$cz" ]] || DRIFT_LAUNCH+="${DRIFT_LAUNCH:+ }TZ"
    return 0
}

# env_drift_warn PID：enable 与开了守护时的 start 当场提示 env_drift 的结果（只列键名）
env_drift_warn() {
    env_drift "$1"
    if [[ -n "$DRIFT_DOTENV" ]]; then
        warn "运行中的程序（PID $1）环境里有这些配置不在 .env 里或与 .env 不同：$DRIFT_DOTENV"
        warn "守护与开机自启由 cron 拉起，看不到这些变量，请写进 .env"
    fi
    if [[ -n "$DRIFT_LAUNCH" ]]; then
        warn "运行中的程序（PID $1）环境里这些变量与 cron 拉起时会得到的不同：$DRIFT_LAUNCH"
        warn "程序只从进程环境读取它们，写进 .env 无效；守护与开机自启拉起的进程会不一样，请写进 $WD_CONF（格式同 .env，拉起时进程环境里没有才补上）"
    fi
    return 0
}

# health_probe ADDR [超时秒数]：GET /health（不需要认证，不走代理），返回 curl 的退出码
health_probe() {
    curl -fs -o /dev/null -m "${2:-10}" --noproxy '*' "http://$1/health" 2>/dev/null
}

# 健康检查状态文件一行：PID 地址 应答过(0 否 / 1 是 / 2 否且已提示) 连续失败次数 这次检查的时间（epoch 秒）
wd_health_save() {
    { printf '%s %s %s %s %s\n' "$1" "$2" "$3" "$4" "$(date +%s)" >"$WD_HEALTH_STATE"; } 2>/dev/null
}

# wd_fails_fresh FAILS TIME：失败计数还算“连续”（上次检查在 WD_HEALTH_WINDOW 秒内）时输出 FAILS，否则输出 0。
# 守护停用、cron 中断或连续几轮拿不到锁之后留下的旧计数不能和新的失败连起来
wd_fails_fresh() {
    local now
    now="$(date +%s)"
    if [[ "$1" =~ ^[0-9]+$ && "$2" =~ ^[0-9]+$ ]] && ((now - $2 <= WD_HEALTH_WINDOW)); then
        printf '%s' "$1"
    else
        printf '0'
    fi
}

# health_mark_ok PID：start 就绪后确认 /health 能应答，记为这个进程在这个地址上应答过（守护据此才对失败计数）
health_mark_ok() {
    have curl || return 0
    local addr
    addr="$(probe_host):$(listen_port)"
    if health_probe "$addr" 3; then
        wd_health_save "$1" "$addr" 1 0
    fi
    return 0
}

# wd_launch_stats：输出“30 分钟内自动拉起次数 最近一次自动拉起的时间（epoch 秒）”
wd_launch_stats() {
    local now t recent=0 last=0
    now="$(date +%s)"
    if [[ -f "$WD_LAUNCHES" ]]; then
        while read -r t _; do
            [[ "$t" =~ ^[0-9]+$ ]] || continue
            ((t > now - WD_LOOP_WINDOW)) && recent=$((recent + 1))
            ((t > last)) && last="$t"
        done <"$WD_LAUNCHES"
    fi
    printf '%s %s' "$recent" "$last"
}

wd_record_launch() {
    local now
    now="$(date +%s)"
    {
        [[ -f "$WD_LAUNCHES" ]] && awk -v c="$((now - WD_LOOP_WINDOW))" '$1 ~ /^[0-9]+$/ && $1 + 0 > c + 0' "$WD_LAUNCHES"
        printf '%s\n' "$now"
    } >"$WD_LAUNCHES.tmp" 2>/dev/null && mv -f "$WD_LAUNCHES.tmp" "$WD_LAUNCHES"
}

# wd_allow_launch REASON：崩溃循环保护。30 分钟内已自动拉起 5 次时改为每 10 分钟最多拉起一次（进入时写一条 WARN），
# 30 分钟内没有再自动拉起时恢复正常；返回 1 表示本轮不拉起
wd_allow_launch() {
    local reason="$1" now recent last next
    now="$(date +%s)"
    read -r recent last <<<"$(wd_launch_stats)"
    if [[ -f "$WD_THROTTLE" ]] && ((recent == 0)); then
        rm -f "$WD_THROTTLE"
    fi
    if [[ ! -f "$WD_THROTTLE" ]] && ((recent >= WD_LOOP_MAX)); then
        date '+%F %T' >"$WD_THROTTLE"
        next="$(fmt_time $((last + WD_LOOP_INTERVAL)))"
        wd_log WARN "连续崩溃，降低拉起频率：30 分钟内已自动拉起 $recent 次，改为每 10 分钟最多拉起一次${next:+（下次最早 $next）}；本次原因：$reason"
    fi
    if [[ -f "$WD_THROTTLE" ]] && ((now - last < WD_LOOP_INTERVAL)); then
        return 1
    fi
    return 0
}

# wd_launch REASON：以守护身份拉起程序（不构建、不写人为停止标记），主日志写分隔行，结果写进守护日志
wd_launch() {
    local reason="$1" out="$RUN_DIR/watchdog.out" count note=""
    wd_record_launch
    read -r count _ <<<"$(wd_launch_stats)"
    [[ -f "$WD_THROTTLE" ]] && note="；降频中：每 10 分钟最多拉起一次"
    if launch_app "watchdog 自动拉起：$reason" >"$out" 2>&1; then
        redact <"$out" | utf8_cut 300 | wd_detail
        wd_log INFO "已自动拉起（PID $LAUNCH_PID，打开文件数上限 $(ulimit -n)，30 分钟内第 $count 次$note）"
    else
        redact <"$out" | utf8_cut 300 | wd_detail
        wd_log WARN "自动拉起失败：进程启动后立即退出（PID $LAUNCH_PID，打开文件数上限 $(ulimit -n)，30 分钟内第 $count 次$note），主日志：$LOG_FILE"
    fi
    rm -f "$out" "$WD_HEALTH_STATE" "$WD_DEFER"
}

# wd_revive：进程不在时记录原因与主日志末尾，再以守护身份拉起
wd_revive() {
    local oldpid="" reason flag="$RUN_DIR/watchdog.nobin" orphan="$RUN_DIR/watchdog.orphan" up addr oom
    # 刚开机时交给 @reboot 那一行（它先等 20 秒，网络与磁盘就绪后再启动）
    up="$(awk '{print int($1)}' /proc/uptime 2>/dev/null)"
    [[ "$up" =~ ^[0-9]+$ ]] && ((up < WD_BOOT_SETTLE)) && return 0
    [[ -f "$PID_FILE" ]] && oldpid="$(tr -cd '0-9' <"$PID_FILE")"
    if [[ ! -f "$PID_FILE" ]]; then
        reason="没有 PID 文件"
    elif [[ -z "$oldpid" ]]; then
        reason="PID 文件为空"
    elif alive "$oldpid"; then
        if [[ "$(ps -p "$oldpid" -o comm= 2>/dev/null)" == "$APP_NAME" ]]; then
            reason="PID 文件里的进程 $oldpid 是其他目录的 $APP_NAME（PID 已被复用）"
        else
            reason="PID 文件里的进程 $oldpid 已不是 $APP_NAME（PID 已被其他程序复用）"
        fi
    else
        reason="PID 文件里的进程 $oldpid 已不存在"
        oom_killed "$oldpid"
        oom=$?
        if ((oom == 0)); then
            reason+="，被系统 OOM 结束"
        elif older_than_boot "$PID_FILE"; then
            reason+="，系统重启过（PID 文件早于本次开机）"
        elif ((oom == 2)); then
            reason+="（读不到内核日志，无法判断是否被 OOM 结束）"
        fi
    fi
    # 找不到本目录的进程，但监听地址上已有程序应答 /health：多半是同一个程序（PID 文件丢失、经另一种路径启动），
    # 再拉起一个只会因端口被占而退出，退出前还可能改写运行中实例在用的文件；不拉起，同样的情况每小时最多记一次
    addr="$(probe_host):$(listen_port)"
    if have curl && health_probe "$addr" 3; then
        if wd_hourly "$orphan"; then
            wd_log WARN "找不到本目录的进程（$reason），但监听地址 $addr 上已有程序应答 /health，找不到它的 PID，不拉起（请检查 PID 文件 $PID_FILE 与启动路径；每小时最多记一次）"
        fi
        return 0
    fi
    rm -f "$orphan"
    if [[ ! -x "$BIN" ]]; then
        # 守护不构建（cron 里通常没有 Go 与 node，构建也太久）；同样的情况每小时最多记一次
        if wd_hourly "$flag"; then
            wd_log WARN "进程不在运行：$reason；没有可执行文件 $BIN，守护不构建也不拉起（请执行 ./start.sh start，或先 ./start.sh build）"
        fi
        return 0
    fi
    rm -f "$flag"
    wd_allow_launch "$reason" || return 0
    wd_log WARN "进程不在运行：$reason"
    wd_log_tail | wd_detail
    wd_launch "$reason"
}

# wd_restart_hung PID REASON：判定卡死后以守护身份 stop + 拉起（不写人为停止标记）
wd_restart_hung() {
    local pid="$1" reason="$2" out="$RUN_DIR/watchdog.out" last next i
    if ! wd_allow_launch "$reason"; then
        # 降频中推迟重启：每次推迟只记一条，说明最早什么时候重启
        read -r _ last <<<"$(wd_launch_stats)"
        if [[ "$(head -n 1 "$WD_DEFER" 2>/dev/null)" != "$last" ]]; then
            { printf '%s\n' "$last" >"$WD_DEFER"; } 2>/dev/null
            next="$(fmt_time $((last + WD_LOOP_INTERVAL)))"
            wd_log INFO "判定卡死（$reason），但守护处于降频中，暂不重启${next:+，最早 $next 重启}（PID $pid）"
        fi
        return 0
    fi
    env_drift "$pid"
    if [[ -n "$DRIFT_DOTENV" ]]; then
        wd_log WARN "运行中进程的环境里有这些配置不在 .env 里或与 .env 不同：$DRIFT_DOTENV；守护拉起的新进程看不到，请写进 .env"
    fi
    if [[ -n "$DRIFT_LAUNCH" ]]; then
        wd_log WARN "运行中进程的环境里这些变量与守护拉起时不同：$DRIFT_LAUNCH；程序只从进程环境读取它们（写进 .env 无效），请写进 $WD_CONF"
    fi
    wd_log WARN "判定卡死：$reason，重启（PID $pid）"
    STOP_SENT_QUIT=0
    stop >"$out" 2>&1
    redact <"$out" | utf8_cut 300 | wd_detail
    rm -f "$out"
    # 卡在哪里的线索：此时最后一条分隔行仍属于被重启的这个进程；stop 超时后发过 SIGQUIT 时附上 goroutine 汇总
    wd_log_tail hung | wd_detail
    # 强制结束后旧进程可能还没退干净（大内存进程回收慢）：再等一会儿，仍在就不拉起，免得两个实例同时运行
    for ((i = 0; i < 10; i++)); do
        alive "$pid" || break
        sleep 1
    done
    if alive "$pid"; then
        # stop 删了 PID 文件：写回，下一轮仍按这个进程判断
        printf '%s\n' "$pid" >"$PID_FILE"
        wd_log WARN "旧进程 $pid 强制结束后仍未退出，本轮不拉起，下一轮再检查"
        return 0
    fi
    wd_launch "$reason"
}

# wd_check_health PID：进程在时用 /health 判断是否卡死
wd_check_health() {
    local pid="$1" limit grace age addr rc why fails penv st_pid="" st_addr="" st_ok=0 st_fails=0 st_time=""
    limit="$(wd_value WATCHDOG_HEALTH_FAILS)"
    [[ "$limit" =~ ^[0-9]+$ ]] || limit=5
    if ((limit == 0)); then
        rm -f "$WD_HEALTH_STATE"
        return 0
    fi
    have curl || return 0
    grace="$(wd_value WATCHDOG_GRACE)"
    [[ "$grace" =~ ^[0-9]+$ ]] || grace=300
    # 监听地址以运行中进程启动时的环境为准：在终端 export 的 LISTEN_ADDR，cron 里看不到
    penv="$(trim "$(proc_env "$pid" LISTEN_ADDR)")"
    # shellcheck disable=SC2034 # env_value 通过 ${!key} 读取同名变量
    [[ -n "$penv" ]] && LISTEN_ADDR="$penv"
    addr="$(probe_host):$(listen_port)"
    if [[ -f "$WD_HEALTH_STATE" ]]; then
        read -r st_pid st_addr st_ok st_fails st_time _ <"$WD_HEALTH_STATE"
    fi
    [[ "$st_ok" =~ ^[0-2]$ ]] || st_ok=0
    # 上次检查已超过 WD_HEALTH_WINDOW 秒（守护停用过、cron 中断）：旧的失败计数不算连续，从头计
    st_fails="$(wd_fails_fresh "$st_fails" "$st_time")"
    if [[ "$st_pid" != "$pid" || "$st_addr" != "$addr" ]]; then
        st_ok=0
        st_fails=0
    fi
    age="$(proc_age "$pid")" || age=0

    health_probe "$addr" 10
    rc=$?
    if ((rc == 0)); then
        ((st_fails > 0)) && wd_log INFO "健康检查恢复（PID $pid，此前连续失败 $st_fails 次），计数清零"
        wd_health_save "$pid" "$addr" 1 0
        return 0
    fi
    # 启动宽限期内（预热 Worker）不判定
    if ((age < grace)); then
        wd_health_save "$pid" "$addr" "$st_ok" 0
        return 0
    fi
    # 这个进程还没在这个地址上应答过 /health：多半是地址与运行中的程序不一致（或绑在本机连不到的地址），不当成卡死
    if ((st_ok != 1)); then
        if ((st_ok == 0)); then
            wd_log INFO "健康检查：PID $pid 还没有在 $addr 应答过 /health，不做卡死判定（可能是启动卡在开始监听之前，例如首次下载 Camoufox；或 .env 的 LISTEN_ADDR 与运行中的程序不一致）"
        fi
        wd_health_save "$pid" "$addr" 2 0
        return 0
    fi
    case "$rc" in
        28) why="10 秒内无响应" ;;
        7) why="连接被拒绝" ;;
        22) why="返回错误状态码" ;;
        52) why="空响应" ;;
        56) why="连接中断" ;;
        *) why="curl 退出码 $rc" ;;
    esac
    if ((st_fails < limit)); then
        fails=$((st_fails + 1))
        wd_log INFO "健康检查失败 第 $fails/$limit 次（PID $pid，http://$addr/health $why）"
    else
        # 已到阈值仍在：降频中推迟了重启（或刚调低阈值），计数封顶，不再重复记录
        fails="$limit"
    fi
    wd_health_save "$pid" "$addr" 1 "$fails"
    ((fails >= limit)) || return 0
    wd_restart_hung "$pid" "健康检查连续 $fails 次失败（$why）"
}

watchdog() {
    local pid rc recent
    WATCHDOG_MODE=1
    [[ -f "$STOP_MARK" ]] && return 0
    mkdir -p "$RUN_DIR" "$LOG_DIR" 2>/dev/null || return 1
    # 没有 ps/pgrep 时 alive 总是为假，会把运行中的程序当成已退出、再拉起一个：什么都不做
    if ! have ps || ! have pgrep; then
        if wd_hourly "$RUN_DIR/watchdog.noprocps"; then
            wd_log WARN "缺少 ps/pgrep（procps），无法判断进程是否在运行，守护不做任何动作（每小时最多记一次）；请安装 procps"
        fi
        return 0
    fi
    rm -f "$RUN_DIR/watchdog.noprocps"
    lock_acquire try
    rc=$?
    if ((rc == 1)); then
        # 用户的 start/stop/restart（或上一轮守护）正在进行：本轮跳过
        return 0
    fi
    if ! have flock && [[ ! -f "$RUN_DIR/watchdog.noflock" ]]; then
        : >"$RUN_DIR/watchdog.noflock"
        if ((rc == 2)); then
            wd_log WARN "未找到 flock 命令（util-linux），也建不了目录锁 $LOCK_DIR，守护与 start/stop/restart 之间不加锁；建议安装 util-linux"
        else
            wd_log WARN "未找到 flock 命令（util-linux），改用目录锁 $LOCK_DIR；建议安装 util-linux"
        fi
    fi
    # 拿到锁后再看一次：第一次检查之后、拿到锁之前，用户可能刚执行完 stop
    [[ -f "$STOP_MARK" ]] && return 0
    wd_rotate_log
    # 30 分钟内没有再自动拉起：结束降频（程序恢复稳定后 status 不再显示降频中）
    if [[ -f "$WD_THROTTLE" ]]; then
        read -r recent _ <<<"$(wd_launch_stats)"
        ((recent == 0)) && rm -f "$WD_THROTTLE" "$WD_DEFER"
    fi
    if pid="$(find_pid)"; then
        wd_check_health "$pid"
    else
        wd_revive
    fi
    return 0
}

# cron_line_is_ours LINE：带标记、且执行的是本目录的 start.sh（按目录的物理路径比较，经符号链接写入的行也认得）。
# 同一台机器上每个部署目录用的是同一个标记，不能只按标记判断
cron_line_is_ours() {
    local re='"([^"]+)/start\.sh"' p d
    [[ "$1" == *"$CRON_MARKER"* && "$1" =~ $re ]] || return 1
    p="${BASH_REMATCH[1]}"
    [[ "$p" == "$APP_DIR" || "$p" == "$APP_DIR_P" ]] && return 0
    d="$(cd "$p" 2>/dev/null && pwd -P)"
    [[ -n "$d" && "$d" == "$APP_DIR_P" ]]
}

# cron_lines mine|others：crontab 里本目录的开机自启、轮转与守护行，或除此以外的所有行（用户自己的任务、其他目录的带标记行）
cron_lines() {
    local l
    crontab -l 2>/dev/null | while IFS= read -r l || [[ -n "$l" ]]; do
        if cron_line_is_ours "$l"; then
            [[ "$1" == mine ]] && printf '%s\n' "$l"
        else
            [[ "$1" == others ]] && printf '%s\n' "$l"
        fi
    done
    return 0
}

watchdog_in_cron() {
    have crontab || return 1
    cron_lines mine | grep -E '"[[:space:]]+watchdog([[:space:]]|$)' >/dev/null
}

# watchdog_status [PID]：status 里显示守护是否开启、人为停止、降频、健康检查失败计数与最近 3 条守护记录
watchdog_status() {
    local pid="${1-}" st_pid="" st_addr="" st_ok="" st_fails="" st_time="" recent launches
    if watchdog_in_cron; then
        info "守护：已开启（cron 每分钟检查），记录：$WD_LOG"
        if [[ ! -x "$APP_DIR/start.sh" ]]; then
            warn "start.sh 没有执行权限，cron 执行它会失败，守护、开机自启与日志轮转实际都没有运行；请执行 chmod +x start.sh"
        fi
    else
        info "守护：未开启（./start.sh enable 开启）"
    fi
    if [[ -f "$STOP_MARK" ]]; then
        warn "人为停止中（$(head -n 1 "$STOP_MARK" 2>/dev/null)）：守护不会自动拉起，执行 ./start.sh start 后恢复"
    fi
    read -r launches _ <<<"$(wd_launch_stats)"
    if [[ -f "$WD_THROTTLE" ]] && ((launches > 0)); then
        warn "守护降频中（$(head -n 1 "$WD_THROTTLE" 2>/dev/null) 起连续崩溃）：每 10 分钟最多自动拉起一次；30 分钟内没有再自动拉起时恢复，./start.sh start 或 restart 后清除"
    fi
    if [[ -n "$pid" && -f "$WD_HEALTH_STATE" ]]; then
        read -r st_pid st_addr st_ok st_fails st_time _ <"$WD_HEALTH_STATE"
        st_fails="$(wd_fails_fresh "$st_fails" "$st_time")"
        if [[ "$st_pid" == "$pid" ]] && ((st_fails > 0)); then
            warn "健康检查已连续失败 $st_fails 次（http://$st_addr/health）"
        fi
    fi
    [[ -f "$WD_LOG" ]] || return 0
    recent="$(grep -E '^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9:]{8} \[' "$WD_LOG" 2>/dev/null | tail -n 3)"
    if [[ -n "$recent" ]]; then
        info "守护最近记录："
        printf '%s\n' "$recent" | sed 's/^/  /'
    fi
}

enable_boot() {
    if ! have crontab; then
        fail "未找到 crontab；请安装 cron，或改用 systemd 管理"
        return 1
    fi
    local line="@reboot sleep 20 && \"$APP_DIR/start.sh\" start >/dev/null 2>&1 $CRON_MARKER"
    local rotate="17 * * * * \"$APP_DIR/start.sh\" rotate >/dev/null 2>&1 $CRON_MARKER"
    local guard="* * * * * \"$APP_DIR/start.sh\" watchdog >/dev/null 2>&1 $CRON_MARKER"
    local limit grace pid others
    # cron 直接执行 start.sh：面板上传或解压的文件常常没有执行权限，cron 执行失败时输出被丢弃，什么都看不到
    if [[ ! -x "$APP_DIR/start.sh" ]]; then
        if chmod +x "$APP_DIR/start.sh" 2>/dev/null; then
            info "已为 start.sh 加上执行权限（cron 直接执行它）"
        else
            warn "start.sh 没有执行权限，cron 无法运行开机自启、日志轮转与守护；请执行 chmod +x start.sh"
        fi
    fi
    # 先去掉本目录的旧行再写入，重复执行不会重复添加；其他目录的带标记行与用户自己的任务原样保留
    if ! { cron_lines others; printf '%s\n%s\n%s\n' "$line" "$rotate" "$guard"; } | crontab -; then
        fail "写入 crontab 失败"
        return 1
    fi
    info "已设置开机自启、每小时日志轮转与守护：
  $line
  $rotate
  $guard"
    others="$(cron_lines others | grep -cF "$CRON_MARKER")"
    if [[ "$others" =~ ^[0-9]+$ ]] && ((others > 0)); then
        info "crontab 里另有 $others 行其他目录的 aistudio2api 任务（同一标记），未改动"
    fi
    # cron 只读文件：这里也只看 run/watchdog.conf 与 .env，显示守护实际使用的值
    limit="$(wd_setting WATCHDOG_HEALTH_FAILS)"
    [[ "$limit" =~ ^[0-9]+$ ]] || limit=5
    grace="$(wd_setting WATCHDOG_GRACE)"
    [[ "$grace" =~ ^[0-9]+$ ]] || grace=300
    if ((limit == 0)); then
        info "守护已开启：每分钟检查一次，进程不在时自动拉起；WATCHDOG_HEALTH_FAILS=0，不做卡死检查"
    else
        info "守护已开启：每分钟检查一次，进程不在时自动拉起；启动 ${grace}s 后 /health 连续 $limit 次无响应判定卡死并重启"
    fi
    info "守护记录：$WD_LOG（./start.sh status 显示最近 3 条）"
    info "执行 ./start.sh stop 后守护不会自动拉起，执行 ./start.sh start 后恢复"
    info "守护与开机自启由 cron 执行，只读 .env 中的配置，看不到在终端 export 的变量"
    info "WATCHDOG_HEALTH_FAILS、WATCHDOG_GRACE 建议写在 $WD_CONF（格式同 .env，优先于 .env）：管理页保存服务配置时会重写 .env 并去掉这两项，这个文件不受影响"
    info "程序只从进程环境读取的 CAMOUFOX_PATH、SHUTDOWN_GRACE、LANG、TZ、http_proxy 等写进 .env 无效，需要时写在 $WD_CONF：拉起时进程环境里没有才补上"
    if have pgrep && ! pgrep -x cron >/dev/null 2>&1 && ! pgrep -x crond >/dev/null 2>&1; then
        warn "没有检测到 cron 服务（cron/crond）在运行，crontab 里的任务不会执行；请安装并启动 cron"
    fi
    if pid="$(find_pid)"; then
        env_drift_warn "$pid"
    elif [[ -f "$STOP_MARK" ]]; then
        info "当前处于人为停止状态，守护不会拉起；执行 ./start.sh start 启动"
    elif [[ ! -x "$BIN" ]]; then
        warn "没有可执行文件 $BIN，守护不构建也不拉起；请先执行 ./start.sh start（会自动构建）或 ./start.sh build"
    else
        info "程序当前未运行，守护会在 1 分钟内拉起；不需要的话先执行 ./start.sh stop"
    fi
}

disable_boot() {
    have crontab || return 0
    local others
    # 只去掉本目录的行：同一台机器上其他目录的守护不受影响
    cron_lines others | crontab -
    info "已取消开机自启、日志轮转与守护"
    others="$(cron_lines others | grep -cF "$CRON_MARKER")"
    if [[ "$others" =~ ^[0-9]+$ ]] && ((others > 0)); then
        info "crontab 里另有 $others 行其他目录的 aistudio2api 任务（同一标记），未改动"
    fi
}

usage() {
    awk 'NR > 2 { if (/^#/) { sub(/^# ?/, ""); print } else exit }' "$APP_DIR/${BASH_SOURCE[0]##*/}"
}

cmd="${1:-}"
case "$cmd" in
    start) user_lock && start ;;
    stop) user_lock && stop ;;
    restart) user_lock && stop && start ;;
    watchdog)
        # 输出会收进 watchdog.log：在终端手动执行时也不带颜色转义
        C_G='' C_Y='' C_R='' C_0=''
        watchdog_env
        watchdog
        ;;
    status) status ;;
    log | logs)
        [[ -f "$LOG_FILE" ]] || { fail "还没有日志：$LOG_FILE"; exit 1; }
        tail -n 200 -F "$LOG_FILE"
        ;;
    build) build ;;
    rotate) rotate_log ;;
    enable) enable_boot ;;
    disable) disable_boot ;;
    disk) disk_usage ;;
    clean-cache) clean_cache ;;
    diag) APP_DIR="$APP_DIR" bash "$APP_DIR/tools/diag_slow.sh" ;;
    report) APP_DIR="$APP_DIR" bash "$APP_DIR/tools/report.sh" ;;
    trace) APP_DIR="$APP_DIR" bash "$APP_DIR/tools/trace_pack.sh" ;;
    *)
        usage
        exit 1
        ;;
esac
