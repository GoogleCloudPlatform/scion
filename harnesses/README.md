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

## Future Work

A `scion harness-config list --available` command to discover installable
bundles programmatically is a planned follow-up.
