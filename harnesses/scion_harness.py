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
"""Shared library for scion harness provision.py and capture_auth.py scripts.

Staged into agent_home/.scion/harness/scion_harness.py during
ContainerScriptHarness.Provision(). Each harness's provision.py adds the
bundle dir to sys.path so it can `import scion_harness`.

Stdlib-only so it works in any container image that ships python3.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
from typing import Any

# ---------------------------------------------------------------------------
# Version contract (§3.3)
# ---------------------------------------------------------------------------

INTERFACE_VERSION = 2
LIB_VERSION = "2026-07-05"

# ---------------------------------------------------------------------------
# Exit codes
# ---------------------------------------------------------------------------

EXIT_OK = 0
EXIT_ERROR = 1
EXIT_UNSUPPORTED = 2

# ---------------------------------------------------------------------------
# Exceptions
# ---------------------------------------------------------------------------


class ProvisionError(Exception):
    """Raised by library code when provisioning must abort with EXIT_ERROR."""
    pass


# ---------------------------------------------------------------------------
# Low-level helpers (original API — signatures preserved exactly)
# ---------------------------------------------------------------------------


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
        try:
            written += _merge_into_file(target, global_path, global_entries)
        except OSError as exc:
            warn(f"failed to write MCP global config {target}: {exc}")

    if project_entries and project_file and project_path:
        target = project_file if os.path.isabs(project_file) else os.path.join(home, project_file)
        # {workspace} substitution in the path component.
        resolved_path = project_path.replace("{workspace}", agent_workspace)
        try:
            written += _merge_into_file(target, resolved_path, project_entries)
        except OSError as exc:
            warn(f"failed to write MCP project config {target}: {exc}")

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


# ===========================================================================
# New API — §3.1 layers
# ===========================================================================

# ---------------------------------------------------------------------------
# Auth specification (declarative auth engine)
# ---------------------------------------------------------------------------


class AuthMethod:
    """A single auth method declaration."""

    def __init__(
        self,
        name: str,
        kind: str,
        *,
        any_of: list[str] | None = None,
        all_of: list[str] | None = None,
        path: str | None = None,
        hint: str = "",
        env_fallback: bool = False,
        secret_key: str = "",
    ):
        self.name = name
        self.kind = kind  # "env" or "file"
        self.any_of = any_of or []
        self.all_of = all_of or []
        self.path = path
        self.hint = hint
        self.env_fallback = env_fallback
        self.secret_key = secret_key


def env_method(
    name: str,
    *,
    any_of: list[str] | None = None,
    all_of: list[str] | None = None,
    hint: str = "",
    env_fallback: bool = False,
) -> AuthMethod:
    """Declare an env-based auth method."""
    return AuthMethod(name, "env", any_of=any_of, all_of=all_of, hint=hint, env_fallback=env_fallback)


def file_method(
    name: str,
    *,
    path: str,
    hint: str = "",
    secret_key: str = "",
) -> AuthMethod:
    """Declare a file-based auth method."""
    return AuthMethod(name, "file", path=path, hint=hint, secret_key=secret_key)


class AuthSpec:
    """Declarative auth specification for a harness."""

    def __init__(
        self,
        harness: str,
        methods: list[AuthMethod],
        *,
        fallback_to_none_on_error: bool = False,
    ):
        self.harness = harness
        self.methods = methods
        self.fallback_to_none_on_error = fallback_to_none_on_error

    def valid_types(self) -> tuple[str, ...]:
        return tuple(m.name for m in self.methods)


class ResolvedAuth:
    """Result of auth selection."""

    def __init__(
        self,
        method: str,
        env_key: str = "",
        spec_entry: AuthMethod | None = None,
        *,
        auth_file: str = "",
    ):
        self.method = method
        self.env_key = env_key
        self.spec_entry = spec_entry
        self.auth_file = auth_file


# ---------------------------------------------------------------------------
# ProvisionContext
# ---------------------------------------------------------------------------


class ProvisionContext:
    """Runtime context for a provision function."""

    def __init__(self, harness_name: str, manifest: dict[str, Any]):
        self.harness_name = harness_name
        self.manifest = manifest
        self._candidates: dict[str, Any] | None = None
        self._telemetry: dict[str, Any] | None = None
        self._harness_config: dict[str, Any] | None = None

    @property
    def bundle_dir(self) -> str:
        raw = self.manifest.get("harness_bundle_dir") or "$HOME/.scion/harness"
        return expand_path(raw)

    @property
    def inputs_dir(self) -> str:
        return os.path.join(self.bundle_dir, "inputs")

    @property
    def workspace(self) -> str:
        return str(self.manifest.get("agent_workspace") or "/workspace")

    @property
    def home(self) -> str:
        return os.environ.get("HOME") or os.path.expanduser("~")

    @property
    def harness_config(self) -> dict[str, Any]:
        if self._harness_config is None:
            self._harness_config = self.manifest.get("harness_config") or {}
        return self._harness_config

    @property
    def candidates(self) -> dict[str, Any]:
        if self._candidates is None:
            path = os.path.join(self.inputs_dir, "auth-candidates.json")
            if os.path.isfile(path):
                try:
                    self._candidates = load_json(path) or {}
                except (OSError, json.JSONDecodeError) as exc:
                    raise ProvisionError(f"invalid auth-candidates.json: {exc}") from exc
            else:
                self._candidates = {}
        return self._candidates

    @property
    def explicit_type(self) -> str:
        return str(self.candidates.get("explicit_type") or "").strip()

    @property
    def env_keys(self) -> set[str]:
        raw = self.candidates.get("env_vars") or []
        return {str(k) for k in raw if isinstance(k, str)}

    @property
    def file_paths(self) -> list[str]:
        raw = self.candidates.get("files") or []
        out: list[str] = []
        for entry in raw:
            if isinstance(entry, dict):
                cp = entry.get("container_path")
                if isinstance(cp, str) and cp:
                    out.append(cp)
        return out

    @property
    def env_secret_files(self) -> dict[str, str]:
        raw = self.candidates.get("env_secret_files") or {}
        if not isinstance(raw, dict):
            return {}
        return {str(k): str(v) for k, v in raw.items() if isinstance(k, str) and isinstance(v, str) and v}

    @property
    def file_secret_files(self) -> dict[str, str]:
        raw = self.candidates.get("file_secret_files") or {}
        if not isinstance(raw, dict):
            return {}
        return {str(k): str(v) for k, v in raw.items() if isinstance(k, str) and isinstance(v, str) and v}

    @property
    def telemetry(self) -> dict[str, Any]:
        if self._telemetry is None:
            path = os.path.join(self.inputs_dir, "telemetry.json")
            if os.path.isfile(path):
                try:
                    self._telemetry = load_json(path) or {}
                except json.JSONDecodeError as exc:
                    raise ProvisionError(f"malformed telemetry.json: {exc}") from exc
                except OSError:
                    self._telemetry = {}
            else:
                self._telemetry = {}
        return self._telemetry

    @property
    def model_resolution(self) -> dict[str, Any]:
        return self.manifest.get("model_resolution") or {}

    def read_secret(self, name: str, *, env_fallback: bool = False) -> str:
        """Read a staged secret file. Policy: rstrip("\\r\\n") only (§4.1)."""
        secret_files = self.env_secret_files
        path = secret_files.get(name)
        if path:
            try:
                with open(expand_path(path), "r", encoding="utf-8") as f:
                    return f.read().rstrip("\r\n")
            except OSError:
                pass
        if env_fallback:
            return os.environ.get(name, "")
        return ""

    def read_file_secret(self, name: str) -> str:
        """Read a staged file-type secret."""
        secret_files = self.file_secret_files
        path = secret_files.get(name)
        if path:
            try:
                with open(expand_path(path), "r", encoding="utf-8") as f:
                    return f.read().rstrip("\r\n")
            except OSError:
                pass
        return ""

    def read_input_text(self, name: str) -> str:
        """Read a text file from inputs/."""
        path = os.path.join(self.inputs_dir, name)
        try:
            with open(path, "r", encoding="utf-8") as f:
                return f.read()
        except OSError:
            return ""

    def output_paths(self) -> tuple[str, str]:
        """Return (resolved_auth_path, env_json_path)."""
        outputs_dir = os.path.join(self.bundle_dir, "outputs")
        return (
            os.path.join(outputs_dir, "resolved-auth.json"),
            os.path.join(outputs_dir, "env.json"),
        )

    def info(self, message: str) -> None:
        print(f"{self.harness_name} provision: {message}", file=sys.stderr)

    def warn(self, message: str) -> None:
        print(f"{self.harness_name} provision: warning: {message}", file=sys.stderr)

    # --- Auth selection ---

    def select_auth(self, spec: AuthSpec) -> ResolvedAuth:
        """Select auth method per spec. Returns ResolvedAuth or raises ProvisionError."""
        explicit = self.explicit_type
        env_keys = self.env_keys
        file_paths = self.file_paths
        secret_files = self.env_secret_files
        file_secrets = self.file_secret_files

        no_auth_cfg = self.harness_config.get("no_auth") or {}
        no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

        if explicit:
            valid = spec.valid_types()
            if explicit not in valid:
                raise ProvisionError(
                    f"{spec.harness}: unknown auth type {explicit!r}; "
                    f"valid types are: {', '.join(valid)}"
                )

        try:
            return self._try_select(spec, explicit, env_keys, file_paths,
                                    secret_files, file_secrets)
        except ProvisionError:
            if not explicit and no_auth_behavior and spec.fallback_to_none_on_error:
                self.info(f"auth selection failed; falling back to no-auth (behavior={no_auth_behavior})")
                return ResolvedAuth(method="none")
            if not explicit and not self.candidates and no_auth_behavior:
                self.info(f"no auth candidates staged; running in no-auth mode (behavior={no_auth_behavior})")
                return ResolvedAuth(method="none")
            raise

    def _try_select(
        self,
        spec: AuthSpec,
        explicit: str,
        env_keys: set[str],
        file_paths: list[str],
        secret_files: dict[str, str],
        file_secrets: dict[str, str],
    ) -> ResolvedAuth:
        no_auth_cfg = self.harness_config.get("no_auth") or {}
        no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

        if not explicit and not self.candidates and no_auth_behavior:
            self.info(f"no auth candidates staged; running in no-auth mode (behavior={no_auth_behavior})")
            return ResolvedAuth(method="none")

        for method in spec.methods:
            if explicit and method.name != explicit:
                continue

            if method.kind == "env":
                matched_key = self._match_env_method(method, env_keys, secret_files)
                if matched_key is not None:
                    return ResolvedAuth(method=method.name, env_key=matched_key,
                                       spec_entry=method)
                if explicit:
                    hints = method.hint or f"set {' or '.join(method.any_of or method.all_of)}"
                    raise ProvisionError(
                        f"{spec.harness}: auth type {explicit!r} selected but "
                        f"no credentials found; {hints}"
                    )

            elif method.kind == "file":
                if self._match_file_method(method, file_paths, file_secrets):
                    return ResolvedAuth(method=method.name,
                                       auth_file=method.path or "",
                                       spec_entry=method)
                if explicit:
                    raise ProvisionError(
                        f"{spec.harness}: auth type {explicit!r} selected but "
                        f"no auth file found; expected {method.path}"
                    )

        hints_parts: list[str] = []
        for m in spec.methods:
            if m.hint:
                hints_parts.append(m.hint)
            elif m.kind == "env" and m.any_of:
                hints_parts.append(f"set {' or '.join(m.any_of)}")
            elif m.kind == "file" and m.path:
                hints_parts.append(f"provide auth at {m.path}")
        raise ProvisionError(
            f"{spec.harness}: no valid auth method found; "
            + ", or ".join(hints_parts) if hints_parts
            else f"{spec.harness}: no valid auth method found"
        )

    def _match_env_method(
        self,
        method: AuthMethod,
        env_keys: set[str],
        secret_files: dict[str, str],
    ) -> str | None:
        """Check if an env method's requirements are met. Returns the matched key or None.

        Two-pass matching: candidates/secret-files are checked across all keys
        first; environ fallback only fires if nothing matched in the first pass.
        This prevents a stale container env var for a lower-priority key from
        shadowing a user-staged secret for a higher-priority key.
        """
        if method.all_of:
            # Pass 1: check candidates/secret-files only
            missing = [k for k in method.all_of
                       if k not in env_keys and k not in secret_files]
            if missing and method.env_fallback:
                # Pass 2: allow env fallback for keys not in candidates
                missing = [k for k in missing if not os.environ.get(k)]
            if missing:
                return None

        if method.any_of:
            # Pass 1: check candidates/secret-files across ALL keys first
            for key in method.any_of:
                if key in env_keys or key in secret_files:
                    return key
            # Pass 2: only if no key matched above, try env fallback
            if method.env_fallback:
                for key in method.any_of:
                    if os.environ.get(key):
                        return key
            return None

        if method.all_of and not method.any_of:
            return method.all_of[0] if method.all_of else ""

        return None

    def _match_file_method(
        self,
        method: AuthMethod,
        file_paths: list[str],
        file_secrets: dict[str, str],
    ) -> bool:
        if not method.path:
            return False
        target = expand_path(method.path)
        if any(expand_path(p) == target for p in file_paths):
            return True
        if os.path.isfile(target):
            return True
        if method.secret_key and method.secret_key in file_secrets:
            return True
        return False

    # --- Standard outputs ---

    def write_outputs(
        self,
        resolved: ResolvedAuth,
        *,
        env: dict[str, str] | None = None,
        extra: dict[str, Any] | None = None,
    ) -> None:
        """Write resolved-auth.json (v2) and env.json."""
        auth_path, env_path = self.output_paths()

        auth_payload: dict[str, Any] = {
            "schema_version": 2,
            "harness": self.harness_name,
            "method": resolved.method,
            "explicit_type": self.explicit_type or None,
        }
        # env_var only for api-key-style methods (single-key auth), not for
        # multi-key methods like vertex-ai where env_key is a location, not a credential.
        if resolved.env_key:
            is_multi_key = resolved.spec_entry and resolved.spec_entry.all_of
            if not is_multi_key:
                auth_payload["env_var"] = resolved.env_key
        if resolved.auth_file:
            auth_payload["auth_file"] = resolved.auth_file
        if extra:
            auth_payload.update(extra)

        atomic_write_json(auth_path, auth_payload)

        env_payload = dict(env) if env else {}
        atomic_write_json(env_path, env_payload)


# ---------------------------------------------------------------------------
# Instruction projection (§3.1 layer 5)
# ---------------------------------------------------------------------------

_MANAGED_BEGIN_STANDARD = "<!-- BEGIN SCION MANAGED -->"
_MANAGED_END_STANDARD = "<!-- END SCION MANAGED -->"

_LEGACY_BEGIN_MARKERS = [
    "<!-- BEGIN SCION MANAGED CODEX INSTRUCTIONS -->",
    "<!-- BEGIN SCION MANAGED HERMES INSTRUCTIONS -->",
    "<!-- SCION_MANAGED_BEGIN -->",
    "<!-- BEGIN SCION MANAGED -->",
]

_LEGACY_END_MARKERS = [
    "<!-- END SCION MANAGED CODEX INSTRUCTIONS -->",
    "<!-- END SCION MANAGED HERMES INSTRUCTIONS -->",
    "<!-- SCION_MANAGED_END -->",
    "<!-- END SCION MANAGED -->",
]


def _read_text_if_exists(path: str) -> str:
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read()
    except OSError:
        return ""


def _strip_managed_block(content: str, harness_name: str = "") -> str:
    """Strip any scion managed block from content, accepting legacy marker variants."""
    start_idx = -1
    begin_marker = ""
    for marker in _LEGACY_BEGIN_MARKERS:
        idx = content.find(marker)
        if idx != -1:
            start_idx = idx
            begin_marker = marker
            break

    if start_idx == -1:
        return content

    end_idx = -1
    end_marker = ""
    for marker in _LEGACY_END_MARKERS:
        idx = content.find(marker, start_idx + len(begin_marker))
        if idx != -1:
            end_idx = idx
            end_marker = marker
            break

    if end_idx == -1:
        prefix = f"{harness_name} provision: " if harness_name else "scion_harness: "
        print(
            f"{prefix}warning: found {begin_marker} but no matching end marker. "
            "Aborting strip to prevent data loss.",
            file=sys.stderr,
        )
        return content

    end_idx += len(end_marker)
    return (content[:start_idx] + content[end_idx:]).strip() + "\n"


def _markdown_section(title: str, content: str) -> str:
    body = content.strip()
    if not body:
        return ""
    return f"# {title}\n\n{body}\n"


def _skill_sections(home: str, skills_dir: str, harness_name: str = "") -> list[str]:
    """Read installed SKILL.md files from the skills directory."""
    if not skills_dir:
        return []
    root = os.path.join(home, skills_dir)
    if not os.path.isdir(root):
        return []

    sections: list[str] = []
    try:
        entries = sorted(os.listdir(root))
    except OSError as exc:
        prefix = f"{harness_name} provision" if harness_name else "scion_harness"
        print(f"{prefix}: could not list skills dir {root}: {exc}", file=sys.stderr)
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


def project_instructions(
    ctx: ProvisionContext,
    target: str,
    *,
    marker_label: str | None = None,
    include_skills: bool = True,
    system_prompt_mode: str | None = None,
    skills_dir: str | None = None,
) -> None:
    """Compose staged Scion prompt inputs into a target instruction file.

    Uses managed-block markers to separate Scion-managed content from
    user-authored content in the target file.
    """
    harness_cfg = ctx.harness_config
    if system_prompt_mode is None:
        system_prompt_mode = str(harness_cfg.get("system_prompt_mode") or "none")
    if skills_dir is None:
        skills_dir = str(harness_cfg.get("skills_dir") or "")

    instructions = ctx.read_input_text("instructions.md")
    system_prompt = ctx.read_input_text("system-prompt.md")
    skills = _skill_sections(ctx.home, skills_dir, ctx.harness_name) if include_skills else []

    target_path = os.path.join(ctx.home, target) if not os.path.isabs(target) else target
    existing = _strip_managed_block(_read_text_if_exists(target_path), ctx.harness_name)

    begin = _MANAGED_BEGIN_STANDARD
    end = _MANAGED_END_STANDARD

    sections: list[str] = []
    if system_prompt.strip() and system_prompt_mode != "none":
        sections.append(_markdown_section("System Instruction", system_prompt))

    if instructions.strip():
        sections.append(_markdown_section("Agent Instructions", instructions))

    if skills:
        sections.append("# Skills\n\n" + "\n\n".join(skill.strip() for skill in skills) + "\n")

    if not sections and not existing.strip():
        if os.path.isfile(target_path):
            os.remove(target_path)
        return

    managed = ""
    if sections:
        managed = (
            f"{begin}\n\n"
            + "\n\n".join(section.strip() for section in sections if section.strip())
            + f"\n\n{end}\n"
        )

    unmanaged = ""
    if existing.strip():
        unmanaged = existing.strip() + "\n"
        if managed:
            unmanaged = "\n" + unmanaged
    content = managed + unmanaged

    parent = os.path.dirname(target_path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    tmp = target_path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(content)
    os.replace(tmp, target_path)
    ctx.info(f"wrote instructions to {target_path}")


# ---------------------------------------------------------------------------
# MCP translation helper (§3.1 layer 6)
# ---------------------------------------------------------------------------


def apply_mcp_translated(
    ctx: ProvisionContext,
    translate_fn: Any,
    write_fn: Any,
) -> int:
    """Shared warn-and-skip loop for harnesses with custom MCP translation.

    translate_fn(name, spec) -> native_entry | None (None = skip)
    write_fn(servers_dict) -> None (writes the native config file)

    Returns the number of servers successfully translated.
    """
    try:
        servers = read_mcp_servers(ctx.bundle_dir)
    except ValueError as exc:
        ctx.info(str(exc))
        return 0

    if not servers:
        return 0

    translated: dict[str, Any] = {}
    for name in sorted(servers.keys()):
        spec = servers[name]
        if not isinstance(spec, dict):
            continue
        scope = (spec.get("scope") or "global").strip().lower()
        if scope == "project":
            ctx.info(
                f"mcp server {name!r} requested project scope; "
                "registering globally (project-scoped MCP not implemented)"
            )
        entry = translate_fn(name, spec)
        if entry is not None:
            translated[name] = entry

    if not translated:
        return 0

    try:
        write_fn(translated)
    except OSError as exc:
        ctx.warn(f"failed to write MCP config: {exc}")
        return 0
    ctx.info(f"applied {len(translated)} mcp server(s)")
    return len(translated)


# ---------------------------------------------------------------------------
# TOML helpers (§3.1 layer 7)
# ---------------------------------------------------------------------------


def toml_escape(value: str) -> str:
    """Escape a string for a TOML basic string literal."""
    out = value.replace("\\", "\\\\").replace("\"", "\\\"")
    return out.replace("\n", "\\n").replace("\r", "\\r").replace("\t", "\\t")


def toml_inline_table(items: dict[str, str]) -> str:
    """Render dict as a TOML inline table with sorted, quoted keys/values."""
    parts = [f'"{toml_escape(str(k))}" = "{toml_escape(str(v))}"' for k, v in sorted(items.items())]
    return "{ " + ", ".join(parts) + " }"


def toml_string_array(items: list[str]) -> str:
    """Render a list as a TOML string array."""
    parts = [f'"{toml_escape(str(item))}"' for item in items]
    return "[" + ", ".join(parts) + "]"


def strip_toml_sections(content: str, header_predicate: Any) -> str:
    """Remove TOML sections whose header line matches the predicate.

    header_predicate(stripped_line) -> bool

    Also consumes blank lines immediately preceding the header.
    """
    lines = content.split("\n")
    keep = [True] * len(lines)
    i = 0
    while i < len(lines):
        stripped = lines[i].strip()
        if stripped.startswith("[") and stripped.endswith("]") and header_predicate(stripped):
            section_start = i
            section_end = len(lines)
            for j in range(i + 1, len(lines)):
                t = lines[j].strip()
                if t.startswith("[") and t.endswith("]"):
                    section_end = j
                    break
            trim_start = section_start
            while trim_start > 0 and lines[trim_start - 1].strip() == "" and keep[trim_start - 1]:
                trim_start -= 1
            for k in range(trim_start, section_end):
                keep[k] = False
            i = section_end
        else:
            i += 1
    return "\n".join(line for line, k in zip(lines, keep) if k)


def atomic_write_text(path: str, content: str, *, mode: int | None = None) -> None:
    """Write text atomically via tmp + os.replace. Optionally chmod."""
    parent = os.path.dirname(path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(content)
    if mode is not None:
        os.chmod(tmp, mode)
    os.replace(tmp, path)


# ---------------------------------------------------------------------------
# JSON with comment lines (copilot config.json)
# ---------------------------------------------------------------------------


def read_json_skipping_comment_lines(path: str) -> Any:
    """Read a JSON file, stripping lines starting with // before parsing."""
    with open(path, "r", encoding="utf-8") as f:
        raw = f.read()
    lines = [ln for ln in raw.splitlines() if not ln.strip().startswith("//")]
    return json.loads("\n".join(lines))


