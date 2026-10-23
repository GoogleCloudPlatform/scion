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
"""Copilot container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook.
Uses scion_harness library for auth selection, instruction projection,
MCP translation, and output writing.

Copilot-native concerns handled here:
  - Auth token is always exposed as COPILOT_GITHUB_TOKEN in env.json.
  - MCP servers translate to Copilot's native format in ~/.copilot/mcp-config.json
    (stdio→local, sse/streamable-http→http).
  - Instructions project to ~/.copilot/copilot-instructions.md (configurable).
  - ~/.copilot/settings.json and config.json get sane defaults.
  - Native OTel telemetry, when enabled, always routes to sciontool's local
    receiver (never a cloud endpoint); see the Telemetry section below.
  - Agent Plugins (hub.agent_plugins) install through `copilot plugin
    install` / `uninstall`, last in provision(); see CopilotPluginInstaller.

Exception to §4.2 (env.json placeholder policy): copilot receives its auth
token exclusively via env.json — the runtime env projection does not deliver
auth env vars to the copilot process.  Raw token values are written with 0600
permissions on the secret file.
"""

from __future__ import annotations

import json
import math
import os
import shutil
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness

assert scion_harness.INTERFACE_VERSION >= 2, (
    "copilot provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {scion_harness.INTERFACE_VERSION}"
)

COPILOT_CONFIG_FILE = "~/.copilot/config.json"

# What counts as sign-in in copilot's config.json. This is the same rule the
# host applies before it ships the file into an agent
# (copilotConfigHoldsSignIn in pkg/harness/host_credentials_content.go), so
# the host and the container agree about the same file. Keep the two in sync:
# provision_test.py checks these key sets against the Go source and runs this
# predicate over the Go test fixtures in
# pkg/harness/testdata/copilot_config_signin.json.
#
# Top-level keys, lowercased, that hold settings or identity but never a
# credential (copilotSettingsKeys on the host). Includes trustedFolders,
# which _ensure_settings writes on every provision, and installedPlugins,
# where `copilot plugin install` records plugins (CopilotPluginInstaller).
_COPILOT_SETTINGS_KEYS = frozenset({
    "banner",
    "beep",
    "copilot_integration_id",
    "copilot_url",
    "installedplugins",
    "last_logged_in_user",
    "logged_in_users",
    "model",
    "render_markdown",
    "screen_reader",
    "theme",
    "trusted_folders",
    "trustedfolders",
})

# Top-level keys, lowercased, where copilot keeps a credential. They count as
# sign-in only with a non-empty value (see _config_holds_sign_in).
_COPILOT_TOKENS_KEY = "copilot_tokens"
_COPILOT_HMAC_KEYS = frozenset({"copilot_hmac_key", "capi_hmac_key"})
_COPILOT_CREDENTIAL_KEYS = frozenset({_COPILOT_TOKENS_KEY}) | _COPILOT_HMAC_KEYS

def _reject_json_constant(name: str) -> Any:
    # Go's encoding/json rejects NaN and Infinity; Python's json accepts them.
    raise ValueError(f"invalid JSON constant {name}")


def _reject_non_finite_float(literal: str) -> float:
    # A literal such as 1e400 parses to inf, which json would write back as
    # non-standard Infinity. Only the strict loaders use this: the host
    # accepts 1e400 (it never converts numbers), so the sign-in rule must not.
    value = float(literal)
    if not math.isfinite(value):
        raise ValueError("JSON number out of float range")
    return value


def _json_int_unconverted(literal: str) -> int:
    # The host decodes config.json into json.RawMessage values and never
    # converts numbers, so an integer literal beyond Python's int conversion
    # limit (4300 digits) must not make the content unparseable here. The
    # predicate below only tests values for being non-empty strings, so a
    # placeholder number is enough.
    return 0


