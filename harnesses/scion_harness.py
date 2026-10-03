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
import itertools
import json
import os
import re
import subprocess
import sys
import tempfile
import time
import tomllib
from typing import Any, Collection

# ---------------------------------------------------------------------------
# Version contract (§3.3)
# ---------------------------------------------------------------------------

INTERFACE_VERSION = 3
LIB_VERSION = "2026-10-02"

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


_atomic_tmp_name_counter = itertools.count()


def _atomic_tmp_name(name: str) -> str:
    """Returns the temp name atomic_write_json creates before renaming it
    into place: unique to this call (pid + a monotonic timestamp + a
    same-process call counter), so a stale leftover from an earlier,
    interrupted call can never collide with — and so can never block — a
    later one, and so an adversary who does not already share this process's
    pid cannot predict it in advance either. The counter is what keeps two
    calls unique even if the clock reading happens not to advance between
    them (a real possibility on a coarse-resolution clock, not just a
    theoretical one) — pid and time alone narrow the guess space but do not
    themselves guarantee two calls in the same process never produce the
    same name.

    A separate, named function purely so a test can monkeypatch it to a
    fixed, predictable name: that is the only way to deterministically plant
    a symlink or a FIFO at the exact path this call will try to create, to
    prove O_CREAT|O_EXCL|O_NOFOLLOW still refuses a pre-existing entry there,
    since the real name is not predictable from outside this process.
    """
    return f".{name}.tmp-{os.getpid()}-{time.monotonic_ns()}-{next(_atomic_tmp_name_counter)}"


def atomic_write_json(path: str, payload: Any) -> None:
    """Write JSON atomically: tmp file + os.replace, sorted keys, trailing newline.

    Every filesystem operation after the initial os.makedirs is anchored to an
    open directory file descriptor for path's parent, opened with
    O_DIRECTORY|O_NOFOLLOW: if the parent is a symlink (e.g. a workload
    replaced an expected directory with one pointing elsewhere), that open
    fails immediately instead of silently writing through it. The temp file
    is then created relative to that same directory fd with a name unique to
    this call (pid + a monotonic timestamp + a same-process counter, see
    _atomic_tmp_name, matching the Go side's ".<leaf>.tmp-<pid>-<ns>"
    convention) and O_CREAT|O_EXCL|O_NOFOLLOW, so a
    pre-existing entry at that exact name — a symlink to an arbitrary target,
    a leftover regular file, or a FIFO — also fails immediately (EEXIST)
    instead of being followed or, for a FIFO, blocking this call forever
    waiting for a reader. Because the name is unique per call, a stale temp
    file left behind by an earlier call that never reached os.replace (e.g.
    killed mid-write) can never collide with — and so can never permanently
    block — any later call, unlike a fixed ".tmp" name, which a single
    leftover would block forever. This makes the guard independent of which
    uid calls it: it refuses the same way whether this runs as root or as an
    unprivileged user.
    """
    directory = os.path.dirname(path) or "."
    name = os.path.basename(path)
    os.makedirs(directory, exist_ok=True)

    dir_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        tmp_name = _atomic_tmp_name(name)
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
        fd = os.open(tmp_name, flags, 0o666, dir_fd=dir_fd)
        try:
            try:
                f = os.fdopen(fd, "w", encoding="utf-8")
            except BaseException:
                # os.fdopen itself failed before wrapping fd in a file
                # object, so nothing owns fd yet and nothing will close it
                # unless this does.
                os.close(fd)
                raise
            with f:
                json.dump(payload, f, indent=2, sort_keys=True)
                f.write("\n")
            # Inside the same try as the write: a failed replace (e.g. a
            # cross-device rename, or the destination directory vanishing)
            # must clean up tmp_name exactly like a failed write does.
            os.replace(tmp_name, name, src_dir_fd=dir_fd, dst_dir_fd=dir_fd)
        except BaseException:
            try:
                os.unlink(tmp_name, dir_fd=dir_fd)
            except OSError:
                pass
            raise
    finally:
        os.close(dir_fd)


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
# Model resolution (G3)
# ---------------------------------------------------------------------------

# Shorthand spellings for model size aliases. Must stay in lockstep with
# config.NormalizeModelAlias (pkg/config/templates.go): the Go side resolves
# SCION_MODEL against the same shorthand set before it ever reaches a
# provisioner, so accepting a spelling here that Go does not recognize would
# make a provisioner's defense-in-depth resolution disagree with the broker.
_MODEL_ALIAS_SHORTHAND = {"s": "small", "m": "medium", "l": "large", "xl": "extra-large"}

