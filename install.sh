#!/usr/bin/env bash
set -Eeuo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

APP_NAME="xboard-node"
INSTALL_ROOT="/etc/xboard-node"
BACKUP_DIR="${INSTALL_ROOT}/backups"
INSTALL_META="${INSTALL_ROOT}/install-meta.json"
CONFIG_FILE="${INSTALL_ROOT}/config.yml"
CREDENTIALS_FILE="${INSTALL_ROOT}/credentials.env"
BINARY_PATH="/usr/local/bin/xboard-node"
SERVICE_NAME="xboard-node.service"
SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}"
CLI_PATH="/usr/local/bin/xbctl"
INSTALLER_COPY_PATH="${INSTALL_ROOT}/install.sh"
CLI_BINARY_SOURCE=""
DEFAULT_HEALTH_PORT=65530
DEFAULT_KERNEL="singbox"
DEFAULT_MODE="node"
DEFAULT_ACTION="install"
DEFAULT_RELEASE_VERSION="latest"
DEFAULT_LOG_LEVEL="info"
DEFAULT_KERNEL_LOG_LEVEL="warn"
RELEASE_REPOSITORY="${XBOARD_NODE_REPOSITORY:-Toto0912/xboard-node-u}"

ACTION="${DEFAULT_ACTION}"
MODE=""
PANEL_URL=""
TOKEN=""
NODE_ID=""
NODE_TYPE=""
MACHINE_ID=""
KERNEL_TYPE="${DEFAULT_KERNEL}"
RELEASE_VERSION="${DEFAULT_RELEASE_VERSION}"
HEALTH_PORT="${DEFAULT_HEALTH_PORT}"
RUNTIME_GOMEMLIMIT=""
RUNTIME_GOGC=""
BINARY_SOURCE=""
CLI_BINARY_SOURCE=""
FORCE_RECONFIGURE=0
EXPECT_EMPTY=0
INTERACTIVE=0
MODE_GIVEN=0
PURGE=0
YES=0
ARCH=""
OS=""
DOWNLOAD_URL=""
CURRENT_STATE="fresh"
TMP_DIR=""
BACKUP_PATH=""
OLD_ACTIVE_STATE=inactive
WAS_ENABLED=0
OLD_HEALTH_PORT=0
SERVICE_WORK_DIR="$INSTALL_ROOT"
MUTATING=0
CLEANUP_DONE=0

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }
log_step()  { echo -e "${CYAN}[STEP]${NC} ${BOLD}$1${NC}"; }

cleanup_tmp() {
    if [ "$CLEANUP_DONE" -eq 1 ]; then
        return
    fi
    CLEANUP_DONE=1
    if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
        rm -rf "$TMP_DIR"
    fi
}

# The checked candidate reads the same normalized first instance as the node.
load_deployment() {
    local parsed="" inspector
    for inspector in "$TMP_DIR/xbctl" "$CLI_PATH"; do
        [[ -x "$inspector" ]] || continue
        if parsed=$("$inspector" config deployment-port --config "$1" --credentials "$2" --working-dir "$SERVICE_WORK_DIR" --installed-config "$CONFIG_FILE" 2>"$TMP_DIR/probe.err"); then
            break
        fi
        if ! grep -Fxq 'unknown config command: deployment-port' "$TMP_DIR/probe.err"; then
            cat "$TMP_DIR/probe.err" >&2; return 1
        fi
    done
    if [[ -z "$parsed" ]]; then
        # v2.4.1 already provides typed JSON metadata, including the first
        # instance's port. Its old health-port command skipped zero-valued ports.
        "$TMP_DIR/xbctl" config metadata --config "$1" --meta "$TMP_DIR/probe.json" --version "$RELEASE_VERSION" || return 1
        parsed=$(awk '/^[[:space:]]*"instance_count":[[:space:]]*[0-9]+,?$/ {n=$2+0; have=1}
            /^[[:space:]]*"health_port":[[:space:]]*[0-9]+,?$/ && first=="" {first=$2; sub(/,$/,"",first)}
            END{if(!have)exit 1; if(n==0)print "empty"; else if(first!="")print first; else exit 1}' "$TMP_DIR/probe.json") || return 1
        if [[ "$parsed" == empty ]] && ! grep -Eq '^instances:[[:space:]]*\[\][[:space:]]*(#.*)?$' "$1"; then
            log_error "Legacy inspector cannot confirm an empty binding list"; return 1
        fi
    fi
    case "$parsed" in
        empty) HEALTH_PORT=empty ;;
        ''|*[!0-9]*) log_error "Cannot determine the effective health port"; return 1 ;;
        *) HEALTH_PORT=$parsed ;;
    esac
}

lock_installation() {
    mkdir -p "$(dirname "$BINARY_PATH")"
    exec 9<"$(dirname "$BINARY_PATH")"
    flock -n 9 || { log_error "Another node install or upgrade is in progress"; return 1; }
}

