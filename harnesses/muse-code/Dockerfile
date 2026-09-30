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

# Version-pinning ARG (defaulting to latest; enables version-locked CI builds).
# Note: The install script only supports a "channel" (muse-stable/muse-canary),
# not an exact version pin, so this ARG is currently unused by the RUN below.
ARG MUSE_VERSION=latest

# Install Muse Code CLI directly into /usr/local/bin via the installer's
# supported MUSE_INSTALL_DIR override, so it's on PATH for both the root
# build user and the scion runtime user without any extra copy step.
#
# The installer (https://dev.meta.ai/install.sh) is bash-only (uses `[[ ]]`)
# and must be piped to bash, not sh: under sh, `[[` fails ("[[: not found"),
# which trips the script's `|| die "refusing to install into $install_dir"`
# fallback. That message looks like a root/home-directory rejection but
# isn't -- there is no such check; it's purely a shell-dialect mismatch.
#
# The installer places several files alongside the launcher in the install
# dir (a launcher script plus the real muse-bin-<version> binary and state
# files); installing straight into /usr/local/bin keeps them together and
# avoids leaving multi-hundred-MB orphans under /root/.local/bin.
RUN MUSE_INSTALL_DIR=/usr/local/bin \
    MUSE_NO_MODIFY_PATH=1 \
    curl -fsSL https://dev.meta.ai/install.sh | bash && \
    muse --version

# Create config and data directories owned by agent user
RUN mkdir -p /home/scion/.config/muse \
             /home/scion/.local/share/muse \
             /home/scion/.muse/skills && \
    chown -R scion:scion /home/scion/.config \
                         /home/scion/.local \
                         /home/scion/.muse

CMD ["muse"]