# The canonical set of recognized model size aliases. Mirrors
# config.KnownModelAliases (pkg/config/templates.go).
_KNOWN_MODEL_ALIASES = frozenset({"small", "medium", "large", "extra-large"})


def normalize_model_alias(value: str, harness_config: dict[str, Any] | None) -> str:
    """Normalize *value* through *harness_config*'s model_aliases table.

    Factored out of resolve_model() so callers with a raw value that did not
    come from the SCION_MODEL environment variable (e.g. a harness's own
    harness_config.model) can be resolved through the exact same alias
    lookup, rather than reimplementing it or passed through unresolved. See
    resolve_model() for the precise normalization rules this applies.

    Empty/unset *value* returns "" so callers can apply their own default or
    pin.
    """
    value = value.strip()
    if not value:
        return ""
    normalized = value.lower()
    normalized = _MODEL_ALIAS_SHORTHAND.get(normalized, normalized)
    if normalized not in _KNOWN_MODEL_ALIASES:
        return value  # concrete model name: preserve the caller's spelling
    aliases = harness_config.get("model_aliases") if isinstance(harness_config, dict) else None
    if not isinstance(aliases, dict):
        aliases = {}
    return aliases.get(normalized, normalized)


def resolve_model(ctx: "ProvisionContext") -> str:
    """Resolve the effective model name for this harness's CLI/config.

    Python port of config.ResolveModelAlias/config.NormalizeModelAlias
    (pkg/config/templates.go), used as a defense-in-depth layer for
    resume/restart paths where the Go side (hub or broker) had no alias table
    to resolve SCION_MODEL against and passed a bare size alias straight
    through:

      1. Read SCION_MODEL from the environment — the broker-resolved value,
         as provisioners do today. Empty/unset returns "" so callers can
         apply their own default or pin.
      2. Normalize shorthand spellings (s/m/l/xl and case), matching Go's
         NormalizeModelAlias, to decide whether the value is a known size
         alias (small/medium/large/extra-large).
      3. If it is, map the normalized tier through this harness's own
         config.yaml model_aliases (ctx.harness_config). Otherwise it is
         already a concrete model name — return the caller's original
         (stripped) spelling unchanged, case included.

    Unlike Go's config.ResolveModelAlias, this does NOT lower-case concrete
    model names. Go's normalize-then-lookup only ever sees SCION_MODEL after
    the hub/broker has already resolved and lower-cased it (the config/
    --model path), so the lower-casing there is a no-op in practice. But
    SCION_MODEL can also arrive un-normalized from an explicit source (a
    template/hub `env:` block, `--env SCION_MODEL=...`) that Go never
    touches — see run.go's reResolveModelAlias, which only rewrites tier
    names, not concrete ones. Case-sensitive concrete IDs are real (e.g.
    OpenAI fine-tuned model suffixes), so lower-casing them here would break
    them with no upside. A tier alias is still safe to normalize: all five
    harnesses' model_aliases tables and the default pins are lowercase.

    A known tier missing from this harness's model_aliases passes through
    as the normalized tier name, matching Go. An unknown or concrete value
    passes through in its original spelling (see above).
    """
    raw = os.environ.get("SCION_MODEL", "").strip()
    if not raw:
        return ""
    return normalize_model_alias(raw, ctx.harness_config)


# ---------------------------------------------------------------------------
# Thinking level resolution (config.yaml `thinking:` block)
# ---------------------------------------------------------------------------

THINKING_LEVEL_ENV = "SCION_THINKING_LEVEL"


def parse_thinking_level(raw: str | None) -> tuple[int | None, bool]:
    """Parse a raw SCION_THINKING_LEVEL value into ``(level, invalid)``.

    This is the single parse-and-clamp rule for the canonical 0-100 thinking
    level:

      - ``None``, ``""`` or whitespace only -> ``(None, False)`` (unset).
      - Not parseable by ``int()`` after stripping (``"abc"``, ``"1.5"``)
        -> ``(None, True)`` (invalid; callers should warn).
      - Otherwise the integer clamped to 0..100 -> ``(n, False)``. ``int()``
        accepts signs, so ``"-5"`` -> 0 and ``"+7"`` -> 7.
    """
    if raw is None:
        return None, False
    text = str(raw).strip()
    if not text:
        return None, False
    try:
        level = int(text)
    except ValueError:
        return None, True
    return max(0, min(100, level)), False


