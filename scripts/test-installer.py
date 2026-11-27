"""Exercise the actual piped Bash installer using a PTY, without installing anything."""

import errno
import hashlib
import os
import pty
import select
import signal
import subprocess
import tempfile
import time
from pathlib import Path


INSTALLER = Path(__file__).resolve().parents[1] / "install.sh"
GUARD = 'if [[ "${BASH_SOURCE[0]:-$0}" = "$0" ]]; then'
STUBS = r'''
check_root() { :; }
detect_arch() { :; }
detect_os() { :; }
ensure_systemd() { :; }
install_dependencies() { :; }
perform_install() {
    validate_install_request
    printf 'RESULT|%s|%s|%s|%s|%s|%s\n' "$MODE" "$KERNEL_TYPE" "$PANEL_URL" "$MACHINE_ID" "$NODE_ID" "${#TOKEN}"
}
'''


def wizard_case(answers, expected, secret):
    """Keep /dev/tty interactive while the installer's stdin is a script pipe."""
    source = INSTALLER.read_text()
    assert source.count(GUARD) == 1
    script = source.replace(GUARD, STUBS + "\n" + GUARD).encode()
    read_fd, write_fd = os.pipe()
    child, terminal = pty.fork()
    if child == 0:
        os.close(write_fd)
        os.dup2(read_fd, 0)
        os.close(read_fd)
        os.execvp("bash", ["bash", "-s", "--", "--interactive"])
    os.close(read_fd)
    # Write in a child so a script larger than the pipe capacity cannot block
    # the parent from answering terminal prompts while Bash consumes stdin.
    writer = os.fork()
    if writer == 0:
        os.close(terminal)
        with os.fdopen(write_fd, "wb") as pipe:
            pipe.write(script)
        os._exit(0)
    os.close(write_fd)
    transcript = bytearray()
    pending = bytearray()
    next_answer = 0
    deadline = time.monotonic() + 20
    status = None
    try:
        while time.monotonic() < deadline:
            if select.select([terminal], [], [], 0.1)[0]:
                try:
                    block = os.read(terminal, 8192)
                except OSError as error:
                    if error.errno == errno.EIO:
                        break
                    raise
                if not block:
                    break
                transcript.extend(block)
                pending.extend(block)
            if next_answer < len(answers):
                prompt, response = answers[next_answer]
                marker = prompt.encode()
                if marker in pending:
                    pending = pending[pending.index(marker) + len(marker):]
                    os.write(terminal, (response + "\n").encode())
                    next_answer += 1
        else:
            raise AssertionError("Installer prompt timed out")
        _, status = os.waitpid(child, 0)
        assert os.waitstatus_to_exitcode(status) == 0, transcript.decode(errors="replace")
        assert next_answer == len(answers), transcript.decode(errors="replace")
        text = transcript.decode()
        assert expected in text, text
        if secret:
            assert secret not in text, "The terminal echoed the hidden token"
    finally:
        if status is None:
            os.kill(child, signal.SIGKILL)
            os.waitpid(child, 0)
        os.close(terminal)
        os.waitpid(writer, 0)


wizard_case(
    [("部署模式", ""), ("选择内核", ""), ("面板地址", "https://panel.example.com"),
     ("机器 ID", "7"), ("机器令牌", "machine-test-secret")],
    "RESULT|machine|singbox|https://panel.example.com|7||19", "machine-test-secret",
)
wizard_case(
    [("部署模式", "2"), ("选择内核", "2"), ("面板地址", "https://panel.example.com"),
     ("节点 ID", "3"), ("面板通信密钥", "node-test-secret")],
    "RESULT|node|xray|https://panel.example.com||3|16", "node-test-secret",
)
wizard_case(
    [("部署模式", "wrong"), ("部署模式", "1"), ("选择内核", "2"),
     ("面板地址", "invalid"), ("面板地址", "https://panel.example.com"),
     ("机器 ID", "0"), ("机器 ID", "9"), ("机器令牌", "retry-test-secret")],
    "RESULT|machine|xray|https://panel.example.com|9||17", "retry-test-secret",
)
wizard_case(
    [("部署模式", "3")],
    "RESULT|standalone|singbox||||0", "",
)
wizard_case(
    [("部署模式", "wrong"), ("部署模式", "standalone")],
    "RESULT|standalone|singbox||||0", "",
)

