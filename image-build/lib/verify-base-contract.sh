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
# verify-base-contract.sh — assert the contract that scion-base and the harness
# images rely on from whatever image sits beneath them.
#
# WHY THIS EXISTS
#
# scion-base can be built on top of either core-base (the Debian chain) or
# thick-prep (the Cloud Workstations chain; see scripts/lib/targets.sh, where
# `thick` is thick-prep -> scion-base and therefore never passes through
# core-base). Everything scion-base needs from the layer below it used to be
# provided by convention and verified by nobody. Writing the contract down found
# four violations of it, all live at the time of writing:
#
#   * git     — core-base provided >= 2.47, thick-prep provided whatever Ubuntu
#     shipped (2.43). Every thick image silently lost worktree-per-agent mode.
#   * gh      — scion-base only installs its token-injecting wrapper when it
#     finds /usr/bin/gh. When core-base moved gh to /usr/local/bin the wrapper
#     stopped being installed on that chain, with no error anywhere.
#   * gcsfuse — provided by core-base, never installed by thick-prep, so GCS
#     volume mounts (pkg/runtime/common.go) fail at runtime on every thick image.
#   * NPM_CONFIG_PREFIX — exported by core-base, never by thick-prep, so the
#     npm-global directory scion-base chowns is unused on the thick chain and
#     `npm install -g` fails on permissions instead.
#
# All four are the same defect: an inherited assumption with no assertion. This
# script is the assertion. Every base that scion-base is built on must pass it,
# so a divergence fails a build instead of quietly degrading an agent.
#
# Every check below names a concrete consumer. Do not add a check without one —
# an unjustified requirement here becomes an unnecessary install over there.
#
# Usage: verify-base-contract.sh
#   GO_MIN_VERSION   minimum acceptable Go (default 1.26.1; see go.mod)
#   NODE_MIN_VERSION minimum acceptable Node major (default 24)
#   TARGETARCH       BuildKit automatic arg — the arch being built (e.g. arm64)
#   BUILDARCH        BuildKit automatic arg — the arch doing the building
#
# When TARGETARCH and BUILDARCH differ the leg is running under qemu-user, and
# the few checks that emulation cannot support are skipped with a printed
# reason. Both unset (running the script inside a finished container) means
# native, and everything runs.
#
# All checks are network-free. All failures are collected and reported together:
# seeing every violation in one build beats fixing them one round-trip at a time.

set -uo pipefail

GO_MIN_VERSION="${GO_MIN_VERSION:-1.26.1}"
NODE_MIN_VERSION="${NODE_MIN_VERSION:-24}"
GIT_MIN_VERSION="2.47.0" # pkg/util/git.go CheckGitVersion — hard floor, not a preference

# --- Emulation awareness ----------------------------------------------------
#
# Multi-arch builds run the non-native leg under qemu-user, where some things
# genuinely cannot work no matter how correct the image is. qemu-user does not
# implement ptrace, so anything depending on it (chromium's crashpad, and by
# extension its whole multi-process startup) aborts. A check that cannot pass
# in the environment it runs in does not measure the image; it measures the
# emulator, and it fails every multi-arch build.
#
# BuildKit supplies TARGETARCH and BUILDARCH as automatic build args; the
# Dockerfiles forward them. When they differ, this leg is emulated.
#
# Defaults are deliberate: with neither set — running the script inside a
# finished container to re-check a live image, which is a supported use — this
# is real hardware, so EMULATED is false and every check runs. Assuming native
# is the safe default, because the cost of being wrong is a spurious failure
# that gets investigated, not a defect that ships.
BUILD_ARCH="${BUILDARCH:-}"
EXPECTED_ARCH="${TARGETARCH:-}"
EMULATED="false"
if [ -n "$BUILD_ARCH" ] && [ -n "$EXPECTED_ARCH" ] && [ "$BUILD_ARCH" != "$EXPECTED_ARCH" ]; then
  EMULATED="true"
fi

FAILURES=0

fail() {
  printf 'FAIL  %s\n' "$*" >&2
  FAILURES=$((FAILURES + 1))
}

ok() {
  printf 'ok    %s\n' "$*"
}

# note — something deliberately not asserted here, with the reason. Distinct
# from ok (which claims a check passed) and from silence (which hides that a
# check was skipped). A skipped check must be visible in the build log or the
# next person will believe it ran.
note() {
  printf 'note  %s\n' "$*"
}

