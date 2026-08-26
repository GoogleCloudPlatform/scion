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
#
# Architecture: amd64 only (Cloud Workstations base has no arm64 variant).

ARG BASE_IMAGE=us-central1-docker.pkg.dev/cloud-workstations-images/predefined/base:latest
FROM ${BASE_IMAGE}

USER root

# Remove the ubuntu user (UID 1000) to avoid conflict with the scion user
# that scion-base creates at the same UID.
RUN userdel -r ubuntu 2>/dev/null || true && \
    groupdel ubuntu 2>/dev/null || true

# Install packages that scion-base and harnesses expect but the thick base lacks.
# tmux: required by sandbox entrypoint (buildEntrypoint in cloudrun_sandbox_runtime.go)
# for agent session management. Without it, every sandbox dies on launch (exit 127).
RUN apt-get update && apt-get install -y --no-install-recommends \
    zsh \
    fzf \
    gh \
    ripgrep \
    tmux \
    && rm -rf /var/lib/apt/lists/*

# Create the npm-global directory that scion-base expects to chown.
RUN mkdir -p /usr/local/share/npm-global
