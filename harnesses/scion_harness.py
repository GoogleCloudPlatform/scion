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
"""Shared helpers for scion harness provision.py scripts.

Staged into agent_home/.scion/harness/scion_harness.py during
ContainerScriptHarness.Provision(). Each harness's provision.py adds the
bundle dir to sys.path so it can `import scion_harness`.

Stdlib-only so it works in any container image that ships python3.

Provides:
  - expand_path(path): expanduser + expandvars
  - load_json(path): read JSON
  - atomic_write_json(path, data): tmp + os.replace
  - read_manifest(path): load and shape-check the staged manifest.json
  - read_mcp_servers(bundle_path): load inputs/mcp-servers.json -> dict[name -> spec]
  - apply_mcp_servers_simple(bundle_path, mcp_mapping): for harnesses whose
      native MCP config is a 1:1 JSON merge (Claude/Gemini-style). Reads the
      universal mcp_servers map, translates each entry per mcp_mapping, merges
      into the native config file at the given dotted path.

Bespoke harnesses (e.g. OpenCode) skip apply_mcp_servers_simple and translate
themselves using read_mcp_servers only.
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any


def expand_path(path: str) -> str:
    """Expand ~ and $HOME-style variables in a container path."""
    return os.path.expanduser(os.path.expandvars(path))


def load_json(path: str) -> Any:
    """Read JSON from path. Raises OSError or json.JSONDecodeError on failure."""
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def atomic_write_json(path: str, payload: Any) -> None:
    """Write JSON atomically: tmp file + os.replace, sorted keys, trailing newline."""
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(payload, f, indent=2, sort_keys=True)
        f.write("\n")
    os.replace(tmp, path)


def read_manifest(manifest_path: str | None = None) -> dict[str, Any]:
    """Load the staged manifest.json. Defaults to $HOME/.scion/harness/manifest.json.

    Raises FileNotFoundError if missing, ValueError if not a JSON object.
    """
    if not manifest_path:
        home = os.environ.get("HOME") or os.path.expanduser("~")
        manifest_path = os.path.join(home, ".scion", "harness", "manifest.json")
    with open(manifest_path, "r", encoding="utf-8") as f:
        manifest = json.load(f)
    if not isinstance(manifest, dict):
        raise ValueError(f"manifest at {manifest_path} is not a JSON object")
    return manifest


def read_mcp_servers(bundle_path: str) -> dict[str, dict[str, Any]]:
    """Load inputs/mcp-servers.json from the staged bundle.

    Returns the mcp_servers map (name -> spec). Returns an empty dict if the
    file is absent or empty (not an error: "no MCP servers to configure").
    Raises ValueError if the file is malformed.
    """
    path = os.path.join(bundle_path, "inputs", "mcp-servers.json")
    if not os.path.isfile(path):
        return {}
    try:
        payload = load_json(path) or {}
    except json.JSONDecodeError as exc:
        raise ValueError(f"invalid mcp-servers.json: {exc}") from exc
    if not isinstance(payload, dict):
        raise ValueError("mcp-servers.json is not a JSON object")
    servers = payload.get("mcp_servers") or {}
    if not isinstance(servers, dict):
        raise ValueError("mcp-servers.json: mcp_servers must be an object")
    return {str(k): v for k, v in servers.items() if isinstance(v, dict)}


def _walk_dotted_path(root: dict[str, Any], dotted: str) -> dict[str, Any]:
    """Walk root by dotted path, creating intermediate dicts as needed.

    Returns the dict at the leaf. The leaf path component is also created
    as an empty dict if it does not exist or is not a dict.
    """
    cur = root
    parts = [p for p in dotted.split(".") if p]
    for i, part in enumerate(parts):
        nxt = cur.get(part)
        if not isinstance(nxt, dict):
            nxt = {}
            cur[part] = nxt
        # On the last segment we want to return the dict at that key (the
        # caller will insert the per-server entries into it).
        if i == len(parts) - 1:
            return nxt
        cur = nxt
    # Empty dotted path -> return root itself.
    return root


def _translate_simple(spec: dict[str, Any], mapping: dict[str, Any]) -> dict[str, Any]:
    """Translate a universal MCPServerConfig into a native server entry per mapping.

    The mapping renames the transport field and maps transport values, but
    otherwise passes command/args/env/url/headers through unchanged. This
    matches Claude/Gemini's 1:1 schema.
    """
    out: dict[str, Any] = {}
    transport_field = mapping.get("transport_field") or "type"
    transport_map = mapping.get("transport_map") or {}
    for key, value in spec.items():
        if key == "transport":
            native_value = transport_map.get(value, value)
            out[transport_field] = native_value
        elif key == "scope":
            # Scope is consumed by the merger to choose global vs project,
            # not propagated to the native server entry.
            continue
        else:
            out[key] = value
    return out


def apply_mcp_servers_simple(bundle_path: str, mcp_mapping: dict[str, Any], agent_workspace: str = "") -> int:
    """Merge universal mcp_servers into native config files per the declarative mapping.

    Returns the number of server entries written across global and project
    config files. Quietly returns 0 if there is nothing to do (no inputs file,
    empty server list, or mapping has no global/project files declared).
    """
    servers = read_mcp_servers(bundle_path)
    if not servers:
        return 0
    if not mcp_mapping:
        return 0

    global_file = mcp_mapping.get("global_config_file") or ""
    global_path = mcp_mapping.get("global_config_path") or ""
    project_file = mcp_mapping.get("project_config_file") or ""
    project_path = mcp_mapping.get("project_config_path") or ""

    global_entries: dict[str, dict[str, Any]] = {}
    project_entries: dict[str, dict[str, Any]] = {}

    for name, spec in servers.items():
        scope = (spec.get("scope") or "global").lower()
        native = _translate_simple(spec, mcp_mapping)
        if scope == "project" and project_file:
            project_entries[name] = native
        else:
            global_entries[name] = native

    written = 0
    home = os.environ.get("HOME") or os.path.expanduser("~")

    if global_entries and global_file and global_path:
        target = global_file if os.path.isabs(global_file) else os.path.join(home, global_file)
        written += _merge_into_file(target, global_path, global_entries)

    if project_entries and project_file and project_path:
        target = project_file if os.path.isabs(project_file) else os.path.join(home, project_file)
        # {workspace} substitution in the path component.
        resolved_path = project_path.replace("{workspace}", agent_workspace)
        written += _merge_into_file(target, resolved_path, project_entries)

    return written


def _merge_into_file(path: str, dotted_path: str, entries: dict[str, dict[str, Any]]) -> int:
    """Read JSON at path (creating empty if missing), merge entries at dotted_path, write back atomically."""
    data: dict[str, Any] = {}
    if os.path.isfile(path):
        try:
            existing = load_json(path)
        except json.JSONDecodeError as exc:
            raise ValueError(f"existing native config at {path} is not valid JSON: {exc}") from exc
        if isinstance(existing, dict):
            data = existing
    leaf = _walk_dotted_path(data, dotted_path)
    for name, spec in entries.items():
        leaf[name] = spec
    atomic_write_json(path, data)
    return len(entries)


def warn(message: str) -> None:
    """Write a warning to stderr in a consistent format."""
    print(f"scion_harness: {message}", file=sys.stderr)
