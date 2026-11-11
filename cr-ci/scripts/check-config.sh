#!/usr/bin/env bash
# check-config.sh: validate config.env with the same rules as the mediator
# (mediator/config.go) and print the resolved values (names only; the config
# never holds secret values). Read-only; touches nothing.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: check-config.sh [--config FILE]

Validates the config and prints every key with its resolved value.

$crci_common_help"

while (($#)); do
  crci_parse_common_arg "$@"
  shift "$CRCI_SHIFT"
done
crci_help_exit "$USAGE"
crci_load_config "$CRCI_CONFIG"
for k in "${CRCI_KEYS[@]}"; do
  printf '%s=%s\n' "$k" "${CFG[$k]}"
done
