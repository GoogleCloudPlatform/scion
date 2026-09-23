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
#
# install-core-toolchain.sh — bring an arbitrary Linux base up to the state that
# scion-base expects (see verify-base-contract.sh for the contract itself).
#
# WHY A SCRIPT AND NOT TWO DOCKERFILES
#
# scion-base is built on core-base (Debian, from scratch) or on thick-prep
# (Google Cloud Workstations, Ubuntu, already full of tooling). Those two
# started as independent recipes, drifted, and the drift was invisible: the
# thick chain shipped git 2.43 against a hard 2.47 requirement for as long as
# nobody looked. One script run by both chains makes drift impossible to
# express — there is only one recipe.
#
# RECONCILE, DON'T INSTALL
#
# Every step asks what the base already provides before doing anything:
#
#   * Present and adequate -> leave it alone. The Cloud Workstations base
#     already ships gcloud, kubectl and Go; reinstalling them wastes build time
#     and leaves two copies at different paths for PATH order to arbitrate.
#   * Present but too old  -> replace it, and say so.
#   * Absent               -> install it.
#
# Versions are floors, never equality. These base images are used for far more
# than building scion, so a base that ships a NEWER Go than go.mod declares is
# fine and is deliberately left alone: Go builds a module whose `go` directive
# is older without complaint, and pinning the image down to go.mod would
# degrade every other consumer of the image to serve one of them.
#
# CAPABILITY DETECTION, NOT DISTRO DETECTION
#
# Where the two bases differ (chromium has no apt candidate on Ubuntu) the test
# is for the capability — "does apt offer this package?" — not for the distro
# name. A third base then works without a third branch.
#
# Usage: install-core-toolchain.sh <step> [args]
# Steps are invoked individually by the Dockerfiles rather than via `all` so
# each remains its own cacheable layer.

set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

KEYRING=/usr/share/keyrings/cloud.google.gpg

log() { printf '[toolchain] %s\n' "$*"; }
skip() { printf '[toolchain] skip: %s\n' "$*"; }

# ver_ge <have> <want> — true when <have> >= <want>, numeric-aware.
ver_ge() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

deb_arch() { dpkg --print-architecture; }

# Two different questions get asked about apt packages here, they have two
# different answers, and using one helper for both produced a bug in each
# direction during development. Keep them separate and named for what they ask.

# apt_installable <package> — will `apt-get install <package>` succeed?
#
# Asks apt's own resolver, which is the only thing that agrees with the install
# by construction. `apt-cache policy` does not: trixie's `dnsutils` is a pure
# virtual package provided solely by bind9-dnsutils, so policy reports
# "Candidate: (none)" while `apt-get install dnsutils` installs it happily. Use
# this when the question is literally "can I install this name".
apt_installable() {
  apt-get install -s --no-install-recommends "$1" >/dev/null 2>&1
}

# apt_ships_real_package <package> — is there a concrete package of exactly this
# name with an installation candidate?
#
# Use this when the package *name* is standing in for "this distro ships this
# software itself", because virtual names must answer no. Ubuntu's `chromium`
# is the case in point: it is a virtual name satisfied by `chromium-browser`, a
# transitional shim whose actual payload is a snap. apt_installable says yes,
# apt reports a successful install, and no `chromium` binary ever appears —
# so asking the installable question there silently produces a browserless
# image. Debian, which ships a real chromium deb, must still answer yes.
apt_ships_real_package() {
  apt-cache policy "$1" 2>/dev/null | awk '/Candidate:/ {print $2}' | grep -qv '(none)'
}

ensure_google_keyring() {
  [ -s "$KEYRING" ] && return 0
  log "installing Google apt signing key"
  apt-get update -qq
  apt-get install -y --no-install-recommends ca-certificates curl gnupg >/dev/null
  curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg | gpg --dearmor -o "$KEYRING"
}

# ---------------------------------------------------------------------------

