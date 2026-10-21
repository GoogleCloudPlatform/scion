# cr-ci runner image: one ephemeral GitHub Actions JIT runner per Cloud Run
# Job execution. See runner/README.md for the build arguments and how to bump
# them, and docs/architecture.md for how the image is used.
#
# Tool versions and their sha256 checksums are pinned ONLY in the ARG block
# below. Override them with --build-arg, always a version together with its
# checksum. Every download is sha256-verified; the build fails on a mismatch.
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
# https://github.com/golangci/golangci-lint/releases (golangci-lint-<v>-checksums.txt); keep
# in step with the version .github/workflows/ci.yml installs.
ARG GOLANGCI_LINT_VERSION=2.15.0
ARG GOLANGCI_LINT_SHA256=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea
# -------------------------------------------------------------------------------

FROM ${UBUNTU_IMAGE}

ARG ACTIONS_RUNNER_VERSION
ARG ACTIONS_RUNNER_SHA256
ARG GO_VERSION
ARG GO_SHA256
ARG GOLANGCI_LINT_VERSION
ARG GOLANGCI_LINT_SHA256

# RUNNER_MANUALLY_TRAP_SIG=1 makes run.sh (PID 1) trap TERM/INT, forward
# SIGINT to the runner's process group (graceful job cancel on Cloud Run's
# SIGTERM) and exit with the runner's exit code. Without it, run.sh installs
# no handler, so PID 1 drops SIGTERM, and it exits 0 whatever the runner returned.
ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    RUNNER_MANUALLY_TRAP_SIG=1

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

# Base tools from the Ubuntu archive (apt verifies the archive signatures).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git jq tar gzip unzip \
 && rm -rf /var/lib/apt/lists/*

# Non-root runner user. The runner, its work dir and all caches belong to it.
RUN useradd --create-home --uid 1001 --shell /bin/bash runner

# Go toolchain.
RUN curl -fsSL -o /tmp/go.tgz "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz" \
 && echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - \
 && tar -C /usr/local -xzf /tmp/go.tgz \
 && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:${PATH}

# golangci-lint.
RUN curl -fsSL -o /tmp/golangci-lint.tgz \
      "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz" \
 && echo "${GOLANGCI_LINT_SHA256}  /tmp/golangci-lint.tgz" | sha256sum -c - \
 && tar -C /tmp -xzf /tmp/golangci-lint.tgz \
 && install -m 0755 "/tmp/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64/golangci-lint" /usr/local/bin/golangci-lint \
 && rm -rf /tmp/golangci-lint.tgz "/tmp/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64"

# GitHub Actions runner. installdependencies.sh ships in the verified tarball
# and installs the .NET runtime libraries (libicu, libssl, ...) via apt.
# _work and _diag are symlinks into /scratch so that the runner's work folder
# and logs land on the ephemeral disk whichever work_folder the JIT config names.
WORKDIR /home/runner/actions-runner
RUN curl -fsSL -o /tmp/actions-runner.tgz \
      "https://github.com/actions/runner/releases/download/v${ACTIONS_RUNNER_VERSION}/actions-runner-linux-x64-${ACTIONS_RUNNER_VERSION}.tar.gz" \
 && echo "${ACTIONS_RUNNER_SHA256}  /tmp/actions-runner.tgz" | sha256sum -c - \
 && tar -xzf /tmp/actions-runner.tgz \
 && rm /tmp/actions-runner.tgz \
 && ./bin/installdependencies.sh \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /scratch/_work _work \
 && ln -s /scratch/_diag _diag \
 && chown -R runner:runner /home/runner

COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

USER runner
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