def _thinking_table(thinking_cfg: Any) -> tuple[list[tuple[int, str]], str | None] | None:
    """Validate a harness_config ``thinking`` block.

    Returns ``(levels sorted by max, default or None)``, or ``None`` when the
    block is malformed: not a dict, ``levels`` not a non-empty list, an entry
    that is not a dict with an int ``max`` (bools rejected; the manifest is
    JSON) and a non-empty str ``value``, or a ``default`` that is present but
    not a non-empty str.
    """
    if not isinstance(thinking_cfg, dict):
        return None
    levels = thinking_cfg.get("levels")
    if not isinstance(levels, list) or not levels:
        return None
    table: list[tuple[int, str]] = []
    for entry in levels:
        if not isinstance(entry, dict):
            return None
        max_level = entry.get("max")
        value = entry.get("value")
        if not isinstance(max_level, int) or isinstance(max_level, bool):
            return None
        if not isinstance(value, str) or not value:
            return None
        table.append((max_level, value))
    default = thinking_cfg.get("default")
    if default is not None and (not isinstance(default, str) or not default):
        return None
    table.sort(key=lambda item: item[0])
    return table, default


def map_thinking_level(level: int, thinking_cfg: dict[str, Any] | None) -> str | None:
    """Map a clamped thinking *level* through a ``thinking`` block.

    Pure lookup: returns the ``value`` of the first entry (sorted by ``max``)
    whose ``max`` >= *level*. Returns ``None`` when *thinking_cfg* is missing
    or malformed. A level above the last ``max`` returns the last value
    (defensive; the Go load-time check requires the last ``max`` to be 100).
    """
    parsed = _thinking_table(thinking_cfg)
    if parsed is None:
        return None
    table, _ = parsed
    for max_level, value in table:
        if level <= max_level:
            return value
    return table[-1][1]


def resolve_thinking(ctx: "ProvisionContext", raw: str | None = None) -> str | None:
    """Resolve SCION_THINKING_LEVEL (or *raw*) to this harness's native value.

    The mapping comes from ``ctx.harness_config["thinking"]`` (the config.yaml
    ``thinking:`` block carried in the provision manifest):

      - No block: returns ``None``; logs an info line if a level was set.
      - Malformed block: warns, then behaves as if there were no block.
      - Invalid level (not an integer): warns and returns ``default``.
      - Unset/blank level: returns ``default``.
      - Valid level: clamps to 0..100 and returns the mapped value.

    ``default`` is the block's optional ``default``; when absent the result
    is ``None``, meaning the provisioner should emit nothing so the harness
    CLI's own default applies. Writing the value to the native knob stays in
    each provision.py.
    """
    if raw is None:
        raw = os.environ.get(THINKING_LEVEL_ENV)
    level, invalid = parse_thinking_level(raw)

    harness_config = ctx.harness_config if isinstance(ctx.harness_config, dict) else {}
    thinking_cfg = harness_config.get("thinking")
    parsed = None
    if thinking_cfg is not None:
        parsed = _thinking_table(thinking_cfg)
        if parsed is None:
            ctx.warn("harness_config thinking block is malformed; ignoring it")
    if parsed is None:
        if level is not None:
            ctx.info(f"thinking_level={level} ignored (harness has no thinking map)")
        elif invalid:
            ctx.info(f"thinking_level={str(raw).strip()!r} ignored (harness has no thinking map)")
        return None

    _, default = parsed
    default_label = default or "<cli default>"
    if invalid:
        ctx.warn(f"thinking_level={str(raw).strip()!r} is not a valid integer; value={default_label} (default)")
        return default
    if level is None:
        ctx.info(f"thinking_level=<unset>, value={default_label} (default)")
        return default
    value = map_thinking_level(level, thinking_cfg)
    ctx.info(f"thinking_level={level} value={value}")
    return value


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
    include_skills: bool = False,
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