def _config_holds_sign_in(raw: bytes) -> bool:
    """Report whether config.json content may hold sign-in data.

    Mirrors the host's copilotConfigHoldsSignIn. Lines that start with "//"
    (after leading whitespace) are dropped and the rest is parsed as JSON.
    Top-level keys are compared lowercased, as copilot lowercases them.
    One known difference: Python and Go lowercase U+0130 differently, so a
    key spelled with it is unknown here (fail open, signed in) while the
    host may read it as a credential key.

      - copilot_tokens holding a string value that is not blank, or a
        non-empty string in copilot_hmac_key or capi_hmac_key, is sign-in.
      - Otherwise, a file whose other keys are all known settings keys is not
        sign-in; empty credential values do not count either. This is the
        file scion writes, and the file of a user who signs in through the OS
        keychain.
      - Any key in neither list counts as possible sign-in (fail open): a
        future copilot version may keep a credential under a new key.
      - Content that is not a JSON object (unparseable, an array, null, or
        empty) is not sign-in: copilot cannot read sign-in from it, and
        _ensure_settings replaces unparseable JSON. Invalid UTF-8 outside a
        string and NaN or Infinity make content unparseable, as on the host;
        invalid UTF-8 inside a string and integers of any length do not.

    Values are only tested for emptiness; they are never logged or returned.
    """
    try:
        return _config_holds_sign_in_or_too_deep(raw)
    except RecursionError:
        return False


def _config_holds_sign_in_or_too_deep(raw: bytes) -> bool:
    """_config_holds_sign_in, but raise RecursionError for content nested too
    deeply for Python's decoder. The host's decoder allows deeper nesting, so
    such content may hold sign-in there; _ensure_settings must keep it.
    """
    text = raw.decode("utf-8", errors="replace")
    try:
        top = json.loads(
            scion_harness.drop_json_comment_lines(text),
            parse_constant=_reject_json_constant,
            parse_int=_json_int_unconverted,
        )
    except ValueError:
        return False
    if not isinstance(top, dict):
        return False
    unknown = False
    for key, value in top.items():
        k = key.lower()
        if k == _COPILOT_TOKENS_KEY:
            if isinstance(value, dict) and any(
                # HOST_SPACE is what Go's strings.TrimSpace removes; Python's
                # str.strip() also removes U+001C..U+001F.
                isinstance(v, str) and v.strip(scion_harness.HOST_SPACE)
                for v in value.values()
            ):
                return True
        elif k in _COPILOT_HMAC_KEYS:
            if isinstance(value, str) and value:
                return True
        elif k not in _COPILOT_SETTINGS_KEYS:
            unknown = True
    return unknown


def _config_file_may_hold_credential(path: str) -> bool:
    """Report whether an existing ~/.copilot/config.json may hold sign-in state.

    config.json is both copilot's settings file and its sign-in file, and
    _ensure_settings writes trustedFolders into it on every provision, so a
    file holding only settings must not satisfy the auth-file method, or a
    restart with no credential would look signed in. The content rule is the
    host's (_config_holds_sign_in). A file that cannot be read counts as
    possible sign-in (fail open, as before); the host has no such case,
    because it only applies its rule to content it has already read.
    """
    try:
        with open(path, "rb") as f:
            raw = f.read()
    except OSError:
        return True
    return _config_holds_sign_in(raw)


AUTH = scion_harness.AuthSpec(
    "copilot",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"],
            hint=(
                "set COPILOT_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN "
                'with a fine-grained PAT that has "Copilot Requests" permission'
            ),
            env_fallback=True,
        ),
        scion_harness.file_method(
            "auth-file",
            path=COPILOT_CONFIG_FILE,
            hint=f"provide copilot config at {COPILOT_CONFIG_FILE}",
            secret_key="COPILOT_CONFIG",
            present_check=_config_file_may_hold_credential,
        ),
    ],
    fallback_to_none_on_error=True,
)


def _read_token(ctx: scion_harness.ProvisionContext, env_key: str) -> str:
    """Read the token for an env-based auth method.

    Expands $HOME-style variables in secret file paths, then falls back to
    os.environ (hub-registered configs may not stage secret files).
    """
    path = ctx.env_secret_files.get(env_key)
    if path:
        expanded = scion_harness.expand_path(path)
        try:
            with open(expanded, "r", encoding="utf-8") as f:
                return f.read().rstrip("\r\n")
        except OSError:
            pass
    return os.environ.get(env_key, "")


