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

This script's job mirrors the compiled GeminiCLI harness:

  1. Determine which auth method Gemini CLI will use, honoring an explicit
     selection if present and otherwise applying the same precedence as the
     compiled harness:
         GEMINI_API_KEY / GOOGLE_API_KEY > auth-file (OAuth) > vertex-ai.
  2. Map the universal auth type to a Gemini-internal auth type string and
     write it into ~/.gemini/settings.json under security.auth.selectedType.
  3. Write outputs/resolved-auth.json describing the chosen method.
  4. Write outputs/env.json with env vars to project into the harness process
     (e.g. GEMINI_API_KEY, GOOGLE_CLOUD_PROJECT, or Vertex AI vars).

The script is intentionally stdlib-only so it works on any container image
that ships python3 (declared in config.yaml's required_image_tools).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

GEMINI_SETTINGS_FILE = "~/.gemini/settings.json"
GEMINI_OAUTH_CREDS_FILE = "~/.gemini/oauth_creds.json"

VALID_AUTH_TYPES = ("api-key", "auth-file", "vertex-ai")

EXIT_OK = 0
EXIT_ERROR = 1
EXIT_UNSUPPORTED = 2

# Maps universal auth type values to Gemini CLI-internal values.
# Gemini CLI settings.json expects specific strings like "gemini-api-key",
# "oauth-personal", "vertex-ai".
_GEMINI_AUTH_TYPE_MAP = {
    "api-key": "gemini-api-key",
    "auth-file": "oauth-personal",
    "vertex-ai": "vertex-ai",
}


def _expand(path: str) -> str:
    """Expand ~ and $HOME in a container path."""
    return os.path.expanduser(os.path.expandvars(path))


def _load_json(path: str) -> Any:
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def _write_json(path: str, payload: Any) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(payload, f, indent=2, sort_keys=True)
        f.write("\n")
    os.replace(tmp, path)


def _present_env_keys(candidates: dict[str, Any]) -> set[str]:
    """Names of auth env vars staged by the host as candidates."""
    raw = candidates.get("env_vars") or []
    return {str(k) for k in raw if isinstance(k, str)}


def _present_file_paths(candidates: dict[str, Any]) -> list[str]:
    """Container paths of auth files mounted by the host as candidates."""
    raw = candidates.get("files") or []
    out: list[str] = []
    for entry in raw:
        if isinstance(entry, dict):
            cp = entry.get("container_path")
            if isinstance(cp, str) and cp:
                out.append(cp)
    return out


def _env_secret_files(candidates: dict[str, Any]) -> dict[str, str]:
    """Map of env-var name -> path to secret value file staged by the host."""
    raw = candidates.get("env_secret_files") or {}
    if not isinstance(raw, dict):
        return {}
    return {str(k): str(v) for k, v in raw.items() if isinstance(k, str) and isinstance(v, str)}


def _auth_file_present(file_paths: list[str], target: str) -> bool:
    """Return True if the auth file is mounted or already on disk."""
    if any(_expand(p) == _expand(target) for p in file_paths):
        return True
    return os.path.isfile(_expand(target))


def _select_auth_method(
    explicit: str,
    env_keys: set[str],
    file_paths: list[str],
) -> tuple[str, str]:
    """Pick an auth method.

    Returns (method, env_key_or_empty). env_key is the chosen API key env var
    name when method == 'api-key', else "". Raises ValueError on no-creds.
    """
    has_gemini_key = "GEMINI_API_KEY" in env_keys
    has_google_key = "GOOGLE_API_KEY" in env_keys
    has_authfile = _auth_file_present(file_paths, GEMINI_OAUTH_CREDS_FILE)
    has_gcp_project = "GOOGLE_CLOUD_PROJECT" in env_keys
    if explicit:
        if explicit not in VALID_AUTH_TYPES:
            raise ValueError(
                f"gemini: unknown auth type {explicit!r}; valid types are: "
                f"{', '.join(VALID_AUTH_TYPES)}"
            )
        if explicit == "api-key":
            if has_gemini_key:
                return "api-key", "GEMINI_API_KEY"
            if has_google_key:
                return "api-key", "GOOGLE_API_KEY"
            raise ValueError(
                "gemini: auth type 'api-key' selected but no API key found; "
                "set GEMINI_API_KEY or GOOGLE_API_KEY"
            )
        if explicit == "auth-file":
            if not has_authfile:
                raise ValueError(
                    "gemini: auth type 'auth-file' selected but OAuth credentials "
                    f"file not found at {GEMINI_OAUTH_CREDS_FILE}"
                )
            return "auth-file", ""
        if explicit == "vertex-ai":
            if not has_gcp_project:
                raise ValueError(
                    "gemini: auth type 'vertex-ai' selected but "
                    "GOOGLE_CLOUD_PROJECT is not set"
                )
            return "vertex-ai", ""

    # Auto-detect precedence matches the compiled GeminiCLI harness:
    # API key -> OAuth (auth-file) -> Vertex AI
    if has_gemini_key:
        return "api-key", "GEMINI_API_KEY"
    if has_google_key:
        return "api-key", "GOOGLE_API_KEY"
    if has_authfile:
        return "auth-file", ""
    if has_gcp_project:
        return "vertex-ai", ""

    raise ValueError(
        "gemini: no valid auth method found; set GEMINI_API_KEY or "
        "GOOGLE_API_KEY for API key auth, provide OAuth credentials at "
        f"{GEMINI_OAUTH_CREDS_FILE}, or set GOOGLE_CLOUD_PROJECT "
        "(with ADC or GCP service account) for Vertex AI"
    )


def _update_gemini_settings(settings_path: str, gemini_auth_type: str) -> None:
    """Update ~/.gemini/settings.json with the Gemini-native auth type.

    Mirrors GeminiCLI.updateSelectedAuthType in gemini_cli.go.
    """
    settings: dict[str, Any] = {}
    expanded = _expand(settings_path)
    if os.path.isfile(expanded):
        try:
            settings = _load_json(expanded) or {}
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

    if auth.get("selectedType") == gemini_auth_type:
        return

    auth["selectedType"] = gemini_auth_type
    _write_json(expanded, settings)


def _build_env_overlay(method: str, env_key: str) -> dict[str, str]:
    """Build the env vars overlay for outputs/env.json.

    Mirrors the env updates the compiled GeminiCLI.Provision() writes
    into scion-agent.json.
    """
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


def _provision(manifest: dict[str, Any]) -> int:
    bundle = manifest.get("harness_bundle_dir") or "$HOME/.scion/harness"
    bundle = _expand(bundle)
    workspace = manifest.get("agent_workspace") or "/workspace"

    inputs_dir = os.path.join(bundle, "inputs")
    auth_candidates_path = os.path.join(inputs_dir, "auth-candidates.json")

    candidates: dict[str, Any] = {}
    if os.path.isfile(auth_candidates_path):
        try:
            candidates = _load_json(auth_candidates_path) or {}
        except (OSError, json.JSONDecodeError) as exc:
            print(f"gemini provision: invalid auth-candidates.json: {exc}", file=sys.stderr)
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    file_paths = _present_file_paths(candidates)
    secret_files = _env_secret_files(candidates)

    harness_cfg = manifest.get("harness_config") or {}
    no_auth_cfg = harness_cfg.get("no_auth") or {}
    no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

    if not candidates and no_auth_behavior:
        print(f"gemini provision: no-auth mode (behavior={no_auth_behavior}), skipping auth setup", file=sys.stderr)
        method = "none"
        env_key = ""
    else:
        try:
            method, env_key = _select_auth_method(explicit, env_keys, file_paths)
        except ValueError as exc:
            print(str(exc), file=sys.stderr)
            return EXIT_ERROR

    outputs = manifest.get("outputs") or {}
    env_out = _expand(outputs.get("env") or os.path.join(bundle, "outputs", "env.json"))
    auth_out = _expand(outputs.get("resolved_auth") or os.path.join(bundle, "outputs", "resolved-auth.json"))

    # 1. Update ~/.gemini/settings.json with the resolved auth type.
    gemini_auth_type = _GEMINI_AUTH_TYPE_MAP.get(method, "")
    if gemini_auth_type:
        try:
            _update_gemini_settings(GEMINI_SETTINGS_FILE, gemini_auth_type)
        except OSError as exc:
            print(f"gemini provision: failed to update gemini settings: {exc}", file=sys.stderr)
            return EXIT_ERROR

    # 2. Build resolved-auth output.
    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "gemini-cli",
        "method": method,
        "explicit_type": explicit or None,
    }
    if method == "api-key":
        resolved_payload["env_var"] = env_key
    elif method == "auth-file":
        resolved_payload["auth_file"] = GEMINI_OAUTH_CREDS_FILE
    elif method == "vertex-ai":
        resolved_payload["vertex_ai"] = True

    # 3. Build env overlay.
    env_payload = _build_env_overlay(method, env_key)

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(f"gemini provision: failed to write outputs: {exc}", file=sys.stderr)
        return EXIT_ERROR

    print(f"gemini provision: method={method}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(f"gemini provision: unsupported command {command!r}", file=sys.stderr)
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(description="Gemini CLI container-side provisioner")
    parser.add_argument(
        "--manifest",
        help="Path to the staged manifest.json (defaults to $HOME/.scion/harness/manifest.json)",
        default=None,
    )
    args = parser.parse_args()

    manifest_path = args.manifest
    if not manifest_path:
        home = os.environ.get("HOME") or os.path.expanduser("~")
        manifest_path = os.path.join(home, ".scion", "harness", "manifest.json")

    try:
        manifest = _load_json(manifest_path)
    except FileNotFoundError:
        print(f"gemini provision: manifest not found at {manifest_path}", file=sys.stderr)
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(f"gemini provision: failed to load manifest {manifest_path}: {exc}", file=sys.stderr)
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("gemini provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
