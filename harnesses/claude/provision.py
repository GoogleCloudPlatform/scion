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
"""Claude Code container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.
  * Mounted any auth file (e.g. ~/.claude/.credentials.json) at the declared
    container_path, when auth-file mode is in use.
  * Mounted ADC credentials when vertex-ai mode is in use.

This script's job mirrors the compiled ClaudeCode harness:

  1. Determine which auth method Claude Code will use, honoring an explicit
     selection if present and otherwise applying the same precedence as the
     compiled harness:
         ANTHROPIC_API_KEY > CLAUDE_CODE_OAUTH_TOKEN > auth-file > vertex-ai.
  2. For api-key auth, pre-approve the key by writing the last 20 chars of the
     API key as a fingerprint in .claude.json's customApiKeyResponses so
     Claude Code does not prompt for confirmation.
  3. Update .claude.json project paths to point at the container workspace.
  4. Translate universal MCP servers into Claude Code's native mcpServers
     format in .claude.json.
  5. Write outputs/resolved-auth.json describing the chosen method.
  6. Write outputs/env.json with env vars to project into the harness process
     (e.g. ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, or Vertex AI vars).

The script is intentionally stdlib-only so it works on any container image
that ships python3 (declared in config.yaml's required_image_tools).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

# Add the bundle dir to sys.path so we can import the staged scion_harness
# helper module (sibling file of this script).
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

try:
    import scion_harness  # type: ignore[import-not-found]
except ImportError:
    scion_harness = None  # type: ignore[assignment]

CLAUDE_JSON_FILE = "~/.claude.json"
CLAUDE_AUTH_FILE = "~/.claude/.credentials.json"
ADC_FILE = "~/.config/gcloud/application_default_credentials.json"

VALID_AUTH_TYPES = ("api-key", "oauth-token", "auth-file", "vertex-ai")

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
    """Map of env-var name -> path to secret value file staged by the host."""
    raw = candidates.get("env_secret_files") or {}
    if not isinstance(raw, dict):
        return {}
    return {str(k): str(v) for k, v in raw.items() if isinstance(k, str) and isinstance(v, str)}


def _read_secret(secret_files: dict[str, str], env_name: str) -> str:
    """Read the secret value from a staged secret file. Returns empty string if unavailable."""
    path = secret_files.get(env_name, "")
    if not path:
        return ""
    expanded = _expand(path)
    try:
        with open(expanded, "r", encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return ""


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
    name when method == 'api-key', or the chosen GCP region env var name when
    method == 'vertex-ai', else "". Raises ValueError on no-creds.
    """
    has_anthropic = "ANTHROPIC_API_KEY" in env_keys
    has_oauth = "CLAUDE_CODE_OAUTH_TOKEN" in env_keys
    has_authfile = _auth_file_present(file_paths, CLAUDE_AUTH_FILE)
    has_gcp_project = "GOOGLE_CLOUD_PROJECT" in env_keys
    _gcp_region_key = next(
        (k for k in ('GOOGLE_CLOUD_LOCATION', 'GOOGLE_CLOUD_REGION') if k in env_keys),
        "",
    )
    has_gcp_region = bool(_gcp_region_key)

    if explicit:
        if explicit not in VALID_AUTH_TYPES:
            raise ValueError(
                f"claude: unknown auth type {explicit!r}; valid types are: "
                f"{', '.join(VALID_AUTH_TYPES)}"
            )
        if explicit == "api-key":
            if has_anthropic:
                return "api-key", "ANTHROPIC_API_KEY"
            raise ValueError(
                "claude: auth type 'api-key' selected but no API key found; "
                "set ANTHROPIC_API_KEY"
            )
        if explicit == "oauth-token":
            if has_oauth:
                return "oauth-token", "CLAUDE_CODE_OAUTH_TOKEN"
            raise ValueError(
                "claude: auth type 'oauth-token' selected but no OAuth token found; "
                "set CLAUDE_CODE_OAUTH_TOKEN (generate with `claude setup-token`)"
            )
        if explicit == "auth-file":
            if not has_authfile:
                raise ValueError(
                    "claude: auth type 'auth-file' selected but no credentials file "
                    f"found; expected {CLAUDE_AUTH_FILE}"
                )
            return "auth-file", ""
        if explicit == "vertex-ai":
            if not has_gcp_project or not has_gcp_region:
                raise ValueError(
                    "claude: auth type 'vertex-ai' selected but GOOGLE_CLOUD_PROJECT "
                    "and/or GOOGLE_CLOUD_LOCATION / GOOGLE_CLOUD_REGION not set"
                )
            return "vertex-ai", _gcp_region_key

    # Auto-detect precedence matches the compiled ClaudeCode harness:
    # API key -> OAuth token -> credentials file -> Vertex AI
    if has_anthropic:
        return "api-key", "ANTHROPIC_API_KEY"
    if has_oauth:
        return "oauth-token", "CLAUDE_CODE_OAUTH_TOKEN"
    if has_authfile:
        return "auth-file", ""
    if has_gcp_project and has_gcp_region:
        return "vertex-ai", _gcp_region_key

    raise ValueError(
        "claude: no valid auth method found; set ANTHROPIC_API_KEY for direct API "
        "access, CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`) or "
        f"{CLAUDE_AUTH_FILE} for subscription auth, or provide "
        "GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_LOCATION/GOOGLE_CLOUD_REGION "
        "(with ADC or GCP service account) for Vertex AI"
    )


