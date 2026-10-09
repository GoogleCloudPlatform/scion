#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# wallclock.sh -- wall-clock budget run for the agent list, end to end:
# build, seed 100 agents, start an isolated local hub, run the API and
# browser benchmarks, and compare their medians with a stored baseline
# (web/e2e-perf/median-ratio.mjs). See perf/bench/README.md, "Wall-clock
# budgets".
#
# Run it only on a quiet, dedicated runner booked for the run. It is never
# a required check on shared CI hosts: wall-clock times depend on the host.
#
# Usage (from the repo root):
#   perf/bench/wallclock.sh --baseline perf/bench/wallclock-baseline.json
#   perf/bench/wallclock.sh --write-baseline perf/bench/wallclock-baseline.json \
#     --runner "<runner description, no host names>"
#
# Options:
#   --workdir DIR   scratch directory (default: a new mktemp -d dir). Must be
#                   outside the repo checkout. Kept after the run.
#   --skip-build    reuse the binaries and web build from a previous run in
#                   the same --workdir and checkout.
#   --port N        hub web/API port on 127.0.0.1 (default 18080).
#   --api-runs N, --api-warmup N, --browser-runs N, --min-trials N
#                   override the trial counts (defaults 15, 2, 11, 10). Only
#                   for a setup smoke run; a baseline or a check must use the
#                   defaults.
#
# Exit status: 0 within budget (or baseline written), 1 over budget or too
# few trials, 2 usage or setup error.

set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
AGENTS=100
API_RUNS=15
API_WARMUP=2
BROWSER_RUNS=11 # 1 cold (the warm-up, not counted) + 10 warm
SECRET=wallclock-bench-secret
MODE=""
BASELINE=""
RUNNER=""
WORKDIR=""
SKIP_BUILD=0
PORT=18080
MIN_TRIALS=""

die() { echo "wallclock: $*" >&2; exit 2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --baseline) MODE="check"; BASELINE=$2; shift 2 ;;
    --write-baseline) MODE="write"; BASELINE=$2; shift 2 ;;
    --runner) RUNNER=$2; shift 2 ;;
    --workdir) WORKDIR=$2; shift 2 ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --port) PORT=$2; shift 2 ;;
    --api-runs) API_RUNS=$2; shift 2 ;;
    --api-warmup) API_WARMUP=$2; shift 2 ;;
    --browser-runs) BROWSER_RUNS=$2; shift 2 ;;
    --min-trials) MIN_TRIALS=$2; shift 2 ;;
    -h|--help) awk '/^# wallclock.sh/{p=1} p&&!/^#/{exit} p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[ -n "$MODE" ] || die "give --baseline FILE or --write-baseline FILE"
BASELINE=$(realpath -m -- "$BASELINE")
[ "$MODE" = write ] || [ -f "$BASELINE" ] || die "baseline not found: $BASELINE"

for tool in go node npm curl git; do
  command -v "$tool" >/dev/null || die "missing prerequisite: $tool"
done
node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 20 ? 0 : 1)' ||
  die "node 20 or newer is required"

