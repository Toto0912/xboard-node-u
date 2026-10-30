#!/bin/sh
# Standalone Alpine/OpenRC installer for Toto0912/xboard-node-u.
# Does not require Bash, systemd, Docker, or xbctl.
set -eu
umask 077

REPO=${XBOARD_NODE_REPOSITORY:-Toto0912/xboard-node-u}
ROOT=/etc/xboard-node
BIN=/usr/local/bin/xboard-node
SERVICE=/etc/init.d/xboard-node
LAUNCHER=$ROOT/openrc-launch.sh
MANAGER=$ROOT/install-alpine.sh
CLI=/usr/local/bin/xbctl
OLD_MANAGER=/usr/local/sbin/xb-alpine
VERSION_FILE=$ROOT/alpine-release
LOCK=/run/xboard-node-alpine-install.lock
ACTION=install
VERSION=latest
MODE=
KERNEL=singbox
PANEL=
TARGET=
TOKEN=
LOCAL_BINARY=
LOCAL_CLI=
LOCAL_SUMS=
YES=0
PURGE=0
RECONFIGURE=0
INTERACTIVE=0
CONFIG_ARGS=0
TMP=
BACKUP=
MUTATING=0
WAS_RUNNING=0
WAS_ENABLED=0
LOCKED=0
RC_LOCAL_UPDATED=0
CRON_UPDATED=0
LEGACY_FOUND=0

info() { printf '%s\n' "[INFO] $*"; }
die() { printf '%s\n' "[ERROR] $*" >&2; exit 1; }

usage() {
    cat <<'HELP'
Alpine / OpenRC 一键安装 xboard-node-u（以 root 执行）

  sh install-alpine.sh                 交互安装，默认机器模式 + sing-box
  sh install-alpine.sh upgrade         保留现有配置并更新程序
  xbctl status / service restart / service logs
  xbctl upgrade [--version v1.0.1]
  xbctl uninstall --yes                卸载程序和服务，保留配置/备份/日志

无人值守安装：
  --mode machine|node --panel URL --id ID --kernel singbox|xray
  --token-file FILE                    从文件读取令牌（首行，不进入命令参数）
  --version latest|vX.Y.Z              默认最新正式版
  --reconfigure                       明确允许重新生成已有配置
  --yes                               确认卸载
  --purge                             卸载时同时删除 /etc/xboard-node
  --binary FILE --xbctl-binary FILE --checksums FILE
                                       离线安装同一版本程序和 xbctl，校验 SHA256

自动安装 OpenRC 兼容的 xbctl，与 systemd 使用相同管理命令。
HELP
}

parse() {
    while [ "$#" -gt 0 ]; do
        case "$1" in
            install|upgrade|uninstall|status|start|stop|restart|logs) ACTION=$1; shift ;;
            --interactive) INTERACTIVE=1; shift ;;
            --yes|-y) YES=1; shift ;;
            --purge) PURGE=1; shift ;;
            --reconfigure) RECONFIGURE=1; shift ;;
            --help|-h|help) usage; exit 0 ;;
            --mode|--panel|--id|--kernel|--version|--token-file|--binary|--xbctl-binary|--checksums)
                if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "$1 缺少参数"; fi
                case "$1" in --mode|--panel|--id|--kernel|--token-file) CONFIG_ARGS=1 ;; esac
                case "$1" in
                    --mode) MODE=$2 ;;
                    --panel) PANEL=$2 ;;
                    --id) TARGET=$2 ;;
                    --kernel) KERNEL=$2 ;;
                    --version) VERSION=$2 ;;
                    --token-file)
                        [ -f "$2" ] || die "令牌文件不存在"
                        TOKEN=$(head -n 1 "$2") ;;
                    --binary) LOCAL_BINARY=$2 ;;
                    --xbctl-binary) LOCAL_CLI=$2 ;;
                    --checksums) LOCAL_SUMS=$2 ;;
                esac
                shift 2 ;;
            *) die "未知参数：$1；使用 --help 查看用法" ;;
        esac
    done
}

prompt() {
    printf '%s' "$1" >&3
    IFS= read -r ANSWER <&3 || die "输入已中断"
}

