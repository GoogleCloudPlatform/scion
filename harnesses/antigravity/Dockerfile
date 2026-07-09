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

ARG TARGETARCH

USER root

RUN mkdir -p /home/scion/.gemini/antigravity-cli \
  && mkdir -p /home/scion/.agents \
  && chown -R scion:scion /home/scion/.gemini /home/scion/.agents

# Pin the CLI to a specific release version.
ARG AGY_VERSION=1.1.0

RUN ARCH=$(case "${TARGETARCH:-amd64}" in amd64) echo "x64";; arm64) echo "arm64";; *) echo "${TARGETARCH}";; esac) \
  && curl -fsSL -o /tmp/cli.tar.gz \
       "https://github.com/google-antigravity/antigravity-cli/releases/download/${AGY_VERSION}/agy_cli_linux_${ARCH}.tar.gz" \
  && tar -xzf /tmp/cli.tar.gz -C /tmp antigravity \
  && mv /tmp/antigravity /usr/local/bin/agy \
  && chmod +x /usr/local/bin/agy \
  && rm /tmp/cli.tar.gz

RUN printf 'if [ -z "$DBUS_SESSION_BUS_ADDRESS" ]; then\n    eval $(dbus-launch --sh-syntax)\n    export DBUS_SESSION_BUS_ADDRESS\nfi\nif ! pgrep gnome-keyring-daemon >/dev/null; then\n    eval $(echo "test" | gnome-keyring-daemon --unlock 2>/dev/null)\n    gnome-keyring-daemon --start --components=secrets,pkcs11,ssh >/dev/null 2>&1\nfi\n' >> /etc/bash.bashrc

CMD ["agy"]
