# scripts/single-node-vm/tests/test_release_images.sh -- covers Phase 3b's
# image build through the release image driver
# (image-build/scripts/build-release-images.sh, ptone/scion#6625):
#   - the driver is copied to the VM from this checkout, installed
#     root-owned and checked against the local copy's SHA-256;
#   - the generated build command, run for real against a stub driver that
#     records its argv: the golden argv (VERSION, --also-tag latest,
#     --force-latest, local registry, no push) for each of the 3 steps, the
#     marker on success, and no marker after a failed step;
#   - force_rebuild / --rebuild-images, the reuse (skip) path, and the
#     generated "already built?" check run against a stub docker, including
#     the marker an older deploy.sh wrote;
#   - a VERSION that is not a release tag is refused before the driver is
#     staged or called;
#   - before Phase 2 (source: build): a missing driver, or a release older
#     than v0.3.0-preview.4 given with --version or for a new install, stops
#     the deploy; a kept older release is refused only when a build is
#     needed (Phase 3b, after the skip decision); an unexpected staging dir name
#     and a failed driver copy stop it in Phase 3b (the latter removing the
#     staging dir); the IAP proxy fetches its tag only when it is not local.
# Offline: every external command the generated commands could reach (the
# driver, git, docker, podman, gcloud, curl, skopeo) is a stub; the stubs
# for git/docker/podman/gcloud/curl/skopeo record any call and fail, and
# the tests assert none was made, so no path pushes anything.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

DEPLOY_SH="${TIER_DIR}/deploy.sh"
RI_DRIVER="${TIER_DIR}/../../image-build/scripts/build-release-images.sh"
HUB="imghub"
RI_VERSION="v0.3.0-preview.5"

# ri_config_json HUB [FORCE_REBUILD] -- a minimal, valid deploy.sh config
# with container_images.source: build.
ri_config_json() {
  local hub="$1" force="${2:-false}"
  cat <<EOF
{
  "hub_name": "${hub}",
  "project_id": "demo-project",
  "region": "us-central1",
  "machine_size": "small",
  "disk_size_gb": 200,
  "chat_plugins": [],
  "container_images": {"source": "build", "registry": "", "force_rebuild": ${force}},
  "admin_email": "admin@example.com",
  "update_policy": "notify",
  "release_channel": "nightly"
}
EOF
}

# ri_deploy CONFIG_JSON SENTINEL [EXTRA_ARGS...] -- runs deploy.sh in the
# background against the stub until the gcloud stub touches SENTINEL (or
# deploy.sh exits), then stops it. Same pattern as test_install_layout.sh's
# layout_deploy_through_settings (each test file keeps its own copy -- see
# README.md "How it works"). RI_DEPLOY_VERSION picks --version (default
# RI_VERSION; "none" omits --version). Sets DEPLOY_LOG, RI_REACHED (true/false) and RI_EXITED
# (true when deploy.sh exited on its own before the sentinel).
ri_deploy() {
  local config_json="$1" sentinel="${GCLOUD_STUB_STATE_DIR}/$2" config_file log_file pid waited_ms=0
  shift 2
  config_file="$(mktemp)"
  log_file="$(mktemp)"
  printf '%s' "$config_json" > "$config_file"
  export GCLOUD_STUB_SSH_SUCCEEDS=true
  export SCION_TEST_BUILD_POLL_SECS=0
  local version_args=(--version "${RI_DEPLOY_VERSION:-$RI_VERSION}")
  [[ "${RI_DEPLOY_VERSION:-}" == "none" ]] && version_args=()
  set -m
  bash "$DEPLOY_SH" --config "$config_file" "${version_args[@]}" "$@" \
    < /dev/null > "$log_file" 2>&1 &
  pid=$!
  set +m
  RI_EXITED=false
  while [[ ! -f "$sentinel" && "$waited_ms" -lt 30000 ]]; do
    if ! kill -0 "$pid" 2>/dev/null; then RI_EXITED=true; break; fi
    sleep 0.1
    waited_ms=$((waited_ms + 100))
  done
  RI_REACHED=false
  [[ -f "$sentinel" ]] && RI_REACHED=true
  kill -TERM -- "-${pid}" 2>/dev/null || true
  wait "$pid" 2>/dev/null
  waited_ms=0
  while kill -0 -- "-${pid}" 2>/dev/null && [[ "$waited_ms" -lt 5000 ]]; do
    sleep 0.05
    waited_ms=$((waited_ms + 50))
  done
  DEPLOY_LOG="$(cat "$log_file")"
  rm -f "$config_file" "$log_file"
  unset GCLOUD_STUB_SSH_SUCCEEDS SCION_TEST_BUILD_POLL_SECS
}

# ri_ssh_cmd LOG MARKER -- the --command body of the first multi-line
# `compute ssh` call in LOG whose body contains MARKER.
ri_ssh_cmd() {
  printf '%s\n' "$1" | awk -v marker="$2" '
    /^compute / { if (on && index(buf, marker)) { printf "%s", buf; done = 1; exit } on = 0 }
    /^compute ssh .*--command=$/ { buf = ""; on = 1; next }
    on { buf = buf $0 "\n" }
    END { if (!done && on && index(buf, marker)) printf "%s", buf }
  '
}

