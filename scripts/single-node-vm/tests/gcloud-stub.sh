#!/usr/bin/env bash
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

# Fake `gcloud` used by test-iap-fw-scope.sh to exercise the Phase 1/2
# prefix of scripts/single-node-vm/deploy.sh without touching any real GCP
# project or VM. It never talks to the network: every subcommand is
# pattern-matched below and answered with canned output driven by env vars.
#
# Every invocation is appended verbatim to $GCLOUD_STUB_LOG so the test can
# assert both *what* was called and in what *order*.
#
# Scenario knobs (env vars, all default to "false"/empty):
#   MOCK_VM_EXISTS        "true" if the hub VM already exists.
#   MOCK_FW_EXISTS        "true" if the firewall rule already exists.
#   MOCK_FW_TARGET_TAGS   Existing rule's target tags (empty = unscoped
#                         rule from a pre-fix deploy).

set -euo pipefail

: "${GCLOUD_STUB_LOG:?GCLOUD_STUB_LOG must be set}"
: "${MOCK_VM_EXISTS:=false}"
: "${MOCK_FW_EXISTS:=false}"
: "${MOCK_FW_TARGET_TAGS:=}"

echo "gcloud $*" >>"${GCLOUD_STUB_LOG}"

c1="${1:-}"; c2="${2:-}"; c3="${3:-}"

case "$*" in
  "config get-value project"*) echo "test-project"; exit 0 ;;
  "config get-value account"*) echo "deployer@example.com"; exit 0 ;;
esac

case "${c1} ${c2}" in
  "auth list")
    echo "deployer@example.com"
    exit 0
    ;;
esac

case "${c1} ${c2} ${c3}" in
  "compute zones list")
    echo "us-central1-a"
    exit 0
    ;;
  "compute routers describe")
    exit 0
    ;;
  "compute routers create")
    exit 0
    ;;
  "compute routers nats")
    # "compute routers nats describe|create" - $4 carries the verb.
    exit 0
    ;;
  "projects get-ancestors "*)
    exit 0
    ;;
  "services enable "*)
    exit 0
    ;;
  "projects add-iam-policy-binding "*)
    exit 0
    ;;
  "iam service-accounts "*)
    # describe|create - report as already existing to skip creation noise.
    exit 0
    ;;
  "compute instances describe")
    if [[ "${MOCK_VM_EXISTS}" == "true" ]]; then
      exit 0
    else
      exit 1
    fi
    ;;
  "compute instances add-tags")
    exit 0
    ;;
  "compute instances create")
    exit 0
    ;;
  "compute firewall-rules describe")
    if [[ "${MOCK_FW_EXISTS}" == "true" ]]; then
      echo "${MOCK_FW_TARGET_TAGS}"
      exit 0
    else
      exit 1
    fi
    ;;
  "compute firewall-rules create")
    exit 0
    ;;
  "compute firewall-rules update")
    exit 0
    ;;
esac

# Unrecognized command: succeed with no output rather than fail the test
# harness on an incidental gcloud call this test doesn't care about.
exit 0