wizard() {
    # Read /dev/tty, so downloading through a pipe cannot consume answers.
    { exec 3<>/dev/tty; } 2>/dev/null || die "没有交互终端，请传入 --mode --panel --id --token-file"
    info "开始配置；回车使用括号内的默认选项。"
    while :; do
        prompt '部署模式：1 机器模式 / 2 单节点模式 [1]：'
        case "$ANSWER" in ''|1) MODE=machine; break ;; 2) MODE=node; break ;; esac
    done
    while :; do
        prompt '选择内核：1 sing-box / 2 Xray [1]：'
        case "$ANSWER" in ''|1) KERNEL=singbox; break ;; 2) KERNEL=xray; break ;; esac
    done
    prompt '面板地址（网站根地址）：'; PANEL=$ANSWER
    if [ "$MODE" = machine ]; then
        prompt '机器 ID：'; TARGET=$ANSWER
        label='机器令牌：'
    else
        prompt '节点 ID：'; TARGET=$ANSWER
        label='面板通信密钥：'
    fi
    printf '%s' "$label" >&3
    TTY_STATE=$(stty -g <&3)
    stty -echo <&3
    if ! IFS= read -r TOKEN <&3; then
        stty "$TTY_STATE" <&3
        TTY_STATE=
        die "输入已中断"
    fi
    stty "$TTY_STATE" <&3
    TTY_STATE=
    printf '\n' >&3
    exec 3>&-
}

validate_config_input() {
    case "$MODE" in machine|node) ;; *) die '模式必须是 machine 或 node' ;; esac
    case "$KERNEL" in singbox|xray) ;; *) die '内核必须是 singbox 或 xray' ;; esac
    case "$PANEL" in https://?*|http://?*) ;; *) die '面板地址必须以 https:// 或 http:// 开头' ;; esac
    case "$PANEL" in *\?*|*\#*|*\ *|*\@*) die '请填写不含登录信息、查询参数的网站根地址' ;; esac
    case "$TARGET" in ''|*[!0-9]*) die 'ID 必须是正整数' ;; esac
    if [ "${#TARGET}" -gt 9 ] || [ "$TARGET" -le 0 ]; then die 'ID 必须是有效的正整数'; fi
    TARGET=$(printf '%s' "$TARGET" | sed 's/^0*//')
    [ -n "$TOKEN" ] || die '令牌不能为空'
    # No control characters in YAML scalars; punctuation is escaped, not evaluated.
    if printf '%s%s' "$PANEL" "$TOKEN" | LC_ALL=C grep -q '[[:cntrl:]]'; then
        die '地址或令牌含有控制字符'
    fi
}

yaml_quote() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

download() {
    curl --fail --silent --show-error --location --retry 3 \
        --connect-timeout 20 --max-time 600 --proto '=https' --proto-redir '=https' "$1" -o "$2"
}

