#!/usr/bin/env python3
"""Antigravity container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. Responsibilities:

  1. Resolve auth from staged candidates (GEMINI_API_KEY or GOOGLE_API_KEY).
  2. Copy staged instructions to GEMINI.md (AGY's native instructions file).
  3. Generate .agents/hooks.json wiring AGY hook events to sciontool.
  4. Write outputs/env.json and outputs/resolved-auth.json.

Stdlib-only — no external dependencies (beyond the staged scion_harness lib).
"""

from __future__ import annotations

import json
import os
import shutil
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness

assert scion_harness.INTERFACE_VERSION >= 2, (
    f"scion_harness INTERFACE_VERSION {scion_harness.INTERFACE_VERSION} < 2"
)

PROVISION_VERSION = "2026-07-09T01:00:00Z"

FLASH_MODEL = "Gemini 3.5 Flash"
PRO_MODEL = "Gemini 3.1 Pro"


def _resolve_thinking_tier(level: int) -> str:
    """Map a thinking level (0-100) to one of AGY's 4 CLI tiers."""
    level = max(0, min(100, level))
    if level >= 75:
        return "High"
    if level >= 50:
        return "Medium"
    if level >= 25:
        return "Low"
    return "Minimal"

AGY_MCP_MAPPING: dict[str, Any] = {
    "global_config_file": ".gemini/config/mcp_config.json",
    "global_config_path": "mcpServers",
    "transport_field": "type",
    "transport_map": {
        "stdio": "stdio",
        "sse": "sse",
        "streamable-http": "streamable-http",
    },
}

ANTIGRAVITY_AUTH = scion_harness.AuthSpec(
    "antigravity",
    [
        scion_harness.env_method(
            "vertex-ai",
            all_of=["AGY_TOKEN", "GOOGLE_CLOUD_PROJECT"],
            any_of=["GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"],
            env_fallback=True,
            hint="set AGY_TOKEN, GOOGLE_CLOUD_PROJECT, and GOOGLE_CLOUD_LOCATION",
        ),
        scion_harness.env_method(
            "oauth-token",
            any_of=["AGY_TOKEN"],
            env_fallback=True,
            hint="set AGY_TOKEN",
        ),
    ],
    fallback_to_none_on_error=True,
)


def _has_disk_oauth_token(ctx: scion_harness.ProvisionContext) -> bool:
    """Check if a bind-mounted antigravity-oauth-token exists on disk."""
    path = os.path.join(
        ctx.home, ".gemini", "antigravity-cli", "antigravity-oauth-token"
    )
    return os.path.isfile(path)


def provision(ctx: scion_harness.ProvisionContext) -> None:
    ctx.info(
        f"version={PROVISION_VERSION} "
        f"home={ctx.home} uid={os.getuid()} gid={os.getgid()}"
    )

    # Explicit "none" is valid for antigravity (file-only or no-auth setups).
    if ctx.explicit_type == "none":
        resolved = scion_harness.ResolvedAuth(method="none")
    else:
        # Disk-path probe: bind-mounted antigravity-oauth-token must count
        # toward token detection even when AGY_TOKEN is not staged as a
        # candidate. Check before select_auth so we can override "none".
        has_disk_token = _has_disk_oauth_token(ctx)

        try:
            resolved = ctx.select_auth(ANTIGRAVITY_AUTH)
        except scion_harness.ProvisionError:
            if ctx.explicit_type:
                raise
            resolved = scion_harness.ResolvedAuth(method="none")

        if resolved.method == "none" and has_disk_token:
            ctx.info("detected bind-mounted antigravity-oauth-token; using oauth-token auth")
            resolved = scion_harness.ResolvedAuth(method="oauth-token")

    method = resolved.method

    has_token = False
    if method in ("oauth-token", "vertex-ai"):
        token_raw = _read_agy_token(ctx)
        if not token_raw:
            oauth_token_path = os.path.join(
                ctx.home, ".gemini", "antigravity-cli", "antigravity-oauth-token"
            )
            try:
                with open(oauth_token_path, "r", encoding="utf-8") as f:
                    token_raw = f.read().rstrip("\r\n")
                if token_raw:
                    ctx.info(
                        f"read AGY_TOKEN from bind-mounted path {oauth_token_path}"
                    )
            except OSError:
                pass
        if not token_raw:
            raise scion_harness.ProvisionError("AGY_TOKEN secret is empty")
        try:
            token_obj = json.loads(token_raw)
        except json.JSONDecodeError as exc:
            raise scion_harness.ProvisionError(
                f"AGY_TOKEN is not valid JSON: {exc}"
            )
        has_refresh = (
            "refresh_token" in token_obj
            or (
                isinstance(token_obj.get("token"), dict)
                and "refresh_token" in token_obj["token"]
            )
        )
        if not isinstance(token_obj, dict) or not has_refresh:
            raise scion_harness.ProvisionError(
                "AGY_TOKEN must contain refresh_token"
            )
        has_token = True

    is_enterprise = method == "vertex-ai"

    instructions_file = ctx.harness_config.get("instructions_file") or "GEMINI.md"
    model = (
        ctx.harness_config.get("model")
        or os.environ.get("AGY_MODEL", "")
        or FLASH_MODEL
    )
    thinking_raw = os.environ.get("SCION_THINKING_LEVEL", "").strip()
    thinking_tier: str | None = None
    if thinking_raw.isdigit():
        thinking_level = int(thinking_raw)
        thinking_tier = _resolve_thinking_tier(thinking_level)
        ctx.info(f"model={model} thinking_level={thinking_level} tier={thinking_tier}")
    else:
        ctx.info(f"model={model} thinking_level=unset (using AGY default)")

    _generate_wrapper_script(ctx.home, has_token, is_enterprise, thinking_tier=thinking_tier)
    ctx.write_outputs(resolved, env={})
    _copy_instructions(ctx.bundle_dir, ctx.home, instructions_file)
    _generate_hooks_json(ctx.home)
    _prestage_onboarding(
        ctx.home, enterprise=is_enterprise, model=model,
        thinking_tier=thinking_tier,
    )
    _apply_mcp(ctx)

    ctx.info(f"method={method}")


