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

# Scion Hub image - extends scion-base to run the centralized hub/broker
# server. The scion binary baked into scion-base already contains the
# server subcommand (`scion server start`); this layer just sets the
# default entrypoint and prepares hub-specific filesystem state.

ARG BASE_IMAGE
FROM ${BASE_IMAGE}

USER root

# Hub state lives under /home/scion/.scion (sqlite, templates, etc.).
# scion-base already created this directory and chowned it; ensure it
# survives any image flattening here.
RUN mkdir -p /home/scion/.scion && \
  chown -R scion:scion /home/scion/.scion

# The scion-base ENTRYPOINT runs sciontool init -- <cmd>. Override the
# default CMD so the container, when started without arguments, runs the
# hub server in the foreground.
#
# WORKAROUND (B23): Observed behaviour — exec-form CMD daemonizes
# (container exits 0); wrapping in /bin/sh -c exec runs foreground
# correctly.  Same binary, same flags.  Root cause unresolved; see
# .design/project-log/task15-b23-daemonize-investigation.md.
CMD ["/bin/sh", "-c", "exec scion server start --foreground --host 0.0.0.0 --enable-hub"]