stage_binary() {
    case "$(uname -m)" in
        x86_64|amd64) ARCH=amd64 ;;
        aarch64|arm64) ARCH=arm64 ;;
        *) die '仅支持 amd64 / arm64' ;;
    esac
    ASSET=xboard-node-linux-$ARCH
    CLI_ASSET=xbctl-linux-$ARCH
    if [ -n "$LOCAL_BINARY" ]; then
        if [ ! -f "$LOCAL_BINARY" ] || [ ! -f "$LOCAL_CLI" ] || [ ! -f "$LOCAL_SUMS" ]; then
            die '离线安装必须提供 --binary、--xbctl-binary 和 --checksums 文件'
        fi
        cp "$LOCAL_BINARY" "$TMP/$ASSET"
        cp "$LOCAL_CLI" "$TMP/$CLI_ASSET"
        cp "$LOCAL_SUMS" "$TMP/SHA256SUMS.txt"
        [ "$VERSION" != latest ] || VERSION=local
    else
        [ -z "$LOCAL_SUMS$LOCAL_CLI" ] || die '--checksums / --xbctl-binary 只能与 --binary 一起使用'
        if [ "$VERSION" = latest ]; then
            resolved=$(curl --fail --silent --show-error --location --retry 3 \
                --connect-timeout 20 --max-time 90 --proto '=https' --proto-redir '=https' \
                --output /dev/null --write-out '%{url_effective}' "https://github.com/$REPO/releases/latest")
            case "$resolved" in "https://github.com/$REPO/releases/tag/"*) VERSION=${resolved##*/} ;; *) die '无法确定最新正式版' ;; esac
        fi
        case "$VERSION" in ''|*[!a-zA-Z0-9._-]*) die '版本号格式错误' ;; esac
        info "下载 $VERSION / $ARCH"
        BASE=https://github.com/$REPO/releases/download/$VERSION
        download "$BASE/$ASSET" "$TMP/$ASSET"
        download "$BASE/$CLI_ASSET" "$TMP/$CLI_ASSET"
        download "$BASE/SHA256SUMS.txt" "$TMP/SHA256SUMS.txt"
    fi
    tr -d '\r' < "$TMP/SHA256SUMS.txt" > "$TMP/checksums.normalized"
    awk -v node="$ASSET" -v cli="$CLI_ASSET" '$2 == node || $2 == "*" node || $2 == cli || $2 == "*" cli {print}' "$TMP/checksums.normalized" > "$TMP/selected.sha256"
    for name in "$ASSET" "$CLI_ASSET"; do
        count=$(awk -v name="$name" '$2 == name || $2 == "*" name {n++} END {print n+0}' "$TMP/selected.sha256")
        [ "$count" = 1 ] || die '校验清单缺失或重复'
    done
    (cd "$TMP" && sha256sum -c selected.sha256) || die 'SHA256 校验失败，未安装程序'
    chmod 755 "$TMP/$ASSET" "$TMP/$CLI_ASSET"
    "$TMP/$ASSET" -v > "$TMP/version.txt" || die '程序无法在本机运行'
    grep -q '^xboard-node ' "$TMP/version.txt" || die '程序版本输出不正确'
    "$TMP/$CLI_ASSET" version >/dev/null || die 'xbctl 无法在本机运行'
    backend=$("$TMP/$CLI_ASSET" service backend 2>/dev/null) || die '该版本 xbctl 不支持 OpenRC；请安装支持 Alpine 的新版 Release'
    [ "$backend" = openrc ] || die '此安装器要求正在运行的 OpenRC'
}

render_files() {
    if [ "$WRITE_CONFIG" = 1 ]; then
        {
            printf 'panel:\n  url: "%s"\n' "$(yaml_quote "${PANEL%/}")"
            if [ "$MODE" = machine ]; then
                printf 'machine:\n  machine_id: %s\n  token: "%s"\n' "$((TARGET + 0))" "$(yaml_quote "$TOKEN")"
            else
                printf '  node_id: %s\n  token: "%s"\n' "$((TARGET + 0))" "$(yaml_quote "$TOKEN")"
            fi
            printf 'kernel:\n  type: %s\n  config_dir: /var/lib/xboard-node\n  log_level: warn\n' "$KERNEL"
            printf 'log:\n  level: info\n  output: stdout\nhealth_port: 0\n'
        } > "$TMP/config.yml"
    fi
    cat > "$TMP/launcher" <<'LAUNCH'
#!/bin/sh
set -eu
# Compatibility with credentials.env generated by xbctl: literal KEY=value,
# never source/eval it, so token punctuation cannot execute shell commands.
if [ -f /etc/xboard-node/credentials.env ]; then
    while IFS= read -r entry || [ -n "$entry" ]; do
        case "$entry" in ''|\#*) continue ;; *=*) ;; *) exit 1 ;; esac
        key=${entry%%=*}
        case "$key" in ''|[0-9]*|*[!a-zA-Z0-9_]*) exit 1 ;; esac
        export "$entry"
    done < /etc/xboard-node/credentials.env
fi
exec /usr/local/bin/xboard-node -c /etc/xboard-node/config.yml
LAUNCH
    cat > "$TMP/service" <<'OPENRC'
