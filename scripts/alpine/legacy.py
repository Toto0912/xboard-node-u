"""Regression for uninstalled watchdogs leaving files, boot entries and stale PIDs.
Only run in a disposable Alpine/OpenRC test container.
"""
from pathlib import Path

# Reuse the loopback panel/proxy probe without executing the main integration suite.
exec(Path('/src/scripts/alpine/integration.py').read_text().split('server = http.server.ThreadingHTTPServer')[0])

ROOT.mkdir(mode=0o700, parents=True, exist_ok=True)
CONFIG.write_text(yaml.safe_dump({
    'panel': {'url': f'http://127.0.0.1:{SERVER_PORT}'},
    'machine': {'machine_id': 8, 'token_env': 'ALPINE_TEST_TOKEN'},
    'kernel': {'type': 'singbox', 'config_dir': '/etc/xboard-node/data'},
    'log': {'level': 'info', 'output': 'stdout'}, 'health_port': 0}))
CONFIG.chmod(0o600)
creds = ROOT / 'credentials.env'
creds.write_text('ALPINE_TEST_TOKEN=' + TOKEN + '\n')
creds.chmod(0o600)
watch = ROOT / 'xboard-node-run.sh'
watch_text = '#!/bin/sh\n# Simulated old watchdog; no network/listener.\nwhile :; do sleep 1; done\n'
oldpid = ROOT / 'xboard-node.pid'
rc = Path('/etc/rc.local')
bootline = 'nohup setsid /etc/xboard-node/xboard-node-run.sh >>/etc/xboard-node/xboard-node.log 2>&1 & echo $! > /etc/xboard-node/xboard-node.pid # xboard-node-autostart\n'
original_rc = '#!/bin/sh\nprintf unrelated-boot\n' + bootline + 'exit 0\n'
original_cron = '# unrelated tasks\n17 3 * * * echo keep-me\n@reboot ' + bootline
expected_rc = original_rc.replace(bootline, '')
expected_cron = original_cron.replace('@reboot ' + bootline, '')
before_cfg = CONFIG.read_bytes()
before_creds = creds.read_bytes()

def create_residue():
    watch.write_text(watch_text)
    watch.chmod(0o700)
    # A stale PID now points to this unrelated live Python process.
    oldpid.write_text(str(os.getpid()) + '\n')
    rc.write_text(original_rc)
    rc.chmod(0o755)
    run('crontab', '-', input_text=original_cron)

# Extend the run helper just for stdin-backed crontab setup.
previous_run = run
def run(*args, input_text=None, **kwargs):
    if input_text is None:
        return previous_run(*args, **kwargs)
    p = subprocess.run(args, input=input_text, text=True, capture_output=True, timeout=30)
    assert p.returncode == 0, p.stderr
    return p

server = http.server.ThreadingHTTPServer(('127.0.0.1', SERVER_PORT), Panel)
threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    create_residue()
    # A genuinely running watchdog must still block, without being killed.
    active = subprocess.Popen(['sh', str(watch)], start_new_session=True)
    try:
        oldpid.write_text(str(active.pid) + '\n')
        time.sleep(.2)
        result = run('sh', SCRIPT, *OFFLINE, ok=False)
        assert '旧看门狗进程仍在运行' in result.stderr
        assert active.poll() is None and watch.exists()
        assert CONFIG.read_bytes() == before_cfg and rc.read_text() == original_rc
        record('active legacy watchdog blocks migration without stopping or altering it')
    finally:
        os.killpg(active.pid, signal.SIGTERM)
        active.wait(timeout=5)

    # Fault injection: residue/boot entries must be restored if new node fails.
    create_residue()
    fake = Path('/tmp/legacy-bad-node')
    fake.write_text('#!/bin/sh\nif [ "${1:-}" = -v ]; then echo "xboard-node failed-test"; exit 0; fi\nexit 42\n')
    fake.chmod(0o755)
    Path('/tmp/legacy-bad.sha256').write_text(digest(fake) + '  xboard-node-linux-amd64\n' + digest('/release/xbctl-linux-amd64') + '  xbctl-linux-amd64\n')
    run('sh', SCRIPT, '--binary', str(fake), '--xbctl-binary', '/release/xbctl-linux-amd64', '--checksums', '/tmp/legacy-bad.sha256', ok=False)
    assert watch.read_text() == watch_text
    assert oldpid.read_text().strip() == str(os.getpid())
    assert rc.read_text() == original_rc
    assert run('crontab', '-l').stdout == original_cron
    assert CONFIG.read_bytes() == before_cfg and creds.read_bytes() == before_creds
    assert not BIN.exists()
    record('failed residue migration restores boot entries, stale files, configuration and credentials')

    # Screenshot case: uninstall completed, but old files/stale PID remain.
    result = run('sh', SCRIPT, *OFFLINE)
    assert '已备份并清理残留' in result.stdout
    assert CONFIG.read_bytes() == before_cfg and creds.read_bytes() == before_creds
    assert not watch.exists() and not oldpid.exists()
    assert rc.read_text() == expected_rc
    assert rc.stat().st_mode & 0o777 == 0o755
    assert run('crontab', '-l').stdout == expected_cron
    backups = sorted((ROOT / 'backups').iterdir())
    assert any((p / 'etc/xboard-node/xboard-node-run.sh').exists() for p in backups)
    wait_for(proxy_get)
    assert len(node_pids()) == 1
    run('xbctl', 'status')
    record('uninstalled watchdog residue migrates automatically; reused unrelated PID is never signalled')
    record('only matching old autostart lines removed; unrelated rc.local and cron tasks preserved')

    # Installing again must not re-create residue or alter unrelated boot files.
    run('xbctl', 'upgrade', *OFFLINE)
    wait_for(proxy_get)
    assert rc.read_text() == expected_rc and run('crontab', '-l').stdout == expected_cron
    assert CONFIG.read_bytes() == before_cfg and creds.read_bytes() == before_creds
    record('subsequent OpenRC upgrade remains repeatable after migration')

    run('xbctl', 'uninstall', '--yes')
    Path('/tmp/alpine-legacy-results.json').write_text(json.dumps({'passed': RESULTS,
        'alpine': Path('/etc/alpine-release').read_text().strip(), 'scope': 'local disposable Alpine/OpenRC amd64'},
        ensure_ascii=False, indent=2))
    print(f'ALL {len(RESULTS)} LEGACY CHECKS PASSED', flush=True)
finally:
    subprocess.run(['rc-service', 'xboard-node', 'stop'], stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL, timeout=45)
    server.shutdown()
