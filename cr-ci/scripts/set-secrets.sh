#!/usr/bin/env bash
# set-secrets.sh: add new versions to the two cr-ci secrets (permissions-matrix
# C5 first time, C9 rotation). Values come ONLY from a file (mode 0600 or
# stricter) or stdin and are piped to `gcloud secrets versions add --data-file=-`.
# A value is never put in argv, printed, or written anywhere except the
# generated-secret file the operator names.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: set-secrets.sh [--app-key-file FILE] [--webhook-secret-file FILE | --generate-webhook-secret OUT_FILE]
                      [--config FILE] [--dry-run | --apply]

  --app-key-file FILE             GitHub App private key (PEM) -> GITHUB_APP_PRIVATE_KEY_SECRET_NAME
  --webhook-secret-file FILE      webhook secret -> GITHUB_WEBHOOK_SECRET_NAME; one trailing
                                  newline (LF or CRLF) is stripped, since the mediator uses the
                                  bytes as they are
  --generate-webhook-secret OUT   generate 32 random bytes as 64 hex chars (no newline), write
                                  them ONCE to OUT (must not exist; created 0600), and add them.
                                  Paste OUT into the App's webhook secret, then delete OUT.

FILE may be '-' for stdin (at most one of the two). A FILE readable by group
or others is refused (chmod 600 FILE). After rotating the App key, redeploy the
mediator (it caches the key); a new webhook secret is picked up without a restart.

$crci_common_help"

KEY_FILE=""
HOOK_FILE=""
GEN_FILE=""
while (($#)); do
  case $1 in
    --app-key-file)
      crci_need_value "$1" "$@"
      KEY_FILE=$2
      shift 2
      ;;
    --webhook-secret-file)
      crci_need_value "$1" "$@"
      HOOK_FILE=$2
      shift 2
      ;;
    --generate-webhook-secret)
      crci_need_value "$1" "$@"
      GEN_FILE=$2
      shift 2
      ;;
    *)
      crci_parse_common_arg "$@"
      shift "$CRCI_SHIFT"
      ;;
  esac
done
crci_help_exit "$USAGE"
[[ -n $KEY_FILE || -n $HOOK_FILE || -n $GEN_FILE ]] || crci_die "nothing to do; see --help"
[[ -n $HOOK_FILE && -n $GEN_FILE ]] && crci_die "--webhook-secret-file and --generate-webhook-secret are exclusive"
[[ $KEY_FILE == - && $HOOK_FILE == - ]] && crci_die "only one value can come from stdin"
[[ $GEN_FILE == - ]] && crci_die "--generate-webhook-secret needs a file name, not stdin"

