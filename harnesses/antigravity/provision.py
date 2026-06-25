#!/usr/bin/env python3
"""Antigravity container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. Responsibilities:

  1. Resolve auth from staged candidates (GEMINI_API_KEY or GOOGLE_API_KEY).
  2. Copy staged instructions to GEMINI.md (AGY's native instructions file).
  3. Generate .agents/hooks.json wiring AGY hook events to sciontool.
  4. Write outputs/env.json and outputs/resolved-auth.json.

Stdlib-only — no external dependencies.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

try:
    import scion_harness  # type: ignore[import-not-found]
except ImportError:
    scion_harness = None  # type: ignore[assignment]

PROVISION_VERSION = "2026-06-25T01:00:00Z"

VALID_AUTH_TYPES = ("oauth-token", "vertex-ai", "none")

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
    keys = {str(k) for k in raw if isinstance(k, str)}
    if os.environ.get("AGY_TOKEN"):
        keys.add("AGY_TOKEN")
    return keys


def _env_secret_files(candidates: dict[str, Any]) -> dict[str, str]:
    """Map of env-var name -> container path of its 0600 secret value file."""
    raw = candidates.get("env_secret_files") or {}
    out: dict[str, str] = {}
    if not isinstance(raw, dict):
        return out
    for k, v in raw.items():
        if isinstance(k, str) and isinstance(v, str) and v:
            out[k] = v
    return out


def _read_secret(env_secret_files: dict[str, str], name: str) -> str:
    """Read secret from staged file, falling back to env var. Returns '' on miss."""
    path = env_secret_files.get(name)
    if path:
        real = _expand(path)
        try:
            with open(real, "r", encoding="utf-8") as f:
                return f.read().rstrip("\r\n")
        except OSError:
            pass
    return os.environ.get(name, "")


def _parse_env_output(output: str, env: dict[str, str]) -> None:
    """Parse KEY=VALUE lines from daemon output into env dict."""
    for line in output.splitlines():
        for var in (
            "DBUS_SESSION_BUS_ADDRESS", "DBUS_SESSION_BUS_PID",
            "GNOME_KEYRING_CONTROL", "SSH_AUTH_SOCK",
            "GNOME_KEYRING_PID",
        ):
            if line.startswith(var + "="):
                val = line.split("=", 1)[1].rstrip(";").strip("'\"")
                env[var] = val


def _select_auth_method(
    explicit: str, env_keys: set[str], secret_files: dict[str, str],
    home: str,
) -> tuple[str, str]:
    has_token = "AGY_TOKEN" in secret_files or bool(_read_secret(secret_files, "AGY_TOKEN"))
    if not has_token:
        # AGY_TOKEN may be a file-type secret bind-mounted directly to its
        # target path rather than staged to the env secret directory.
        has_token = os.path.isfile(os.path.join(
            home, ".gemini", "antigravity-cli", "antigravity-oauth-token"
        ))
    has_gcp_project = any(k in env_keys for k in ("GOOGLE_CLOUD_PROJECT",))
    has_gcp_location = any(k in env_keys for k in ("GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"))

    # The host-side forwarder may not pass GOOGLE_CLOUD_LOCATION through
    # auth-candidates (only GOOGLE_CLOUD_REGION is forwarded). Fall back
    # to checking os.environ so the provisioner still detects GCP config.
    gcp_project = has_gcp_project or bool(os.environ.get("GOOGLE_CLOUD_PROJECT"))
    gcp_location = has_gcp_location or bool(
        os.environ.get("GOOGLE_CLOUD_LOCATION") or os.environ.get("GOOGLE_CLOUD_REGION")
    )

    if explicit:
        if explicit not in VALID_AUTH_TYPES:
            raise ValueError(
                f"antigravity: unknown auth type {explicit!r}; "
                f"valid types are: {', '.join(VALID_AUTH_TYPES)}"
            )
        if explicit == "vertex-ai":
            if not has_token:
                raise ValueError("antigravity: auth type 'vertex-ai' selected but AGY_TOKEN secret not found")
            if not gcp_project:
                raise ValueError("antigravity: auth type 'vertex-ai' selected but GOOGLE_CLOUD_PROJECT not found")
            if not gcp_location:
                raise ValueError("antigravity: auth type 'vertex-ai' selected but GOOGLE_CLOUD_LOCATION/REGION not found")
            return "vertex-ai", "AGY_TOKEN"
        if explicit == "oauth-token":
            if not has_token:
                raise ValueError("antigravity: auth type 'oauth-token' selected but AGY_TOKEN secret not found")
            return "oauth-token", "AGY_TOKEN"
        if explicit == "none":
            return "none", ""

    if has_token:
        if gcp_project and gcp_location:
            return "vertex-ai", "AGY_TOKEN"
        return "oauth-token", "AGY_TOKEN"

    return "none", ""


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
    os.makedirs(agents_dir, exist_ok=True)
    hooks_path = os.path.join(agents_dir, "hooks.json")
    _write_json(hooks_path, hooks_data)
    print(
        f"antigravity provision: generated hooks.json at {hooks_path}",
        file=sys.stderr,
    )


def _generate_wrapper_script(home: str, has_token: bool, is_enterprise: bool) -> None:
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
exec agy --dangerously-skip-permissions "$@"
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


def _apply_mcp(bundle: str) -> None:
    """Apply staged MCP server configuration into AGY's mcp_config.json."""
    if scion_harness is None:
        return
    try:
        count = scion_harness.apply_mcp_servers_simple(bundle, AGY_MCP_MAPPING)
    except (ValueError, OSError) as exc:
        print(
            f"antigravity provision: MCP config error: {exc}",
            file=sys.stderr,
        )
        return
    if count > 0:
        print(
            f"antigravity provision: applied {count} MCP server(s)",
            file=sys.stderr,
        )


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
    model: str = "",
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
        _write_json(settings_path, settings)

    # cache/onboarding.json — marks onboarding complete.
    # Always set enterpriseOnboardingComplete=true regardless of auth mode:
    # in consumer mode it's a no-op; in GCP mode it's required to skip the
    # enterprise onboarding flow (theme selector, etc.).
    onboarding_path = os.path.join(cache_dir, "onboarding.json")
    if not os.path.isfile(onboarding_path):
        _write_json(onboarding_path, {
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
        _write_json(project_path, {
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


def _provision(manifest: dict[str, Any]) -> int:
    home = os.environ.get("HOME") or os.path.expanduser("~")
    print(
        f"antigravity provision: version={PROVISION_VERSION} "
        f"home={home} uid={os.getuid()} gid={os.getgid()}",
        file=sys.stderr,
    )
    bundle = manifest.get("harness_bundle_dir") or "$HOME/.scion/harness"
    bundle = _expand(bundle)

    inputs_dir = os.path.join(bundle, "inputs")
    auth_candidates_path = os.path.join(inputs_dir, "auth-candidates.json")

    candidates: dict[str, Any] = {}
    if os.path.isfile(auth_candidates_path):
        try:
            candidates = _load_json(auth_candidates_path) or {}
        except (OSError, json.JSONDecodeError) as exc:
            print(
                f"antigravity provision: invalid auth-candidates.json: {exc}",
                file=sys.stderr,
            )
            return EXIT_ERROR

    explicit = str(candidates.get("explicit_type") or "").strip()
    env_keys = _present_env_keys(candidates)
    secret_files = _env_secret_files(candidates)

    # No-auth mode: when no auth candidates were staged and the harness config
    # declares a no_auth behavior, skip auth setup entirely.
    harness_cfg = manifest.get("harness_config") or {}
    no_auth_cfg = harness_cfg.get("no_auth") or {}
    no_auth_behavior = str(no_auth_cfg.get("behavior") or "").strip()

    if not candidates and no_auth_behavior:
        print(f"antigravity provision: no-auth mode (behavior={no_auth_behavior}), skipping auth setup", file=sys.stderr)
        method = "none"
        env_key = ""
    else:
        try:
            method, env_key = _select_auth_method(explicit, env_keys, secret_files, home)
        except ValueError as exc:
            print(str(exc), file=sys.stderr)
            return EXIT_ERROR

    outputs = manifest.get("outputs") or {}
    env_out = _expand(
        outputs.get("env") or os.path.join(bundle, "outputs", "env.json")
    )
    auth_out = _expand(
        outputs.get("resolved_auth")
        or os.path.join(bundle, "outputs", "resolved-auth.json")
    )

    resolved_payload: dict[str, Any] = {
        "schema_version": 1,
        "harness": "antigravity",
        "method": method,
        "explicit_type": explicit or None,
    }

    env_payload: dict[str, Any] = {}

    # Validate token if an auth method requiring it was selected
    has_token = False
    if method in ("oauth-token", "vertex-ai"):
        token_raw = _read_secret(secret_files, "AGY_TOKEN")
        if not token_raw:
            # AGY_TOKEN may be a file-type secret bind-mounted directly to its
            # target path rather than staged to the env secret directory.
            oauth_token_path = os.path.join(
                home, ".gemini", "antigravity-cli", "antigravity-oauth-token"
            )
            try:
                with open(oauth_token_path, "r", encoding="utf-8") as f:
                    token_raw = f.read().rstrip("\r\n")
                if token_raw:
                    print(
                        f"antigravity provision: read AGY_TOKEN from "
                        f"bind-mounted path {oauth_token_path}",
                        file=sys.stderr,
                    )
            except OSError:
                pass
        if not token_raw:
            print("antigravity provision: AGY_TOKEN secret is empty", file=sys.stderr)
            return EXIT_ERROR
        try:
            token_obj = json.loads(token_raw)
        except json.JSONDecodeError as exc:
            print(f"antigravity provision: AGY_TOKEN is not valid JSON: {exc}", file=sys.stderr)
            return EXIT_ERROR
        has_refresh = (
            "refresh_token" in token_obj
            or (isinstance(token_obj.get("token"), dict) and "refresh_token" in token_obj["token"])
        )
        if not isinstance(token_obj, dict) or not has_refresh:
            print("antigravity provision: AGY_TOKEN must contain refresh_token", file=sys.stderr)
            return EXIT_ERROR
        has_token = True

    is_enterprise = method == "vertex-ai"

    # Generate wrapper script that inits keyring and execs AGY.
    # Keyring daemons must run in AGY's process tree (not the
    # provisioner's) so they stay alive for the session.
    _generate_wrapper_script(home, has_token, is_enterprise)

    try:
        _write_json(auth_out, resolved_payload)
        _write_json(env_out, env_payload)
    except OSError as exc:
        print(
            f"antigravity provision: failed to write outputs: {exc}",
            file=sys.stderr,
        )
        return EXIT_ERROR

    harness_cfg = manifest.get("harness_config") or {}
    instructions_file = harness_cfg.get("instructions_file") or "GEMINI.md"
    # AGY doesn't support --model flag so we write the model into settings.json.
    # AGY_MODEL env var overrides; otherwise use the default.
    model = os.environ.get("AGY_MODEL", "") or "Gemini 3.5 Flash"
    _copy_instructions(bundle, home, instructions_file)
    _generate_hooks_json(home)
    _prestage_onboarding(home, enterprise=is_enterprise, model=model)
    _apply_mcp(bundle)

    print(f"antigravity provision: method={method}", file=sys.stderr)
    return EXIT_OK


def _dispatch(manifest: dict[str, Any]) -> int:
    command = str(manifest.get("command") or "provision")
    if command == "provision":
        return _provision(manifest)
    print(
        f"antigravity provision: unsupported command {command!r}",
        file=sys.stderr,
    )
    return EXIT_UNSUPPORTED


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Antigravity container-side provisioner"
    )
    parser.add_argument(
        "--manifest",
        help="Path to the staged manifest.json",
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
        print(
            f"antigravity provision: manifest not found at {manifest_path}",
            file=sys.stderr,
        )
        return EXIT_ERROR
    except (OSError, json.JSONDecodeError) as exc:
        print(
            f"antigravity provision: failed to load manifest: {exc}",
            file=sys.stderr,
        )
        return EXIT_ERROR

    if not isinstance(manifest, dict):
        print("antigravity provision: manifest is not an object", file=sys.stderr)
        return EXIT_ERROR

    return _dispatch(manifest)


if __name__ == "__main__":
    sys.exit(main())
