"""Exercise the actual piped Bash installer using a PTY, without installing anything."""

import errno
import os
import pty
import select
import signal
import subprocess
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

for override, expected_repo in [(None, "Toto0912/xboard-node-u"), ("example/fork", "example/fork")]:
    env = os.environ.copy()
    env.pop("XBOARD_NODE_REPOSITORY", None)
    if override:
        env["XBOARD_NODE_REPOSITORY"] = override
    result = subprocess.run(
        ["bash", "-c", 'source "$1"; parse_args --mode node --panel https://panel.example.com --token test --node-id 1; validate_install_request; resolve_download_url xbctl-linux-amd64; printf "%s" "$DOWNLOAD_URL"', "bash", str(INSTALLER)],
        check=True, capture_output=True, text=True, env=env,
    )
    assert result.stdout == f"https://github.com/{expected_repo}/releases/latest/download/xbctl-linux-amd64"

print("Installer tests passed: piped interactive machine/node setup, hidden tokens, input retries and repository selection.")