def _apply_api_key_approval(claude_json_path: str, api_key: str) -> None:
    """Pre-approve the API key in .claude.json.

    Writes the last 20 characters of the key as a fingerprint in
    customApiKeyResponses.approved so Claude Code does not prompt for
    confirmation. Mirrors ClaudeCode.ApplyAuthSettings in claude_code.go.
    """
    if not api_key:
        return

    fingerprint = api_key[-20:] if len(api_key) > 20 else api_key

    cfg: dict[str, Any] = {}
    if os.path.isfile(claude_json_path):
        try:
            cfg = _load_json(claude_json_path) or {}
        except (OSError, json.JSONDecodeError):
            cfg = {}
    if not isinstance(cfg, dict):
        cfg = {}

    cfg["customApiKeyResponses"] = {
        "approved": [fingerprint],
        "rejected": [],
    }

    _write_json(claude_json_path, cfg)


def _update_project_paths(claude_json_path: str, workspace: str) -> None:
    """Update .claude.json project paths to point at the container workspace.

    Mirrors ClaudeCode.provisionClaudeJSON in claude_code.go. Takes the first
    existing project entry's settings and re-keys it to the container workspace
    path. If no project entries exist, creates a default settings map.
    """
    cfg: dict[str, Any] = {}
    if os.path.isfile(claude_json_path):
        try:
            cfg = _load_json(claude_json_path) or {}
        except (OSError, json.JSONDecodeError):
            cfg = {}
    if not isinstance(cfg, dict):
        cfg = {}

    projects = cfg.get("projects")
    if not isinstance(projects, dict):
        projects = {}

    # Grab the first existing project's settings as the template.
    project_settings: Any = None
    for v in projects.values():
        project_settings = v
        break

    if project_settings is None:
        project_settings = {
            "allowedTools": [],
            "mcpContextUris": [],
            "mcpServers": {},
            "enabledMcpjsonServers": [],
            "disabledMcpjsonServers": [],
            "hasTrustDialogAccepted": True,
            "projectOnboardingSeenCount": 1,
            "hasClaudeMdExternalIncludesApproved": False,
            "hasClaudeMdExternalIncludesWarningShown": False,
            "exampleFiles": [],
        }

    cfg["projects"] = {workspace: project_settings}
    _write_json(claude_json_path, cfg)


def _apply_mcp_servers(bundle: str, workspace: str) -> int:
    """Read inputs/mcp-servers.json and merge into .claude.json.

    Claude Code's MCP schema is nearly 1:1 with the universal format. We use
    scion_harness.apply_mcp_servers_simple() when available, falling back to
    inline logic otherwise.

    Returns the number of servers written. 0 if no MCP input is staged.
    """
    if scion_harness is None:
        servers = _read_mcp_servers_inline(bundle)
    else:
        try:
            servers = scion_harness.read_mcp_servers(bundle)
        except ValueError as exc:
            print(f"claude provision: {exc}", file=sys.stderr)
            return 0

    if not servers:
        return 0

    # Claude Code's native MCP config lives in .claude.json under
    # projects.<workspace>.mcpServers for project-scoped entries and
    # mcpServers at the top level for global entries. The apply_mcp_servers_simple
    # helper handles both via the mapping config.
    if scion_harness is not None:
        mcp_mapping = {
            "transport_field": "type",
            "transport_map": {
                "stdio": "stdio",
                "sse": "sse",
                "streamable-http": "streamable-http",
            },
            "global_config_file": _expand(CLAUDE_JSON_FILE),
            "global_config_path": "mcpServers",
            "project_config_file": _expand(CLAUDE_JSON_FILE),
            "project_config_path": f"projects.{workspace}.mcpServers",
        }
        try:
            count = scion_harness.apply_mcp_servers_simple(bundle, mcp_mapping, workspace)
        except (OSError, ValueError) as exc:
            print(f"claude provision: mcp merge failed: {exc}", file=sys.stderr)
            return 0
        if count > 0:
            print(f"claude provision: applied {count} mcp server(s)", file=sys.stderr)
        return count

    # Inline fallback when scion_harness is not available. Only writes to
    # global mcpServers (not project-scoped) — intentional since scion_harness
    # should always be available in production.
    claude_json_path = _expand(CLAUDE_JSON_FILE)
    cfg: dict[str, Any] = {}
    if os.path.isfile(claude_json_path):
        try:
            cfg = _load_json(claude_json_path) or {}
        except (OSError, json.JSONDecodeError):
            cfg = {}
    if not isinstance(cfg, dict):
        cfg = {}

    # Merge all servers into mcpServers at the top level (global).
    mcp_block = cfg.get("mcpServers")
    if not isinstance(mcp_block, dict):
        mcp_block = {}
    for name, spec in servers.items():
        if not isinstance(spec, dict):
            continue
        # Claude's native MCP shape is close to 1:1; we pass through most fields.
        native: dict[str, Any] = {}
        for key, value in spec.items():
            if key == "transport":
                native["type"] = value
            elif key == "scope":
                continue
            else:
                native[key] = value
        mcp_block[name] = native
    cfg["mcpServers"] = mcp_block
    _write_json(claude_json_path, cfg)

    count = len(servers)
    print(f"claude provision: applied {count} mcp server(s)", file=sys.stderr)
    return count