#!/sbin/openrc-run
# Managed by the standalone xboard-node Alpine installer.
description="Xboard node backend"
supervisor="supervise-daemon"
command="/etc/xboard-node/openrc-launch.sh"
directory="/etc/xboard-node"
pidfile="/run/${RC_SVCNAME}.pid"
output_log="/var/log/xboard-node/node.log"
error_log="/var/log/xboard-node/error.log"
respawn_delay=5
respawn_max=5
respawn_period=60
retry="TERM/30/KILL/5"
depend() {
    need net
}
start_pre() {
    checkpath --directory --mode 0700 /var/lib/xboard-node /var/log/xboard-node
    checkpath --file --mode 0600 "$output_log" "$error_log"
}
OPENRC
    cat > "$TMP/logrotate" <<'ROTATE'
/var/log/xboard-node/*.log {
    daily
    rotate 3
    size 10M
    missingok
    notifempty
    copytruncate
    compress
}
ROTATE
}

managed_files() {
    printf '%s\n' "$BIN" "$CLI" "$ROOT/config.yml" "$ROOT/install-meta.json" "$LAUNCHER" "$SERVICE" "$VERSION_FILE" "$MANAGER" "$OLD_MANAGER" /etc/logrotate.d/xboard-node \
        "$ROOT/xboard-node-run.sh" "$ROOT/xboard-node.pid"
}

snapshot() {
    mkdir -p "$ROOT/backups"
    chmod 700 "$ROOT" "$ROOT/backups"
    BACKUP=$(mktemp -d "$ROOT/backups/alpine-$(date +%Y%m%d-%H%M%S)-XXXXXX")
    managed_files | while IFS= read -r path; do
        if [ -e "$path" ]; then
            mkdir -p "$BACKUP$(dirname "$path")"
            cp -p "$path" "$BACKUP$path"
        fi
    done
    if [ -f "$ROOT/credentials.env" ]; then
        cp -p "$ROOT/credentials.env" "$BACKUP/credentials.env.reference"
    fi
    rc-service xboard-node status >/dev/null 2>&1 && WAS_RUNNING=1 || WAS_RUNNING=0
    [ -e /etc/runlevels/default/xboard-node ] && WAS_ENABLED=1 || WAS_ENABLED=0
}

atomic_install() {
    src=$1 dest=$2 mode=$3
    mkdir -p "$(dirname "$dest")"
    cp "$src" "$dest.alpine-new"
    chmod "$mode" "$dest.alpine-new"
    mv -f "$dest.alpine-new" "$dest"
}

node_pid() {
    # rc-service status may still say "started" between supervisor respawns.
    # Confirm its direct child is the installed ELF program, not just the supervisor.
    [ -f /run/xboard-node.pid ] || return 1
    supervisor_pid=$(cat /run/xboard-node.pid)
    case "$supervisor_pid" in ''|*[!0-9]*) return 1 ;; esac
    children=/proc/$supervisor_pid/task/$supervisor_pid/children
    [ -r "$children" ] || return 1
    child_list=$(cat "$children") || return 1
    # Kernel-provided whitespace-separated decimal PIDs, not arbitrary filenames.
    for child in $child_list; do
        executable=$(readlink "/proc/$child/exe" 2>/dev/null) || continue
        if [ "$executable" = "$BIN" ] && kill -0 "$child" 2>/dev/null; then
            printf '%s\n' "$child"
            return 0
        fi
    done
    return 1
}

check_legacy_processes() {
    # Files and PID numbers can survive uninstall or be reused by unrelated processes.
    # Inspect actual command lines; never signal a process based only on an old PID.
    managed_pid=$(node_pid || :)
    for proc in /proc/[0-9]*; do
        [ -r "$proc/cmdline" ] || continue
        if tr '\000' '\n' < "$proc/cmdline" 2>/dev/null | grep -Fxq "$ROOT/xboard-node-run.sh"; then
            die "旧看门狗进程仍在运行（PID ${proc##*/}），请先停止它；本脚本未停止或修改该进程"
        fi
        exe=$(readlink "$proc/exe" 2>/dev/null) || continue
        case "$exe" in "$BIN"|"$BIN (deleted)") ;; *) continue ;; esac
        [ "${proc##*/}" != "$managed_pid" ] || continue
        if tr '\000' '\n' < "$proc/cmdline" 2>/dev/null | grep -Fxq "$ROOT/config.yml"; then
            die "旧节点进程仍在运行（PID ${proc##*/}），请先停止它；配置和程序尚未更改"
        fi
    done
}