def _validate_staged_config(content: str, path: str) -> bool:
    """Check that staged config.json content is JSON copilot can read.

    The rules are _load_config_strictly's, except for two Python limits:
    the content is copied unchanged, and copilot and the host read both
    integers of any length and nesting deeper than Python's decoder allows,
    so a working config is not refused for either. A long integer is
    accepted; content nested too deeply to check is accepted unchecked and
    False is returned (as _ensure_settings keeps such a config.json). NaN,
    Infinity and numbers beyond float range (1e400) are refused, as copilot
    cannot parse them. Raises ProvisionError naming path; the message never
    includes file content. Returns True when the content was checked.
    """
    prefix = f"COPILOT_CONFIG secret file {path}"
    try:
        json.loads(
            scion_harness.drop_json_comment_lines(content),
            parse_constant=_reject_json_constant,
            parse_float=_reject_non_finite_float,
            parse_int=_json_int_unconverted,
        )
    except json.JSONDecodeError as exc:
        raise scion_harness.ProvisionError(
            f"{prefix} is not valid JSON "
            f"({exc.msg} at line {exc.lineno} column {exc.colno})"
        ) from None
    except ValueError:
        # Only _reject_json_constant and _reject_non_finite_float raise a
        # plain ValueError here.
        raise scion_harness.ProvisionError(
            f"{prefix} holds NaN, Infinity or a number beyond float range, "
            "which copilot cannot read"
        ) from None
    except RecursionError:
        return False
    return True


def _write_copilot_config_file(ctx: scion_harness.ProvisionContext) -> None:
    """Write ~/.copilot/config.json from a staged COPILOT_CONFIG file secret."""
    staged = scion_harness.expand_path(ctx.file_secret_files.get("COPILOT_CONFIG") or "")
    try:
        content = ctx.read_file_secret("COPILOT_CONFIG")
    except UnicodeDecodeError:
        raise scion_harness.ProvisionError(
            f"COPILOT_CONFIG secret file {staged} is not valid UTF-8"
        ) from None
    if not content:
        return
    if not content.strip():
        raise scion_harness.ProvisionError("COPILOT_CONFIG secret is empty")
    if not _validate_staged_config(content, staged):
        ctx.info(
            f"COPILOT_CONFIG secret file {staged} is nested too deeply to "
            "check; copying it unchanged"
        )
    config_dir = scion_harness.expand_path("~/.copilot")
    os.makedirs(config_dir, exist_ok=True)
    target = os.path.join(config_dir, "config.json")
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(content)
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


def _write_mcp_config(ctx: scion_harness.ProvisionContext, servers: dict[str, Any]) -> None:
    """Write MCP servers to ~/.copilot/mcp-config.json."""
    config_dir = os.path.join(ctx.home, ".copilot")
    os.makedirs(config_dir, exist_ok=True)
    config_path = os.path.join(config_dir, "mcp-config.json")
    scion_harness.atomic_write_json(config_path, {"mcpServers": servers})


# ---------------------------------------------------------------------------
# Telemetry – native OTel export
# ---------------------------------------------------------------------------
#
# Copilot's native OTel export always targets sciontool's local receiver.
# Previously this fell back to the shared SCION_OTEL_ENDPOINT (the cloud
# telemetry config alias) and copied cloud headers/CA into the harness env,
# which sent native telemetry straight to the cloud endpoint, bypassing
# sciontool's redaction and identity stamping entirely (fork issue #2053,
# phase 0). Copilot CLI v1.0.88's enterprise-managed OTel export activates
# via COPILOT_OTEL_ENABLED (COPILOT_TELEMETRY_ENABLED is not a real flag) and
# supports only the otlp-http exporter type (no otlp-grpc); the sciontool
# receiver's HTTP endpoint in turn accepts only protobuf
# (receiver.go:319-321), so the protocol here is fixed at http/protobuf.

# Default local HTTP port for sciontool's OTLP receiver.
_DEFAULT_OTEL_HTTP_PORT = "4318"
_DEFAULT_OTEL_PROTOCOL = "http/protobuf"


def _telemetry_enabled(telemetry: dict[str, Any] | None) -> bool:
    """Return True when the effective telemetry config says 'enabled'."""
    if not telemetry:
        return False
    enabled = telemetry.get("enabled")
    if enabled is None:
        return True
    return bool(enabled)


def _resolve_endpoint(env: dict[str, str] | None) -> str:
    """Resolve the OTLP endpoint Copilot's native exporter is pointed at.

    Always the local sciontool receiver, unless SCION_COPILOT_OTEL_ENDPOINT is
    set. That override is a local-debugging escape hatch only: it bypasses
    sciontool's redaction and identity stamping entirely, so it must never be
    pointed at anything but a local collector. The generic SCION_OTEL_ENDPOINT
    (the shared cloud-config alias) and telemetry.cloud.endpoint are
    deliberately NOT honored here anymore.
    """
    env = env or {}
    override = (env.get("SCION_COPILOT_OTEL_ENDPOINT") or os.environ.get("SCION_COPILOT_OTEL_ENDPOINT") or "").strip()
    if override:
        return override
    port = str(env.get("SCION_OTEL_HTTP_PORT") or os.environ.get("SCION_OTEL_HTTP_PORT") or _DEFAULT_OTEL_HTTP_PORT)
    if not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise scion_harness.ProvisionError("invalid local telemetry HTTP port")
    return f"http://127.0.0.1:{port}"


