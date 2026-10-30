#!/bin/sh
set -eu
# Run only inside a disposable alpine:3.22 container with --init.
# Install only OpenRC (the declared LXC precondition), not curl/Bash/Python.
apk add --no-cache openrc
if command -v curl >/dev/null 2>&1; then exit 1; fi
mkdir -p /run/openrc /etc/xboard-node
printf 'default\n' > /run/openrc/softlevel
printf '\nrc_sys="docker"\nrc_provide="net"\n' >> /etc/rc.conf
# Existing standalone config avoids any real or mock panel dependency here.
cat > /etc/xboard-node/config.yml <<'CONFIG'
kernel:
  type: xray
  config_dir: /var/lib/xboard-node
standalone:
  enabled: true
  node:
    protocol: vless
    listen_ip: 127.0.0.1
    server_port: 20447
    network: tcp
    tls: 0
  users:
    - id: 1
      uuid: 00000000-0000-4000-8000-000000000001
log:
  level: info
health_port: 0
CONFIG
chmod 600 /etc/xboard-node/config.yml
sh /src/install-alpine.sh upgrade --version v1.0.0 \
    --binary /release/xboard-node-linux-amd64 --xbctl-binary /release/xbctl-linux-amd64 --checksums /release/SHA256SUMS.txt
command -v curl
command -v logrotate
if command -v bash >/dev/null 2>&1; then exit 1; fi
if command -v python3 >/dev/null 2>&1; then exit 1; fi
xbctl service status
test -L /etc/runlevels/default/xboard-node
test -s /etc/logrotate.d/xboard-node
xbctl uninstall --yes
test -f /etc/xboard-node/config.yml
printf '%s\n' 'PASS: minimal Alpine/OpenRC installs missing dependencies without Bash/Python, starts release Xray and uninstalls cleanly'