def standalone_install_checks():
    example = (INSTALLER.parent / "examples" / "standalone.yml.example").read_text()
    harness = r'''
source "$INSTALLER"
INSTALL_ROOT="$FIXTURE/etc"
BACKUP_DIR="$INSTALL_ROOT/backups"
INSTALL_META="$INSTALL_ROOT/install-meta.json"
CONFIG_FILE="$INSTALL_ROOT/config.yml"
CREDENTIALS_FILE="$INSTALL_ROOT/credentials.env"
BINARY_PATH="$FIXTURE/bin/xboard-node"
CLI_PATH="$FIXTURE/bin/xbctl"
SERVICE_PATH="$FIXTURE/systemd/xboard-node.service"
INSTALLER_COPY_PATH="$INSTALL_ROOT/install.sh"
mkdir -p "$FIXTURE/bin" "$FIXTURE/systemd"
lock_installation() { :; }
ensure_cli_link() { :; }
stage_release() {
    printf '#!/bin/sh\necho "xboard-node v2.4.3"\n' > "$TMP_DIR/xboard-node"
    cat > "$TMP_DIR/xbctl" <<'EOF'
#!/bin/sh
if [ "$1" = config ] && [ "$2" = metadata ]; then
    out=""
    while [ "$#" -gt 0 ]; do
        if [ "$1" = --meta ]; then out=$2; shift; fi
        shift
    done
    printf '{"config_mode":"instances","version":"v2.4.3","instance_count":1,"instances":[{"mode":"standalone"}]}\n' > "$out"
    exit 0
fi
if [ "$1" = config ] && [ "$2" = deployment-port ]; then
    echo 0
    exit 0
fi
echo "xbctl v2.4.3"
EOF
    chmod 755 "$TMP_DIR/xboard-node" "$TMP_DIR/xbctl"
    RELEASE_VERSION=v2.4.3
}
systemctl() {
    echo "$*" >> "$FIXTURE/systemctl.log"
    case "$1" in
        show) echo inactive ;;
        is-enabled|is-active) return 1 ;;
        daemon-reload|disable|stop) return 0 ;;
        *) echo "unexpected systemctl $*" >&2; return 1 ;;
    esac
}
MODE=standalone
MODE_GIVEN=1
if [ "$CASE" = conflict ]; then
    PANEL_URL="https://panel.example.com"
elif [ "$CASE" = force ]; then
    FORCE_RECONFIGURE=1
fi
perform_install
'''
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        env = {**os.environ, "INSTALLER": str(INSTALLER), "FIXTURE": tmp, "CASE": "fresh"}
        p = subprocess.run(["bash", "-c", harness], env=env, capture_output=True, text=True)
        assert p.returncode == 0, (p.stdout, p.stderr)
        assert (root / "etc" / "config.yml").read_text() == example
        assert not (root / "etc" / "credentials.env").exists()
        assert (root / "bin" / "xboard-node").is_file() and (root / "bin" / "xbctl").is_file()
        assert (root / "systemd" / "xboard-node.service").is_file()
        calls = (root / "systemctl.log").read_text().splitlines()
        assert any(c.startswith("disable ") for c in calls), calls
        assert not any(c.startswith(("enable ", "start ", "restart ")) for c in calls), calls
        assert "https://github.com/Toto0912/xboard-node-u/blob/main/docs/STANDALONE.md" in p.stdout, p.stdout
        assert "xbctl enable && xbctl start" in p.stdout, p.stdout

        # Existing config is preserved unless --force-reconfigure is set.
        (root / "etc" / "config.yml").write_text("# custom\n")
        p_keep = subprocess.run(["bash", "-c", harness], env={**env, "CASE": "existing"}, capture_output=True, text=True)
        assert p_keep.returncode != 0, (p_keep.stdout, p_keep.stderr)
        assert (root / "etc" / "config.yml").read_text() == "# custom\n"

        # --force-reconfigure overwrites with backup.
        p_force = subprocess.run(["bash", "-c", harness], env={**env, "CASE": "force"}, capture_output=True, text=True)
        assert p_force.returncode == 0, (p_force.stdout, p_force.stderr)
        assert (root / "etc" / "config.yml").read_text() == example

        # Conflicting panel arguments are rejected.
        p_bad = subprocess.run(["bash", "-c", harness], env={**env, "CASE": "conflict"}, capture_output=True, text=True)
        assert p_bad.returncode != 0, (p_bad.stdout, p_bad.stderr)