record_service_state() {
    local state
    if ! state=$(systemctl show --property=ActiveState --value "$SERVICE_NAME"); then
        if [[ "$CURRENT_STATE" == fresh && "$state" == inactive ]]; then return; fi
        log_error "Cannot read state for $SERVICE_NAME"; return 1
    fi
    case "$state" in
        active|inactive|failed) OLD_ACTIVE_STATE=$state ;;
        *) log_error "Service is changing state ($state); retry when settled"; return 1 ;;
    esac
    if [[ "$CURRENT_STATE" != fresh && -f "$SERVICE_PATH" ]]; then
        SERVICE_WORK_DIR=$(systemctl show --property=WorkingDirectory --value "$SERVICE_NAME") || return 1
        SERVICE_WORK_DIR=${SERVICE_WORK_DIR:-/}
    fi
    if systemctl is-enabled --quiet "$SERVICE_NAME"; then WAS_ENABLED=1; fi
    if [[ "$OLD_ACTIVE_STATE" == active && -f "$CONFIG_FILE" ]]; then
        load_deployment "$CONFIG_FILE" "$CREDENTIALS_FILE"
        OLD_HEALTH_PORT=$HEALTH_PORT
    fi
}

rollback_install() {
    local state failed=0
    log_warn "Rolling back installation"
    if ! systemctl stop "$SERVICE_NAME" && [[ "$CURRENT_STATE" != fresh ]]; then
        log_error "Could not stop the candidate during rollback"
        failed=1
    fi
    if [[ "$WAS_ENABLED" == 0 ]] && systemctl is-enabled --quiet "$SERVICE_NAME"; then
        systemctl disable "$SERVICE_NAME" >/dev/null || failed=1
    fi
    if [ -n "$BACKUP_PATH" ] && [ -d "$BACKUP_PATH" ]; then
        if [ -f "$BACKUP_PATH/xboard-node" ]; then
            install -m 755 "$BACKUP_PATH/xboard-node" "$BINARY_PATH"
        else
            rm -f "$BINARY_PATH"
        fi
        if [ -f "$BACKUP_PATH/config.yml" ]; then
            install -m 600 "$BACKUP_PATH/config.yml" "$CONFIG_FILE"
        else
            rm -f "$CONFIG_FILE"
        fi
        if [ -f "$BACKUP_PATH/credentials.env" ]; then
            install -m 600 "$BACKUP_PATH/credentials.env" "$CREDENTIALS_FILE"
        else
            rm -f "$CREDENTIALS_FILE"
        fi
        if [ -f "$BACKUP_PATH/install-meta.json" ]; then
            install -m 644 "$BACKUP_PATH/install-meta.json" "$INSTALL_META"
        else
            rm -f "$INSTALL_META"
        fi
        if [ -f "$BACKUP_PATH/install.sh" ]; then
            replace_saved_installer "$BACKUP_PATH/install.sh" || failed=1
        elif [[ "$ACTION" != upgrade ]]; then
            rm -f "$INSTALLER_COPY_PATH"
        fi
        if [ -f "$BACKUP_PATH/xbctl" ]; then
            install -m 755 "$BACKUP_PATH/xbctl" "$CLI_PATH"
        else
            rm -f "$CLI_PATH"
        fi
        if [ -f "$BACKUP_PATH/${SERVICE_NAME}" ]; then
            install -m 644 "$BACKUP_PATH/${SERVICE_NAME}" "$SERVICE_PATH"
        else
            rm -f "$SERVICE_PATH"
        fi
    fi
    HEALTH_PORT=$OLD_HEALTH_PORT
    systemctl daemon-reload || failed=1
    if [[ "$WAS_ENABLED" == 1 ]] && ! systemctl is-enabled --quiet "$SERVICE_NAME"; then
        systemctl enable "$SERVICE_NAME" >/dev/null || return 1
    fi
    if [[ "$OLD_ACTIVE_STATE" == active ]]; then
        systemctl reset-failed "$SERVICE_NAME" >/dev/null 2>&1 || true
        systemctl restart "$SERVICE_NAME" >/dev/null 2>&1 || true
        if ! wait_for_health; then
            log_error "Rollback completed but restored service did not become healthy"
            show_recent_logs
            return 1
        fi
    elif [[ "$CURRENT_STATE" != fresh ]]; then
        state=$(systemctl show --property=ActiveState --value "$SERVICE_NAME") || return 1
        if [[ "$OLD_ACTIVE_STATE" == inactive && "$state" == failed ]]; then
            systemctl reset-failed "$SERVICE_NAME" || return 1
            state=$(systemctl show --property=ActiveState --value "$SERVICE_NAME") || return 1
        fi
        if [[ "$state" != "$OLD_ACTIVE_STATE" ]]; then
            log_error "Restored service state is $state (expected $OLD_ACTIVE_STATE)"
            return 1
        fi
    fi
    [[ "$failed" == 0 ]] || return 1
    log_warn "Rollback complete"
}

on_error() {
    local exit_code=$?
    local line_no=${1:-unknown}
    if [ "$exit_code" -ne 0 ]; then
        log_error "Install failed at line ${line_no} (exit=${exit_code})"
        if [[ "$MUTATING" == 1 ]]; then
            rollback_install || log_error "Rollback incomplete; check the restored files and service state"
        fi
    fi
    cleanup_tmp
    exit "$exit_code"
}
trap 'on_error $LINENO' ERR
trap cleanup_tmp EXIT