# The package set shared by every scion base. chromium is deliberately absent —
# it is the one package whose availability differs between the two bases, and
# it gets its own step so that difference is visible rather than buried in a
# 30-line install list that fails as a unit.
COMMON_PACKAGES="
  tmux ca-certificates libexpat1 zlib1g python3 python3-venv make g++ man-db
  curl wget dnsutils less jq bc unzip rsync ripgrep procps psmisc lsof socat
  sudo fzf zsh gnupg2 iptables ipset iproute2 aggregate nano vim openssh-client
  lsb-release dbus-x11 gnome-keyring libsecret-1-0 libsecret-tools
"

step_apt_common() {
  ensure_apt_updated
  # libcurl4t64 on suites that did the 64-bit time_t transition (noble, trixie),
  # libcurl4 elsewhere. The vendored git's HTTPS helper links against
  # libcurl.so.4; without it git-remote-https dies at load time with
  # "error while loading shared libraries: libcurl.so.4".
  local curl_pkg=libcurl4t64
  apt_installable libcurl4t64 || curl_pkg=libcurl4

  # Pre-flight. Without it, one unavailable package fails the whole apt-get with
  # a message that does not obviously name the culprit — which is exactly how
  # chromium-on-Ubuntu presents. Simulate the set in one shot (cheap, and the
  # normal case), and only fall back to the per-package loop to name names.
  # shellcheck disable=SC2086
  if ! apt-get install -s --no-install-recommends $COMMON_PACKAGES "$curl_pkg" >/dev/null 2>&1; then
    local missing=""
    local p
    for p in $COMMON_PACKAGES $curl_pkg; do
      apt_installable "$p" || missing="$missing $p"
    done
    . /etc/os-release
    echo "FAIL: apt cannot install these on ${ID} ${VERSION_CODENAME}:${missing:- (set resolves individually but not together)}" >&2
    echo "      Either the suite lacks them (enable the component), they were" >&2
    echo "      renamed between suites, or they need a dedicated step here with" >&2
    echo "      a capability test — as chromium and libcurl4t64 do." >&2
    exit 1
  fi

  log "installing common packages"
  # shellcheck disable=SC2086
  apt-get install -y --no-install-recommends $COMMON_PACKAGES "$curl_pkg"
  apt_cleanup
}

# chromium. On Debian it is a real apt package. On Ubuntu it is snap-only, so we
# install Google Chrome from Google's own apt repository and put it on PATH
# under the expected name. The name matters as much as the browser: the web-dev
# template runs a service literally called `chromium`, pkg/api/types_test.go
# expects CHROME_PATH=/usr/bin/chromium, and
# pkg/hub/suspended_page_browser_test.go calls exec.LookPath("chromium").
#
# The apt route is both predicted and verified, and both halves earn their keep:
#   * predicted, so that Ubuntu's `chromium` virtual name — a shim that depends
#     on snapd — is never installed at all;
#   * verified, because the whole point of this file is to stop trusting that a
#     thing happened. `apt-get install` exiting 0 is not evidence that a browser
#     exists. Falling back on the postcondition means a future suite that
#     invents a new flavour of transitional package still yields a working
#     image instead of a silently browserless one.
step_chromium() {
  if command -v chromium >/dev/null 2>&1; then
    skip "chromium already present ($(command -v chromium))"
    return 0
  fi
  ensure_apt_updated

  if apt_ships_real_package chromium; then
    log "installing chromium from apt"
    apt-get install -y --no-install-recommends chromium
    if command -v chromium >/dev/null 2>&1; then
      apt_cleanup
      return 0
    fi
    log "apt reported success but produced no chromium binary; falling back"
    apt-get purge -y chromium >/dev/null 2>&1 || true
    apt-get autoremove -y >/dev/null 2>&1 || true
    ensure_apt_updated
  fi

  log "no usable chromium from apt; installing google-chrome-stable instead"
  apt-get install -y --no-install-recommends ca-certificates curl gnupg >/dev/null
  curl -fsSL https://dl.google.com/linux/linux_signing_key.pub |
    gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg
  echo "deb [arch=$(deb_arch) signed-by=/usr/share/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" \
    >/etc/apt/sources.list.d/google-chrome.list
  apt-get update -qq
  apt-get install -y --no-install-recommends google-chrome-stable
  ln -sf /usr/bin/google-chrome-stable /usr/bin/chromium
  apt_cleanup
}

