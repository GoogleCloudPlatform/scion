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

import json
import os
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
# Order"). Unlike --effort it does not accept `max`; a level the active model
# does not support falls back to the highest supported level below it.
EFFORT_ENV = "CLAUDE_CODE_EFFORT_LEVEL"
EFFORT_ENV_VALUES = frozenset({"low", "medium", "high", "xhigh", "auto"})

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


def _parse_semver(version: str) -> tuple[int, int, int] | None:
    """Parse a (major, minor, patch) tuple from a version string like '2.1.270'."""
    if not version:
        return None
    core = version.strip().lstrip("v").split("-", 1)[0].split("+", 1)[0]
    parts = core.split(".")
    if len(parts) < 3:
        return None
    try:
        return (int(parts[0]), int(parts[1]), int(parts[2]))
    except ValueError:
        return None


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
    template or harness-config `env:` entry) is an explicit pin. It would win
    over the env overlay anyway, so it is left alone and nothing is written.

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

    preset = os.environ.get(EFFORT_ENV, "").strip()
    if preset:
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


if __name__ == "__main__":
    scion_harness.run("claude", provision)