usage() {
    cat <<'HELP'

  xboard-node Installer

  ACTIONS:
    install      Install or reconcile the configured deployment (default)
    upgrade      Upgrade binary and restart service
    uninstall    Remove installed service and binary (config kept unless --purge)
    status       Show current installation status
    help         Show this help

  MODES (auto-detected from --node-id or --machine-id if omitted):
    --mode node      Panel single-node mode (default)
    --mode machine   Panel machine mode

  REQUIRED FOR NODE MODE:
    --panel, -a      Panel URL
    --token, -t      Panel server token
    --node-id, -n    Node ID

  REQUIRED FOR MACHINE MODE:
    --panel, -a       Panel URL
    --token, -t       Machine token
    --machine-id      Machine ID

  OPTIONAL:
    --interactive       Ask for mode, kernel, panel and credentials in the terminal
    --node-type, -T     Explicit node type for node mode
    --kernel, -k        singbox or xray (default: singbox)
    --version           Release version or latest (default: latest)
    --binary            Use a local xboard-node binary path instead of downloading
    --xbctl-binary      Use a local xbctl binary path instead of downloading
    --health-port       Local health port (default: 65530, use 0 to disable)
    --gomemlimit        Runtime GOMEMLIMIT value, e.g. 256MiB
    --gogc              Runtime GOGC value, e.g. 50
    --force-reconfigure Overwrite an existing install even if mode/target changed
    --expect-empty      Upgrade only a stopped installation with no bindings
    --purge             With uninstall, delete /etc/xboard-node too
    --yes, -y           Non-interactive confirmation for destructive operations

  ENVIRONMENT:
    XBOARD_NODE_REPOSITORY  Download repository (default: Toto0912/xboard-node-u)

  EXAMPLES:
    sudo bash install.sh --interactive
    sudo bash install.sh --panel https://panel.example.com --token TOKEN --node-id 1
    sudo bash install.sh --panel https://panel.example.com --token TOKEN --machine-id 1
    sudo bash install.sh upgrade
    sudo bash install.sh uninstall --purge --yes

HELP
}

parse_args() {
    local positional=()
    while [ $# -gt 0 ]; do
        case "$1" in
            install|upgrade|uninstall|status|help)
                ACTION="$1"
                shift
                ;;
            --mode)
                MODE="$2"
                MODE_GIVEN=1
                shift 2
                ;;
            --panel|-a|--api)
                PANEL_URL="$2"
                shift 2
                ;;
            --token|-t)
                TOKEN="$2"
                shift 2
                ;;
            --node-id|-n)
                NODE_ID="$2"
                shift 2
                ;;
            --node-type|-T)
                NODE_TYPE="$2"
                shift 2
                ;;
            --machine-id)
                MACHINE_ID="$2"
                shift 2
                ;;
            --kernel|-k)
                KERNEL_TYPE="$2"
                shift 2
                ;;
            --version)
                RELEASE_VERSION="$2"
                shift 2
                ;;
            --binary)
                BINARY_SOURCE="$2"
                shift 2
                ;;
            --xbctl-binary)
                CLI_BINARY_SOURCE="$2"
                shift 2
                ;;
            --health-port)
                HEALTH_PORT="$2"
                shift 2
                ;;
            --gomemlimit)
                RUNTIME_GOMEMLIMIT="$2"
                shift 2
                ;;
            --gogc)
                RUNTIME_GOGC="$2"
                shift 2
                ;;
            --force-reconfigure)
                FORCE_RECONFIGURE=1
                shift
                ;;
            --expect-empty)
                EXPECT_EMPTY=1
                shift
                ;;
            --interactive)
                INTERACTIVE=1
                shift
                ;;
            --purge)
                PURGE=1
                shift
                ;;
            --yes|-y)
                YES=1
                shift
                ;;
            --help|-h)
                ACTION="help"
                shift
                ;;
            *)
                positional+=("$1")
                shift
                ;;
        esac
    done

    if [ ${#positional[@]} -gt 0 ] && [ "$ACTION" = "install" ]; then
        ACTION="${positional[0]}"
    fi
    if [[ "$EXPECT_EMPTY" == 1 && "$ACTION" != upgrade ]]; then
        log_error "--expect-empty is only valid for upgrade"; return 1
    fi

    case "$KERNEL_TYPE" in
        singbox|SingBox|SINGBOX) KERNEL_TYPE="singbox" ;;
        xray|Xray|XRAY) KERNEL_TYPE="xray" ;;
        *) ;;
    esac

    # Auto-detect mode from arguments when --mode is not specified.
    if [ -z "$MODE" ]; then
        if [ -n "$MACHINE_ID" ]; then
            MODE="machine"
        else
            MODE="node"
        fi
    fi

    case "$MODE" in
        node|machine) ;;
        *)
            log_error "Unsupported mode: $MODE"
            usage
            exit 1
            ;;
    esac
}

read_terminal() {
    local target_name="$1" prompt_text="$2" default_value="${3:-}" hide_value="${4:-0}" answer_value
    if [ "$hide_value" -eq 1 ]; then
        IFS= read -r -s -p "$prompt_text" answer_value </dev/tty || return 1
        printf '\n' >/dev/tty
    else
        IFS= read -r -p "$prompt_text" answer_value </dev/tty || return 1
    fi
    printf -v "$target_name" '%s' "${answer_value:-$default_value}"
}

