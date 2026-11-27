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

# Verified Grok Build CLI version; keep in sync with GrokBuildVerifiedVersions
# in pkg/sciontool/telemetry/usage.go. Passing --build-arg GROK_VERSION=latest
# still works for ad-hoc testing. See
# docs-site/src/content/docs/contributing/harness-cli-version-bump.md before
# bumping.
ARG GROK_VERSION=1.0.92

# Pre-seed grok config directories and seed config.
RUN mkdir -p /home/scion/.grok/hooks \
             /home/scion/.grok/skills \
             /home/scion/.grok/plugins \
             /home/scion/.grok/sessions \
             /home/scion/.grok/agents && \
    chown -R scion:scion /home/scion/.grok

COPY --chown=scion:scion home/.grok/config.toml /home/scion/.grok/config.toml

# Install grok CLI binary.
# The official install script downloads to ~/.grok/bin/ and symlinks into
# /usr/local/bin/.  We download the script to a file rather than piping curl to
# bash (Docker's /bin/sh lacks pipefail), pass GROK_VERSION as its positional
# version argument unless explicitly set to "latest", copy the real binary
# (cp -L dereferences) so the image is self-contained after removing ~/.grok,
# and run `grok --version` to record the installed version in the build log.
# Do NOT switch back to USER scion — sciontool init handles the user switch.
USER root
RUN curl -fsSL -o /tmp/grok-install.sh https://x.ai/cli/install.sh \
    && if [ -z "${GROK_VERSION}" ] || [ "${GROK_VERSION}" = "latest" ]; then \
         bash /tmp/grok-install.sh; \
       else \
         bash /tmp/grok-install.sh "${GROK_VERSION}"; \
       fi \
    && rm -f /tmp/grok-install.sh /usr/local/bin/grok /usr/local/bin/agent \
    && cp -fL /root/.grok/bin/grok /usr/local/bin/grok \
    && chmod +x /usr/local/bin/grok \
    && rm -rf /root/.grok \
    && /usr/local/bin/grok --version

CMD ["grok"]
