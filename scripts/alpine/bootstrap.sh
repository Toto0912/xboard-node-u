#!/bin/sh
set -eu
# Test container only: Docker supplies its network, and --init reaps child processes.
mkdir -p /run/openrc
printf 'default\n' > /run/openrc/softlevel
printf '\nrc_sys="docker"\nrc_provide="net"\n' >> /etc/rc.conf
sh -n /src/install-alpine.sh
shellcheck -s sh /src/install-alpine.sh
python3 "/src/scripts/alpine/${1:-integration.py}"