configure_interactively() {
    if ! { true </dev/tty; } 2>/dev/null; then
        log_error "Interactive installation requires a terminal; use --panel, --token and --node-id/--machine-id for unattended installation"
        return 1
    fi

    printf '\n=== xboard-node-u 安装向导 ===\n' >/dev/tty
    local mode_choice kernel_choice target_id
    if [ "$MODE_GIVEN" -eq 0 ] && [ -z "$MACHINE_ID" ] && [ -z "$NODE_ID" ]; then
        while true; do
            read_terminal mode_choice '部署模式：1) 机器模式  2) 单节点模式 [1]: ' '1'
            case "$mode_choice" in
                1|machine) MODE=machine; break ;;
                2|node) MODE=node; break ;;
                *) printf '请输入 1 或 2。\n' >/dev/tty ;;
            esac
        done
    fi

    local kernel_default=1
    [ "$KERNEL_TYPE" != xray ] || kernel_default=2
    while true; do
        read_terminal kernel_choice "选择内核：1) sing-box  2) Xray [${kernel_default}]: " "$kernel_default"
        case "$kernel_choice" in
            1|singbox) KERNEL_TYPE=singbox; break ;;
            2|xray) KERNEL_TYPE=xray; break ;;
            *) printf '请输入 1 或 2。\n' >/dev/tty ;;
        esac
    done

    while [ -z "$PANEL_URL" ]; do
        read_terminal PANEL_URL '面板地址（https://panel.example.com）: '
        if ! [[ "$PANEL_URL" =~ ^https?://[^[:space:]]+$ ]]; then
            printf '请输入以 http:// 或 https:// 开头的面板地址。\n' >/dev/tty
            PANEL_URL=''
        fi
    done

    if [ "$MODE" = machine ]; then
        target_id="$MACHINE_ID"
    else
        target_id="$NODE_ID"
    fi
    while ! [[ "$target_id" =~ ^[1-9][0-9]*$ ]]; do
        if [ "$MODE" = machine ]; then
            read_terminal target_id '机器 ID: '
        else
            read_terminal target_id '节点 ID: '
        fi
    done
    if [ "$MODE" = machine ]; then MACHINE_ID="$target_id"; else NODE_ID="$target_id"; fi

    while [ -z "$TOKEN" ]; do
        if [ "$MODE" = machine ]; then
            read_terminal TOKEN '机器令牌（输入不显示）: ' '' 1
        else
            read_terminal TOKEN '面板通信密钥（输入不显示）: ' '' 1
        fi
    done
    printf '\n配置已填写，开始下载安装并创建 systemd 服务。\n\n' >/dev/tty
}

check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        log_error "Please run as root or with sudo"
        exit 1
    fi
}

detect_arch() {
    local raw
    raw=$(uname -m)
    case "$raw" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *)
            log_error "Unsupported architecture: $raw"
            exit 1
            ;;
    esac
}

detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS="$ID"
    else
        OS="unknown"
    fi
}

ensure_systemd() {
    if ! command -v systemctl >/dev/null 2>&1; then
        log_error "systemd is required for this installer"
        exit 1
    fi
    if [ ! -d /run/systemd/system ]; then
        log_error "This host does not appear to be running systemd"
        exit 1
    fi
}

run_with_retry() {
    local attempts="$1"
    local delay="$2"
    shift 2
    local i=1
    while [ "$i" -le "$attempts" ]; do
        if "$@"; then
            return 0
        fi
        if [ "$i" -lt "$attempts" ]; then
            log_warn "Command failed, retrying in ${delay}s: $*"
            sleep "$delay"
        fi
        i=$((i + 1))
    done
    return 1
}

install_dependencies() {
    local -a firewall_packages=()
    if ! command -v nft >/dev/null 2>&1 && { ! command -v iptables >/dev/null 2>&1 || ! command -v ip6tables >/dev/null 2>&1; }; then
        firewall_packages=(nftables)
    fi
    case "$OS" in
        ubuntu|debian)
            DEBIAN_FRONTEND=noninteractive run_with_retry 10 3 apt-get update -qq
            DEBIAN_FRONTEND=noninteractive run_with_retry 10 3 apt-get install -y -qq curl wget ca-certificates "${firewall_packages[@]}" >/dev/null 2>&1
            ;;
        centos|rhel|rocky|almalinux|fedora)
            if command -v dnf >/dev/null 2>&1; then
                run_with_retry 5 3 dnf install -y -q curl wget ca-certificates "${firewall_packages[@]}" >/dev/null 2>&1
            else
                run_with_retry 5 3 yum install -y -q curl wget ca-certificates "${firewall_packages[@]}" >/dev/null 2>&1
            fi
            ;;
        *)
            log_warn "OS ${OS} is not in the official support set; continuing best-effort"
            ;;
    esac
}

ensure_dirs() {
    mkdir -p "$INSTALL_ROOT" "$BACKUP_DIR"
    chmod 700 "$INSTALL_ROOT"
}

validate_positive_int() {
    local label="$1"
    local value="$2"
    if ! [[ "$value" =~ ^[0-9]+$ ]] || [ "$value" -le 0 ]; then
        log_error "${label} must be a positive integer, got: ${value}"
        exit 1
    fi
}

