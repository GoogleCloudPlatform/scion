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

## Future Work

A `scion harness-config list --available` command to discover installable
bundles programmatically is a planned follow-up.
