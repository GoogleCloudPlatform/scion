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
"""Codex container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. The host-side
ContainerScriptHarness has already:

  * Staged this script and config.yaml under $HOME/.scion/harness/.
  * Written inputs/auth-candidates.json with the env-var names + paths to
    secret-value files under $HOME/.scion/harness/secrets/<NAME>.
  * Written inputs/telemetry.json describing the effective TelemetryConfig
    (the same struct ApplyTelemetrySettings receives).
  * Mounted any auth file (e.g. ~/.codex/auth.json) at the declared
    container_path, when auth-file mode is in use.

This script's job:

  1. Determine which auth method Codex will use, honoring an explicit
     selection if present and otherwise applying the same precedence:
         CodexAPIKey > OpenAIAPIKey > CodexAuthFile.
  2. For api-key methods, read the secret value and write
     `~/.codex/auth.json` with `{"auth_mode": "apikey", "OPENAI_API_KEY": "<value>"}`.
  3. Reconcile the [otel] section in `~/.codex/config.toml` from
     inputs/telemetry.json.
  4. Write outputs/resolved-auth.json describing the chosen method.
  5. Write outputs/env.json (intentionally empty).

The script is stdlib-only; it does manual TOML editing because tomllib (3.11+)
is read-only and we must avoid third-party dependencies. tomllib is still
used, read-only, as a post-edit validation backstop: _toml_edit_preserves
parses both the original and the finished config.toml and checks that
everything outside the keys this script owns (model, model_reasoning_effort,
otel) is unchanged, plus that those owned keys round-tripped to the values
just written. So a TOML construct the line-oriented editor doesn't fully
understand (e.g. a multi-line string whose body gets a key spliced into or
deleted from it) fails safe — the existing file is left untouched, with a
warning logged — instead of writing a subtly-altered file. If even a
telemetry-only edit doesn't preserve the file, the file is left completely
untouched.
"""

from __future__ import annotations

import json
import os
import sys
import tomllib
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness  # type: ignore[import-not-found]

assert scion_harness.INTERFACE_VERSION >= 2, (
    "codex provision.py requires scion_harness INTERFACE_VERSION >= 2; "
    f"got {scion_harness.INTERFACE_VERSION}"
)

CODEX_AUTH_FILE = "~/.codex/auth.json"
CODEX_CONFIG_FILE = "~/.codex/config.toml"

AUTH = scion_harness.AuthSpec(
    "codex",
    [
        scion_harness.env_method(
            "api-key",
            any_of=["CODEX_API_KEY", "OPENAI_API_KEY"],
            hint="set CODEX_API_KEY or OPENAI_API_KEY",
        ),
        scion_harness.file_method(
            "auth-file",
            path=CODEX_AUTH_FILE,
            hint=f"provide auth credentials at {CODEX_AUTH_FILE}",
            secret_key="CODEX_AUTH",
        ),
    ],
)


# --- Native auth writers ---------------------------------------------------


