#!/bin/sh
# Runs as the harness under sciontool init, so it and its children inherit
# init's capability state (a `docker exec` process would not).
out=/tmp/netcap-probe
mkdir -p "$out"
grep '^CapBnd:' /proc/self/status | awk '{print $2}' > "$out/capbnd"
grep '^CapEff:' /proc/self/status | awk '{print $2}' > "$out/capeff"
grep '^CapPrm:' /proc/self/status | awk '{print $2}' > "$out/capprm"
id -u > "$out/uid"
{ sudo -n iptables -t nat -F; } > "$out/sudo-flush.log" 2>&1
echo $? > "$out/sudo-flush-rc"
touch "$out/done"
exec sleep 3600