# is_elf <path> — true when the file starts with the ELF magic number.
is_elf() {
  [ -f "$1" ] && [ "$(head -c 4 "$1" 2>/dev/null | tr -d '\0')" = "$(printf '\177ELF')" ]
}

# elf_arch <path> — echoes amd64 | arm64 | unknown.
#
# Reads e_machine (2 bytes, little-endian, at offset 0x12) with od rather than
# calling readelf or file: binutils is not installed in these final images, and
# adding a package so a verification script can run is the wrong trade.
elf_arch() {
  local b
  b="$(od -An -tu1 -j18 -N1 "$1" 2>/dev/null | tr -d ' ')"
  case "$b" in
    62)  echo amd64 ;;   # EM_X86_64
    183) echo arm64 ;;   # EM_AARCH64
    *)   echo unknown ;;
  esac
}

# ver_ge <have> <want> — true when <have> >= <want>, numeric-aware.
ver_ge() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

need_cmd() {
  # need_cmd <binary> <consumer>
  if command -v "$1" >/dev/null 2>&1; then
    ok "$1 present ($(command -v "$1"))"
  else
    fail "$1 missing — required by $2"
  fi
}

echo "--- base contract ---"

# ---------------------------------------------------------------------------
# git — pkg/util/git.go CheckGitVersion requires >= 2.47.0 for
# `worktree add --relative-paths`, which is what worktree-per-agent mode runs.
#
# The five sub-checks are not redundant. Each maps to a distinct real failure
# seen while vendoring git from Chainguard, and several of them are invisible
# to `git --version`:
# ---------------------------------------------------------------------------
if ! command -v git >/dev/null 2>&1; then
  fail "git missing — required by pkg/util/git.go and all worktree operations"
else
  git_path="$(command -v git)"
  git_ver="$(git --version | awk '{print $3}')"

  # 1. PATH precedence. A second, older git earlier on PATH shadows ours.
  if [ "$git_path" = /usr/bin/git ]; then
    ok "git at /usr/bin/git"
  else
    fail "git resolves to $git_path, expected /usr/bin/git — Chainguard's
      /usr/libexec/git-core/git is a relative symlink to ../../bin/git, so the
      binary must occupy /usr/bin/git or git re-execs into a different one"
  fi

  # 2. Version floor.
  if ver_ge "$git_ver" "$GIT_MIN_VERSION"; then
    ok "git $git_ver >= $GIT_MIN_VERSION"
  else
    fail "git $git_ver < $GIT_MIN_VERSION — scion requires
      'worktree add --relative-paths' (pkg/util/git.go CheckGitVersion).
      On Ubuntu noble apt caps at 2.43.0; vendor git instead of apt-installing it"
  fi

  # 3. Helpers on the compiled-in GIT_EXEC_PATH. When these are misplaced the
  #    error is "git: 'remote-https' is not a git command", which reads like
  #    missing HTTPS support rather than a path problem.
  if [ -x /usr/libexec/git-core/git-remote-https ]; then
    ok "git helpers on GIT_EXEC_PATH"
  else
    fail "/usr/libexec/git-core/git-remote-https missing — git helpers are not
      on the compiled-in GIT_EXEC_PATH; HTTPS remotes will fail at runtime"
  fi

  # 4. Shared libraries resolve. A vendored binary can be one glibc bump away
  #    from unusable and still pass `git --version` if the helper is what links
  #    against the missing library.
  if [ -x /usr/libexec/git-core/git-remote-https ]; then
    missing="$(ldd /usr/libexec/git-core/git-remote-https 2>/dev/null | grep 'not found' || true)"
    if [ -z "$missing" ]; then
      ok "git-remote-https libraries resolve"
    else
      fail "unresolved shared libraries in git-remote-https:
