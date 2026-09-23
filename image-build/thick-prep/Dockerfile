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

# Thick Base Prep Layer
# Brings the Google Cloud Workstations base image up to the same contract
# core-base satisfies, so scion-base can be built on either one.
#
# This file used to carry its own hand-maintained list of "things the
# Workstations image happens to be missing". That list was assembled by
# observation, so it only ever covered gaps somebody had already tripped over —
# and it went stale silently. It is replaced by the same two scripts core-base
# runs:
#
#   ../lib/install-core-toolchain.sh  reconciles any base to the required state
#   ../lib/verify-base-contract.sh    asserts the result
#
# Reconcile, not install: the Workstations base already ships Go 1.27.1, node
# 24, npm, gcloud, kubectl and sudo, and every one of those is detected and left
# alone. In particular Go is NOT downgraded to scion's go.mod version — these
# images serve many consumers, versions here are floors, never pins.
#
# What this image still does for itself, because it is genuinely specific to
# this base:
#   * git — Ubuntu 24.04 (noble) caps at 2.43.0 with no newer apt candidate,
#     and scion hard-requires 2.47.0 (pkg/util/git.go CheckGitVersion, for
#     `worktree add --relative-paths`). This matters only here: scion-base never
#     installs git, and targets.sh routes thick as thick-prep -> scion-base,
#     bypassing core-base, the only other place git comes from. Before the
#     vendored git below, every thick image silently lost worktree-per-agent
#     mode.
#
# Architecture: amd64 only (Cloud Workstations base has no arm64 variant).
#
# NOTE: the build context is image-build/, not image-build/thick-prep/, so that
# ../lib is reachable. See image-build/scripts/lib/targets.sh step_context_dir.

ARG BASE_IMAGE=us-central1-docker.pkg.dev/cloud-workstations-images/predefined/base:latest

# Keep in sync with image-build/core-base/Dockerfile. See that file for the
# rationale and for the digest-pinning note.
ARG GIT_IMAGE=chainguard/git:latest

# Floor and pin, as in core-base. The Workstations base ships 1.27.1, which
# clears the floor, so GO_VERSION is never actually used on this chain today —
# it exists so the two Dockerfiles stay literally comparable.
ARG GO_MIN_VERSION=1.26.1
ARG GO_VERSION=1.26.1

ARG GH_VERSION=2.100.0

FROM ${GIT_IMAGE} AS git-source