validate_install_request() {
    if [ -z "$PANEL_URL" ]; then
        log_error "Panel URL is required"
        exit 1
    fi
    if [ -z "$TOKEN" ]; then
        log_error "Token is required"
        exit 1
    fi
    if ! [[ "$HEALTH_PORT" =~ ^[0-9]+$ ]]; then
        log_error "health-port must be a non-negative integer"
        exit 1
    fi
    case "$KERNEL_TYPE" in
        singbox|xray) ;;
        *)
            log_error "Kernel must be singbox or xray"
            exit 1
            ;;
    esac
    case "$MODE" in
        node)
            validate_positive_int "Node ID" "$NODE_ID"
            ;;
        machine)
            validate_positive_int "Machine ID" "$MACHINE_ID"
            ;;
    esac
}

detect_current_state() {
    local has_binary=0 has_config=0 has_service=0
    [ -x "$BINARY_PATH" ] && has_binary=1
    [ -f "$CONFIG_FILE" ] && has_config=1
    [ -f "$SERVICE_PATH" ] && has_service=1

    if [ "$has_binary" -eq 0 ] && [ "$has_config" -eq 0 ] && [ "$has_service" -eq 0 ]; then
        CURRENT_STATE="fresh"
    elif [ "$has_binary" -eq 1 ] && [ "$has_config" -eq 1 ] && [ "$has_service" -eq 1 ]; then
        CURRENT_STATE="installed"
    else
        CURRENT_STATE="partial"
    fi
}

require_reconfigure_confirmation() {
    return
}