# Matches a table header ([table] / [[table]]) with an optional trailing
# comment, and nothing else on the line. The header's inner content allows
# quoted segments (which may themselves contain `=`, `]`, `#`, or `[`, e.g.
# `[projects."/a=b"]`) as well as ordinary bare-key characters, but rejects
# anything else — in particular a trailing `,` (an array element, not a
# header) or an unquoted `=`/`]` (which would make it a key assignment, not
# a header). Used together with toml_entering_array_depths: a line can only
# be a genuine header if it also has zero open-bracket depth entering it —
# see that function's docstring for why the shape check alone isn't enough.
#
# In valid TOML outside a multi-line string, "depth == 0 and starts with
# '['" is already sufficient on its own — this shape check mostly overlaps
# with depth tracking (ptone/scion#2365 review round 4 mutation testing:
# reverting this regex to a bare `startswith("[")` while keeping depth
# tracking still passes the full suite). It's kept anyway as a second,
# independent line of defense: the one case where it earns its keep is a
# line *inside* a top-level multi-line string that happens to look like a
# header, e.g. `[projects."/a=b"]` in prose — there, toml_edit_preserves
# (the tomllib round-trip/preservation backstop below) is what actually
# fails the edit safely either way, but rejecting the shape earlier means
# fewer edits need that backstop to save them.
#
# Groups: (1) the opening bracket(s), (2) the inner content, (3) the
# closing bracket(s), (4) an optional trailing comment. Grouping doesn't
# change what the regex matches (only what's captured), so callers that
# only care whether a line is a header can keep using a plain `.match(...)
# is not None`; normalize_toml_header additionally uses groups 1-3 to build
# a canonical form for predicate matching.
TOML_TABLE_HEADER_RE = re.compile(
    r'^\s*(\[\[?)\s*((?:[^\[\]="\'#]|"(?:\\.|[^"\\])*"|\'[^\']*\')+)(\]\]?)\s*(#.*)?$'
)

# Matches a single-line basic ("...") or literal ('...') string, or a
# trailing comment, for masking before bracket-counting (see
# toml_mask_strings_and_comments). Does not match triple-quoted
# (multi-line) strings — toml_entering_array_depths tracks those itself
# (see _toml_code_and_ml_state) rather than via this regex, since a
# multi-line string's delimiter can open on one line and close many lines
# later.
_TOML_STR_OR_COMMENT_RE = re.compile(r'"(?:\\.|[^"\\])*"|\'[^\']*\'|#.*')

# Tokenizes a table header's inner content into alternating quoted/unquoted
# spans, for normalize_toml_header: whitespace is only insignificant in the
# unquoted spans.
_TOML_HEADER_TOKEN_RE = re.compile(r'"(?:\\.|[^"\\])*"|\'[^\']*\'|[^"\']+')


def toml_mask_strings_and_comments(line: str) -> str:
    """Blank out string literals and strip trailing comments so bracket
    counting on the result only sees TOML structure, not `[`/`]` characters
    that happen to appear inside a string value or a comment (e.g. `#
    temperature range [0, 1)` or `hint = "press [ to go"`, neither of which
    opens a real array)."""
    return _TOML_STR_OR_COMMENT_RE.sub(
        lambda m: "" if m.group(0).startswith("#") else '""', line
    )


def _toml_skip_closing_quotes(line: str, j: int, delim: str) -> int:
    """Index just past a multi-line string's closing delimiter found at
    `j` (a `delim`-length run — `\"\"\"` or `'''` — confirmed to start at
    `j`). Per TOML 1.0, 1-2 extra quote characters of the same kind
    immediately before the real closing delimiter are string *content*,
    not part of the delimiter — e.g. `\"\"\"say "hi\"\"\"\"` is the content
    `say "hi"` closed by the last three quotes, not content `say "hi` with
    a leftover `"` after it. Naively stopping right after the first
    3-quote run found (`j + 3`) leaves that leftover quote character to be
    mis-scanned as the *start* of a new single-line string, mis-pairing it
    with whatever quote character comes next on the line and miscounting
    any brackets in between as real structure (ptone/scion#2427 review
    round 5, R5-1). A run of 4 or 5 quote characters must be consumed as a
    whole, not just its first 3.
    """
    k = j + 3
    while k < len(line) and k < j + 5 and line[k] == delim[0]:
        k += 1
    return k