filter_legacy_boot() {
    # Remove only lines owned by the supplied old installer, keeping other tasks.
    awk -v script="$ROOT/xboard-node-run.sh" -v marker='# xboard-node-autostart' \
        '!(index($0, script) && index($0, marker))' "$1"
}

legacy_boot_present() {
    awk -v script="$ROOT/xboard-node-run.sh" -v marker='# xboard-node-autostart' \
        'index($0, script) && index($0, marker) {found=1} END {exit !found}' "$1"
}

cleanup_legacy() {
    for stale in "$ROOT/xboard-node-run.sh" "$ROOT/xboard-node.pid"; do
        if [ -e "$stale" ]; then
            rm -f "$stale"
            LEGACY_FOUND=1
        fi
    done
    if [ -f /etc/rc.local ] && legacy_boot_present /etc/rc.local; then
        RC_LOCAL_PATH=$(readlink -f /etc/rc.local)
        cp -p "$RC_LOCAL_PATH" "$TMP/rc.local.original"
        filter_legacy_boot "$TMP/rc.local.original" > "$TMP/rc.local.clean"
        if ! cmp -s "$TMP/rc.local.original" "$TMP/rc.local.clean"; then
            cp -p "$TMP/rc.local.original" "$BACKUP/legacy-rc.local"
            cmp -s "$RC_LOCAL_PATH" "$TMP/rc.local.original" || die 'rc.local 在安装期间变化，已中止以保护其他设置'
            atomic_install "$TMP/rc.local.clean" "$RC_LOCAL_PATH" "$(stat -c %a "$RC_LOCAL_PATH")"
            RC_LOCAL_UPDATED=1
            LEGACY_FOUND=1
        fi
    fi
    if command -v crontab >/dev/null 2>&1 && crontab -l > "$TMP/cron.original" 2>/dev/null \
        && legacy_boot_present "$TMP/cron.original"; then
        filter_legacy_boot "$TMP/cron.original" > "$TMP/cron.clean"
        if ! cmp -s "$TMP/cron.original" "$TMP/cron.clean"; then
            cp "$TMP/cron.original" "$BACKUP/legacy-crontab"
            crontab -l > "$TMP/cron.current"
            cmp -s "$TMP/cron.current" "$TMP/cron.original" || die 'crontab 在安装期间变化，已中止以保护其他任务'
            crontab "$TMP/cron.clean"
            CRON_UPDATED=1
            LEGACY_FOUND=1
        fi
    fi
    if [ "$LEGACY_FOUND" = 1 ]; then info '旧服务未运行，已备份并清理残留脚本、PID 文件及对应旧自启条目。'; fi
}

restore_legacy_boot() {
    if [ "$RC_LOCAL_UPDATED" = 1 ]; then
        if cmp -s "$RC_LOCAL_PATH" "$TMP/rc.local.clean"; then
            atomic_install "$BACKUP/legacy-rc.local" "$RC_LOCAL_PATH" "$(stat -c %a "$BACKUP/legacy-rc.local")"
        else
            info 'rc.local 后续被其他程序修改，未覆盖；原文件在备份目录。'
        fi
    fi
    if [ "$CRON_UPDATED" = 1 ]; then
        crontab -l > "$TMP/cron.rollback-current" 2>/dev/null
        if cmp -s "$TMP/cron.rollback-current" "$TMP/cron.clean"; then
            crontab "$BACKUP/legacy-crontab"
        else
            info 'crontab 后续被其他程序修改，未覆盖；原任务在备份目录。'
        fi
    fi
}