# ri_sha256 FILE -- the file's SHA-256 (hex).
ri_sha256() {
  local s
  s="$(sha256sum < "$1")"
  printf '%s' "${s%% *}"
}

# ri_shims DIR -- writes the offline stubs into DIR/shim: a pass-through
# sudo, and git/docker/podman/gcloud/curl/skopeo stubs that append their
# argv to DIR/forbidden.log and fail.
ri_shims() {
  local d="$1" c
  mkdir -p "${d}/shim"
  # shellcheck disable=SC2016 # script text written to a stub file
  printf '#!/usr/bin/env bash\nexec "$@"\n' > "${d}/shim/sudo"
  for c in git docker podman gcloud curl skopeo; do
    # shellcheck disable=SC2016 # script text written to a stub file
    printf '#!/usr/bin/env bash\nprintf "%%s %%s\\n" "%s" "$*" >> "%s/forbidden.log"\nexit 97\n' "$c" "$d" > "${d}/shim/${c}"
  done
  chmod 0755 "${d}"/shim/*
  : > "${d}/forbidden.log"
}

# ri_stub_driver PATH ARGV_LOG -- a stub build-release-images.sh that
# appends its argv ('|'-separated) to ARGV_LOG, and fails when its --target
# value equals RI_DRIVER_FAIL_TARGET.
ri_stub_driver() {
  mkdir -p "$(dirname "$1")"
  cat > "$1" <<EOF
#!/usr/bin/env bash
{ printf '%s|' "\$@"; echo; } >> "$2"
prev=""
for a in "\$@"; do
  if [[ "\$prev" == "--target" && -n "\${RI_DRIVER_FAIL_TARGET:-}" && "\$a" == "\${RI_DRIVER_FAIL_TARGET}" ]]; then
    echo "stub driver: failing --target \$a" >&2
    exit 3
  fi
  prev="\$a"
done
exit 0
EOF
  chmod 0755 "$1"
}

# ri_run_build_cmd CMD WORK -- runs the generated build-start command for
# real: /opt paths and HOME point into WORK, the offline stubs come first
# on PATH, and the stub driver sits where the command expects the
# installed driver. Waits for the background build's exit file. Sets
# RI_OUT, RI_RC and RI_EXIT.
ri_run_build_cmd() {
  local cmd="$1" work="$2" waited=0
  mkdir -p "${work}/home" "${work}/opt/scion-image-build" "${work}/opt/scion-source"
  ri_shims "$work"
  ri_stub_driver "${work}/opt/scion-image-build/build-release-images.sh" "${work}/argv.log"
  cmd="${cmd//\/opt\/scion-image-build/${work}/opt/scion-image-build}"
  cmd="${cmd//\/opt\/scion-source/${work}/opt/scion-source}"
  RI_OUT="$(cd "$work" && HOME="${work}/home" PATH="${work}/shim:${PATH}" bash -c "$cmd" 2>&1 < /dev/null)"
  RI_RC=$?
  while [[ ! -s "${work}/home/.scion-image-build/exit" && "$waited" -lt 100 ]]; do sleep 0.1; waited=$((waited + 1)); done
  RI_EXIT="$(cat "${work}/home/.scion-image-build/exit" 2>/dev/null)"
}

# The golden argv of one driver step (as the stub records it).
ri_golden_argv() {
  printf '%s|--src-dir|/opt/scion-image-src/%s|--also-tag|latest|--force-latest|--|--builder|local-docker|--registry|localhost/scion|--target|%s|' \
    "$RI_VERSION" "$RI_VERSION" "$1"
}

# =====================================================================
# Driver staging
# =====================================================================

# The driver comes from this checkout: copied into a private staging dir
# on the VM, installed root-owned, and checked against the local hash.
test_release_images_driver_copied_installed_and_hash_checked() {
  fresh_gcloud_state
  ri_deploy "$(ri_config_json "$HUB")" image-build-started
  assert_eq "true" "$RI_REACHED" "the deploy must reach the build start (log: ${DEPLOY_LOG: -400})"
  local log install
  log="$(gcloud_log)"
  assert_contains "$log" "compute ssh scion-hub-imghub" "sanity: deploy.sh drove compute ssh"
  assert_contains "$log" "mktemp -d /tmp/scion-imgdriver.XXXXXX" "the driver must be staged in a fresh private dir"
  assert_contains "$log" "compute scp ${TIER_DIR}/../../image-build/scripts/build-release-images.sh scion-hub-imghub:/tmp/scion-imgdriver.STUB01/build-release-images.sh" \
    "the checkout's driver must be copied into the staging dir"
  install="$(ri_ssh_cmd "$log" "SHA-256 mismatch")"
  assert_contains "$install" "sudo install -m 0755 -o root -g root /tmp/scion-imgdriver.STUB01/build-release-images.sh /opt/scion-image-build/build-release-images.sh" \
    "the driver must be installed root-owned"
  assert_contains "$install" "echo '$(ri_sha256 "$RI_DRIVER")  /opt/scion-image-build/build-release-images.sh' | sha256sum -c" \
    "the installed driver must be checked against the local copy's SHA-256"
  assert_contains "$install" "trap 'rm -rf /tmp/scion-imgdriver.STUB01' EXIT" "the staging dir must be removed"
  assert_not_contains "$install" "push" "staging the driver pushes nothing"
  # Up to the build start, nothing clones the release: the driver does
  # that itself. (The IAP proxy step after the build still checks out
  # /opt/scion-source at VERSION.)
  local start
  start="$(line_number "Image build started in background" "$log")"
  assert_not_contains "$(head -n "${start:-0}" <<< "$log")" "git clone" \
    "Phase 3b must no longer clone the release itself"
}

# Run the generated install command for real (sudo passes through, install
# is a stub that copies): a matching hash keeps the driver, a mismatching
# one removes it and fails.
test_release_images_driver_hash_mismatch_refused() {
  fresh_gcloud_state
  ri_deploy "$(ri_config_json "$HUB")" image-driver-installed
  local cmd kind work out rc
  cmd="$(ri_ssh_cmd "$(gcloud_log)" "SHA-256 mismatch")"
  assert_contains "$cmd" "sudo install" "sanity: the install command must have been captured"
  for kind in match mismatch; do
    work="$(mktemp -d)"
    ri_shims "$work"
    # shellcheck disable=SC2016 # script text written to a stub file
    printf '#!/usr/bin/env bash\nargs=()\nwhile [[ $# -gt 0 ]]; do case "$1" in -d) dir=1; shift ;; -m|-o|-g) shift 2 ;; *) args+=("$1"); shift ;; esac; done\nif [[ -n "${dir:-}" ]]; then mkdir -p "${args[@]}"; else cp "${args[0]}" "${args[1]}"; fi\n' > "${work}/shim/install"
    chmod 0755 "${work}/shim/install"
    mkdir -p "${work}/tmp/scion-imgdriver.STUB01"
    if [[ "$kind" == match ]]; then
      cp "$RI_DRIVER" "${work}/tmp/scion-imgdriver.STUB01/build-release-images.sh"
    else
      { cat "$RI_DRIVER"; echo "# tampered"; } > "${work}/tmp/scion-imgdriver.STUB01/build-release-images.sh"
    fi
    local c="$cmd"
    c="${c//\/tmp\/scion-imgdriver/${work}/tmp/scion-imgdriver}"
    c="${c//\/opt\//${work}/opt/}"
    out="$(PATH="${work}/shim:${PATH}" bash -c "$c" 2>&1 < /dev/null)"
    rc=$?
    if [[ "$kind" == match ]]; then
      assert_eq "0" "$rc" "a matching driver must install (output: ${out})"
      assert_true "$(cmp -s "$RI_DRIVER" "${work}/opt/scion-image-build/build-release-images.sh" && echo true || echo false)" \
        "the installed driver must be the checkout's"
    else
      assert_true "$([[ "$rc" -ne 0 ]] && echo true || echo false)" "a mismatching driver must be refused"
      assert_contains "$out" "SHA-256 mismatch" "the refusal must say why"
      assert_false "$([[ -e "${work}/opt/scion-image-build/build-release-images.sh" ]] && echo true || echo false)" \
        "a mismatching driver must be removed"
    fi
    assert_false "$([[ -e "${work}/tmp/scion-imgdriver.STUB01" ]] && echo true || echo false)" "${kind}: the staging dir must be removed"
    assert_eq "" "$(cat "${work}/forbidden.log")" "${kind}: no git/docker/podman/gcloud/curl/skopeo call"
    rm -rf "$work"
  done
}

# =====================================================================
# The build command
# =====================================================================

# Golden argv: 3 driver calls (core-base, scion-base, scion-antigravity),
# each with the release VERSION, --also-tag latest --force-latest, the
# local registry and no --push; then the marker. Nothing else is called.
test_release_images_build_golden_argv() {
  fresh_gcloud_state
  ri_deploy "$(ri_config_json "$HUB")" image-build-started
  local cmd work want
  cmd="$(ri_ssh_cmd "$(gcloud_log)" "Image build started in background")"
  assert_contains "$cmd" "build-release-images.sh" "sanity: the build-start command must have been captured"
  assert_not_contains "$cmd" "build-images.sh \\" "the build must not call build-images.sh directly"
  assert_not_contains "$cmd" "push" "the build command pushes nothing"
  work="$(mktemp -d)"
  ri_run_build_cmd "$cmd" "$work"
  assert_eq "0" "$RI_RC" "the build start must succeed (output: ${RI_OUT})"
  assert_eq "0" "$RI_EXIT" "the background build must succeed"
  want="$(ri_golden_argv core-base)"$'\n'"$(ri_golden_argv scion-base)"$'\n'"$(ri_golden_argv scion-antigravity)"
  assert_eq "$want" "$(cat "${work}/argv.log" 2>/dev/null)" "the driver must be called exactly with the golden argv, in order"
  assert_eq "$RI_VERSION" "$(cat "${work}/opt/scion-image-build/images-built.version" 2>/dev/null)" "the marker must record VERSION"
  assert_eq "" "$(cat "${work}/forbidden.log")" "no git/docker/podman/gcloud/curl/skopeo call (nothing is pushed)"
  rm -rf "$work"
}

# A failed step stops the build: later steps are not run, the exit status
# is non-zero, and no marker is written (the old ones are removed first).
test_release_images_build_failure_writes_no_marker() {
  fresh_gcloud_state
  ri_deploy "$(ri_config_json "$HUB")" image-build-started
  local cmd work
  cmd="$(ri_ssh_cmd "$(gcloud_log)" "Image build started in background")"
  work="$(mktemp -d)"
  mkdir -p "${work}/opt/scion-image-build" "${work}/opt/scion-source"
  echo "v0.0.1" > "${work}/opt/scion-image-build/images-built.version"
  echo "v0.0.1" > "${work}/opt/scion-source/.images-built.version"
  RI_DRIVER_FAIL_TARGET=scion-base ri_run_build_cmd "$cmd" "$work"
  assert_eq "0" "$RI_RC" "the build start itself must succeed (output: ${RI_OUT})"
  assert_true "$([[ -n "$RI_EXIT" && "$RI_EXIT" != 0 ]] && echo true || echo false)" "a failed step must fail the build (exit '${RI_EXIT}')"
  assert_eq "$(ri_golden_argv core-base)"$'\n'"$(ri_golden_argv scion-base)" "$(cat "${work}/argv.log" 2>/dev/null)" \
    "the build must stop at the failed step"
  assert_false "$([[ -e "${work}/opt/scion-image-build/images-built.version" ]] && echo true || echo false)" "no marker after a failed build"
  assert_false "$([[ -e "${work}/opt/scion-source/.images-built.version" ]] && echo true || echo false)" "the legacy marker must be removed too"
  assert_eq "" "$(cat "${work}/forbidden.log")" "no git/docker/podman/gcloud/curl/skopeo call"
  rm -rf "$work"
}

# =====================================================================
# force_rebuild, reuse, and the "already built?" check
# =====================================================================

test_release_images_force_rebuild_skips_check() {
  local how
  for how in config cli; do
    fresh_gcloud_state
    if [[ "$how" == config ]]; then
      ri_deploy "$(ri_config_json "$HUB" true)" image-build-started
    else
      ri_deploy "$(ri_config_json "$HUB")" image-build-started --rebuild-images
    fi
    assert_eq "true" "$RI_REACHED" "${how}: the deploy must reach the build start (log: ${DEPLOY_LOG: -400})"
    assert_contains "$DEPLOY_LOG" "Forcing a rebuild" "${how}: the forced rebuild must be announced"
    assert_false "$([[ -e "${GCLOUD_STUB_STATE_DIR}/image-build-checked" ]] && echo true || echo false)" \
      "${how}: a forced rebuild must not ask whether the images are built"
    assert_contains "$(gcloud_log)" "compute scp" "${how}: the driver must be staged"
  done
}

# The reuse path: the check answers skip, so the driver is neither staged
# nor run, and the deploy goes on to the image check.
test_release_images_reuse_skips_driver() {
  fresh_gcloud_state
  export GCLOUD_STUB_SSH_BUILD_CHECK=skip
  ri_deploy "$(ri_config_json "$HUB")" images-verified
  unset GCLOUD_STUB_SSH_BUILD_CHECK
  local log
  log="$(gcloud_log)"
  assert_eq "true" "$RI_REACHED" "the deploy must reach the image check (log: ${DEPLOY_LOG: -400})"
  assert_true "$([[ -e "${GCLOUD_STUB_STATE_DIR}/image-build-checked" ]] && echo true || echo false)" "the check must be asked"
  assert_contains "$DEPLOY_LOG" "Images already built for version ${RI_VERSION}" "the skip must be announced"
  assert_not_contains "$log" "scion-imgdriver" "the driver must not be staged"
  assert_not_contains "$log" "build-release-images.sh" "the driver must not be called"
  assert_false "$([[ -e "${GCLOUD_STUB_STATE_DIR}/image-build-started" ]] && echo true || echo false)" "no build may start"
}

# The generated check, run against a stub docker: skip only when a marker
# (current or legacy) holds VERSION and all 3 images exist.
test_release_images_check_command_honours_both_markers() {
  fresh_gcloud_state
  ri_deploy "$(ri_config_json "$HUB")" image-build-checked
  local cmd case_ work out want
  cmd="$(ri_ssh_cmd "$(gcloud_log)" "MARKER_OK=false")"
  assert_contains "$cmd" "localhost/scion/scion-antigravity:latest" "sanity: the check command must have been captured"
  for case_ in new:skip legacy:skip stale:build missing-image:build none:build; do
    work="$(mktemp -d)"
    ri_shims "$work"
    mkdir -p "${work}/opt/scion-image-build" "${work}/opt/scion-source"
    # docker: only `image inspect` answers, from the images file.
    # shellcheck disable=SC2016 # script text written to a stub file
    printf '#!/usr/bin/env bash\n[[ "$1 $2" == "image inspect" ]] && grep -qxF -- "$3" "%s/images" && exit 0\nexit 1\n' "$work" > "${work}/shim/docker"
    printf '%s\n' localhost/scion/core-base:latest localhost/scion/scion-base:latest localhost/scion/scion-antigravity:latest > "${work}/images"
    case "${case_%%:*}" in
      new) echo "$RI_VERSION" > "${work}/opt/scion-image-build/images-built.version" ;;
      legacy) echo "$RI_VERSION" > "${work}/opt/scion-source/.images-built.version" ;;
      stale) echo "v0.0.1" > "${work}/opt/scion-image-build/images-built.version" ;;
      missing-image)
        echo "$RI_VERSION" > "${work}/opt/scion-image-build/images-built.version"
        sed -i '/scion-antigravity/d' "${work}/images" ;;
    esac
    local c="$cmd"
    c="${c//\/opt\//${work}/opt/}"
    out="$(PATH="${work}/shim:${PATH}" bash -c "$c" 2>&1 < /dev/null)"
    want="${case_##*:}"
    assert_eq "$want" "$out" "check with marker case '${case_%%:*}'"
    rm -rf "$work"
  done
}

# =====================================================================
# VERSION validation
# =====================================================================

# A VERSION that passes deploy.sh's general check but is not a release tag
# is refused in Phase 3b, before the driver is staged or called.
test_release_images_non_release_version_refused() {
  local v
  for v in v1.0.0-test main v1.2 v01.2.3; do
    fresh_gcloud_state
    RI_DEPLOY_VERSION="$v" ri_deploy "$(ri_config_json "$HUB")" image-build-started
    local log
    log="$(gcloud_log)"
    assert_eq "true" "$RI_EXITED" "${v}: deploy.sh must stop on its own (log: ${DEPLOY_LOG: -300})"
    assert_contains "$DEPLOY_LOG" "Cannot build images for version '${v}': it is not a release tag" "${v}: the refusal must say why"
    assert_contains "$log" "compute ssh" "${v}: sanity: the deploy got as far as the VM"
    assert_not_contains "$log" "scion-imgdriver" "${v}: the driver must not be staged"
    assert_not_contains "$log" "build-release-images.sh" "${v}: the driver must not be called"
    assert_false "$([[ -e "${GCLOUD_STUB_STATE_DIR}/image-build-checked" ]] && echo true || echo false)" "${v}: nothing may be asked of the VM for the build"
  done
}

# A VERSION that starts with '-' never reaches Phase 3b: deploy.sh's
# general check refuses it first, before any VM call.
test_release_images_dash_version_refused_early() {
  fresh_gcloud_state
  RI_DEPLOY_VERSION="-v1.0.0" ri_deploy "$(ri_config_json "$HUB")" image-build-started
  assert_eq "true" "$RI_EXITED" "deploy.sh must stop on its own"
  assert_contains "$DEPLOY_LOG" "Invalid version: '-v1.0.0'" "the refusal must say why"
  assert_not_contains "$(gcloud_log)" "build-release-images.sh" "the driver must not be called"
  assert_not_contains "$(gcloud_log)" "compute ssh" "no VM command may run"
}

# Release tags the driver accepts pass the Phase 3b check (function level).
test_release_images_version_pattern() {
  # shellcheck disable=SC1090 # the function only, from deploy.sh itself
  source <(sed -n '/^release_image_version_ok() {$/,/^}$/p' "$DEPLOY_SH")
  local v
  for v in v0.3.0 v0.3.0-preview.5 v1.2.3-rc.0 v10.20.30-preview.12 nightly-20261106; do
    assert_true "$(release_image_version_ok "$v" && echo true || echo false)" "${v} must be accepted"
  done
  for v in "" v1.2 1.2.3 v1.2.3-beta.1 v1.2.3-preview v01.2.3 v1.2.3-preview.01 nightly-2026110 nightly-202611066 \
    "v1.2.3;id" "-v1.2.3" "v1.2.3 " $'v1.2.3\n' main latest; do
    assert_false "$(release_image_version_ok "$v" && echo true || echo false)" "'${v}' must be refused"
  done
}

# =====================================================================
# The IAP proxy checkout
# =====================================================================

# Phase 3b no longer keeps a release checkout, so the IAP proxy step does:
# with source: build it clones /opt/scion-source at VERSION or moves it to
# VERSION (what Phase 3b used to leave there); with source: registry it
# only clones a missing checkout, as before.
# ri_run_proxy_cmd CMD present|absent -- runs the generated proxy build
# command for real against an existing checkout (/opt points into a work
# dir; sudo passes through; git and docker are stubs that log their argv).
# The git stub answers `rev-parse -q --verify refs/tags/...` with 0 when the
# tag is "present", 1 when "absent". Sets RI_GIT_CALLS and RI_PROXY_RC.
ri_run_proxy_cmd() {
  local cmd="$1" tag="$2" work
  work="$(mktemp -d)"
  ri_shims "$work"
  mkdir -p "${work}/opt/scion-source"
  # shellcheck disable=SC2016 # script text written to a stub file
  printf '#!/usr/bin/env bash
printf "%%s\n" "$*" >> "%s/git.log"
if [[ "$1" == rev-parse ]]; then [[ "%s" == present ]]; exit; fi
exit 0
' "$work" "$tag" > "${work}/shim/git"
  printf '#!/usr/bin/env bash
exit 0
' > "${work}/shim/docker"
  chmod 0755 "${work}/shim/git" "${work}/shim/docker"
  cmd="${cmd//\/opt\//${work}/opt/}"
  (cd "$work" && PATH="${work}/shim:${PATH}" bash -c "$cmd" > /dev/null 2>&1 < /dev/null)
  RI_PROXY_RC=$?
  RI_GIT_CALLS="$(cat "${work}/git.log" 2>/dev/null)"
  assert_eq "0" "$RI_PROXY_RC" "proxy command with the tag ${tag} must succeed"
  rm -rf "$work"
}

test_release_images_proxy_checkout_follows_image_source() {
  local mode cmd
  for mode in build registry; do
    fresh_gcloud_state
    export GCLOUD_STUB_SSH_BUILD_CHECK=skip
    if [[ "$mode" == build ]]; then
      ri_deploy "$(ri_config_json "$HUB")" proxy-image-built
    else
      ri_deploy "$(ri_config_json "$HUB" | sed 's#"source": "build", "registry": ""#"source": "registry", "registry": "us-docker.pkg.dev/demo-project/scion"#')" proxy-image-built
    fi
    unset GCLOUD_STUB_SSH_BUILD_CHECK
    assert_eq "true" "$RI_REACHED" "${mode}: the deploy must reach the proxy build (log: ${DEPLOY_LOG: -400})"
    cmd="$(ri_ssh_cmd "$(gcloud_log)" "extras/cloudrun-iap-proxy")"
    assert_contains "$cmd" "sudo docker build -t" "${mode}: sanity: the proxy build command must have been captured"
    if [[ "$mode" == build ]]; then
      assert_contains "$cmd" "sudo git clone --depth 1 --branch '${RI_VERSION}'" "build: a missing checkout must be cloned at VERSION"
      assert_contains "$cmd" "sudo git fetch --depth 1 origin tag '${RI_VERSION}'" "build: an existing checkout must fetch VERSION"
      assert_contains "$cmd" "sudo git checkout '${RI_VERSION}'" "build: an existing checkout must move to VERSION"
      ri_run_proxy_cmd "$cmd" present
      assert_not_contains "$RI_GIT_CALLS" "fetch" "build: a tag already local must not be fetched again (calls: ${RI_GIT_CALLS})"
      assert_contains "$RI_GIT_CALLS" "checkout ${RI_VERSION}" "build: a local tag is still checked out"
      ri_run_proxy_cmd "$cmd" absent
      assert_contains "$RI_GIT_CALLS" "fetch --depth 1 origin tag ${RI_VERSION}" "build: a tag not yet local must be fetched (calls: ${RI_GIT_CALLS})"
      assert_contains "$RI_GIT_CALLS" "checkout ${RI_VERSION}" "build: the fetched tag is checked out"
    else
      assert_contains "$cmd" "if [ ! -d /opt/scion-source ]; then" "registry: only a missing checkout is cloned"
      assert_contains "$cmd" "https://github.com/GoogleCloudPlatform/scion.git /opt/scion-source" \
        "registry: a missing checkout must be cloned as before"
      assert_not_contains "$cmd" "--branch" "registry: the checkout is not tied to VERSION"
      assert_not_contains "$cmd" "git checkout" "registry: an existing checkout is left alone"
    fi
  done
}

# =====================================================================
# Fix round 1: staging dir, scp failure, early checks
# =====================================================================

# A staging dir name that is not a fresh /tmp/scion-imgdriver.* dir is
# refused before anything is copied or installed (same rule as hub.env).
test_release_images_refuses_unexpected_staging_dir() {
  local bad log
  for bad in "" "/tmp/../etc" "/tmp/scion-imgdriver.X;id" "/tmp/x"; do
    fresh_gcloud_state
    export GCLOUD_STUB_SSH_IMGDRIVER_DIR="$bad"
    ri_deploy "$(ri_config_json "$HUB")" image-driver-installed
    unset GCLOUD_STUB_SSH_IMGDRIVER_DIR
    log="$(gcloud_log)"
    assert_eq "true" "$RI_EXITED" "'${bad}': deploy.sh must stop on its own (log: ${DEPLOY_LOG: -300})"
    assert_contains "$DEPLOY_LOG" "Could not create a private staging directory" "'${bad}': the refusal must say why"
    assert_contains "$log" "mktemp -d /tmp/scion-imgdriver.XXXXXX" "'${bad}': sanity: the staging dir was asked for"
    assert_not_contains "$log" "compute scp ${TIER_DIR}/../../image-build/scripts/build-release-images.sh" "'${bad}': the driver must not be copied"
    assert_not_contains "$log" "sudo install" "'${bad}': nothing may be installed"
  done
}

# A failed copy of the driver removes the staging dir (best effort) and
# stops before anything is installed.
test_release_images_scp_failure_cleans_staging_dir() {
  fresh_gcloud_state
  export GCLOUD_STUB_SCP_FAILS_FOR="build-release-images.sh"
  ri_deploy "$(ri_config_json "$HUB")" image-driver-installed
  unset GCLOUD_STUB_SCP_FAILS_FOR
  local log
  log="$(gcloud_log)"
  assert_eq "true" "$RI_EXITED" "deploy.sh must stop on its own (log: ${DEPLOY_LOG: -300})"
  assert_contains "$DEPLOY_LOG" "Could not copy the image driver to scion-hub-imghub" "the failure must say why"
  assert_contains "$log" "rm -rf -- '/tmp/scion-imgdriver.STUB01'" "the staging dir must be removed"
  assert_not_contains "$log" "sudo install" "nothing may be installed"
  assert_false "$([[ -e "${GCLOUD_STUB_STATE_DIR}/image-build-started" ]] && echo true || echo false)" "no build may start"
}

# A release below the floor with source: build stops before Phase 2, i.e.
# before any gcloud call that creates something.
test_release_images_old_release_refused_before_phase2() {
  local v log
  for v in v0.3.0-preview.3 v0.2.20; do
    fresh_gcloud_state
    RI_DEPLOY_VERSION="$v" ri_deploy "$(ri_config_json "$HUB")" image-build-started
    log="$(gcloud_log)"
    assert_eq "true" "$RI_EXITED" "${v}: deploy.sh must stop on its own (log: ${DEPLOY_LOG: -300})"
    assert_contains "$DEPLOY_LOG" "Cannot build images for ${v} on the VM: container_images.source: build needs v0.3.0-preview.4 or later" \
      "${v}: the refusal must say why"
    assert_not_contains "$log" " create" "${v}: no resource may be created"
    assert_not_contains "$log" "compute ssh" "${v}: no VM command may run"
  done
}

# With source: registry an old release is not refused by the image floor.
test_release_images_old_release_allowed_with_registry_source() {
  fresh_gcloud_state
  RI_DEPLOY_VERSION=v0.2.20 ri_deploy \
    "$(ri_config_json "$HUB" | sed 's#"source": "build", "registry": ""#"source": "registry", "registry": "us-docker.pkg.dev/demo-project/scion"#')" \
    phase2-complete
  assert_eq "true" "$RI_REACHED" "an old release with source: registry must get past the early checks (log: ${DEPLOY_LOG: -300})"
  assert_not_contains "$DEPLOY_LOG" "needs v0.3.0-preview.4 or later" "the image floor applies to source: build only"
}

# deploy.sh run from a tree without image-build/ (no driver) stops before
# Phase 2 when it would build images.
test_release_images_driver_not_found_refused_before_phase2() {
  local tree log
  tree="$(mktemp -d)"
  mkdir -p "${tree}/scripts"
  cp -R "$TIER_DIR" "${tree}/scripts/single-node-vm"
  fresh_gcloud_state
  DEPLOY_SH="${tree}/scripts/single-node-vm/deploy.sh" ri_deploy "$(ri_config_json "$HUB")" image-build-started
  log="$(gcloud_log)"
  assert_eq "true" "$RI_EXITED" "deploy.sh must stop on its own (log: ${DEPLOY_LOG: -300})"
  assert_contains "$DEPLOY_LOG" "Release image driver not found: ${tree}/scripts/single-node-vm/../../image-build/scripts/build-release-images.sh" \
    "the refusal must name the missing driver"
  assert_contains "$DEPLOY_LOG" "Run deploy.sh from a scion checkout" "the refusal must say how to fix it"
  assert_not_contains "$log" " create" "no resource may be created"
  assert_not_contains "$log" "compute ssh" "no VM command may run"
  rm -rf "$tree"
}

# The floor check (function level): below v0.3.0-preview.4 is refused;
# the floor itself, later releases and nightlies pass.
test_release_images_version_floor() {
  # shellcheck source=scripts/single-node-vm/release-order.sh
  source "${TIER_DIR}/release-order.sh"
  # shellcheck disable=SC1090 # the helper only, from deploy.sh itself
  source <(sed -n '/^RELEASE_IMAGE_FLOOR=/p; /^release_image_version_floor_ok() {$/,/^}$/p' "$DEPLOY_SH")
  local v
  for v in v0.3.0-preview.4 v0.3.0-preview.5 v0.3.0-preview.10 v0.3.0-rc.1 v0.3.0 v0.4.0 v1.0.0 v1.0.0-test nightly-20261106; do
    assert_true "$(release_image_version_floor_ok "$v" && echo true || echo false)" "${v} must pass the floor"
  done
  for v in v0.3.0-preview.3 v0.3.0-preview.1 v0.2.20 v0.1.0; do
    assert_false "$(release_image_version_floor_ok "$v" && echo true || echo false)" "${v} must be refused by the floor"
  done
}

# =====================================================================
# Fix round 2: the floor applies only when a build runs
# =====================================================================

# _ri_existing_vm CURRENT_RELEASE BUILD_CHECK -- an existing hub VM whose
# /opt/scion/current is releases/CURRENT_RELEASE, and whose "already
# built?" check answers BUILD_CHECK (skip or build).
_ri_existing_vm() {
  fresh_gcloud_state
  seed_instance "scion-hub-${HUB}" "us-central1-b"
  export GCLOUD_STUB_SSH_CURRENT_LINK="releases/$1"
  export GCLOUD_STUB_SSH_BUILD_CHECK="$2"
}

# A re-run without --version keeps a VM on a release below the floor whose
# images are built: it reaches the skip, with no refusal and no driver.
test_release_images_kept_old_release_with_images_reaches_skip() {
  _ri_existing_vm v0.3.0-preview.3 skip
  RI_DEPLOY_VERSION=none ri_deploy "$(ri_config_json "$HUB")" images-verified
  unset GCLOUD_STUB_SSH_CURRENT_LINK GCLOUD_STUB_SSH_BUILD_CHECK
  local log
  log="$(gcloud_log)"
  assert_eq "true" "$RI_REACHED" "the re-run must reach the image check (log: ${DEPLOY_LOG: -400})"
  assert_contains "$DEPLOY_LOG" "Keeping the installed release v0.3.0-preview.3" "sanity: the installed release is kept"
  assert_contains "$DEPLOY_LOG" "Images already built for version v0.3.0-preview.3" "the build must be skipped"
  assert_not_contains "$DEPLOY_LOG" "needs v0.3.0-preview.4 or later" "no floor refusal when no build runs"
  assert_not_contains "$log" "scion-imgdriver" "the driver must not be staged"
  assert_not_contains "$log" "build-release-images.sh" "the driver must not be called"
}

# The same kept release when a build IS needed (marker or images missing):
# Phase 3b refuses it with the floor message, before staging the driver.
test_release_images_kept_old_release_without_images_refused_in_phase3b() {
  _ri_existing_vm v0.3.0-preview.3 build
  RI_DEPLOY_VERSION=none ri_deploy "$(ri_config_json "$HUB")" image-build-started
  unset GCLOUD_STUB_SSH_CURRENT_LINK GCLOUD_STUB_SSH_BUILD_CHECK
  local log
  log="$(gcloud_log)"
  assert_eq "true" "$RI_EXITED" "deploy.sh must stop on its own (log: ${DEPLOY_LOG: -300})"
  assert_true "$([[ -e "${GCLOUD_STUB_STATE_DIR}/image-build-checked" ]] && echo true || echo false)" \
    "the floor applies after the skip decision"
  assert_contains "$DEPLOY_LOG" "Cannot build images for v0.3.0-preview.3 on the VM: container_images.source: build needs v0.3.0-preview.4 or later" \
    "a needed build of a release below the floor is refused"
  assert_not_contains "$log" "scion-imgdriver" "the driver must not be staged"
}

# An explicit --version below the floor is refused before Phase 2 even
# when the VM's images for it exist (see FINDINGS-round2.md: the early
# check cannot know that without a VM call, and an explicit --version is
# a request to install that release; source: registry or a newer release
# is the way forward).
test_release_images_explicit_old_version_refused_early_even_with_images() {
  _ri_existing_vm v0.3.0-preview.3 skip
  RI_DEPLOY_VERSION=v0.3.0-preview.3 ri_deploy "$(ri_config_json "$HUB")" images-verified
  unset GCLOUD_STUB_SSH_CURRENT_LINK GCLOUD_STUB_SSH_BUILD_CHECK
  local log
  log="$(gcloud_log)"
  assert_eq "true" "$RI_EXITED" "deploy.sh must stop on its own (log: ${DEPLOY_LOG: -300})"
  assert_contains "$DEPLOY_LOG" "needs v0.3.0-preview.4 or later" "the explicit old release is refused"
  assert_contains "$DEPLOY_LOG" "if the VM already runs this release, re-run without --version" \
    "the refusal must point at the re-run that keeps the installed release"
  assert_not_contains "$log" " create" "no resource may be created"
  assert_not_contains "$log" "compute ssh" "no VM command may run"
  assert_false "$([[ -e "${GCLOUD_STUB_STATE_DIR}/image-build-checked" ]] && echo true || echo false)" "it stops before Phase 3b"
}