# ---------------------------------------------------------------------------
# run() scaffold (§3.1 layer 1)
# ---------------------------------------------------------------------------


def run(harness_name: str, provision_fn: Any) -> None:
    """Entry point scaffold for provision.py scripts.

    Handles argparse, manifest loading, command dispatch, and error mapping.
    provision_fn receives a ProvisionContext and should return None (success)
    or raise ProvisionError.
    """
    parser = argparse.ArgumentParser(
        description=f"{harness_name} container-side provisioner"
    )
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
        manifest = load_json(manifest_path)
    except FileNotFoundError:
        print(f"{harness_name} provision: manifest not found at {manifest_path}", file=sys.stderr)
        sys.exit(EXIT_ERROR)
    except (OSError, json.JSONDecodeError) as exc:
        print(f"{harness_name} provision: failed to load manifest {manifest_path}: {exc}", file=sys.stderr)
        sys.exit(EXIT_ERROR)

    if not isinstance(manifest, dict):
        print(f"{harness_name} provision: manifest is not an object", file=sys.stderr)
        sys.exit(EXIT_ERROR)

    command = str(manifest.get("command") or "provision")
    if command != "provision":
        print(f"{harness_name} provision: unsupported command {command!r}", file=sys.stderr)
        sys.exit(EXIT_UNSUPPORTED)

    ctx = ProvisionContext(harness_name, manifest)
    try:
        provision_fn(ctx)
    except ProvisionError as exc:
        print(f"{harness_name} provision: {exc}", file=sys.stderr)
        sys.exit(EXIT_ERROR)
    except OSError as exc:
        print(f"{harness_name} provision: {exc}", file=sys.stderr)
        sys.exit(EXIT_ERROR)

    sys.exit(EXIT_OK)


