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
"""pi container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook. The
host has already staged this script and config.yaml under
$HOME/.scion/harness/ and projected any declared provider API-key variables
into the launch environment (they are stripped from THIS script's env, so
credential *names* come from inputs/auth-candidates.json).

This script:

  1. Selects the auth method (api-key env var, or a staged auth-file secret).
  2. Writes ~/.pi/agent/auth.json (0600) for auth-file, but never replaces an
     existing file: pi rewrites it when an OAuth token is refreshed, and a
     stale staged copy must not clobber the newer tokens.
  3. Merges settings into ~/.pi/agent/settings.json, keeping user edits.
     Everything is overridable by environment variable:
       SCION_PI_PROJECT_TRUST  never (default) | ask | always
       SCION_PI_PROVIDER       provider name for defaultProvider
       SCION_MODEL             concrete provider/id also sets defaultModel
       SCION_PI_SETTINGS       JSON object deep-merged into settings.json;
                               keys that look like credentials are rejected
  4. Projects scion instructions and the system prompt into pi's native
     AGENTS.md / APPEND_SYSTEM.md, and translates MCP servers into mcp.json.
  5. Writes outputs/resolved-auth.json and outputs/env.json.

stdlib-only and idempotent: it runs on every start and resume.
"""

from __future__ import annotations

import contextlib
import copy
import json
import os
import re
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness as sh  # noqa: E402

assert sh.INTERFACE_VERSION >= 2, (
    "pi provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {sh.INTERFACE_VERSION}. Update the staged scion_harness.py."
)

AGENT_DIR = "~/.pi/agent"
PI_AUTH_FILE = f"{AGENT_DIR}/auth.json"
PI_SETTINGS_FILE = f"{AGENT_DIR}/settings.json"
PI_MCP_FILE = f"{AGENT_DIR}/mcp.json"
PI_INSTRUCTIONS_FILE = f"{AGENT_DIR}/AGENTS.md"
PI_SYSTEM_PROMPT_FILE = f"{AGENT_DIR}/APPEND_SYSTEM.md"

PROJECT_TRUST_ENV = "SCION_PI_PROJECT_TRUST"
PROVIDER_ENV = "SCION_PI_PROVIDER"
SETTINGS_ENV = "SCION_PI_SETTINGS"
TRUST_VALUES = ("never", "ask", "always")
DEFAULT_PROJECT_TRUST = "never"

# Defaults applied only where the settings file does not already set a key.
DEFAULT_SETTINGS: dict[str, Any] = {
    "quietStartup": True,
    "enableInstallTelemetry": False,
}

VALID_AUTH_TYPES = ("api-key", "auth-file")
_TIER_NAMES = frozenset({"small", "medium", "large", "extra-large", "s", "m", "l", "xl"})
_PROVIDER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
_CREDENTIAL_KEY_RE = re.compile(
    r"(api[_-]?key|secret|passw|credential|authoriz|bearer|token$|^(key|pat)$|[_-]key$)",
    re.IGNORECASE,
)
# camelCase names ending in Key (e.g. secretKey) without matching "monkey".
_CAMEL_KEY_RE = re.compile(r"[a-z0-9]Key$")

# Provider variables mirror config.yaml's auth.types.api-key.required_env.
# Anthropic is listed first and wins when several are present.
API_KEY_VARS = [
    "ANTHROPIC_API_KEY",
    "AI_GATEWAY_API_KEY",
    "ANT_LING_API_KEY",
    "BASETEN_API_KEY",
    "CEREBRAS_API_KEY",
    "CLOUDFLARE_API_KEY",
    "DEEPSEEK_API_KEY",
    "FIREWORKS_API_KEY",
    "GEMINI_API_KEY",
    "GROQ_API_KEY",
    "KIMI_API_KEY",
    "META_API_KEY",
    "MINIMAX_API_KEY",
    "MINIMAX_CN_API_KEY",
    "MISTRAL_API_KEY",
    "MOONSHOT_API_KEY",
    "NVIDIA_API_KEY",
    "OPENAI_API_KEY",
    "OPENCODE_API_KEY",
    "OPENROUTER_API_KEY",
    "QWEN_TOKEN_PLAN_API_KEY",
    "QWEN_TOKEN_PLAN_CN_API_KEY",
    "RADIUS_API_KEY",
    "TOGETHER_API_KEY",
    "TYPESAFE_API_KEY",
    "XAI_API_KEY",
    "XIAOMI_API_KEY",
    "XIAOMI_TOKEN_PLAN_AMS_API_KEY",
    "XIAOMI_TOKEN_PLAN_CN_API_KEY",
    "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
    "ZAI_API_KEY",
    "ZAI_CODING_CN_API_KEY",
]

