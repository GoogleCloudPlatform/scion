# cr-ci runner image: one ephemeral GitHub Actions JIT runner per Cloud Run
# Job execution. See runner/README.md for the build arguments and how to bump
# them, and docs/architecture.md for how the image is used.
#
# Tool versions and their sha256 checksums are pinned ONLY in the ARG block
# below. Override them with --build-arg, always a version together with its
# checksum. Every download is sha256-verified; the build fails on a mismatch.
# golangci-lint is the exception: it is built from source by
# golangci-lint/build.sh, the single copy of the patched build that CI also uses
# (dominikh/go-tools#1839), with its pins in golangci-lint/VERSIONS.
#
# No Docker daemon: Cloud Run runs in gVisor, so DinD and `services:` do not
# work (design section 1.4). No secret enters any layer: the JIT config is
# fetched at run time by entrypoint.sh through the mediator's claim exchange.

# --- Pinned versions (the single source) ---------------------------------------
# ubuntu:24.04 multi-arch index digest (Docker Hub, last_updated 2026-10-17).
ARG UBUNTU_IMAGE=ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac
# https://github.com/actions/runner/releases (sha256 from the release notes).
ARG ACTIONS_RUNNER_VERSION=2.338.0
ARG ACTIONS_RUNNER_SHA256=af4b794c1bc41d73d40535e3fe092a39f9679cd8d965954c2aca25a05ca41d32
# https://go.dev/dl/ (linux-amd64 archive).
ARG GO_VERSION=1.26.1
ARG GO_SHA256=031f088e5d955bab8657ede27ad4e3bc5b7c1ba281f05f245bcc304f327c987a
# -------------------------------------------------------------------------------

# --- Go toolchain (used by the golangci-lint build and copied into the image) ---
FROM ${UBUNTU_IMAGE} AS go
ARG GO_VERSION
ARG GO_SHA256
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL -o /tmp/go.tgz "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz" \
 && echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - \
 && tar -C /usr/local -xzf /tmp/go.tgz \
 && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:${PATH}

# --- golangci-lint, patched for dominikh/go-tools#1839 -------------------------
# The same build.sh, VERSIONS and patch as .github/workflows/ci.yml's lint job, so
# the image ships the binary CI runs (it reports 2.15.0-scion-gotools1839). The
# modules come from the Go module proxy as checksum-pinned source; see
# golangci-lint/README.md, which also says when to drop this stage.
FROM go AS golangci-lint
RUN apt-get update \
 && apt-get install -y --no-install-recommends git \
 && rm -rf /var/lib/apt/lists/*
COPY golangci-lint/ /src/golangci-lint/
RUN GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomodcache bash /src/golangci-lint/build.sh /out/golangci-lint \
 && rm -rf /tmp/gocache /tmp/gomodcache

# --- The runner image ------------------------------------------------------------
FROM ${UBUNTU_IMAGE}

ARG ACTIONS_RUNNER_VERSION
ARG ACTIONS_RUNNER_SHA256

# RUNNER_MANUALLY_TRAP_SIG=1 makes run.sh trap TERM/INT and forward SIGINT to the
# runner's process group: a graceful job cancel when entrypoint.sh (PID 1) forwards
# Cloud Run's SIGTERM. Without it, run.sh installs no handler. run.sh exits 0 for
# any runner exit code but 2 and 7 (run-helper.sh maps unknown codes to 0), so
# entrypoint.sh recovers the real code (runner/README.md, step 5).
ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    RUNNER_MANUALLY_TRAP_SIG=1

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

# Base tools from the Ubuntu archive (apt verifies the archive signatures). zstd: with
# it on PATH, actions/cache compresses with zstd as on ubuntu-latest, so cache versions
# (which include the compression method) match across the two runner types.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq tar gzip unzip zstd \
 && rm -rf /var/lib/apt/lists/*

# Non-root runner user. The runner, its work dir and all caches belong to it. uid 1001
# and HOME=/home/runner as on ubuntu-latest: actions/cache keys its cache version on
# the cache paths, so ~/... must resolve to the same absolute paths on both runner types.
# HOME is set explicitly (below, before USER) rather than left to the runtime: Docker
# derives it from /etc/passwd, but the image config is what Cloud Run is sure to pass on.
RUN useradd --create-home --home-dir /home/runner --uid 1001 --shell /bin/bash runner

# Go toolchain (verified in the go stage) and the patched golangci-lint.
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
COPY --from=golangci-lint --chmod=0755 /out/golangci-lint /usr/local/bin/golangci-lint

# GitHub Actions runner. installdependencies.sh ships in the verified tarball
# and installs the .NET runtime libraries (libicu, libssl, ...) via apt.
# _diag (the runner's logs and the job's step logs under _diag/pages; about 1 MB for
# the P6 lint job, more for jobs with large output) stays a plain directory on the
# root filesystem, NOT on /scratch: when a job fills /scratch, the runner must still be
# able to write its logs and report the failure (P6 PoC run 2 lost the runner, and the
# job's log, to ENOSPC on _diag).
# The work folder is /home/runner/work (the mediator's JIT work_folder), which
# entrypoint.sh links to /scratch/_work; the _work symlink here only covers a JIT
# config that names the default _work. Nothing here may create /home/runner/work,
# /home/runner/.cache or /home/runner/go: the entrypoint creates them as links.
WORKDIR /home/runner/actions-runner
RUN curl -fsSL -o /tmp/actions-runner.tgz \
      "https://github.com/actions/runner/releases/download/v${ACTIONS_RUNNER_VERSION}/actions-runner-linux-x64-${ACTIONS_RUNNER_VERSION}.tar.gz" \
 && echo "${ACTIONS_RUNNER_SHA256}  /tmp/actions-runner.tgz" | sha256sum -c - \
 && tar -xzf /tmp/actions-runner.tgz \
 && rm /tmp/actions-runner.tgz \
 && ./bin/installdependencies.sh \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /scratch/_work _work \
 && mkdir -p _diag \
 && chown -R runner:runner /home/runner

COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

ENV HOME=/home/runner
USER runner
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
