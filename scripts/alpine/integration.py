"""Integration tests: real Alpine ash, OpenRC, release binary and loopback panel.

Run inside a throwaway Alpine container; /src and /release are read-only mounts.
Never contacts a real panel, VPS, or proxy destination.
"""
import errno
import hashlib
import http.server
import json
import os
from pathlib import Path
import pty
import select
import signal
import socket
import struct
import subprocess
import threading
import time
import urllib.parse
import uuid

import yaml

SCRIPT = "/src/install-alpine.sh"
ROOT = Path("/etc/xboard-node")
CONFIG = ROOT / "config.yml"
BIN = Path("/usr/local/bin/xboard-node")
SERVER_PORT = 18745
PROXY_PORT = 20446
USER_UUID = "00000000-0000-4000-8000-000000000001"
TOKEN = 'local-test-$(`touch /tmp/must-not-exist`)-"quote"-\\-end'
EVENTS = []
RESULTS = []


class Panel(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass  # Do not log query strings/tokens.

    def do_POST(self):
        self.handle_api()

    def do_GET(self):
        self.handle_api()

    def handle_api(self):
        parsed = urllib.parse.urlsplit(self.path)
        path = parsed.path
        if path == "/target":
            payload = b"alpine-proxy-ok"
        else:
            query = {k: v[0] for k, v in urllib.parse.parse_qs(parsed.query).items()}
            data = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
            auth = data or query
            # Every node API must receive the actual literal token, not shell-expanded text.
            if auth.get("token") != TOKEN:
                self.send_error(403)
                return
            EVENTS.append((path, dict(auth)))
            if path.endswith("/machine/nodes"):
                body = {"nodes": [{"id": 11, "type": "vless", "name": "loopback"}],
                        "base_config": {"push_interval": 5, "pull_interval": 5}}
            elif path.endswith("/handshake"):
                body = {"websocket": {"enabled": False},
                        "settings": {"push_interval": 5, "pull_interval": 5}}
            elif path.endswith("/config"):
                body = {"protocol": "vless", "listen_ip": "127.0.0.1",
                        "server_port": 20448 if str(auth.get("node_id")) == "9" else PROXY_PORT, "network": "tcp", "tls": 0,
                        "custom_route_rules": [{"match": {"ip_cidrs": ["127.0.0.1/32"]},
                                                "action": {"type": "route", "target": "direct"}}],
                        "base_config": {"push_interval": 5, "pull_interval": 5}}
            elif path.endswith("/user"):
                body = {"users": [{"id": 1, "uuid": USER_UUID, "speed_limit": 0, "device_limit": 0}]}
            else:
                body = {"data": True}
            payload = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def run(*args, ok=True, timeout=180, env=None):
    p = subprocess.run(args, capture_output=True, text=True, timeout=timeout, env=env)
    if ok and p.returncode:
        raise AssertionError(f"{args}: {p.returncode}\n{p.stdout}\n{p.stderr}")
    if not ok:
        assert p.returncode != 0, f"Unexpected success: {args}"
    assert TOKEN not in p.stdout + p.stderr, "installer exposed token"
    return p


def record(name):
    RESULTS.append(name)
    print("PASS:", name, flush=True)


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def wait_for(test, timeout=20):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            result = test()
            if result:
                return result
        except (OSError, AssertionError):
            pass
        time.sleep(.2)
    raise AssertionError("Timed out waiting for condition")


def proxy_get():
    # VLESS v0 TCP -> local HTTP server. Both cores must transport real bytes.
    with socket.create_connection(("127.0.0.1", PROXY_PORT), timeout=3) as sock:
        request = (b"\x00" + uuid.UUID(USER_UUID).bytes + b"\x00\x01"
                   + struct.pack("!H", SERVER_PORT) + b"\x01\x7f\x00\x00\x01")
        sock.sendall(request + b"GET /target HTTP/1.0\r\nHost: localhost\r\n\r\n")
        response = b""
        while True:
            chunk = sock.recv(65536)
            if not chunk:
                break
            response += chunk
            # Xray may keep the transport open after the HTTP body is complete.
            if b"alpine-proxy-ok" in response:
                break
    assert response[:2] == b"\x00\x00", response[:80]
    assert b"200 OK" in response and b"alpine-proxy-ok" in response, response[:120]
    return True


def node_pids():
    found = []
    for item in Path("/proc").iterdir():
        if item.name.isdigit():
            try:
                cmdline = (item / "cmdline").read_bytes().split(b"\x00")
                if cmdline and cmdline[0] == b"/usr/local/bin/xboard-node":
                    found.append(int(item.name))
            except OSError:
                pass
    return found


OFFLINE = ["--binary", "/release/xboard-node-linux-amd64",
           "--xbctl-binary", "/release/xbctl-linux-amd64",
           "--checksums", "/release/SHA256SUMS.txt", "--version", "v1.0.0"]
Path("/tmp/test-token").write_text(TOKEN + "\n")
os.chmod("/tmp/test-token", 0o600)
PARAMS = ["--panel", f"http://127.0.0.1:{SERVER_PORT}", "--id", "008",
          "--token-file", "/tmp/test-token"]


def interactive_install():
    # A real controlling terminal checks both default choices and secret echo suppression.
    answers = [("部署模式", ""), ("选择内核", ""),
               ("面板地址", f"http://127.0.0.1:{SERVER_PORT}"),
               ("机器 ID", "008"), ("机器令牌", TOKEN)]
    child, terminal = pty.fork()
    if child == 0:
        os.execvp("sh", ["sh", SCRIPT, *OFFLINE])
    transcript = bytearray()
    pending = bytearray()
    index = 0
    status = None
    deadline = time.monotonic() + 120
    try:
        while time.monotonic() < deadline:
            if select.select([terminal], [], [], .1)[0]:
                try:
                    chunk = os.read(terminal, 16384)
                except OSError as e:
                    if e.errno == errno.EIO:
                        break
                    raise
                if not chunk:
                    break
                transcript.extend(chunk)
                pending.extend(chunk)
            if index < len(answers):
                marker, answer = answers[index]
                encoded = marker.encode()
                if encoded in pending:
                    pending = pending[pending.index(encoded) + len(encoded):]
                    # Ensure the terminal's echo setting has been applied after prompt.
                    time.sleep(.1)
                    os.write(terminal, (answer + "\n").encode())
                    index += 1
            exited, exit_status = os.waitpid(child, os.WNOHANG)
            if exited:
                status = exit_status
                break
        else:
            raise AssertionError("interactive install timeout")
        if status is None:
            _, status = os.waitpid(child, 0)
        text = transcript.decode(errors="replace")
        assert os.waitstatus_to_exitcode(status) == 0, text
        assert index == len(answers), text
        assert TOKEN not in text, "secret echoed in terminal"
        assert "安装完成" in text, text
    finally:
        if status is None:
            os.kill(child, signal.SIGKILL)
            os.waitpid(child, 0)
        os.close(terminal)


server = http.server.ThreadingHTTPServer(("127.0.0.1", SERVER_PORT), Panel)
threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    run("sh", SCRIPT, "--help")
    run("sh", SCRIPT, "--mode", ok=False)
    run("sh", SCRIPT, "--mode", "invalid", *PARAMS, *OFFLINE, ok=False)
    assert not BIN.exists()
    record("ash syntax, ShellCheck, help and invalid-input rejection")

    # Corrupt checksum must fail before any deployment is touched.
    Path("/tmp/bad.sha256").write_text("0" * 64 + "  xboard-node-linux-amd64\n" + digest('/release/xbctl-linux-amd64') + "  xbctl-linux-amd64\n")
    run("sh", SCRIPT, "--mode", "machine", *PARAMS,
        "--binary", "/release/xboard-node-linux-amd64", "--xbctl-binary", "/release/xbctl-linux-amd64", "--checksums", "/tmp/bad.sha256", ok=False)
    assert not BIN.exists() and not CONFIG.exists()
    record("checksum failure leaves no installed binary/config")

    interactive_install()
    conf = yaml.safe_load(CONFIG.read_text())
    assert conf["machine"] == {"machine_id": 8, "token": TOKEN}
    assert conf["kernel"]["type"] == "singbox"
    assert CONFIG.stat().st_mode & 0o777 == 0o600
    assert ROOT.stat().st_mode & 0o777 == 0o700
    assert Path("/etc/runlevels/default/xboard-node").is_symlink()
    assert Path("/usr/local/bin/xbctl").is_file()
    assert Path("/etc/xboard-node/install-alpine.sh").is_file()
    assert not Path("/usr/local/sbin/xb-alpine").exists()
    assert run("xbctl", "service", "backend").stdout.strip() == "openrc"
    assert json.loads(run("xbctl", "list", "--output", "json").stdout)[0]["service"] == "active"
    run("xbctl", "service", "status")
    run("xbctl", "service", "logs", "-n", "2", "--no-pager")
    run("xbctl", "disable")
    assert not Path("/etc/runlevels/default/xboard-node").exists()
    run("xbctl", "enable")
    wait_for(proxy_get)
    assert any(p.endswith("/machine/nodes") and a.get("machine_id") == 8 for p, a in EVENTS)
    assert any(p == "/api/v2/server/config" for p, _ in EVENTS)
    assert not Path("/tmp/must-not-exist").exists()
    record("interactive machine/sing-box install, secret-safe YAML, permissions, proxy HTTP 200")

    # Existing standalone Alpine installs used a shell manager, without unified xbctl.
    old_manager = Path('/usr/local/sbin/xb-alpine')
    old_manager.parent.mkdir(parents=True, exist_ok=True)
    old_manager.write_text('#!/bin/sh\nexit 0\n')
    old_manager.chmod(0o755)
    Path('/usr/local/bin/xbctl').unlink()
    preserved = CONFIG.read_bytes()
    run('sh', SCRIPT, *OFFLINE)
    assert CONFIG.read_bytes() == preserved and not old_manager.exists()
    run('xbctl', 'service', 'status')
    wait_for(proxy_get)
    record('running standalone Alpine install migrates from xb-alpine to xbctl without rebinding')

    # A v1.0.0-style CLI lacking OpenRC must be rejected before stopping the node.
    old_cli = Path('/tmp/old-xbctl')
    old_cli.write_text('#!/bin/sh\nif [ "${1:-}" = version ]; then echo "xbctl old"; exit 0; fi\nexit 1\n')
    old_cli.chmod(0o755)
    Path('/tmp/old-cli.sha256').write_text(digest('/release/xboard-node-linux-amd64') + '  xboard-node-linux-amd64\n' + digest(old_cli) + '  xbctl-linux-amd64\n')
    previous_pids = node_pids()
    run('sh', SCRIPT, '--binary', '/release/xboard-node-linux-amd64', '--xbctl-binary', str(old_cli), '--checksums', '/tmp/old-cli.sha256', ok=False)
    assert node_pids() == previous_pids and CONFIG.read_bytes() == preserved
    wait_for(proxy_get)
    record('old incompatible xbctl is rejected before stopping existing service')

    # Actual process crash followed by OpenRC respawn, not a mocked rc-service.
    old_pid = node_pids()
    assert len(old_pid) == 1
    os.kill(old_pid[0], signal.SIGKILL)
    new_pid = wait_for(lambda: node_pids() if node_pids() and node_pids() != old_pid else None)
    assert len(new_pid) == 1
    wait_for(proxy_get)
    record("OpenRC restarts killed node and proxy service recovers")

    before = CONFIG.read_bytes()
    run("xbctl", "upgrade", *OFFLINE)
    assert CONFIG.read_bytes() == before
    wait_for(proxy_get)
    run("xbctl", "status")
    record("installed manager upgrades while preserving config and restores working proxy")

    # Explicit reconfigure, both kernels in both panel modes.
    for mode, kernel in [("node", "xray"), ("machine", "xray"), ("node", "singbox")]:
        event_start = len(EVENTS)
        run("sh", SCRIPT, "--reconfigure", "--mode", mode, "--kernel", kernel, *PARAMS, *OFFLINE)
        conf = yaml.safe_load(CONFIG.read_text())
        assert conf["kernel"]["type"] == kernel
        assert ("machine" in conf) == (mode == "machine")
        if kernel == "xray":
            # Raw local test override: recent freedom defaults also block private IPs.
            # Never included in the installer or generated production config.
            conf["kernel"]["custom_outbound"] = [{"tag": "direct", "protocol": "freedom",
                "settings": {"finalRules": [{"action": "allow", "ip": ["127.0.0.1/32"]}]}}]
            CONFIG.write_text(yaml.safe_dump(conf, allow_unicode=True))
            run("xbctl", "restart")
        wait_for(proxy_get)
        assert any(p == ("/api/v2/server/config" if mode == "machine" else "/api/v1/server/UniProxy/config")
                   for p, _ in EVENTS[event_start:])
        assert not Path("/tmp/must-not-exist").exists()
        record(f"{mode}/{kernel}: panel auth, config/users and actual proxy HTTP 200")

    # Existing config should never be silently rebound by an install invocation.
    original = CONFIG.read_bytes()
    run("sh", SCRIPT, "--mode", "machine", *PARAMS, *OFFLINE, ok=False)
    assert CONFIG.read_bytes() == original
    record("existing binding protected unless --reconfigure is explicit")

    # Existing credentials use literal export, never shell source/eval.
    CONFIG.write_text(original.decode().replace('token: "' + TOKEN.replace('\\', '\\\\').replace('"', '\\"') + '"',
                                               'token_env: ALPINE_TEST_TOKEN'))
    creds = ROOT / "credentials.env"
    creds.write_text("ALPINE_TEST_TOKEN=" + TOKEN + "\n")
    os.chmod(creds, 0o600)
    run("xbctl", "restart")
    wait_for(proxy_get)
    assert not Path("/tmp/must-not-exist").exists()
    record("legacy credentials.env loaded literally without evaluating token punctuation")

    # Binding writes and service restarts must work with the same public xbctl API.
    run("xbctl", "bind", "add-node", "--panel-url", f"http://127.0.0.1:{SERVER_PORT}",
        "--token", TOKEN, "--node-id", "9", "--kernel", "singbox")
    assert len(json.loads(run("xbctl", "list", "--output", "json").stdout)) == 2
    wait_for(proxy_get)
    run("xbctl", "bind", "remove-node", "--panel", f"http://127.0.0.1:{SERVER_PORT}", "--node-id", "9")
    assert len(json.loads(run("xbctl", "list", "--output", "json").stdout)) == 1
    wait_for(proxy_get)
    record("xbctl bindings preserve existing credentials and restart OpenRC services")

    # A binary that passes -v but dies on startup exercises transactional rollback.
    fake = Path("/tmp/bad-node")
    fake.write_text('#!/bin/sh\nif [ "${1:-}" = -v ]; then echo "xboard-node broken-test"; exit 0; fi\nexit 42\n')
    fake.chmod(0o755)
    Path("/tmp/fake.sha256").write_text(digest(fake) + "  xboard-node-linux-amd64\n" + digest('/release/xbctl-linux-amd64') + "  xbctl-linux-amd64\n")
    before_cfg = CONFIG.read_bytes()
    before_hash = digest(BIN)
    before_creds = creds.read_bytes()
    run("xbctl", "upgrade", "--binary", str(fake), "--xbctl-binary", "/release/xbctl-linux-amd64", "--checksums", "/tmp/fake.sha256", ok=False)
    assert CONFIG.read_bytes() == before_cfg and digest(BIN) == before_hash
    assert creds.read_bytes() == before_creds
    assert Path("/etc/runlevels/default/xboard-node").is_symlink()
    wait_for(proxy_get)
    record("failed upgrade rolls back binary/config/credentials and running service")

    run("xbctl", "stop")
    wait_for(lambda: not node_pids())
    run("xbctl", "start")
    wait_for(proxy_get)
    record("stop/start uses OpenRC and leaves no duplicate node processes")

    # Boot runlevel re-entry: service must start through the default runlevel.
    run("xbctl", "stop")
    run("openrc", "default")
    wait_for(proxy_get)
    record("OpenRC default runlevel starts enabled service (container boot simulation)")

    # Upgrading a stopped service and failing must preserve the stopped state.
    run("xbctl", "stop")
    run("xbctl", "upgrade", "--binary", str(fake), "--xbctl-binary", "/release/xbctl-linux-amd64", "--checksums", "/tmp/fake.sha256", ok=False)
    assert not node_pids() and CONFIG.read_bytes() == before_cfg and digest(BIN) == before_hash
    run("xbctl", "start")
    wait_for(proxy_get)
    record("failed upgrade of stopped deployment preserves its stopped state")

    # CI validates the newly built pair before it is published as a Release.
    run("xbctl", "upgrade", *OFFLINE, timeout=600)
    assert CONFIG.read_bytes() == before_cfg
    wait_for(proxy_get)
    record("xbctl upgrade installs the checksummed node/CLI pair and refreshes metadata")

    cancelled = run("xbctl", "uninstall")
    assert "Uninstall cancelled" in cancelled.stdout and BIN.exists()
    run("xbctl", "uninstall", "--yes")
    assert CONFIG.read_bytes() == before_cfg and creds.read_bytes() == before_creds
    assert not BIN.exists() and not Path("/etc/init.d/xboard-node").exists()
    assert not Path("/etc/runlevels/default/xboard-node").exists()
    assert not node_pids() and not Path("/run/xboard-node-alpine-install.lock").exists()
    record("confirmed uninstall removes service/program and preserves config, credentials, backups")

    run('sh', SCRIPT, *OFFLINE)
    wait_for(proxy_get)
    run('xbctl', 'bind', 'remove-node', '--panel', f'http://127.0.0.1:{SERVER_PORT}', '--node-id', '8')
    wait_for(lambda: not node_pids())
    record('removing final binding stops OpenRC node service')
    run('xbctl', 'uninstall', '--purge', '--yes')
    assert not ROOT.exists() and not BIN.exists() and not Path('/usr/local/bin/xbctl').exists()
    record('xbctl uninstall --purge removes configuration only when explicitly requested')

    reports = sum(1 for p, _ in EVENTS if p.endswith(("/report", "/push", "/status")))
    assert reports > 0
    summary = {"passed": RESULTS, "alpine": Path("/etc/alpine-release").read_text().strip(),
               "release_binary_sha256": digest("/release/xboard-node-linux-amd64"),
               "report_requests": reports, "scope": "local disposable Docker/OpenRC; loopback mocked panel; amd64"}
    Path("/tmp/alpine-test-results.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2))
    print(f"ALL {len(RESULTS)} CHECKS PASSED; local report requests: {reports}", flush=True)
finally:
    subprocess.run(["rc-service", "xboard-node", "stop"], stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL, timeout=45)
    server.shutdown()