$missing"
    fi
  fi

  # 5. Re-exec target. `git --version` can report the new binary while
  #    subcommands silently run an old one via the exec-path symlink. The
  #    symptom is baffling and unrelated-looking:
  #      error: unknown option `detach'
  #      fatal: unknown repository extension found: relativeworktrees
  if [ -x /usr/libexec/git-core/git ]; then
    if [ "$(/usr/libexec/git-core/git --version)" = "$(git --version)" ]; then
      ok "git re-exec target matches ($git_ver)"
    else
      fail "git re-execs into a different binary via GIT_EXEC_PATH:
      $(/usr/libexec/git-core/git --version) vs $(git --version)"
    fi
  fi

  # 6. End-to-end. The feature, not the version number — this is the only check
  #    that would survive a version string that lies.
  if git init -q /tmp/_contract_git 2>/dev/null &&
    git -C /tmp/_contract_git -c user.email=build@scion.invalid -c user.name=build \
      commit -q --allow-empty -m check 2>/dev/null &&
    git -C /tmp/_contract_git worktree add --relative-paths -q /tmp/_contract_git_wt HEAD 2>/dev/null; then
    ok "git worktree add --relative-paths works end-to-end"
  else
    fail "'git worktree add --relative-paths' failed end-to-end despite
      git $git_ver — worktree-per-agent mode would be broken at runtime"
  fi
  rm -rf /tmp/_contract_git /tmp/_contract_git_wt

  # 7. Clean stderr. The vendored libpcre2 exists solely to stop the dynamic
  #    loader printing "no version information available" on every git call,
  #    into output that agents parse. Check the symptom, not the mechanism.
  git_noise="$(git --version 2>&1 1>/dev/null)"
  if [ -z "$git_noise" ]; then
    ok "git writes nothing to stderr"
  else
    fail "git writes to stderr on a plain --version, so agent-visible output is
      polluted: $git_noise"
  fi

  # 8. And it must be clean *without* a global LD_LIBRARY_PATH. That variable
  #    would force the vendored libpcre2 on every process in the container
  #    rather than on git; the RUNPATH set by stage-vendored-git.sh is what
  #    makes it unnecessary, and this check is what stops it coming back.
  if [ -z "${LD_LIBRARY_PATH:-}" ]; then
    ok "LD_LIBRARY_PATH unset (vendored libs reached via RUNPATH)"
  else
    fail "LD_LIBRARY_PATH is set to '$LD_LIBRARY_PATH' — vendored libraries
      should be reached through a RUNPATH on the binaries that need them, not
      imposed image-wide (see image-build/lib/stage-vendored-git.sh)"
  fi
fi

# ---------------------------------------------------------------------------
# uid 1000 must be free — scion-base runs `useradd -m -s /bin/zsh -u 1000 scion`.
# Checked by uid, not by name: the Debian chain collides with `node` and the
# Cloud Workstations chain with `ubuntu`, and the next base will invent a third.
# ---------------------------------------------------------------------------
if occupant="$(getent passwd 1000 2>/dev/null)"; then
  fail "uid 1000 is taken by '${occupant%%:*}' — scion-base creates the scion
      user at uid 1000 and will fail with 'UID 1000 is not unique'"
else
  ok "uid 1000 free"
fi

# /bin/zsh — scion-base passes it as the scion user's login shell.
if [ -x /bin/zsh ]; then
  ok "/bin/zsh present"
else
  fail "/bin/zsh missing — scion-base runs 'useradd -s /bin/zsh scion'"
fi

# ---------------------------------------------------------------------------
# npm global prefix. Two halves, and only having both is useful:
#   * the directory must exist, because scion-base chowns it to the scion user;
#   * NPM_CONFIG_PREFIX must actually point at it, or npm ignores the directory
#     entirely and `npm install -g` writes to /usr/lib/node_modules — root-owned,
#     so it fails for the scion user. core-base set the variable and thick-prep
#     only created the directory, which made the chown on the thick chain a
#     no-op decoration. Third instance of the same defect class.
# ---------------------------------------------------------------------------
NPM_PREFIX_EXPECTED=/usr/local/share/npm-global
if [ -d "$NPM_PREFIX_EXPECTED" ]; then
  ok "$NPM_PREFIX_EXPECTED present"
else
  fail "$NPM_PREFIX_EXPECTED missing — scion-base chowns it to scion"
fi
if [ "${NPM_CONFIG_PREFIX:-}" = "$NPM_PREFIX_EXPECTED" ]; then
  ok "NPM_CONFIG_PREFIX=$NPM_PREFIX_EXPECTED"
else
  fail "NPM_CONFIG_PREFIX is '${NPM_CONFIG_PREFIX:-<unset>}', expected
      $NPM_PREFIX_EXPECTED — otherwise 'npm install -g' as the scion user writes
      to /usr/lib/node_modules and fails on permissions, and the directory
      scion-base chowns is never used"
