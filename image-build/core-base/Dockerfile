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

# Core Base Image - Foundation tools only (Go, Git, unix tools, GCS fuse)
# No scion-specific tooling or user setup

# GitHub CLI version — bump this variable to upgrade gh across all images.
ARG GH_VERSION=2.100.0

# Git is vendored from the Chainguard git image rather than compiled from
# source. Chainguard tracks upstream closely (currently 2.55.0) and rebuilds on
# CVE disclosure, which removes a multi-minute `make` from every core-base
# build. See the "Vendored git" block in the final stage for the two layout
# constraints this imposes.
#
# NOTE: `:latest` is a floating tag. CI should override this with a digest
# (`--build-arg GIT_IMAGE=chainguard/git@sha256:...`) so builds are
# reproducible, and refresh that digest on a schedule — a vendored binary stops
# receiving Chainguard's rebuild cadence the moment it is copied out.
ARG GIT_IMAGE=chainguard/git:latest

# Bun runtime source
FROM oven/bun:latest AS bun-source

# Git source (vendored binary + helpers, see final stage)
FROM ${GIT_IMAGE} AS git-source

# --- Stage the vendored git and localize its libpcre2 ------------------------
# Chainguard's git links against a libpcre2 carrying versioned symbols that
# Debian's build does not have. Against the system copy git still works, but
# every single invocation prints
#   git: /lib/x86_64-linux-gnu/libpcre2-8.so.0: no version information
#   available (required by git)
# to stderr — harmless, but it pollutes output that agents parse. So we ship
# Chainguard's libpcre2 alongside.
#
# The override is applied as a RUNPATH on the five binaries that actually need
# it, NOT as a global `ENV LD_LIBRARY_PATH`. An image-wide LD_LIBRARY_PATH
# forces *every* process in the container — node, python, any agent-built
# binary — to prefer Chainguard's libpcre2 over the system one, which is a much
# larger blast radius than the problem being solved.
#
# patchelf and binutils live only in this stage, so neither reaches the final
# image.
FROM node:24-trixie-slim AS git-patched
RUN apt-get update && apt-get install -y --no-install-recommends \
    patchelf \
    binutils \
    && rm -rf /var/lib/apt/lists/*
COPY --from=git-source /usr/bin/git          /staged/usr/bin/git
COPY --from=git-source /usr/libexec/git-core /staged/usr/libexec/git-core
COPY --from=git-source /usr/share/git-core   /staged/usr/share/git-core
COPY --from=git-source /usr/lib/libpcre2-8.so.0 /staged/usr/local/lib/scion-git/libpcre2-8.so.0
# `-type f` matters: git-core is ~151 symlinks around 4 real binaries, and
# patchelf must not be pointed at the symlinks.
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

FROM node:24-trixie-slim AS builder
ENV DEBIAN_FRONTEND=noninteractive

# Install build dependencies. The former git-from-source toolchain (make, gcc,
# libssl-dev, libcurl4-gnutls-dev, libexpat1-dev, zlib1g-dev, gettext,
# ncurses-term) is no longer needed here — this stage now only fetches Go and
# dearmors the gcsfuse signing key.
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    ca-certificates \
    gnupg \
    && rm -rf /var/lib/apt/lists/*

# Install Go
ARG GO_VERSION=1.26.1
RUN ARCH=$(dpkg --print-architecture) && \
    case "${ARCH}" in \
      amd64) GO_ARCH='linux-amd64' ;; \
      arm64) GO_ARCH='linux-arm64' ;; \
      *) echo "Unsupported architecture: ${ARCH}"; exit 1 ;; \
    esac && \
    curl -L "https://go.dev/dl/go${GO_VERSION}.${GO_ARCH}.tar.gz" -o go.tar.gz && \
    tar -C /usr/local -xzf go.tar.gz && \
    rm go.tar.gz

# Download gcsfuse GPG key
RUN curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg

# Final Stage
FROM node:24-trixie-slim
USER root

# Remove default node user to avoid UID 1000 collision with host user
RUN if getent passwd node; then userdel -r node; fi

COPY --from=builder /usr/share/keyrings/cloud.google.gpg /usr/share/keyrings/cloud.google.gpg

# Setup gcsfuse repository
RUN . /etc/os-release && \
    echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt gcsfuse-${VERSION_CODENAME} main" | tee /etc/apt/sources.list.d/gcsfuse.list

# Install runtime dependencies for Git and common tools.
# libcurl4t64 (not libcurl4-gnutls-dev) is required: the vendored Chainguard
# git links against libcurl.so.4, and without it git-remote-https fails to load
# with "error while loading shared libraries: libcurl.so.4".
RUN apt-get update && apt-get install -y --no-install-recommends \
    tmux \
    ca-certificates \
    libcurl4t64 \
    libexpat1 \
    zlib1g \
    python3 \
    make \
    g++ \
    man-db \
    curl \
    wget \
    dnsutils \
    less \
    jq \
    bc \
    unzip \
    rsync \
    ripgrep \
    procps \
    psmisc \
    lsof \
    socat \
    sudo \
    fzf \
    zsh \
    gnupg2 \
    iptables \
    ipset \
    iproute2 \
    aggregate \
    nano \
    vim \
    openssh-client \
    python3-venv \
    lsb-release \
    chromium \
    dbus-x11 \
    gnome-keyring \
    libsecret-1-0 \
    libsecret-tools \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Setup GCS-FUSE
RUN GCSFUSE_REPO=gcsfuse-`lsb_release -c -s` && echo "deb [signed-by=/usr/share/keyrings/cloud.google.asc] https://packages.cloud.google.com/apt $GCSFUSE_REPO main" | sudo tee /etc/apt/sources.list.d/gcsfuse.list
RUN curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | sudo tee /usr/share/keyrings/cloud.google.asc

RUN apt-get update && apt-get install -y --no-install-recommends \
    gcsfuse \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Copy Go from builder
COPY --from=builder /usr/local/go /usr/local/go

# --- Vendored git (Chainguard) ---------------------------------------------
# Three constraints, all learned the hard way:
#
# 1. Helpers MUST land in /usr/libexec/git-core, not /usr/local/libexec.
#    That path is compiled into the Chainguard binary as GIT_EXEC_PATH. Putting
#    them anywhere else yields the very misleading "git: 'remote-https' is not
#    a git command" / "fatal: remote helper 'https' aborted session", which
#    reads like missing HTTPS support rather than a path error.
#
# 2. The binary MUST land at /usr/bin/git specifically. Chainguard ships
#    /usr/libexec/git-core/git as a relative symlink to ../../bin/git, and git
#    re-execs itself through that path for internal subcommands (auto
#    maintenance, etc). Install the binary anywhere else (e.g.
#    /usr/local/bin/git) on a base that already has a distro git and the
#    symlink silently resolves to the OLD binary. The failure is baffling:
#    `git --version` reports 2.55.0 while a subcommand dies with
#    "error: unknown option \`detach'" followed by
#    "fatal: unknown repository extension found: relativeworktrees".
#
# 3. This COPY must stay AFTER every apt-get above. Several packages pull in
#    the distro git as a dependency; landing our binary last is what makes it
#    win — and per (2) it must overwrite /usr/bin/git, not sit beside it.
#
# Chainguard builds against a newer glibc, so the base image needs glibc >= 2.38
# (trixie ships 2.41). This is now a real constraint on future base changes:
# bookworm (2.36) would fail at exec with "GLIBC_2.38 not found".
# Taken from git-patched, not git-source: those binaries carry a RUNPATH that
# points at the vendored libpcre2 below, so no image-wide LD_LIBRARY_PATH is
# needed. See the git-patched stage for why.
COPY --from=git-patched /staged/usr/bin/git          /usr/bin/git
COPY --from=git-patched /staged/usr/libexec/git-core /usr/libexec/git-core
COPY --from=git-patched /staged/usr/share/git-core   /usr/share/git-core
COPY --from=git-patched /staged/usr/local/lib/scion-git /usr/local/lib/scion-git

# Fail the build, not the agent, if the vendored git is wrong. Each check maps
# to one real way this lift breaks: the binary being shadowed on PATH, helpers
# landing outside GIT_EXEC_PATH, a missing shared library, git re-execing into
# a different (older) binary via the exec-path symlink, and the 2.47.0 feature
# gate itself. All are network-free.
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

# Install GitHub CLI from official release (pinned version for reproducibility).
# Bump GH_VERSION at the top of this file to upgrade.
# Uses the tar.gz archive instead of .deb to avoid dpkg dependency issues
# (the vendored git above is not tracked by dpkg).
ARG GH_VERSION
RUN ARCH=$(dpkg --print-architecture) && \
    curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${ARCH}.tar.gz" -o /tmp/gh.tar.gz && \
    tar -xzf /tmp/gh.tar.gz -C /tmp && \
    mv /tmp/gh_${GH_VERSION}_linux_${ARCH}/bin/gh /usr/local/bin/gh && \
    rm -rf /tmp/gh.tar.gz /tmp/gh_${GH_VERSION}_linux_${ARCH}

# Copy Bun from bun-source
COPY --from=bun-source /usr/local/bin/bun /usr/local/bin/bun

# Install gcloud CLI
RUN echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" | tee -a /etc/apt/sources.list.d/google-cloud-sdk.list && \
    apt-get update && apt-get install -y google-cloud-cli && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Install kubectl
RUN ARCH=$(dpkg --print-architecture) && \
    curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/${ARCH}/kubectl" && \
    install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl && \
    rm kubectl

# Install golangci-lint
RUN ARCH=$(dpkg --print-architecture) && \
    curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b /usr/local/bin

# Setup npm global directory
RUN mkdir -p /usr/local/share/npm-global
ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global

# Optional npm mirror. Corporate networks commonly block registry.npmjs.org;
# point this at an internal proxy (Artifactory, Nexus, Verdaccio) to build
# behind one. Defaults to the public registry, so behaviour is unchanged when
# unset. Inherited by scion-base, every harness image, and agents at runtime.
ARG NPM_REGISTRY=https://registry.npmjs.org/
ENV NPM_CONFIG_REGISTRY=${NPM_REGISTRY}

# Optional Python package index, for the same reason as NPM_REGISTRY above:
# corporate networks commonly block pypi.org. Point this at an internal proxy
# (Artifactory, Nexus, devpi). Defaults to the public index, so behaviour is
# unchanged when unset. Inherited by scion-base, every harness image, and
# agents at runtime. Credentials do NOT belong here - a value baked into ENV
# persists in the image; use the pipconf build secret instead.
ARG PIP_INDEX_URL=https://pypi.org/simple
ENV PIP_INDEX_URL=${PIP_INDEX_URL}
ENV PATH=/usr/local/share/npm-global/bin:/usr/local/go/bin:/usr/local/gcloud/google-cloud-sdk/bin:$PATH
ENV LC_ALL=C.UTF-8
ENV EDITOR=nano
ENV VISUAL=nano

# Install Chrome DevTools MCP server globally
RUN --mount=type=secret,id=npmrc,target=/root/.npmrc,required=false \
    npm install -g chrome-devtools-mcp

# Install playwright CLI
RUN --mount=type=secret,id=npmrc,target=/root/.npmrc,required=false \
    npm install -g @playwright/cli@latest

# Default to bash - no scion-specific entrypoint
CMD ["bash"]
