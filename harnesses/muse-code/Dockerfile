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

# Image is immutable; update muse by rebuilding, not by runtime auto-update.
ENV MUSE_NO_AUTO_UPDATE=1

# Installer is bash-only ([[ ]]); download to a file rather than piping to
# bash so a failed curl aborts the build instead of silently running nothing.
# Installs straight into /usr/local/bin (on PATH for scion) via the
# installer's supported MUSE_INSTALL_DIR override.
RUN curl -fsSL -o /tmp/muse-install.sh https://dev.meta.ai/install.sh && \
    MUSE_INSTALL_DIR=/usr/local/bin MUSE_NO_MODIFY_PATH=1 bash /tmp/muse-install.sh && \
    rm /tmp/muse-install.sh && \
    muse --version

# Create config and data directories owned by agent user
RUN mkdir -p /home/scion/.config/muse \
             /home/scion/.local/share/muse \
             /home/scion/.muse/skills && \
    chown -R scion:scion /home/scion/.config \
                         /home/scion/.local \
                         /home/scion/.muse

CMD ["muse"]
