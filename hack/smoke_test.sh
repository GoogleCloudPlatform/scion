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

# Build the binary
echo "Building scion..."
go build -buildvcs=false -o scion ./cmd/scion

# Check if binary exists
if [ ! -f ./scion ]; then
    echo "Build failed, ./scion not found"
    exit 1
fi

# Run help
echo "Running scion --help..."
./scion --help

# Run version (if available, assuming 'version' or similar command exists, 
# but help is guaranteed by Cobra usually)
# ./scion version

echo "Smoke test passed!"
rm ./scion
