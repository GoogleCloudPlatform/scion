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

# ── System dependencies ──────────────────────────────────────
USER root

# Install uv (Python package manager used by LHH)
RUN curl -LsSf https://astral.sh/uv/install.sh | sh \
    && ln -sf /root/.local/bin/uv /usr/local/bin/uv \
    && ln -sf /root/.local/bin/uvx /usr/local/bin/uvx

# ── LHH source ───────────────────────────────────────────────
ARG LHH_VERSION=main
RUN git clone --depth 1 --branch ${LHH_VERSION} \
        https://github.com/google/adk-samples.git /tmp/adk-samples \
    && mv /tmp/adk-samples/core/python/long-horizon-harness /opt/lhh \
    && rm -rf /tmp/adk-samples

WORKDIR /opt/lhh

# Install LHH and its dependencies via uv
RUN uv sync --no-dev --frozen 2>/dev/null || uv sync --no-dev

# Install agents-cli (useful for one-shot runs and debugging)
RUN uv tool install google-agents-cli

# ── Scion REPL runner ────────────────────────────────────────
COPY lhh_scion_repl.py /opt/lhh/lhh_scion_repl.py

# ── Permissions ──────────────────────────────────────────────
RUN mkdir -p /home/scion/.lhh \
    && mkdir -p /opt/lhh/.data \
    && chown -R scion:scion /home/scion/.lhh /opt/lhh/.data /opt/lhh

# Expose web UI port
EXPOSE 8080

CMD ["python3", "/opt/lhh/lhh_scion_repl.py"]