def _resolve_protocol(env: dict[str, str] | None) -> str:
    """Resolve the OTLP protocol.

    Always http/protobuf -- the only protocol the local receiver's HTTP
    endpoint accepts -- unless SCION_COPILOT_OTEL_PROTOCOL is set. Unlike
    SCION_COPILOT_OTEL_ENDPOINT (_resolve_endpoint), this override does not
    itself bypass redaction or identity stamping: it only changes the wire
    format used to reach whichever endpoint is in effect, so a debug
    endpoint pointed at a non-protobuf collector can still be reached.
    """
    env = env or {}
    override = (env.get("SCION_COPILOT_OTEL_PROTOCOL") or os.environ.get("SCION_COPILOT_OTEL_PROTOCOL") or "").strip()
    if override:
        return override
    return _DEFAULT_OTEL_PROTOCOL


def _build_telemetry_env(env: dict[str, str] | None) -> dict[str, str]:
    """Build env vars that direct Copilot CLI's native OTel emitter to sciontool.

    This always targets the local receiver -- see _resolve_endpoint.

    OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta requests delta
    temporality for Copilot's native metrics (design §3.7). A real capture
    (phase 3a) shows Copilot CLI 1.0.89 does not honor this -- it has no
    documented temporality override at all, and every point it exports is
    cumulative regardless of this setting. The usage deriver's metric rule
    (ptone/scion#2053 phase 3a) converts cumulative points to per-export
    deltas itself rather than relying on the source to do it (design §5's
    copilot row). The env var is kept anyway: it is harmless, forward
    compatible if a future Copilot release adds real support, and not left
    to the default, which an unpinned CLI could change.
    """
    return {
        "COPILOT_OTEL_ENABLED": "true",
        "COPILOT_OTEL_EXPORTER_TYPE": "otlp-http",
        "OTEL_EXPORTER_OTLP_ENDPOINT": _resolve_endpoint(env),
        "OTEL_EXPORTER_OTLP_PROTOCOL": _resolve_protocol(env),
        "OTEL_METRICS_EXPORTER": "otlp",
        "OTEL_LOGS_EXPORTER": "otlp",
        "OTEL_METRIC_EXPORT_INTERVAL": "30000",
        "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "delta",
    }


# ---------------------------------------------------------------------------
# Hooks – sciontool event bridge
# ---------------------------------------------------------------------------

_COPILOT_HOOK_EVENTS = [
    "sessionStart",
    "sessionEnd",
    "userPromptSubmitted",
    "preToolUse",
    "postToolUse",
    "errorOccurred",
    "agentStop",
    "subagentStop",
]


def _write_hooks(home: str) -> None:
    """Write ~/.copilot/hooks/scion.json wiring Copilot events to sciontool.

    Each hook fires ``sciontool hook <event> --dialect=copilot``. Copilot
    pipes the event's JSON payload (camelCase keys, selected by the camelCase
    event names) on stdin; sciontool merges it under the named event and
    processes it through the copilot mapping dialect (staged as dialect.yaml
    alongside this script).
    """
    hooks: dict[str, list[dict[str, Any]]] = {}
    for event in _COPILOT_HOOK_EVENTS:
        hooks[event] = [
            {
                "type": "command",
                "bash": f"sciontool hook {event} --dialect=copilot",
            }
        ]

    hooks_data: dict[str, Any] = {"version": 1, "hooks": hooks}

    hooks_dir = os.path.join(home, ".copilot", "hooks")
    os.makedirs(hooks_dir, exist_ok=True)
    hooks_path = os.path.join(hooks_dir, "scion.json")
    try:
        scion_harness.atomic_write_json(hooks_path, hooks_data)
    except (OSError, PermissionError) as exc:
        print(
            f"copilot provision: warning: could not write hooks to "
            f"{hooks_path}: {exc}",
            file=sys.stderr,
        )


