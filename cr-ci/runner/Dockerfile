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
# https://mirrors.edge.kernel.org/pub/software/scm/git/ (git-<v>.tar.xz). The sha256 is from
# that directory's sha256sums.asc, and the tarball was checked against Junio C Hamano's
# detached git-<v>.tar.sign (key 96E07AF25771955980DAD10020D04E5A713660A7) when pinned. The
# repository's tests need git >= 2.48 (worktree --relative-paths); Ubuntu 24.04 ships 2.43.
ARG GIT_VERSION=2.56.0
ARG GIT_SHA256=26c56c296b38c0695b26fa95f475f1d01704d2d38e73465ca30b0b2f5dc789d3
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

# --- git, built from the pinned source ------------------------------------------
# gcc, make and the -dev libraries exist only in this stage; the runner image gets the
# installed tree (/usr/local), never a compiler. No Perl, Python, gettext or Tk parts.
# No Rust: git 2.55+ builds its Rust parts (libgitcore.a, via cargo) unless NO_RUST is
# set (git's Makefile, "Optional Rust support"; mandatory only from git 3.0). Without
# them git uses its C code; the only feature lost is the compatibility hash algorithm
# (extensions.compatObjectFormat, SHA-1/SHA-256 interop), which nothing here uses. The
# stage asserts that the build reports "rust: disabled".
FROM ${UBUNTU_IMAGE} AS git
ARG GIT_VERSION
ARG GIT_SHA256
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl xz-utils gcc libc6-dev make \
      libcurl4-openssl-dev libexpat1-dev libssl-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL -o /tmp/git.tar.xz "https://mirrors.edge.kernel.org/pub/software/scm/git/git-${GIT_VERSION}.tar.xz" \
 && echo "${GIT_SHA256}  /tmp/git.tar.xz" | sha256sum -c - \
 && mkdir /tmp/git-src \
 && tar -C /tmp/git-src --strip-components=1 -xJf /tmp/git.tar.xz \
 && make -C /tmp/git-src -j"$(nproc)" prefix=/usr/local NO_TCLTK=YesPlease NO_GETTEXT=YesPlease \
      NO_PERL=YesPlease NO_PYTHON=YesPlease NO_GITWEB=YesPlease NO_RUST=YesPlease INSTALL_SYMLINKS=YesPlease all \
 && make -C /tmp/git-src prefix=/usr/local NO_TCLTK=YesPlease NO_GETTEXT=YesPlease \
      NO_PERL=YesPlease NO_PYTHON=YesPlease NO_GITWEB=YesPlease NO_RUST=YesPlease INSTALL_SYMLINKS=YesPlease \
      DESTDIR=/out install \
 && /out/usr/local/bin/git --version | grep -qx "git version ${GIT_VERSION}" \
 && /out/usr/local/bin/git version --build-options | grep -qx 'rust: disabled'

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
# The second line is what the routed ci.yml jobs (phase B of the primary-runner work)
# call as if preinstalled, as they are on ubuntu-latest: make (build-and-test,
# ent-generate-check, sqlite-nonhub-tests), procps (the build-and-test watchdog's ps,
# pgrep, pkill; the lint sampler's free), python3 + python3-yaml (harness and cloud-init
# checks, single-node tests), ripgrep, tmux (SCION_TEST_REQUIRE_TMUX) and shellcheck
# (24.04's 0.9.0, the version upstream CI's shellcheck job gets from apt), and
# openssh-client (cmd's ssh tests run the ssh client). No gcc, no Docker, no sudo: no
# routed job needs them, and they would widen what a job can do. git is NOT from apt
# (24.04's is 2.43): it comes from the git stage below; libexpat1 is its runtime library
# for http-push (libcurl and zlib come with curl and the base image).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl jq tar gzip unzip zstd libexpat1 \
      make procps python3 python3-yaml ripgrep tmux shellcheck openssh-client \
 && rm -rf /var/lib/apt/lists/*

# Non-root runner user. The runner, its work dir and all caches belong to it. uid 1001
# and HOME=/home/runner as on ubuntu-latest: actions/cache keys its cache version on
# the cache paths, so ~/... must resolve to the same absolute paths on both runner types.
# HOME is set explicitly (below, before USER) rather than left to the runtime: Docker
# derives it from /etc/passwd, but the image config is what Cloud Run is sure to pass on.
RUN useradd --create-home --home-dir /home/runner --uid 1001 --shell /bin/bash runner

# git from the git stage (pinned source build), then build-time asserts: the
# repository's tests need git >= 2.48 (worktree --relative-paths), and every runtime
# library of the HTTPS transport is present (git --version alone does not load libcurl,
# so a missing one would otherwise fail only the first checkout).
COPY --from=git /out/usr/local/ /usr/local/
RUN v=$(git --version | awk '{print $3}') \
 && { printf '%s\n%s\n' 2.48 "$v" | sort -V -C || { echo "git $v is older than 2.48" >&2; exit 1; }; } \
 && for b in git-remote-https git-http-push; do \
      f=/usr/local/libexec/git-core/$b; \
      if [ "$b" = git-remote-https ] && [ ! -x "$f" ]; then echo "$f is missing" >&2; exit 1; fi; \
      if [ -e "$f" ]; then libs=$(ldd "$f"); \
        if grep 'not found' <<<"$libs"; then echo "$b is missing a runtime library" >&2; exit 1; fi; \
      fi; \
    done

# Go toolchain (verified in the go stage) and the patched golangci-lint.
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}
COPY --from=golangci-lint --chmod=0755 /out/golangci-lint /usr/local/bin/golangci-lint

# GitHub Actions runner. installdependencies.sh ships in the verified tarball
# and installs the .NET runtime libraries (libicu, libssl, ...) via apt.
# The runner is installed at /opt/actions-runner, NOT under HOME: the job's
# ephemeral-disk volume is mounted at HOME (/home/runner), which hides whatever the image
# has there. _diag (the runner's logs and the job's step logs under _diag/pages; about
# 1 MB for the P6 lint job, more for jobs with large output) therefore stays a plain
# directory on the root filesystem, not on the volume: when a job fills the volume, the
# runner must still be able to write its logs and report the failure (P6 PoC run 2 lost
# the runner, and the job's log, to ENOSPC on _diag).
# The work folder is /home/runner/work (the mediator's JIT work_folder), a real
# directory the entrypoint creates on the volume; the _work symlink here only covers a
# JIT config that names the default _work. Nothing here may create anything under
# /home/runner: it is hidden by the mount.
WORKDIR /opt/actions-runner
RUN curl -fsSL -o /tmp/actions-runner.tgz \
      "https://github.com/actions/runner/releases/download/v${ACTIONS_RUNNER_VERSION}/actions-runner-linux-x64-${ACTIONS_RUNNER_VERSION}.tar.gz" \
 && echo "${ACTIONS_RUNNER_SHA256}  /tmp/actions-runner.tgz" | sha256sum -c - \
 && tar -xzf /tmp/actions-runner.tgz \
 && rm /tmp/actions-runner.tgz \
 && ./bin/installdependencies.sh \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s /home/runner/work _work \
 && mkdir -p _diag \
 && chown -R runner:runner /opt/actions-runner /home/runner

COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh
# The job-started hook (entrypoint.sh exports ACTIONS_RUNNER_HOOK_JOB_STARTED to it):
# refuses, with a hard stop, any job that is not a same-repo push or pull_request of the
# repository the runner serves. Its sha256 is pinned in the committed
# cr-ci-job-guard.sh.sha256 (a Go test keeps the two in step) and installed read-only as
# /etc/cr-ci/job-guard.sha256. entrypoint.sh refuses to start the runner unless the hook
# matches it and both files and their directories are root-owned and not writable by the
# runner; the build fails here already if they are not.
COPY --chmod=0755 cr-ci-job-guard.sh /usr/local/bin/cr-ci-job-guard.sh
COPY --chmod=0444 cr-ci-job-guard.sh.sha256 /etc/cr-ci/job-guard.sha256
RUN echo "$(cat /etc/cr-ci/job-guard.sha256)  /usr/local/bin/cr-ci-job-guard.sh" | sha256sum -c - \
 && meta=$(stat -c '%u %a' /usr/local/bin /usr/local/bin/cr-ci-job-guard.sh /etc/cr-ci /etc/cr-ci/job-guard.sha256 | tr '\n' ' ') \
 && { [ "$meta" = "0 755 0 755 0 755 0 444 " ] || { echo "job guard files: owner/mode '$meta'" >&2; exit 1; }; }

ENV HOME=/home/runner
USER runner
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