fi
case ":${PATH}:" in
*":${NPM_PREFIX_EXPECTED}/bin:"*)
  ok "$NPM_PREFIX_EXPECTED/bin on PATH"
  ;;
*)
  fail "$NPM_PREFIX_EXPECTED/bin is not on PATH — globally installed harness
      tooling would be installed but not runnable"
  ;;
esac

# gh — scion-base replaces the real binary with a wrapper that injects the
# GitHub App token. If gh is absent the wrapper is never installed and agents
# fall back to unauthenticated gh with no warning.
need_cmd gh "scion-base's sciontool gh-wrapper (GitHub App token injection)"

# tmux — buildEntrypoint in pkg/runtime/cloudrun_sandbox_runtime.go execs it for
# agent session management. Missing tmux kills every sandbox at launch (exit 127).
need_cmd tmux "sandbox entrypoint (buildEntrypoint, cloudrun_sandbox_runtime.go)"

# gcsfuse — pkg/runtime/common.go builds a literal `gcsfuse` command line for
# GCS volume mounts. Absent, those mounts fail at runtime rather than at build.
need_cmd gcsfuse "GCS volume mounts (pkg/runtime/common.go)"

# sudo — scion-base installs /etc/sudoers.d/scion for passwordless sudo.
need_cmd sudo "scion-base passwordless sudo for the scion user"

# ---------------------------------------------------------------------------
# chromium — the *name* is what is depended on, not merely a browser being
# present: .scion/templates/web-dev/scion-agent.yaml runs a `chromium` service,
# pkg/api/types_test.go expects CHROME_PATH=/usr/bin/chromium, and
# pkg/hub/suspended_page_browser_test.go does exec.LookPath("chromium").
# On Ubuntu chromium is snap-only and has no apt candidate, so that chain
# satisfies this by symlinking google-chrome-stable into place.
# ---------------------------------------------------------------------------
if command -v chromium >/dev/null 2>&1; then
  chromium_bin="$(command -v chromium)"

  # /usr/bin/chromium is a ~5KB wrapper *script* on Debian, and a symlink to
  # google-chrome-stable on the Ubuntu chain. Neither is an ELF file, so the
  # architecture check below has to find the real payload first. Checking the
  # wrapper directly reports "unknown" for every image and is worse than no
  # check at all, because it looks like it passed.
  chromium_payload="$(readlink -f "$chromium_bin")"
  if ! is_elf "$chromium_payload"; then
    for _cand in /usr/lib/chromium/chromium \
                 /usr/lib/chromium-browser/chromium-browser \
                 /opt/google/chrome/chrome \
                 /opt/google/chrome/google-chrome; do
      if [ -x "$_cand" ] && is_elf "$_cand"; then
        chromium_payload="$_cand"
        break
      fi
    done
  fi

  # 1. Architecture. Catches a cross-install landing the wrong binary — which
  #    an execution test cannot distinguish from an emulation failure, so this
  #    is the check that actually earns its place on an emulated leg.
  if is_elf "$chromium_payload"; then
    chromium_arch="$(elf_arch "$chromium_payload")"
    if [ -z "$EXPECTED_ARCH" ] || [ "$chromium_arch" = "$EXPECTED_ARCH" ]; then
      ok "chromium payload is $chromium_arch ($chromium_payload)"
    else
      fail "chromium payload $chromium_payload is $chromium_arch but this image
      targets $EXPECTED_ARCH — a wrong-architecture browser will fail at runtime
      on the host it ships to, long after this build"
    fi
  else
    note "could not locate an ELF payload behind $chromium_bin; skipping the
      architecture check (the execution checks below still apply)"
  fi

  # 2. Dynamic linkage. This is what usually breaks — libnss3, libgbm1 and
  #    friends go missing when a package set changes — and it is checkable
  #    without running the browser's multi-process machinery.
  if command -v ldd >/dev/null 2>&1 && is_elf "$chromium_payload"; then
    chromium_missing="$(ldd "$chromium_payload" 2>/dev/null | grep 'not found' || true)"
    if [ -z "$chromium_missing" ]; then
      ok "chromium shared libraries all resolve"
    else
      fail "chromium is missing shared libraries:
