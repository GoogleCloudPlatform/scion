#!/usr/bin/env python3
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
"""Capture-auth shim — delegates to scion_harness.capture_auth_main().

The PI_AUTH mapping (~/.pi/agent/auth.json) comes from config.yaml's
auth-file required_files, so the shared flow does the single write, at the
requested --scope (default: user), and reports a conflict with a non-zero
exit instead of overwriting."""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness

if __name__ == "__main__":
    sys.exit(scion_harness.capture_auth_main())