rollback() {
    info '安装未完成，正在恢复旧程序、配置和服务状态。'
    rc-service xboard-node stop >/dev/null 2>&1 || :
    managed_files | while IFS= read -r path; do
        rm -f "$path.alpine-new"
        if [ -f "$BACKUP$path" ]; then
            cp -p "$BACKUP$path" "$path.alpine-restore"
            mv -f "$path.alpine-restore" "$path"
        else
            rm -f "$path"
        fi
    done
    if [ "$WAS_ENABLED" = 1 ]; then rc-update add xboard-node default >/dev/null 2>&1 || :
    else rc-update del xboard-node default >/dev/null 2>&1 || :; fi
    restore_legacy_boot
    if [ "$WAS_RUNNING" = 1 ]; then
        rc-service xboard-node zap >/dev/null 2>&1 || :
        if ! rc-service xboard-node start; then
            printf '%s\n' '[ERROR] 旧文件已恢复，但旧服务启动失败，请检查日志。' >&2
        fi
    fi
    info "备份保存在 $BACKUP"
}

finish() {
    result=$1
    trap - 0 INT TERM
    set +e
    if [ -n "${TTY_STATE:-}" ]; then stty "$TTY_STATE" <&3; printf '\n' >&3; fi
    if [ "$MUTATING" = 1 ]; then rollback; fi
    if [ -n "$TMP" ]; then rm -rf -- "$TMP"; fi
    if [ "$LOCKED" = 1 ]; then rmdir "$LOCK"; fi
    exit "$result"
}

install_or_upgrade() {
    WRITE_CONFIG=0
    if [ "$ACTION" = upgrade ]; then
        [ -f "$ROOT/config.yml" ] || die '没有现有配置，请先运行安装'
        [ "$RECONFIGURE" = 0 ] || die '重新配置请使用 install --reconfigure'
        [ "$CONFIG_ARGS" = 0 ] || die 'upgrade 保留配置；修改绑定/内核请使用 install --reconfigure'
    elif [ -f "$ROOT/config.yml" ] && [ "$RECONFIGURE" = 0 ]; then
        [ "$CONFIG_ARGS" = 0 ] || die '现有配置默认保留；如需更换绑定，请添加 --reconfigure'
        info '检测到已有配置，将保留并更新程序。'
    else
        WRITE_CONFIG=1
        if [ "$INTERACTIVE" = 1 ] || [ -z "$MODE$PANEL$TARGET$TOKEN" ]; then wizard; fi
        validate_config_input
    fi
    check_legacy_processes
    if ! command -v curl >/dev/null 2>&1 || ! command -v supervise-daemon >/dev/null 2>&1 \
        || ! command -v logrotate >/dev/null 2>&1 || [ ! -s /etc/ssl/certs/ca-certificates.crt ]; then
        apk add --no-cache curl ca-certificates openrc logrotate
    fi
    TMP=$(mktemp -d /tmp/xboard-alpine.XXXXXX)
    stage_binary
    render_files
    [ -f "$0" ] || die '请先将脚本下载为文件，再使用 sh 执行，以便保存升级入口'
    snapshot
    MUTATING=1
    if [ "$WAS_RUNNING" = 1 ]; then rc-service xboard-node stop; fi
    # Check again just before removing stale files in case a legacy boot job raced us.
    check_legacy_processes
    cleanup_legacy
    atomic_install "$TMP/$ASSET" "$BIN" 755
    atomic_install "$TMP/$CLI_ASSET" "$CLI" 755
    if [ "$WRITE_CONFIG" = 1 ]; then atomic_install "$TMP/config.yml" "$ROOT/config.yml" 600; fi
    atomic_install "$TMP/launcher" "$LAUNCHER" 700
    atomic_install "$TMP/service" "$SERVICE" 755
    atomic_install "$TMP/logrotate" /etc/logrotate.d/xboard-node 644
    # Copy a running installer safely before replacing its installed pathname.
    cp "$0" "$TMP/installer"
    atomic_install "$TMP/installer" "$MANAGER" 755
    rm -f "$OLD_MANAGER"
    printf '%s\n' "$VERSION" > "$TMP/release"
    atomic_install "$TMP/release" "$VERSION_FILE" 600
    "$CLI" config metadata --config "$ROOT/config.yml" --meta "$TMP/install-meta.json" --version "$VERSION"
    atomic_install "$TMP/install-meta.json" "$ROOT/install-meta.json" 644
    rc-update add xboard-node default
    rc-service xboard-node zap >/dev/null 2>&1 || :
    rc-service xboard-node start
    # Require a stable real child, not just a live supervisor waiting to respawn.
    tries=0
    previous_pid=
    while [ "$tries" -lt 5 ]; do
        sleep 1
        rc-service xboard-node status >/dev/null 2>&1 || die '服务未稳定运行，请检查 /var/log/xboard-node/'
        current_pid=$(node_pid) || die '节点程序已退出；未将看门狗存活误判为安装成功，正在回滚'
        if [ -n "$previous_pid" ] && [ "$previous_pid" != "$current_pid" ]; then
            die '节点程序在启动检查期间反复退出，正在回滚'
        fi
        previous_pid=$current_pid
        tries=$((tries + 1))
    done
    MUTATING=0
    info '安装完成：已配置 OpenRC 后台运行、异常重启和开机自启。'
    cat "$TMP/version.txt"
    info '进程已启动；面板连接和节点下发情况请查看 xbctl service logs。'
    info '配置：/etc/xboard-node/config.yml；管理：xbctl status / service restart / upgrade'
    info "回退备份：$BACKUP"
    if ! rc-service crond status >/dev/null 2>&1; then
        info 'crond 未运行，日志轮转暂不会定时执行；可按需启用 crond。'
    fi
}

