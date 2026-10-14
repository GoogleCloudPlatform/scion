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
  5. Publish the resolved model (the SCION_MODEL env var, already resolved
     by the Go side) as ANTHROPIC_MODEL in the env overlay.
  6. Map SCION_THINKING_LEVEL through config.yaml's `thinking:` block and
     publish the result as CLAUDE_CODE_EFFORT_LEVEL in the env overlay. With
     no level set nothing is published, so Claude keeps its per-model default.
  7. Write outputs/resolved-auth.json describing the chosen method.
  8. Write outputs/env.json with env vars to project into the harness process
     (e.g. ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_MODEL,
     CLAUDE_CODE_EFFORT_LEVEL, or Vertex AI vars).

The script is intentionally stdlib-only so it works on any container image
that ships python3 (declared in config.yaml's required_image_tools).

Design references below (section N, Dn) are to
.design/hosted/usage-telemetry.md (ptone/scion#2053).
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness

assert scion_harness.INTERFACE_VERSION >= 3, (  # 3: resolve_thinking
    "scion_harness.py INTERFACE_VERSION is too old "
    f"(got {scion_harness.INTERFACE_VERSION}, need >= 3); "
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

# Claude Code < 2.1.280 rejects claude-opus-5-5* (and the "opus" alias, which
# maps to Opus 5 in 2.1.270) with HTTP 400 `claude_code_version_too_old`.
# When the container's `claude` binary is older than this threshold, fall back
# to claude-opus-4-8 and emit a clear warning so agents do not boot into a
# non-recoverable 400 loop.
OPUS_5_5_MIN_CLAUDE_VERSION = (2, 1, 280)
OPUS_5_5_FALLBACK_MODEL = "claude-opus-4-8"

# Claude Code reads the session effort from this env var. It outranks the
# --effort flag, /effort, and the effortLevel / modelSettings keys in
# settings.json (https://code.claude.com/docs/en/model-config, "Precedence
# Order"). Accepted values per https://code.claude.com/docs/en/env-vars; a
# level the active model does not support falls back to the highest supported
# level below it. The bundled table tops out at xhigh rather than max to avoid
# max's excessive-token runs; max stays reachable via a preset env var or a
# custom thinking table.
EFFORT_ENV = "CLAUDE_CODE_EFFORT_LEVEL"
EFFORT_ENV_VALUES = frozenset({"low", "medium", "high", "xhigh", "max", "auto"})

AUTH = scion_harness.AuthSpec(
    harness="claude",
    methods=[
        scion_harness.env_method("api-key", any_of=["ANTHROPIC_API_KEY"]),
        scion_harness.env_method("oauth-token", any_of=["CLAUDE_CODE_OAUTH_TOKEN"],
                                 hint="set CLAUDE_CODE_OAUTH_TOKEN (generate with `claude setup-token`)"),
        scion_harness.file_method("auth-file", path=CLAUDE_AUTH_FILE, secret_key="CLAUDE_AUTH"),
        scion_harness.env_method("vertex-ai",
                                 all_of=["GOOGLE_CLOUD_PROJECT"],
                                 # Shared precedence: LOCATION > REGION > CLOUD_ML_REGION.
                                 # select_auth picks the first present key as env_key.
                                 any_of=list(scion_harness.vertex_location_keys()),
                                 hint="provide GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_LOCATION/GOOGLE_CLOUD_REGION/CLOUD_ML_REGION "
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


# Moved to the shared helper (design 2.1); the alias keeps existing callers
# and tests working.
_parse_semver = scion_harness.parse_semver


def _requires_opus_5_5_min_version(model: str) -> bool:
    """Return True if *model* requires Claude Code >= 2.1.280."""
    lowered = model.strip().lower()
    return lowered == "opus" or lowered.startswith("claude-opus-5-5")


def _update_project_paths(
    ctx: scion_harness.ProvisionContext,
    claude_version: str | None = None,
) -> str:
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

    version = _detect_claude_version() if claude_version is None else claude_version
    if version:
        cfg["lastReleaseNotesSeen"] = version
        cfg["lastOnboardingVersion"] = version

    scion_harness.atomic_write_json(claude_json_path, cfg)
    return version


def _apply_model(
    ctx: scion_harness.ProvisionContext,
    env: dict[str, str],
    claude_version: str = "",
) -> str:
    """Read the resolved model from SCION_MODEL and publish as ANTHROPIC_MODEL.

    SCION_MODEL is expected to arrive already resolved by the Go side
    (pkg/agent/provision.go and pkg/hub/handlers_agent_create_helpers.go
    resolve size aliases before the container starts). This function applies
    it as ANTHROPIC_MODEL and handles the edge case where ANTHROPIC_MODEL is
    already set in the environment.

    Defense in depth: if SCION_MODEL still carries a bare size alias (e.g.
    "large") — which has happened on resume/restart paths where the Go side
    had no alias table to resolve against — scion_harness.resolve_model maps
    it using this harness's own config.yaml rather than exporting the alias
    verbatim.

    When *claude_version* is older than 2.1.280 and the resolved model is
    ``opus`` or ``claude-opus-5-5*``, falls back to ``claude-opus-4-8`` with a
    warning so agents do not boot into a 400 ``claude_code_version_too_old``
    failure loop.

    Returns the concrete model name that was applied.
    """
    raw = os.environ.get("SCION_MODEL", "").strip()
    model = scion_harness.resolve_model(ctx) or DEFAULT_MODEL

    parsed_version = _parse_semver(claude_version)
    if parsed_version is not None and parsed_version < OPUS_5_5_MIN_CLAUDE_VERSION:
        if _requires_opus_5_5_min_version(model):
            ctx.warn(
                f"Claude Code {claude_version} in container is older than "
                f"2.1.280 and rejects model {model!r} with 400 "
                f"claude_code_version_too_old; falling back to "
                f"{OPUS_5_5_FALLBACK_MODEL!r}. Rebuild the scion-claude "
                "container image with Claude Code >= 2.1.280 to use "
                "claude-opus-5-5."
            )
            model = OPUS_5_5_FALLBACK_MODEL

    preset = os.environ.get("ANTHROPIC_MODEL", "").strip()
    if raw and preset and preset != model:
        ctx.warn(
            f"ANTHROPIC_MODEL={preset!r} is set in the container environment and "
            f"takes precedence over the requested model {model!r}. This usually "
            "comes from an `env:` entry in your template or harness-config; "
            "remove it there to let the agent's model setting apply."
        )

    env["ANTHROPIC_MODEL"] = model
    return model


def _apply_effort(ctx: scion_harness.ProvisionContext, env: dict[str, str]) -> str | None:
    """Resolve the thinking level and publish it as CLAUDE_CODE_EFFORT_LEVEL.

    The level -> effort table lives in config.yaml's `thinking:` block, which
    has no `default` on purpose: with no level set, nothing is published and
    Claude Code keeps its own per-model default effort.

    A CLAUDE_CODE_EFFORT_LEVEL already in the container environment (from a
    template or harness-config `env:` entry) is an explicit pin. The
    supervisor's MergeEnvOverlay drops any overlay key that is present in the
    runtime env, even with an empty value, so presence (not truthiness) is
    what counts: the pin is left alone and nothing is written.

    Returns the effort value written to *env*, or None when none was written.
    """
    requested = bool(os.environ.get(scion_harness.THINKING_LEVEL_ENV, "").strip())
    harness_cfg = ctx.harness_config if isinstance(ctx.harness_config, dict) else {}
    if requested and not harness_cfg.get("thinking"):
        # Only when a level was asked for: a customized config.yaml without
        # the block silently drops the request.
        ctx.warn(
            f"config.yaml has no thinking block; {EFFORT_ENV} not set "
            "(copy the block from the bundled config.yaml or run "
            "`scion harness-config upgrade claude`)"
        )

    if EFFORT_ENV in os.environ:
        preset = os.environ[EFFORT_ENV]
        if requested:
            ctx.warn(
                f"{EFFORT_ENV}={preset!r} is set in the container environment and "
                "takes precedence over the requested thinking level. This usually "
                "comes from an `env:` entry in your template or harness-config; "
                "remove it there to let the agent's thinking level apply."
            )
        return None

    effort = scion_harness.resolve_thinking(ctx)
    if not effort:
        return None
    if effort not in EFFORT_ENV_VALUES:
        ctx.warn(
            f"thinking map value {effort!r} is not a documented {EFFORT_ENV} "
            f"value ({', '.join(sorted(EFFORT_ENV_VALUES))}); Claude Code may ignore it"
        )
    env[EFFORT_ENV] = effort
    return effort


def _build_env_overlay(ctx: scion_harness.ProvisionContext, auth: scion_harness.ResolvedAuth) -> dict[str, str]:
    """Build the env vars overlay for outputs/env.json."""
    if auth.method == "api-key" and auth.env_key:
        env = {auth.env_key: _resolve(ctx, auth.env_key)}
    elif auth.method == "oauth-token":
        env = {"CLAUDE_CODE_OAUTH_TOKEN": _resolve(ctx, "CLAUDE_CODE_OAUTH_TOKEN")}
    elif auth.method == "vertex-ai":
        region_key = auth.env_key or "GOOGLE_CLOUD_REGION"
        # Read the sources selection and _resolve use: a staged secret file,
        # else the process env for candidate keys (Claude then reads the
        # env value through the ${VAR} placeholder).
        scion_harness.warn_vertex_location_conflict(
            lambda k: ctx.read_secret(k, env_fallback=k in ctx.env_keys),
            ctx.warn,
        )
        env = {
            "CLAUDE_CODE_USE_VERTEX": "1",
            "ANTHROPIC_VERTEX_PROJECT_ID": _resolve(ctx, "GOOGLE_CLOUD_PROJECT"),
            "CLOUD_ML_REGION": _resolve(ctx, region_key),
        }
    else:
        env = {}

    # Suppress model upgrade/fallback dialogs — Scion manages the provider.
    env["CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"] = "1"
    # Prevent non-root container process from failing background npm self-updates.
    env["DISABLE_AUTOUPDATER"] = "1"

    return env


# --- Agent Plugins (hub.agent_plugins) ---------------------------------------
#
# Route (design 2.5, 3): a pure file write. The staged bundle is copied into
# Claude's skills dir as ~/.claude/skills/<name>/ with a generated
# .claude-plugin/plugin.json beside the unchanged bundle files; Claude
# auto-loads it as <name>@skills-dir. MCP servers are inlined in that manifest,
# translated from the Go-validated plugins.json mcp.servers entries.

CLAUDE_PLUGINS_MIN_VERSION = (2, 1, 157)  # approximate skills-dir plugin floor (L6)
CLAUDE_PLUGIN_MANIFEST_DIR = ".claude-plugin"
AGENT_PLUGIN_MANIFEST = "plugin.json"

# Literal text Claude expands to the plugin's root directory.
CLAUDE_PLUGIN_ROOT_VAR = "${CLAUDE_PLUGIN_ROOT}"

# The stdio cwd wrapper (design 3.2). cwd, command and args are positional
# parameters: the shell never re-parses plugin strings.
STDIO_CWD_WRAPPER = 'cd -- "$1" || exit 127; shift; exec "$@"'

_PLUGIN_PLACEHOLDER_RE = re.compile(r"\$\{PLUGIN_(ROOT|DATA)\}")

# Paths Claude loads by default from a plugin root (claude 2.1.296: probed
# with `claude plugin details`, and read from the binary's plugin loader).
# An Agent Plugins bundle gets no Claude component beyond its skills/ and the
# Go-validated MCP servers in the generated manifest (design 3.1), so these
# are never installed, and a bundle that ships one gets a warning:
#   .mcp.json         MCP servers, loaded in addition to manifest mcpServers
#   .lsp.json         LSP server processes
#   hooks/            hooks/hooks.json: commands run on Claude events
#   commands/         slash commands (loaded as skills)
#   agents/           subagents
#   monitors/         monitors/monitors.json: unsandboxed background scripts
#   output-styles/    output styles (change the system prompt)
#   workflows/        plugin workflows
#   themes/           themes
#   settings.json     plugin settings
#   .claude-plugin/   replaced by the generated manifest
# Kept: skills/ (the spec component), plugin.json and mcp.json (Claude
# ignores both), and every other file, which MCP servers may need. bin/ is
# kept too: stdio commands live there (./bin/x), and Claude appends a
# plugin's bin/ to the PATH of its Bash tool (append only, so it cannot
# shadow existing commands).
CLAUDE_STRIPPED_PATHS = (
    ".mcp.json", ".lsp.json", "hooks", "commands", "agents", "monitors",
    "output-styles", "workflows", "themes", "settings.json",
)


class McpServerSkipped(Exception):
    """A server that cannot be expressed faithfully in Claude's config."""


def _has_foreign_placeholder(value: str, allow_plugin_tokens: bool) -> bool:
    """True when value holds `${` that Claude would expand but the spec
    requires to stay literal. With allow_plugin_tokens, exact
    ${PLUGIN_ROOT}/${PLUGIN_DATA} tokens are fine (they are translated)."""
    if allow_plugin_tokens:
        value = _PLUGIN_PLACEHOLDER_RE.sub("", value)
    return "${" in value


def _expand_plugin_placeholders(value: str, root: str, data: str) -> str:
    """Single-pass, non-recursive expansion (spec 9.2): replacement text is
    never rescanned."""
    return _PLUGIN_PLACEHOLDER_RE.sub(lambda m: root if m.group(1) == "ROOT" else data, value)


def _translate_cwd(cwd: str | None, root: str, data: str) -> str:
    if not cwd:
        return root  # spec 7.2.1: the plugin root
    if cwd.startswith("./"):
        rest = cwd[2:].rstrip("/")
        return f"{root}/{rest}" if rest else root
    return _expand_plugin_placeholders(cwd, root, data)


def _stripped_ref(path: str | None, args_only: bool = False) -> str | None:
    """The stripped top-level entry a plugin-relative path ("./x/...",
    "${PLUGIN_ROOT}/x/...") points into, or None. For an args element only
    the ${PLUGIN_ROOT}/ form is plugin-relative ("./" is relative to cwd)."""
    if not path:
        return None
    for prefix in (("${PLUGIN_ROOT}/",) if args_only else ("./", "${PLUGIN_ROOT}/")):
        if path.startswith(prefix):
            first = os.path.normpath(path[len(prefix):]).split("/", 1)[0]
            if first in CLAUDE_STRIPPED_PATHS or first == CLAUDE_PLUGIN_MANIFEST_DIR:
                return first
    return None


def translate_mcp_server(server: dict[str, Any], data_dir: str) -> dict[str, Any]:
    """Translate one Go-validated mcp.json server into a Claude mcpServers
    entry (design 3.2). Raises McpServerSkipped when the server holds
    placeholder-like text Claude would expand, or an unsupported type."""
    root = CLAUDE_PLUGIN_ROOT_VAR
    typ = server.get("type")
    if typ == "stdio":
        command = server.get("command")
        if not isinstance(command, str) or not command:
            raise McpServerSkipped("missing command")
        args = [a for a in (server.get("args") or []) if isinstance(a, str)]
        env = {k: v for k, v in (server.get("env") or {}).items() if isinstance(k, str) and isinstance(v, str)}
        cwd = server.get("cwd") if isinstance(server.get("cwd"), str) else None
        # The spec does not expand command (or anything outside args, env
        # and cwd), so any `${` there must stay literal; Claude would
        # expand it.
        if (
            _has_foreign_placeholder(command, False)
            or any(_has_foreign_placeholder(a, True) for a in args)
            or any(_has_foreign_placeholder(v, True) for v in env.values())
            or (cwd is not None and _has_foreign_placeholder(cwd, True))
        ):
            raise McpServerSkipped("placeholder")
        stripped = (
            _stripped_ref(command)
            or (_stripped_ref(cwd) if cwd else None)
            or next((r for r in (_stripped_ref(a, args_only=True) for a in args) if r), None)
        )
        if stripped:
            raise McpServerSkipped(f"it uses {stripped}, which is not installed for claude")
        if command.startswith("./"):
            command = f"{root}/{command[2:]}"
        out_env = {k: _expand_plugin_placeholders(v, root, data_dir) for k, v in env.items()}
        # Set last (spec 9.1), replacing any same-name entry.
        out_env.pop("PLUGIN_ROOT", None)
        out_env.pop("PLUGIN_DATA", None)
        out_env["PLUGIN_ROOT"] = root
        out_env["PLUGIN_DATA"] = data_dir
        return {
            "type": "stdio",
            "command": "/bin/sh",
            "args": [
                "-c", STDIO_CWD_WRAPPER, "sh",
                _translate_cwd(cwd, root, data_dir),
                command,
                *[_expand_plugin_placeholders(a, root, data_dir) for a in args],
            ],
            "env": out_env,
        }
    if typ in ("streamable-http", "sse"):
        url = server.get("url")
        if not isinstance(url, str) or not url:
            raise McpServerSkipped("missing url")
        headers = {k: v for k, v in (server.get("headers") or {}).items() if isinstance(k, str) and isinstance(v, str)}
        # The spec forbids any expansion in url and headers.
        if _has_foreign_placeholder(url, False) or any(
            _has_foreign_placeholder(v, False) or _has_foreign_placeholder(k, False) for k, v in headers.items()
        ):
            raise McpServerSkipped("placeholder")
        out: dict[str, Any] = {"type": "http" if typ == "streamable-http" else "sse", "url": url}
        if headers:
            out["headers"] = headers
        return out
    raise McpServerSkipped(f"unsupported type {typ!r}")


def translate_mcp_servers(
    ctx: scion_harness.ProvisionContext,
    plugin: scion_harness.AgentPlugin,
    data_dir: str,
) -> dict[str, dict[str, Any]]:
    """Claude mcpServers for plugin, from the Go-validated plugins.json
    mcp.servers only. Skipped servers are warned about; the rest load."""
    out: dict[str, dict[str, Any]] = {}
    for name in sorted(plugin.mcp_servers or {}):
        try:
            out[name] = translate_mcp_server(plugin.mcp_servers[name], data_dir)
        except McpServerSkipped as exc:
            reason = (
                "value contains placeholder-like text that Claude would expand"
                if str(exc) == "placeholder" else str(exc)
            )
            ctx.warn(f"agent plugin {plugin.name}: mcp server {name} skipped for claude: {reason}")
    return out


def _warn_invalid_mcp(ctx: scion_harness.ProvisionContext, plugin: scion_harness.AgentPlugin) -> None:
    """Re-warn the servers Go skipped (plugins.json mcp.invalid)."""
    mcp = plugin.raw.get("mcp")
    invalid = mcp.get("invalid") if isinstance(mcp, dict) else None
    for diag in invalid if isinstance(invalid, list) else []:
        msg = diag.get("message") if isinstance(diag, dict) else None
        if isinstance(msg, str) and msg:
            ctx.warn(f"agent plugin {plugin.name}: {msg}")


def claude_plugin_name(name: str) -> str:
    """The name written to the generated manifest. Spec names are used
    as-is: Claude 2.1.296 accepts dotted names (smoke L2)."""
    return name


def build_claude_manifest(
    ctx: scion_harness.ProvisionContext,
    plugin: scion_harness.AgentPlugin,
    data_dir: str,
) -> dict[str, Any]:
    """The generated .claude-plugin/plugin.json (design 3.1)."""
    manifest: dict[str, Any] = {"name": claude_plugin_name(plugin.name)}
    version = plugin.raw.get("version")
    if isinstance(version, str) and version:
        manifest["version"] = version
    description = plugin.raw.get("description")
    if isinstance(description, str) and description:
        manifest["description"] = description
    servers = translate_mcp_servers(ctx, plugin, data_dir)
    if servers:
        manifest["mcpServers"] = servers
    return manifest


def _remove_path(path: str) -> None:
    """Remove path without following a symlink."""
    if os.path.islink(path) or (os.path.lexists(path) and not os.path.isdir(path)):
        os.unlink(path)
    elif os.path.isdir(path):
        shutil.rmtree(path)


class ClaudePluginInstaller(scion_harness.PluginInstaller):
    """Installs Agent Plugins as Claude skills-dir plugins (design 2.5).

    Ownership: a directory in the skills dir is Scion's only when the
    plugin is recorded in ~/.scion/agent-plugins/state-claude.json. Any
    other directory (a template, platform, bank or user skill, or a user's
    own plugin) is never replaced or removed.
    """

    harness = "claude"
    cli = "claude"
    min_version = CLAUDE_PLUGINS_MIN_VERSION

    def __init__(self, ctx: scion_harness.ProvisionContext, claude_version: str | None = None):
        self.ctx = ctx
        skills_rel = str(ctx.harness_config.get("skills_dir") or ".claude/skills")
        self.skills_dir = os.path.join(ctx.home, skills_rel)
        self.data_root = os.path.join(scion_harness.agent_plugins_dir(), "data")
        # provision() already ran `claude --version`; reuse it.
        self._claude_version = claude_version
        try:
            desired = scion_harness.read_agent_plugins(ctx)
        except ValueError:
            desired = []  # reconcile reports it and changes nothing
        self.conflicts = {p.name for p in desired if p.skills_dir_conflict}
        self.owned = set(self._recorded_plugins())

    def _state_path(self) -> str:
        return os.path.join(scion_harness.agent_plugins_dir(), f"state-{self.harness}.json")

    def _recorded_plugins(self) -> dict[str, Any]:
        """The plugins recorded in the state file; {} when absent or
        unreadable (reconcile reports an unreadable file)."""
        path = self._state_path()
        try:
            if os.path.islink(path):
                return {}
            doc = scion_harness.load_json(path)
        except (OSError, ValueError):
            return {}
        plugins = doc.get("plugins") if isinstance(doc, dict) else None
        return plugins if isinstance(plugins, dict) else {}

    def target(self, name: str) -> str:
        return os.path.join(self.skills_dir, name)

    def data_dir(self, name: str) -> str:
        return os.path.join(self.data_root, name)

    def _has_generated_manifest(self, path: str) -> bool:
        return (
            os.path.isdir(path) and not os.path.islink(path)
            and os.path.isfile(os.path.join(path, AGENT_PLUGIN_MANIFEST))
            and not os.path.islink(os.path.join(path, AGENT_PLUGIN_MANIFEST))
            and os.path.isfile(os.path.join(path, CLAUDE_PLUGIN_MANIFEST_DIR, AGENT_PLUGIN_MANIFEST))
            and not os.path.islink(os.path.join(path, CLAUDE_PLUGIN_MANIFEST_DIR))
        )

    def release_stale(self) -> None:
        """Stale ownership (design 2.5): a plugin Scion installed now shares
        its name with a skill (a template added one, and Go merged it into
        the plugin's directory on re-provision). Remove the generated
        .claude-plugin/ so Claude treats the directory as that skill again
        (and the root plugin.json when it is the bundle's own, so the Go
        skills-dir skips do not hide the skill), drop the plugin from the
        state file, and warn. Runs before
        reconcile_agent_plugins, which then reports the conflict."""
        recorded = self._recorded_plugins()
        self._sweep_leftovers(recorded)
        stale = sorted(n for n in self.conflicts if n in recorded)
        if not stale:
            return
        for name in stale:
            rec = recorded.get(name)
            ref = rec.get("native_ref") if isinstance(rec, dict) else None
            try:
                self._release_dir(name, ref if isinstance(ref, dict) else {})
            except OSError as exc:
                self.ctx.warn(f"agent plugin {name}: cannot release {self.target(name)}: {exc}")
                continue
            recorded.pop(name, None)
            self.owned.discard(name)
            self.ctx.warn(
                f"agent plugin {name}: a skill named {name} now shares its directory; "
                "removed the generated .claude-plugin manifest, so claude loads it as that skill"
            )
        scion_harness.atomic_write_json(self._state_path(), {
            "schema_version": scion_harness.AGENT_PLUGINS_STATE_SCHEMA_VERSION,
            "harness": self.harness,
            "plugins": recorded,
        })

    def _sweep_leftovers(self, recorded: dict[str, Any]) -> None:
        """Remove .<n>.tmp / .<n>.old left by an interrupted install of an
        owned plugin. Runs on every start, so the fast path cleans up too
        (claude skips hidden skills-dir entries, so they never load)."""
        for name in sorted(set(recorded) | self.owned):
            if not scion_harness.valid_agent_plugin_name(name):
                continue
            for leftover in (f".{name}.tmp", f".{name}.old"):
                path = os.path.join(self.skills_dir, leftover)
                if os.path.lexists(path):
                    try:
                        _remove_path(path)
                    except OSError as exc:
                        self.ctx.warn(f"agent plugin {name}: cannot remove leftover {path}: {exc}")

    def _release_dir(self, name: str, native_ref: dict[str, Any]) -> None:
        """Hand a plugin dir that now also holds a skill back to that skill:
        remove the generated .claude-plugin/ and the bundle's own root
        plugin.json (so the Go skills-dir skips of design 2.7 do not hide
        the skill). Every other file stays (R6)."""
        target = self.target(name)
        if os.path.islink(target) or not os.path.isdir(target):
            return
        gen = os.path.join(target, CLAUDE_PLUGIN_MANIFEST_DIR)
        if os.path.lexists(gen):
            _remove_path(gen)
        stray = os.path.join(target, AGENT_PLUGIN_MANIFEST)
        if not os.path.islink(stray) and os.path.isfile(stray) and self._is_bundle_manifest(name, stray, native_ref):
            os.unlink(stray)

    def _is_bundle_manifest(self, name: str, path: str, native_ref: dict[str, Any]) -> bool:
        """Whether path is the root plugin.json Scion installed: its sha256
        matches the one recorded at install time. Records from before the
        sha was kept fall back to the staged bundle's bytes."""
        try:
            with open(path, "rb") as f:
                got = hashlib.sha256(f.read()).hexdigest()
        except OSError:
            return False
        want = native_ref.get("manifest_sha256")
        if isinstance(want, str) and want:
            return got == want
        staged = os.path.join(self.ctx.inputs_dir, "plugins", name, AGENT_PLUGIN_MANIFEST)
        try:
            if os.path.islink(staged):
                return False
            with open(staged, "rb") as f:
                return got == hashlib.sha256(f.read()).hexdigest()
        except OSError:
            return False

    def detect_version(self) -> tuple[int, int, int] | None:
        if self._claude_version is None:
            proc = scion_harness.run_cli(
                ["claude", "--version"],
                timeout=self.cli_timeout(scion_harness.AGENT_PLUGIN_VERSION_TIMEOUT),
            )
            self._claude_version = (proc.stdout or "") if proc.returncode == 0 else ""
        return scion_harness.parse_semver(self._claude_version)

    def install(self, plugin: scion_harness.AgentPlugin, bundle_dir: str) -> dict[str, Any]:
        name = plugin.name
        target = self.target(name)
        # The reason is the whole message: reconcile reports it once.
        if plugin.skills_dir_conflict:
            raise scion_harness.PluginInstallError(
                f"plugin {name} not installed for claude: a skill named {name} is already installed in the skills dir")
        if os.path.lexists(target) and name not in self.owned:
            raise scion_harness.PluginInstallError(
                f"plugin {name} not installed for claude: {target} exists and was not installed by Scion")
        if os.path.islink(target):
            raise scion_harness.PluginInstallError(f"{target} is a symlink")

        _warn_invalid_mcp(self.ctx, plugin)
        data_dir = self.data_dir(name)
        manifest = build_claude_manifest(self.ctx, plugin, data_dir)

        tmp = os.path.join(self.skills_dir, f".{name}.tmp")
        old = os.path.join(self.skills_dir, f".{name}.old")
        try:
            # PLUGIN_DATA (spec 9.1): created before launch, private to the
            # agent user, kept across updates and removals. Created first,
            # so nothing can fail after the new dir is in place.
            os.makedirs(data_dir, mode=0o700, exist_ok=True)
            os.chmod(data_dir, 0o700)
            os.makedirs(self.skills_dir, exist_ok=True)
            for leftover in (tmp, old):
                _remove_path(leftover)
            shutil.copytree(plugin.staged_dir, tmp, symlinks=True)
            for rel in (*CLAUDE_STRIPPED_PATHS, CLAUDE_PLUGIN_MANIFEST_DIR):
                path = os.path.join(tmp, rel)
                if os.path.lexists(path):
                    _remove_path(path)
                    self.ctx.warn(f"agent plugin {name}: ignored claude-specific {rel} in bundle")
            gen = os.path.join(tmp, CLAUDE_PLUGIN_MANIFEST_DIR)
            os.makedirs(gen)
            scion_harness.atomic_write_json(os.path.join(gen, AGENT_PLUGIN_MANIFEST), manifest)
            # Atomic swap: old aside, tmp into place, old removed.
            if os.path.lexists(target):
                os.rename(target, old)
            try:
                os.rename(tmp, target)
            except OSError:
                if os.path.lexists(old) and not os.path.lexists(target):
                    os.rename(old, target)
                raise
        except OSError as exc:
            try:
                _remove_path(tmp)
            except OSError:
                pass
            raise scion_harness.PluginInstallError(f"cannot write {target}: {exc}") from None
        # The new plugin is in place: a leftover old copy is only a warning
        # (it is hidden, and the next install removes it).
        try:
            _remove_path(old)
        except OSError as exc:
            self.ctx.warn(f"agent plugin {name}: cannot remove the previous copy {old}: {exc}")
        self.owned.add(name)
        ref: dict[str, Any] = {"path": target}
        try:
            with open(os.path.join(target, AGENT_PLUGIN_MANIFEST), "rb") as f:
                ref["manifest_sha256"] = hashlib.sha256(f.read()).hexdigest()
        except OSError:
            pass
        return ref

    def uninstall(self, name: str, native_ref: dict[str, Any]) -> None:
        """Remove the plugin dir when it still carries both the root
        plugin.json and the generated manifest. Anything else at that path
        is no longer a Scion install and is left alone. data/<name> is kept
        (spec 9.1 makes deletion optional). Success is judged by the
        result: the generated plugin must be gone."""
        target = self.target(name)
        skill_md = os.path.join(target, "SKILL.md")
        if self._has_generated_manifest(target) and os.path.isfile(skill_md) and not os.path.islink(skill_md):
            # A skill was merged into the plugin dir (a template added skill
            # <name> in the same edit that dropped the plugin). An Agent
            # Plugins bundle keeps its skills under skills/, so a root
            # SKILL.md is not ours: hand the dir to the skill, never rmtree.
            try:
                self._release_dir(name, native_ref)
            except OSError as exc:
                raise scion_harness.PluginInstallError(f"cannot release {target}: {exc}") from None
            self.ctx.warn(
                f"agent plugin {name}: removed; a skill named {name} shares its directory, so only the "
                "generated .claude-plugin manifest was removed and claude loads it as that skill"
            )
            self.owned.discard(name)
            return
        if self._has_generated_manifest(target):
            try:
                shutil.rmtree(target)
            except OSError as exc:
                raise scion_harness.PluginInstallError(f"cannot remove {target}: {exc}") from None
        if self._has_generated_manifest(target):
            raise scion_harness.PluginInstallError(f"{target} is still present")
        # A bundle dir that lost only its generated manifest (removed by
        # hand) stays Scion's for this run, so a reinstall at a new digest
        # replaces it, as one at the same digest does.
        root_manifest = os.path.join(target, AGENT_PLUGIN_MANIFEST)
        if not (os.path.isdir(target) and not os.path.islink(target)
                and os.path.isfile(root_manifest) and not os.path.islink(root_manifest)):
            self.owned.discard(name)

    def is_present(self, name: str, native_ref: dict[str, Any]) -> bool:
        """Claude's registration of a skills-dir plugin is the directory
        itself: root plugin.json plus the generated manifest at the current
        skills dir. The data dir must exist too (spec 9.1: created before
        launch). A plugin now in conflict with a skill is reported missing
        so install handles it."""
        if name in self.conflicts:
            return False
        path = native_ref.get("path")
        if isinstance(path, str) and path and os.path.normpath(path) != os.path.normpath(self.target(name)):
            return False
        return self._has_generated_manifest(self.target(name)) and os.path.isdir(self.data_dir(name))


def provision(ctx: scion_harness.ProvisionContext) -> None:
    auth = ctx.select_auth(AUTH)

    try:
        claude_version = _update_project_paths(ctx)
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
    telemetry = ctx.telemetry
    config = telemetry.get("telemetry") if isinstance(telemetry, dict) else None
    enabled = isinstance(config, dict) and config.get("enabled", True)
    source_env = telemetry.get("env", {}) if isinstance(telemetry, dict) else {}
    cloud = config.get("cloud") if isinstance(config, dict) else None
    configured_provider = cloud.get("provider", "") if isinstance(cloud, dict) else ""
    staged_provider = source_env.get("SCION_TELEMETRY_CLOUD_PROVIDER", "")
    if enabled and configured_provider and staged_provider and configured_provider != staged_provider:
        raise scion_harness.ProvisionError("conflicting telemetry cloud provider")
    provider = staged_provider or configured_provider
    if enabled and not provider:
        # The receiver can infer GCP from credentials even when provider is
        # absent. A credential path is only a reason to stop, not a selector.
        has_credentials = bool(source_env.get("SCION_OTEL_GCP_CREDENTIALS")) or os.path.isfile(
            os.path.join(ctx.home, ".scion", "telemetry-gcp-credentials.json")
        )
        generic_endpoint = (cloud.get("endpoint") if isinstance(cloud, dict) else None) or source_env.get("SCION_OTEL_ENDPOINT")
        if has_credentials or not generic_endpoint:
            raise scion_harness.ProvisionError("explicit telemetry cloud provider required")
    port = str(source_env.get("SCION_OTEL_GRPC_PORT") or "4317")
    if not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise scion_harness.ProvisionError("invalid local telemetry gRPC port")
    env.update({
        "SCION_NATIVE_TELEMETRY_POLICY": "enabled" if enabled else "disabled",
        "CLAUDE_CODE_ENABLE_TELEMETRY": "1" if enabled else "0",
        "OTEL_METRICS_EXPORTER": "otlp" if enabled and provider != "gcp" else "none",
        "OTEL_LOGS_EXPORTER": "otlp" if enabled else "none",
        "OTEL_TRACES_EXPORTER": "none",
        "OTEL_EXPORTER_OTLP_ENDPOINT": f"http://127.0.0.1:{port}",
        "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": f"http://127.0.0.1:{port}",
        "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": f"http://127.0.0.1:{port}",
        "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
    })
    if enabled:
        # Claude's usage (gen_ai.api.calls / scion.usage.tokens) is derived by
        # sciontool's receiver from the native api_request/api_error log
        # events (ptone/scion#2053 phase 1). This is narrow to usage only
        # (D4): tool, session and turn hook telemetry are unaffected, and
        # unset here means no usage is published at all (D10).
        env["SCION_USAGE_SOURCE"] = "native"
    model = _apply_model(ctx, env, claude_version=claude_version)
    effort = _apply_effort(ctx, env)
    extra: dict[str, Any] | None = None
    if auth.method == "vertex-ai":
        extra = {"vertex_ai": True}
    ctx.write_outputs(auth, env=env, extra=extra)
    ctx.info(f"method={auth.method} model={model} effort={effort or '<claude default>'}")

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

    # Last (design 2.1): install Agent Plugins into the skills dir.
    try:
        installer = ClaudePluginInstaller(ctx, claude_version=claude_version)
        installer.release_stale()
    except Exception as exc:  # noqa: BLE001 - never fail provisioning for plugins
        ctx.warn(f"agent plugins: reconcile failed: {exc}")
        return
    scion_harness.reconcile_agent_plugins(ctx, installer)


if __name__ == "__main__":
    scion_harness.run("claude", provision)