def _load_config_strictly(raw: bytes) -> Any:
    """Parse config.json content that is safe to rewrite.

    Lines that start with "//" are dropped, as copilot allows. Raises
    ValueError (UnicodeDecodeError and json.JSONDecodeError included) or
    RecursionError when the content is not strict UTF-8, not JSON, holds an
    integer too long to convert, holds NaN or Infinity, which copilot's own
    JSON parser rejects, or holds a number beyond float range (1e400), which
    would be written back as Infinity.

    Mirrors scion_harness.read_json_skipping_comment_lines, and shares its
    comment-line rule (the host's), except that it takes bytes, rejects NaN,
    Infinity and numbers beyond float range, and lets UnicodeDecodeError and
    the long-integer ValueError reach the caller as unreadable content.
    """
    text = raw.decode("utf-8")
    return json.loads(
        scion_harness.drop_json_comment_lines(text),
        parse_constant=_reject_json_constant,
        parse_float=_reject_non_finite_float,
    )


def _ensure_settings(ctx: scion_harness.ProvisionContext) -> None:
    """Ensure ~/.copilot/settings.json and config.json have sane defaults."""
    config_dir = os.path.join(ctx.home, ".copilot")
    os.makedirs(config_dir, exist_ok=True)

    settings_path = os.path.join(config_dir, "settings.json")
    settings: dict[str, Any] = {}
    if os.path.isfile(settings_path):
        try:
            with open(settings_path, "r", encoding="utf-8") as f:
                loaded = json.load(
                    f,
                    parse_constant=_reject_json_constant,
                    parse_float=_reject_non_finite_float,
                )
            if isinstance(loaded, dict):
                settings = loaded
        except (OSError, ValueError, RecursionError):
            # Unreadable, not UTF-8, not JSON, an integer too long to
            # convert, NaN or Infinity, a number beyond float range (1e400),
            # or nested too deeply: rewritten with the defaults, so
            # non-standard JSON is never written back.
            pass

    # remoteSessions: keep Copilot's remote control (steering the session from
    # github.com or GitHub Mobile) off. It is off by default; this pins it now
    # that the --no-remote flag no longer exists. A value already set is kept.
    defaults = {"autoUpdate": False, "banner": "never", "remoteSessions": False}
    changed = False
    for key, value in defaults.items():
        if key not in settings:
            settings[key] = value
            changed = True
    if changed:
        scion_harness.atomic_write_json(settings_path, settings)

    config_path = os.path.join(config_dir, "config.json")
    config: dict[str, Any] = {}
    too_deep_note = (
        "~/.copilot/config.json is nested too deeply to read; leaving it "
        "unchanged (workspace not added to trustedFolders)"
    )
    if os.path.isfile(config_path):
        try:
            with open(config_path, "rb") as f:
                raw = f.read()
        except OSError:
            raw = None
        if raw is not None:
            try:
                loaded = _load_config_strictly(raw)
                if isinstance(loaded, dict):
                    config = loaded
            except RecursionError:
                # Too deep for Python's decoder, though the host (Go, 10000
                # levels) may still read sign-in from it: never replace it.
                ctx.info(too_deep_note)
                return
            except ValueError:
                # Not cleanly readable: not JSON, not UTF-8, an integer too
                # long to convert, NaN or Infinity, or a number beyond float
                # range. Rewriting it could only write back a lossy copy, so
                # a file that may hold sign-in (the host's rule) is left
                # untouched and any other file is replaced. The sign-in parse
                # may itself hit the nesting limit past the defect: keep that
                # file too.
                try:
                    may_hold_sign_in = _config_holds_sign_in_or_too_deep(raw)
                except RecursionError:
                    ctx.info(too_deep_note)
                    return
                if may_hold_sign_in:
                    ctx.info(
                        "~/.copilot/config.json is not clean JSON but may hold "
                        "sign-in; leaving it unchanged (workspace not added to "
                        "trustedFolders)"
                    )
                    return

    folders = config.get("trustedFolders")
    if not isinstance(folders, list):
        config["trustedFolders"] = [ctx.workspace]
        scion_harness.atomic_write_json(config_path, config)
    elif ctx.workspace not in folders:
        folders.append(ctx.workspace)
        scion_harness.atomic_write_json(config_path, config)


