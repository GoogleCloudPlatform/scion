---
title: Agent Plugins (Experimental)
description: Declare Agent Plugins 1.0.0 bundles in a template and have Scion install them natively into codex, copilot, muse-code and claude agents.
---

:::caution[Experimental]
Agent Plugins support is behind the `hub.agent_plugins` experiment, which is **off by default**. While it is off, Scion ignores `agent_plugins` in templates. The behaviour, messages and file locations on this page may change.
:::

An **Agent Plugin** is a bundle of agent extensions in the open [Agent Plugins](https://github.com/agentplugins/agent-plugins-spec) format. Scion supports version **1.0.0** of the format. A bundle is a directory with:

- `plugin.json`: the manifest (required). It holds the plugin's `name`, `version` and `description`.
- `skills/`: one or more [Agent Skills](https://agentskills.io/home) folders, each with a `SKILL.md`.
- `mcp.json`: MCP servers for the plugin (optional).
- Any other files the skills or MCP servers need, such as executables under `bin/`.

You declare plugins in a template. When Scion provisions an agent from that template, it validates and copies each bundle into the agent. The harness then installs it with its own plugin mechanism (for example `codex plugin add` or `copilot plugin install`).

### How Agent Plugins differ from Scion skills

| | [Scion skills](/scion/local/skills/) | Agent Plugins |
| :--- | :--- | :--- |
| Unit | One skill folder | A whole bundle: skills, MCP servers and support files |
| Delivery | Copied into the harness's skills directory | Installed through the harness's own plugin mechanism |
| Naming | The skill folder name | The harness namespaces plugin skills, for example `scion-smoke:smoke-hello` in claude or `plugin:scion-smoke:smoke-hello` in muse-code |
| Harnesses | Every harness with a skills directory | codex, copilot, muse-code and claude (see the [support matrix](#harness-support)) |
| Sources | A template's `skills/` directory, or the Skill Bank | Template-relative paths only |

## Turning the experiment on

Agents are started through a Hub (this includes the workstation [combo server](/scion/workstation/workstation-server/)), and only the Hub decides. An admin turns on **Agent Plugins (native install)** (`hub.agent_plugins`) under **Admin → Server Config → Experiments**; see [Experiments](/scion/reference/experiments/). The Hub sends the value to the Runtime Broker with every create and start. A Runtime Broker never reads its own `settings.yaml` for this, so a Runtime Broker operator cannot turn the feature on for a Hub that has it off. The `local_experiments` setting of the removed local-only mode has no effect (see [Local-only mode removal](/scion/reference/local-mode-removal/)).

**Turning it off.** Scion checks the experiment when it provisions an agent **and at every start**. If it is off at a start, Scion stages no plugins for that start, and the harness removes the plugins that Scion installed earlier. The start prints:

```text
agent_plugins: ignored: experiment hub.agent_plugins is off; plugins Scion installed earlier, if any, are removed
```

Bundles staged while the experiment was on stay in the agent's control-plane record, so turning the experiment back on reinstalls them at the next start, without re-provisioning. An agent that was provisioned while the experiment was off has nothing staged: it gets plugins only when it is provisioned again (see [When changes take effect](#when-changes-take-effect)).

## Declaring plugins in a template

Put each bundle inside the template directory, and list it under `agent_plugins` in the template's `scion-agent.yaml`:

```text
my-template/
├── scion-agent.yaml
└── plugins/
    └── scion-smoke/
        ├── plugin.json
        ├── mcp.json
        ├── bin/
        │   └── stdio-mcp
        └── skills/
            └── smoke-hello/
                └── SKILL.md
```

```yaml
# my-template/scion-agent.yaml
agent_plugins:
  - path: plugins/scion-smoke
  - path: plugins/extra-tools
    optional: true
```

`plugin.json`:

```json
{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "scion-smoke",
  "version": "0.1.0",
  "description": "Scion smoke fixture"
}
```

`mcp.json`:

```json
{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {
    "smoke-stdio": {
      "type": "stdio",
      "command": "./bin/stdio-mcp",
      "args": ["--data", "${PLUGIN_DATA}/x"]
    },
    "smoke-http": {
      "type": "streamable-http",
      "url": "http://127.0.0.1:8765/mcp"
    }
  }
}
```

The fields of an `agent_plugins` entry:

| Field | Meaning |
| :--- | :--- |
| `path` | Required. The bundle directory, relative to the directory of the template that declares the entry. A leading `./` is allowed. Absolute paths, `..` components and symlinked directories are rejected. |
| `optional` | When `true`, a plugin that cannot be loaded is skipped with a warning. When `false` (the default), it fails provisioning, as a required skill does. |
| `scope` | Set by Scion when it merges templates (`template`). It is informational only. |

### Rules to know

- **The installed name is the manifest `name`,** not the directory name. Names are 1–64 characters of lowercase letters, digits, `.` and `-`; they start and end with a letter or digit and do not contain `--` or `..`.
- **Each entry resolves against its own template.** In a template chain, a child template's entry resolves inside the child template's directory, never in the parent's.
- **Same name in two templates: the later template wins.** If a parent and a child template both provide a plugin named `acme`, the child's is installed, with a warning.
- **No fallback to the parent.** If the child's plugin replaces the parent's and then fails to stage (for example, it is marked `optional` and exceeds a size limit), the plugin is skipped. Scion does not fall back to the parent template's plugin of the same name. (A child bundle whose `plugin.json` cannot be read has no known name, so it does not replace anything.)
- **Template entries only.** Phase 1 installs only plugins declared in templates. An `agent_plugins` entry that comes from somewhere else, such as an inline agent config, is ignored with a warning.
- **The persisted list is informational.** An agent's stored config (`scion-agent.json`) contains a merged `agent_plugins` list. It is **not** the effective set of plugins: a persisted entry keeps its old fields after a template edits them, and entries that a template dropped can stay in it. Scion always re-reads each template's own `agent_plugins` when it provisions an agent. To see what is actually installed, read the [status file](#where-plugins-end-up).

### When changes take effect

| Change | Takes effect |
| :--- | :--- |
| Edit a bundle, or add or remove an `agent_plugins` entry | When the agent is provisioned again: delete the agent and create it again. On a Hub you can also use [`scion reincarnate`](/scion/reference/cli/#scion-reincarnate), except for agents in worktree-per-agent workspaces. A plain restart or resume reuses the bundles staged at provisioning. |
| Turn the experiment off or on | At the next start of the agent. |
| Switch harness-config at start (`--harness-config`) | At that start. Bundles are staged even for a harness that cannot install them, so the new harness installs them from the record. The old harness's install stays in the old harness's directories. |
| Restart with no change | No plugin work: the harness checks a few files and makes no plugin CLI call. |

A plugin that a user removes by hand inside the agent is reinstalled at the next start. A plugin that a user disables in copilot or muse-code is re-enabled at the next start. Plugins a user installs themselves are never touched: Scion only removes plugins that it recorded as installed.

## Harness support

| Harness | Support | Minimum CLI | Tested with¹ | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `codex` | partial | 0.147.0 | 0.162.1 | Installed through a Scion-owned local marketplace. Codex skips `sse` MCP servers. |
| `copilot` | yes | 1.0.74 | 1.0.97 | Installed with `copilot plugin install <dir>`. See the [copilot notes](#copilot). |
| `muse-code` | partial | 1.4.6 | 1.4.6, 1.4.7; 1.4.9 (live check) | Skills install natively with `muse plugins install`. muse 1.4.9 and earlier keeps Agent Plugins MCP servers **inactive**: they are listed but never started. |
| `claude` | partial | 2.1.157 (approximate) | 2.1.296 | Installed as a skills-dir plugin with a generated manifest. See the [claude notes](#claude). |
| `gemini-cli`, `grok-build`, `antigravity`, `hermes`, `opencode`, `pi` | no | — | — | Bundles are staged but not installed. The start prints a warning. |

¹ CLI-level smoke tests: the real harness CLI driven by Scion's installer with staged inputs. None of the four harnesses has yet been verified end to end in a Scion-provisioned agent; for claude in particular, the path through `scion start` is not yet verified.

If the harness CLI in the image is older than the minimum, the harness skips plugin install with a warning and changes nothing. The claude minimum is approximate: only 2.1.296 has been tested.

Each harness-config declares its support level in an `agent-plugins.yaml` file next to its `config.yaml`, for example:

```yaml
support: "partial"
reason: "Native install via local marketplace; Codex skips sse MCP servers"
```

The file belongs to the harness-config's provisioner: `scion harness-config reset` and `scion harness-config upgrade` refresh it together with `provision.py`. A harness-config without it (and without the deprecated key below) is reported as predating Agent Plugins support. If the file cannot be read, does not parse or has a support level this Scion version does not know, Scion logs a warning and treats the harness as not supporting Agent Plugins, with the error as the reason; the harness itself stays usable. Harness-configs from the first Agent Plugins release declared the level as `capabilities.agent_plugins` in `config.yaml`; Scion still reads that key when there is no `agent-plugins.yaml`, but it is deprecated, because hubs, brokers and CLIs built before Agent Plugins reject a `config.yaml` that contains it.

### codex

- Scion keeps a local marketplace named `scion-agent-plugins` in `~/.scion/agent-plugins/` and runs `codex plugin marketplace add`, `codex plugin add <name>@scion-agent-plugins` and `codex plugin remove`. The local marketplace path does not clone or download the bundle; a fully offline install is not yet verified end to end.
- `sse` MCP servers are skipped, with the warning `agent plugin <name>: codex skips sse server(s): <list>` in `agent.log`.
- The `codex` harness-config's `provisioner.timeout` is 60s to leave room for plugin installs.

### copilot

- Scion runs `copilot plugin install ~/.scion/agent-plugins/bundles/<name>` and removes with `copilot plugin uninstall <name>@` (only the direct install, never a marketplace plugin of the same name).
- **Direct local-path installs are deprecated by copilot.** copilot 1.0.97 prints `Direct plugin installs (repos, URLs, local paths) are deprecated` on every install. The route works today. If a later copilot release drops it, the install fails with a warning and the agent starts without the plugin. The fallback is a local marketplace, as for codex.
- **Agents that sign in with an auth file reinstall every plugin on every start.** On those agents, provisioning rewrites copilot's `config.json` from the staged credential at each start, which drops copilot's plugin registration. Each reinstall takes about 1 second of CLI time. Plugin work has a 20-second budget per start, so with about 20 or more plugins, the same plugins at the end of the list are deferred at every start (`agent plugins: time budget reached; N plugin(s) deferred to next start`). Agents that use other sign-in methods take the no-CLI fast path after the first start.
- The `copilot` harness-config's `provisioner.timeout` is 60s.

### muse-code

- Scion runs `muse plugins install <dir> --scope user --json`, and `muse plugins remove <name> --json` to remove. It runs `muse plugins approve` or `muse plugins enable` only when the install result asks for it.
- muse's own diagnostics are copied to `agent.log`. On muse 1.4.9 and earlier, every MCP server of a plugin produces `Agent Plugins MCP server "<s>" is not supported in this release; it stays visible and inactive`.
- muse refuses a bundle that has no usable skill, such as an MCP-only bundle.
- The installer reads muse's registry under `$XDG_DATA_HOME/muse/plugins/` (default `~/.local/share/muse/plugins/`). A template `env` that sets `XDG_DATA_HOME` is not visible to the installer, so plugins would be installed in the default location while muse looks in the other one. Do not set `XDG_DATA_HOME` for muse-code agents that use plugins.
- The `muse-code` harness-config's `provisioner.timeout` is 60s.

### claude

Claude has no plugin install command for local bundles, so Scion writes the plugin as a **skills-dir plugin**:

- The bundle is copied to `~/.claude/skills/<name>/` (the harness-config's `skills_dir`), and Scion writes a generated `.claude-plugin/plugin.json` there. Claude loads it as `<name>@skills-dir`, and its skills appear as `<name>:<skill>`.
- MCP servers are written into the generated manifest, translated from `mcp.json`:
  - stdio servers run through a small `/bin/sh` wrapper that changes to the server's `cwd` (the plugin root by default, as the spec requires) before it runs the command. Claude has no `cwd` field of its own.
  - `${PLUGIN_ROOT}` becomes Claude's `${CLAUDE_PLUGIN_ROOT}`. `${PLUGIN_DATA}` becomes `~/.scion/agent-plugins/data/<name>/`, which Scion creates (mode 0700) and keeps when the plugin is removed.
  - `streamable-http` servers become Claude `http` servers; `sse` servers stay `sse`.
  - **Skip rule:** Claude expands `${VAR}` text in MCP config, but the spec requires unknown placeholder text to stay literal. A server whose `args`, `env` or `cwd` contains `${` other than an exact `${PLUGIN_ROOT}` or `${PLUGIN_DATA}`, or whose `command`, `url` or header values contain any `${`, is skipped with `mcp server <s> skipped for claude: …`. The plugin's other servers still load.
- **Claude-specific files in a bundle are not installed.** Claude loads these from a plugin root by default, and they would bypass Scion's validation. Each one a bundle ships is removed from the installed copy, with a warning (`ignored claude-specific <path> in bundle`):
  - `.mcp.json`, `.lsp.json`, `settings.json`;
  - `hooks/`, `commands/`, `agents/`, `monitors/`, `output-styles/`, `workflows/`, `themes/`;
  - `.claude-plugin/` (replaced by the generated manifest).

  A root `SKILL.md` is not installed either, with its own warning: `agent plugin <name>: ignored root SKILL.md in bundle (Agent Plugins skills live under skills/)`. Agent Plugins skills live under `skills/`.

  An MCP server whose command, cwd, args or env point into a removed path is skipped (`mcp server <s> skipped for claude: it uses <path>, which is not installed for claude`).
- **Name conflicts with skills.** If a template, platform or Skill Bank skill has the same name as a plugin, the plugin is not installed for claude (`plugin <name> not installed for claude: a skill named <name> is already installed in the skills dir`). A directory in `~/.claude/skills/` that Scion did not install is never replaced or removed (`… exists and was not installed by Scion`).
- **Scion removes only what it wrote.** Scion records every file it installs, with its checksum, in `~/.scion/agent-plugins/state-claude.json`. On removal or update it deletes only recorded files whose content is unchanged.
- **Presence check.** At each start, Scion checks that the generated `.claude-plugin/plugin.json` and the bundle's root `plugin.json` are still in place and unchanged (by checksum), and that `~/.scion/agent-plugins/data/<name>/` exists.
  - If either manifest or the data dir is missing, the plugin is installed again.
  - If the generated manifest was changed, Scion gives up the directory (see the next item).
  - If the root `plugin.json` was edited, the reinstall carries the edited file over like runtime output, so it is blocked at that path (see below) and the old copy stays.
- **When Scion gives up a directory.** Only signs that something else now lives in the plugin directory make Scion hand it back: a skill with the same name (see above), a root `SKILL.md`, or a `.claude-plugin/` that is a symlink or whose `plugin.json` is not the one Scion generated. Scion then removes only its own files, drops the plugin from its state, and warns (`… removed only the files Scion installed`).
- **Runtime output is kept.** Other files that appear in the plugin directory, such as `__pycache__/` or logs that an MCP server writes, and installed files that were edited, are treated as runtime output and never deleted:
  - On an update they are carried into the new copy. If one sits at a path the new version also uses, the update is blocked and the old version stays installed (`plugin <name> not updated for claude: … move them away to update`).
  - On removal they stay behind in a directory that no longer has a plugin manifest, which Claude does not load. Delete `~/.claude/skills/<name>/` by hand if you want it gone.
- The `claude` harness-config's `provisioner.timeout` stays at 30s: the install is a file write with no extra CLI call.

## Where plugins end up

Inside the agent container:

| Path | Contents |
| :--- | :--- |
| `~/.scion/harness/inputs/plugins/<name>/`, `~/.scion/harness/inputs/plugins.json` | The bundles Scion staged for this start, and their index. Restored from the agent's control-plane record at every start. |
| `~/.scion/agent-plugins/bundles/<name>/` | Scion's copy of each bundle, the source codex, copilot and muse-code install from (claude copies from the staged inputs). |
| `~/.scion/agent-plugins/state-<harness>.json` | The plugins Scion installed for that harness. Scion only ever removes plugins listed here. |
| `~/.scion/agent-plugins/status-<harness>.json` | The result of the last start: `installed`, `stale` (an older version kept after a failed upgrade, or a removal deferred to the next start), `skipped` (each with a reason), `removed`, `cli_version` and `updated_at`. |
| `~/.scion/agent-plugins/.agents/plugins/marketplace.json` | codex only: the `scion-agent-plugins` local marketplace. |
| `~/.scion/agent-plugins/data/<name>/` | claude only: the plugin's `${PLUGIN_DATA}` directory. |
| `~/.scion/agent-plugins/pending-claude.json` | claude only: an intent record written just before a plugin directory is swapped in. If provisioning stops before the state file is saved, the next start uses it to adopt the new copy or keep the old one. See [Claude recovery](#claude-recovery) for the case it does not cover. |

The native install, per harness:

| Harness | Location |
| :--- | :--- |
| `codex` | `~/.codex/plugins/cache/scion-agent-plugins/<name>/<version>/`, enabled in `~/.codex/config.toml` |
| `copilot` | `~/.copilot/installed-plugins/_direct/<name>/`, registered in `~/.copilot/config.json` (`installedPlugins`) |
| `muse-code` | `~/.local/share/muse/plugins/cache/local/<name>/…`, registered in `~/.local/share/muse/plugins/installed.json` |
| `claude` | `~/.claude/skills/<name>/`, with the generated `.claude-plugin/plugin.json` |

## Troubleshooting

Plugin problems show up in two places.

### Start warnings

Scion's own checks run when it provisions or starts the agent. Their messages start with `agent_plugins:` and appear in the `scion create` / `scion start` output, and with the agent's warnings on a Hub.

| Message | Meaning |
| :--- | :--- |
| `ignored: experiment hub.agent_plugins is off` | Plugins are declared (by a template or inline), but the experiment is off. Nothing is staged. |
| `ignored: experiment hub.agent_plugins is off; plugins Scion installed earlier, if any, are removed` | Same, at a start of an agent that has staged plugins. |
| `ignored: harness "<h>" is not a container-script harness` | The harness-config does not use a container-script provisioner, so it cannot install plugins. |
| `entry "<path>" ignored: only template-relative paths are supported` | The entry does not come from a template. |
| `template <t>: path "<p>" must be relative to the template directory` | The path is absolute, has `..`, or leaves the template directory. |
| `template <t>: plugin dir "<p>" not found or is a symlink` | The directory is missing, is not a directory, or is (or sits under) a symlink. |
| `template <t>: plugin dir "<p>": <error>` | Any other error opening the plugin directory, such as an unreadable template directory. |
| `plugin at "<p>": plugin.json …` | `plugin.json` is missing, not a regular file, not valid JSON, has a field of the wrong type, or has an invalid name. |
| `plugin "<n>": unsupported Agent Plugins version "<x>"; Scion supports 1.0.0` | The manifest's `$schema` is not the 1.0.0 schema. |
| `plugin "<n>": bundle exceeds <limit>` | The bundle is over 10,000 entries (files, directories and skipped symlinks), 100 MiB, or a directory depth of 32. |
| `plugin "<n>": skipped <k> symlink(s) or special file(s) (not followed): …` | Those entries were not copied. |
| `plugin "<n>": unknown manifest field "<f>" ignored` | Unknown `plugin.json` fields are ignored. |
| `plugin "<n>": manifest field "extensions" is not an object; ignored` | `extensions` must be a JSON object; anything else is ignored. |
| `plugin "<n>": MCP disabled: <reason>` | `mcp.json` is invalid as a whole, so none of the plugin's MCP servers is installed. Its skills still are. |
| `plugin "<n>": mcp server "<s>" skipped: <reason>` | One server is invalid; the others are kept. |
| `plugin "<n>": skills ignored: <reason>` | `skills/` is not a usable directory. |
| `plugin "<n>": from template <t2> replaces the one from <t1>` | Two templates provide the same plugin name; the later one wins. |
| `harness "<h>" does not support Agent Plugins; plugins staged but not installed (<reason>)` | The harness has no plugin support (see the [matrix](#harness-support)). |
| `harness-config "<h>" predates Agent Plugins support; plugins staged but not installed (run scion harness-config reset <h> for the bundled defaults, or scion harness-config upgrade <h> to keep local edits)` | The installed harness-config is older than plugin support. See [Old harness-configs](#old-harness-configs). |

The messages in the first column are shown without their `agent_plugins:` prefix. The path, manifest, version and size errors **fail provisioning** for a plugin that is not `optional` (the error starts with `agent plugins:`). For an `optional` plugin they are warnings that end in `(optional plugin skipped)`.

### Harness messages in agent.log

The harness's provisioner installs the plugins inside the container just before the harness starts. **An agent never fails to start because of a plugin.** Problems are written to `agent.log` as `[provision]` lines such as:

```text
[provision] codex provision: warning: agent plugin scion-smoke: install failed (…); will retry next start
```

| Message | Meaning |
| :--- | :--- |
| `agent plugins skipped: <cli> <version> is older than <min> (needs Agent Plugins support)` | The image's harness CLI is too old. Nothing is installed or removed. |
| `agent plugin <n>: install failed (…); will retry next start` | The harness CLI failed. The CLI output is included. |
| `agent plugin <n>: removal failed (…); will retry next start` | A plugin that was dropped could not be removed. |
| `agent plugins: time budget reached; <k> plugin(s) deferred to next start`, or `…; <k> removal(s) deferred to next start` | Plugin work has a 20-second budget per start. |
| `agent plugin <n>: codex skips sse server(s): …` | codex only. |
| `agent plugin <n>: muse: Agent Plugins MCP server "<s>" is not supported in this release; …` | muse-code only. |
| `agent plugin <n>: mcp server <s> skipped for claude: …` | claude only; see the [skip rule](#claude). |

These lines are logged at INFO level, so `SCION_LOG_LEVEL=warn` hides them; see [Observability](/scion/hosted/single-node/observability/). For the full result of the last start, read `~/.scion/agent-plugins/status-<harness>.json` in the agent.

### Old harness-configs

Harness-configs installed before plugin support have no plugin code in their provisioner and do not declare Agent Plugins support. Scion stages the bundles but does not install them, and the start prints:

```text
agent_plugins: harness-config "codex" predates Agent Plugins support; plugins staged but not installed (run scion harness-config reset codex for the bundled defaults, or scion harness-config upgrade codex to keep local edits)
```

To fix this:

- For a bundled harness-config in `~/.scion/harness-configs/<name>`, run `scion harness-config reset <name>`, which restores the embedded version (it overwrites local edits). `scion harness-config upgrade <name>` is the additive alternative: it replaces the provisioner scripts and `agent-plugins.yaml` (with a backup), but it keeps your existing `provisioner.timeout`. For codex, copilot and muse-code, raise that to `60s` by hand.
- Harness-configs stored on a Hub must be updated there: install an updated source over it with `scion harness-config install --force <source>`, or upload a fixed local copy with `scion harness-config sync <name>`.
- Copies of a harness-config bundled inside templates must be updated too.

### Claude recovery

- **Lost Scion state.** If `~/.scion/agent-plugins/state-claude.json` is missing or corrupt, Scion no longer knows which `~/.claude/skills/` directories it installed. It treats them as user content and does not replace or remove them, so the plugin is not updated (`… exists and was not installed by Scion`). To recover, delete `~/.claude/skills/<name>/` in the agent; the next start installs the plugin again.
- **Interrupted update.** If the provisioner is killed in a narrow window during an update of a plugin directory, carried runtime output can be lost, or the old copy can be left behind without Scion managing it. `pending-claude.json` covers most interruptions, but not this one. To recover, delete `~/.claude/skills/<name>/`; the next start installs the plugin again.
- **Changing `skills_dir`.** If you change the claude harness-config's `skills_dir`, the plugin is installed in the new directory, and the copy in the old directory stays behind. Delete `<old skills_dir>/<plugin name>/` by hand.

## Security notes

- **Bundles run code.** A stdio MCP server is a program from the bundle, started by the harness with the agent's permissions. Only use bundles you trust, as you would for template files.
- **Template-relative sources only.** Scion does not fetch plugins from URLs or registries in this release. A plugin path must stay inside its template directory, and the plugin directory and its parents inside the template must be real directories, not symlinks.
- **Symlinks are never followed.** Symlinks and special files inside a bundle are skipped and reported. Copied files get mode 0644, or 0755 if any execute bit was set; setuid, setgid and sticky bits are dropped.
- **Size limits.** Each bundle is limited to 10,000 entries (files, directories and skipped symlinks), 100 MiB and a directory depth of 32. `plugin.json` and `mcp.json` are limited to 1 MiB each.
- **MCP values are visible data.** Header and `env` values in `mcp.json` are stored in the template and copied into the agent home and the harness's config. Do not put secrets in them.
- **Remote MCP URLs must use https,** except for `localhost` and loopback addresses. URLs with user information or a fragment are rejected.
- **claude: `bin/` is on the model's shell PATH.** Claude appends each plugin's `bin/` directory to the PATH of its Bash tool. It is appended, so it cannot shadow existing commands, but every executable in the bundle's `bin/` can be run from the model's shell.
- **claude: Claude-specific root files are not installed.** `.mcp.json`, `.lsp.json`, `settings.json`, `hooks/`, `commands/`, `agents/`, `monitors/`, `output-styles/`, `workflows/`, `themes/`, `.claude-plugin/` and a root `SKILL.md` are removed from the installed copy, so a bundle cannot add hooks, commands, agents or unvalidated MCP servers to Claude. See the [claude notes](#claude).

## Limitations

- Only Agent Plugins **1.0.0** is supported.
- Sources are template-relative paths only: no GitHub or registry sources, no CLI flag and no Hub API to add plugins to an agent.
- gemini-cli, grok-build, antigravity, hermes, opencode and pi do not install plugins yet.
- Plugin MCP servers are not added to Scion's own MCP configuration (`mcp_servers`); each harness reads them from the plugin.
- Install results inside the container (`status-<harness>.json`) are not reported back to the Hub.
