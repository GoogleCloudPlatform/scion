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
"""OpenCode container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Projected available auth env vars into the container's launch environment
    (so the OpenCode child process will see ANTHROPIC_API_KEY, OPENAI_API_KEY,
    etc. — but `sciontool harness provision` strips them from THIS script's env
    for containment, so we read the *names* of available creds from
    inputs/auth-candidates.json instead of os.environ).
  * Mounted any auth file (e.g. ~/.local/share/opencode/auth.json) at the
    declared container_path.

This script's job is therefore minimal:

  1. Determine which auth method OpenCode will use, honoring an explicit
     selection if present and otherwise applying the same precedence as the
     compiled OpenCode harness:
         AnthropicAPIKey > OpenAIAPIKey > OpenCodeAuthFile.
  2. Fail (exit 1) with an actionable message if no method is available.
  3. Write outputs/resolved-auth.json describing the choice (for diagnostics
     and resume-time consistency).
  4. Write outputs/env.json — for OpenCode this is intentionally empty: the
     harness child already inherits the projected env, and OpenCode performs
     its own env-key precedence internally. We do not need to override.

The script is intentionally stdlib-only so it works on any container image
that ships python3 (declared in config.yaml's required_image_tools).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

OPENCODE_AUTH_FILE = "~/.local/share/opencode/auth.json"

VALID_AUTH_TYPES = ("api-key", "auth-file")

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


def _opencode_auth_file_present(file_paths: list[str]) -> bool:
    """Return True if the OpenCode auth file is mounted or already on disk."""
    if any(_expand(p) == _expand(OPENCODE_AUTH_FILE) for p in file_paths):
        return True
    # Defensive: the script may run on a resume where the candidates list is
    # stale but the file is on disk.
    return os.path.isfile(_expand(OPENCODE_AUTH_FILE))


def _select_auth_method(explicit: str, env_keys: set[str], file_paths: list[str]) -> tuple[str, str]:
    """Pick an auth method.

    Returns (method, env_key_or_empty). env_key is the chosen API key env var
    name when method == 'api-key', else "". Raises ValueError on no-creds.
    """
    has_anthropic = "ANTHROPIC_API_KEY" in env_keys
    has_openai = "OPENAI_API_KEY" in env_keys
    has_authfile = _opencode_auth_file_present(file_paths)

    if explicit:
        if explicit not in VALID_AUTH_TYPES:
            raise ValueError(
                f"opencode: unknown auth type {explicit!r}; valid types are: "
                f"{', '.join(VALID_AUTH_TYPES)}"
            )
        if explicit == "api-key":
            if has_anthropic:
                return "api-key", "ANTHROPIC_API_KEY"
            if has_openai:
                return "api-key", "OPENAI_API_KEY"
            raise ValueError(
                "opencode: auth type 'api-key' selected but no API key found; "
                "set ANTHROPIC_API_KEY or OPENAI_API_KEY"
            )
        if explicit == "auth-file":
            if not has_authfile:
                raise ValueError(
                    "opencode: auth type 'auth-file' selected but no auth file "
                    f"found; expected {OPENCODE_AUTH_FILE}"
                )
            return "auth-file", ""

    # Auto-detect precedence matches the compiled OpenCode harness.
    if has_anthropic:
        return "api-key", "ANTHROPIC_API_KEY"
    if has_openai:
        return "api-key", "OPENAI_API_KEY"
    if has_authfile:
        return "auth-file", ""

    raise ValueError(
        "opencode: no valid auth method found; set ANTHROPIC_API_KEY or "
        f"OPENAI_API_KEY, or provide auth credentials at {OPENCODE_AUTH_FILE}"
    )


def _provision(manifest: dict[str, Any]) -> int:
    bundle = manifest.get("harness_bundle_dir") or "$HOME/.scion/harness"
    bundle = _expand(bundle)

    # Inputs directory is fixed by the staging contract; we don't trust the
    # manifest's Inputs map alone because ApplyAuthSettings may write
    # auth-candidates.json AFTER Provision generated the manifest.
    inputs_dir = os.path.join(bundle, "inputs")
    auth_candidates_path = os.path.join(inputs_dir, "auth-candidates.json")

    candidates: dict[str, Any] = {}
    if os.path.isfile(auth_candidates_path):
        try:
            candidates = _load_json(auth_candidates_path) or {}
        except (OSError, json.JSONDecodeError) as exc:
            print(f"opencode provision: invalid auth-candidates.json: {exc}", file=sys.stderr)
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    file_paths = _present_file_paths(candidates)

    try:
        method, env_key = _select_auth_method(explicit, env_keys, file_paths)
    except ValueError as exc:
        print(str(exc), file=sys.stderr)
        return EXIT_ERROR

    outputs = manifest.get("outputs") or {}
    env_out = _expand(outputs.get("env") or os.path.join(bundle, "outputs", "env.json"))
    auth_out = _expand(outputs.get("resolved_auth") or os.path.join(bundle, "outputs", "resolved-auth.json"))

    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "opencode",
        "method": method,
        "explicit_type": explicit or None,
    }
    if method == "api-key":
        # The actual secret value lives in the launch env; we record the env
        # var name only so the resolved-auth.json never contains a credential.
        resolved_payload["env_var"] = env_key
    elif method == "auth-file":
        resolved_payload["auth_file"] = OPENCODE_AUTH_FILE

    # OpenCode does not require additional env injection from the script. The
    # OpenCode CLI reads its own env precedence; the host already projected
    # all candidate keys. We write an empty overlay so sciontool init has a
    # well-formed file to consume.
    env_payload: dict[str, Any] = {}

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(f"opencode provision: failed to write outputs: {exc}", file=sys.stderr)
        return EXIT_ERROR

    print(f"opencode provision: method={method}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(f"opencode provision: unsupported command {command!r}", file=sys.stderr)
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(description="OpenCode container-side provisioner")
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
        print(f"opencode provision: manifest not found at {manifest_path}", file=sys.stderr)
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(f"opencode provision: failed to load manifest {manifest_path}: {exc}", file=sys.stderr)
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("opencode provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
