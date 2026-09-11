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

# Install Muse Code CLI
RUN MUSE_NO_MODIFY_PATH=1 \
    curl -fsSL https://dev.meta.ai/install.sh | sh && \
    ln -sf /home/scion/.local/bin/muse /usr/local/bin/muse

# Create config and data directories owned by agent user
RUN mkdir -p /home/scion/.config/muse \
             /home/scion/.local/share/muse \
             /home/scion/.muse/skills && \
    chown -R scion:scion /home/scion/.config \
                         /home/scion/.local \
                         /home/scion/.muse

CMD ["muse"]
