# syntax=docker/dockerfile:1
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

# Thick Base Prep Layer
# Prepares the Google Cloud Workstations base image for use as a scion-base
# foundation. Fixes compatibility issues between the Cloud Workstations image
# and scion-base's expectations:
#
#   1. UID 1000 conflict  - Cloud Workstations ships an 'ubuntu' user at UID
#      1000; scion-base creates 'scion' at the same UID.
#   2. Missing packages   - zsh (required by useradd -s /bin/zsh), fzf (claude
#      harness keybindings), gh and ripgrep (core scion tools).
#   3. Missing directory  - /usr/local/share/npm-global (scion-base chowns it).
#   4. git too old        - the Cloud Workstations base is Ubuntu 24.04 (noble),
#      which ships git 2.43.0 with no newer apt candidate. Scion hard-requires
#      2.47.0 (pkg/util/git.go CheckGitVersion, for `worktree add
#      --relative-paths`). This matters only on the thick chain: scion-base
#      never installs git, and targets.sh routes thick as
#      thick-prep -> scion-base, bypassing core-base -- which is the only other
#      place git is provided. Without the vendored git below, every thick image
#      silently loses worktree-per-agent mode.
#
# Architecture: amd64 only (Cloud Workstations base has no arm64 variant).

ARG BASE_IMAGE=us-central1-docker.pkg.dev/cloud-workstations-images/predefined/base:latest

# Keep in sync with image-build/core-base/Dockerfile. See that file for the
# rationale and for the digest-pinning note.
ARG GIT_IMAGE=chainguard/git:latest
FROM ${GIT_IMAGE} AS git-source