${chromium_missing}"
    fi
  fi

  # 3. Execution. --version starts the real binary and exercises the whole
  #    dynamic-link path, but stays single-process, so it works under qemu.
  if chromium_ver="$(chromium --version 2>&1)"; then
    ok "chromium executes (${chromium_ver%%$'\n'*})"
  else
    fail "chromium is on PATH but 'chromium --version' failed — the binary
      cannot start at all. Its output:
${chromium_ver:-(no output)}"
  fi

  # 4. Headless. Deliberately NOT run under emulation.
  #
  # qemu-user does not implement ptrace (the log line is literally
  # "ptrace: Function not implemented (38)"). Chromium's crashpad handler needs
  # it, so the GPU process cannot be launched or supervised and the browser
  # aborts with SIGABRT — even with --disable-gpu, which suppresses GPU *use*
  # but not the process launch. This is a property of the emulator, not of the
  # image: the previously shipped core-base arm64 (7ee54fa), built before this
  # check existed, fails here identically. Asserting it on an emulated leg
  # tests qemu and fails every multi-arch build.
  #
  # The checks above still catch every defect this one was written for — a
  # missing binary, the Ubuntu snap-shim that installs nothing, a broken
  # symlink, missing libraries, a wrong-architecture payload.
  if [ "$EMULATED" = "true" ]; then
    note "skipping the chromium headless smoke test: building $EXPECTED_ARCH on
      $BUILD_ARCH, so this runs under qemu-user, which cannot support chromium's
      multi-process model (no ptrace). Checks 1-3 above still ran. Headless is
      verified on the native leg of this same build."
  else
    # Keep stderr: when this fails it is almost always a missing shared library
    # (libnss3, libgbm1, ...) or a sandbox refusal, and the loader names the
    # culprit precisely. Discarding it turns a one-line diagnosis into a bisect.
    # --dump-dom writes the DOM to stdout, which is noise here, so stdout goes to
    # /dev/null and stderr is what gets captured.
    if chromium_err="$(chromium --headless --no-sandbox --disable-gpu --dump-dom about:blank 2>&1 >/dev/null)"; then
      ok "chromium runs headless"
    else
      fail "chromium is on PATH but failed a headless smoke test — a browser that
      cannot start headless is no use to the web-dev template. Its stderr:
${chromium_err:-(no output)}"
    fi
  fi
else
  fail "chromium missing from PATH — required by name (not just 'a browser') by
      .scion/templates/web-dev/scion-agent.yaml and pkg/hub/suspended_page_browser_test.go.
      On Ubuntu there is no apt candidate; install google-chrome-stable and
      symlink it to /usr/bin/chromium"
fi

# ---------------------------------------------------------------------------
# Toolchains. Both are floors, never equality: these images are used for far
# more than building scion, so a base that ships something newer is fine and
# must not be downgraded to match go.mod.
# ---------------------------------------------------------------------------
if command -v go >/dev/null 2>&1; then
  go_ver="$(go version | awk '{print $3}' | sed 's/^go//')"
  if ver_ge "$go_ver" "$GO_MIN_VERSION"; then
    ok "go $go_ver >= $GO_MIN_VERSION"
  else
    fail "go $go_ver < $GO_MIN_VERSION — scion-base compiles scion and sciontool
      with this toolchain (go.mod declares $GO_MIN_VERSION)"
  fi
else
  fail "go missing — scion-base's builder stage runs 'go build'"
fi

if command -v node >/dev/null 2>&1; then
  node_major="$(node --version | sed 's/^v//' | cut -d. -f1)"
  if [ "$node_major" -ge "$NODE_MIN_VERSION" ] 2>/dev/null; then
    ok "node $(node --version) >= v${NODE_MIN_VERSION}"
  else
    fail "node $(node --version) < v${NODE_MIN_VERSION} — harness tooling
      (chrome-devtools-mcp, @playwright/cli) requires Node ${NODE_MIN_VERSION}+"
  fi
else
  fail "node missing — required by every npm-installed harness tool"
fi

need_cmd npm "global harness installs (chrome-devtools-mcp, @playwright/cli)"

echo "--- end base contract ---"

if [ "$FAILURES" -ne 0 ]; then
  printf '\nbase contract: %d check(s) failed.\n' "$FAILURES" >&2
  printf 'This image cannot be used as a foundation for scion-base as-is.\n' >&2
  printf 'See image-build/lib/install-core-toolchain.sh for how each item is provided.\n' >&2
  exit 1
fi

printf '\nbase contract: all checks passed.\n'