def _toml_code_and_ml_state(line: str, in_ml: str | None) -> tuple[str, str | None]:
    """Strip string literals and comments from `line` for bracket counting,
    tracking a triple-quoted multi-line string ('\"\"\"' or "'''") that may
    still be open entering this line (`in_ml`: the open delimiter, or
    None), and may still be open leaving it.

    Returns (code, exit_state): `code` is `line` with every string literal
    and trailing comment removed outright (not replaced with a same-length
    placeholder — unlike toml_mask_strings_and_comments, callers here only
    ever `.count()` brackets in the result, so the placeholder doesn't
    matter and dropping is simpler); `exit_state` is the delimiter still
    open at the end of the line, or None.

    This is a character scan rather than a regex because the multi-line
    case requires carrying state *across* lines: a naive per-line regex
    (as toml_mask_strings_and_comments uses) has no way to know a line is
    the *body* of a string that opened on an earlier line, so literal `[`/
    `]` characters in ordinary prose there would be miscounted as real
    structure — exactly the bug this closes (ptone/scion#2427 review round
    1 R1): an unbalanced bracket in multi-line prose (e.g. "Prefix tasks
    with [TODO or [WIP when unfinished.") used to permanently corrupt
    toml_entering_array_depths' running depth for every later line in the
    file, since the old per-line masking had no memory of being inside a
    string at all.

    An unterminated *single*-line string (no closing quote before EOL) is
    left unmasked from the opening quote onward, matching
    toml_mask_strings_and_comments' single-line contract — that shape is
    already invalid TOML the tomllib backstop (toml_edit_preserves) will
    reject regardless, so there's no multi-line state to track for it.

    A multi-line *basic* string (`\"\"\"`) supports backslash escapes, so a
    backslash-escaped quote is skipped while searching for the closing
    delimiter — otherwise an escaped closing-delimiter sequence embedded in
    the string body (TOML's way to write a literal triple-quote inside one)
    would close the string early (ptone/scion#2427 review round 2,
    "Consider 2"). A multi-line *literal* string (`'''`) has no escapes at
    all in TOML, so no such handling applies there — a `'''` can never
    legally appear in a literal multi-line string's body at all, escaped or
    not.

    There is exactly one place in this function that ever searches for a
    closing delimiter (`\"\"\"` or `'''`) — the state-scan branch just
    above, which every opening delimiter hands off to immediately (setting
    `state` and looping back around) rather than also searching for its
    own closer. An earlier version had a second, non-escape-aware search
    in the opening branch for a delimiter closing on the *same* line —
    correct for `'''` (no escapes to consider) but wrong for `\"\"\"`,
    since an escaped closing-delimiter sequence on that same line closed
    the string early exactly as it did before "Consider 2", just one line
    earlier than that fix covered (ptone/scion#2427 review round 4,
    "R4-1").
    """
    out: list[str] = []
    i, n = 0, len(line)
    state = in_ml
    while i < n:
        if state == '"""':
            j = i
            found = -1
            while j < n:
                if line[j] == "\\":
                    j += 2
                    continue
                if line[j:j + 3] == state:
                    found = j
                    break
                j += 1
            if found == -1:
                return "".join(out), state
            i = _toml_skip_closing_quotes(line, found, state)
            state = None
            continue
        if state is not None:
            j = line.find(state, i)
            if j == -1:
                return "".join(out), state
            i = _toml_skip_closing_quotes(line, j, state)
            state = None
            continue
        ch = line[i]
        if ch == "#":
            break
        if line[i:i + 3] in ('"""', "'''"):
            # Hand off to the state-scan branch above (loop back around)
            # rather than searching for the closing delimiter here too: a
            # second, non-escape-aware search here closed an escaped
            # closing-delimiter sequence early when it appeared on the same
            # line that opened the string (ptone/scion#2427 review round 4,
            # R4-1) — the exact bug the state-scan branch was already
            # hardened against for a string that opened on an *earlier*
            # line. Falling through instead of duplicating the search means
            # there is only one place that ever looks for a `"""` closing
            # delimiter, and it's the escape-aware one.
            state = line[i:i + 3]
            i += 3
            continue
        if ch == '"':
            j = i + 1
            while j < n and line[j] != '"':
                if line[j] == "\\":
                    j += 1
                j += 1
            if j >= n:
                out.append(ch)
                i += 1
                continue
            i = j + 1
            continue
        if ch == "'":
            j = line.find("'", i + 1)
            if j == -1:
                out.append(ch)
                i += 1
                continue
            i = j + 1
            continue
        out.append(ch)
        i += 1
    return "".join(out), state


# A real bracket-nesting depth is always >= 0; toml_entering_array_depths
# uses this sentinel for a line whose entering position is inside a
# still-open multi-line string, so is_toml_table_header's `depth == 0`
# check also (correctly) excludes it without needing a second signal.
_TOML_IN_MULTILINE_STRING = -1