# ---------------------------------------------------------------------------
# Agent Plugins (hub.agent_plugins)
# ---------------------------------------------------------------------------
#
# Route: `copilot plugin install <abs dir>` from the mirrored bundle
# (~/.scion/agent-plugins/bundles/<name>) and `copilot plugin uninstall
# <name>@` (the direct install only; see below). Observed with copilot 1.0.97:
#   - install has no --json flag; it prints a plain-text result and a
#     deprecation warning for direct (non-marketplace) installs;
#   - it copies the source into ~/.copilot/installed-plugins/_direct/<id>,
#     where <id> is the basename of the source dir (not the plugin name), and
#     records {name, marketplace: "", version, cache_path, enabled, source}
#     under the top-level installedPlugins list in ~/.copilot/config.json;
#   - installing over an installed (even disabled) plugin replaces and
#     re-enables it;
#   - uninstall of a plugin that is not installed exits 1;
#   - `copilot plugin uninstall <name>` is ambiguous: with a same-named
#     marketplace plugin it acts on that one (a local marketplace plugin is
#     disabled, the direct install stays). `<name>@` (empty marketplace)
#     targets only the direct install and leaves marketplace plugins alone.
# config.json is also copilot's sign-in file and an auth-file provision
# replaces it with the staged secret, so registration is read back from
# config.json rather than inferred from the cache dir.

COPILOT_PLUGINS_MIN_VERSION = (1, 0, 74)