AUTH = sh.AuthSpec(
    harness="pi",
    methods=[
        sh.env_method(
            "api-key",
            any_of=API_KEY_VARS,
            hint="set ANTHROPIC_API_KEY (or another pi provider API key variable)",
        ),
        sh.file_method("auth-file", path=PI_AUTH_FILE, secret_key="PI_AUTH"),
    ],
)


# ---------------------------------------------------------------------------
# Auth file
# ---------------------------------------------------------------------------


@contextlib.contextmanager
def _private_umask():
    """Create files 0600 from the start (no world-readable window before chmod)."""
    old = os.umask(0o077)
    try:
        yield
    finally:
        os.umask(old)


def _write_auth_file(ctx: sh.ProvisionContext) -> bool:
    """Write ~/.pi/agent/auth.json from the staged PI_AUTH secret.

    Returns True if the file was written. An existing file is kept (and its
    mode tightened) because pi refreshes OAuth tokens in place.
    """
    if "PI_AUTH" not in ctx.file_secret_files:
        return False
    content = ctx.read_file_secret("PI_AUTH")
    if not content.strip():
        raise sh.ProvisionError("PI_AUTH secret is empty")
    try:
        parsed = json.loads(content)
    except json.JSONDecodeError as exc:
        # Report only the parser's position, never the secret text.
        raise sh.ProvisionError(
            f"PI_AUTH secret is not valid JSON (line {exc.lineno}, column {exc.colno})"
        ) from None
    if not isinstance(parsed, dict):
        raise sh.ProvisionError("PI_AUTH secret must be a JSON object")
    target = sh.expand_path(PI_AUTH_FILE)
    if os.path.exists(target):
        os.chmod(target, 0o600)
        ctx.info(f"{target} already exists; kept (delete it to apply the staged PI_AUTH secret)")
        return False
    with _private_umask():
        sh.atomic_write_text(target, content + "\n", mode=0o600)
    return True


# ---------------------------------------------------------------------------
# Settings
# ---------------------------------------------------------------------------


def _load_json_object(path: str, what: str) -> dict[str, Any]:
    """Load a JSON object; {} if the file is absent. Raises on malformed input."""
    if not os.path.exists(path):
        return {}
    try:
        data = sh.load_json(path)
    except (OSError, ValueError) as exc:
        raise sh.ProvisionError(f"{what} {path} is not valid JSON: {exc}") from None
    if not isinstance(data, dict):
        raise sh.ProvisionError(f"{what} {path} must be a JSON object")
    return data


def _deep_merge(base: dict[str, Any], overlay: dict[str, Any]) -> dict[str, Any]:
    for key, value in overlay.items():
        if isinstance(value, dict) and isinstance(base.get(key), dict):
            _deep_merge(base[key], value)
        else:
            base[key] = copy.deepcopy(value)
    return base


def _credential_paths(value: Any, prefix: str = "") -> list[str]:
    """Return dotted paths of keys that look like credentials (names only)."""
    found: list[str] = []
    if isinstance(value, dict):
        for k, v in value.items():
            path = f"{prefix}.{k}" if prefix else str(k)
            if _CREDENTIAL_KEY_RE.search(str(k)) or _CAMEL_KEY_RE.search(str(k)):
                found.append(path)
            else:
                found.extend(_credential_paths(v, path))
    elif isinstance(value, list):
        for i, v in enumerate(value):
            found.extend(_credential_paths(v, f"{prefix}[{i}]"))
    return found


