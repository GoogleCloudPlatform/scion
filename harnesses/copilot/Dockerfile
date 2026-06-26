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

ARG BASE_IMAGE
FROM ${BASE_IMAGE}

ARG COPILOT_CLI_VERSION=latest

RUN mkdir -p /home/scion/.copilot /home/scion/.copilot/skills \
             /home/scion/.github && \
    chown -R scion:scion /home/scion/.copilot /home/scion/.github

# Install copilot binary (Alpine/musl variant for scion-base).
# The binary is a self-contained Node.js SEA — no npm/node dependency needed.
RUN ARCH=$(uname -m | sed 's/x86_64/x64/;s/aarch64/arm64/') && \
    if [ "$COPILOT_CLI_VERSION" = "latest" ]; then \
      URL="https://github.com/github/copilot-cli/releases/latest/download/copilot-linuxmusl-${ARCH}.tar.gz"; \
    else \
      URL="https://github.com/github/copilot-cli/releases/download/v${COPILOT_CLI_VERSION}/copilot-linuxmusl-${ARCH}.tar.gz"; \
    fi && \
    curl -fsSL "$URL" | tar xz -C /usr/local/bin && \
    chmod +x /usr/local/bin/copilot

CMD ["copilot"]