# Stage and patch the vendored git exactly as core-base does — same script, same
# arguments. See ../lib/stage-vendored-git.sh: Chainguard's libpcre2 is reached
# through a RUNPATH on the handful of binaries that declare it, rather than an
# image-wide LD_LIBRARY_PATH that every process in the container would inherit.
#
# The patching stage is Debian even though this image is Ubuntu — patchelf only
# rewrites ELF headers, so the distro running it is irrelevant, and using the
# same stage as core-base keeps the two chains byte-identical here.
FROM node:24-trixie-slim AS git-patched
RUN apt-get update && apt-get install -y --no-install-recommends \
    patchelf \
    binutils \
    && rm -rf /var/lib/apt/lists/*
COPY lib/stage-vendored-git.sh /tmp/stage-vendored-git.sh
COPY --from=git-source /usr/bin/git          /staged/usr/bin/git
COPY --from=git-source /usr/libexec/git-core /staged/usr/libexec/git-core
COPY --from=git-source /usr/share/git-core   /staged/usr/share/git-core
COPY --from=git-source /usr/lib/libpcre2-8.so.0 /staged/usr/local/lib/scion-git/libpcre2-8.so.0
RUN bash /tmp/stage-vendored-git.sh /staged

FROM ${BASE_IMAGE}
USER root
ENV DEBIAN_FRONTEND=noninteractive

# Kept in the finished image so the contract can be re-checked against a running
# container. Deliberately not on PATH. See core-base/Dockerfile.
COPY lib/ /opt/scion-image-build/lib/
RUN chmod +x /opt/scion-image-build/lib/*.sh

# This base ships `ubuntu` at uid 1000; scion-base creates `scion` there. The
# step matches by uid, not by name, so it works on both bases.
RUN /opt/scion-image-build/lib/install-core-toolchain.sh free-uid-1000

RUN /opt/scion-image-build/lib/install-core-toolchain.sh apt-common
# No apt candidate for chromium on Ubuntu (snap-only); the step detects that and
# installs google-chrome-stable, symlinked to /usr/bin/chromium because scion
# depends on the name. See the step for the callers.
RUN /opt/scion-image-build/lib/install-core-toolchain.sh chromium
# Absent from this base entirely. pkg/runtime/common.go shells out to a literal
# `gcsfuse`, so without this every GCS volume mount on a thick image fails at
# runtime, far from the cause.
RUN /opt/scion-image-build/lib/install-core-toolchain.sh gcsfuse
# Present already (/usr/bin/gcloud, /usr/bin/kubectl) — both steps detect and
# skip rather than laying down a second copy at a different path.
RUN /opt/scion-image-build/lib/install-core-toolchain.sh gcloud
RUN /opt/scion-image-build/lib/install-core-toolchain.sh kubectl

# --- Vendored git (Chainguard) ---------------------------------------------
# Mirrors core-base so both chains get the same git from the same place. See
# core-base/Dockerfile for the full rationale; the short version:
#   - helpers MUST go to /usr/libexec/git-core (compiled-in GIT_EXEC_PATH);
#   - the binary MUST overwrite /usr/bin/git, because Chainguard ships
#     /usr/libexec/git-core/git as a symlink to ../../bin/git and git re-execs
#     itself through it. This base DOES ship its own /usr/bin/git (2.43), so
#     installing ours anywhere else leaves that symlink pointing at the old
#     binary — `git --version` says 2.55.0 while subcommands fail with
#     "fatal: unknown repository extension found: relativeworktrees".
#     This is not hypothetical: it is exactly what this image did before.
# Must stay after the apt steps so nothing reinstates the distro git over it.
# Ubuntu 24.04 ships glibc 2.39, clearing Chainguard's 2.38 floor — by a single
# release, so any future change of BASE_IMAGE needs to re-check this.
COPY --from=git-patched /staged/usr/bin/git          /usr/bin/git
COPY --from=git-patched /staged/usr/libexec/git-core /usr/libexec/git-core
COPY --from=git-patched /staged/usr/share/git-core   /usr/share/git-core
COPY --from=git-patched /staged/usr/local/lib/scion-git /usr/local/lib/scion-git

# Go 1.27.1 is already here and stays. The step prints why it skipped.
ARG GO_MIN_VERSION
ARG GO_VERSION
RUN /opt/scion-image-build/lib/install-core-toolchain.sh go "${GO_MIN_VERSION}" "${GO_VERSION}"

# Installed to /usr/bin/gh specifically: scion-base wraps that exact path to
# inject the GitHub App token. The apt gh this file used to install happened to
# land there; relying on that coincidence is what broke the other chain.
ARG GH_VERSION
RUN /opt/scion-image-build/lib/install-core-toolchain.sh gh "${GH_VERSION}"

RUN /opt/scion-image-build/lib/install-core-toolchain.sh golangci-lint

RUN /opt/scion-image-build/lib/install-core-toolchain.sh npm-global

# Both of these were missing on this chain while core-base had them, which made
# scion-base's `chown -R scion:scion /usr/local/share/npm-global` a decoration:
# with NPM_CONFIG_PREFIX unset, `npm install -g` targets /usr/lib/node_modules
# and fails on permissions for the scion user.
ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=/usr/local/share/npm-global/bin:/usr/local/go/bin:$PATH

# Same corporate-mirror escape hatches core-base offers; see that file. Having
# them on only one chain meant a thick build could not be pointed at an internal
# registry at all.
ARG NPM_REGISTRY=https://registry.npmjs.org/
ENV NPM_CONFIG_REGISTRY=${NPM_REGISTRY}
ARG PIP_INDEX_URL=https://pypi.org/simple
ENV PIP_INDEX_URL=${PIP_INDEX_URL}

RUN --mount=type=secret,id=npmrc,target=/root/.npmrc,required=false \
    /opt/scion-image-build/lib/install-core-toolchain.sh npm-tools

# Identical closing line to core-base. That is the whole point: whichever base
# scion-base is built on, it has passed the same assertion.
#
# This image is amd64-only, so the emulation branch never triggers here today.
# The args are forwarded anyway, so the two closing lines stay identical and
# this chain does not have to be revisited if it ever gains an arm64 variant.
ARG TARGETARCH
ARG BUILDARCH
RUN GO_MIN_VERSION="${GO_MIN_VERSION}" \
    TARGETARCH="${TARGETARCH}" \
    BUILDARCH="${BUILDARCH}" \
    /opt/scion-image-build/lib/verify-base-contract.sh
