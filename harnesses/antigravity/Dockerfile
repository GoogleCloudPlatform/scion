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

USER root

RUN mkdir -p /home/scion/.gemini/antigravity-cli \
  && mkdir -p /home/scion/.agents \
  && chown -R scion:scion /home/scion/.gemini /home/scion/.agents

# Fetch the latest CLI via the auto-updater manifest for the current platform.
# This trades build reproducibility for staying current, but verifies the
# download integrity via SHA-512 checksum from the manifest.
RUN PLATFORM="linux_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')" \
  && MANIFEST=$(curl -fsSL "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/${PLATFORM}.json") \
  && URL=$(printf '%s\n' "$MANIFEST" | python3 -c "import sys, json; print(json.load(sys.stdin)['url'])") \
  && SHA512=$(printf '%s\n' "$MANIFEST" | python3 -c "import sys, json; print(json.load(sys.stdin)['sha512'])") \
  && curl -fsSL -o /tmp/cli.tar.gz "$URL" \
  && printf '%s  /tmp/cli.tar.gz\n' "$SHA512" | sha512sum -c - \
  && tar -xzf /tmp/cli.tar.gz -C /tmp \
  && mv /tmp/antigravity /usr/local/bin/agy \
  && chmod +x /usr/local/bin/agy \
  && rm /tmp/cli.tar.gz

CMD ["agy"]
