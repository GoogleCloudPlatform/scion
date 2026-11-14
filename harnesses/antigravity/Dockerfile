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

# Install Antigravity CLI via its upstream installer into /usr/local/bin:
# - Download to a file instead of piping curl to bash so a failed download
#   aborts under Docker's non-pipefail /bin/sh rather than running nothing.
# - Remove any pre-existing /usr/local/bin/agy first (install.sh exits 0
#   without updating when the target binary already exists) and isolate HOME
#   so staging files and shell-rc PATH edits do not touch /root.
# - Run `agy --version` to record the installed version in the build log and
#   fail the build if the binary is missing or broken.
RUN rm -f /usr/local/bin/agy \
  && tmp_home="$(mktemp -d)" \
  && curl -fsSL -o /tmp/agy-install.sh https://antigravity.google/cli/install.sh \
  && HOME="${tmp_home}" bash /tmp/agy-install.sh --dir /usr/local/bin \
  && rm -rf /tmp/agy-install.sh "${tmp_home}" \
  && /usr/local/bin/agy --version

RUN printf 'if [ -z "$DBUS_SESSION_BUS_ADDRESS" ]; then\n    eval $(dbus-launch --sh-syntax)\n    export DBUS_SESSION_BUS_ADDRESS\nfi\nif ! pgrep gnome-keyring-daemon >/dev/null; then\n    eval $(echo "test" | gnome-keyring-daemon --unlock 2>/dev/null)\n    gnome-keyring-daemon --start --components=secrets,pkcs11,ssh >/dev/null 2>&1\nfi\n' >> /etc/bash.bashrc

CMD ["agy"]
