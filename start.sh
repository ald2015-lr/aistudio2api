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
#   ./start.sh enable    开机自启并每小时轮转日志（写入当前用户 crontab）
#   ./start.sh disable   取消开机自启与日志轮转
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

set -uo pipefail

APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$APP_DIR" || exit 1

APP_NAME="aistudio2api"
BIN="$APP_DIR/$APP_NAME"
RUN_DIR="$APP_DIR/run"
LOG_DIR="$APP_DIR/logs"
PID_FILE="$RUN_DIR/$APP_NAME.pid"
LOG_FILE="$LOG_DIR/$APP_NAME.log"
LOG_MAX_BYTES=$((50 * 1024 * 1024))
STOP_TIMEOUT="${STOP_TIMEOUT:-90}"
START_WAIT="${START_WAIT:-300}"
CRON_MARKER="# aistudio2api-autostart"

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

# env_value KEY DEFAULT：与程序相同的优先级，进程环境变量优先，其次 .env
env_value() {
    local key="$1" default="${2-}" value="" line
    if [[ -n "${!key+x}" ]]; then
        value="${!key}"
    elif [[ -f .env ]]; then
        line="$(grep -E "^[[:space:]]*${key}[[:space:]]*=" .env 2>/dev/null | tail -n 1 | tr -d '\r')"
        if [[ -n "$line" ]]; then
            value="$(trim "${line#*=}")"
            case "$value" in
                \"*\" | \'*\') value="${value:1:${#value}-2}" ;;
                *) value="$(trim "${value%% #*}")" ;;
            esac
        fi
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
    local pid
    pid="$(pgrep -f -- "^${BIN}( |\$)" 2>/dev/null | head -n 1)"
    if [[ -n "$pid" ]]; then
        printf '%s' "$pid"
        return 0
    fi
    # 按 README 用 ./aistudio2api 直接启动的实例命令行是相对路径：按进程名查找，再用工作目录确认是本目录的实例
    for pid in $(pgrep -x -- "$APP_NAME" 2>/dev/null); do
        if [[ "$(readlink "/proc/$pid/cwd" 2>/dev/null)" == "$APP_DIR" ]]; then
            printf '%s' "$pid"
            return 0
        fi
    done
    return 1
}

find_pid() {
    local pid=""
    if [[ -f "$PID_FILE" ]]; then
        pid="$(tr -cd '0-9' <"$PID_FILE")"
        if [[ -n "$pid" ]] && alive "$pid" && [[ "$(ps -p "$pid" -o comm= 2>/dev/null)" == "$APP_NAME" ]]; then
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
    local pid="$1" offset="$2" deadline=$((SECONDS + START_WAIT)) state="" last="" auto
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
            rm -f "$PID_FILE"
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

start() {
    local pid launched offset
    if pid="$(find_pid)"; then
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
    kill_orphan_camoufox
    check_camoufox_deps

    mkdir -p "$RUN_DIR" "$LOG_DIR"
    rotate_log
    printf '\n===== %s start.sh start =====\n' "$(date '+%F %T')" >>"$LOG_FILE"
    offset="$(stat -c %s "$LOG_FILE" 2>/dev/null || wc -c <"$LOG_FILE")"

    if have setsid; then
        nohup setsid "$BIN" -open-ui=false >>"$LOG_FILE" 2>&1 </dev/null &
    else
        nohup "$BIN" -open-ui=false >>"$LOG_FILE" 2>&1 </dev/null &
    fi
    launched=$!
    sleep 1
    if alive "$launched" && [[ "$(ps -p "$launched" -o comm= 2>/dev/null)" == "$APP_NAME" ]]; then
        pid="$launched"
    else
        pid="$(pid_by_path)"
    fi
    if [[ -z "$pid" ]]; then
        fail "进程启动后立即退出，最近日志："
        tail -n 30 "$LOG_FILE" >&2
        return 1
    fi
    printf '%s\n' "$pid" >"$PID_FILE"
    info "已启动（PID $pid），日志：$LOG_FILE"
    wait_ready "$pid" "$offset"
}

stop() {
    local pid i
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
    if alive "$pid"; then
        warn "${STOP_TIMEOUT}s 内未退出，强制结束"
        kill -KILL "$pid" 2>/dev/null
        sleep 1
    fi
    rm -f "$PID_FILE"
    kill_orphan_camoufox
    info "已停止"
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
        return 3
    fi
    info "运行中（PID $pid，已运行 $(trim "$(ps -p "$pid" -o etime= 2>/dev/null)")）"
    status_detail
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

enable_boot() {
    if ! have crontab; then
        fail "未找到 crontab；请安装 cron，或改用 systemd 管理"
        return 1
    fi
    local line="@reboot sleep 20 && \"$APP_DIR/start.sh\" start >/dev/null 2>&1 $CRON_MARKER"
    local rotate="17 * * * * \"$APP_DIR/start.sh\" rotate >/dev/null 2>&1 $CRON_MARKER"
    { crontab -l 2>/dev/null | grep -vF "$CRON_MARKER"; printf '%s\n%s\n' "$line" "$rotate"; } | crontab -
    info "已设置开机自启与每小时日志轮转：
  $line
  $rotate"
}

disable_boot() {
    have crontab || return 0
    crontab -l 2>/dev/null | grep -vF "$CRON_MARKER" | crontab -
    info "已取消开机自启与日志轮转"
}

usage() {
    sed -n '3,23p' "$0" | sed 's/^# \{0,1\}//'
}

cmd="${1:-}"
case "$cmd" in
    start) start ;;
    stop) stop ;;
    restart) stop && start ;;
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