# Only explicitly supplied paths are trusted as local builds. All other files
# come from one concrete release, even when the caller requested "latest".
stage_release() {
    local node="xboard-node-linux-$ARCH" cli="xbctl-linux-$ARCH"
    local base="https://github.com/$RELEASE_REPOSITORY/releases" resolved name src output reported
    local i
    local -a remote=() names=("$node" "$cli") sources=("$BINARY_SOURCE" "$CLI_BINARY_SOURCE")
    if [[ -z "$BINARY_SOURCE" || -z "$CLI_BINARY_SOURCE" ]]; then
        if ! [[ "$RELEASE_REPOSITORY" =~ ^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+$ ]] ||
           [[ "$RELEASE_REPOSITORY" == */. || "$RELEASE_REPOSITORY" == */.. ]]; then
            log_error "Invalid release repository: $RELEASE_REPOSITORY"; return 1
        fi
        if [[ "$RELEASE_VERSION" == latest ]]; then
            resolved=$(curl -fsSL --proto '=https' --proto-redir '=https' --retry 2 --connect-timeout 20 --max-time 90 -o /dev/null -w '%{url_effective}' "$base/latest")
            if [[ "${resolved,,}" != "${base,,}/tag/"* ]]; then
                log_error "Latest release did not resolve to a tag in $RELEASE_REPOSITORY"; return 1
            fi
            RELEASE_VERSION=${resolved:${#base}+5}
        fi
        if ! [[ "$RELEASE_VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]*$ ]]; then
            log_error "Invalid release tag: $RELEASE_VERSION"; return 1
        fi
        base="$base/download/$RELEASE_VERSION"
        fetch_asset "$base/SHA256SUMS.txt" "$TMP_DIR/SHA256SUMS.txt"
        if [[ "$ACTION" == upgrade && -e "$INSTALLER_COPY_PATH" ]]; then
            if [[ ! -f "$INSTALLER_COPY_PATH" ]]; then
                log_error "Expected a regular installer: $INSTALLER_COPY_PATH"; return 1
            fi
            fetch_asset "$base/install.sh" "$TMP_DIR/install.sh"
            remote+=("install.sh")
        fi
    fi
    for i in 0 1; do
        name=${names[i]}; src=${sources[i]}
        if [[ -n "$src" ]]; then
            log_step "Using operator-provided binary: $src"
            cp -- "$src" "$TMP_DIR/$name"
        else
            fetch_asset "$base/$name" "$TMP_DIR/$name"
            remote+=("$name")
        fi
    done
    for name in "${remote[@]}"; do
        # Reconstruct one canonical line so malformed paths and duplicate
        # binary/text entries cannot select a different file for verification.
        local sum
        sum=$(awk -v target="$name" '{ sub(/\r$/,""); file=$2; sub(/^\*/,"",file); if(file==target){n++; if(NF!=2 || length($1)!=64 || $1~/[^a-fA-F0-9]/) bad=1; hash=$1} } END{if(n!=1 || bad) exit 1; print hash}' "$TMP_DIR/SHA256SUMS.txt") || {
            log_error "Missing, invalid or duplicate SHA256 for $name"; return 1; }
        (cd "$TMP_DIR" && printf '%s  %s\n' "$sum" "$name" | sha256sum -c -)
    done
    mv -- "$TMP_DIR/$node" "$TMP_DIR/xboard-node"
    mv -- "$TMP_DIR/$cli" "$TMP_DIR/xbctl"
    chmod 755 "$TMP_DIR/xboard-node" "$TMP_DIR/xbctl"
    local first_version="" arg
    for name in xboard-node xbctl; do
        if [[ "$name" == xboard-node ]]; then arg=-v; else arg=version; fi
        output=$("$TMP_DIR/$name" "$arg")
        local program
        read -r program reported _ <<< "$output"
        if [[ "$program" != "$name" || -z "$reported" || ( ${#remote[@]} -gt 0 && "$reported" != "$RELEASE_VERSION" ) ||
              ( -n "$first_version" && "$reported" != "$first_version" ) ]]; then
            log_error "Unexpected version from $name (expected ${first_version:-$RELEASE_VERSION})"; return 1
        fi
        first_version=$reported
    done
    if [[ "$RELEASE_VERSION" == latest ]]; then RELEASE_VERSION=$first_version; fi
}

fetch_asset() {
    log_step "Downloading $1"
    curl -fsSL --proto '=https' --proto-redir '=https' --retry 2 --connect-timeout 20 --max-time 180 -o "$2" "$1"
}

render_config() {
    local init_args=(
        config init
        --mode "$MODE"
        --panel-url "$PANEL_URL"
        --kernel "${KERNEL_TYPE:-singbox}"
        --health-port "${HEALTH_PORT:-0}"
        --token "$TOKEN"
        --version "$RELEASE_VERSION"
        --output "$TMP_DIR/config.yml"
        --credentials-out "$TMP_DIR/credentials.env"
        --meta "$TMP_DIR/install-meta.json"
        --install-root "$INSTALL_ROOT"
    )
    if [ -f "$CONFIG_FILE" ]; then
        init_args+=(--config "$CONFIG_FILE")
    fi
    if [ -f "$CREDENTIALS_FILE" ]; then
        init_args+=(--credentials-in "$CREDENTIALS_FILE")
    fi
    if [ "$MODE" = "machine" ]; then
        init_args+=(--machine-id "$MACHINE_ID")
    else
        init_args+=(--node-id "$NODE_ID")
        if [ -n "$NODE_TYPE" ]; then
            init_args+=(--node-type "$NODE_TYPE")
        fi
    fi
    if [ -n "$RUNTIME_GOMEMLIMIT" ]; then
        init_args+=(--gomemlimit "$RUNTIME_GOMEMLIMIT")
    fi
    if [ -n "$RUNTIME_GOGC" ] && [ "$RUNTIME_GOGC" -gt 0 ] 2>/dev/null; then
        init_args+=(--gogc "$RUNTIME_GOGC")
    fi

    local output
    output=$("$TMP_DIR/xbctl" "${init_args[@]}") || {
        log_error "xbctl config init failed"
        exit 1
    }

    INSTANCE_ID=$(echo "$output" | grep '^INSTANCE_ID=' | cut -d= -f2-)
    chmod 600 "$TMP_DIR/credentials.env"
}

render_service() {
    cat >"$TMP_DIR/${SERVICE_NAME}" <<EOF_UNIT
[Unit]
Description=Xboard Node Backend
Documentation=https://github.com/cedar2025/xboard-node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${INSTALL_ROOT}
EnvironmentFile=-${CREDENTIALS_FILE}
ExecStart=${BINARY_PATH} -c ${CONFIG_FILE}
ExecStopPost=${BINARY_PATH} --cleanup-port-hopping
TimeoutStopSec=90
Restart=always
RestartSec=5
LimitNOFILE=1048576
NoNewPrivileges=true
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF_UNIT
}

backup_existing_state() {
    BACKUP_PATH="${BACKUP_DIR}/$(date +%Y%m%d-%H%M%S)"
    if ! mkdir -m 700 "$BACKUP_PATH"; then
        BACKUP_PATH=$(mktemp -d "${BACKUP_PATH}.XXXXXX")
    fi
    if [ -x "$BINARY_PATH" ]; then
        cp "$BINARY_PATH" "$BACKUP_PATH/xboard-node"
    fi
    if [ -x "$CLI_PATH" ]; then
        cp "$CLI_PATH" "$BACKUP_PATH/xbctl"
    fi
    if [ -f "$CONFIG_FILE" ]; then
        cp "$CONFIG_FILE" "$BACKUP_PATH/config.yml"
    fi
    if [ -f "$CREDENTIALS_FILE" ]; then
        cp "$CREDENTIALS_FILE" "$BACKUP_PATH/credentials.env"
    fi
    if [ -f "$INSTALL_META" ]; then
        cp "$INSTALL_META" "$BACKUP_PATH/install-meta.json"
    fi
    if [ -f "$INSTALLER_COPY_PATH" ]; then
        cp "$INSTALLER_COPY_PATH" "$BACKUP_PATH/install.sh"
    fi
    if [ -f "$SERVICE_PATH" ]; then
        cp "$SERVICE_PATH" "$BACKUP_PATH/${SERVICE_NAME}"
    fi
}

stop_existing_service() {
    if [ -f "$SERVICE_PATH" ] || systemctl is-active "$SERVICE_NAME" >/dev/null 2>&1; then
        systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
    fi
}

replace_saved_installer() {
    local dest staged
    dest=$(realpath -m -- "$INSTALLER_COPY_PATH") || return 1
    staged=$(mktemp -- "${dest}.XXXXXX") || return 1
    if install -m 755 -- "$1" "$staged" && mv -fT -- "$staged" "$dest"; then return 0; fi
    rm -f -- "$staged"
    return 1
}

install_staged_files() {
    stop_existing_service
    install -m 755 "$TMP_DIR/xboard-node" "$BINARY_PATH"
    install -m 600 "$TMP_DIR/config.yml" "$CONFIG_FILE"
    install -m 600 "$TMP_DIR/credentials.env" "$CREDENTIALS_FILE"
    install -m 644 "$TMP_DIR/install-meta.json" "$INSTALL_META"
    if [ -f "$0" ] && [ "$(realpath "$0")" != "$(realpath "$INSTALLER_COPY_PATH" 2>/dev/null || echo "$INSTALLER_COPY_PATH")" ]; then
        replace_saved_installer "$0"
    fi
    install -m 755 "$TMP_DIR/xbctl" "$CLI_PATH"
    install -m 644 "$TMP_DIR/${SERVICE_NAME}" "$SERVICE_PATH"
    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME" > /dev/null 2>&1
}

wait_for_health() {
    local first="" current props state pid restarts attempt=0
    while (( attempt < 30 )); do
        props=$(systemctl show --property=ActiveState,MainPID,NRestarts "$SERVICE_NAME") || return 1
        state=; pid=; restarts=
        while IFS='=' read -r key value; do
            case "$key" in ActiveState) state=$value ;; MainPID) pid=$value ;; NRestarts) restarts=$value ;; esac
        done <<< "$props"
        [[ "$state" == active && "$pid" =~ ^[1-9][0-9]*$ && "$restarts" =~ ^[0-9]+$ ]] || return 1
        current=$pid:$restarts
        if [[ -z "$first" ]]; then first=$current; fi
        [[ "$first" == "$current" ]] || return 1
        if (( attempt >= 6 )); then
            if [[ "$HEALTH_PORT" == 0 ]] || [[ $(curl -sS --max-time 1 --noproxy '*' -o /dev/null -w '%{http_code}' "http://127.0.0.1:$HEALTH_PORT/healthz") == 200 ]]; then return 0; fi
        fi
        sleep 1
        attempt=$((attempt+1))
    done
    return 1
}

show_recent_logs() {
    if command -v journalctl >/dev/null 2>&1; then
        journalctl -u "$SERVICE_NAME" -n 30 --no-pager || true
    fi
}

start_service() {
    if systemctl is-enabled "$SERVICE_NAME" >/dev/null 2>&1; then
        systemctl restart "$SERVICE_NAME"
    else
        systemctl start "$SERVICE_NAME"
    fi
    if ! wait_for_health; then
        log_error "Service failed health check"
        show_recent_logs
        return 1
    fi
}

perform_install() {
    validate_install_request
    lock_installation
    detect_current_state
    require_reconfigure_confirmation
    TMP_DIR=$(mktemp -d)
    ensure_dirs
    stage_release
    render_config
    render_service
    record_service_state
    load_deployment "$TMP_DIR/config.yml" "$TMP_DIR/credentials.env"
    backup_existing_state
    MUTATING=1
    install_staged_files
    start_service

    ensure_cli_link
    log_info "Installation succeeded"
    log_info "Service: ${SERVICE_NAME}"
    log_info "Config: ${CONFIG_FILE}"
    log_info "Credentials: ${CREDENTIALS_FILE}"
    if [[ "$HEALTH_PORT" != 0 ]]; then
        log_info "Health: http://127.0.0.1:${HEALTH_PORT}/healthz"
    fi
    log_info "CLI: ${CLI_PATH}  (run '${CLI_PATH} list' if xbctl is not in PATH)"
}

perform_upgrade() {
    local checked_config="$CONFIG_FILE" legacy_sums=""
    detect_current_state
    if [[ "$EXPECT_EMPTY" == 1 && "$CURRENT_STATE" != installed ]]; then
        log_error "--expect-empty requires an existing installation"; return 1
    fi
    if [ "$CURRENT_STATE" = "fresh" ]; then
        log_warn "No existing install found; falling back to install"
        perform_install
        return
    fi
    lock_installation
    TMP_DIR=$(mktemp -d)
    ensure_dirs
    stage_release
    render_service
    record_service_state
    if [[ "$EXPECT_EMPTY" == 1 && "$OLD_ACTIVE_STATE" != inactive ]]; then
        log_error "--expect-empty requires a stopped service"; return 1
    fi
    if ! load_deployment "$CONFIG_FILE" "$CREDENTIALS_FILE" 2>"$TMP_DIR/layout.err"; then
        if [[ "$EXPECT_EMPTY" != 1 ]]; then cat "$TMP_DIR/layout.err" >&2; return 1; fi
        legacy_sums=$(sha256sum -- "$CONFIG_FILE" "$INSTALL_META") || return 1
        checked_config="$TMP_DIR/empty.yml"
        if ! "$TMP_DIR/xbctl" config migrate-empty-v241 --config "$CONFIG_FILE" --meta "$INSTALL_META" --output "$checked_config"; then
            cat "$TMP_DIR/layout.err" >&2
            return 1
        fi
        load_deployment "$checked_config" "$CREDENTIALS_FILE"
        log_info "Confirmed an empty v2.4.1 deployment; upgrading its saved configuration"
    fi
    if [[ "$EXPECT_EMPTY" == 1 && "$HEALTH_PORT" != empty ]]; then
        log_error "--expect-empty could not confirm an empty deployment"; return 1
    fi
    "$TMP_DIR/xbctl" config metadata --config "$checked_config" --meta "$TMP_DIR/install-meta.json" --version "$RELEASE_VERSION"
    backup_existing_state
    # Historical bind commands did not take the install lock. Keep a newer
    # configuration if an operator rebound or restarted the node during staging.
    if [[ -n "$legacy_sums" ]]; then
        local current_sums state
        current_sums=$(sha256sum -- "$CONFIG_FILE" "$INSTALL_META") || return 1
        state=$(systemctl show --property=ActiveState --value "$SERVICE_NAME") || return 1
        if [[ "$current_sums" != "$legacy_sums" || "$state" != inactive ]]; then
            log_error "Legacy files or service state changed while preparing migration"; return 1
        fi
    fi
    MUTATING=1
    if [[ "$checked_config" != "$CONFIG_FILE" ]]; then
        install -m 600 "$checked_config" "$CONFIG_FILE"
    fi
    install -m 755 "$TMP_DIR/xboard-node" "$BINARY_PATH"
    install -m 755 "$TMP_DIR/xbctl" "$CLI_PATH"
    install -m 644 "$TMP_DIR/install-meta.json" "$INSTALL_META"
    install -m 644 "$TMP_DIR/${SERVICE_NAME}" "$SERVICE_PATH"
    if [[ -f "$TMP_DIR/install.sh" ]]; then
        replace_saved_installer "$TMP_DIR/install.sh"
    fi
    systemctl daemon-reload
    if [[ "$HEALTH_PORT" == empty ]]; then
        if [[ "$OLD_ACTIVE_STATE" == active ]]; then systemctl stop "$SERVICE_NAME"; fi
        ensure_cli_link
        log_info "Upgrade succeeded; no node bindings, service remains stopped"
        return
    fi
    systemctl restart "$SERVICE_NAME"
    if ! wait_for_health; then
        log_error "Upgrade health check failed"
        show_recent_logs
        return 1
    fi
    ensure_cli_link
    log_info "Upgrade succeeded"
}

ensure_cli_link() {
    if [[ ! -e /usr/bin/xbctl && ! -L /usr/bin/xbctl ]]; then
        ln -s "$CLI_PATH" /usr/bin/xbctl || log_warn "Could not create the optional /usr/bin/xbctl link"
    fi
}

confirm_uninstall() {
    if [ "$YES" -eq 1 ]; then
        return
    fi
    echo
    read -r -p "Proceed with uninstall? [y/N]: " answer
    if ! [[ "$answer" =~ ^[Yy]$ ]]; then
        log_warn "Uninstall cancelled"
        exit 0
    fi
}

perform_uninstall() {
    confirm_uninstall
    if [ -f "$SERVICE_PATH" ]; then
        systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
        systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
    fi
    if [ -x "$BINARY_PATH" ] && [ -d /run/xboard-node/port-hopping ]; then
        "$BINARY_PATH" --cleanup-port-hopping --require-stopped || { log_error "HY2 redirect cleanup failed; binaries preserved"; exit 1; }
    fi
    if [ -f "$SERVICE_PATH" ]; then
        rm -f "$SERVICE_PATH"
        systemctl daemon-reload || true
    fi
    rm -f "$BINARY_PATH"
    rm -f "$CLI_PATH"
    rm -f /usr/bin/xbctl 2>/dev/null || true
    if [ "$PURGE" -eq 1 ]; then
        rm -rf "$INSTALL_ROOT"
        log_info "Removed ${INSTALL_ROOT}"
    else
        rm -f "$INSTALL_META"
        log_info "Config preserved under ${INSTALL_ROOT}"
    fi
    log_info "Uninstall complete"
}

perform_status() {
    detect_current_state
    echo
    echo -e "${BOLD}xboard-node install status${NC}"
    echo "  state:   ${CURRENT_STATE}"
    if [ -f "$INSTALL_META" ]; then
        echo "  meta:    ${INSTALL_META}"
        if [ -x "$CLI_PATH" ]; then
            "$CLI_PATH" list 2>/dev/null || true
        else
            # Simple key extraction from JSON (no Python needed)
            local val
            for key in config_mode version latest_instance_id instance_count updated_at; do
                val=$(sed -n "s/.*\"${key}\": *\"\{0,1\}\([^\"]*\)\"\{0,1\}.*/\1/p" "$INSTALL_META" | head -1)
                val="${val%,}"  # strip trailing comma from numeric JSON values
                [ -n "$val" ] && echo "  ${key}: ${val}"
            done
        fi
    fi
    if [ -f "$SERVICE_PATH" ]; then
        echo "  service: ${SERVICE_NAME}"
        systemctl status "$SERVICE_NAME" --no-pager || true
    fi
}

main() {
    parse_args "$@"
    case "$ACTION" in
        help)
            usage
            exit 0
            ;;
        status)
            ensure_systemd
            perform_status
            exit 0
            ;;
    esac

    check_root
    detect_arch
    detect_os
    ensure_systemd
    if [ "$ACTION" = install ] && [ "$INTERACTIVE" -eq 1 ]; then
        configure_interactively
    fi
    install_dependencies

    case "$ACTION" in
        install)
            perform_install
            ;;
        upgrade)
            perform_upgrade
            ;;
        uninstall)
            perform_uninstall
            ;;
        *)
            log_error "Unknown action: $ACTION"
            usage
            exit 1
            ;;
    esac
}

if [[ "${BASH_SOURCE[0]:-$0}" = "$0" ]]; then
    main "$@"
fi
