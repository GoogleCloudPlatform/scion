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
"""Amp container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names and paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.
  * Mounted no auth files (Amp uses env vars only; it relies on the OS keychain
    or AMP_API_KEY, but keychain is not available in containers).

This script's responsibilities:

  1. Determine which auth method Amp will use, honoring an explicit selection
     if present, otherwise applying precedence: AMP_API_KEY > ANTHROPIC_API_KEY.
  2. Read the secret value from the staged secrets/<NAME> file and project it
     into outputs/env.json as AMP_API_KEY so Amp picks it up from the env
     overlay that sciontool init loads before launching the agent process.
  3. Reconcile ~/.config/amp/settings.json: ensure harness-required defaults
     (dangerouslyAllowAll, terminal theme) are present without clobbering any
     keys the user may have placed in the home overlay.
  4. Write outputs/resolved-auth.json describing the chosen method.

Telemetry and hook dialect setup are not performed: Amp has no native OTEL
integration and no structured hook dialect in scope for this example.

The script is stdlib-only so it works on any container image that ships
python3 (declared in config.yaml's required_image_tools).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

AMP_SETTINGS_FILE = "~/.config/amp/settings.json"

# Default settings required for non-interactive container operation.
# Applied as fallback values: existing keys in the file are never overwritten.
_DEFAULT_SETTINGS: dict[str, Any] = {
    "amp.dangerouslyAllowAll": True,
    "amp.terminal.theme": "plain",
}

VALID_AUTH_TYPES = ("api-key",)

# Exit codes mirror the contract documented in the design doc:
#   0 = success
#   1 = error (stderr is captured and surfaced)
#   2 = unsupported command (treated as no-op for optional operations)
EXIT_OK = 0
EXIT_ERROR = 1
EXIT_UNSUPPORTED = 2


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


def _env_secret_files(candidates: dict[str, Any]) -> dict[str, str]:
    """Map of env-var name -> container path of its 0600 secret value file."""
    raw = candidates.get("env_secret_files") or {}
    out: dict[str, str] = {}
    if not isinstance(raw, dict):
        return out
    for k, v in raw.items():
        if isinstance(k, str) and isinstance(v, str) and v:
            out[k] = v
    return out


def _read_secret(env_secret_files: dict[str, str], name: str) -> str:
    """Read the 0600 secret value file for an env var. Returns "" on miss."""
    path = env_secret_files.get(name)
    if not path:
        return ""
    real = _expand(path)
    try:
        with open(real, "r", encoding="utf-8") as f:
            return f.read().rstrip("\r\n")
    except OSError:
        return ""


def _select_auth_method(explicit: str, env_keys: set[str]) -> tuple[str, str]:
    """Pick an auth method.

    Returns (method, env_key). Raises ValueError if no credentials are found.
    Precedence: AMP_API_KEY > ANTHROPIC_API_KEY.
    """
    has_amp = "AMP_API_KEY" in env_keys
    has_anthropic = "ANTHROPIC_API_KEY" in env_keys

    if explicit:
        if explicit not in VALID_AUTH_TYPES:
            raise ValueError(
                f"amp: unknown auth type {explicit!r}; valid types are: "
                f"{', '.join(VALID_AUTH_TYPES)}"
            )
        if explicit == "api-key":
            if has_amp:
                return "api-key", "AMP_API_KEY"
            if has_anthropic:
                return "api-key", "ANTHROPIC_API_KEY"
            raise ValueError(
                "amp: auth type 'api-key' selected but no API key found; "
                "set AMP_API_KEY or ANTHROPIC_API_KEY"
            )

    # Auto-detect: AMP_API_KEY takes precedence over ANTHROPIC_API_KEY.
    if has_amp:
        return "api-key", "AMP_API_KEY"
    if has_anthropic:
        return "api-key", "ANTHROPIC_API_KEY"

    raise ValueError(
        "amp: no valid auth method found; set AMP_API_KEY or ANTHROPIC_API_KEY"
    )


def _reconcile_settings(settings_path: str) -> None:
    """Merge required defaults into settings.json without overwriting existing keys."""
    real = _expand(settings_path)
    os.makedirs(os.path.dirname(real), exist_ok=True)

    existing: dict[str, Any] = {}
    if os.path.isfile(real):
        try:
            loaded = _load_json(real)
            if isinstance(loaded, dict):
                existing = loaded
        except (OSError, json.JSONDecodeError):
            pass

    changed = False
    for key, value in _DEFAULT_SETTINGS.items():
        if key not in existing:
            existing[key] = value
            changed = True

    if changed or not os.path.isfile(real):
        _write_json(real, existing)


def _provision(manifest: dict[str, Any]) -> int:
    bundle = manifest.get("harness_bundle_dir") or "$HOME/.scion/harness"
    bundle = _expand(bundle)
    inputs_dir = os.path.join(bundle, "inputs")

    # Auth candidates — load by path; the manifest's Inputs map may be stale
    # on first provision if ApplyAuthSettings writes auth-candidates.json after
    # Provision generated the manifest.
    auth_candidates_path = os.path.join(inputs_dir, "auth-candidates.json")
    candidates: dict[str, Any] = {}
    if os.path.isfile(auth_candidates_path):
        try:
            candidates = _load_json(auth_candidates_path) or {}
        except (OSError, json.JSONDecodeError) as exc:
            print(f"amp provision: invalid auth-candidates.json: {exc}", file=sys.stderr)
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    secret_files = _env_secret_files(candidates)

    # No-auth mode: when no auth candidates were staged and the harness config
    # declares a no_auth behavior, skip auth setup entirely.
    harness_cfg = manifest.get("harness_config") or {}
    no_auth_cfg = harness_cfg.get("no_auth") or {}
    no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

    if not candidates and no_auth_behavior:
        print(f"amp provision: no-auth mode (behavior={no_auth_behavior}), skipping auth setup", file=sys.stderr)
        method = "none"
        env_key = ""
    else:
        try:
            method, env_key = _select_auth_method(explicit, env_keys)
        except ValueError as exc:
            print(str(exc), file=sys.stderr)
            return EXIT_ERROR

    # Read the secret value and project it as AMP_API_KEY so Amp can find it
    # regardless of which source key was used (AMP_API_KEY or ANTHROPIC_API_KEY).
    api_key = ""
    if method == "api-key":
        api_key = _read_secret(secret_files, env_key)
        if not api_key:
            print(
                f"amp provision: chose api-key ({env_key}) but no secret value "
                f"was staged at the recorded path; check ApplyAuthSettings",
                file=sys.stderr,
            )
            return EXIT_ERROR

    # Reconcile settings before writing outputs so any failures are reported
    # before the env overlay is committed.
    try:
        _reconcile_settings(AMP_SETTINGS_FILE)
    except OSError as exc:
        print(f"amp provision: reconcile settings.json failed: {exc}", file=sys.stderr)
        return EXIT_ERROR

    outputs = manifest.get("outputs") or {}
    env_out = _expand(outputs.get("env") or os.path.join(bundle, "outputs", "env.json"))
    auth_out = _expand(
        outputs.get("resolved_auth") or os.path.join(bundle, "outputs", "resolved-auth.json")
    )

    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "amp",
        "method": method,
        "explicit_type": explicit or None,
    }
    if method == "api-key":
        resolved_payload["env_var"] = env_key

    # Project the resolved key as AMP_API_KEY. Amp reads AMP_API_KEY from the
    # environment; normalizing here means the agent process sees a single
    # canonical variable regardless of whether the user supplied AMP_API_KEY or
    # ANTHROPIC_API_KEY.
    env_payload: dict[str, Any] = {}
    if api_key:
        env_payload["AMP_API_KEY"] = api_key

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(f"amp provision: failed to write outputs: {exc}", file=sys.stderr)
        return EXIT_ERROR

    print(f"amp provision: method={method} env_var={env_key}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(f"amp provision: unsupported command {command!r}", file=sys.stderr)
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(description="Amp container-side provisioner")
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
        print(f"amp provision: manifest not found at {manifest_path}", file=sys.stderr)
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(f"amp provision: failed to load manifest {manifest_path}: {exc}", file=sys.stderr)
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("amp provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
