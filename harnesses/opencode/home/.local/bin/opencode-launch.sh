#!/bin/sh
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

# Rename GITHUB_TOKEN so opencode's Copilot auto-detection does not trigger.
# OpenCode's LoadGitHubToken() checks $GITHUB_TOKEN (priority #1) before
# VertexAI (#10) and Anthropic API (#2). When scion's git PAT is present,
# opencode always tries the Copilot endpoint and fails with an auth error.
#
# Git operations still work because the credential helper is reconfigured
# to read the renamed variable.
if [ -n "$GITHUB_TOKEN" ]; then
    export SCION_GIT_TOKEN="$GITHUB_TOKEN"
    unset GITHUB_TOKEN
    # Only reconfigure the credential helper for simple PAT-based auth.
    # When GitHub App is enabled, sciontool credential-helper handles
    # token refresh and must not be overwritten.
    if [ "$SCION_GITHUB_APP_ENABLED" != "true" ]; then
        # shellcheck disable=SC2016  # expanded at helper invocation, not config write
        git config --global credential.helper \
            '!f() { echo "password=${SCION_GIT_TOKEN}"; echo "username=oauth2"; }; f'
    fi
fi
exec opencode "$@"
