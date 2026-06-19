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
"""Cogo container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. Responsibilities:

  1. Resolve auth from staged candidates (GOOGLE_API_KEY or GOOGLE_CLOUD_PROJECT).
  2. Copy staged instructions (and system prompt) to AGENTS.md in workspace.
  3. Pre-create .agents/ directory.
  4. Write outputs/env.json and outputs/resolved-auth.json.
  5. Apply universal MCP servers to .agents/mcp.json.

Stdlib-only — no external dependencies.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

# Add the bundle dir to sys.path so we can import the staged scion_harness helper
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

try:
    import scion_harness  # type: ignore[import-not-found]
except ImportError:
    scion_harness = None  # type: ignore[assignment]

VALID_AUTH_TYPES = ("api-key", "vertex-ai", "none")

EXIT_OK = 0
EXIT_ERROR = 1
EXIT_UNSUPPORTED = 2


def _expand(path: str) -> str:
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
    raw = candidates.get("env_vars") or []
    return {str(k) for k in raw if isinstance(k, str)}


def _env_secret_files(candidates: dict[str, Any]) -> dict[str, str]:
    raw = candidates.get("env_secret_files") or {}
    out: dict[str, str] = {}
    if not isinstance(raw, dict):
        return out
    for k, v in raw.items():
        if isinstance(k, str) and isinstance(v, str) and v:
            out[k] = v
    return out


def _read_secret(env_secret_files: dict[str, str], name: str) -> str:
    path = env_secret_files.get(name)
    if path:
        real = _expand(path)
        try:
            with open(real, "r", encoding="utf-8") as f:
                return f.read().rstrip("\r\n")
        except OSError:
            pass
    return os.environ.get(name, "")


def _select_auth_method(explicit: str, env_keys: set[str]) -> tuple[str, str]:
    if explicit:
        if explicit not in VALID_AUTH_TYPES:
            raise ValueError(
                f"cogo: unknown auth type {explicit!r}; "
                f"valid types are: {', '.join(VALID_AUTH_TYPES)}"
            )
        if explicit == "api-key":
            if "GOOGLE_API_KEY" in env_keys:
                return "api-key", "GOOGLE_API_KEY"
            raise ValueError("cogo: auth type 'api-key' selected but GOOGLE_API_KEY secret not found")
        if explicit == "vertex-ai":
            if "GOOGLE_CLOUD_PROJECT" in env_keys:
                return "vertex-ai", "GOOGLE_CLOUD_PROJECT"
            raise ValueError("cogo: auth type 'vertex-ai' selected but GOOGLE_CLOUD_PROJECT secret not found")
        if explicit == "none":
            return "none", ""

    if "GOOGLE_CLOUD_PROJECT" in env_keys:
        return "vertex-ai", "GOOGLE_CLOUD_PROJECT"
    if "GOOGLE_API_KEY" in env_keys:
        return "api-key", "GOOGLE_API_KEY"

    return "none", ""


def _provision(manifest: dict[str, Any]) -> int:
    home = os.environ.get("HOME") or os.path.expanduser("~")
    bundle = manifest.get("harness_bundle_dir") or "$HOME/.scion/harness"
    bundle = _expand(bundle)

    inputs_dir = os.path.join(bundle, "inputs")
    auth_candidates_path = os.path.join(inputs_dir, "auth-candidates.json")

    candidates: dict[str, Any] = {}
    if os.path.isfile(auth_candidates_path):
        try:
            candidates = _load_json(auth_candidates_path) or {}
        except (OSError, json.JSONDecodeError) as exc:
            print(f"cogo provision: invalid auth-candidates.json: {exc}", file=sys.stderr)
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    secret_files = _env_secret_files(candidates)

    harness_cfg = manifest.get("harness_config") or {}
    no_auth_cfg = harness_cfg.get("no_auth") or {}
    no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

    if not candidates and no_auth_behavior:
        print(f"cogo provision: no-auth mode (behavior={no_auth_behavior}), skipping auth setup", file=sys.stderr)
        method = "none"
        env_key = ""
    else:
        try:
            method, env_key = _select_auth_method(explicit, env_keys)
        except ValueError as exc:
            print(str(exc), file=sys.stderr)
            return EXIT_ERROR

    outputs = manifest.get("outputs") or {}
    env_out = _expand(outputs.get("env") or os.path.join(bundle, "outputs", "env.json"))
    auth_out = _expand(outputs.get("resolved_auth") or os.path.join(bundle, "outputs", "resolved-auth.json"))

    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "cogo",
        "method": method,
        "explicit_type": explicit or None,
    }

    env_payload: dict[str, Any] = {}

    if method == "api-key":
        api_key = _read_secret(secret_files, "GOOGLE_API_KEY")
        if not api_key:
            print("cogo provision: GOOGLE_API_KEY secret is empty", file=sys.stderr)
            return EXIT_ERROR
        env_payload["GOOGLE_API_KEY"] = api_key
        resolved_payload["env_var"] = "GOOGLE_API_KEY"

    elif method == "vertex-ai":
        project = _read_secret(secret_files, "GOOGLE_CLOUD_PROJECT")
        location = _read_secret(secret_files, "GOOGLE_CLOUD_LOCATION") or os.environ.get("GOOGLE_CLOUD_LOCATION") or "us-central1"
        if not project:
            print("cogo provision: GOOGLE_CLOUD_PROJECT secret is empty", file=sys.stderr)
            return EXIT_ERROR
        env_payload["GOOGLE_CLOUD_PROJECT"] = project
        env_payload["GOOGLE_CLOUD_LOCATION"] = location
        env_payload["GOOGLE_GENAI_USE_VERTEXAI"] = "true"
        resolved_payload["env_var"] = "GOOGLE_CLOUD_PROJECT"

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(f"cogo provision: failed to write outputs: {exc}", file=sys.stderr)
        return EXIT_ERROR

    # 1. Ensure project directories exist inside the container workspace
    agent_workspace = manifest.get("agent_workspace")
    if agent_workspace:
        agent_workspace = _expand(agent_workspace)
        agents_dir = os.path.join(agent_workspace, ".agents")
        skills_dir = os.path.join(agents_dir, "skills")
        os.makedirs(skills_dir, exist_ok=True)

        # 2. Reconcile memory files: AGENTS.md
        instructions_src = os.path.join(inputs_dir, "instructions.md")
        sys_prompt_src = os.path.join(inputs_dir, "system-prompt.md")

        agents_md_content = ""
        if os.path.isfile(instructions_src):
            try:
                with open(instructions_src, "r", encoding="utf-8") as f:
                    agents_md_content = f.read()
            except OSError as exc:
                print(f"cogo provision: warning: failed to read staged instructions: {exc}", file=sys.stderr)

        if os.path.isfile(sys_prompt_src) and harness_cfg.get("system_prompt_mode") == "prepend_to_instructions":
            try:
                with open(sys_prompt_src, "r", encoding="utf-8") as f:
                    sys_prompt = f.read().strip()
                if sys_prompt:
                    header = f"# System Prompt\n\n{sys_prompt}\n\n---\n\n"
                    agents_md_content = header + agents_md_content
            except OSError as exc:
                print(f"cogo provision: warning: failed to read staged system prompt: {exc}", file=sys.stderr)

        if agents_md_content:
            target_agents_md = os.path.join(agent_workspace, "AGENTS.md")
            try:
                with open(target_agents_md, "w", encoding="utf-8") as f:
                    f.write(agents_md_content)
                # Keep permissions intact
                os.chmod(target_agents_md, 0o644)
            except OSError as exc:
                print(f"cogo provision: warning: failed to write AGENTS.md to workspace: {exc}", file=sys.stderr)

        # 3. Apply universal MCP servers to .agents/mcp.json
        if scion_harness is not None:
            mcp_mapping = harness_cfg.get("mcp") or {}
            if mcp_mapping:
                # Pre-seed version in mcp.json if not present
                mcp_json_path = os.path.join(agents_dir, "mcp.json")
                if not os.path.isfile(mcp_json_path):
                    try:
                        _write_json(mcp_json_path, {"version": 1, "servers": {}})
                    except OSError as exc:
                        print(f"cogo provision: warning: failed to pre-seed mcp.json: {exc}", file=sys.stderr)
                
                try:
                    scion_harness.apply_mcp_servers_simple(bundle, mcp_mapping, agent_workspace)
                except Exception as exc:
                    print(f"cogo provision: warning: failed to apply MCP servers: {exc}", file=sys.stderr)

    print(f"cogo provision: method={method}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(f"cogo provision: unsupported command {command!r}", file=sys.stderr)
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(description="Cogo container-side provisioner")
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
        print(f"cogo provision: manifest not found at {manifest_path}", file=sys.stderr)
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(f"cogo provision: failed to load manifest {manifest_path}: {exc}", file=sys.stderr)
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("cogo provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
