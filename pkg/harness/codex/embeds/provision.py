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
"""Codex container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.
  * Written inputs/telemetry.json describing the effective TelemetryConfig
    (the same struct ApplyTelemetrySettings receives).
  * Mounted any auth file (e.g. ~/.codex/auth.json) at the declared
    container_path, when auth-file mode is in use.

This script's job mirrors the compiled Codex harness:

  1. Determine which auth method Codex will use, honoring an explicit
     selection if present and otherwise applying the same precedence as the
     compiled harness:
         CodexAPIKey > OpenAIAPIKey > CodexAuthFile.
  2. For api-key methods, read the secret value from the staged
     secrets/<NAME> file and write `~/.codex/auth.json` with
     `{"auth_mode": "apikey", "OPENAI_API_KEY": "<value>"}`. The compiled
     harness always writes the OPENAI_API_KEY field regardless of which
     env-var name was the source — Codex itself only reads OPENAI_API_KEY
     from auth.json.
  3. Reconcile the [otel] section in `~/.codex/config.toml` from
     inputs/telemetry.json. If telemetry is enabled, rebuild [otel]; if not,
     strip any pre-existing [otel] block.
  4. Write outputs/resolved-auth.json describing the chosen method.
  5. Write outputs/env.json (intentionally empty — Codex reads creds from
     auth.json, not from env, so no overlay is needed).

The script is stdlib-only; it does manual TOML editing because tomllib (3.11+)
is read-only and we must avoid third-party dependencies (the script ships in
the harness-config artifact and runs inside any image that declares python3).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from typing import Any

CODEX_AUTH_FILE = "~/.codex/auth.json"
CODEX_CONFIG_FILE = "~/.codex/config.toml"

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


def _codex_auth_file_present(file_paths: list[str]) -> bool:
    """Return True if the Codex auth file is mounted or already on disk."""
    if any(_expand(p) == _expand(CODEX_AUTH_FILE) for p in file_paths):
        return True
    return os.path.isfile(_expand(CODEX_AUTH_FILE))


def _select_auth_method(
    explicit: str,
    env_keys: set[str],
    file_paths: list[str],
) -> tuple[str, str]:
    """Pick an auth method.

    Returns (method, env_key_or_empty). env_key is the chosen API key env var
    name when method == 'api-key', else "". Raises ValueError on no-creds.
    """
    has_codex = "CODEX_API_KEY" in env_keys
    has_openai = "OPENAI_API_KEY" in env_keys
    has_authfile = _codex_auth_file_present(file_paths)

    if explicit:
        if explicit not in VALID_AUTH_TYPES:
            raise ValueError(
                f"codex: unknown auth type {explicit!r}; valid types are: "
                f"{', '.join(VALID_AUTH_TYPES)}"
            )
        if explicit == "api-key":
            if has_codex:
                return "api-key", "CODEX_API_KEY"
            if has_openai:
                return "api-key", "OPENAI_API_KEY"
            raise ValueError(
                "codex: auth type 'api-key' selected but no API key found; "
                "set CODEX_API_KEY or OPENAI_API_KEY"
            )
        if explicit == "auth-file":
            if not has_authfile:
                raise ValueError(
                    "codex: auth type 'auth-file' selected but no auth file "
                    f"found; expected {CODEX_AUTH_FILE}"
                )
            return "auth-file", ""

    # Auto-detect precedence matches the compiled Codex harness.
    if has_codex:
        return "api-key", "CODEX_API_KEY"
    if has_openai:
        return "api-key", "OPENAI_API_KEY"
    if has_authfile:
        return "auth-file", ""

    raise ValueError(
        "codex: no valid auth method found; set CODEX_API_KEY or OPENAI_API_KEY,"
        f" or provide auth credentials at {CODEX_AUTH_FILE}"
    )


def _write_codex_auth_json(api_key: str) -> None:
    """Mirror the compiled ApplyAuthSettings: {"auth_mode": "apikey", ...}."""
    auth_dir = _expand("~/.codex")
    os.makedirs(auth_dir, exist_ok=True)
    target = os.path.join(auth_dir, "auth.json")
    payload = {"auth_mode": "apikey", "OPENAI_API_KEY": api_key}
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(payload, f, indent=2)
        f.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


# --- TOML reconciliation ---------------------------------------------------
#
# We do NOT use tomllib because (a) it is read-only, (b) we must preserve
# user-edited keys outside [otel], (c) the file is small enough for line-based
# rewriting, and (d) the compiled harness does the same thing in Go.
# Keep behavior byte-equivalent to pkg/harness/codex_config.go.


_SECTION_RE = re.compile(r"^\s*\[([^\]\s]+)(?:\.[^\]]*)?\]\s*$")


def _strip_otel_section(content: str) -> str:
    """Remove the [otel] section (and any blank lines immediately before it).

    Sub-tables under [otel] (e.g. an inline [otel.foo]) are NOT in use today,
    but matching the Go behavior: we stop at the next bracketed top-level
    section header. The Go implementation uses simple bracket matching, so we
    do the same for parity.
    """
    lines = content.split("\n")
    target = "[otel]"
    section_start = -1
    section_end = len(lines)
    for i, line in enumerate(lines):
        if line.strip() == target:
            section_start = i
            for j in range(i + 1, len(lines)):
                t = lines[j].strip()
                if t.startswith("[") and t.endswith("]"):
                    section_end = j
                    break
            break
    if section_start == -1:
        return content
    while section_start > 0 and lines[section_start - 1].strip() == "":
        section_start -= 1
    return "\n".join(lines[:section_start] + lines[section_end:])


def _list_contains(items: list[Any], target: str) -> bool:
    for item in items or []:
        if isinstance(item, str) and item.strip() == target:
            return True
    return False


def _resolve_endpoint(telemetry: dict[str, Any] | None, env: dict[str, str] | None) -> str:
    env = env or {}
    for key in ("SCION_CODEX_OTEL_ENDPOINT", "SCION_OTEL_ENDPOINT"):
        v = (env.get(key) or "").strip()
        if v:
            return v
    if telemetry and isinstance(telemetry.get("cloud"), dict):
        ep = (telemetry["cloud"].get("endpoint") or "").strip()
        if ep:
            return ep
    return "localhost:4317"


def _resolve_protocol(telemetry: dict[str, Any] | None, env: dict[str, str] | None) -> str:
    env = env or {}
    for key in ("SCION_CODEX_OTEL_PROTOCOL", "SCION_OTEL_PROTOCOL"):
        v = (env.get(key) or "").strip()
        if v:
            return v
    if telemetry and isinstance(telemetry.get("cloud"), dict):
        proto = (telemetry["cloud"].get("protocol") or "").strip()
        if proto:
            return proto
    return "grpc"


def _telemetry_enabled(telemetry: dict[str, Any] | None) -> bool:
    if not telemetry:
        return False
    enabled = telemetry.get("enabled")
    if enabled is None:
        return True
    return bool(enabled)


def _build_otel_section(telemetry: dict[str, Any], env: dict[str, str] | None) -> str:
    endpoint = _resolve_endpoint(telemetry, env)
    protocol = _resolve_protocol(telemetry, env)

    log_user_prompt = False
    flt = telemetry.get("filter") or {}
    events = flt.get("events") if isinstance(flt, dict) else None
    if isinstance(events, dict):
        if _list_contains(events.get("include") or [], "agent.user.prompt"):
            log_user_prompt = True
        if _list_contains(events.get("exclude") or [], "agent.user.prompt"):
            log_user_prompt = False

    exporter_key = "otlp-grpc"
    if protocol in ("http", "http/protobuf"):
        exporter_key = "otlp-http"

    headers_str = ""
    cloud = telemetry.get("cloud") or {}
    headers = cloud.get("headers") if isinstance(cloud, dict) else None
    if isinstance(headers, dict) and headers:
        parts = [f'"{k}" = "{v}"' for k, v in headers.items()]
        parts.sort()
        headers_str = ",\n  headers = { " + ", ".join(parts) + " }"

    log_user_prompt_str = "true" if log_user_prompt else "false"
    return (
        f"[otel]\nenabled = true\nlog_user_prompt = {log_user_prompt_str}\n"
        f"exporter = {{ {exporter_key} = {{\n"
        f'  endpoint = "{endpoint}"{headers_str}\n'
        "}}\n"
    )


def _reconcile_codex_toml(telemetry: dict[str, Any] | None, env: dict[str, str] | None) -> None:
    codex_dir = _expand("~/.codex")
    os.makedirs(codex_dir, exist_ok=True)
    config_path = os.path.join(codex_dir, "config.toml")
    content = ""
    if os.path.isfile(config_path):
        with open(config_path, "r", encoding="utf-8") as f:
            content = f.read()
    content = _strip_otel_section(content)
    if _telemetry_enabled(telemetry):
        section = _build_otel_section(telemetry or {}, env)
        content = content.rstrip("\n\t ") + "\n\n" + section
    content = content.strip() + "\n"
    tmp = config_path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(content)
    os.replace(tmp, config_path)


# --- Entry point -----------------------------------------------------------


def _provision(manifest: dict[str, Any]) -> int:
    bundle = manifest.get("harness_bundle_dir") or "$HOME/.scion/harness"
    bundle = _expand(bundle)
    inputs_dir = os.path.join(bundle, "inputs")

    # Auth candidates — load by path because ApplyAuthSettings may write
    # auth-candidates.json AFTER Provision generated the manifest, so the
    # manifest's Inputs.AuthCandidates may be empty on first provision.
    auth_candidates_path = os.path.join(inputs_dir, "auth-candidates.json")
    candidates: dict[str, Any] = {}
    if os.path.isfile(auth_candidates_path):
        try:
            candidates = _load_json(auth_candidates_path) or {}
        except (OSError, json.JSONDecodeError) as exc:
            print(f"codex provision: invalid auth-candidates.json: {exc}", file=sys.stderr)
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    file_paths = _present_file_paths(candidates)
    secret_files = _env_secret_files(candidates)

    try:
        method, env_key = _select_auth_method(explicit, env_keys, file_paths)
    except ValueError as exc:
        print(str(exc), file=sys.stderr)
        return EXIT_ERROR

    # Apply api-key auth: read the staged secret value and write .codex/auth.json.
    if method == "api-key":
        api_key = _read_secret(secret_files, env_key)
        if not api_key:
            print(
                f"codex provision: chose api-key ({env_key}) but no secret value "
                f"was staged at the recorded path; check ApplyAuthSettings",
                file=sys.stderr,
            )
            return EXIT_ERROR
        try:
            _write_codex_auth_json(api_key)
        except OSError as exc:
            print(f"codex provision: write auth.json failed: {exc}", file=sys.stderr)
            return EXIT_ERROR

    # Telemetry — load by path; the manifest's Inputs.Telemetry may be stale on
    # first provision for the same reason as auth-candidates.json.
    telemetry_path = os.path.join(inputs_dir, "telemetry.json")
    telemetry_payload: dict[str, Any] = {}
    if os.path.isfile(telemetry_path):
        try:
            telemetry_payload = _load_json(telemetry_path) or {}
        except (OSError, json.JSONDecodeError) as exc:
            print(f"codex provision: invalid telemetry.json: {exc}", file=sys.stderr)
            return EXIT_ERROR

    telemetry = telemetry_payload.get("telemetry") if isinstance(telemetry_payload, dict) else None
    env_overlay = telemetry_payload.get("env") if isinstance(telemetry_payload, dict) else None
    if not isinstance(env_overlay, dict):
        env_overlay = None
    try:
        _reconcile_codex_toml(telemetry if isinstance(telemetry, dict) else None, env_overlay)
    except OSError as exc:
        print(f"codex provision: reconcile config.toml failed: {exc}", file=sys.stderr)
        return EXIT_ERROR

    # Outputs.
    outputs = manifest.get("outputs") or {}
    env_out = _expand(outputs.get("env") or os.path.join(bundle, "outputs", "env.json"))
    auth_out = _expand(outputs.get("resolved_auth") or os.path.join(bundle, "outputs", "resolved-auth.json"))

    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "codex",
        "method": method,
        "explicit_type": explicit or None,
    }
    if method == "api-key":
        resolved_payload["env_var"] = env_key
        resolved_payload["auth_file_written"] = CODEX_AUTH_FILE
    elif method == "auth-file":
        resolved_payload["auth_file"] = CODEX_AUTH_FILE

    # Codex reads its credentials from .codex/auth.json, not from env, so the
    # env overlay is intentionally empty. We still emit a well-formed file so
    # sciontool init's overlay loader has a target to consume.
    env_payload: dict[str, Any] = {}

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(f"codex provision: failed to write outputs: {exc}", file=sys.stderr)
        return EXIT_ERROR

    print(f"codex provision: method={method}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(f"codex provision: unsupported command {command!r}", file=sys.stderr)
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(description="Codex container-side provisioner")
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
        print(f"codex provision: manifest not found at {manifest_path}", file=sys.stderr)
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(f"codex provision: failed to load manifest {manifest_path}: {exc}", file=sys.stderr)
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("codex provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