def _read_mcp_servers_inline(bundle: str) -> dict[str, dict[str, Any]]:
    """Fallback when scion_harness import fails."""
    path = os.path.join(bundle, "inputs", "mcp-servers.json")
    if not os.path.isfile(path):
        return {}
    try:
        payload = _load_json(path) or {}
    except (OSError, json.JSONDecodeError) as exc:
        print(f"claude provision: invalid mcp-servers.json: {exc}", file=sys.stderr)
        return {}
    if not isinstance(payload, dict):
        return {}
    servers = payload.get("mcp_servers") or {}
    if not isinstance(servers, dict):
        return {}
    return {str(k): v for k, v in servers.items() if isinstance(v, dict)}


def _build_env_overlay(method: str, env_key: str) -> dict[str, str]:
    """Build the env vars overlay for outputs/env.json.

    These mirror the env updates the compiled ClaudeCode.Provision() writes
    into scion-agent.json. The container runtime reads env.json and projects
    the vars into the harness process environment.
    """
    if method == "api-key" and env_key:
        return {env_key: f"${{{env_key}}}"}
    if method == "oauth-token":
        return {"CLAUDE_CODE_OAUTH_TOKEN": "${CLAUDE_CODE_OAUTH_TOKEN}"}
    if method == "vertex-ai":
        region_ref = env_key or "GOOGLE_CLOUD_REGION"
        return {
            "CLAUDE_CODE_USE_VERTEX": "1",
            "ANTHROPIC_VERTEX_PROJECT_ID": "${GOOGLE_CLOUD_PROJECT}",
            "CLOUD_ML_REGION": f"${{{region_ref}}}",
        }
    # auth-file: no env updates needed.
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
            print(f"claude provision: invalid auth-candidates.json: {exc}", file=sys.stderr)
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    file_paths = _present_file_paths(candidates)
    secret_files = _env_secret_files(candidates)

    harness_cfg = manifest.get("harness_config") or {}
    no_auth_cfg = harness_cfg.get("no_auth") or {}
    no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

    if not candidates and no_auth_behavior:
        print(f"claude provision: no-auth mode (behavior={no_auth_behavior}), skipping auth setup", file=sys.stderr)
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

    claude_json_path = _expand(CLAUDE_JSON_FILE)

    # 1. Update project paths in .claude.json.
    try:
        _update_project_paths(claude_json_path, workspace)
    except OSError as exc:
        print(f"claude provision: failed to update project paths: {exc}", file=sys.stderr)
        return EXIT_ERROR

    # 2. For api-key auth, pre-approve the key so Claude Code doesn't prompt.
    if method == "api-key" and env_key:
        api_key_value = _read_secret(secret_files, env_key)
        if api_key_value:
            try:
                _apply_api_key_approval(claude_json_path, api_key_value)
            except OSError as exc:
                print(f"claude provision: failed to write API key approval: {exc}", file=sys.stderr)
                # Non-fatal: Claude Code will prompt but still work.

    # 3. Build resolved-auth output.
    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "claude",
        "method": method,
        "explicit_type": explicit or None,
    }
    if method == "api-key":
        resolved_payload["env_var"] = env_key
    elif method == "auth-file":
        resolved_payload["auth_file"] = CLAUDE_AUTH_FILE
    elif method == "vertex-ai":
        resolved_payload["vertex_ai"] = True

    # 4. Build env overlay — mirrors ClaudeCode.Provision() env updates.
    env_payload = _build_env_overlay(method, env_key)

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(f"claude provision: failed to write outputs: {exc}", file=sys.stderr)
        return EXIT_ERROR

    # 5. Apply universal MCP servers if any. Failures are warnings, not
    # provisioning errors — auth is the hard gate.
    _apply_mcp_servers(bundle, workspace)

    print(f"claude provision: method={method}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(f"claude provision: unsupported command {command!r}", file=sys.stderr)
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(description="Claude Code container-side provisioner")
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
        print(f"claude provision: manifest not found at {manifest_path}", file=sys.stderr)
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(f"claude provision: failed to load manifest {manifest_path}: {exc}", file=sys.stderr)
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("claude provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