# Go. Floor, not pin: a base shipping something newer keeps it. Only install
# when Go is absent or older than the floor.
step_go() {
  local min="${1:?usage: go <min-version> <pin-version>}"
  local pin="${2:?usage: go <min-version> <pin-version>}"

  if command -v go >/dev/null 2>&1; then
    local have
    have="$(go version | awk '{print $3}' | sed 's/^go//')"
    if ver_ge "$have" "$min"; then
      skip "go $have already satisfies the >= $min floor (not downgrading to $pin)"
      return 0
    fi
    log "go $have is below the $min floor; installing $pin"
  else
    log "go absent; installing $pin"
  fi

  local go_arch
  case "$(deb_arch)" in
  amd64) go_arch=linux-amd64 ;;
  arm64) go_arch=linux-arm64 ;;
  *)
    echo "FAIL: unsupported architecture $(deb_arch) for the Go toolchain" >&2
    exit 1
    ;;
  esac
  curl -fsSL "https://go.dev/dl/go${pin}.${go_arch}.tar.gz" -o /tmp/go.tar.gz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tar.gz
  rm -f /tmp/go.tar.gz
}

step_gcsfuse() {
  if command -v gcsfuse >/dev/null 2>&1; then
    skip "gcsfuse already present"
    return 0
  fi
  ensure_google_keyring
  # The repo component is derived from the suite codename (gcsfuse-trixie,
  # gcsfuse-noble, ...). Verified to exist for both bases in use.
  . /etc/os-release
  log "adding gcsfuse-${VERSION_CODENAME} repository"
  echo "deb [signed-by=${KEYRING}] https://packages.cloud.google.com/apt gcsfuse-${VERSION_CODENAME} main" \
    >/etc/apt/sources.list.d/gcsfuse.list
  apt-get update -qq
  if ! apt_installable gcsfuse; then
    echo "FAIL: gcsfuse has no candidate in gcsfuse-${VERSION_CODENAME}." >&2
    echo "      That suite likely does not exist yet for this base." >&2
    exit 1
  fi
  apt-get install -y --no-install-recommends gcsfuse
  apt_cleanup
}

step_gcloud() {
  if command -v gcloud >/dev/null 2>&1; then
    skip "gcloud already present ($(command -v gcloud)) — not installing a second copy"
    return 0
  fi
  ensure_google_keyring
  log "installing google-cloud-cli"
  echo "deb [signed-by=${KEYRING}] https://packages.cloud.google.com/apt cloud-sdk main" \
    >/etc/apt/sources.list.d/google-cloud-sdk.list
  apt-get update -qq
  apt-get install -y google-cloud-cli
  apt_cleanup
}

step_kubectl() {
  if command -v kubectl >/dev/null 2>&1; then
    skip "kubectl already present ($(command -v kubectl))"
    return 0
  fi
  log "installing kubectl"
  local stable
  stable="$(curl -fsSL https://dl.k8s.io/release/stable.txt)"
  curl -fsSLo /tmp/kubectl "https://dl.k8s.io/release/${stable}/bin/linux/$(deb_arch)/kubectl"
  install -o root -g root -m 0755 /tmp/kubectl /usr/local/bin/kubectl
  rm -f /tmp/kubectl
}