# ---------------------------------------------------------------------------
# capture_auth_main (§3.5)
# ---------------------------------------------------------------------------

_CA_EXIT_OK = 0
_CA_EXIT_ERROR = 1
_CA_EXIT_NO_CREDS = 2
_CA_EXIT_CONFLICT = 3


def capture_auth_main(argv: list[str] | None = None) -> int:
    """Portable capture-auth logic. Returns exit code."""
    parser = argparse.ArgumentParser(
        description="Capture auth credentials and store as project secrets"
    )
    parser.add_argument("--force", action="store_true", help="Overwrite existing secrets")
    parser.add_argument(
        "--bundle",
        default=os.path.join(
            os.environ.get("HOME") or os.path.expanduser("~"),
            ".scion", "harness",
        ),
        help="Path to harness bundle directory",
    )
    args = parser.parse_args(argv)

    config_path = os.path.join(args.bundle, "inputs", "capture-auth-config.json")
    if not os.path.isfile(config_path):
        print("capture-auth: no credential mappings found in inputs/capture-auth-config.json",
              file=sys.stderr)
        return _CA_EXIT_NO_CREDS

    try:
        with open(config_path, "r", encoding="utf-8") as f:
            data = json.load(f)
    except (json.JSONDecodeError, OSError):
        print("capture-auth: no credential mappings found in inputs/capture-auth-config.json",
              file=sys.stderr)
        return _CA_EXIT_NO_CREDS

    entries = data.get("credentials")
    if not isinstance(entries, list) or not entries:
        print("capture-auth: no credential mappings found in inputs/capture-auth-config.json",
              file=sys.stderr)
        return _CA_EXIT_NO_CREDS

    # Deduplicate by key — a credential may appear under multiple auth methods
    # (e.g. AGY_TOKEN under both oauth-token and vertex-ai) but should only be
    # captured once.  First entry wins.
    seen_keys: set[str] = set()
    unique_entries: list[dict[str, Any]] = []
    for entry in entries:
        key = entry.get("key", "")
        if key and key not in seen_keys:
            seen_keys.add(key)
            unique_entries.append(entry)
        elif not key:
            unique_entries.append(entry)
    entries = unique_entries

    captured = 0
    conflicts = 0
    errors = 0

    for entry in entries:
        key = entry.get("key", "<unknown>")
        source = entry.get("source", "")
        expanded = expand_path(source) if source else ""

        if not expanded or not os.path.isfile(expanded):
            print(f"capture-auth: {key}: source not found ({source})")
            continue

        ok, err = _capture_one_cred(entry, args.force)
        if err == "CONFLICT":
            print(f'CONFLICT: secret "{key}" already exists (use --force to overwrite)')
            conflicts += 1
        elif err:
            print(f"capture-auth: {key}: {err}", file=sys.stderr)
            errors += 1
        elif ok:
            print(f"capture-auth: {key}: captured from {source}")
            captured += 1

    if conflicts > 0:
        return _CA_EXIT_CONFLICT
    if errors > 0 and captured == 0:
        return _CA_EXIT_ERROR
    if captured == 0:
        print("capture-auth: no credentials found to capture")
        return _CA_EXIT_NO_CREDS

    print(f"capture-auth: {captured} credential(s) captured successfully")
    return _CA_EXIT_OK


def _capture_one_cred(entry: dict[str, Any], force: bool) -> tuple[bool, str | None]:
    key = entry.get("key", "")
    source = expand_path(entry.get("source", ""))
    secret_type = entry.get("type", "file")
    target = entry.get("target", "")

    if not key or not source:
        return False, "invalid entry: missing key or source"

    if not os.path.isfile(source):
        return False, None

    cmd = ["sciontool", "secret", "set", key, f"@{source}",
           "--type", secret_type, "--target", target]
    if force:
        cmd.append("--force")

    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        return False, "sciontool not found in PATH"
    except subprocess.TimeoutExpired:
        return False, f"sciontool timed out for key {key}"

    if result.returncode != 0:
        stderr = result.stderr.strip()
        if "already exists" in stderr.lower():
            return False, "CONFLICT"
        return False, f"sciontool failed for {key}: {stderr}"

    return True, None