standalone_install_checks()

def staged_release(scenario, repository=None):
    with tempfile.TemporaryDirectory() as temp:
        root = Path(temp)
        stage, dist = root / "stage", root / "dist"
        stage.mkdir()
        dist.mkdir()
        names = ("xboard-node", "xbctl")
        hashes = []
        old_script, new_script = b"#!/bin/bash\n# v0\n", b"#!/bin/bash\n# v1\n"
        saved = root / "saved.sh"
        if scenario != "no-saved-installer":
            saved.write_bytes(old_script)
        if scenario == "saved-symlink":
            target = root / "actual-script"
            saved.rename(target)
            saved.symlink_to(target)
        if scenario != "no-saved-installer":
            (dist / "install.sh").write_bytes(new_script)
            hashes.append(f"{hashlib.sha256(new_script).hexdigest()} *install.sh")
        for name in names:
            asset = name + "-linux-amd64"
            tag = "v0" if scenario == "wrong-version" and name == "xbctl" else "v1"
            body = f'#!/bin/sh\necho {name} >> "$TRACE"\necho "{name} {tag}"\n'.encode()
            (dist / asset).write_bytes(body)
            hashes.append(f"{hashlib.sha256(body).hexdigest()} *{asset}")
        if scenario == "damaged-node":
            (dist / "xboard-node-linux-amd64").write_text("# damaged")
        if scenario == "damaged-cli":
            (dist / "xbctl-linux-amd64").write_text("# damaged")
        if scenario == "damaged-installer":
            (dist / "install.sh").write_text("# damaged")
        if scenario == "missing":
            hashes.pop()
        if scenario == "duplicate":
            hashes.append(hashes[-1])
        (dist / "SHA256SUMS.txt").write_text("\r\n".join(hashes) + "\r\n")
        harness = r'''
source "$INSTALLER"
ARCH=amd64
TMP_DIR="$FIXTURE/stage"
ACTION=upgrade
INSTALLER_COPY_PATH="$FIXTURE/saved.sh"
case "$SCENARIO" in
    explicit) RELEASE_VERSION=v1 ;;
    local)
        BINARY_SOURCE="$FIXTURE/dist/xboard-node-linux-amd64"
        CLI_BINARY_SOURCE="$FIXTURE/dist/xbctl-linux-amd64"
        RELEASE_VERSION=local-label ;;
esac
curl() {
    local last=${!#} out=""
    while (( $# )); do if [[ "$1" == -o ]]; then out=$2; shift; fi; shift; done
    case "$last" in
        "$EXPECTED/latest")
            echo query >> "$FIXTURE/lookups"
            printf '%s' "${last%latest}tag/v1";;
        "$EXPECTED/download/v1/"*) cp "$FIXTURE/dist/${last##*/}" "$out";;
        *) return 1;;
    esac
}
stage_release
if [[ -f "$TMP_DIR/install.sh" ]]; then replace_saved_installer "$TMP_DIR/install.sh"; fi
'''
        trace = root / "trace"
        env = {**os.environ, "INSTALLER":str(INSTALLER), "FIXTURE":temp, "TRACE":str(trace), "SCENARIO":scenario}
        env.pop("XBOARD_NODE_REPOSITORY", None)
        if repository:
            env["XBOARD_NODE_REPOSITORY"] = repository
        env["EXPECTED"] = "https://github.com/" + (repository or "Toto0912/xboard-node-u") + "/releases"
        p = subprocess.run(["bash", "-c", harness], env=env, capture_output=True)
        success = scenario in ("valid", "explicit", "local", "no-saved-installer", "saved-symlink")
        assert (p.returncode == 0) == success, (scenario, p.stdout, p.stderr)
        if scenario == "no-saved-installer":
            assert not saved.exists()
        else:
            assert saved.read_bytes() == (new_script if success and scenario != "local" else old_script)
            assert saved.is_symlink() == (scenario == "saved-symlink")
        if scenario in ("explicit", "local"):
            assert not (root / "lookups").exists()
        else:
            assert (root / "lookups").read_text().splitlines() == ["query"]
        ran = trace.read_text().splitlines() if trace.exists() else []
        assert ran == (list(names) if success or scenario == "wrong-version" else []), (scenario, ran)


