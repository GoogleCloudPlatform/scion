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

FROM node:20-slim AS builder
ENV DEBIAN_FRONTEND=noninteractive

# Install build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    wget \
    curl \
    make \
    gcc \
    libssl-dev \
    libcurl4-gnutls-dev \
    libexpat1-dev \
    zlib1g-dev \
    gettext \
    ncurses-term \
    ca-certificates \
    gnupg \
    && rm -rf /var/lib/apt/lists/*

# Install Go
ARG GO_VERSION=1.25.4
RUN ARCH=$(dpkg --print-architecture) && \
    case "${ARCH}" in \
      amd64) GO_ARCH='linux-amd64' ;; \
      arm64) GO_ARCH='linux-arm64' ;; \
      *) echo "Unsupported architecture: ${ARCH}"; exit 1 ;; \
    esac && \
    curl -L "https://go.dev/dl/go${GO_VERSION}.${GO_ARCH}.tar.gz" -o go.tar.gz && \
    tar -C /usr/local -xzf go.tar.gz && \
    rm go.tar.gz

# Download GitHub CLI keyring
RUN mkdir -p -m 755 /etc/apt/keyrings && \
    wget -qO- https://cli.github.com/packages/githubcli-archive-keyring.gpg > /etc/apt/keyrings/githubcli-archive-keyring.gpg

# Download gcsfuse GPG key
RUN curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg

# Install Git from source
WORKDIR /opt
RUN wget https://www.kernel.org/pub/software/scm/git/git-2.52.0.tar.gz
RUN tar -xvf git-2.52.0.tar.gz
WORKDIR /opt/git-2.52.0/
RUN make prefix=/usr/local all
RUN make prefix=/usr/local install

# Final Stage
FROM node:20-slim
USER root

# Remove default node user to avoid UID 1000 collision with host user
RUN if getent passwd node; then userdel -r node; fi

COPY --from=builder /etc/apt/keyrings/githubcli-archive-keyring.gpg /etc/apt/keyrings/githubcli-archive-keyring.gpg
COPY --from=builder /usr/share/keyrings/cloud.google.gpg /usr/share/keyrings/cloud.google.gpg
RUN echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list

# Setup gcsfuse repository
RUN . /etc/os-release && \
    echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt gcsfuse-${VERSION_CODENAME} main" | tee /etc/apt/sources.list.d/gcsfuse.list

# Install runtime dependencies for Git and common tools
RUN apt-get update && apt-get install -y --no-install-recommends \
    tmux \
    ca-certificates \
    libcurl4-gnutls-dev \
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
    gh \
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
    python3-venv \
    lsb-release \
    chromium \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Setup GCS-FUSE
RUN GCSFUSE_REPO=gcsfuse-`lsb_release -c -s` && echo "deb [signed-by=/usr/share/keyrings/cloud.google.asc] https://packages.cloud.google.com/apt $GCSFUSE_REPO main" | sudo tee /etc/apt/sources.list.d/gcsfuse.list
RUN curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | sudo tee /usr/share/keyrings/cloud.google.asc

RUN apt-get update && apt-get install -y --no-install-recommends \
    gcsfuse \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Copy Go and Git from builder
COPY --from=builder /usr/local/go /usr/local/go
COPY --from=builder /usr/local/bin/git /usr/local/bin/git
COPY --from=builder /usr/local/libexec/git-core /usr/local/libexec/git-core
COPY --from=builder /usr/local/share/git-core /usr/local/share/git-core

# Install gcloud CLI
RUN echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" | tee -a /etc/apt/sources.list.d/google-cloud-sdk.list && \
    apt-get update && apt-get install -y google-cloud-cli && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Setup npm global directory
RUN mkdir -p /usr/local/share/npm-global
ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=/usr/local/share/npm-global/bin:/usr/local/go/bin:/usr/local/gcloud/google-cloud-sdk/bin:$PATH
ENV LC_ALL=C.UTF-8
ENV EDITOR=nano
ENV VISUAL=nano

# Install Chrome DevTools MCP server globally
RUN npm install -g chrome-devtools-mcp

# Default to bash - no scion-specific entrypoint
CMD ["bash"]
