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


container image pull us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-gemini:latest
container image pull us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-claude:latest
container image pull us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-opencode:latest
container image pull us-central1-docker.pkg.dev/ptone-misc/public-docker/scion-codex:latest
container image prune
