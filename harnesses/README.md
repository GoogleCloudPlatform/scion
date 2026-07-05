# Opt-In Harness Bundles

Self-contained harness configuration bundles for coding agents that are **not
installed by default**. The default-install set is `{claude, gemini}` — these
bundles are opt-in and can be installed with a single command.

Each bundle includes everything needed to run the harness: configuration
(`config.yaml`), a container-side provisioner (`provision.py`), a Dockerfile,
and a Cloud Build configuration.

## Available Bundles

| Bundle | Description | Install |
|--------|-------------|---------|
| [opencode](opencode/README.md) | [OpenCode](https://opencode.ai) AI coding assistant | `scion harness-config install harnesses/opencode` |
| [codex](codex/README.md) | [Codex](https://github.com/openai/codex) OpenAI coding agent CLI | `scion harness-config install harnesses/codex` |
| [antigravity](antigravity/README.md) | [Antigravity](https://github.com/ptone/scion-antigravity) Gemini-based coding agent via OAuth | `scion harness-config install harnesses/antigravity` |

Or install directly from GitHub (no local checkout needed):

```sh
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/<name>
```

## Bundle Layout

Each bundle directory contains:

```
<name>/
  config.yaml       # Harness configuration (provisioner, capabilities, auth)
  provision.py       # Container-side provisioner (pre-start hook)
  Dockerfile         # Image build (FROM scion-base)
  cloudbuild.yaml    # Cloud Build configuration
  README.md          # Bundle-specific docs (auth modes, build instructions)
  home/              # Home directory files seeded at install time
```

## Migrating Existing Installs

If you previously had opencode or codex harness configs installed (from
when they were part of the default set), here's what you need to know:

1. **Already on `provisioner.type: container-script`** — no action needed.
   Your existing config keeps working exactly as before. This is the case
   for any config that was upgraded or installed after container-script
   provisioning was introduced.

2. **Legacy config on `provisioner.type: builtin`** — the compiled-in Go
   implementation has been removed. Run the upgrade command to switch to
   container-script provisioning:
   ```sh
   scion harness-config upgrade <name> --activate-script
   ```
   If your config directory contains a `provision.py`, the upgrade
   auto-activates container-script provisioning even without the
   `--activate-script` flag. If no `provision.py` exists, reinstall
   from the bundle:
   ```sh
   scion harness-config install harnesses/<name>
   ```

3. **Fresh installs** — opencode, codex, and antigravity are no longer
   installed automatically. Restore any of them with a single command:
   ```sh
   scion harness-config install harnesses/opencode
   scion harness-config install harnesses/codex
   scion harness-config install harnesses/antigravity
   ```

4. **Existing agents are unaffected** — no agent-home rewrites are
   performed. Already-created agents continue to work with their
   existing harness-config directories.

## Writing a New Harness

### Bundle layout

```
<name>/
  config.yaml          # Harness config (provisioner, capabilities, auth)
  provision.py         # Container-side provisioner (required)
  capture_auth.py      # Credential capture script (optional)
  scion_harness.py     # Vendored copy of the shared library (generated)
  Dockerfile           # Image build (FROM scion-base)
  cloudbuild.yaml      # Cloud Build configuration
  README.md            # Bundle-specific docs
  home/                # Home directory files seeded at install time
```

### provision.py skeleton

Every provision.py should use the shared `scion_harness` library:

```python
#!/usr/bin/env python3
import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import scion_harness
assert scion_harness.INTERFACE_VERSION >= 2

AUTH = scion_harness.AuthSpec(
    "<name>",
    [
        scion_harness.env_method("api-key", any_of=["MY_API_KEY"]),
        # scion_harness.file_method("auth-file", path="~/.myapp/creds.json"),
    ],
)

def provision(ctx: scion_harness.ProvisionContext) -> None:
    auth = ctx.select_auth(AUTH)
    # harness-native setup here (config files, MCP, etc.)
    ctx.write_outputs(auth, env={"MY_API_KEY": "${MY_API_KEY}"})

if __name__ == "__main__":
    scion_harness.run("<name>", provision)
```

Key API:
- `ctx.select_auth(spec)` — declarative auth selection with explicit-type
  validation, precedence, no-auth gate, error handling
- `ctx.write_outputs(auth, env=...)` — writes resolved-auth.json and env.json
- `ctx.read_secret(name)` — reads a staged secret with whitespace normalization
- `scion_harness.project_instructions(...)` — managed-block instruction projection
- `scion_harness.apply_mcp_translated(ctx, translate_fn, write_fn)` — MCP
  translation with warn-and-skip error handling
- `scion_harness.apply_mcp_servers_simple(...)` — simple MCP merge into
  dotted-path config files

### Vendoring `scion_harness.py`

The canonical source is `harnesses/scion_harness.py`. Each bundle gets a
vendored copy generated by `go generate ./harnesses/gen/...`. The copy is
byte-identical except for a `# GENERATED` header line. The drift-gate test
(`TestVendoredLibMatchesCanonical`) enforces this — CI fails if any vendored
copy drifts from the canonical.

After editing `harnesses/scion_harness.py`, regenerate vendored copies:
```sh
go generate ./harnesses/gen/...
```

### `provisioner.lib` in config.yaml

The `provisioner.lib` field in `config.yaml` controls how `scion_harness.py`
is staged in the container:

- `vendored` (default) — the bundled `scion_harness.py` is used as-is.
  This is the normal mode for installed harnesses.
- `injected` — the Go host injects the embedded canonical copy at provision
  time. Used for development or when the bundle lacks a vendored copy.

### INTERFACE_VERSION contract

- `scion_harness.py` declares `INTERFACE_VERSION` (currently `2`).
- Within a version: additive-only changes (new functions, new keyword args
  with defaults). Breaking changes bump the version.
- `config.yaml` declares `provisioner.interface_version` so the Go host can
  detect mismatches between the binary and the bundle.
- provision.py should assert: `assert scion_harness.INTERFACE_VERSION >= 2`

### Golden fixture requirements

Every harness must ship at least 4 golden fixture cases under
`pkg/harness/testdata/bundle_contract/<name>/`. Each case is a directory
containing:
- `input.json` — `{"auth_candidates": {...}, "harness_config": {...}}`
- `want.json` — `{"exit_code": N, "resolved_auth": {...}, "env": {...}}`

Required cases: `api_key`, `no_auth_mode`, `no_creds_error`, `explicit_invalid`.
The bundle contract test (`TestBundleContract`) runs provision.py against each
fixture and asserts the outputs match. `TestBundleContractCoverage` enforces
the minimum fixture count.

## Future Work

A `scion harness-config list --available` command to discover installable
bundles programmatically is a planned follow-up.