uninstall() {
    [ "$YES" = 1 ] || die '卸载需添加 --yes；配置、备份和日志会保留'
    if [ -f "$SERVICE" ]; then
        rc-service xboard-node stop >/dev/null 2>&1 || die '停止服务失败，未删除程序'
        rc-update del xboard-node default >/dev/null 2>&1 || :
    fi
    rm -f "$BIN" "$CLI" "$SERVICE" "$LAUNCHER" "$MANAGER" "$OLD_MANAGER" "$VERSION_FILE" "$ROOT/install-meta.json" /etc/logrotate.d/xboard-node
    if [ -L /usr/bin/xbctl ] && [ "$(readlink /usr/bin/xbctl)" = "$CLI" ]; then rm -f /usr/bin/xbctl; fi
    if [ "$PURGE" = 1 ]; then
        [ "$ROOT" = /etc/xboard-node ] || die '配置目录不正确，未清理'
        rm -rf -- "$ROOT"
        info '程序、OpenRC 服务及 /etc/xboard-node 已卸载。'
    else
        info '程序和 OpenRC 服务已卸载；配置、凭据、运行数据、日志和备份均保留。'
    fi
}

main() {
    parse "$@"
    case "$REPO" in */*) ;; *) die '仓库必须是 OWNER/REPOSITORY' ;; esac
    case "$REPO" in *[!a-zA-Z0-9_./-]*|*..*|/*|*/|*/*/*) die '仓库名称格式错误' ;; esac
    [ "$(id -u)" = 0 ] || die '请以 root 执行'
    [ -f /etc/alpine-release ] || die '此脚本仅适用于 Alpine Linux'
    command -v rc-service >/dev/null 2>&1 || die '此环境没有 OpenRC，请使用带 OpenRC 的 Alpine LXC 模板'
    case "$ACTION" in
        status)
            "$BIN" -v
            rc-service xboard-node status
            node_pid >/dev/null || die 'OpenRC 存活，但节点程序未运行'
            return ;;
        start|stop|restart) rc-service xboard-node "$ACTION"; return ;;
        logs) tail -n 80 -F /var/log/xboard-node/node.log /var/log/xboard-node/error.log; return ;;
    esac
    mkdir "$LOCK" 2>/dev/null || die "已有安装/升级任务，或残留锁目录：$LOCK"
    LOCKED=1
    trap 'finish $?' 0
    trap 'exit 130' INT
    trap 'exit 143' TERM
    case "$ACTION" in install|upgrade) install_or_upgrade ;; uninstall) uninstall ;; esac
}

main "$@"