class CopilotPluginInstaller(scion_harness.PluginInstaller):
    harness = "copilot"
    cli = "copilot"
    min_version = COPILOT_PLUGINS_MIN_VERSION

    def __init__(self, ctx: scion_harness.ProvisionContext):
        self.ctx = ctx
        self.copilot_home = os.path.join(ctx.home, ".copilot")

    def _env(self) -> dict[str, str]:
        # COPILOT_HOME pins the CLI to the directory this provisioner writes
        # (settings.json, config.json); auto-update is off so a plugin call
        # never downloads a CLI.
        return {**os.environ, "COPILOT_HOME": self.copilot_home, "COPILOT_AUTO_UPDATE": "false"}

    def _run(self, *args: str, timeout: float = scion_harness.AGENT_PLUGIN_CLI_TIMEOUT):
        return scion_harness.run_cli(["copilot", *args], timeout=self.cli_timeout(timeout), env=self._env())

    def detect_version(self) -> tuple[int, int, int] | None:
        # copilot 1.0.97 prints "GitHub Copilot CLI 1.0.97."
        proc = self._run("--version", timeout=scion_harness.AGENT_PLUGIN_VERSION_TIMEOUT)
        if proc.returncode != 0:
            return None
        return scion_harness.parse_semver(proc.stdout or proc.stderr or "")

    def prepare(self, desired: list[scion_harness.AgentPlugin]) -> None:
        os.makedirs(self.copilot_home, exist_ok=True)

    def registration(self, name: str, *, strict: bool = False) -> dict[str, Any] | None:
        """The direct-install entry for name in config.json installedPlugins,
        or None. Read-only.

        Lenient by default (is_present): a config.json that exists but
        cannot be parsed here (invalid UTF-8, an over-long integer, NaN, too
        deep) counts as not registered, so the plugin is reinstalled. With
        strict (install, uninstall), an unparseable file raises
        PluginInstallError instead: uninstall must not take it as removed
        (copilot's own parser may still list the plugin), and install
        reports the real cause. A missing file is "not registered" either
        way."""
        path = os.path.join(self.copilot_home, "config.json")
        try:
            with open(path, "rb") as f:
                data = _load_config_strictly(f.read())
        except FileNotFoundError:
            return None
        except (OSError, ValueError, RecursionError) as exc:
            if strict:
                raise scion_harness.PluginInstallError(
                    f"cannot read ~/.copilot/config.json to confirm the result ({type(exc).__name__})"
                ) from None
            return None
        plugins = data.get("installedPlugins") if isinstance(data, dict) else None
        if not isinstance(plugins, list):
            return None
        for entry in plugins:
            if (isinstance(entry, dict) and entry.get("name") == name
                    and not entry.get("marketplace")):
                return entry
        return None

    @staticmethod
    def enabled(entry: dict[str, Any] | None) -> bool:
        """The one "registered and enabled" predicate install and is_present
        share. Strict: copilot 1.0.97 always writes enabled: true; an entry
        without it fails the install loudly rather than reinstalling
        silently every start."""
        return isinstance(entry, dict) and entry.get("enabled") is True

    def direct_root(self) -> str:
        return os.path.join(self.copilot_home, "installed-plugins", "_direct")

    def _remove_direct_dir(self, name: str, path: Any) -> None:
        """Remove a leftover install dir, only when it is strictly inside
        installed-plugins/_direct (never the root, never outside it)."""
        if not isinstance(path, str) or not path or not os.path.lexists(path):
            return
        root = os.path.realpath(self.direct_root())
        real = os.path.realpath(path)
        if os.path.dirname(real) != root or os.path.islink(path):
            self.ctx.info(f"agent plugin {name}: not removing {path}: not a dir directly under {root}")
            return
        shutil.rmtree(real, ignore_errors=True)
        if os.path.lexists(real):
            raise scion_harness.PluginInstallError(f"cannot remove leftover install dir {real}")

    def _find_direct_dir(self, name: str) -> str:
        """Scan installed-plugins/_direct for a dir whose plugin.json names
        the plugin; "" when none does."""
        root = self.direct_root()
        try:
            entries = sorted(os.listdir(root))
        except OSError:
            return ""
        for entry in entries:
            d = os.path.join(root, entry)
            try:
                manifest = scion_harness.load_json(os.path.join(d, "plugin.json"))
            except (OSError, ValueError):
                continue
            if isinstance(manifest, dict) and manifest.get("name") == name:
                return d
        return ""

    def install(self, plugin: scion_harness.AgentPlugin, bundle_dir: str) -> dict[str, Any]:
        proc = self._run("plugin", "install", os.path.abspath(bundle_dir))
        if proc.returncode != 0:
            raise scion_harness.cli_failure("copilot plugin install", proc)
        try:
            entry = self.registration(plugin.name, strict=True)
        except scion_harness.PluginInstallError as exc:
            # Same outcome as "not registered" (not recorded, retried next
            # start), with the real cause in the warning.
            raise scion_harness.PluginInstallError(f"copilot plugin install exit 0, but {exc}") from None
        if not self.enabled(entry):
            raise scion_harness.PluginInstallError(
                f"copilot plugin install exit 0 but {plugin.name} is not registered "
                "as enabled in ~/.copilot/config.json")
        path = entry.get("cache_path")
        if not isinstance(path, str) or not path or not os.path.isdir(path):
            path = self._find_direct_dir(plugin.name)
        if not path:
            # Recording it would report "installed" while is_present stays
            # false, a silent reinstall every start.
            raise scion_harness.PluginInstallError(
                f"copilot plugin install registered {plugin.name} but no install dir was found "
                "(no cache_path, nothing under installed-plugins/_direct)")
        return {"name": plugin.name, "path": path}

    def uninstall(self, name: str, native_ref: dict[str, Any]) -> None:
        """copilot plugin uninstall <name>@ (the direct install only, never a
        user's same-named marketplace plugin). Success is judged by the
        result: the plugin must no longer be registered in config.json
        (copilot 1.0.97 exits 1 for a plugin that is not installed). A
        leftover install dir is then removed: when the registration was
        already gone (an auth-file start rewrites config.json), the CLI
        removes nothing."""
        proc = self._run("plugin", "uninstall", f"{name}@")
        try:
            entry = self.registration(name, strict=True)
        except scion_harness.PluginInstallError as exc:
            # Unknown outcome: keep the state record and the install dir,
            # and retry next start.
            raise scion_harness.PluginInstallError(
                f"copilot plugin uninstall exit {proc.returncode}, but {exc}") from None
        if entry is not None:
            if proc.returncode != 0:
                raise scion_harness.cli_failure("copilot plugin uninstall", proc)
            raise scion_harness.PluginInstallError(
                f"copilot plugin uninstall exit 0 but {name} is still registered in ~/.copilot/config.json")
        if proc.returncode != 0:
            self.ctx.info(
                f"agent plugin {name}: copilot plugin uninstall exit {proc.returncode}, "
                "plugin no longer registered")
        self._remove_direct_dir(name, native_ref.get("path"))

    def is_present(self, name: str, native_ref: dict[str, Any]) -> bool:
        """The install still exists and copilot still lists it as enabled.
        config.json can lose installedPlugins while the cache dir survives
        (an auth-file provision writes the staged config.json, a re-provision
        can reset the home); the plugin is then reinstalled. No CLI call."""
        entry = self.registration(name)
        if not self.enabled(entry):
            return False
        path = entry.get("cache_path") or native_ref.get("path")
        return isinstance(path, str) and bool(path) and os.path.isdir(path)