# gh. Installed to /usr/bin/gh, not /usr/local/bin/gh, and that is load-bearing:
# scion-base only installs its GitHub-App-token-injecting wrapper when
# /usr/bin/gh exists (image-build/scion-base/Dockerfile). gh living anywhere
# else means no wrapper and no token, silently.
#
# The tar.gz release is used rather than apt (stale: 2.23.0) or the .deb (leaves
# dpkg in a broken state, because the vendored git is not dpkg-tracked and the
# package depends on git).
step_gh() {
  local want="${1:?usage: gh <version>}"
  if command -v gh >/dev/null 2>&1; then
    local have
    have="$(gh --version 2>/dev/null | head -n1 | awk '{print $3}')"
    if [ -n "$have" ] && ver_ge "$have" "$want"; then
      skip "gh $have already satisfies the >= $want floor"
      # Still enforce the path contract even when we keep the existing binary.
      [ -x /usr/bin/gh ] || ln -sf "$(command -v gh)" /usr/bin/gh
      return 0
    fi
    log "gh ${have:-unknown} is below $want; replacing"
  fi
  log "installing gh $want"
  local arch tarball
  arch="$(deb_arch)"
  tarball="gh_${want}_linux_${arch}"
  curl -fsSL "https://github.com/cli/cli/releases/download/v${want}/${tarball}.tar.gz" -o /tmp/gh.tar.gz
  tar -xzf /tmp/gh.tar.gz -C /tmp
  install -o root -g root -m 0755 "/tmp/${tarball}/bin/gh" /usr/bin/gh
  rm -rf /tmp/gh.tar.gz "/tmp/${tarball}"
}

step_golangci_lint() {
  if command -v golangci-lint >/dev/null 2>&1; then
    skip "golangci-lint already present"
    return 0
  fi
  log "installing golangci-lint"
  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh |
    sh -s -- -b /usr/local/bin
}

step_npm_global() {
  log "creating npm global prefix"
  mkdir -p /usr/local/share/npm-global
}

# Global npm tooling. NPM_CONFIG_PREFIX must already be exported by the
# Dockerfile (ENV cannot be set from here) or these land in the wrong place.
step_npm_tools() {
  log "installing global npm tooling"
  npm install -g chrome-devtools-mcp @playwright/cli@latest
}

# Free uid 1000 for the scion user that scion-base creates. Matched by uid
# rather than by name: node:24 names it `node`, Cloud Workstations names it
# `ubuntu`, and the next base will pick a third name.
step_free_uid_1000() {
  local occupant
  if occupant="$(getent passwd 1000 | cut -d: -f1)" && [ -n "$occupant" ]; then
    log "removing user '$occupant' to free uid 1000 for scion"
    userdel -r "$occupant" 2>/dev/null || userdel "$occupant"
    groupdel "$occupant" 2>/dev/null || true
  else
    skip "uid 1000 already free"
  fi
}

# ---------------------------------------------------------------------------

_APT_UPDATED=0
ensure_apt_updated() {
  [ "$_APT_UPDATED" -eq 1 ] && return 0
  apt-get update -qq
  _APT_UPDATED=1
}

apt_cleanup() {
  apt-get clean
  rm -rf /var/lib/apt/lists/*
  _APT_UPDATED=0
}

main() {
  local step="${1:?usage: install-core-toolchain.sh <step> [args]}"
  shift || true
  case "$step" in
  apt-common) step_apt_common "$@" ;;
  chromium) step_chromium "$@" ;;
  go) step_go "$@" ;;
  gcsfuse) step_gcsfuse "$@" ;;
  gcloud) step_gcloud "$@" ;;
  kubectl) step_kubectl "$@" ;;
  gh) step_gh "$@" ;;
  golangci-lint) step_golangci_lint "$@" ;;
  npm-global) step_npm_global "$@" ;;
  npm-tools) step_npm_tools "$@" ;;
  free-uid-1000) step_free_uid_1000 "$@" ;;
  *)
    echo "unknown step: $step" >&2
    echo "steps: apt-common chromium go gcsfuse gcloud kubectl gh golangci-lint npm-global npm-tools free-uid-1000" >&2
    exit 2
    ;;
  esac
}

main "$@"
