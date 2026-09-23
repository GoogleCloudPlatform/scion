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

# Scion Base Image - Adds sciontool and scion user on top of core-base
ARG BASE_IMAGE
FROM ${BASE_IMAGE} AS base

# Builder stage for sciontool compilation
FROM base AS builder
USER root
WORKDIR /src
ENV PATH="/usr/local/go/bin:${PATH}"
ENV CGO_ENABLED=1

# Build arguments for version info
ARG GIT_COMMIT
ARG BUILD_TIME

# Copy go.mod and go.sum first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build scion and sciontool
COPY cmd/ ./cmd/
COPY harnesses/ ./harnesses/
COPY pkg/ ./pkg/
COPY proto/ ./proto/
COPY resources/ ./resources/
COPY web/*.go ./web/

# Build with version info injected via ldflags
# VCS stamping is disabled since .git is not in the build context;
# commit info is passed explicitly via the GIT_COMMIT build arg
RUN BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    SCION_PKG="github.com/GoogleCloudPlatform/scion/pkg/version" && \
    SCIONTOOL_PKG="github.com/GoogleCloudPlatform/scion/cmd/sciontool/commands" && \
    SCION_LDFLAGS="-s -w -X ${SCION_PKG}.BuildTime=${BUILD_TIME}" && \
    SCIONTOOL_LDFLAGS="-s -w -X ${SCIONTOOL_PKG}.BuildTime=${BUILD_TIME}" && \
    if [ -n "${GIT_COMMIT}" ]; then \
        SCION_LDFLAGS="${SCION_LDFLAGS} -X ${SCION_PKG}.Commit=${GIT_COMMIT}"; \
        SCIONTOOL_LDFLAGS="${SCIONTOOL_LDFLAGS} -X ${SCIONTOOL_PKG}.Commit=${GIT_COMMIT}"; \
    fi && \
    go build -buildvcs=false -tags no_embed_web -ldflags="${SCION_LDFLAGS}" -o /usr/local/bin/scion ./cmd/scion/ && \
    go build -buildvcs=false -tags no_embed_web -ldflags="${SCIONTOOL_LDFLAGS}" -o /usr/local/bin/sciontool ./cmd/sciontool/

# Final stage
FROM base
USER root

# Copy scion and sciontool from builder
COPY --from=builder /usr/local/bin/scion /usr/local/bin/scion
COPY --from=builder /usr/local/bin/sciontool /usr/local/bin/sciontool

# Set up gh CLI wrapper for GitHub App token injection.
# Move the real gh binary aside and install a shell wrapper that delegates
# through sciontool gh-wrapper, which reads the latest token from the
# refreshable token file before exec-ing the real binary.
#
# Resolve gh via PATH rather than hard-coding /usr/bin/gh. This used to test
# `[ -f /usr/bin/gh ]`, which silently did nothing on any base that installed
# gh somewhere else — and core-base did exactly that when it switched from the
# apt package to the release tarball, disabling token injection on that whole
# chain with no error anywhere. sciontool's own lookup already searches both
# /usr/bin/gh.real and /usr/local/bin/gh.real (cmd/sciontool/commands/
# gh_wrapper.go), so the build side is what was too narrow.
#
# Not finding gh at all is a hard error: falling through leaves agents running
# unauthenticated gh, which fails later and further away.
RUN set -eu; \
    gh_path="$(command -v gh || true)"; \
    if [ -z "$gh_path" ]; then \
      echo "FAIL: gh not found on PATH; the base image must provide it"; \
      echo "      (see image-build/lib/verify-base-contract.sh)"; \
      exit 1; \
    fi; \
    mv "$gh_path" "${gh_path}.real"; \
    printf '#!/bin/sh\nexec sciontool gh-wrapper "$@"\n' > "$gh_path"; \
    chmod +x "$gh_path"

# Install whoami shim that delegates to scion whoami.
# The shim is placed in /usr/local/bin so it shadows /usr/bin/whoami via PATH
# ordering. The system whoami remains accessible at its original path.
# Guard: only delegate when Scion agent env vars are set. Otherwise fall back
# to the real /usr/bin/whoami to preserve Unix-username detection (e.g.
# ExecAsUserCmd) and avoid an infinite fork loop when scion whoami itself
# calls exec.LookPath("whoami").
RUN printf '#!/bin/sh\nif [ -n "${SCION_AGENT_SLUG:-}" ] || [ -n "${SCION_AGENT_NAME:-}" ]; then\n    exec scion whoami "$@"\nfi\nexec /usr/bin/whoami "$@"\n' > /usr/local/bin/whoami && \
    chmod +x /usr/local/bin/whoami

# Create dev-override bin directory at highest PATH priority.
# When SCION_DEV_BINARIES is set, this directory is bind-mounted with
# locally-built binaries, allowing rapid iteration without image rebuilds.
RUN mkdir -p /opt/scion/bin
ENV PATH="/opt/scion/bin:${PATH}"

# Create scion user with passwordless sudo
RUN useradd -m -s /bin/zsh -u 1000 scion && \
    echo "scion ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/scion && \
    chmod 0440 /etc/sudoers.d/scion

# Precreate shared Scion home state so mounted file secrets and runtime
# config writes do not force root-owned parent directories at startup.
RUN mkdir -p /home/scion/.scion && \
    chown -R scion:scion /home/scion

# Set ownership for npm global directory
# TODO: Consider making npm-global world read-write (chmod) instead of chown
# to scion. This would decouple it from the scion UID and avoid any ownership
# concerns when the UID is remapped at container start.
RUN chown -R scion:scion /usr/local/share/npm-global

# Note: Container starts as root to allow sciontool init to adjust
# the scion user's UID/GID to match the host user for proper file
# permissions on mounted volumes. The child process drops privileges.

# Set sciontool as the container entrypoint (PID 1)
# The -- separator ensures everything after it is treated as the child command
ENTRYPOINT ["sciontool", "init", "--"]

# Default command (can be overridden at runtime)
CMD ["bash"]
