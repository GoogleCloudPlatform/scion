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

ARG HERMES_VERSION=

# Install Node.js 22 LTS (required by Hermes for tool execution)
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
  && apt-get install -y --no-install-recommends nodejs \
  && apt-get clean && rm -rf /var/lib/apt/lists/*

# Install ripgrep (required by Hermes for code search)
RUN apt-get update && apt-get install -y --no-install-recommends ripgrep \
  && apt-get clean && rm -rf /var/lib/apt/lists/*

# Install Hermes Agent via pip (available in scion-base)
RUN if [ -n "${HERMES_VERSION}" ]; then \
      pip install --no-cache-dir "hermes-agent==${HERMES_VERSION}"; \
    else \
      pip install --no-cache-dir hermes-agent; \
    fi

RUN mkdir -p /home/scion/.hermes /home/scion/.hermes/skills \
  && chown -R scion:scion /home/scion/.hermes

CMD ["hermes"]