def _read_agy_token(ctx: scion_harness.ProvisionContext) -> str:
    """Read AGY_TOKEN from staged secret file (with path expansion) or env."""
    secret_path = ctx.env_secret_files.get("AGY_TOKEN")
    if secret_path:
        try:
            with open(scion_harness.expand_path(secret_path), "r", encoding="utf-8") as f:
                return f.read().rstrip("\r\n")
        except OSError:
            pass
    return os.environ.get("AGY_TOKEN", "")


# ---- Native functions below: keyring, hooks, onboarding, MCP ----


def _generate_hooks_json(home: str) -> None:
    """Generate .agents/hooks.json wiring AGY events to sciontool."""
    hook_cmd_template = (
        "jq --arg ev {event} '. + {{\"hook_event_name\": $ev}}' "
        "| sciontool hook --dialect=antigravity"
    )

    def _simple_hook(event: str) -> list[dict[str, Any]]:
        return [
            {
                "type": "command",
                "command": hook_cmd_template.format(event=event),
                "timeout": 10,
            }
        ]

    def _tool_hook(event: str) -> list[dict[str, Any]]:
        return [
            {
                "matcher": ".*",
                "hooks": [
                    {
                        "type": "command",
                        "command": hook_cmd_template.format(event=event),
                        "timeout": 10,
                    }
                ],
            }
        ]

    hooks_data: dict[str, Any] = {
        "scion-hooks": {
            "PreInvocation": _simple_hook("PreInvocation"),
            "PostInvocation": _simple_hook("PostInvocation"),
            "PreToolUse": _tool_hook("PreToolUse"),
            "PostToolUse": _tool_hook("PostToolUse"),
            "Stop": _simple_hook("Stop"),
        }
    }

    # AGY only fires project-local hooks. The global path
    # (~/.gemini/antigravity-cli/hooks.json) loads but never executes.
    agents_dir = os.path.join("/workspace", ".agents")
    try:
        os.makedirs(agents_dir, exist_ok=True)
        hooks_path = os.path.join(agents_dir, "hooks.json")
        scion_harness.atomic_write_json(hooks_path, hooks_data)
        print(
            f"antigravity provision: generated hooks.json at {hooks_path}",
            file=sys.stderr,
        )
    except (OSError, PermissionError) as exc:
        print(
            f"antigravity provision: warning: could not write hooks.json "
            f"to {agents_dir}: {exc}",
            file=sys.stderr,
        )


