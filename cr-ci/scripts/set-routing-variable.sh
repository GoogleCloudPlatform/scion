#!/usr/bin/env bash
# set-routing-variable.sh: set or clear the GitHub Actions variable that routes
# CI jobs to cr-ci runners (ROUTING_VARIABLE_NAME, default SCION_LARGE_RUNNER;
# permissions-matrix G10 set, G11 fallback). gh only; no GCP access. Needs
# write access to each repository, not admin.
#
# Setting it is the rollout switch; clearing it is the fallback toggle (newly
# queued jobs go back to ubuntu-latest). See docs/github-setup.md, step 8.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

USAGE="Usage: set-routing-variable.sh [--repo OWNER/REPO]... [--clear | --value LABEL]
                              [--config FILE] [--dry-run | --apply]

  --repo OWNER/REPO   repository to act on; repeat for several. Default: every
                      repository in TARGET_REPOS. To set the variable, the repo
                      must be in TARGET_REPOS; --clear accepts any repo, so the
                      fallback still works after a repo left TARGET_REPOS.
  --clear             delete the variable (fallback to ubuntu-latest). A repo
                      where it is not set is left alone.
  --value LABEL       value to set instead of RUNNER_LABEL, for example a larger
                      GitHub-hosted runner label as the fallback target.

Set the variable only after docs/github-setup.md steps 4-7 hold for the repo
(App installed, webhook checked, fork PR approval for ALL external
contributors, PoC passed), and after ci.yml carries the routing expression.

$crci_common_help"

REPOS=()
CLEAR=0
VALUE=""
while (($#)); do
  case $1 in
    --repo)
      crci_need_value "$1" "$@"
      REPOS+=("$2")
      shift 2
      ;;
    --clear)
      CLEAR=1
      shift
      ;;
    --value)
      crci_need_value "$1" "$@"
      VALUE=$2
      shift 2
      ;;
    *)
      crci_parse_common_arg "$@"
      shift "$CRCI_SHIFT"
      ;;
  esac
done
crci_help_exit "$USAGE"
((CLEAR == 1)) && [[ -n $VALUE ]] && crci_die "--clear and --value are exclusive"

crci_load_config "$CRCI_CONFIG"
[[ $CRCI_MODE == apply ]] && crci_require_tool gh "$CRCI_MIN_GH"

NAME=${CFG[ROUTING_VARIABLE_NAME]}
[[ -n $VALUE ]] || VALUE=${CFG[RUNNER_LABEL]}
# Same rule as RUNNER_LABEL in mediator/config.go: one label, no spaces or
# commas (runs-on would split them), at most 64 characters.
[[ $VALUE =~ ^[A-Za-z0-9._-]{1,64}$ ]] || crci_die "--value '$VALUE' is not a single runner label"

# Target repos from config (comma-separated, spaces allowed around entries).
declare -A TARGET=()
IFS=',' read -r -a _targets <<<"${CFG[TARGET_REPOS]}"
for t in "${_targets[@]}"; do
  t=${t//[[:space:]]/}
  [[ -n $t ]] && TARGET[${t,,}]=$t
done
if ((${#REPOS[@]} == 0)); then
  for t in "${_targets[@]}"; do
    t=${t//[[:space:]]/}
    [[ -n $t ]] && REPOS+=("$t")
  done
fi

for r in "${REPOS[@]}"; do
  [[ $r =~ ^[A-Za-z0-9](-?[A-Za-z0-9])*/[A-Za-z0-9._-]+$ && ${#r} -le 140 ]] ||
    crci_die "--repo '$r' is not OWNER/REPO"
  if ((CLEAR == 0)) && [[ -z ${TARGET[${r,,}]+x} ]]; then
    crci_die "$r is not in TARGET_REPOS (${CFG[TARGET_REPOS]}); the mediator would reject its jobs, so they would never get a runner"
  fi
done

if ((CLEAR == 1)); then
  crci_note "cr-ci $CRCI_PROG ($CRCI_MODE): clear $NAME on ${REPOS[*]} (fallback: new jobs go to ubuntu-latest)"
else
  crci_note "cr-ci $CRCI_PROG ($CRCI_MODE): set $NAME=$VALUE on ${REPOS[*]}"
  crci_note "only after docs/github-setup.md steps 4-7 hold for each repo (fork PR approval for ALL external contributors)"
fi
[[ $CRCI_MODE == apply ]] || crci_note "dry-run: nothing is changed; '?' lines are read-only checks, assumed to find nothing"

for r in "${REPOS[@]}"; do
  if ((CLEAR == 1)); then
    present=0
    crci_exists gh variable get "$NAME" --repo "$r" && present=1
    if ((present == 1)) || [[ $CRCI_MODE != apply ]]; then
      crci_run gh variable delete "$NAME" --repo "$r"
    else
      crci_note "$r: $NAME is not set; nothing to clear"
    fi
  else
    crci_run gh variable set "$NAME" --repo "$r" --body "$VALUE"
  fi
done

if ((CLEAR == 1)); then
  crci_note "jobs already queued on the cr-ci label keep waiting: cancel and re-run them (gh run rerun RUN_ID)"
fi
