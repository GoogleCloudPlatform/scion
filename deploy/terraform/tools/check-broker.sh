#!/usr/bin/env bash
# Verifies that a Cloud Run hub revision's co-located broker registered
# successfully. Read-only: reads Cloud Logging, changes nothing.
#
# Checks, over the revision's own app log lines:
#   - no "invalid UUID" errors (the broker_id rendered into settings.yaml
#     must be a canonical UUID; the store rejects anything else)
#   - a "Registered global project with runtime broker" line
#   - an "Hub connection active" line with both control_channel=true and
#     heartbeat=true
#   - no "Skipping heartbeat"/"Skipping control channel"/"without HMAC keys"
#     lines (these mean the broker never came up)
#
# Usage:
#   check-broker.sh [-p|--project PROJECT] [-s|--service SERVICE] <revision-name>
#
# PROJECT and SERVICE can also come from the CHECK_BROKER_PROJECT and
# CHECK_BROKER_SERVICE environment variables. A flag overrides the
# environment variable. There is no default for either — every deployment
# names its own project and service, so guessing one would silently check
# the wrong hub.
#
# Exit codes: 0 all checks true | 1 a check failed | 2 cannot determine
# (bad usage, missing project/service/revision, or the logging read failed).
set -uo pipefail

PROG="$(basename "$0")"

usage() {
  cat >&2 <<EOF
Usage: $PROG [-p|--project PROJECT] [-s|--service SERVICE] <revision-name>

PROJECT and SERVICE may also be set via the CHECK_BROKER_PROJECT and
CHECK_BROKER_SERVICE environment variables (a flag overrides the
environment). Both, plus a revision name, are required.
EOF
}

PROJECT="${CHECK_BROKER_PROJECT:-}"
SERVICE="${CHECK_BROKER_SERVICE:-}"
REV=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -p|--project)
      PROJECT="${2:?--project needs a value}"; shift 2 ;;
    --project=*)
      PROJECT="${1#*=}"; shift ;;
    -s|--service)
      SERVICE="${2:?--service needs a value}"; shift 2 ;;
    --service=*)
      SERVICE="${1#*=}"; shift ;;
    -h|--help)
      usage; exit 2 ;;
    --)
      shift; break ;;
    -*)
      echo "$PROG: unknown option: $1" >&2; usage; exit 2 ;;
    *)
      break ;;
  esac
done
REV="${1:-}"

if [[ -z "$PROJECT" || -z "$SERVICE" || -z "$REV" ]]; then
  {
    [[ -z "$PROJECT" ]] && echo "$PROG: missing project (--project or CHECK_BROKER_PROJECT)"
    [[ -z "$SERVICE" ]] && echo "$PROG: missing service (--service or CHECK_BROKER_SERVICE)"
    [[ -z "$REV" ]] && echo "$PROG: missing revision name"
  } >&2
  usage
  exit 2
fi

J=$(mktemp)
trap 'rm -f "$J"' EXIT
gcloud logging read \
  "resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"${SERVICE}\" AND resource.labels.revision_name=\"${REV}\"" \
  --project="$PROJECT" --order=asc --limit=5000 --format=json > "$J" || exit 2
python3 - "$J" <<'PY'
import json,sys
L=json.load(open(sys.argv[1]))
app=[e for e in L if 'httpRequest' not in e]
def m(e):
    j=e.get('jsonPayload') or {}; return (j.get('message') or e.get('textPayload') or ''), j
print(f"control: {len(L)} entries, {len(app)} app lines for this revision")
if len(app)<10: print("CANNOT DETERMINE: too few app lines (boot not logged yet?)"); sys.exit(2)
bad=0
uu=[e for e in app if 'invalid UUID' in m(e)[0]]
print(("FAIL" if uu else "ok  "),f"'invalid UUID' lines: {len(uu)}"); bad|=bool(uu)
reg=[e for e in app if m(e)[0].startswith('Registered global project with runtime broker')]
print(("ok  " if reg else "FAIL"),f"registration lines: {len(reg)}", (m(reg[0])[0][:200] if reg else '')); bad|=not reg
hc=[m(e)[1] for e in app if m(e)[0]=='Hub connection active']
good=[j for j in hc if j.get('control_channel') is True and j.get('heartbeat') is True]
print(("ok  " if good else "FAIL"),f"'Hub connection active' lines: {len(hc)}, with control_channel=true heartbeat=true: {len(good)}"); bad|=not good
sk=[e for e in app if 'Skipping heartbeat' in m(e)[0] or 'Skipping control channel' in m(e)[0] or 'without HMAC keys' in m(e)[0]]
print(("FAIL" if sk else "ok  "),f"skip-heartbeat/no-HMAC lines: {len(sk)}"); bad|=bool(sk)
for e in app:
    t,j=m(e)
    if 'broker' in (t+json.dumps(j)).lower() and e.get('severity') in ('WARNING','ERROR'):
        print("  note:",e['timestamp'][11:23],e.get('severity'),t[:200])
sys.exit(1 if bad else 0)
PY
exit $?