def _generate_wrapper_script(
    home: str, has_token: bool, is_enterprise: bool,
    thinking_tier: str | None = None,
) -> None:
    """Generate agy-wrapper.sh that inits keyring and execs AGY.

    The keyring daemons must run in the same process tree as AGY so they
    stay alive for the duration of the session. A provisioner-started daemon
    dies when the provisioner exits, so we bootstrap inline here.

    Token injection always includes an env-var fallback because the
    provisioner runs before scion-env is sourced — AGY_TOKEN may
    only be available in the child process environment, not during provisioning.

    GCP/enterprise settings are also patched here at runtime for the same
    reason — GOOGLE_CLOUD_PROJECT/GOOGLE_CLOUD_LOCATION and AGY_USE_GCP
    are only available in the child environment.
    """
    secret_path = os.path.join(
        home, ".scion", "harness", "secrets", "AGY_TOKEN"
    )
    oauth_token_path = os.path.join(
        home, ".gemini", "antigravity-cli", "antigravity-oauth-token"
    )
    settings_path = os.path.join(
        home, ".gemini", "antigravity-cli", "settings.json"
    )
    onboarding_path = os.path.join(
        home, ".gemini", "antigravity-cli", "cache", "onboarding.json"
    )

    # Enterprise marker: provisioner writes this when explicit vertex-ai
    # is selected. The wrapper checks both this file and AGY_USE_GCP env.
    enterprise_marker = os.path.join(
        home, ".scion", "harness", ".enterprise-mode"
    )
    if is_enterprise:
        os.makedirs(os.path.dirname(enterprise_marker), exist_ok=True)
        with open(enterprise_marker, "w") as f:
            f.write("1")
    elif os.path.exists(enterprise_marker):
        # Idempotent reprovision: if the auth mode switched away from
        # vertex-ai (e.g. to oauth-token), remove the stale marker so the
        # wrapper does not keep running in enterprise/GCP mode.
        os.remove(enterprise_marker)

    script = f"""#!/bin/bash
# Generated by antigravity provision.py {PROVISION_VERSION}
set -e

# Initialize DBUS session bus
eval $(dbus-launch --sh-syntax)
export DBUS_SESSION_BUS_ADDRESS

# Unlock and start gnome-keyring
eval $(echo "test" | gnome-keyring-daemon --unlock 2>/dev/null)
gnome-keyring-daemon --start --components=secrets,pkcs11,ssh > /dev/null 2>&1

echo "agy-wrapper: keyring initialized (DBUS=$DBUS_SESSION_BUS_ADDRESS)" >&2

# Save DBUS session address so other processes (e.g. capture_auth.py) can use the keyring
echo "DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS" > ~/.scion/harness/.dbus-env

# Inject OAuth token into keyring (staging file, target path, env var fallback)
if [ -f "{secret_path}" ]; then
    secret-tool store \\
        --label="Password for antigravity on gemini" \\
        service gemini username antigravity \\
        < "{secret_path}" 2>/dev/null \\
        && echo "agy-wrapper: token injected into keyring (from staging file)" >&2 \\
        || echo "agy-wrapper: WARNING: failed to inject token" >&2
elif [ -f "{oauth_token_path}" ]; then
    secret-tool store \\
        --label="Password for antigravity on gemini" \\
        service gemini username antigravity \\
        < "{oauth_token_path}" 2>/dev/null \\
        && echo "agy-wrapper: token injected into keyring (from target path)" >&2 \\
        || echo "agy-wrapper: WARNING: failed to inject token" >&2
elif [ -n "${{AGY_TOKEN:-}}" ]; then
    printf '%s' "$AGY_TOKEN" | secret-tool store \\
        --label="Password for antigravity on gemini" \\
        service gemini username antigravity 2>/dev/null \\
        && echo "agy-wrapper: token injected into keyring (from env)" >&2 \\
        || echo "agy-wrapper: WARNING: failed to inject token" >&2
else
    echo "agy-wrapper: no token available, AGY will prompt for login" >&2
fi

# GCP/enterprise mode: patch settings.json with gcp block and mark
# enterprise onboarding complete. Triggered by explicit vertex-ai auth
# (marker file) or AGY_USE_GCP=true env var.
_use_gcp=false
if [ -f "{enterprise_marker}" ]; then
    _use_gcp=true
elif [ "${{AGY_USE_GCP:-}}" = "true" ] || [ "${{AGY_USE_GCP:-}}" = "1" ] || [ "${{AGY_USE_GCP:-}}" = "yes" ]; then
    _use_gcp=true
elif [ -n "${{GOOGLE_CLOUD_PROJECT:-}}" ]; then
    _use_gcp=true
fi

if [ "$_use_gcp" = "true" ]; then
    _gcp_project="${{GOOGLE_CLOUD_PROJECT:-}}"
    _gcp_location="${{GOOGLE_CLOUD_LOCATION:-${{GOOGLE_CLOUD_REGION:-global}}}}"

    if [ -n "$_gcp_project" ]; then
        python3 -c "
import json, sys
p = '{settings_path}'
with open(p) as f: d = json.load(f)
d['gcp'] = {{'project': '$_gcp_project', 'location': '$_gcp_location'}}
d['enableTelemetry'] = False
with open(p, 'w') as f: json.dump(d, f, indent=2); f.write('\\n')
print('agy-wrapper: patched settings.json with gcp config', file=sys.stderr)
"
    else
        echo "agy-wrapper: WARNING: GCP mode but GOOGLE_CLOUD_PROJECT not set" >&2
    fi

    python3 -c "
import json, sys
p = '{onboarding_path}'
with open(p) as f: d = json.load(f)
d['enterpriseOnboardingComplete'] = True
with open(p, 'w') as f: json.dump(d, f, indent=2); f.write('\\n')
print('agy-wrapper: marked enterprise onboarding complete', file=sys.stderr)
"
fi

# Exec AGY with all arguments passed through
exec agy --dangerously-skip-permissions{f' --thinking-level {thinking_tier}' if thinking_tier else ''} "$@"
"""

    wrapper_path = os.path.join(home, ".scion", "harness", "agy-wrapper.sh")
    os.makedirs(os.path.dirname(wrapper_path), exist_ok=True)
    with open(wrapper_path, "w", encoding="utf-8") as f:
        f.write(script)
    os.chmod(wrapper_path, 0o755)
    print(
        f"antigravity provision: generated wrapper at {wrapper_path}",
        file=sys.stderr,
    )


