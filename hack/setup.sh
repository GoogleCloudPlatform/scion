#!/bin/bash
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

set -e

# hack/setup.sh - Setup isolated test environment

REPO_ROOT=$(pwd)
TEST_DIR="${REPO_ROOT}/../qa-scion"
BIN_DIR="${HOME}/UNIX/bin"

echo "=== Setting up test environment in ${TEST_DIR} ==="

mkdir -p "${TEST_DIR}"
mkdir -p "${BIN_DIR}"

echo "=== Building scion binary to ${BIN_DIR} ==="
go build -o "${BIN_DIR}/scion" ./cmd/scion

cd "${TEST_DIR}"
if [ ! -d ".git" ]; then
    git init -q
fi
echo ".scion/agents/" > .gitignore

echo "=== Initializing grove ==="
scion grove init

echo "=== Setup Complete ==="
ls -A1 .scion