WORKDIR=${WORKDIR:-$(mktemp -d /tmp/scion-wallclock.XXXXXX)}
mkdir -p "$WORKDIR"
WORKDIR=$(cd "$WORKDIR" && pwd)
case "$WORKDIR/" in "$REPO"/*) die "--workdir must be outside the repo checkout" ;; esac
BIN=$WORKDIR/bin
DB=$WORKDIR/hub.db
SEED=$WORKDIR/seed-$AGENTS.json
API_OUT=$WORKDIR/api-$AGENTS.json
BROWSER_OUT=$WORKDIR/browser-$AGENTS.json
HUB_LOG=$WORKDIR/hub.log
echo "wallclock: workdir $WORKDIR"

step() { echo "== $(date -u +%H:%M:%SZ) $*"; }

if [ "$SKIP_BUILD" = 0 ]; then
  step "build hub, seed and apibench"
  mkdir -p "$BIN"
  (cd "$REPO" && go build -o "$BIN/scion" ./cmd/scion/)
  # seed and apibench record their own commit from Go's VCS stamp; fall
  # back to an unstamped build where the checkout cannot be stamped.
  for t in seed apibench; do
    (cd "$REPO" && go build -o "$BIN/$t" "./perf/bench/$t/") ||
      (cd "$REPO" && go build -buildvcs=false -o "$BIN/$t" "./perf/bench/$t/")
  done
  step "build web assets and install Playwright Chromium"
  (cd "$REPO/web" && npm ci --no-audit --no-fund && npm run build && npx playwright install chromium)
fi

step "seed $AGENTS agents"
rm -f "$DB" "$SEED"
"$BIN/seed" --db "$DB" --session-secret "$SECRET" --agents "$AGENTS" \
  --project-slug "bench-$AGENTS" --out "$SEED"

step "start the hub on 127.0.0.1:$PORT (isolated environment)"
mkdir -p "$WORKDIR/home"
(
  cd "$WORKDIR"
  exec env -i HOME="$WORKDIR/home" PATH="$PATH" SCION_CLI_MODE=human \
    GCE_METADATA_HOST=127.0.0.1:1 GCE_METADATA_IP=127.0.0.1:1 \
    "$BIN/scion" server start --hosted --enable-hub --enable-web --enable-test-login \
    --db "$DB" --session-secret "$SECRET" \
    --port $((PORT + 1)) --web-port "$PORT" --host 127.0.0.1 \
    --web-assets-dir "$REPO/web/dist/client" \
    --foreground --no-auto-migrate
) >"$HUB_LOG" 2>&1 &
HUB_PID=$!
trap 'kill "$HUB_PID" 2>/dev/null || true; wait "$HUB_PID" 2>/dev/null || true' EXIT
for _ in $(seq 1 120); do
  curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  kill -0 "$HUB_PID" 2>/dev/null || die "hub exited during startup; see $HUB_LOG"
  sleep 1
done
curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null || die "hub not healthy after 120 s; see $HUB_LOG"

step "API benchmark: $API_RUNS timed runs after $API_WARMUP warm-up requests per scenario"
"$BIN/apibench" --hub "http://127.0.0.1:$PORT" --seed "$SEED" \
  --runs "$API_RUNS" --warmup "$API_WARMUP" --timeout-seconds 120 \
  --notes "wallclock.sh ${RUNNER:-}" --out "$API_OUT"

step "browser benchmark: $BROWSER_RUNS runs per scenario (the first, cold run is the warm-up)"
(cd "$REPO/web" && node e2e-perf/large-project-bench.mjs --hub "http://127.0.0.1:$PORT" \
  --seed "$SEED" --runs "$BROWSER_RUNS" --page-changes 0 --burst-runs 0 --out "$BROWSER_OUT")

kill "$HUB_PID" 2>/dev/null || true

step "median ratio"
cd "$REPO/web"
MR_OPTS=()
[ -z "$MIN_TRIALS" ] || MR_OPTS+=(--min-trials "$MIN_TRIALS")
if [ "$MODE" = write ]; then
  node e2e-perf/median-ratio.mjs "${MR_OPTS[@]}" --write-baseline "$BASELINE" --api "$API_OUT" --browser "$BROWSER_OUT" \
    --commit "$(git -C "$REPO" rev-parse --short HEAD)" --runner "$RUNNER"
else
  set +e
  node e2e-perf/median-ratio.mjs "${MR_OPTS[@]}" --baseline "$BASELINE" --api "$API_OUT" --browser "$BROWSER_OUT" |
    tee "$WORKDIR/median-ratio.txt"
  rc=${PIPESTATUS[0]}
  set -e
  echo "wallclock: reports in $WORKDIR"
  exit "$rc"
fi
echo "wallclock: reports in $WORKDIR"
