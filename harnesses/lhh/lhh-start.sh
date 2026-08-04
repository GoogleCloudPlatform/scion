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

set -eu

# Start FastAPI web server in background (port 8080)
cd /opt/lhh
python3 -m uvicorn horizon.fast_api_app:app \
    --host 0.0.0.0 --port 8080 \
    --log-level warning &
WEB_PID=$!

# Trap to clean up background server
trap "kill $WEB_PID 2>/dev/null; wait $WEB_PID 2>/dev/null" EXIT

# Run the REPL in foreground — passes through all args from Scion
exec python3 /opt/lhh/lhh_scion_repl.py "$@"
