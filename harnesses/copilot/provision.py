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
"""Copilot container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.
  * Written inputs/instructions.md with the composed agent instructions
    (system prompt prepended when system_prompt_mode is prepend_to_instructions).
  * Written inputs/mcp-servers.json describing the MCP servers to configure.

This script's job:

  1. Determine which auth method to use, honoring an explicit selection if
     present and otherwise applying precedence:
         COPILOT_GITHUB_TOKEN > GH_TOKEN > GITHUB_TOKEN.
  2. For api-key methods, read the secret value from the staged
     secrets/<NAME> file and write it to outputs/env.json as
     {"COPILOT_GITHUB_TOKEN": "<value>"}. Unlike Codex, Copilot reads auth
     from environment variables, not from a config file.
  3. Write outputs/resolved-auth.json describing the chosen method.
  4. Copy staged instructions to .github/copilot-instructions.md in the
     workspace, using managed block markers to protect Scion-injected content.
  5. Translate MCP servers to Copilot's native format in
     ~/.copilot/mcp-config.json (stdio→local, sse/streamable-http→http).
  6. Ensure ~/.copilot/settings.json has sane defaults.

The script is stdlib-only; no external dependencies.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

try:
    import scion_harness  # type: ignore[import-not-found]
except ImportError:
    scion_harness = None  # type: ignore[assignment]

VALID_AUTH_TYPES = ("api-key",)

# Managed block markers for the instructions file.
MANAGED_BEGIN = "<!-- SCION_MANAGED_BEGIN -->"
MANAGED_END = "<!-- SCION_MANAGED_END -->"

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


_TOKEN_ENV_NAMES = ("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")


def _present_env_keys(candidates: dict[str, Any]) -> set[str]:
    """Names of auth env vars staged by the host as candidates.

    Falls back to scanning os.environ for known token env vars when the
    candidates list is empty. This covers the case where the harness-config
    was registered in the hub but the broker's env-gather couldn't resolve
    auth requirements (hub-registered configs are hydrated after env-gather).
    """
    raw = candidates.get("env_vars") or []
    if not isinstance(raw, (list, set, tuple)):
        raw = []
    keys = {str(k) for k in raw if isinstance(k, str)}
    if not keys:
        # Fallback: check container environment directly.
        keys = {name for name in _TOKEN_ENV_NAMES if os.environ.get(name)}
    return keys


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
    """Read the secret value for an env var.

    Tries the staged 0600 secret file first. Falls back to reading from
    os.environ when no secret file was staged (hub-registered harness configs
    may not have their auth requirements propagated through env-gather).
    """
    path = env_secret_files.get(name)
    if path:
        real = _expand(path)
        try:
            with open(real, "r", encoding="utf-8") as f:
                return f.read().rstrip("\r\n")
        except OSError:
            pass
    # Fallback: read from container environment.
    return os.environ.get(name, "")


def _select_auth_method(
    explicit: str,
    env_keys: set[str],
) -> tuple[str, str]:
    """Pick an auth method.

    Returns (method, env_key). env_key is the chosen env var name when
    method == 'api-key'. Raises ValueError on no-creds.
    """
    has_copilot = "COPILOT_GITHUB_TOKEN" in env_keys
    has_gh = "GH_TOKEN" in env_keys
    has_github = "GITHUB_TOKEN" in env_keys

    if explicit:
        if explicit not in VALID_AUTH_TYPES:
            raise ValueError(
                f"copilot: unknown auth type {explicit!r}; valid types are: "
                f"{', '.join(VALID_AUTH_TYPES)}"
            )
        if explicit == "api-key":
            if has_copilot:
                return "api-key", "COPILOT_GITHUB_TOKEN"
            if has_gh:
                return "api-key", "GH_TOKEN"
            if has_github:
                return "api-key", "GITHUB_TOKEN"
            raise ValueError(
                "copilot: auth type 'api-key' selected but no token found; "
                "set COPILOT_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN"
            )

    if has_copilot:
        return "api-key", "COPILOT_GITHUB_TOKEN"
    if has_gh:
        return "api-key", "GH_TOKEN"
    if has_github:
        return "api-key", "GITHUB_TOKEN"

    raise ValueError(
        "copilot: no valid auth method found; set COPILOT_GITHUB_TOKEN, "
        "GH_TOKEN, or GITHUB_TOKEN with a fine-grained PAT that has "
        '"Copilot Requests" permission'
    )


# --- Instructions projection ------------------------------------------------


def _project_instructions(bundle: str, workspace: str) -> None:
    """Copy staged instructions to .github/copilot-instructions.md.

    Uses managed block markers so Scion-injected content is identifiable
    and preserved across reprovisions without clobbering user edits outside
    the managed block.
    """
    src = os.path.join(bundle, "inputs", "instructions.md")
    if not os.path.isfile(src):
        return

    try:
        with open(src, "r", encoding="utf-8") as f:
            scion_content = f.read().strip()
    except OSError:
        return

    if not scion_content:
        return

    managed_block = f"{MANAGED_BEGIN}\n{scion_content}\n{MANAGED_END}"

    dst = os.path.join(workspace, ".github", "copilot-instructions.md")
    os.makedirs(os.path.dirname(dst), exist_ok=True)

    existing = ""
    if os.path.isfile(dst):
        try:
            with open(dst, "r", encoding="utf-8") as f:
                existing = f.read()
        except OSError:
            existing = ""

    if MANAGED_BEGIN in existing and MANAGED_END in existing:
        before = existing[: existing.index(MANAGED_BEGIN)]
        after = existing[existing.index(MANAGED_END) + len(MANAGED_END) :]
        content = before + managed_block + after
    elif existing.strip():
        content = managed_block + "\n\n" + existing
    else:
        content = managed_block + "\n"

    tmp = dst + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(content)
    os.replace(tmp, dst)
    print(
        f"copilot provision: projected instructions to {dst}",
        file=sys.stderr,
    )


# --- MCP server reconciliation ----------------------------------------------


def _translate_mcp_server(name: str, spec: dict[str, Any]) -> dict[str, Any] | None:
    """Translate a universal MCPServerConfig into Copilot's native shape.

    Returns None on skip (unsupported transport).
    """
    transport = (spec.get("transport") or "").strip()

    if transport == "stdio":
        cmd = spec.get("command")
        if not isinstance(cmd, str) or not cmd:
            print(
                f"copilot provision: mcp server {name!r}: "
                "stdio transport missing command",
                file=sys.stderr,
            )
            return None
        out: dict[str, Any] = {"type": "local", "command": cmd}
        args = spec.get("args") or []
        if isinstance(args, list) and args:
            out["args"] = [str(a) for a in args]
        env = spec.get("env")
        if isinstance(env, dict) and env:
            out["env"] = {str(k): str(v) for k, v in env.items()}
        return out

    if transport in ("sse", "streamable-http"):
        url = spec.get("url")
        if not isinstance(url, str) or not url:
            print(
                f"copilot provision: mcp server {name!r}: "
                f"{transport} transport missing url",
                file=sys.stderr,
            )
            return None
        out = {"type": "http", "url": url}
        headers = spec.get("headers")
        if isinstance(headers, dict) and headers:
            out["headers"] = {str(k): str(v) for k, v in headers.items()}
        return out

    print(
        f"copilot provision: mcp server {name!r}: "
        f"unsupported transport {transport!r}",
        file=sys.stderr,
    )
    return None


def _apply_mcp_servers(bundle: str) -> int:
    """Write MCP servers to ~/.copilot/mcp-config.json.

    Returns the number of servers written. Failures are warnings, not errors.
    """
    if scion_harness is not None:
        try:
            servers = scion_harness.read_mcp_servers(bundle)
        except ValueError as exc:
            print(f"copilot provision: {exc}", file=sys.stderr)
            return 0
    else:
        servers = _read_mcp_servers_inline(bundle)

    if not servers:
        return 0

    mcp_servers: dict[str, Any] = {}
    for name in sorted(servers.keys()):
        spec = servers[name]
        if not isinstance(spec, dict):
            continue
        scope = (spec.get("scope") or "global").strip().lower()
        if scope == "project":
            print(
                f"copilot provision: mcp server {name!r} requested project scope; "
                "registering globally (project-scoped MCP not implemented)",
                file=sys.stderr,
            )
        translated = _translate_mcp_server(name, spec)
        if translated is not None:
            mcp_servers[name] = translated

    if not mcp_servers:
        return 0

    config_dir = _expand("~/.copilot")
    try:
        os.makedirs(config_dir, exist_ok=True)
    except OSError as exc:
        print(
            f"copilot provision: could not create {config_dir}: {exc}",
            file=sys.stderr,
        )
        return 0

    config_path = os.path.join(config_dir, "mcp-config.json")
    payload = {"mcpServers": mcp_servers}
    tmp = config_path + ".tmp"
    try:
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(payload, f, indent=2, sort_keys=True)
            f.write("\n")
        os.replace(tmp, config_path)
    except OSError as exc:
        print(
            f"copilot provision: failed to write mcp-config.json: {exc}",
            file=sys.stderr,
        )
        return 0

    print(
        f"copilot provision: applied {len(mcp_servers)} mcp server(s)",
        file=sys.stderr,
    )
    return len(mcp_servers)


def _read_mcp_servers_inline(bundle: str) -> dict[str, dict[str, Any]]:
    """Fallback when scion_harness import fails."""
    path = os.path.join(bundle, "inputs", "mcp-servers.json")
    if not os.path.isfile(path):
        return {}
    try:
        payload = _load_json(path) or {}
    except (OSError, json.JSONDecodeError) as exc:
        print(
            f"copilot provision: invalid mcp-servers.json: {exc}",
            file=sys.stderr,
        )
        return {}
    if not isinstance(payload, dict):
        return {}
    servers = payload.get("mcp_servers") or {}
    if not isinstance(servers, dict):
        return {}
    return {str(k): v for k, v in servers.items() if isinstance(v, dict)}


# --- Settings ----------------------------------------------------------------


def _ensure_settings() -> None:
    """Ensure ~/.copilot/settings.json has sane defaults."""
    config_dir = _expand("~/.copilot")
    os.makedirs(config_dir, exist_ok=True)
    settings_path = os.path.join(config_dir, "settings.json")

    settings: dict[str, Any] = {}
    if os.path.isfile(settings_path):
        try:
            loaded = _load_json(settings_path)
            if isinstance(loaded, dict):
                settings = loaded
        except (OSError, json.JSONDecodeError):
            settings = {}

    workspace = os.environ.get("SCION_AGENT_WORKSPACE") or "/workspace"

    # settings.json: user-facing settings (autoUpdate, banner).
    settings_defaults = {
        "autoUpdate": False,
        "banner": "never",
    }
    changed = False
    for key, value in settings_defaults.items():
        if key not in settings:
            settings[key] = value
            changed = True

    if changed:
        tmp = settings_path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(settings, f, indent=2, sort_keys=True)
            f.write("\n")
        os.replace(tmp, settings_path)

    # config.json: auto-managed config (trustedFolders).
    # Copilot reads trustedFolders from config.json, NOT settings.json.
    config_path = os.path.join(config_dir, "config.json")
    config: dict[str, Any] = {}
    if os.path.isfile(config_path):
        try:
            # config.json may have a leading comment line; strip it.
            with open(config_path, "r", encoding="utf-8") as f:
                raw = f.read()
            # Remove lines starting with // (copilot writes a comment header).
            lines = [
                ln for ln in raw.splitlines()
                if not ln.strip().startswith("//")
            ]
            loaded = json.loads("\n".join(lines))
            if isinstance(loaded, dict):
                config = loaded
        except (OSError, json.JSONDecodeError):
            config = {}

    if "trustedFolders" not in config:
        config["trustedFolders"] = [workspace]
        tmp = config_path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(config, f, indent=2, sort_keys=True)
            f.write("\n")
        os.replace(tmp, config_path)


# --- Entry point -------------------------------------------------------------


def _provision(manifest: dict[str, Any]) -> int:
    bundle = manifest.get("harness_bundle_dir") or "$HOME/.scion/harness"
    bundle = _expand(bundle)
    inputs_dir = os.path.join(bundle, "inputs")

    auth_candidates_path = os.path.join(inputs_dir, "auth-candidates.json")
    candidates: dict[str, Any] = {}
    if os.path.isfile(auth_candidates_path):
        try:
            candidates = _load_json(auth_candidates_path) or {}
        except (OSError, json.JSONDecodeError) as exc:
            print(
                f"copilot provision: invalid auth-candidates.json: {exc}",
                file=sys.stderr,
            )
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    secret_files = _env_secret_files(candidates)

    harness_cfg = manifest.get("harness_config") or {}
    no_auth_cfg = harness_cfg.get("no_auth") or {}
    no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

    # Determine auth method. When no candidates are present (empty env_vars)
    # AND the harness config declares a no_auth behavior, skip auth gracefully
    # instead of failing. This covers both explicit --no-auth and the case
    # where the hub's env-gather couldn't resolve auth requirements for a
    # new hub-registered harness type.
    has_candidates = bool(env_keys)
    if not has_candidates and no_auth_behavior:
        print(
            f"copilot provision: no-auth mode (behavior={no_auth_behavior}), "
            "skipping auth setup",
            file=sys.stderr,
        )
        method = "none"
        env_key = ""
    else:
        try:
            method, env_key = _select_auth_method(explicit, env_keys)
        except ValueError as exc:
            if no_auth_behavior:
                print(
                    f"copilot provision: {exc} — falling back to "
                    f"no-auth mode (behavior={no_auth_behavior})",
                    file=sys.stderr,
                )
                method = "none"
                env_key = ""
            else:
                print(str(exc), file=sys.stderr)
                return EXIT_ERROR

    env_payload: dict[str, Any] = {}
    if method == "api-key":
        secret = _read_secret(secret_files, env_key)
        if not secret:
            print(
                f"copilot provision: chose api-key ({env_key}) but no secret "
                "value was staged at the recorded path; check ApplyAuthSettings",
                file=sys.stderr,
            )
            return EXIT_ERROR
        env_payload["COPILOT_GITHUB_TOKEN"] = secret

    # Outputs.
    outputs = manifest.get("outputs") or {}
    env_out = _expand(
        outputs.get("env") or os.path.join(bundle, "outputs", "env.json")
    )
    auth_out = _expand(
        outputs.get("resolved_auth")
        or os.path.join(bundle, "outputs", "resolved-auth.json")
    )

    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "copilot",
        "method": method,
        "explicit_type": explicit or None,
    }
    if method == "api-key":
        resolved_payload["env_var"] = env_key

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(
            f"copilot provision: failed to write outputs: {exc}",
            file=sys.stderr,
        )
        return EXIT_ERROR

    # Project instructions to .github/copilot-instructions.md.
    workspace = os.environ.get("SCION_WORKSPACE") or "/workspace"
    try:
        _project_instructions(bundle, workspace)
    except OSError as exc:
        print(
            f"copilot provision: failed to project instructions: {exc}",
            file=sys.stderr,
        )

    # Apply MCP servers. Failures are warnings, not provisioning errors.
    _apply_mcp_servers(bundle)

    # Ensure settings have sane defaults.
    try:
        _ensure_settings()
    except OSError as exc:
        print(
            f"copilot provision: failed to write settings: {exc}",
            file=sys.stderr,
        )

    print(f"copilot provision: method={method}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(
        f"copilot provision: unsupported command {command!r}",
        file=sys.stderr,
    )
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Copilot container-side provisioner"
    )
    parser.add_argument(
        "--manifest",
        help="Path to the staged manifest.json "
        "(defaults to $HOME/.scion/harness/manifest.json)",
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
        print(
            f"copilot provision: manifest not found at {manifest_path}",
            file=sys.stderr,
        )
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(
            f"copilot provision: failed to load manifest {manifest_path}: {exc}",
            file=sys.stderr,
        )
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("copilot provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