# check_file FILE: exists, regular, not readable by group or others.
check_file() {
  local f=$1 mode
  [[ $f == - ]] && return 0
  [[ -f $f && -r $f ]] || crci_die "cannot read $f"
  mode=$(stat -c %a -- "$f" 2>/dev/null || stat -f %Lp -- "$f") || crci_die "cannot stat $f"
  ((8#$mode & 8#077)) && crci_die "$f is readable by group or others (mode $mode); run: chmod 600 $f"
  return 0
}
[[ -n $KEY_FILE ]] && check_file "$KEY_FILE"
[[ -n $HOOK_FILE ]] && check_file "$HOOK_FILE"
if [[ -n $GEN_FILE ]]; then
  [[ -e $GEN_FILE || -L $GEN_FILE ]] && crci_die "$GEN_FILE exists; choose a new file name"
  [[ -d $(dirname -- "$GEN_FILE") ]] || crci_die "directory of $GEN_FILE does not exist"
fi

# Paths as typed are shown; absolute ones are used (crci_start changes directory).
KEY_SHOW=$KEY_FILE HOOK_SHOW=$HOOK_FILE GEN_SHOW=$GEN_FILE
[[ -n $KEY_FILE && $KEY_FILE != - ]] && KEY_FILE=$(crci_abspath "$KEY_FILE")
[[ -n $HOOK_FILE && $HOOK_FILE != - ]] && HOOK_FILE=$(crci_abspath "$HOOK_FILE")
[[ -n $GEN_FILE ]] && GEN_FILE=$(crci_abspath "$GEN_FILE")

crci_start
P=${CFG[GCP_PROJECT]}

# add_version SECRET SOURCE_TEXT: prints the command with its input source
# (never the content); with --apply runs it with the caller's stdin.
add_version() {
  local secret=$1 src=$2
  printf '+ %s < %s\n' "$(crci_quote gcloud secrets versions add "$secret" --project="$P" --data-file=-)" "$src"
  if [[ $CRCI_MODE == apply ]]; then
    gcloud secrets versions add "$secret" --project="$P" --data-file=- --format=none ||
      crci_die "step failed: gcloud secrets versions add $secret"
  fi
}

# strip_one_newline: copies stdin to stdout without one trailing LF or CRLF.
strip_one_newline() {
  local data
  # Read all bytes; a sentinel keeps every trailing newline of the input.
  data=$(cat && printf x)
  data=${data%x}
  data=${data%$'\n'}
  data=${data%$'\r'}
  [[ -n $data ]] || crci_die "the webhook secret is empty"
  printf '%s' "$data"
}

if [[ -n $KEY_FILE ]]; then
  crci_note "C5/C9: App private key -> ${CFG[GITHUB_APP_PRIVATE_KEY_SECRET_NAME]}"
  if [[ $KEY_FILE != - ]]; then
    grep -q -- '-----BEGIN .*PRIVATE KEY-----' "$KEY_FILE" || crci_die "$KEY_SHOW does not look like a PEM private key"
    src=$(crci_quote "$KEY_SHOW")
    add_version "${CFG[GITHUB_APP_PRIVATE_KEY_SECRET_NAME]}" "$src" <"$KEY_FILE"
  else
    add_version "${CFG[GITHUB_APP_PRIVATE_KEY_SECRET_NAME]}" "(stdin)"
  fi
fi

if [[ -n $HOOK_FILE ]]; then
  crci_note "C5/C9: webhook secret -> ${CFG[GITHUB_WEBHOOK_SECRET_NAME]} (one trailing newline stripped)"
  if [[ $CRCI_MODE == apply ]]; then
    if [[ $HOOK_FILE == - ]]; then
      strip_one_newline | add_version "${CFG[GITHUB_WEBHOOK_SECRET_NAME]}" "(stdin)"
    else
      src=$(crci_quote "$HOOK_SHOW")
      strip_one_newline <"$HOOK_FILE" | add_version "${CFG[GITHUB_WEBHOOK_SECRET_NAME]}" "$src"
    fi
  else
    src="(stdin)"
    [[ $HOOK_FILE != - ]] && src=$(crci_quote "$HOOK_SHOW")
    add_version "${CFG[GITHUB_WEBHOOK_SECRET_NAME]}" "$src"
  fi
fi

if [[ -n $GEN_FILE ]]; then
  crci_note "C5/C9: generated webhook secret -> ${CFG[GITHUB_WEBHOOK_SECRET_NAME]}"
  if [[ $CRCI_MODE == apply ]]; then
    (
      umask 077
      set -o noclobber
      od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$GEN_FILE"
    ) || crci_die "cannot create $GEN_SHOW"
    [[ $(wc -c <"$GEN_FILE") -eq 64 ]] || crci_die "generated secret has the wrong length"
    crci_note "wrote 64 hex chars (no newline) to $GEN_SHOW, mode 0600"
  else
    crci_note "would write 32 random bytes as 64 hex chars (no newline) to $GEN_SHOW, mode 0600"
  fi
  if [[ $CRCI_MODE == apply ]]; then
    src=$(crci_quote "$GEN_SHOW")
    add_version "${CFG[GITHUB_WEBHOOK_SECRET_NAME]}" "$src" <"$GEN_FILE"
  else
    src=$(crci_quote "$GEN_SHOW")
    add_version "${CFG[GITHUB_WEBHOOK_SECRET_NAME]}" "$src" </dev/null
  fi
  crci_note "paste the content of $GEN_SHOW into the GitHub App's webhook secret, then delete $GEN_SHOW"
fi
crci_note "done"