def toml_entering_array_depths(lines: list[str]) -> list[int]:
    """Per-line array-bracket depth (unmatched `[` count carried in from
    prior lines), or `_TOML_IN_MULTILINE_STRING` (-1) if the line's
    entering position is inside a still-open multi-line ('\"\"\"' or
    "'''") string.

    A line inside a multi-line array can itself be a bracketed value (e.g.
    a nested single-element array `["x"]`, syntactically indistinguishable
    by shape alone from a table header spelled with a quoted key, `["x"]`)
    — so a naive "line starts with `[`" check misidentifies an array
    continuation line as a table header. Tracking bracket depth resolves
    the ambiguity: a `[`-shaped line only means "table header" when depth
    is zero entering it, i.e. no multi-line array is still open.

    Brackets are counted after stripping string literals and comments
    (_toml_code_and_ml_state), so a stray `[`/`]` inside either one doesn't
    throw off the whole file's section tracking — an earlier version of
    this function counted raw brackets and could silently delete
    table-scoped keys (e.g. `[profiles.fast]`'s `model`) after a single
    unbalanced bracket in an unrelated comment (ptone/scion#2365 review
    round 3). The stripping is multi-line-string-aware (unlike the
    single-line-only toml_mask_strings_and_comments), so the same applies
    to a bracket anywhere in a `\"\"\"`/`'''` block's body, however many
    lines it spans (ptone/scion#2427 review round 1): those lines are
    reported with the -1 sentinel and contribute nothing to depth, rather
    than corrupting it for the rest of the file. This also closes the
    previously-documented gap where a header-shaped line *inside* such a
    string (with balanced brackets, so it wouldn't have corrupted depth
    either way) was mistaken for a real header — is_toml_table_header now
    correctly excludes it via the same sentinel. _toml_code_and_ml_state
    also understands backslash-escaped delimiters inside a multi-line
    *basic* string (ptone/scion#2427 review round 2, "Consider 2"), so a
    delimiter sequence embedded in the string body via escaping doesn't
    close it early either. The tomllib backstop, toml_edit_preserves,
    remains the last line of defense for anything past that — this is
    still a line-oriented scanner, not a full TOML tokenizer.
    """
    depths: list[int] = []
    depth = 0
    in_ml: str | None = None
    for line in lines:
        depths.append(_TOML_IN_MULTILINE_STRING if in_ml is not None else depth)
        code, in_ml = _toml_code_and_ml_state(line, in_ml)
        depth = max(0, depth + code.count("[") - code.count("]"))
    return depths


def is_toml_table_header(line: str, depth: int) -> bool:
    """True if `line` is a top-level table header, given the bracket-nesting
    `depth` entering it (0 means no multi-line array is currently open;
    `_TOML_IN_MULTILINE_STRING` means the line is inside a still-open
    multi-line string, so its raw text is prose/content, not TOML syntax,
    regardless of what it looks like)."""
    return depth == 0 and TOML_TABLE_HEADER_RE.match(line) is not None


def normalize_toml_header(line: str) -> str | None:
    """Canonical form of a TOML table-header line, for matching against a
    header_predicate without being tripped up by cosmetic differences: a
    trailing comment (`[cli] # note`) is dropped, and whitespace around the
    brackets or a dotted key (`[ models ]`) is removed. Whitespace *inside*
    a quoted key (`["a b"]`) is left alone, since it's part of the key's
    value rather than formatting. Returns None if `line` isn't shaped like
    a table header at all — callers should generally gate on
    is_toml_table_header (which also checks bracket-nesting depth) before
    trusting this.
    """
    m = TOML_TABLE_HEADER_RE.match(line)
    if m is None:
        return None
    open_br, inner, close_br = m.group(1), m.group(2), m.group(3)
    cleaned = "".join(
        token if token[:1] in ("'", '"') else re.sub(r"\s+", "", token)
        for token in _TOML_HEADER_TOKEN_RE.findall(inner)
    )
    return f"{open_br}{cleaned}{close_br}"


def strip_toml_sections(content: str, header_predicate: Any) -> str:
    """Remove TOML sections whose header line matches the predicate.

    header_predicate(normalized_header) -> bool, where normalized_header is
    the header's canonical form (see normalize_toml_header): a trailing
    comment is dropped and whitespace inside the brackets or around a
    dotted key is removed, so a predicate written as `line == "[cli]"` also
    matches `[ cli ] # note`.

    Header detection tracks bracket-nesting depth (see
    toml_entering_array_depths), so a line that is really a nested-array
    element on its own line (e.g. `["a", "b"]` inside a still-open
    multi-line array) is never mistaken for a table header, and it tracks
    multi-line ('\"\"\"'/"'''") strings across lines — including
    backslash-escaped closing-delimiter sequences inside a multi-line
    *basic* string — so a header-shaped line, or an unbalanced bracket in
    ordinary prose, inside one of those is correctly treated as string
    content, not TOML structure, however many lines the string spans.

    This is still line-oriented, not a full TOML tokenizer, so callers that
    write the result back to disk should still validate it with
    toml_edit_preserves (or write_toml_if_preserves) before persisting, as
    defense-in-depth against any other case this scanner doesn't
    understand.

    Also consumes blank lines immediately preceding a removed header.
    """
    lines = content.split("\n")
    depths = toml_entering_array_depths(lines)
    keep = [True] * len(lines)
    i = 0
    while i < len(lines):
        header = normalize_toml_header(lines[i]) if is_toml_table_header(lines[i], depths[i]) else None
        if header is not None and header_predicate(header):
            section_start = i
            section_end = len(lines)
            for j in range(i + 1, len(lines)):
                if is_toml_table_header(lines[j], depths[j]):
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