for scenario in ("valid", "explicit", "local", "no-saved-installer", "saved-symlink", "damaged-node", "damaged-cli", "damaged-installer", "missing", "duplicate", "wrong-version"):
    staged_release(scenario)
staged_release("valid", "example/fork")

def maintenance_rollback(previous, candidate, resets, succeeds, reset_error=False):
    harness = r'''
source "$INSTALLER"
CURRENT_STATE=managed
SERVICE_PATH="$FIXTURE/unit"
systemctl() {
    case "$1" in
        show) cat "$FIXTURE/state" ;;
        is-enabled) return 1 ;;
        stop|daemon-reload) return 0 ;;
        reset-failed)
            echo reset >> "$FIXTURE/calls"
            [[ "$FAIL_RESET" == no ]] || return 1
            echo inactive > "$FIXTURE/state" ;;
        *) echo "unexpected systemctl $*" >&2; return 1 ;;
    esac
}
record_service_state
echo "$CANDIDATE" > "$FIXTURE/state"
rollback_install
'''
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        (root / "state").write_text(previous)
        (root / "calls").touch()
        env = {**os.environ, "INSTALLER": str(INSTALLER), "FIXTURE": tmp,
               "CANDIDATE": candidate, "FAIL_RESET": "yes" if reset_error else "no"}
        result = subprocess.run(["bash", "-c", harness], env=env, capture_output=True)
        assert (result.returncode == 0) == succeeds, (previous, candidate, result.stdout, result.stderr)
        assert (root / "calls").read_text().splitlines() == ["reset"] * resets
        if succeeds:
            assert (root / "state").read_text().strip() == previous


maintenance_rollback("inactive", "failed", 1, True)
maintenance_rollback("inactive", "inactive", 0, True)
maintenance_rollback("failed", "failed", 0, True)
maintenance_rollback("inactive", "failed", 1, False, reset_error=True)
maintenance_rollback("failed", "inactive", 0, False)

def legacy_rebind_during_stage(change):
    harness = r'''
source "$INSTALLER"
ACTION=upgrade
EXPECT_EMPTY=1
CONFIG_FILE="$FIXTURE/config.yml"
INSTALL_META="$FIXTURE/install-meta.json"
detect_current_state() { CURRENT_STATE=installed; }
lock_installation() { :; }
ensure_dirs() { :; }
stage_release() {
    cat > "$TMP_DIR/xbctl" <<'EOF'
#!/bin/sh
if [ "$2" = migrate-empty-v241 ]; then
    for arg do :; done
    echo 'instances: []' > "$arg"
fi
EOF
    chmod +x "$TMP_DIR/xbctl"
}
render_service() { :; }
record_service_state() { OLD_ACTIVE_STATE=inactive; }
load_deployment() { [[ "$1" != "$CONFIG_FILE" ]] || return 1; HEALTH_PORT=empty; }
backup_existing_state() {
    case "$CHANGE" in
        config) echo rebound > "$CONFIG_FILE";;
        metadata) echo rebound > "$INSTALL_META";;
        state) echo active > "$FIXTURE/state";;
    esac
}
systemctl() { [[ "$1" == show ]] && cat "$FIXTURE/state"; }
install() { touch "$FIXTURE/mutated"; return 1; }
rollback_install() { touch "$FIXTURE/rolled-back"; }
perform_upgrade
'''
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        for name in ("config.yml", "install-meta.json", "state"):
            (root / name).write_text("inactive\n" if name == "state" else "original\n")
        p = subprocess.run(["bash", "-c", harness], capture_output=True,
                           env={**os.environ, "INSTALLER": str(INSTALLER), "FIXTURE": tmp, "CHANGE": change})
        assert p.returncode and b"changed while preparing migration" in p.stdout, (p.stdout, p.stderr)
        assert not (root / "mutated").exists() and not (root / "rolled-back").exists()
        for name, changed_by in (("config.yml", "config"), ("install-meta.json", "metadata")):
            assert (root / name).read_text() == ("rebound\n" if change == changed_by else "original\n")


for change in ("config", "metadata", "state"):
    legacy_rebind_during_stage(change)

print("Installer tests passed: piped interactive machine/node/standalone setup, hidden tokens, input retries and repository selection.")
