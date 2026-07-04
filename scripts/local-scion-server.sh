#!/usr/bin/env bash
set -eou pipefail

# Kill any existing scion server processes
echo "Checking for existing scion server processes..."
existing=$(ps aux 2>/dev/null | grep -i "scion server" | grep -v grep | awk '{print $2}' || true)
if [ -n "$existing" ]; then
  echo "Killing existing server processes: $existing"
  echo "$existing" | xargs kill 2>/dev/null || true
  sleep 2
fi

pushd $HOME/.scion

# Start the server detached (nohup + disown = survives after script exits)
nohup scion server start \
  --host 0.0.0.0 \
  --enable-hub \
  --enable-web --web-port 9810 \
  --enable-runtime-broker \
  > /tmp/scion-server.log 2>&1 &
disown

echo "Waiting for server to be ready on port 9810..."
MAX_ATTEMPTS=60
for i in $(seq 1 $MAX_ATTEMPTS); do
  if curl -s http://localhost:9810/healthz > /dev/null 2>&1; then
    echo "Server is ready on port 9810 (attempt $i)"
    popd
    exit 0
  fi
  if [ "$i" -eq "$MAX_ATTEMPTS" ]; then
    echo "ERROR: Server failed to start after $MAX_ATTEMPTS attempts"
    echo "Server logs:"
    cat /tmp/scion-server.log
    popd
    exit 1
  fi
  sleep 1
done

popd