def _is_toml_key_line(line: str, key: str) -> bool:
    """True if line is a top-level TOML assignment for exactly `key`."""
    s = line.strip()
    if not s.startswith(key):
        return False
    rest = s[len(key):]
    return len(rest) > 0 and rest[0] in (" ", "=", "\t")


def strip_toml_top_level_key(content: str, key: str) -> str:
    """Remove a top-level TOML key = value line from content.

    A line whose entering depth is `_TOML_IN_MULTILINE_STRING` is string
    content, not TOML syntax, regardless of what it looks like — a prose
    line like "model choice matters." inside a multi-line string must not
    be mistaken for a real `model = ...` assignment and stripped from the
    string's body (ptone/scion#2427 review round 2, "Consider 1"). This
    mirrors is_toml_table_header's `depth == 0` check, which already
    excludes such lines from header detection the same way.
    """
    lines = content.split("\n")
    depths = toml_entering_array_depths(lines)
    kept = []
    in_section = False
    for line, depth in zip(lines, depths):
        if is_toml_table_header(line, depth):
            in_section = True
        if not in_section and depth >= 0 and _is_toml_key_line(line, key):
            continue
        kept.append(line)
    return "\n".join(kept)


def insert_toml_top_level_line(content: str, line: str) -> str:
    """Insert a top-level `key = value` line before the first table header.

    TOML requires top-level keys to precede every `[table]`/`[[array-of-
    tables]]` header; a key appended after one is parsed as belonging to
    that table instead of being a top-level key. Appending at EOF used to
    land keys inside whatever table happened to be last in the file, both
    having no effect on the harness reading them and producing a
    duplicate-key TOML parse error on the next provision, since
    strip_toml_top_level_key only looks at top-level lines and can't find
    (or remove) the misplaced copy (ptone/scion#2365). Inserting here keeps
    the reconcile idempotent: the next call's strip finds this line at top
    level and removes it cleanly before a fresh copy is inserted in the
    same place.
    """
    lines = content.split("\n")
    depths = toml_entering_array_depths(lines)
    insert_at = len(lines)
    for i, (existing, depth) in enumerate(zip(lines, depths)):
        if is_toml_table_header(existing, depth):
            insert_at = i
            break
    lines.insert(insert_at, line)
    return "\n".join(lines)


def _drop_toml_path(data: dict[str, Any], path: tuple[str, ...]) -> None:
    """Remove the nested key at `path` from `data` in place, if present."""
    node = data
    for key in path[:-1]:
        if not isinstance(node, dict) or key not in node:
            return
        node = node[key]
    if isinstance(node, dict):
        node.pop(path[-1], None)


