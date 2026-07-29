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

This script's job:

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
  5. Resolve the requested model (the SCION_MODEL env var), mapping size
     aliases (small/medium/large/extra-large) through config.yaml's
     model_aliases, and publish it as ANTHROPIC_MODEL in the env overlay.
  6. Write outputs/resolved-auth.json describing the chosen method.
  7. Write outputs/env.json with env vars to project into the harness process
     (e.g. ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_MODEL, or
     Vertex AI vars).

The script is intentionally stdlib-only so it works on any container image
that ships python3 (declared in config.yaml's required_image_tools).
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness

assert scion_harness.INTERFACE_VERSION >= 2, (
    "scion_harness.py INTERFACE_VERSION is too old "
    f"(got {scion_harness.INTERFACE_VERSION}, need >= 2); "
    "this is a staging bug — the host should have staged a compatible library"
)

CLAUDE_JSON_FILE = "~/.claude.json"
CLAUDE_AUTH_FILE = "~/.claude/.credentials.json"

# Model used when the agent config does not request one. This preserves the
# historical default that used to live in config.yaml's env block
# (ANTHROPIC_MODEL: "opus"); it now lives here because a static env entry in
# config.yaml lands in the container environment, and the container
# environment always wins over the provisioner's env overlay — which made the
# requested model impossible to apply.
DEFAULT_MODEL = "opus"

# The canonical size aliases, mirroring config.KnownModelAliases
# (pkg/config/templates.go). Only these four resolve through config.yaml's
# model_aliases map; anything else is treated as a concrete model name.
KNOWN_MODEL_ALIASES = frozenset({"small", "medium", "large", "extra-large"})

# Shorthand spellings for those aliases, mirroring config.NormalizeModelAlias
# (pkg/config/templates.go). Deliberately no extra spellings: a form accepted
# here but not there would make ANTHROPIC_MODEL disagree with the --model flag
# the Go side computes from the same request.
MODEL_ALIAS_SHORTHAND = {
    "s": "small",
    "m": "medium",
    "l": "large",
    "xl": "extra-large",
}

AUTH = scion_harness.AuthSpec(
    harness="claude",
    methods=[
        scion_harness.env_method("api-key", any_of=["ANTHROPIC_API_KEY"]),
        scion_harness.env_method("oauth-token", any_of=["CLAUDE_CODE_OAUTH_TOKEN"],
                                 hint="set CLAUDE_CODE_OAUTH_TOKEN (generate with `claude setup-token`)"),
        scion_harness.file_method("auth-file", path=CLAUDE_AUTH_FILE, secret_key="CLAUDE_AUTH"),
        scion_harness.env_method("vertex-ai",
                                 all_of=["GOOGLE_CLOUD_PROJECT"],
                                 any_of=["GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"],
                                 hint="provide GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_LOCATION/GOOGLE_CLOUD_REGION "
                                      "(with ADC or GCP service account) for Vertex AI"),
    ],
)

CLAUDE_MCP_MAPPING = {
    "transport_field": "type",
    "transport_map": {
        "stdio": "stdio",
        "sse": "sse",
        "streamable-http": "streamable-http",
    },
    "global_config_file": scion_harness.expand_path(CLAUDE_JSON_FILE),
    "global_config_path": "mcpServers",
    "project_config_file": scion_harness.expand_path(CLAUDE_JSON_FILE),
    "project_config_path": "projects.{workspace}.mcpServers",
}


def _resolve(ctx: scion_harness.ProvisionContext, name: str) -> str:
    """Read a secret value, falling back to a ${VAR} placeholder."""
    val = ctx.read_secret(name)
    return val if val else "${" + name + "}"


def _apply_api_key_approval(ctx: scion_harness.ProvisionContext, api_key: str) -> None:
    """Pre-approve the API key in .claude.json.

    Writes the last 20 characters of the key as a fingerprint in
    customApiKeyResponses.approved so Claude Code does not prompt for
    confirmation. Mirrors ClaudeCode.ApplyAuthSettings in claude_code.go.
    """
    if not api_key:
        return

    fingerprint = api_key[-20:] if len(api_key) > 20 else api_key
    claude_json_path = scion_harness.expand_path(CLAUDE_JSON_FILE)

    cfg: dict[str, Any] = {}
    if os.path.isfile(claude_json_path):
        try:
            cfg = scion_harness.load_json(claude_json_path) or {}
        except (OSError, json.JSONDecodeError):
            cfg = {}
    if not isinstance(cfg, dict):
        cfg = {}

    cfg["customApiKeyResponses"] = {
        "approved": [fingerprint],
        "rejected": [],
    }

    scion_harness.atomic_write_json(claude_json_path, cfg)


def _detect_claude_version() -> str:
    """Detect the installed Claude Code version by running ``claude --version``."""
    try:
        result = subprocess.run(
            ["claude", "--version"],
            capture_output=True, text=True, timeout=10,
        )
        if result.returncode == 0 and result.stdout.strip():
            raw_version = result.stdout.strip().split()[0]
            # Extract clean semver from formats like '@anthropic-ai/claude-code/0.2.9' or 'v0.2.9'
            version = raw_version.split("/")[-1].lstrip("v")
            return version
    except (OSError, subprocess.TimeoutExpired):
        pass
    return ""


def _update_project_paths(ctx: scion_harness.ProvisionContext) -> None:
    """Update .claude.json project paths to point at the container workspace.

    Mirrors ClaudeCode.provisionClaudeJSON in claude_code.go. Takes the first
    existing project entry's settings and re-keys it to the container workspace
    path. If no project entries exist, creates a default settings map.

    Also pre-trusts /workspace so the trust dialog does not fire when
    Claude Code is launched there for git-clone-per-agent agents whose
    ctx.workspace is a subdirectory of /workspace.
    """
    claude_json_path = scion_harness.expand_path(CLAUDE_JSON_FILE)

    cfg: dict[str, Any] = {}
    if os.path.isfile(claude_json_path):
        try:
            cfg = scion_harness.load_json(claude_json_path) or {}
        except (OSError, json.JSONDecodeError):
            cfg = {}
    if not isinstance(cfg, dict):
        cfg = {}

    projects = cfg.get("projects")
    if not isinstance(projects, dict):
        projects = {}

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

    new_projects: dict[str, Any] = {ctx.workspace: project_settings}

    workspace_root = "/workspace"
    if ctx.workspace != workspace_root:
        new_projects[workspace_root] = {
            "hasTrustDialogAccepted": True,
            "projectOnboardingSeenCount": 1,
        }

    cfg["projects"] = new_projects

    version = _detect_claude_version()
    if version:
        cfg["lastReleaseNotesSeen"] = version
        cfg["lastOnboardingVersion"] = version

    scion_harness.atomic_write_json(claude_json_path, cfg)


def _normalize_model_alias(model: str) -> str:
    """Lower-case a model string and expand the single-letter shorthands.

    Mirrors config.NormalizeModelAlias (pkg/config/templates.go) exactly so the
    model this script derives cannot disagree with the one the Go side derives
    for --model / SCION_MODEL. Keep the two in sync.
    """
    normalized = model.strip().lower()
    return MODEL_ALIAS_SHORTHAND.get(normalized, normalized)


def _resolve_model_alias(ctx: scion_harness.ProvisionContext, raw_model: str) -> str:
    """Resolve a size alias (e.g. 'medium') to a concrete Claude model name.

    Mirrors config.ResolveModelAlias (pkg/config/templates.go): only the four
    canonical tiers are treated as aliases, and the concrete name they map to
    comes from config.yaml's model_aliases block so operators can re-point the
    tiers without touching this script. Anything else (e.g. 'sonnet' or
    'claude-sonnet-4-5') is a concrete model name and passes through.
    """
    if not raw_model:
        return ""

    normalized = _normalize_model_alias(raw_model)
    if normalized not in KNOWN_MODEL_ALIASES:
        return normalized

    aliases = ctx.harness_config.get("model_aliases") or {}
    if not isinstance(aliases, dict):
        return normalized

    concrete = aliases.get(normalized)
    if not concrete:
        return normalized

    if str(concrete) != raw_model:
        ctx.info(f"resolved model alias {raw_model!r} → {concrete!r}")
    return str(concrete)


def _apply_model(ctx: scion_harness.ProvisionContext, env: dict[str, str]) -> str:
    """Resolve the requested model and publish it as ANTHROPIC_MODEL.

    The request arrives as the SCION_MODEL env var, which Scion injects into
    every agent container from the agent's applied config. (The manifest has no
    model_resolution field — see changelog 2026-07-20 — so SCION_MODEL is the
    only live channel, and it can still carry a raw tier name: an agent started
    with `--model medium` reaches the container as SCION_MODEL=medium.)

    Where this sits in Claude Code's precedence chain
    (--model > ANTHROPIC_MODEL > settings.json `model`): when the agent has a
    model configured, pkg/agent/run.go also prepends `--model <resolved>` to
    the CLI args, and that outranks ANTHROPIC_MODEL. ANTHROPIC_MODEL is what
    covers the rest — the default when nothing is requested, subprocesses the
    harness spawns, and any consumer reading the env var directly — and this
    keeps it agreeing with the flag instead of contradicting it.

    Returns the concrete model name that was applied.
    """
    requested = os.environ.get("SCION_MODEL", "").strip()
    model = _resolve_model_alias(ctx, requested) or DEFAULT_MODEL

    preset = os.environ.get("ANTHROPIC_MODEL", "").strip()
    if requested and preset and preset != model:
        ctx.warn(
            f"ANTHROPIC_MODEL={preset!r} is set in the container environment and "
            f"takes precedence over the requested model {model!r}. This usually "
            "comes from an `env:` entry in your template or harness-config; "
            "remove it there to let the agent's model setting apply."
        )

    env["ANTHROPIC_MODEL"] = model
    return model


def _build_env_overlay(ctx: scion_harness.ProvisionContext, auth: scion_harness.ResolvedAuth) -> dict[str, str]:
    """Build the env vars overlay for outputs/env.json."""
    if auth.method == "api-key" and auth.env_key:
        return {auth.env_key: _resolve(ctx, auth.env_key)}
    if auth.method == "oauth-token":
        return {"CLAUDE_CODE_OAUTH_TOKEN": _resolve(ctx, "CLAUDE_CODE_OAUTH_TOKEN")}
    if auth.method == "vertex-ai":
        region_key = auth.env_key or "GOOGLE_CLOUD_REGION"
        return {
            "CLAUDE_CODE_USE_VERTEX": "1",
            "ANTHROPIC_VERTEX_PROJECT_ID": _resolve(ctx, "GOOGLE_CLOUD_PROJECT"),
            "CLOUD_ML_REGION": _resolve(ctx, region_key),
        }
    return {}


def provision(ctx: scion_harness.ProvisionContext) -> None:
    auth = ctx.select_auth(AUTH)

    try:
        _update_project_paths(ctx)
    except OSError as exc:
        raise scion_harness.ProvisionError(f"failed to update project paths: {exc}") from exc

    if auth.method == "api-key" and auth.env_key:
        api_key_value = ctx.read_secret(auth.env_key)
        if api_key_value:
            try:
                _apply_api_key_approval(ctx, api_key_value)
            except OSError as exc:
                ctx.warn(f"failed to write API key approval: {exc}")

    env = _build_env_overlay(ctx, auth)
    model = _apply_model(ctx, env)
    extra: dict[str, Any] | None = None
    if auth.method == "vertex-ai":
        extra = {"vertex_ai": True}
    ctx.write_outputs(auth, env=env, extra=extra)
    ctx.info(f"method={auth.method} model={model}")

    mcp_mapping = dict(CLAUDE_MCP_MAPPING)
    mcp_mapping["project_config_path"] = f"projects.{ctx.workspace}.mcpServers"
    try:
        count = scion_harness.apply_mcp_servers_simple(ctx.bundle_dir, mcp_mapping, ctx.workspace)
    except (OSError, ValueError) as exc:
        ctx.warn(f"mcp merge failed: {exc}")
        count = 0
    if count > 0:
        ctx.info(f"applied {count} mcp server(s)")

    harness_cfg = ctx.harness_config
    instructions_file = str(harness_cfg.get("instructions_file") or ".claude/CLAUDE.md")
    scion_harness.project_instructions(
        ctx,
        instructions_file,
        system_prompt_mode="none",
    )


if __name__ == "__main__":
    scion_harness.run("claude", provision)
