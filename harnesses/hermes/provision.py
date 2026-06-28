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
"""Hermes container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.

This script's job:

  1. Determine which API key is available, with precedence:
         ANTHROPIC_API_KEY > OPENAI_API_KEY > GOOGLE_API_KEY.
  2. Read the secret value from the staged secrets/<NAME> file and write it
     to ~/.hermes/.env (Hermes reads secrets from this dotenv file).
  3. Compose staged Scion prompt inputs into AGENTS.md (instruction
     projection — Hermes auto-reads AGENTS.md as context).
  4. Apply MCP server configuration to ~/.hermes/mcp.json.
  5. Write outputs/resolved-auth.json and outputs/env.json (env overlay
     with HERMES_YOLO_MODE, HERMES_QUIET, HERMES_ACCEPT_HOOKS, and
     optionally HERMES_INFERENCE_MODEL).

The script is stdlib-only — no third-party dependencies.
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

SCION_MANAGED_BEGIN = "<!-- BEGIN SCION MANAGED HERMES INSTRUCTIONS -->"
SCION_MANAGED_END = "<!-- END SCION MANAGED HERMES INSTRUCTIONS -->"

AUTH_KEY_PRECEDENCE = ["ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOGLE_API_KEY"]

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
    if not path:
        return ""
    real = _expand(path)
    try:
        with open(real, "r", encoding="utf-8") as f:
            return f.read().rstrip("\r\n")
    except OSError:
        return ""


def _select_auth_key(
    explicit: str,
    env_keys: set[str],
) -> tuple[str, str]:
    """Pick an API key env var.

    Returns (method, env_key). Raises ValueError on no-creds.
    """
    if explicit and explicit != "api-key":
        raise ValueError(
            f"hermes: unknown auth type {explicit!r}; only 'api-key' is supported"
        )

    for key in AUTH_KEY_PRECEDENCE:
        if key in env_keys:
            return "api-key", key

    raise ValueError(
        "hermes: no valid API key found; set ANTHROPIC_API_KEY, OPENAI_API_KEY, "
        "or GOOGLE_API_KEY"
    )


def _write_hermes_env(env_vars: dict[str, str]) -> None:
    """Write key=value pairs to ~/.hermes/.env."""
    hermes_dir = _expand("~/.hermes")
    os.makedirs(hermes_dir, exist_ok=True)
    target = os.path.join(hermes_dir, ".env")
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        for k, v in sorted(env_vars.items()):
            f.write(f"{k}={v}\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


# --- Instruction projection ------------------------------------------------


def _read_text_if_exists(path: str) -> str:
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read()
    except OSError:
        return ""


def _strip_scion_managed_block(content: str) -> str:
    start = content.find(SCION_MANAGED_BEGIN)
    if start == -1:
        return content
    end = content.find(SCION_MANAGED_END, start)
    if end == -1:
        print(
            f"hermes provision: warning: found {SCION_MANAGED_BEGIN} but no matching "
            f"{SCION_MANAGED_END}. Aborting strip to prevent data loss.",
            file=sys.stderr,
        )
        return content
    end += len(SCION_MANAGED_END)
    return (content[:start] + content[end:]).strip() + "\n"


def _markdown_section(title: str, content: str) -> str:
    body = content.strip()
    if not body:
        return ""
    return f"# {title}\n\n{body}\n"


def _skill_sections(home: str, skills_dir: str) -> list[str]:
    if not skills_dir:
        return []
    root = os.path.join(home, skills_dir)
    if not os.path.isdir(root):
        return []

    sections: list[str] = []
    try:
        entries = sorted(os.listdir(root))
    except OSError as exc:
        print(f"hermes provision: could not list skills dir {root}: {exc}", file=sys.stderr)
        return []

    for entry in entries:
        if entry.startswith("."):
            continue
        skill_md = os.path.join(root, entry, "SKILL.md")
        if not os.path.isfile(skill_md):
            continue
        content = _read_text_if_exists(skill_md).strip()
        if not content:
            continue
        sections.append(f"## {entry}\n\n{content}\n")
    return sections


def _apply_instruction_projection(bundle: str, manifest: dict[str, Any]) -> None:
    """Compose staged Scion prompt inputs into AGENTS.md.

    Hermes auto-reads AGENTS.md as context. The system prompt is downgraded
    into AGENTS.md when config.yaml requests prepend_to_instructions.
    """
    harness_cfg = manifest.get("harness_config") or {}
    home = os.environ.get("HOME") or _expand("~")
    instructions_file = str(harness_cfg.get("instructions_file") or "AGENTS.md")
    system_prompt_mode = str(harness_cfg.get("system_prompt_mode") or "none")
    skills_dir = str(harness_cfg.get("skills_dir") or ".hermes/skills")

    inputs_dir = os.path.join(bundle, "inputs")
    instructions = _read_text_if_exists(os.path.join(inputs_dir, "instructions.md"))
    system_prompt = _read_text_if_exists(os.path.join(inputs_dir, "system-prompt.md"))
    skills = _skill_sections(home, skills_dir)

    target = os.path.join(home, instructions_file)
    existing = _strip_scion_managed_block(_read_text_if_exists(target))

    sections: list[str] = []
    if system_prompt.strip() and system_prompt_mode != "none":
        sections.append(_markdown_section("System Instruction", system_prompt))

    if instructions.strip():
        sections.append(_markdown_section("Agent Instructions", instructions))

    if skills:
        sections.append("# Skills\n\n" + "\n\n".join(skill.strip() for skill in skills) + "\n")

    if not sections and not existing.strip():
        if os.path.isfile(target):
            os.remove(target)
        return

    managed = ""
    if sections:
        managed = (
            f"{SCION_MANAGED_BEGIN}\n\n"
            + "\n\n".join(section.strip() for section in sections if section.strip())
            + f"\n\n{SCION_MANAGED_END}\n"
        )

    unmanaged = ""
    if existing.strip():
        unmanaged = existing.strip() + "\n"
        if managed:
            unmanaged = "\n" + unmanaged
    content = managed + unmanaged

    os.makedirs(os.path.dirname(target) if os.path.dirname(target) else ".", exist_ok=True)
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(content)
    os.replace(tmp, target)
    print(f"hermes provision: wrote instructions to {target}", file=sys.stderr)


# --- MCP server reconciliation ---------------------------------------------
#
# Hermes reads MCP config from ~/.hermes/mcp.json. The format is a JSON
# object with an "mcpServers" key containing named server definitions:
#   {
#     "mcpServers": {
#       "name": { "command": "...", "args": [...], "env": {...} },
#       "name": { "url": "..." }
#     }
#   }


def _build_mcp_entry(name: str, spec: dict[str, Any]) -> dict[str, Any] | None:
    """Translate a universal MCPServerConfig into a Hermes mcp.json entry."""
    transport = (spec.get("transport") or "").strip()

    if transport == "stdio":
        cmd = spec.get("command")
        if not isinstance(cmd, str) or not cmd:
            print(f"hermes provision: mcp server {name!r}: stdio transport missing command", file=sys.stderr)
            return None
        entry: dict[str, Any] = {"command": cmd}
        args = spec.get("args") or []
        if isinstance(args, list) and args:
            entry["args"] = [str(a) for a in args]
        env = spec.get("env")
        if isinstance(env, dict) and env:
            entry["env"] = {str(k): str(v) for k, v in env.items()}
        return entry
    elif transport in ("sse", "streamable-http"):
        url = spec.get("url")
        if not isinstance(url, str) or not url:
            print(f"hermes provision: mcp server {name!r}: {transport} transport missing url", file=sys.stderr)
            return None
        entry = {"url": url}
        headers = spec.get("headers")
        if isinstance(headers, dict) and headers:
            entry["headers"] = {str(k): str(v) for k, v in headers.items()}
        return entry
    else:
        print(f"hermes provision: mcp server {name!r}: unsupported transport {transport!r}", file=sys.stderr)
        return None


def _apply_mcp_servers(bundle: str) -> int:
    """Write MCP server config to ~/.hermes/mcp.json.

    Returns the number of servers written.
    """
    if scion_harness is None:
        servers = _read_mcp_servers_inline(bundle)
    else:
        try:
            servers = scion_harness.read_mcp_servers(bundle)
        except ValueError as exc:
            print(f"hermes provision: {exc}", file=sys.stderr)
            return 0

    hermes_dir = _expand("~/.hermes")
    config_path = os.path.join(hermes_dir, "mcp.json")

    if not servers:
        if os.path.isfile(config_path):
            try:
                os.remove(config_path)
                print("hermes provision: removed stale mcp.json (no servers configured)", file=sys.stderr)
            except OSError as exc:
                print(f"hermes provision: could not remove stale mcp.json: {exc}", file=sys.stderr)
        return 0

    mcp_servers: dict[str, Any] = {}
    for name in sorted(servers.keys()):
        spec = servers[name]
        if not isinstance(spec, dict):
            continue
        scope = (spec.get("scope") or "global").strip().lower()
        if scope == "project":
            print(
                f"hermes provision: mcp server {name!r} requested project scope; "
                "registering globally (project-scoped MCP not implemented)",
                file=sys.stderr,
            )
        entry = _build_mcp_entry(name, spec)
        if entry is not None:
            mcp_servers[name] = entry

    if not mcp_servers:
        return 0

    try:
        os.makedirs(hermes_dir, exist_ok=True)
    except OSError as exc:
        print(f"hermes provision: could not create {hermes_dir}: {exc}", file=sys.stderr)
        return 0
    payload = {"mcpServers": mcp_servers}
    try:
        _write_json(config_path, payload)
        os.chmod(config_path, 0o600)
    except OSError as exc:
        print(f"hermes provision: failed to write mcp.json: {exc}", file=sys.stderr)
        return 0

    print(f"hermes provision: applied {len(mcp_servers)} mcp server(s)", file=sys.stderr)
    return len(mcp_servers)


def _read_mcp_servers_inline(bundle: str) -> dict[str, dict[str, Any]]:
    """Fallback when scion_harness import fails."""
    path = os.path.join(bundle, "inputs", "mcp-servers.json")
    if not os.path.isfile(path):
        return {}
    try:
        payload = _load_json(path) or {}
    except (OSError, json.JSONDecodeError) as exc:
        print(f"hermes provision: invalid mcp-servers.json: {exc}", file=sys.stderr)
        return {}
    if not isinstance(payload, dict):
        return {}
    servers = payload.get("mcp_servers") or {}
    if not isinstance(servers, dict):
        return {}
    return {str(k): v for k, v in servers.items() if isinstance(v, dict)}


# --- Entry point -----------------------------------------------------------


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
            print(f"hermes provision: invalid auth-candidates.json: {exc}", file=sys.stderr)
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    secret_files = _env_secret_files(candidates)

    harness_cfg = manifest.get("harness_config") or {}
    no_auth_cfg = harness_cfg.get("no_auth") or {}
    no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

    if not env_keys and no_auth_behavior:
        print(f"hermes provision: no-auth mode (behavior={no_auth_behavior}), skipping auth setup", file=sys.stderr)
        method = "none"
        env_key = ""
    else:
        try:
            method, env_key = _select_auth_key(explicit, env_keys)
        except ValueError as exc:
            print(str(exc), file=sys.stderr)
            return EXIT_ERROR

    # Write the API key to ~/.hermes/.env so Hermes can read it.
    hermes_env_vars: dict[str, str] = {}
    if method == "api-key":
        api_key = _read_secret(secret_files, env_key)
        if not api_key:
            print(
                f"hermes provision: chose api-key ({env_key}) but no secret value "
                f"was staged at the recorded path; check ApplyAuthSettings",
                file=sys.stderr,
            )
            return EXIT_ERROR
        hermes_env_vars[env_key] = api_key

    if hermes_env_vars:
        try:
            _write_hermes_env(hermes_env_vars)
        except OSError as exc:
            print(f"hermes provision: write .env failed: {exc}", file=sys.stderr)
            return EXIT_ERROR

    try:
        _apply_instruction_projection(bundle, manifest)
    except OSError as exc:
        print(f"hermes provision: instruction projection failed: {exc}", file=sys.stderr)
        return EXIT_ERROR

    # Build env overlay — these env vars are injected into the container
    # environment by sciontool before starting hermes.
    env_payload: dict[str, str] = {
        "HERMES_HOME": "/home/scion/.hermes",
        "HERMES_YOLO_MODE": "1",
        "HERMES_QUIET": "1",
        "HERMES_ACCEPT_HOOKS": "auto",
    }

    # Resolve model alias if provided.
    model_resolution = manifest.get("model_resolution") or {}
    resolved_model = str(model_resolution.get("resolved_model") or "").strip()
    if resolved_model:
        env_payload["HERMES_INFERENCE_MODEL"] = resolved_model

    # Outputs.
    outputs = manifest.get("outputs") or {}
    env_out = _expand(outputs.get("env") or os.path.join(bundle, "outputs", "env.json"))
    auth_out = _expand(outputs.get("resolved_auth") or os.path.join(bundle, "outputs", "resolved-auth.json"))

    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "hermes",
        "method": method,
        "explicit_type": explicit or None,
    }
    if method == "api-key":
        resolved_payload["env_var"] = env_key

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(f"hermes provision: failed to write outputs: {exc}", file=sys.stderr)
        return EXIT_ERROR

    _apply_mcp_servers(bundle)

    print(f"hermes provision: method={method}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(f"hermes provision: unsupported command {command!r}", file=sys.stderr)
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(description="Hermes container-side provisioner")
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
        print(f"hermes provision: manifest not found at {manifest_path}", file=sys.stderr)
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(f"hermes provision: failed to load manifest {manifest_path}: {exc}", file=sys.stderr)
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("hermes provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
