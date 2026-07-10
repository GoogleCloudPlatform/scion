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
"""Gemini CLI container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.
  * Mounted any auth file (e.g. ~/.gemini/oauth_creds.json) at the declared
    container_path, when auth-file mode is in use.
  * Mounted ADC credentials when vertex-ai mode is in use.

This script's job:

  1. Determine which auth method Gemini CLI will use, honoring an explicit
     selection if present and otherwise applying the same precedence as the
     compiled harness:
         GEMINI_API_KEY / GOOGLE_API_KEY > auth-file (OAuth) > vertex-ai.
  2. Map the universal auth type to a Gemini-internal auth type string and
     write it into ~/.gemini/settings.json under security.auth.selectedType.
  3. Write outputs/resolved-auth.json describing the chosen method.
  4. Write outputs/env.json with env vars to project into the harness process
     (e.g. GEMINI_API_KEY, GOOGLE_CLOUD_PROJECT, or Vertex AI vars).
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness

assert scion_harness.INTERFACE_VERSION >= 2, (
    f"scion_harness INTERFACE_VERSION {scion_harness.INTERFACE_VERSION} < 2; "
    "update the shared library"
)

GEMINI_SETTINGS_FILE = "~/.gemini/settings.json"
GEMINI_OAUTH_CREDS_FILE = "~/.gemini/oauth_creds.json"

_GEMINI_AUTH_TYPE_MAP = {
    "api-key": "gemini-api-key",
    "auth-file": "oauth-personal",
    "vertex-ai": "vertex-ai",
}

AUTH = scion_harness.AuthSpec(
    "gemini-cli",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["GEMINI_API_KEY", "GOOGLE_API_KEY"],
            hint="set GEMINI_API_KEY or GOOGLE_API_KEY",
        ),
        scion_harness.file_method(
            "auth-file",
            path=GEMINI_OAUTH_CREDS_FILE,
            hint=f"provide OAuth credentials at {GEMINI_OAUTH_CREDS_FILE}",
            secret_key="GEMINI_OAUTH_CREDS",
        ),
        scion_harness.env_method(
            "vertex-ai",
            any_of=["GOOGLE_CLOUD_PROJECT"],
            hint="set GOOGLE_CLOUD_PROJECT (with ADC or GCP service account) for Vertex AI",
        ),
    ],
)


def _update_gemini_settings(settings_path: str, gemini_auth_type: str) -> None:
    """Update ~/.gemini/settings.json with the Gemini-native auth type."""
    expanded = scion_harness.expand_path(settings_path)
    settings: dict[str, Any] = {}
    if os.path.isfile(expanded):
        try:
            settings = scion_harness.load_json(expanded) or {}
        except (OSError, json.JSONDecodeError):
            settings = {}
    if not isinstance(settings, dict):
        settings = {}

    security = settings.get("security")
    if not isinstance(security, dict):
        security = {}
        settings["security"] = security

    auth = security.get("auth")
    if not isinstance(auth, dict):
        auth = {}
        security["auth"] = auth

    if gemini_auth_type:
        if auth.get("selectedType") == gemini_auth_type:
            return
        auth["selectedType"] = gemini_auth_type
    else:
        auth.pop("selectedType", None)
        if not auth:
            security.pop("auth", None)

    scion_harness.atomic_write_json(expanded, settings)


def _build_env_overlay(method: str, env_key: str) -> dict[str, str]:
    """Build the env vars overlay for outputs/env.json."""
    if method == "api-key" and env_key:
        return {env_key: f"${{{env_key}}}"}
    if method == "auth-file":
        overlay: dict[str, str] = {}
        if os.environ.get("GOOGLE_CLOUD_PROJECT"):
            overlay["GOOGLE_CLOUD_PROJECT"] = "${GOOGLE_CLOUD_PROJECT}"
        return overlay
    if method == "vertex-ai":
        return {
            "GOOGLE_CLOUD_PROJECT": "${GOOGLE_CLOUD_PROJECT}",
            "GOOGLE_CLOUD_REGION": "${GOOGLE_CLOUD_REGION}",
            "GOOGLE_CLOUD_LOCATION": "${GOOGLE_CLOUD_REGION}",
        }
    return {}


def provision(ctx: scion_harness.ProvisionContext) -> None:
    resolved = ctx.select_auth(AUTH)

    gemini_auth_type = _GEMINI_AUTH_TYPE_MAP.get(resolved.method, "")
    _update_gemini_settings(GEMINI_SETTINGS_FILE, gemini_auth_type)

    env = _build_env_overlay(resolved.method, resolved.env_key)
    extra: dict[str, Any] | None = None
    if resolved.method == "vertex-ai":
        extra = {"vertex_ai": True}

    ctx.write_outputs(resolved, env=env, extra=extra)
    ctx.info(f"method={resolved.method}")


if __name__ == "__main__":
    scion_harness.run("gemini-cli", provision)