def _env_settings_overlay() -> dict[str, Any]:
    raw = os.environ.get(SETTINGS_ENV, "").strip()
    if not raw:
        return {}
    try:
        overlay = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise sh.ProvisionError(
            f"{SETTINGS_ENV} is not valid JSON (line {exc.lineno}, column {exc.colno})"
        ) from None
    if not isinstance(overlay, dict):
        raise sh.ProvisionError(f"{SETTINGS_ENV} must be a JSON object")
    bad = _credential_paths(overlay)
    if bad:
        raise sh.ProvisionError(
            f"{SETTINGS_ENV} must not carry credentials; rejected keys: {', '.join(sorted(bad))}. "
            "Use a provider API-key variable or the auth-file secret instead"
        )
    return overlay


def _validate_trust(value: Any, source: str) -> str:
    text = str(value).strip().lower() if isinstance(value, str) else ""
    if text not in TRUST_VALUES:
        raise sh.ProvisionError(
            f"{source}: invalid project trust {value!r}; valid values are: {', '.join(TRUST_VALUES)}"
        )
    return text


def effective_project_trust() -> str:
    raw = os.environ.get(PROJECT_TRUST_ENV, "").strip()
    if not raw:
        return DEFAULT_PROJECT_TRUST
    return _validate_trust(raw, PROJECT_TRUST_ENV)


def _is_concrete_model(raw: str) -> bool:
    return bool(raw) and raw.lower() not in _TIER_NAMES


def _model_settings(ctx: sh.ProvisionContext) -> dict[str, str]:
    """defaultProvider/defaultModel from SCION_PI_PROVIDER and a concrete SCION_MODEL.

    Size aliases resolve to pi --model *patterns* (see config.yaml), which are
    not exact IDs, so they are not written here; Scion passes the resolved
    value to pi with --model.
    """
    out: dict[str, str] = {}
    provider = os.environ.get(PROVIDER_ENV, "").strip()
    if provider:
        if not _PROVIDER_RE.match(provider):
            raise sh.ProvisionError(f"{PROVIDER_ENV}: invalid provider name {provider!r}")
        out["defaultProvider"] = provider
    raw = os.environ.get("SCION_MODEL", "").strip()
    if _is_concrete_model(raw):
        model = sh.resolve_model(ctx)
        prov, sep, model_id = model.partition("/")
        if sep and prov and model_id:
            out.setdefault("defaultProvider", prov)
            out["defaultModel"] = model_id.split(":", 1)[0]
        else:
            out["defaultModel"] = model.split(":", 1)[0]
    return out


def _write_settings(ctx: sh.ProvisionContext) -> dict[str, Any]:
    path = sh.expand_path(PI_SETTINGS_FILE)
    settings = _load_json_object(path, "pi settings")

    for key, value in DEFAULT_SETTINGS.items():
        settings.setdefault(key, copy.deepcopy(value))

    overlay = _env_settings_overlay()
    if "defaultProjectTrust" in overlay:
        print(
            f"pi provision: warning: {SETTINGS_ENV} cannot set defaultProjectTrust; "
            f"use {PROJECT_TRUST_ENV}",
            file=sys.stderr,
        )
        del overlay["defaultProjectTrust"]
    _deep_merge(settings, overlay)
    settings.update(_model_settings(ctx))

    # The policy is recomputed on every start from the env alone (default
    # "never"): a value persisted by an earlier run, a hand edit or the overlay
    # can neither keep trust raised nor raise it. The bridge extension enforces
    # the same value above pi's saved trust.json decisions.
    settings["defaultProjectTrust"] = effective_project_trust()

    sh.atomic_write_json(path, settings)
    return settings


# ---------------------------------------------------------------------------
# MCP translation
# ---------------------------------------------------------------------------

_MCP_NAME_RE = re.compile(r"^[A-Za-z0-9_-]+$")