# Stage and patch the vendored git exactly as core-base does. See
# core-base/Dockerfile's git-patched stage for the full rationale: the short
# version is that Chainguard's libpcre2 is reached through a RUNPATH on the
# handful of binaries that need it, rather than an image-wide LD_LIBRARY_PATH
# that every process in the container would inherit.
#
# The patching stage is Debian even though this image is Ubuntu — patchelf only
# rewrites ELF headers, so the distro running it is irrelevant, and using the
# same stage as core-base keeps the two chains byte-identical here.
FROM node:24-trixie-slim AS git-patched
RUN apt-get update && apt-get install -y --no-install-recommends \
    patchelf \
    binutils \
    && rm -rf /var/lib/apt/lists/*
COPY --from=git-source /usr/bin/git          /staged/usr/bin/git
COPY --from=git-source /usr/libexec/git-core /staged/usr/libexec/git-core
COPY --from=git-source /usr/share/git-core   /staged/usr/share/git-core
COPY --from=git-source /usr/lib/libpcre2-8.so.0 /staged/usr/local/lib/scion-git/libpcre2-8.so.0
RUN set -eu; \
    command -v readelf >/dev/null \
      || { echo "FAIL: readelf missing — the NEEDED scan below would silently \
match nothing and quietly patch zero binaries"; exit 1; }; \
    patched=0; \
    for f in /staged/usr/bin/git $(find /staged/usr/libexec/git-core -maxdepth 1 -type f); do \
      readelf -d "$f" 2>/dev/null | grep -q libpcre2 || continue; \
      patchelf --set-rpath /usr/local/lib/scion-git "$f"; \
      patched=$((patched + 1)); \
    done; \
    echo "patched ${patched} binaries with RUNPATH=/usr/local/lib/scion-git"; \
    test "${patched}" -gt 0 \
      || { echo "FAIL: no binary declared libpcre2 as NEEDED — either the \
vendored libpcre2 is now unnecessary (drop it) or the scan is broken"; exit 1; }; \
    readelf -d /staged/usr/bin/git | grep -qE 'RPATH|RUNPATH' \
      || { echo "FAIL: /usr/bin/git has no RUNPATH after patchelf"; exit 1; }

FROM ${BASE_IMAGE}

USER root

# Remove the ubuntu user (UID 1000) to avoid conflict with the scion user
# that scion-base creates at the same UID.
RUN userdel -r ubuntu 2>/dev/null || true && \
    groupdel ubuntu 2>/dev/null || true

# Install packages that scion-base and harnesses expect but the thick base lacks.
# tmux: required by sandbox entrypoint (buildEntrypoint in cloudrun_sandbox_runtime.go)
# for agent session management. Without it, every sandbox dies on launch (exit 127).
# libcurl4t64 is required by the vendored git's HTTPS helper (see below).
RUN apt-get update && apt-get install -y --no-install-recommends \
    zsh \
    fzf \
    gh \
    ripgrep \
    tmux \
    ca-certificates \
    zlib1g \
    libcurl4t64 \
    && rm -rf /var/lib/apt/lists/*

# --- Vendored git (Chainguard) ---------------------------------------------
# Mirrors core-base so both chains get the same git from the same place. See
# core-base/Dockerfile for the full rationale; the short version:
#   - helpers MUST go to /usr/libexec/git-core (compiled-in GIT_EXEC_PATH);
#   - the binary MUST overwrite /usr/bin/git, because Chainguard ships
#     /usr/libexec/git-core/git as a symlink to ../../bin/git and git re-execs
#     itself through it. This base DOES ship its own /usr/bin/git (2.43), so
#     installing ours anywhere else leaves that symlink pointing at the old
#     binary — `git --version` says 2.55.0 while subcommands fail with
#     "fatal: unknown repository extension found: relativeworktrees".
#     This is not hypothetical: it is exactly what this image did before.
# Must stay after apt-get so nothing reinstates the distro git over it.
# Ubuntu 24.04 ships glibc 2.39, clearing Chainguard's 2.38 floor — by a single
# release, so any future change of BASE_IMAGE needs to re-check this.
COPY --from=git-patched /staged/usr/bin/git          /usr/bin/git
COPY --from=git-patched /staged/usr/libexec/git-core /usr/libexec/git-core
COPY --from=git-patched /staged/usr/share/git-core   /usr/share/git-core
COPY --from=git-patched /staged/usr/local/lib/scion-git /usr/local/lib/scion-git

# Same build-time assertions as core-base: PATH precedence, GIT_EXEC_PATH,
# shared-library resolution, re-exec target, and the 2.47.0 worktree gate.
RUN set -eu; \
    test "$(command -v git)" = /usr/bin/git \
      || { echo "FAIL: vendored git is shadowed on PATH by $(command -v git)"; exit 1; }; \
    git --version; \
    test -x /usr/libexec/git-core/git-remote-https \
      || { echo "FAIL: git helpers missing from compiled-in GIT_EXEC_PATH"; exit 1; }; \
    if ldd /usr/libexec/git-core/git-remote-https | grep -q "not found"; then \
      echo "FAIL: unresolved shared libraries in git-remote-https:"; \
      ldd /usr/libexec/git-core/git-remote-https | grep "not found"; exit 1; \
    fi; \
    test "$(/usr/libexec/git-core/git --version)" = "$(git --version)" \
      || { echo "FAIL: git re-execs into a different binary via GIT_EXEC_PATH: \
$(/usr/libexec/git-core/git --version) vs $(git --version)"; exit 1; }; \
    git init -q /tmp/_gitcheck \
      && git -C /tmp/_gitcheck -c user.email=build@scion.invalid -c user.name=build \
           commit -q --allow-empty -m check \
      && git -C /tmp/_gitcheck worktree add --relative-paths -q /tmp/_gitcheck_wt HEAD \
      || { echo "FAIL: 'git worktree add --relative-paths' did not work end-to-end \
(scion requires git >= 2.47.0 -- see pkg/util/git.go CheckGitVersion)"; exit 1; }; \
    test -z "${LD_LIBRARY_PATH:-}" \
      || { echo "FAIL: LD_LIBRARY_PATH is set to '${LD_LIBRARY_PATH}'. The \
vendored libpcre2 is reached via a RUNPATH on the git binaries; an image-wide \
LD_LIBRARY_PATH would impose it on every process in the container"; exit 1; }; \
    test -z "$(git --version 2>&1 1>/dev/null)" \
      || { echo "FAIL: git writes to stderr on a plain --version, so the \
dynamic loader is unhappy and agent output will be polluted: \
$(git --version 2>&1 1>/dev/null)"; exit 1; }; \
    rm -rf /tmp/_gitcheck /tmp/_gitcheck_wt

# Create the npm-global directory that scion-base expects to chown.
RUN mkdir -p /usr/local/share/npm-global