def _write_codex_auth_json(api_key: str) -> None:
    """Mirror the compiled ApplyAuthSettings: {"auth_mode": "apikey", ...}."""
    auth_dir = scion_harness.expand_path("~/.codex")
    os.makedirs(auth_dir, exist_ok=True)
    target = os.path.join(auth_dir, "auth.json")
    payload = {"auth_mode": "apikey", "OPENAI_API_KEY": api_key}
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(payload, f, indent=2)
        f.write("\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


def _write_codex_auth_file(ctx: scion_harness.ProvisionContext) -> None:
    """Write ~/.codex/auth.json from a staged CODEX_AUTH file secret."""
    auth_content = _read_file_secret(ctx, "CODEX_AUTH")
    if not auth_content:
        return
    if not auth_content.strip():
        raise scion_harness.ProvisionError("CODEX_AUTH secret is empty")
    try:
        json.loads(auth_content)
    except json.JSONDecodeError as exc:
        raise scion_harness.ProvisionError(
            f"CODEX_AUTH secret is not valid JSON: {exc}"
        ) from exc
    auth_dir = scion_harness.expand_path("~/.codex")
    os.makedirs(auth_dir, exist_ok=True)
    target = os.path.join(auth_dir, "auth.json")
    tmp = target + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(auth_content)
    os.chmod(tmp, 0o600)
    os.replace(tmp, target)


# --- TOML reconciliation (otel) --------------------------------------------


def _resolve_endpoint(telemetry: dict[str, Any] | None, env: dict[str, str] | None) -> str:
    env = env or {}
    port = str(env.get("SCION_OTEL_GRPC_PORT") or "4317")
    if not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise scion_harness.ProvisionError("invalid local telemetry gRPC port")
    return f"http://127.0.0.1:{port}"


def _resolve_otel_environment(telemetry: dict[str, Any], env: dict[str, str] | None) -> str:
    env = env or {}
    for key in ("SCION_CODEX_OTEL_ENVIRONMENT", "SCION_OTEL_ENVIRONMENT", "SCION_SERVER_ENV"):
        v = (env.get(key) or "").strip()
        if v:
            return v

    resource = telemetry.get("resource") or {}
    if isinstance(resource, dict):
        for key in ("deployment.environment", "service.environment", "environment"):
            v = str(resource.get(key) or "").strip()
            if v:
                return v

    return "production"


def _telemetry_enabled(telemetry: Any) -> bool:
    # ApplyTelemetrySettings only ever writes an object or null, so a
    # non-dict value can't reach here in production -- but treat anything
    # that isn't a dict (True, a string, a list, ...) the same as absent,
    # rather than crashing on `.get()`.
    if not isinstance(telemetry, dict) or not telemetry:
        return False
    enabled = telemetry.get("enabled")
    if enabled is None:
        return True
    return bool(enabled)


def _telemetry_output_env(telemetry: dict[str, Any] | None) -> dict[str, str]:
    """The telemetry-derived entries of outputs/env.json (ptone/scion#2053
    phase 3c). SCION_USAGE_SOURCE is set only when telemetry is enabled:
    codex's usage (gen_ai.api.calls / scion.usage.tokens) is derived by
    sciontool's receiver from the native codex.sse_event response.completed
    log event. This is narrow to usage only (D4): tool, session and turn
    hook telemetry are unaffected, and unset here means no usage is
    published at all (D10)."""
    enabled = _telemetry_enabled(telemetry)
    env = {"SCION_NATIVE_TELEMETRY_POLICY": "enabled" if enabled else "disabled"}
    if enabled:
        env["SCION_USAGE_SOURCE"] = "native"
    return env


def _telemetry_provider(telemetry: Any, env: dict[str, str] | None) -> str:
    """Resolves the configured telemetry cloud provider ("gcp", or "" when
    unset or some other provider). An explicit env override wins over the
    staged telemetry config, mirroring harnesses/claude/provision.py's
    provider resolution, minus claude's stricter "explicit provider
    required" validation, which is out of scope for codex."""
    env = env or {}
    # Same non-dict defensiveness as _telemetry_enabled above: a non-dict
    # telemetry (e.g. True) would otherwise crash `.get("cloud")`.
    cloud = telemetry.get("cloud") if isinstance(telemetry, dict) else None
    configured_provider = cloud.get("provider", "") if isinstance(cloud, dict) else ""
    staged_provider = env.get("SCION_TELEMETRY_CLOUD_PROVIDER", "")
    return staged_provider or configured_provider


def _build_otel_section(telemetry: dict[str, Any], env: dict[str, str] | None) -> str:
    endpoint = _resolve_endpoint(telemetry, env)
    environment = _resolve_otel_environment(telemetry, env)
    provider = _telemetry_provider(telemetry, env)

    exporter_key = "otlp-grpc"
    # Native metrics stay off on GCP, the same as claude (ptone/scion#2053
    # design §3.7 "codex" bullet): the GCP admission allowlist rejects
    # codex's raw metric attributes today, so forwarding them just to have
    # them dropped is pointless, and Codex's own usage now reaches the
    # dashboard through the native log-event deriver instead (see
    # _telemetry_output_env's SCION_USAGE_SOURCE). Logs and traces are
    # unaffected: this is narrow to metrics, the same as the usage-source
    # switch is narrow to usage.
    metrics_line = (
        'metrics_exporter = "none"'
        if provider == "gcp"
        else f'metrics_exporter."{exporter_key}".endpoint = "{scion_harness.toml_escape(endpoint)}"'
    )

    lines = [
        "[otel]",
        f'environment = "{scion_harness.toml_escape(environment)}"',
        "log_user_prompt = false",
        metrics_line,
        f'exporter."{exporter_key}".endpoint = "{scion_harness.toml_escape(endpoint)}"',
        f'trace_exporter."{exporter_key}".endpoint = "{scion_harness.toml_escape(endpoint)}"',
    ]

    return "\n".join(lines) + "\n"


def _resolve_reasoning_effort(level: int) -> str:
    """Map a thinking level (0-100) to OpenAI reasoning_effort (low/medium/high/xhigh)."""
    level = max(0, min(100, level))
    if level >= 76:
        return "xhigh"
    if level >= 51:
        return "high"
    if level >= 26:
        return "medium"
    return "low"


# The line-oriented TOML string/comment masking, bracket-depth tracking,
# header detection, top-level key strip/insert, and the tomllib
# round-trip/preservation backstop all live in scion_harness now (shared
# with grok-build, which has the same class of TOML-editing needs) — see
# scion_harness.strip_toml_top_level_key, .insert_toml_top_level_line, and
# .toml_edit_preserves. This module keeps only the codex-specific parts:
# the managed-key list and the model/effort value checks layered on top of
# the shared preservation check.

# Top-level keys this script owns the value of. _toml_edit_preserves ignores
# these when comparing the original file to an edited one — they're
# expected to change — and requires everything else to be byte-for-byte
# identical after a round trip through tomllib. `reasoning_effort` is
# included even though this script never writes it: _reconcile_codex_toml
# unconditionally strips it (a legacy key name it replaces with
# `model_reasoning_effort`), so a file that legitimately had it would
# otherwise always fail the "everything else is unchanged" comparison.
_MANAGED_TOP_LEVEL_KEYS = ("model", "model_reasoning_effort", "reasoning_effort", "otel")


def _toml_edit_preserves(
    original: str, content: str, model: str | None, effort: str | None
) -> bool:
    """True if `content` is a safe edit of `original`.

    "Safe" means: `content` parses as TOML; the keys this script just wrote
    (`model`, `model_reasoning_effort`) equal the values it meant to write,
    when those values were supplied; and every top-level key `content`
    doesn't own (i.e. not in _MANAGED_TOP_LEVEL_KEYS) is unchanged from
    `original` (scion_harness.toml_edit_preserves).

    This is the backstop for this module's line-oriented TOML editing,
    which — despite the string/comment masking and bracket-depth tracking
    in scion_harness — is still not a full TOML tokenizer. An earlier
    version of this function only checked "does it parse" and "is the
    top-level model correct", which passes even when a top-level
    multi-line string's body gets a `model`/`model_reasoning_effort`-shaped
    line spliced into or deleted from it: the file is still valid TOML, and
    when the edit was only inserting `model_reasoning_effort` (no `model`
    supplied), the 'model' check has nothing to catch it at all
    (ptone/scion#2365 review round 4). Comparing everything the script
    doesn't own closes that whole class of edit, not just the one shape a
    given review happened to try. Rather than trust every edit blindly, the
    caller verifies the finished content before writing it to disk, and
    leaves the existing file untouched (logging a warning) when this
    returns False.
    """
    try:
        after = tomllib.loads(content)
    except tomllib.TOMLDecodeError:
        return False
    if model and after.get("model") != model:
        return False
    if effort and after.get("model_reasoning_effort") != effort:
        return False
    return scion_harness.toml_edit_preserves(original, content, _MANAGED_TOP_LEVEL_KEYS)


def _warn_if_stale_top_level_model_survives(
    ctx: scion_harness.ProvisionContext, content: str, model: str | None, reason: str
) -> None:
    """Logs an observability warning when SCION_MODEL was empty (no model
    for this script to write) but `content` — whichever variant is about to
    be committed to disk — still has a top-level `model` key left over from
    a prior provision or a hand-edited file.

    `reason` names why, for *this specific call site*, the stale key wasn't
    removed — the three callers in _reconcile_codex_toml reach this for
    different causes (a header-shaped line hiding the real strip target on
    the main path; the whole model/effort edit being rejected on the two
    fallback paths — see each call site) and a single generic explanation
    would mislead on at least two of the three (ptone/scion#2365 review
    round 6, N2).
    It's purely observational, logging only, with no change to what gets
    written: rejecting the edit wouldn't help, since every path here keeps
    whatever `content` already has for keys it doesn't own, and this stale
    key is exactly such an unowned survivor. Without SCION_MODEL there's no
    `--model` argv either, so codex will actually run on this stale value —
    worth surfacing in provision logs even though nothing here can fix it.
    """
    if model:
        return
    try:
        parsed = tomllib.loads(content)
    except tomllib.TOMLDecodeError:
        return
    stale = parsed.get("model")
    if stale:
        ctx.info(
            f"config.toml still sets top-level model={stale!r} with no "
            f"SCION_MODEL to replace it ({reason}); codex will use it, "
            "since no --model argv is passed either."
        )


def _reconcile_codex_toml(
    ctx: scion_harness.ProvisionContext,
    telemetry: dict[str, Any] | None,
    env: dict[str, str] | None,
    reasoning_effort: str | None = None,
    model: str | None = None,
) -> None:
    codex_dir = scion_harness.expand_path("~/.codex")
    os.makedirs(codex_dir, exist_ok=True)
    config_path = os.path.join(codex_dir, "config.toml")
    original = ""
    if os.path.isfile(config_path):
        with open(config_path, "r", encoding="utf-8") as f:
            original = f.read()

    otel_section = (_build_otel_section(telemetry or {}, env) if _telemetry_enabled(telemetry)
                     else '[otel]\nexporter = "none"\nmetrics_exporter = "none"\ntrace_exporter = "none"\n')

    def _with_reconciled_otel(base: str) -> str:
        stripped = scion_harness.strip_toml_sections(base, lambda h: h == "[otel]" or h.startswith("[otel."))
        stripped = stripped.rstrip("\n\t ") + "\n\n" + otel_section
        return stripped.strip() + "\n"

    content = scion_harness.strip_toml_top_level_key(original, "reasoning_effort")
    content = scion_harness.strip_toml_top_level_key(content, "model_reasoning_effort")
    content = scion_harness.strip_toml_top_level_key(content, "model")

    if model:
        model_line = f'model = "{scion_harness.toml_escape(model)}"'
        content = scion_harness.insert_toml_top_level_line(content, model_line)

    if reasoning_effort:
        re_line = f'model_reasoning_effort = "{scion_harness.toml_escape(reasoning_effort)}"'
        content = scion_harness.insert_toml_top_level_line(content, re_line)

    content = _with_reconciled_otel(content)

    if _toml_edit_preserves(original, content, model, reasoning_effort):
        _warn_if_stale_top_level_model_survives(
            ctx,
            content,
            model,
            "a header-shaped line earlier in the file may be hiding the "
            "real table header from this script's line-oriented strip, so "
            "it never found this key",
        )
        scion_harness.atomic_write_text(config_path, content)
        return

    # The full edit (model/model_reasoning_effort strip+insert, plus the
    # otel swap) altered something it doesn't own — most likely a top-level
    # multi-line string that a header- or key-shaped line inside its body
    # got spliced into or stripped out of (see _toml_edit_preserves).
    # Retrying with *only* the otel swap, untouched by the model/effort
    # strip-and-insert steps, keeps telemetry reconciliation working even
    # when model/model_reasoning_effort can't be safely written to this
    # file: an explicit model still reaches codex via SCION_MODEL/--model
    # argv (pkg/agent/run.go) regardless of what config.toml says, so this
    # isn't a full feature loss — just this file not reflecting it.
    otel_only_content = _with_reconciled_otel(original)
    if _toml_edit_preserves(original, otel_only_content, None, None):
        ctx.info(
            "config.toml edit for model/model_reasoning_effort did not "
            "preserve existing content (top-level model/effort mismatch, "
            "or other content changed unexpectedly); applied telemetry "
            "settings only and left model/model_reasoning_effort "
            "unwritten in config.toml. codex still receives an explicit "
            "model via SCION_MODEL/--model argv when one is resolved. "
            "This can happen with hand-edited TOML this line-oriented "
            "editor doesn't fully understand, e.g. a multi-line string "
            "containing a header-shaped or key-shaped line."
        )
        _warn_if_stale_top_level_model_survives(
            ctx,
            otel_only_content,
            model,
            "the model/model_reasoning_effort edit above was rejected, so "
            "this script never attempted to strip this key from the "
            "original file",
        )
        scion_harness.atomic_write_text(config_path, otel_only_content)
        return

    ctx.info(
        "config.toml edit did not round-trip through tomllib safely even "
        "for the telemetry-only fallback; leaving config.toml completely "
        "untouched rather than risk corrupting or silently altering "
        "existing settings."
    )
    _warn_if_stale_top_level_model_survives(
        ctx,
        original,
        model,
        "both the model/model_reasoning_effort edit and the telemetry-only "
        "fallback above were rejected, so config.toml was left completely "
        "untouched and this key was never stripped",
    )


# --- MCP server emission ---------------------------------------------------


def _build_mcp_section(name: str, spec: dict[str, Any]) -> str | None:
    """Translate a universal MCPServerConfig into a TOML section. None on skip."""
    transport = (spec.get("transport") or "").strip()
    body: list[str] = [f"[mcp_servers.{name}]"]

    if transport == "stdio":
        cmd = spec.get("command")
        if not isinstance(cmd, str) or not cmd:
            print(f"codex provision: mcp server {name!r}: stdio transport missing command", file=sys.stderr)
            return None
        body.append(f'command = "{scion_harness.toml_escape(cmd)}"')
        args = spec.get("args") or []
        if isinstance(args, list) and args:
            body.append(f"args = {scion_harness.toml_string_array([str(a) for a in args])}")
        env = spec.get("env")
        if isinstance(env, dict) and env:
            body.append(f"env = {scion_harness.toml_inline_table({str(k): str(v) for k, v in env.items()})}")
    elif transport in ("sse", "streamable-http"):
        url = spec.get("url")
        if not isinstance(url, str) or not url:
            print(f"codex provision: mcp server {name!r}: {transport} transport missing url", file=sys.stderr)
            return None
        body.append(f'url = "{scion_harness.toml_escape(url)}"')
        headers = spec.get("headers")
        if isinstance(headers, dict) and headers:
            body.append(f"http_headers = {scion_harness.toml_inline_table({str(k): str(v) for k, v in headers.items()})}")
    else:
        print(f"codex provision: mcp server {name!r}: unsupported transport {transport!r}", file=sys.stderr)
        return None

    return "\n".join(body) + "\n"


def _write_mcp_to_config(ctx: scion_harness.ProvisionContext, servers: dict[str, str]) -> None:
    """Write translated MCP server sections into ~/.codex/config.toml."""
    codex_dir = scion_harness.expand_path("~/.codex")
    os.makedirs(codex_dir, exist_ok=True)
    config_path = os.path.join(codex_dir, "config.toml")
    original = ""
    if os.path.isfile(config_path):
        with open(config_path, "r", encoding="utf-8") as f:
            original = f.read()
    content = scion_harness.strip_toml_sections(
        original, lambda h: h.startswith("[mcp_servers.")
    )
    sections = list(servers.values())
    appended = "\n".join(sections)
    content = content.rstrip("\n\t ") + "\n\n" + appended
    content = content.strip() + "\n"
    scion_harness.write_toml_if_preserves(
        ctx, config_path, original, content,
        managed_keys={"mcp_servers"}, what="MCP server registration",
    )


# --- Entry point -----------------------------------------------------------


def _read_env_secret(ctx: scion_harness.ProvisionContext, name: str) -> str:
    """Read an env secret, expanding $HOME in the staged path."""
    path = ctx.env_secret_files.get(name, "")
    if not path:
        return ""
    path = scion_harness.expand_path(path)
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read().rstrip("\r\n")
    except OSError:
        return ""


def _read_file_secret(ctx: scion_harness.ProvisionContext, name: str) -> str:
    """Read a file-type secret, expanding $HOME in the staged path."""
    path = ctx.file_secret_files.get(name, "")
    if not path:
        return ""
    path = scion_harness.expand_path(path)
    try:
        with open(path, "r", encoding="utf-8") as f:
            return f.read().rstrip("\r\n")
    except OSError:
        return ""


def provision(ctx: scion_harness.ProvisionContext) -> None:
    resolved = ctx.select_auth(AUTH)

    if resolved.method == "api-key":
        api_key = _read_env_secret(ctx, resolved.env_key)
        if not api_key:
            raise scion_harness.ProvisionError(
                f"chose api-key ({resolved.env_key}) but no secret value "
                "was staged at the recorded path; check ApplyAuthSettings"
            )
        _write_codex_auth_json(api_key)

    if resolved.method == "auth-file":
        _write_codex_auth_file(ctx)

    harness_cfg = ctx.harness_config
    instructions_file = str(harness_cfg.get("instructions_file") or ".codex/AGENTS.md")
    scion_harness.project_instructions(ctx, instructions_file)

    thinking_raw = os.environ.get("SCION_THINKING_LEVEL", "").strip()
    reasoning_effort: str | None = None
    if thinking_raw:
        try:
            thinking_level = int(thinking_raw)
            reasoning_effort = _resolve_reasoning_effort(thinking_level)
            ctx.info(f"thinking_level={thinking_level} reasoning_effort={reasoning_effort}")
        except ValueError:
            pass

    telemetry_payload = ctx.telemetry
    telemetry = telemetry_payload.get("telemetry") if isinstance(telemetry_payload, dict) else None
    env_overlay = telemetry_payload.get("env") if isinstance(telemetry_payload, dict) else None
    if not isinstance(env_overlay, dict):
        env_overlay = None

    # SCION_MODEL arrives already resolved by the Go side (pkg/agent/provision.go
    # and pkg/hub/handlers_agent_create_helpers.go resolve size aliases before
    # the container starts). Write it into config.toml so it isn't silently
    # shadowed by a static `model` baked into the harness home image
    # (ptone/scion#2365).
    model = os.environ.get("SCION_MODEL", "").strip()
    if model:
        ctx.info(f"model={model}")
    else:
        ctx.info("model=<unset>, falling back to codex's own built-in default")

    _reconcile_codex_toml(
        ctx,
        telemetry if isinstance(telemetry, dict) else None,
        env_overlay,
        reasoning_effort=reasoning_effort,
        model=model or None,
    )

    extra: dict[str, Any] | None = None
    if resolved.method == "auth-file":
        extra = {"auth_file_written": True}
    env: dict[str, str] = {"CODEX_HOME": os.path.join(ctx.home, ".codex")}
    env.update(_telemetry_output_env(telemetry))
    ctx.write_outputs(resolved, env=env, extra=extra)

    scion_harness.apply_mcp_translated(
        ctx, _build_mcp_section, lambda servers: _write_mcp_to_config(ctx, servers)
    )

    ctx.info(f"method={resolved.method}")


if __name__ == "__main__":
    scion_harness.run("codex", provision)