def toml_edit_preserves(
    original: str,
    content: str,
    managed_keys: Collection[str | tuple[str, ...]] = (),
) -> bool:
    """True if `content` is a safe edit of `original`.

    "Safe" means: `content` parses as TOML, and everything not covered by
    `managed_keys` is unchanged from `original`. Each entry in
    `managed_keys` is either:

      - a bare top-level key (a `str`) — the whole top-level table or
        value is allowed to change freely; or
      - a key-path (a `tuple[str, ...]`, e.g. `("model", "vertex-grok")`)
        — only that specific nested subtree is allowed to change, and
        every *other* entry under the same parent table(s) must stay the
        same.

    Use a key-path when a write only owns one sub-table of a larger,
    potentially-shared top-level table — e.g. one `[model.<name>]` block
    among several a user or another tool might add. A bare top-level key
    for `"model"` would let that write silently delete an unrelated
    `[model.custom]` sibling if strip_toml_sections' section-boundary
    detection ever went wrong for that input; a `("model", "<name>")`
    key-path can't be fooled that way, since only its own subtree is ever
    excluded from the comparison (ptone/scion#2427 review round 1).

    This is the backstop for this module's line-oriented TOML editing
    (strip_toml_sections, strip_toml_top_level_key,
    insert_toml_top_level_line), which — despite the string/comment/
    multi-line-string masking and bracket-depth tracking they use — are
    still not a full TOML tokenizer. Comparing everything the caller
    doesn't own (at whatever key-path granularity it declares) catches any
    class of line-oriented editing mistake, not just the specific shapes
    those helpers are already hardened against. Callers should leave the
    existing file untouched (logging a warning) when this returns False —
    see write_toml_if_preserves.

    If `original` doesn't parse (missing, empty, or already-invalid file),
    there's no baseline to diff against, so only "does `content` parse" is
    checked.

    `managed_keys` is iterated more than once, so it is materialized into a
    tuple immediately — a one-shot generator would otherwise be silently
    exhausted after the first pass, and every managed key after that would
    look unmanaged, over-rejecting the edit (ptone/scion#2427 review round
    2). Typed as `Collection` rather than `Iterable` for the same reason.
    """
    managed_keys = tuple(managed_keys)
    try:
        after = tomllib.loads(content)
    except tomllib.TOMLDecodeError:
        return False
    try:
        before = tomllib.loads(original)
    except tomllib.TOMLDecodeError:
        return True

    top_level = {k for k in managed_keys if isinstance(k, str)}
    paths = [k for k in managed_keys if not isinstance(k, str) and k and k[0] not in top_level]
    path_roots = {p[0] for p in paths}

    def _prepare(data: dict[str, Any]) -> dict[str, Any]:
        data = {k: v for k, v in data.items() if k not in top_level}
        for path in paths:
            _drop_toml_path(data, path)
        # A key-path drop can leave its root key pointing at an emptied-out
        # dict (e.g. `{"model": {}}`) purely as an artifact of removing the
        # one sub-table this write owns. Normalize that to "key absent" so
        # a table that exists only because of that sub-table compares
        # equal, on both sides of the diff, to one that never existed —
        # otherwise the very first write to a file lacking the parent
        # table entirely would be rejected as "changing" an unmanaged key
        # that in fact never had any content of its own.
        for root in path_roots:
            if isinstance(data.get(root), dict) and not data[root]:
                del data[root]
        return data

    return _prepare(before) == _prepare(after)


def write_toml_if_preserves(
    ctx: "ProvisionContext",
    path: str,
    original: str,
    content: str,
    managed_keys: Collection[str | tuple[str, ...]] = (),
    *,
    what: str = "",
    mode: int | None = None,
) -> bool:
    """Validate `content` against `original` with toml_edit_preserves, and
    atomically write it to `path` only if it passes. On failure, leaves
    `path` untouched and logs a warning via `ctx.warn` — naming `what` (a
    short caller-supplied label for the change being attempted, e.g.
    "vertex-ai auth/model config") and the managed keys, so the warning is
    actionable rather than generic (ptone/scion#2427 review round 1, R2):
    which step failed, and what it owned, is visible without reading the
    caller's source. Returns whether the write happened — a change this
    fatal to skip (e.g. vertex-ai routing) should have its caller check the
    return value rather than assume success.

    `managed_keys` is materialized into a tuple immediately (see
    toml_edit_preserves) since it is iterated again below to build the
    warning message.
    """
    managed_keys = tuple(managed_keys)
    if not toml_edit_preserves(original, content, managed_keys):
        managed_desc = ", ".join(
            k if isinstance(k, str) else ".".join(k) for k in managed_keys
        ) or "none"
        label = f"{what}: " if what else ""
        ctx.warn(
            f"{label}generated TOML for {path} did not preserve existing "
            f"unmanaged content when round-tripped through tomllib "
            f"(managed: [{managed_desc}]); leaving the file untouched — "
            f"{what or 'this change'} NOT applied"
        )
        return False
    atomic_write_text(path, content, mode=mode)
    return True


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
        description="Capture auth credentials and store as secrets"
    )
    parser.add_argument("--force", action="store_true", help="Overwrite existing secrets")
    parser.add_argument(
        "--scope",
        choices=["project", "user"],
        default="user",
        help="Secret scope: project or user (default)",
    )
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

        ok, err = _capture_one_cred(entry, args.force, args.scope)
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


def _capture_one_cred(entry: dict[str, Any], force: bool, scope: str = "user") -> tuple[bool, str | None]:
    key = entry.get("key", "")
    source = expand_path(entry.get("source", ""))
    secret_type = entry.get("type", "file")
    target = entry.get("target", "")

    if not key or not source:
        return False, "invalid entry: missing key or source"

    if not os.path.isfile(source):
        return False, None

    cmd = ["sciontool", "secret", "set", key, f"@{source}",
           "--type", secret_type, "--target", target,
           "--scope", scope]
    if scope == "user":
        cmd.append("--allow-progeny")
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
