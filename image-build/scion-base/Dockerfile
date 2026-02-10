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
ARG BASE_IMAGE=us-central1-docker.pkg.dev/ptone-misc/public-docker/core-base:latest
FROM ${BASE_IMAGE} AS base

# Builder stage for sciontool compilation
FROM base AS builder
USER root
WORKDIR /src
ENV PATH="/usr/local/go/bin:${PATH}"
ENV CGO_ENABLED=1

# Build arguments for version info
# GIT_COMMIT: If provided, overrides Go's VCS stamping. If empty, falls back to vcs.revision from .git
ARG GIT_COMMIT
ARG BUILD_TIME

# Copy go.mod and go.sum first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy .git for Go's VCS stamping (enables debug.ReadBuildInfo fallback)
# This allows vcs.revision to be embedded when GIT_COMMIT isn't passed
COPY .git/ ./.git/

# Copy source and build scion and sciontool
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/

# Build with version info injected via ldflags
# BUILD_TIME is computed at build time if not provided
# GIT_COMMIT is only added to ldflags if provided; otherwise Go's VCS stamping handles it via .git
RUN BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    SCION_PKG="github.com/ptone/scion-agent/pkg/version" && \
    SCIONTOOL_PKG="github.com/ptone/scion-agent/cmd/sciontool/commands" && \
    SCION_LDFLAGS="-s -w -X ${SCION_PKG}.BuildTime=${BUILD_TIME}" && \
    SCIONTOOL_LDFLAGS="-s -w -X ${SCIONTOOL_PKG}.BuildTime=${BUILD_TIME}" && \
    if [ -n "${GIT_COMMIT}" ]; then \
        SCION_LDFLAGS="${SCION_LDFLAGS} -X ${SCION_PKG}.Commit=${GIT_COMMIT}"; \
        SCIONTOOL_LDFLAGS="${SCIONTOOL_LDFLAGS} -X ${SCIONTOOL_PKG}.Commit=${GIT_COMMIT}"; \
    fi && \
    go build -ldflags="${SCION_LDFLAGS}" -o /usr/local/bin/scion ./cmd/scion/ && \
    go build -ldflags="${SCIONTOOL_LDFLAGS}" -o /usr/local/bin/sciontool ./cmd/sciontool/

# Final stage
FROM base
USER root

# Copy scion and sciontool from builder
COPY --from=builder /usr/local/bin/scion /usr/local/bin/scion
COPY --from=builder /usr/local/bin/sciontool /usr/local/bin/sciontool

# Create scion user with passwordless sudo
RUN useradd -m -s /bin/zsh -u 1000 scion && \
    echo "scion ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/scion && \
    chmod 0440 /etc/sudoers.d/scion

# Set ownership for npm global directory
RUN chown -R scion:scion /usr/local/share/npm-global

# Note: Container starts as root to allow sciontool init to adjust
# the scion user's UID/GID to match the host user for proper file
# permissions on mounted volumes. The child process drops privileges.

# Set sciontool as the container entrypoint (PID 1)
# The -- separator ensures everything after it is treated as the child command
ENTRYPOINT ["sciontool", "init", "--"]

# Default command (can be overridden at runtime)
CMD ["bash"]