def _apply_mcp(ctx: scion_harness.ProvisionContext) -> None:
    """Apply staged MCP server configuration into AGY's mcp_config.json."""
    try:
        count = scion_harness.apply_mcp_servers_simple(ctx.bundle_dir, AGY_MCP_MAPPING)
    except (ValueError, OSError) as exc:
        ctx.info(f"MCP config error: {exc}")
        return
    if count > 0:
        ctx.info(f"applied {count} MCP server(s)")


def _copy_instructions(bundle: str, home: str, instructions_file: str) -> None:
    """Copy staged instructions to the path declared in config.yaml."""
    src = os.path.join(bundle, "inputs", "instructions.md")
    if not os.path.isfile(src):
        return
    dst = os.path.join(home, instructions_file)
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    shutil.copy2(src, dst)
    print(
        f"antigravity provision: copied instructions to {dst}",
        file=sys.stderr,
    )


def _prestage_onboarding(
    home: str, workspace: str = "/workspace", enterprise: bool = False,
    model: str = "", thinking_tier: str | None = None,
) -> None:
    """Pre-stage AGY config files to skip interactive onboarding.

    AGY requires several files to exist before it will skip the login
    menu, theme selection, workspace trust prompt, and TOS agreement.

    When enterprise=True (vertex-ai mode), also marks enterprise
    onboarding complete. GCP project/location are patched at runtime
    by the wrapper script (env vars aren't available during provisioning).
    """
    import uuid

    gemini_dir = os.path.join(home, ".gemini")
    cli_dir = os.path.join(gemini_dir, "antigravity-cli")
    config_dir = os.path.join(gemini_dir, "config")
    projects_dir = os.path.join(config_dir, "projects")
    skills_dir = os.path.join(config_dir, "skills")
    cache_dir = os.path.join(cli_dir, "cache")
    bin_dir = os.path.join(cli_dir, "bin")
    antigravity_dir = os.path.join(home, ".antigravitycli")

    for d in (cli_dir, config_dir, projects_dir, skills_dir,
              cache_dir, bin_dir, antigravity_dir,
              os.path.join(cli_dir, "knowledge"),
              os.path.join(cli_dir, "log"),
              os.path.join(cli_dir, "conversations"),
              os.path.join(cli_dir, "brain")):
        os.makedirs(d, exist_ok=True)

    # settings.json — trusts workspace, marks onboarding complete, sets model.
    # onboardingComplete lives here (not in cache/onboarding.json) per
    # observed post-login AGY config state.
    settings_path = os.path.join(cli_dir, "settings.json")
    if not os.path.isfile(settings_path):
        settings: dict[str, Any] = {
            "colorScheme": "dark",
            "onboardingComplete": True,
            "trustedWorkspaces": [workspace],
        }
        if model:
            settings["model"] = model
        scion_harness.atomic_write_json(settings_path, settings)

    # cache/onboarding.json — marks onboarding complete.
    # Always set enterpriseOnboardingComplete=true regardless of auth mode:
    # in consumer mode it's a no-op; in GCP mode it's required to skip the
    # enterprise onboarding flow (theme selector, etc.).
    onboarding_path = os.path.join(cache_dir, "onboarding.json")
    if not os.path.isfile(onboarding_path):
        scion_harness.atomic_write_json(onboarding_path, {
            "consumerOnboardingComplete": True,
            "enterpriseOnboardingComplete": True,
            "onboardingComplete": True,
        })

    # installation_id — unique per container
    install_id_path = os.path.join(cli_dir, "installation_id")
    if not os.path.isfile(install_id_path):
        with open(install_id_path, "w") as f:
            f.write(str(uuid.uuid4()))

    # project registration with gitFolder format
    project_id = str(uuid.uuid4())
    project_path = os.path.join(projects_dir, project_id + ".json")
    if not any(f.endswith(".json") for f in os.listdir(projects_dir)):
        scion_harness.atomic_write_json(project_path, {
            "id": project_id,
            "name": workspace,
            "projectResources": {
                "resources": [{
                    "gitFolder": {
                        "folderUri": f"file://{workspace}",
                        "allowWrite": True,
                    },
                }],
            },
        })

    # workspace marker
    workspace_marker = os.path.join(antigravity_dir, project_id + ".json")
    if not any(f.endswith(".json") for f in os.listdir(antigravity_dir)):
        with open(workspace_marker, "w") as f:
            pass  # empty file

    # bin/agentapi shim
    agentapi_path = os.path.join(bin_dir, "agentapi")
    if not os.path.isfile(agentapi_path):
        with open(agentapi_path, "w") as f:
            f.write('#!/bin/sh\nexec "/usr/local/bin/agy" agentapi "$@"\n')
        os.chmod(agentapi_path, 0o755)

    # skills/.gitkeep
    gitkeep_path = os.path.join(skills_dir, ".gitkeep")
    if not os.path.isfile(gitkeep_path):
        with open(gitkeep_path, "w") as f:
            pass

    # config migration marker
    migrated_path = os.path.join(config_dir, ".migrated")
    if not os.path.isfile(migrated_path):
        with open(migrated_path, "w") as f:
            pass

    # .geminiignore
    ignore_path = os.path.join(gemini_dir, ".geminiignore")
    if not os.path.isfile(ignore_path):
        with open(ignore_path, "w") as f:
            f.write(".scion/\n")

    # Chown everything under ~/.gemini to the agent user so AGY (which runs
    # as that user) can write back to settings.json, onboarding.json, etc.
    # The provisioner runs as root; without this AGY silently fails on writes
    # and loops back to onboarding steps like the theme selector.
    # Use stat(home) to get the target uid/gid — USER env var may be "root"
    # when the provisioner runs as root, making getpwnam("root") wrong.
    try:
        home_stat = os.stat(home)
        uid, gid = home_stat.st_uid, home_stat.st_gid
        print(
            f"antigravity provision: chowning ~/.gemini to uid={uid} gid={gid}",
            file=sys.stderr,
        )
        count = 0
        skipped = 0
        for dirpath, dirnames, filenames in os.walk(gemini_dir):
            try:
                os.chown(dirpath, uid, gid)
            except OSError:
                skipped += 1
            for fname in filenames:
                fpath = os.path.join(dirpath, fname)
                try:
                    os.chown(fpath, uid, gid)
                    count += 1
                except OSError:
                    skipped += 1
        print(
            f"antigravity provision: chown complete "
            f"({count} files, {skipped} skipped read-only)",
            file=sys.stderr,
        )
    except OSError as exc:
        print(
            f"antigravity provision: warning: chown ~/.gemini failed: {exc}",
            file=sys.stderr,
        )

    print(
        "antigravity provision: pre-staged onboarding files",
        file=sys.stderr,
    )


if __name__ == "__main__":
    scion_harness.run("antigravity", provision)