def _translate_mcp_server(name: str, spec: dict[str, Any]) -> dict[str, Any] | None:
    """Translate a universal MCPServerConfig into pi's mcp.json entry.

    pi supports stdio and streamable HTTP only; SSE is rejected by pi and is
    skipped here with a warning. Returns None for unusable entries.
    """
    if not _MCP_NAME_RE.match(name):
        print(f"pi provision: mcp server {name!r}: pi allows only letters, digits, '_' and '-' in names", file=sys.stderr)
        return None
    transport = (spec.get("transport") or "").strip()

    if transport == "stdio":
        cmd = spec.get("command")
        if not isinstance(cmd, str) or not cmd:
            print(f"pi provision: mcp server {name!r}: stdio transport missing command", file=sys.stderr)
            return None
        out: dict[str, Any] = {"command": cmd}
        args = spec.get("args")
        if isinstance(args, list) and args:
            out["args"] = [str(a) for a in args]
        env = spec.get("env")
        if isinstance(env, dict) and env:
            out["env"] = {str(k): str(v) for k, v in env.items()}
        return out

    if transport == "streamable-http":
        url = spec.get("url")
        if not isinstance(url, str) or not url:
            print(f"pi provision: mcp server {name!r}: streamable-http transport missing url", file=sys.stderr)
            return None
        out = {"url": url}
        headers = spec.get("headers")
        if isinstance(headers, dict) and headers:
            out["headers"] = {str(k): str(v) for k, v in headers.items()}
        return out

    if transport == "sse":
        print(f"pi provision: warning: mcp server {name!r}: pi does not support the SSE transport; skipped", file=sys.stderr)
        return None

    print(f"pi provision: mcp server {name!r}: unsupported transport {transport!r}", file=sys.stderr)
    return None


def _write_mcp_config(servers: dict[str, Any]) -> None:
    path = sh.expand_path(PI_MCP_FILE)
    try:
        config = _load_json_object(path, "pi mcp config")
    except sh.ProvisionError as exc:
        raise OSError(str(exc)) from None
    block = config.get("mcpServers")
    if not isinstance(block, dict):
        block = {}
    block.update(servers)
    config["mcpServers"] = block
    # Entries can carry env/headers tokens, so the file is owner-only.
    with _private_umask():
        sh.atomic_write_json(path, config)


# ---------------------------------------------------------------------------
# Prompts
# ---------------------------------------------------------------------------


def _write_system_prompt(ctx: sh.ProvisionContext) -> None:
    prompt = ctx.read_input_text("system-prompt.md")
    if not prompt.strip():
        return
    target = sh.expand_path(PI_SYSTEM_PROMPT_FILE)
    sh.atomic_write_text(target, prompt)
    ctx.info(f"wrote system prompt to {target}")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def provision(ctx: sh.ProvisionContext) -> None:
    explicit = ctx.explicit_type
    if explicit and explicit not in VALID_AUTH_TYPES:
        raise sh.ProvisionError(
            f"pi: unknown auth type {explicit!r}; valid types are: {', '.join(VALID_AUTH_TYPES)}"
        )
    resolved = ctx.select_auth(AUTH)

    extra: dict[str, Any] = {}
    if resolved.method == "auth-file":
        extra["auth_file_written"] = _write_auth_file(ctx)

    settings = _write_settings(ctx)

    instructions_target = sh.expand_path(PI_INSTRUCTIONS_FILE)
    os.makedirs(os.path.dirname(instructions_target), exist_ok=True)
    sh.project_instructions(ctx, instructions_target, system_prompt_mode="none")
    _write_system_prompt(ctx)
    sh.apply_mcp_translated(ctx, _translate_mcp_server, _write_mcp_config)

    # The container env (config.yaml env:) already sets PI_SKIP_VERSION_CHECK
    # and PI_TELEMETRY; repeating them here keeps them set if a project
    # override of env: drops the block. Container env still wins when set.
    env = {"PI_SKIP_VERSION_CHECK": "1", "PI_TELEMETRY": "0"}
    ctx.write_outputs(resolved, env=env, extra=extra)

    ctx.info(f"method={resolved.method} project_trust={settings['defaultProjectTrust']}")


if __name__ == "__main__":
    sh.run("pi", provision)