def provision(ctx: scion_harness.ProvisionContext) -> None:
    """Main provisioning logic for the Copilot harness."""

    # Auth selection. Copilot falls back to no-auth when selection fails
    # and no explicit type was requested (preserves pre-library behavior).
    try:
        resolved = ctx.select_auth(AUTH)
    except scion_harness.ProvisionError:
        if ctx.explicit_type:
            raise
        ctx.info("auth selection failed; falling back to no-auth mode")
        resolved = scion_harness.ResolvedAuth(method="none")

    env: dict[str, str] = {}
    extra: dict[str, Any] | None = None
    if resolved.method == "api-key" and resolved.env_key:
        secret = _read_token(ctx, resolved.env_key)
        if not secret:
            raise scion_harness.ProvisionError(
                f"chose api-key ({resolved.env_key}) but no secret "
                "value was staged at the recorded path; check ApplyAuthSettings"
            )
        env["COPILOT_GITHUB_TOKEN"] = secret

    if resolved.method == "auth-file":
        _write_copilot_config_file(ctx)
        extra = {"config_file_written": True}

    # --- Telemetry: inject native OTel env vars when enabled ----------------
    telemetry_payload = ctx.telemetry
    telemetry = telemetry_payload.get("telemetry") if isinstance(telemetry_payload, dict) else None
    env_overlay = telemetry_payload.get("env") if isinstance(telemetry_payload, dict) else None
    if not isinstance(env_overlay, dict):
        env_overlay = None

    telemetry_enabled = _telemetry_enabled(telemetry if isinstance(telemetry, dict) else None)
    if telemetry_enabled:
        otel_env = _build_telemetry_env(env_overlay)
        env.update(otel_env)
        ctx.info(f"telemetry: injected {len(otel_env)} OTel env var(s)")
    # SCION_NATIVE_TELEMETRY_POLICY lets the env guard (hooks/envoverlay.go)
    # protect the OTEL_* vars above from being overridden at runtime.
    env["SCION_NATIVE_TELEMETRY_POLICY"] = "enabled" if telemetry_enabled else "disabled"
    if telemetry_enabled:
        # Copilot's usage (gen_ai.api.calls / scion.usage.tokens) is derived
        # by sciontool's receiver from the native gen_ai.client.inference.*
        # metrics (design §5's copilot row, ptone/scion#2053 phase 3a). This
        # is narrow to usage only (D4): tool and session hook telemetry are
        # unaffected, and unset here means no usage is published at all
        # (D10).
        env["SCION_USAGE_SOURCE"] = "native"
    # --- end telemetry ----------------------------------------------------

    ctx.write_outputs(resolved, env=env, extra=extra)

    harness_cfg = ctx.harness_config
    instructions_file = harness_cfg.get('instructions_file') or '.copilot/copilot-instructions.md'
    target = os.path.join(ctx.home, instructions_file)
    os.makedirs(os.path.dirname(target), exist_ok=True)
    try:
        scion_harness.project_instructions(ctx, target)
    except OSError as exc:
        ctx.warn(f"failed to project instructions: {exc}")

    def translate_mcp(name: str, spec: dict[str, Any]) -> dict[str, Any] | None:
        transport = (spec.get("transport") or "").strip()

        if transport == "stdio":
            cmd = spec.get("command")
            if not isinstance(cmd, str) or not cmd:
                ctx.info(f"mcp server {name!r}: stdio transport missing command")
                return None
            out: dict[str, Any] = {"type": "local", "command": cmd}
            args = spec.get("args") or []
            if isinstance(args, list) and args:
                out["args"] = [str(a) for a in args]
            env_map = spec.get("env")
            if isinstance(env_map, dict) and env_map:
                out["env"] = {str(k): str(v) for k, v in env_map.items()}
            return out

        if transport in ("sse", "streamable-http"):
            url = spec.get("url")
            if not isinstance(url, str) or not url:
                ctx.info(
                    f"mcp server {name!r}: {transport} transport missing url"
                )
                return None
            out = {"type": "http", "url": url}
            headers = spec.get("headers")
            if isinstance(headers, dict) and headers:
                out["headers"] = {str(k): str(v) for k, v in headers.items()}
            return out

        ctx.info(f"mcp server {name!r}: unsupported transport {transport!r}")
        return None

    scion_harness.apply_mcp_translated(
        ctx, translate_mcp, lambda servers: _write_mcp_config(ctx, servers)
    )

    _write_hooks(ctx.home)

    try:
        _ensure_settings(ctx)
    except OSError as exc:
        ctx.warn(f"failed to write settings: {exc}")

    ctx.info(f"method={resolved.method}")

    # Last: `copilot plugin install` rewrites config.json (installedPlugins),
    # so it must be the final writer this start, after _ensure_settings.
    scion_harness.reconcile_agent_plugins(ctx, CopilotPluginInstaller(ctx))


if __name__ == "__main__":
    scion_harness.run("copilot", provision)
