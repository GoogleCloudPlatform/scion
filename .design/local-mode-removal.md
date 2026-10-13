# Design: removing scion's local-only mode

Status: design with all decisions (D1-D7, D13) approved by the maintainer on 2026-10-10. Amendments at the end:
- Amendment 1 (2026-10-10): the import keeps non-running agents not running.
- Amendment 2 (2026-10-10): phase-1 acceptance and fresh-install preconditions.
- **Amendment 3 (2026-10-10): no auto-start; accepted, and it supersedes the auto-start parts of this document.**
- Amendment 4 (2026-10-12): interaction with the CLI current-context design.
- Amendment 5 (2026-10-12): new agents in a linked repository were cloned, not given a worktree; D4's premise corrected. Resolved by Amendment 6.
- Amendment 6 (2026-10-12, accepted): phase 3a, local worktrees for linked repositories (variant L).
- Amendment 7 (2026-10-12, accepted): phase 3b, hub project ID scoping and the link guard in a git repository without `.scion`.
Base: main at ae24a3c. All file:line references are to that commit unless stated otherwise.
Input: an inventory of local-only code paths, tests and docs (the "findings"; not published). This design does not repeat that inventory; it assigns it.

This is the public copy of the design. User-facing documentation of the result is in `docs-site/src/content/docs/reference/local-mode-removal.md`.

## Summary

> **Current behaviour:** nothing auto-starts a server (Amendment 3). Where this document describes auto-start, read it as superseded: with no hub, commands point at `scion server start`.

Local-only mode is the CLI calling the `pkg/agent` Manager directly whenever `CheckHubAvailability*` returns a nil HubContext (`--no-hub`, or hub not enabled in settings). The engine itself stays, because the Runtime Broker is built on it. What goes is the CLI's direct-dispatch path, the mode switch (`--no-hub`, `hub enable`, `hub disable`, `hub.enabled`, `hub.local_only`), and the local-to-hub reconcile in `pkg/hubsync`.

After removal, every agent operation goes through a hub. A workstation user gets one automatically: when no hub is configured, the CLI starts the existing workstation server (hub, broker and web on loopback, SQLite, dev auth) as a daemon, writes its endpoint, and links the current repo to it without prompting. A new user's first command can be `scion start <agent> "<task>"`, the same as in local mode today.

The work is seven mergeable phases (0 to 6, with phase 2 split into 2a and 2b) plus one manual runtime check:

| # | Phase | Size | Touches ./cmd |
|---|---|---|---|
| 0 | Fix the nil-deref in skills commands; add `requireHub` helper | XS | yes |
| V | Manual runtime check: adoption of running local containers after `hub link` | — (no code) | no |
| 1 | Vertical slice: `hub link` auto-starts and links a local hub; endpoint plus credential implies enabled; workstation smoke test | M | yes |
| 1b | Onboarding-status wizard trigger on `server start`; the workstation server tolerates a missing `image_registry` (Amendment 3) | S | yes |
| 2a | New semantics for unlink, delete recovery, error hints, broker reads of `hub.enabled` | M | yes (plus pkg/runtimebroker, pkg/agent) |
| 2b | Host credential files for agents on a co-located workstation broker (parity with local mode) | M | one line (pkg/runtimebroker, pkg/agent, pkg/harness, pkg/api, pkg/config; one config field in cmd) |
| 3 | The flip: hub always required, auto-start on any command, delete the mode switch, collapse the nil contract | L | yes |
| 4 | Delete the now-unreachable local tails in cmd and their tests | M | yes |
| 5 | Docs rewrite (tutorial, choosing-a-mode, reference, AGENTS.md, glossary) | M | no |
| 6 | Engine and package cleanup: collapse `BrokerMode`, explicit project scan in `List`, delete the hubsync reconcile and `state.go` | M | no (pkg/agent, pkg/runtime, pkg/runtimebroker, pkg/hubsync, pkg/config only) |

Phases 3 and 5 ship in the same release. Phase 6 waits one release after phase 3, because the reconcile it deletes is the migration tool.

## Decisions

Seven questions went to the maintainer, each with a recommended answer. The maintainer approved all seven recommendations on 2026-10-10. An eighth (D13, credentials) was asked separately and approved the same day.

| # | Question | Decision | Status |
|---|---|---|---|
| D1 | Hybrid users: local container with a hub-connected agent (cmd/common.go:924-940); local `create` resolving skills through a hub (cmd/create.go:150). | Accept the loss. Both are served by running the agent on the local workstation broker, where the agent is hub-connected and skills resolve on the hub. | approved by maintainer (2026-10-10) |
| D2 | Is a running daemon acceptable as the minimum? Auto-start or embed? | **Superseded by Amendment 3: no auto-start. With no hub, commands fail with a one-line pointer to `scion server start`, whose first run opens the onboarding wizard.** Original text: a running workstation server is the minimum. The CLI auto-starts it as a daemon when no hub is configured, or when the configured endpoint is the local workstation endpoint and nothing answers. No in-process embedded hub. Unlinked repos are linked without a prompt when the hub is the local workstation hub. | approved by maintainer (2026-10-10) |
| D3 | Migration: clean break, or keep the `hub link` import for one release? | Keep the existing import (hub link reconciles `.scion/agents` onto the hub) for one release as the migration tool; delete it in phase 6. | approved by maintainer (2026-10-10) |
| D4 | macOS Apple container: root DNS/pf step for container-to-hub reachability. | Accept for now. Auto-detect on macOS prefers podman and docker, which get the automatic bridge rewrite; the onboarding wizard already has the apple-dns helper. Document it; not a blocker. | approved by maintainer (2026-10-10) |
| D5 | Filesystem template and harness-config views (`--local`, local install). | Keep them as explicit filesystem features. Remove the silent fallback to the filesystem on hub errors (cmd/templates.go `hubCtx, _ =`). | approved by maintainer (2026-10-10) |
| D6 | sciontool's own no-hub mode (pkg/sciontool/hub/client.go:87). | Out of scope. It is in-container robustness, not a user-selectable mode; revisit separately. | approved by maintainer (2026-10-10) |
| D7 | Adoption of already-running local containers after `hub link` is unverified. | Phase V checks it manually on a disposable machine before phase 3 merges. If adoption fails, the migration message says "stop your agents before upgrading"; no adoption code is added. | approved by maintainer (2026-10-10) |

Decisions this design makes on its own (reversible unless marked):

- **D8, load-bearing.** "Hub configured" means an endpoint is resolvable (flag, env or settings). Credentials are resolved separately (token, API key, OAuth login, dev token). `hub.enabled` stops being a switch.
- **D9, load-bearing.** The settings schema keeps `hub.enabled`, `hub.local_only` and `hub.lastSyncedAt` as deprecated, ignored keys, because the schema is strict (`additionalProperties: false`, pkg/config/schemas/settings-v1.schema.json:176-179). Removing them would make every existing settings file that sets them fail validation.
- **D10.** `hub.linked` becomes tri-state: unset means never linked (auto-link may run), `true` means linked, `false` means the user ran `hub unlink` (no auto-link; agent commands refuse with a pointer to `hub link`).
- **D11.** `--no-hub`, `hub enable` and `hub disable` stay for one release as hidden commands that fail with a pointer to the migration doc, then go in phase 6. Without them, users get Cobra's bare "unknown flag" error.
- **D12.** The recovery hint after a failed post-delete cleanup becomes `scion delete --cleanup-only <name>`, which runs only the local file cleanup and needs no mode switch.
- **D13, load-bearing; asked as an 8th question.** Under a co-located workstation broker, agents get the host's harness credential files (OAuth login files, gcloud ADC) as local mode does today. The scope is loopback with dev auth, where the user is the operator. It is on by default and can be turned off. Hosted brokers are unchanged. See "Credential parity" below. Status: approved by maintainer (2026-10-10).

## Target state

### What scion is after removal

There is one CLI architecture: the hub-connected CLI. Agent state lives in a hub database, and agents run on a Runtime Broker. A workstation user's hub is the workstation server (`scion server start`, cmd/server.go:76-135): hub, broker and web together on 127.0.0.1, with SQLite at `~/.scion/hub.db` and a dev token at `~/.scion/dev-token`. A team user points the CLI at a remote hub. There is no other way to run an agent.

Unchanged, because the broker depends on them:
- the `pkg/agent` Manager;
- `pkg/runtime` and container labels;
- `.scion` project resolution and template seeding;
- agent homes under `<project>/.scion/agents/<name>`;
- worktrees at `<repo>/.scion/agents/<agent>/workspace` (corrected; an earlier version of this design said `../.scion_worktrees`).

A linked project keeps its directory layout. The co-located broker uses the same paths.

### Hub resolution (the new dispatcher contract)

`CheckHubAvailability*` (cmd/common.go:339-412) becomes a single `requireHub(projectPath, opts) (*HubContext, error)` that never returns `(nil, nil)`:

```
requireHub(projectPath, opts):
  ep := resolveEndpoint()            # --hub flag > SCION_HUB_ENDPOINT/URL > settings hub.endpoint
  if ep == "":
      if autoStartAllowed():         # see "Auto-start" below
          ep = ensureLocalServer()   # starts the daemon, waits, writes hub.endpoint
      else:
          return errHubNotConfigured # "No hub configured. Run 'scion server start' or 'scion config set hub.endpoint <url>'."
  client := newClient(ep, credentials())
  if !client.Healthy():
      if isLocalWorkstationEndpoint(ep) && autoStartAllowed():
          ensureLocalServer()
      else:
          return hubUnreachable(ep)  # hint: 'scion hub status'; for loopback: 'scion server start'
  if hub project ref: resolve on hub (unchanged)
  link := settings.hub.linked
  if link == false: return errUnlinked   # "This project was unlinked. Run 'scion hub link' to re-link."
  if not registered:
      if isLocalWorkstationEndpoint(ep): register silently, print one line
      else: existing link prompt (pkg/hubsync/sync.go EnsureHubReady, register step)
  return hubCtx                       # no per-command agent reconcile
```

`isLocalWorkstationEndpoint(ep)` is true when the host is a loopback address and the CLI authenticates with the dev token for that server (pkg/apiclient/devauth.go:115-152). The dev token only exists for a loopback workstation server, so a remote or tunnelled hub on localhost with a real token is not auto-linked.

Commands that must not trigger auto-start: `help`, `version`, `doctor`, `config *`, `server *`, `hub status`, `hub auth *`, `completion`, and everything inside an agent container. The container check in `checkAgentContainerContext` (cmd/root.go:609-680) already refuses to run without a remote hub. Commands that read the filesystem only (`templates --local`, `harness-config` local views, `cdw`, `clean`, `project prune/list`) do not call `requireHub` at all.

### Command behaviour changes

| Command | Today with no hub | After removal |
|---|---|---|
| `start`, `create`, `list`, `stop`, `suspend`, `resume`, `delete`, `attach`, `logs`, `look`, `keys`, `broadcast`, `message`, `sync <agent>` | runs locally through `agent.NewManager` | always goes through the hub (auto-start and auto-link if needed). The local tails are deleted. |
| `logs --follow` | refused with a hub; locally prints the log file once (cmd/logs.go:64-95) | the flag is removed (it never streamed). |
| `sync <agent>` without a direction | allowed locally (cmd/sync.go:251-261) | direction required, as with a hub today. |
| `delete --force` | local-mode warning path (cmd/delete.go:95-100) | the warning is removed; `--force` keeps its hub meaning. |
| `stop/suspend/delete --all/--stopped` | local fan-out | hub fan-out only (exists today). |
| Hub-only commands (restore, schedule, secret, skills, …) | refused, or a nil-deref panic for skills | work, after auto-start and auto-link. |
| `init` | creates `.scion`; prompts to link only if `hub.enabled` (cmd/project.go:221-245) | creates `.scion`, then tries `requireHub` to link. If no hub can be reached or started, it warns instead of failing: linking happens on first use anyway. |
| `hub link` | needs an endpoint; offers the agent import (cmd/hub.go:2644-2656) | if there is no endpoint, auto-starts the local server; writes `hub.linked=true`, not `hub.enabled`; the import stays until phase 6. |
| `hub unlink` | writes `hub.enabled=false`, which drops the project into local mode (cmd/hub.go:2790) | writes `hub.linked=false` and clears `hub.projectId`; afterwards agent commands in that project refuse with a pointer to `hub link`. |
| `hub enable` / `hub disable` | toggles `hub.enabled` | hidden for one release; they fail with a pointer to the migration doc. Removed in phase 6. |
| `hub status` | renders the enabled scope and the no-hub override (cmd/hub.go:646-697, 766, 846-848) | shows the endpoint and its source, reachability, auth, link state, and "local workstation: yes/no". The enabled-scope rendering is removed. |
| `--no-hub` | selects local mode | hidden for one release; it fails with a pointer to the migration doc. Removed in phase 6. |
| `templates list/show/delete/clone` | silently fall back to the filesystem on hub errors | a hub error is returned; `--local` stays as the explicit filesystem view (D5). |
| `project prune` | stops orphaned containers through the runtime (cmd/project_prune.go:150-167) | unchanged: this is host maintenance of orphaned project configs, not a mode. |
| `doctor`, `build`, `cdw`, `clean` | runtime- or filesystem-direct | unchanged. `clean` loses its "hub disabled, check anyway?" prompt (cmd/clean.go:107-114). |

## Replacement ergonomics

This section was designed first. The removal is only acceptable if a new local user's path is no longer than it is today.

### Minimum steps for a new local user

| | Local mode today | After removal |
|---|---|---|
| 1 | install the binary and a container runtime | same |
| 2 | once per machine: `scion init --machine`, which needs a container runtime, and an `image_registry` (set by `--image-registry`, the Homebrew install, or `SCION_IMAGE_REGISTRY`) | same |
| 3 | once per repo: `cd repo && scion init` (outside a project, commands need `--global`) | same |
| 4 | `scion start fix-bug "task"`. The image registry is checked only at agent start. | same command. On first use the CLI prints `No hub configured; starting local scion server…`, starts the workstation daemon, prints the web URL, links the repo (`Linked project 'repo' to local hub`), and starts the agent through the hub. The image registry is checked before auto-start (Amendment 2). |
| — | — | Optional: `scion server install` so the server starts at login. On first auto-start the CLI prints one line suggesting it. |

Today's explicit path stays valid and becomes shorter: `scion server start`, then any command in any repo. `scion hub enable` and `scion hub link` are no longer required.

### Gaps from the findings, closed or accepted

| Gap (findings, "Replacement path and gaps") | Resolution |
|---|---|
| No single command; `hub.enabled` not set; per-repo link | **Closed.** A resolvable endpoint is enough (D8). Auto-link on the local workstation hub (D2). Remote hubs keep the link prompt, because linking to a shared hub is a visible act for other users. |
| A daemon must be running | **Closed** by auto-start (D2). It is opt-out through the setting `hub.auto_start: false` or the env var `SCION_HUB_AUTO_START=0`. With auto-start off, the CLI fails with the `errHubNotConfigured` text instead. Auto-start runs `<self> server start`, the existing daemon path (cmd/server_daemon.go:172-236), as a subprocess. It does not refactor that code into a library call, so a CLI-started server is identical to a user-started one. The subprocess path already writes `hub.endpoint` (cmd/server_daemon.go:979-990). Auto-start does not open a browser; it prints the URL. |
| Container-to-hub reachability on Apple container | **Accepted** (D4). Docker and podman are handled automatically (pkg/runtimebroker/hubenv.go:270-295). The doc gets an Apple container section. |
| Fixed dev identity | **Accepted.** Single-user workstation; cosmetic only. |
| Offline | No regression. |
| Footprint (a persistent process) | **Accepted.** `scion server stop` is the off switch. The design records it as a known cost. Measuring idle RSS of the workstation server is an open item. |
| **New, not in the findings: credential parity** | **Closed by phase 2b** (D13). See below. |

### Credential parity (D13)

Local mode starts agents with `harness.GatherAuthWithEnv(env, localSources=true, …)` (pkg/agent/run.go:996). It reads env API keys and the harness-declared credential files in the user's home: OAuth login files, and gcloud ADC (pkg/harness/auth.go:50-110). The broker passes `localSources=false` so that operator credentials never leak into hub-dispatched agents.

On a workstation server:
- env keys are covered by the CLI env-gather flow (cmd/common.go:1305, 1467ff);
- ADC is covered only when the user opts in with `auto_inject_gcloud_adc` (pkg/runtimebroker/start_context.go:1146-1160; the onboarding page offers it);
- harness credential files are not covered at all.

So a user who logs a harness in by subscription would see the agent fail auth after the removal.

Phase 2b extends the existing co-located injection (`isColocated` in start_context.go) from ADC alone to the harness-declared credential files. It reads `authMeta` required_files from the host home and injects them as file secrets, with the same precedence rule (hub secrets win). It applies only when the broker is co-located with a hub on loopback using dev auth. It is controlled by a new workstation setting `use_host_credentials` (default true in workstation mode, ignored in hosted mode). `auto_inject_gcloud_adc` becomes an alias of it for ADC. This is a security-relevant default; the maintainer approved it (8th question).

## Semantics changes

Each entangled behaviour gets a defined replacement. Phase numbers say where it lands.

| Entanglement | Today | Replacement | Phase |
|---|---|---|---|
| `hub unlink` | writes `hub.enabled=false`, `hub.linked=false`, clears `hub.projectId` (cmd/hub.go:2790-2798); the project falls into local mode | writes `hub.linked=false` and clears `hub.projectId`; does not touch `hub.enabled`. The "is linked" check at cmd/hub.go:2766 changes from `IsHubEnabled()` to `IsHubLinked()`. Afterwards `requireHub` returns `errUnlinked` for agent commands in that project (D10). Output: "Agents on this machine for this project keep running on the broker; manage them with `--project <hub-slug>` or re-link." | 2a |
| List's link-or-disable prompt (cmd/list.go:1061-1143, pkg/hubsync/prompt.go:396-448) | offers "disable hub" | phase 2a removes the disable choice and leaves "link / cancel". Phase 3 drops the prompt entirely: `requireHub` auto-links on the local hub, and on a remote hub the registration prompt in EnsureHubReady already covers it. | 2a, 3 |
| Delete recovery hint `noHubDeleteCommand` (cmd/delete.go:320-328, used at :396 and stop.go:619; comment :199-207) | "run `scion --no-hub delete <name>`" | new flag `scion delete --cleanup-only <name>` (with `--preserve-branch` honoured): runs only `agent.DeleteAgentFiles` (the body of `cleanupAfterHubDelete`, delete.go:306-317). Safety check: if the hub is reachable and still has the agent, refuse with "agent still exists on the hub; run `scion delete <name>`". If the hub is unreachable, refuse unless `--force`. `noHubDeleteCommand` is renamed `cleanupOnlyCommand` and the hint text changes. | 2a |
| `wrapHubError` local-only hint (cmd/common.go:473-476, 598-607) | appends "To use local-only mode, run: scion hub disable" to connectivity and 5xx errors | `hubCheckHint(endpoint)`: for a local workstation endpoint, "Is the local server running? Check with `scion server status`; start it with `scion server start`." For any other endpoint, "Check the hub with `scion hub status`." `shouldSuggestLocalOnly` is renamed `shouldSuggestHubCheck`, with unchanged logic. About 8 test assertions update (broker_failure_test, message_notfound_test, message_thread_2026_test, common_hubhint_test). **Also pkg/hubsync's own `wrapHubError`** (sync.go ~1595, "To use local-only mode, use: scion --no-hub <command>") gets the same wording, in a separate 2a commit. The hubsync link-refusal lines (~513, ~610, "Or use local-only mode") and the `local_only` error (~297) are not hub-error remedies; they go in phase 3 with the NoHub/LocalOnly paths. | 2a |
| `--no-hub` (cmd/root.go:30, 426, 505-507, 555; root_options.go:37; common.go:368-376, 672) | selects local mode | Phase 3: hidden; any use fails before dispatch with "`--no-hub` was removed together with local-only mode; scion always uses a hub. See <migration doc>." Phase 6: flag deleted. `EnsureHubReadyOptions.NoHub` is deleted in phase 3. | 3, 6 |
| `hub enable` / `hub disable` (cmd/hub.go:226-251, 2227-2370) | toggle `hub.enabled` | Phase 3: hidden; both fail with guidance. For enable: "no longer needed: a configured endpoint is used automatically; set one with `scion config set hub.endpoint <url>`". For disable: "local-only mode was removed; stop the local server with `scion server stop`". Phase 6: deleted. About 38 help strings that say "scion hub enable" are swept in phase 3 (`git grep -n "hub enable" -- cmd pkg`), including the `conversation` help text. | 3, 6 |
| `hub.enabled`, `hub.local_only`, `hub.lastSyncedAt` settings | the mode switch, a sync opt-out, and the reconcile watermark | Phase 3: no longer read by the CLI. `Settings.IsHubEnabled()` is replaced by `HubEndpoint() string` / `HasHubEndpoint() bool`, and `IsHubLocalOnly` is deleted. The schema keeps the keys, marked deprecated and ignored (D9). `hub status` prints a one-line note when a settings file still sets them. Phase 6 deletes the struct fields and v1 mirrors (settings.go:96-126, settings_v1.go per the findings) but keeps the schema properties. | 3, 6 |
| Broker reads of `hub.enabled`: `hubEndpointFromProjectSettings` (pkg/runtimebroker/hubenv.go:241-250); `hubDisabled` stripping (pkg/agent/run.go:1216, 1314); `hubEnabled` for platform skills (pkg/agent/provision.go:1835) | a project with `hub.enabled=false` on the broker host loses its hub endpoint fallback; local starts strip hub env | Phase 2a: hubenv.go ignores `IsHubExplicitlyDisabled`, because the broker is authoritative about hub connectivity, as run.go:1309-1313 already says. provision.go:1835 becomes `IsBrokerModeFromContext(ctx) \|\| settings.HasHubEndpoint()`. Phase 6: run.go `hubDisabled` and its stripping are deleted with the non-broker branches. | 2a, 6 |
| `BrokerMode` doubling as "not local" (set only at pkg/runtimebroker/start_context.go:847; ~17 branches in run.go, 6 in provision.go, pkg/runtime/common.go:493, pkg/agent/broker_hub_env_authority.go) | false means a local CLI start | After phase 4 no caller of `Manager.Start`/`Provision` outside pkg/runtimebroker remains (verify with `git grep -n "agent.NewManager(" -- cmd`: only server_foreground.go:3212 and project_prune.go:160 may remain, and prune never starts agents). Phase 6 makes broker semantics unconditional: it deletes the `!BrokerMode` branches (local credential sources at run.go:996, hub-env stripping, the gcloud host-dir mount at runtime/common.go:493-510, the non-strict agent state dir at provision.go:1190-1270) and removes `api.StartOptions.BrokerMode`, `ContextWithBrokerMode` and `IsBrokerModeFromContext`. **Exception:** D13's host credentials are not restored through `localSources`; they come in as co-located file secrets (phase 2b). Deleting the branch therefore loses nothing. | 6 |
| `AgentManager.List` with an empty filter scans the cwd project plus the global dir (pkg/agent/list.go:66-80), and the broker heartbeat calls it that way (pkg/runtimebroker/heartbeat.go:579) | a CLI convenience leaking into the broker | Phase 6: an empty filter scans nothing extra. The broker passes `scion.project_path` per provided project (it knows them from its project registry), or a new `ListOptions{ProjectPaths []string}`. | 6 |
| Per-command reconcile in `EnsureHubReady` (CompareAgents/ExecuteSync step, pkg/hubsync/sync.go ~545-600) | registers agents created in local mode; can delete hub records missing locally | Phase 3: removed from the per-command path. After the flip, only a broker creates agent dirs, so drift cannot come from the CLI. The reconcile stays reachable only from `hub link` (the migration import) until phase 6. | 3, 6 |
| Templates silent fallback (cmd/templates.go `hubCtx, _ =`) | degrades to the filesystem on hub errors | Phase 3: `requireHub` errors propagate; `--local` remains the explicit filesystem view (D5). `templates list` has no `--local` today, so phase 3 **adds `templates list --local`**: filesystem templates only, no hub call. It reuses the body of `printTemplateListLocalMode` (templates.go:180-219), renamed, instead of deleting it. Without this, the flip would remove the only offline template listing, against D5. | 3 |
| Local create's hub skill resolution (cmd/create.go:150, `withLocalSkillResolution` 172-218) | hybrid | deleted with the local tail (phase 4). Hub create resolves skills on the hub. | 4 |
| Local start threading a hub endpoint into a local agent (cmd/common.go:924-940) | hybrid | deleted with the local tail (phase 4); D1. | 4 |

## Migration

**Recommendation: a soft break with an import (D3).** Under alpha policy a clean break would be allowed. The import already exists, though: `hub link` runs the hubsync reconcile, offered at cmd/hub.go:2644-2656, which registers every `.scion/agents/*/scion-agent.{yaml,json}` on the hub (pkg/hubsync/sync.go:761-1114). So keeping it for one release costs no new code. What carries over is the agent name, template and harness config. Status and history do not.

What an existing local user experiences on the first agent command after upgrading:

1. If no endpoint is configured, the CLI auto-starts the local server (D2). With a remote endpoint already configured, it is used. A stale `hub.enabled=false` is ignored, and `hub status` notes it.
2. The repo is not registered on the hub, so the CLI links it automatically on the local hub, or prompts on a remote hub. **During the import release only**, if the repo has agent dirs that the hub does not know, the auto-link also runs the import. The import is non-interactive on the local hub. On a remote hub it shows the existing plan prompt. The CLI prints:

   ```
   Local-only mode has been removed: scion now always runs agents through a hub.
   Started a local scion server at http://127.0.0.1:<port> (stop it with 'scion server stop').
   Linked project '<name>' and imported N existing agents (name, template and harness config; history is not imported).
   Running agents were adopted in place by the local broker.
   Created and stopped/suspended agents were imported as stopped; start them with 'scion start <name>' (a suspended agent's harness session is not resumed).
   See <docs>/reference/local-mode-removal for details.
   ```

3. Agent homes and worktrees stay in place. The co-located broker uses the same layout and reuses existing agent dirs (pkg/agent/provision.go:2299).

Every repo migrates lazily, on its first command. No bulk migration command is needed. `scion hub link --global` is the explicit path for the global project, and the global project auto-links the same way.

**Running containers (verified, Phase V run 1).** After `scion server start` and `hub link`, a running local agent is adopted in place: same container ID, and look, message, stop and delete all work through the hub, including worktree and branch cleanup. The message says so. No adoption code is needed (D7).

**Agents that are not running (fixed by Amendment 1).** Today the import starts created-only and suspended agents. `ExecuteSync` sends a plain `CreateAgentRequest` (pkg/hubsync/sync.go:924-937 at 8d83e9b), which means create plus start. The agent comes up running, in a new container, with its task prompt re-sent. Amendment 1 makes the import keep these agents not running. A created-only agent is imported as created. A suspended or stopped agent is imported as stopped (a provisioned agent with no container). It is not imported as suspended, because the hub cannot resume a harness session that the hub never saw.

**After the import release**, phase 6 deletes the reconcile. A user who skipped that release still gets an automatic link; only their old agent records are not imported. Their agent dirs still exist on disk, and `scion start <name>` with the same name reuses the dir (provision.go:2299). This is the accepted residual break.

**Rollback for a user.** Downgrade the binary. Old binaries still understand `hub.enabled` and `hub.linked`. Phase 3 writes `hub.linked` and `hub.endpoint`, which old binaries read the same way. The only incompatible write is `hub.linked=false` without `hub.enabled=false` after an unlink (2a). An old binary reads that as "hub enabled if an endpoint and token exist". That is benign.

## Phased plan

Every phase merges on its own and leaves main releasable. Phases 0, V, 1 and 2b can run in parallel. **Phase 2a and everything after it wait for phase 1 to be validated**: its smoke test is green and a human has run the slice once on a real machine. Phase 1 is where an interface mismatch in the new contract (`requireHub`, endpoint-implies-enabled, auto-start, auto-link) shows up while it is still one fix.

| Phase | Scope | Size | Depends on | Touches ./cmd or pkg/hub | Main files |
|---|---|---|---|---|---|
| 0 | Nil-deref fix in skills/registries; `requireHub` helper (thin wrapper: nil becomes `errHubNotConfigured`) | XS | — | ./cmd | cmd/skills.go, cmd/skill_registries.go, cmd/common.go |
| V | Manual runtime check of adoption and of the phase-1 slice on a disposable machine | — | 1 merged for the slice part | — | none (report only) |
| 1 | Vertical slice (reduced by Amendment 3 to: local-hub detection, auto-link, link-prompt skip, smoke test) — originally: `requireHub`'s auto-start (`ensureLocalServer`) and local-hub detection; `hub link` auto-starts the local server when there is no endpoint; auto-link on the local hub when hub is enabled; workstation smoke test | M | — | ./cmd | pkg/config/settings.go, pkg/hubsync/sync.go, cmd/hub.go, cmd/common.go, new cmd/hub_autostart.go, hack/workstation-smoke.sh, .github/workflows (new job) |
| 1b | Amendment 3: `server start` opens `/onboarding` when the hub's onboarding status is not `Complete`; in workstation mode a missing `image_registry` no longer exits the server (hub and web up, broker deferred, fix printed) | S | 1 | ./cmd | cmd/server_daemon.go, cmd/server_foreground.go |
| 2a | Semantics: unlink, `delete --cleanup-only`, hub-check hints, list prompt without disable, broker ignores `hub.enabled`; **import keeps non-running agents not running (Amendment 1, separate commit, pkg/hubsync only)** | M | 1 validated | ./cmd (plus pkg/runtimebroker, pkg/agent) | cmd/hub.go, cmd/delete.go, cmd/stop.go, cmd/common.go, cmd/list.go, pkg/hubsync/prompt.go, pkg/runtimebroker/hubenv.go, pkg/agent/provision.go |
| 2b | Host credential files on a co-located workstation broker | M | — (decision D13) | XS in ./cmd (one config field; CI compiles it) | pkg/runtimebroker/start_context.go and server.go, pkg/api/types.go, pkg/agent/run.go, pkg/harness/auth.go, pkg/config/settings_v1.go, schema, cmd/server_foreground.go (one line) |
| 3 | The flip: `requireHub` everywhere, auto-start on any agent command, no nil contract; hidden erroring `--no-hub`/enable/disable; no per-command reconcile; template fallback removed; prompts removed; test harness rewritten | L | 1 validated, 2a (including Amendment 1), 2b, V run 2 | ./cmd | cmd/common.go, root.go, root_options.go, hub.go, hub_ref_enable.go (deleted), hub_auth_endpoint.go, list.go, clean.go, completion_helper.go, project.go, project_gate.go, templates.go, artifact.go, notifications.go, broker.go, about 25 files with `hubCtx == nil` sites; pkg/hubsync/sync.go; pkg/config/settings.go; cmd tests |
| 4 | Delete the unreachable local tails and their tests | M | 3 | ./cmd | Appendix A rows in common, list, stop, suspend, delete, attach, logs, look, keys, broadcast, message, create, sync, harness_config_install; tests listed in the findings |
| 5 | Docs | M | lands in the same release as 3 (may merge right before or after it) | no | see Docs |
| 6 | Engine and package cleanup; drop the import, `state.go`, deprecated flags and commands, settings fields; collapse `BrokerMode`; explicit `List` scan | M | 4, plus one release after 3 | no for the pkg parts; the deletion of the hidden `--no-hub`/enable/disable stubs touches ./cmd (XS) and can ride along or go separately | pkg/hubsync/sync.go, prompt.go, pkg/config/state.go, settings.go, settings_v1.go, pkg/agent/run.go, provision.go, manager.go, list.go, broker_hub_env_authority.go, pkg/api/types.go, pkg/runtime/common.go, pkg/runtimebroker/start_context.go, heartbeat.go; cmd/root.go, cmd/hub.go |

Build note: per the findings and the module graph, pkg/agent, pkg/runtimebroker, pkg/hubsync, pkg/config and pkg/runtime do not depend on pkg/hub (`go list -deps` shows no pkg/hub). Their tests can run without compiling ./cmd or pkg/hub, which are much larger builds.

### Phase 0: nil-deref fix (XS)

Scope: cmd/skills.go (64, 132, 302, 458, 501, 577, 633) and cmd/skill_registries.go (43, 82, 132, 174, 230, 258) call `CheckHubAvailability`, check only `err`, then dereference `hubCtx.Client`. In local mode that panics. Add `requireHub(projectPath string) (*HubContext, error)` in cmd/common.go, which wraps `CheckHubAvailability` and turns nil into `errHubNotConfigured` ("this command needs a hub; run 'scion server start' or 'scion hub enable'"; the text changes in phase 3). Switch those 13 sites to it.
Tests: one table test per file that runs the command with no hub configured and asserts the error, not a panic.
Acceptance: `scion skills list` and `scion skills registries list` print the error and exit non-zero in a temp HOME with no hub. No other behaviour changes.
Why first: it is a live bug that is independent of the removal schedule, and it seeds the `requireHub` name that phase 3 generalises.

### Phase V: runtime validation (no code)

Run on a disposable machine with docker or podman, using a binary built from main after phase 1:
1. In a fresh HOME and a scratch git repo, run `scion --no-hub start a1 "sleep"` and confirm the container is running.
2. Run `scion server start`, then `scion hub link` and accept the import.
3. `scion list` must show `a1` as running, on the local broker. Check that `scion look a1`, `scion message a1 hi`, `scion stop a1` and `scion delete a1` work through the hub, and that the worktree is removed.
4. Repeat steps 1-3 with a suspended agent and a created-only agent.
5. Phase-1 slice check: in another fresh HOME, after `scion init --machine` (with an image registry) and `scion init` in a repo, run `scion hub link` with no server running. Confirm that it auto-starts, links, and that `scion start a2` works.
6. Credential check (input to 2b): with a harness logged in only by an OAuth credential file in the home dir, start an agent through the workstation server and record whether auth succeeds.
Output: a short report attached to the tracking issue. It decides the migration message's running-container line (D7) and confirms the 2b gap.

**Run 1 (2026-10-10, docker, main 8d83e9b):**
- Step 3, running agent: PASS (adopted in place).
- Step 4, suspended and created-only agents: started by the import → Amendment 1.
- Step 6, credential file: not testable (no harness login file on the host).
- Steps 1-5 used `hub link` by hand, because phase 1 had not merged.

**Run 2 (required before phase 3 merges), on a binary with phase 1 and Amendment 1:**
- (a) Repeat step 4 and check the Amendment 1 acceptance in phase 2a.
- (b) Record the agent-home path that a local-mode agent mounts, and the path after import. Run 1 saw the post-import home mounted from `~/.scion/project-configs/<project>__<id>/.scion/agents/<name>/home` but did not capture the pre-import path. If the two differ, the harness state in the old home does not carry over; the migration message already says the session is not resumed.
- (c) Step 5 (the phase-1 slice).
- (d) Step 6, on a host where a harness is logged in through a home-dir credential file, before and after phase 2b.

### Phase 1: vertical slice (M)

Scope:
- `isLocalWorkstationEndpoint(ep)` (loopback host, and a dev token resolves through pkg/apiclient/devauth.go:115) and `autoStartAllowed()`. Both are pure helpers and are reused unchanged by phase 3. `Settings.IsHubEnabled()` is **not** changed in this phase. Making "endpoint implies enabled" the rule would switch users who once ran `scion server start` (so their endpoint was auto-written) but still use local mode. That flip belongs to phase 3.
- `ensureLocalServer()` in a new cmd/hub_autostart.go: runs `<os.Executable()> server start` as a subprocess, waits with the existing readiness probe (cmd/server_daemon.go:907 `waitForServerReady`), then reloads settings. The subprocess writes `hub.endpoint`.
- `ensureLocalServer()` first runs the broker's image-registry precondition in the CLI. `requireImageRegistryForBroker` (cmd/server_foreground.go:3987) is moved to, or wrapped as, a shared helper with the same sources and the same message. If the check fails, it returns that error without spawning, so the user gets the fix instead of a server that exits and a log path (Amendment 2).
- `scion hub link` with no endpoint calls `ensureLocalServer()` instead of failing. It writes `hub.linked=true` and, in this phase only, still `hub.enabled=true`.
- Auto-link: in `EnsureHubReady`'s registration step (pkg/hubsync/sync.go, the `ShowLinkPrompt` branch), when `isLocalWorkstationEndpoint`, accept without prompting and print one line. A new option field `AutoLinkLocal bool` is set by cmd.
- Explicit `scion hub link` on the local workstation endpoint skips `ShowProjectLinkPrompt` ("Continue with linking?"). This holds whether the server was just auto-started or was already running. Running the command is the confirmation, and D2 already links implicitly without a prompt on the local hub; the explicit command must not be stricter than the implicit path. Remote endpoints keep the prompt. The agent-import prompt (`ShowSyncAfterLinkPrompt`) is unchanged in phase 1. (Clarified 2026-10-10, from the phase-1 review.)
- `hub.auto_start` setting plus the `SCION_HUB_AUTO_START` env var, read by `autoStartAllowed()`. Schema entry added.
- Smoke test `hack/workstation-smoke.sh`, plus a CI job that builds with the default tags (sqlite on; `make test-fast` uses `no_sqlite`). In a temp HOME and temp git repo it runs: `scion hub link` (auto-start) → `scion hub status` shows linked → `scion list` returns an empty list through the hub → `scion server stop`. It needs no container runtime. Starting an agent stays a manual (phase V) or nightly check.

Tests: unit tests for `isLocalWorkstationEndpoint` (loopback plus dev token = true; remote plus dev token = false; loopback with a non-dev token = false), `autoStartAllowed` (setting, env, inside container = false); a hubsync test that `AutoLinkLocal` registers without calling the prompt; the smoke job.
Acceptance:
- Preconditions, all unchanged from today's local-mode baseline (Amendment 2): the machine is initialised (`scion init --machine`), a container runtime is present, `image_registry` is configured (in settings or `SCION_IMAGE_REGISTRY`), and no hub endpoint is configured.
- In a project (after `scion init`), `scion hub link` starts the server, links, and exits 0. A following `scion start a2 "task"` runs through the hub with no further prompts.
- Outside any project, `scion --global hub link` does the same for the global project.
- With no `image_registry`, `scion hub link` fails **before** spawning the server, with the actionable registry message (`scion config set --global image_registry <registry>` / `SCION_IMAGE_REGISTRY`), and no daemon is left running.
- `SCION_HUB_AUTO_START=0 scion hub link` with no endpoint fails with the not-configured error and starts nothing.
- Existing local users (no endpoint, or `hub.enabled=false`) see no change on agent commands.
- The smoke job is green.
Validation gate: phase V step 5 passes and the maintainer has run the slice once. Only then do 2a and 3 start.

### Phase 2a: semantics (M)

**Amendment 1 scope item (separate commit; pkg/hubsync only, no ./cmd change).** In `ExecuteSync` (pkg/hubsync/sync.go:921-937), decide per agent whether its container is running before sending `CreateAgentRequest`:
- Running: keep today's request (create plus start). Phase V shows the broker adopts the existing container.
- Not running (no container, or container exited, which covers created-only, suspended and stopped), or state unknown: set `ProvisionOnly: true` (pkg/hubclient/agents.go:248). The hub records the agent and the broker provisions without starting. Its existing agent dir is reused (pkg/agent/provision.go:2299).

The running check uses the runtime directly: `runtime.GetRuntime(projectPath, "")` and `List` with the `scion.agent=true` and `scion.name=<name>` label filters. That adds a pkg/hubsync → pkg/runtime import; the import goes away with the reconcile in phase 6. "Unknown" (runtime error) defaults to not starting. Do not rely on `agent-info.json`'s phase: local mode reports a suspended agent as `stopped`, and the file can be stale. The import prints a per-agent line: `+ a1 (running, adopted)` / `+ b1 (imported as stopped)` / `+ c1 (imported as created)`.

**Exited leftover containers (clarified 2026-10-11, from Phase V run 2 (a)).** A locally suspended agent leaves an exited container (Exited 255). After a `ProvisionOnly` import, the broker heartbeat reports it, and the hub moves the agent to `error`. From `error`, neither `start` nor `resume` works without `--force` (pkg/hub `classifyExistingAgent`). The fix:
- **What is removed:** before the `ProvisionOnly` create, the import removes a non-running container only when it carries this agent's labels **and** this project's path. It never touches running containers or another project's containers.
- **What survives:** home, worktree and branch are untouched, because they are mounted dirs. Only the container's writable layer is lost, consistent with "harness session not resumed".
- **If removal fails:** warn, still import with `ProvisionOnly`, and print "run `scion stop <name>` before `scion start <name>`".
- **Output line:** `+ s1 (imported as stopped; its exited container was removed)`.

Hub semantics for `error` are unchanged. Rejected alternatives:
- a new hub create phase that adopts exited containers: it widens the API for a one-release import;
- letting `start` recover from `error`: it changes deliberate semantics for every agent.

Tests for the amendment, with a fake runtime and a fake hub client:
- running → request without `ProvisionOnly`;
- exited container → `ProvisionOnly: true`;
- no container → `ProvisionOnly: true`;
- runtime error → `ProvisionOnly: true` and a warning;
- the per-agent output lines.

Scope: the rows marked 2a in "Semantics changes": unlink, `delete --cleanup-only` and the renamed hint, `hubCheckHint`, the list prompt without the disable choice, hubenv.go ignoring `hub.enabled`, provision.go's `hubEnabled` computation. Local mode still exists in this phase. The unlink output still mentions `scion hub disable` as the route to local mode, until phase 3 removes it.
Tests:
- update the ~8 hint assertions;
- new tests for `delete --cleanup-only`: the hub still has the agent → refuse; the hub has no agent → files removed; hub unreachable → refuse without `--force`; `--preserve-branch` respected;
- unlink writes only `linked` and `projectId`;
- hubenv: a project with `hub.enabled=false` still gets the broker-resolved endpoint;
- the list prompt has two choices.
Acceptance:
- No user-facing string tells the user to run `--no-hub` or `hub disable` as a remedy for a hub error or a failed cleanup (`git grep -n "no-hub delete\|scion hub disable" -- cmd pkg ':!*_test.go'` shows only the hub disable command itself).
- After `hub unlink`, `scion list` in that repo refuses with the re-link pointer when the hub is enabled globally.
- Amendment 1, checked in Phase V run 2:
  - a created-only local agent shows `created` on the hub after `hub link`, and no container is started;
  - a suspended local agent shows `stopped` or `created`, not `running` and not `error`, and no new container is started. Its exited leftover container is removed by the import, before the create (clarified on 2026-10-11, see below). `scion start <name>` and `scion resume <name>` then start it;
  - a running agent is still adopted with the same container ID;
  - in all three cases the worktree and branch are unchanged.

### Phase 2b: credential parity on a workstation broker (M)

Mechanism (clarified 2026-10-10). The harness auth metadata (`authMeta`) is only known inside `pkg/agent` `Manager.Start` (run.go ~960). On start and restart, `buildStartContext` has no harness config. So the policy decision and the file gathering live in different layers:

1. **Eligibility (cmd → broker config).** A new `runtimebroker.ServerConfig.HostCredentials bool`. cmd/server_foreground.go sets it, next to `HubToken: devAuthToken`, to `config.HostCredentialsEligible(hostedMode, devAuth)`: workstation mode with dev auth enabled. Do **not** infer it from `HubToken != ""`. That field means "token for hub API calls", and a future non-dev use of it would silently widen host-credential access. The user setting `use_host_credentials` is **not** frozen into the server config at startup. It is re-read on every start (step 2), so turning it off takes effect on the next start or restart without restarting the server.
2. **Per start (pkg/runtimebroker/start_context.go).** `colocatedHostCredentials(...)` returns true only if all of these hold:
   - `s.config.HostCredentials`;
   - `use_host_credentials` is effective, re-read now (unset = true; any read error fails closed);
   - the request is verified as coming over the co-located connection (an HMAC mismatch, or an ambiguous fallback with 2+ connections, is refused);
   - the hub endpoint host is loopback. The loopback check is defence in depth. The result goes into a new **non-persisted** `api.StartOptions.HostCredentialFiles bool`, in the same style as `ResolvedKubernetesServiceAccountName`. It is re-evaluated on every start, restart and resume and is never written to agent config.
3. **Gathering (pkg/agent/run.go, BrokerMode only, just before `OverlayFileSecrets`).** If `opts.HostCredentialFiles`, call a new exported `harness.HostCredentialFiles(authMeta, home)`, split out of `gatherConfigFiles` (pkg/harness/auth.go). `home` is the broker process's `os.UserHomeDir()`, which is the user in workstation mode. The returned files are appended as file secrets (Name = required-file name, Target = container home + TargetSuffix, Source = runtime_broker), **unless** a resolved secret with the same name **or** the same target already exists. Hub secrets win.
4. Host injection skips ADC, which stays with `auto_inject_gcloud_adc` (next point). A `target_suffix` that escapes the container home (absolute or `..`) is rejected.
5. Unchanged: `localSources` stays false, so no host env vars are read. The ADC / `auto_inject_gcloud_adc` block in start_context stays as is (`true` still implies ADC).
6. Log the injected file **names** (never contents) at info level, once per start.

Not in scope: the workstation admin-settings allow-list in pkg/hub. `use_host_credentials` is a machine settings key, edited through `scion config set` or the file.

Tests:
- start_context: policy off → false; co-located and loopback and policy on → true; non-co-located → false; non-loopback endpoint → false.
- run.go: flag set → files appended; same-name or same-target hub secret wins; flag unset → nothing; non-BrokerMode path unchanged.
- harness: `HostCredentialFiles` returns only existing files.
- schema test for the new key.
- A cmd-side unit test for the policy computation, if it is factored into a pure helper. That is preferred, so the cmd line stays trivial.

Acceptance:
- On a workstation server, an agent whose harness is logged in only through a home-dir credential file authenticates without any `scion secret` step (Phase V run 2 (d)).
- A hosted broker, or a workstation with `use_host_credentials: false`, injects no host files.
- Restarting an agent after turning the setting off injects nothing.

### Phase 3: the flip (L)

Scope:
- `requireHub` replaces `CheckHubAvailability*` and never returns nil. No endpoint → auto-start or `errHubNotConfigured`. A local workstation endpoint that does not answer → auto-start. `hub.linked=false` → `errUnlinked`.
- Every `hubCtx == nil` site (about 40 in about 25 files) is deleted: the local branch below it becomes unreachable and is left for phase 4, except where the branch is a "requires Hub mode" guard, which is deleted now.
- `EnsureHubReady` drops `NoHub`, the `IsHubLocalOnly` error, the `!IsHubEnabled() && !hubContext → nil` return (pkg/hubsync/sync.go:242-301) and the per-command sync step. Auto-link on the local hub becomes unconditional (not gated on hub being enabled). During the import release, auto-link runs the import when unknown agent dirs exist.
- `IsHubEnabled` is replaced by `HasHubEndpoint` in the 14 cmd callers, hubsync, and the gates in artifact.go, notifications.go and broker.go. `hub.enabled` and `hub.local_only` become deprecated in the schema (D9).
- `--no-hub`, `hub enable` and `hub disable` become hidden and error. hub_ref_enable.go (whole file), hub_auth_endpoint.go:163-175, clean.go:107-114, the project.go:221-245 gates and the completion_helper.go:116-137 gates are deleted. project_gate.go:146-161's `noHub` branch is deleted. The `hub status` rendering is rewritten.
- The templates silent fallback is removed. The "hub enable" help-text sweep is done.
- The migration message (above) is printed once per project, recorded by `hub.linked` being set.
- Test harness: a `cmd` test helper `withFakeHub(t)` (httptest server: health, project lookup and register, empty agent list; sets `SCION_HUB_ENDPOINT` plus a dev token in a temp HOME). `TestMain` in cmd sets `SCION_HUB_AUTO_START=0` so that no test ever spawns a server. root_test.go's usage-error tests (~450 LOC) move from `--no-hub` to `withFakeHub`. Tests that relied on "no hub configured ⇒ local" are rewritten against the fake hub or deleted with their local subject in phase 4. Expect hidden churn beyond the counted 2.2k LOC (findings, risk 7); budget for it here.

Tests:
- `requireHub` table test: no endpoint and auto-start off → error; no endpoint and auto-start on → `ensureLocalServer` called (stubbed); unreachable remote → error with hint; unreachable loopback workstation → auto-start; linked=false → errUnlinked; local hub and unregistered → auto-linked; remote and unregistered → prompt.
- `--no-hub`, `hub enable` and `hub disable` print their guidance and exit non-zero.
- Templates show a hub error instead of falling back.
- The smoke job is extended to call `scion list` with no prior `hub link` (auto-link path).

Acceptance:
- `git grep -n "hubCtx == nil" -- cmd ':!*_test.go'` returns nothing.
- `git grep -n "IsHubEnabled\|IsHubLocalOnly\|NoHub:" -- cmd pkg/hubsync ':!*_test.go'` returns nothing.
- With the phase-1 preconditions (initialised machine and repo, runtime, image_registry, no endpoint), `scion start a1 "task"` in the repo auto-starts, links and starts the agent.
- With an existing local-mode project and agent dirs, the first command imports them and prints the migration message once.
- `make ci` green; the smoke job green.

### Phase 4: delete the local tails (M)

Scope: delete every now-unreachable local-only symbol from findings Appendix A, as listed under "Deletion inventory" below. Delete their tests (~37 functions / ~1.3k LOC) and the remaining `noHub` test plumbing. Remove `logs --follow`. Make the direction required for `sync <agent>`. Drop `displayAgents`'s `hubMode=false` branch.
Tests: the deletions; `go vet` and staticcheck/golangci-lint scoped to cmd show no unused symbols left behind.
Acceptance:
- `git grep -n "agent.NewManager(" -- cmd ':!*_test.go'` shows only server_foreground.go and project_prune.go.
- `git grep -n "noHub\|--no-hub" -- cmd` shows only the hidden stub and its test.
- `make ci-full` green.

### Phase 5: docs (M)

Scope: see "Docs". It lands in the same release as phase 3. If it merges first, the pages describe the new flow as "coming in vX.Y". The preferred order is to merge it immediately after phase 3.
Acceptance:
- The docs site builds.
- `git grep -n -i "no-hub\|local-only mode\|local mode\|hub disable\|hub enable\|solo mode" -- docs-site README.md AGENTS.md GLOSSARY.md` returns only the migration/reference page and historical notes.
- The getting-started tutorial, followed literally on a fresh machine, works (checked by reading it against the code).

### Phase 6: engine and package cleanup (M)

Scope, one release after phase 3:
- pkg/hubsync: delete the reconcile (SyncResult and Exclude 62-147; watermark helpers 633-760; CompareAgents, ExecuteSync, GetLocalAgents and related 761-1197; ShowSyncPlan, ShowSyncAfterLinkPrompt, ShowCheckHubAnywayPrompt, ShowProjectLinkOrDisablePrompt in prompt.go) and the cmd callers of the watermark (cmd/common.go:1536, create.go:433, delete.go:314-316). `hub link` no longer offers the import.
- pkg/config: delete state.go and the `Enabled`, `LocalOnly` and `LastSyncedAt` fields with their accessors and v1 mirrors; keep the deprecated schema properties (D9).
- pkg/agent: delete `SendKeysLocal` (manager.go:1019-1057) and the `!BrokerMode` branches; remove `BrokerMode` from `api.StartOptions` along with the context helpers; give `List` an explicit project scan (broker passes its projects; heartbeat.go:579 updated).
- pkg/runtime: delete the gcloud host-dir mount (common.go:493-510).
- cmd: delete the hidden `--no-hub`, `hub enable` and `hub disable` stubs (XS).
- Optional: move the remaining ~1.75k LOC of hubsync bootstrap into a `pkg/hubcli` package. This is a pure move with no behaviour change, so it is a separate PR.

Tests: pkg/hubsync tests for the deleted code (most of ~4.1k LOC) are deleted. pkg/agent tests that set `BrokerMode: true` drop the field. Tests that covered non-broker branches are deleted. New: a heartbeat test that `List` scans exactly the broker's projects and not the cwd.
Acceptance:
- `git grep -n "BrokerMode\|IsHubExplicitlyDisabled\|LastSyncedAt\|LocalOnly\|SendKeysLocal" -- pkg cmd ':!*_test.go'` returns nothing.
- Broker unit tests and the smoke job are green.
- A broker started from a directory inside some unrelated project does not report that project's created-only agents.

## Alternatives considered

**Ergonomics: how a hub comes to exist for a local user**
- **Chosen: auto-start the existing workstation daemon as a subprocess.** It reuses a shipped path (daemon, readiness probe, endpoint write, onboarding). Agents outlive the CLI invocation, which they must.
- **Rejected: embed a hub in the CLI process for the duration of a command.** Agents run for hours and keep calling the hub (status, messages). An in-process hub dies with the command, so an embedded hub would need its own daemonisation, which recreates the workstation server.
- **Rejected: require an explicit `scion server start` and fail clearly otherwise.** It is the simplest design, and it is the fallback when `hub.auto_start: false`. But it makes the removal a strict regression for the first-run path, which is the main user-facing cost the maintainer asked to avoid. It stays one setting away.
- **Rejected: auto-install a systemd/launchd unit on first use.** It writes to service-manager configuration without being asked. It is suggested in a one-line hint instead.

**Linking**
- **Chosen: auto-link only on the local workstation hub, keep the prompt for remote hubs.** On a shared hub a link is visible to others and can collide with an existing project; the existing matching-project prompt handles that.
- **Rejected: auto-link everywhere.** It silently registers projects on shared hubs.
- **Rejected: keep explicit `hub link` always.** It is one more required step for every repo, for no benefit on a single-user hub.

**Overall approach** (from the findings' options, refined)
- **Chosen: phased (findings option B).** The changed semantics land and are validated before anything is deleted.
- **Rejected: single hard removal (option A).** The review surface is too large, and the semantic redesign would be rushed inside a deletion.
- **Rejected: keep a read-only local mode (option C).** It keeps the nil contract, `--no-hub` and the test plumbing for little value, and `scion doctor` plus `docker ps` cover the "hub is down" diagnostic.

**Unlink**
- **Chosen: `hub.linked=false`, with agent commands refusing.** Unlink keeps a defined meaning ("this directory is not a hub project") without a local mode to fall into.
- **Rejected: unlink deletes `.scion`.** It destroys agent homes; `scion clean` already exists for that.
- **Rejected: unlink is a no-op alias of "forget project ID".** Auto-link on the local hub would immediately re-link, so unlink would do nothing observable.

**Delete recovery**
- **Chosen: `delete --cleanup-only`.** It sits next to the command whose cleanup failed and is easy to suggest in a hint.
- **Rejected: point users at `scion clean`.** That resets the whole project, which is far too broad for one agent's leftover worktree.

**Credential parity**
- **Chosen: co-located file-secret injection, generalising the existing ADC injection.** It keeps the broker's rule that `localSources` is never true, and adds exactly one, scoped exception that already has a precedent.
- **Rejected: `localSources=true` for co-located brokers.** It reopens the operator-credential leak path that `localSources=false` closes for every env var, not just harness files.
- **Rejected: require users to upload hub file secrets.** It is a regression for every subscription-login user.

## Deletion inventory mapped to phases

Line numbers are from ae24a3c and from the findings' Appendix A and B. "Stays" means the code remains; it is listed so that nothing is silently kept or lost.

### cmd/

| Symbol / lines | Phase |
|---|---|
| skills.go / skill_registries.go nil-deref sites (13) | 0 |
| hub.go unlink body (2748-2820): semantics change | 2a |
| delete.go `noHubDeleteCommand` 320-328 (→ `cleanupOnlyCommand`), comment 199-207; stop.go:619 hint | 2a |
| common.go `localOnlyHint`, `shouldSuggestLocalOnly` 473-476, 598-607 (→ `hubCheckHint`) | 2a |
| list.go `handleUnlinkedProjectPrompt` disable branch (~10 LOC) | 2a |
| list.go `handleUnlinkedProjectPrompt` remaining body (1061-1143) | 3 |
| common.go `CheckHubAvailability*` 339-412 (→ `requireHub`), the "or use --no-hub" text at 672 | 3 |
| root.go `noHub` 30, flag 426 (→ hidden stub), `IsHubEnabled`/`IsNoHub` 529-537, `GetHubEndpoint` branch 505-507, dev-auth warning branch 555; root_options.go:37 | 3 (stub deleted in 6) |
| hub.go `hubEnableCmd`/`runHubEnable` 226-240, 2227-2296; `hubDisableCmd`/`runHubDisable` 242-251, 2298-2370 (→ hidden stubs); status enabled-scope rendering 646-697, 766, 846-848 | 3 (stubs deleted in 6) |
| hub_ref_enable.go (whole file, 92 LOC) and hub_ref_enable_test.go | 3 |
| hub_auth_endpoint.go 163-175 | 3 |
| clean.go 107-114 | 3 |
| completion_helper.go `--no-hub` check 116-120, `IsHubEnabled` gate 137 (local dir scan 69-90 is low-value: deleted in 4) | 3, 4 |
| project.go `promptHubRegistration` gates 221-245 (→ `requireHub`, warn on failure) | 3 |
| project_gate.go `noHub` branch 146-161 | 3 |
| templates.go silent fallback in list/show/delete/clone; `printTemplateListLocalMode` 180-219 is renamed and reused for the new `templates list --local` | 3 |
| artifact.go:296, notifications.go:204 (`requireHubClient`), broker.go:524, 640: gates become `HasHubEndpoint`/`requireHub` | 3 |
| ~75 LOC of "requires Hub mode" guards across ~25 files (restore, reincarnate, reset-auth, set-message-mode, schedule, secret, validate, templates sync/pull/status/update, harness-config sync/push/delete/update, pre-start-hook, project/user skills, project `sync`, message guards 390-420 and 462-476, list filter rejection 127-157) | 3 |
| common.go RunAgent local tail 830-1012, `localResumeDecision` 700-728, `waitForTmuxSession` 1015-1040 (if no other callers), `localAgentStartEnv` 115-124 | 4 |
| list.go `listAgentsLocal` 159-186, local tail 119-123, `displayAgents` hubMode=false branch | 4 |
| stop.go local tail 86-123, `stopAllAgents` 128-286 | 4 |
| suspend.go local tail 91-116, `checkHarnessResumeSupport` 119-136, `suspendAllAgents` 138-281 | 4 |
| delete.go `--force` warning 95-100, local branches 104-155, `deleteStoppedLocal` 441-527, `textOutf` 528-537, `deleteAgentLocal` 539-590 | 4 |
| attach.go local tail 106-157, `localAttachNotFoundFormats`/`localAttachError` 160-187 | 4 |
| logs.go local tail 64-95 and the `--follow` flag | 4 |
| look.go local tail 77-85 | 4 |
| keys.go `classifyLocalKeysError` 292-328, local send path 391-589, `shellQuoteIfNeeded` if unused | 4 |
| broadcast.go `broadcastLocal` 163-213 and its constants 158-159 | 4 |
| message.go local tail (`mgr.Message`) 478-492 | 4 |
| create.go local tail 84-170, `withLocalSkillResolution` 172-218, `localCreateResult`/`writeLocalCreateResult` 270-287, `skillResolverHubOptions` 490-502 | 4 |
| sync.go local tail 257-261 and `SyncUnspecified` | 4 |
| harness_config_install.go `installLocally` 217-268 and the fallback at 111 | 4. It is a no-hub fallback (harness_config_install.go:107-111), not one of D5's explicit `--local` views. `harness-config install` always installs to the hub. |
| common.go / create.go / delete.go hubsync watermark calls (common.go:1536, create.go:433, delete.go:314-316) | 6 |
| **Stays:** `loadInlineConfig` and related, `validate*`, `CheckAgentsGitignore`, `confirmStopAllRm`, `keysResult` and related, `requireAttachTerminal`, `buildLookCmd`, `buildBroadcastMessage`, `cleanupAfterHubDelete`, `findLocalTemplates`, `cloneLocalTemplate`, `GetHubEndpoint` (minus `noHub`), server_foreground.go:3205-3212, project_prune.go:150-167, doctor.go:179, cdw.go | — |

### pkg/

| Symbol | Phase |
|---|---|
| pkg/runtimebroker/hubenv.go:247 `IsHubExplicitlyDisabled` check | 2a |
| pkg/agent/provision.go:1835 `hubEnabled` computation | 2a |
| pkg/runtimebroker/start_context.go co-located credential injection (extension, not a deletion); pkg/harness/auth.go helper export | 2b |
| pkg/hubsync/sync.go `EnsureHubReady` NoHub/LocalOnly/not-enabled returns (~242-301), per-command sync step (~545-600); `EnsureHubReadyOptions.NoHub` | 3 |
| pkg/config/settings.go `IsHubEnabled` (→ `HasHubEndpoint`), `IsHubLocalOnly`; schema keys marked deprecated | 3 |
| pkg/hubsync reconcile: SyncResult/Exclude 62-147, watermark 633-760, CompareAgents/ExecuteSync/GetLocalAgents and related 761-1197; prompt.go ShowSyncPlan 101-154, ShowSyncAfterLinkPrompt 451, ShowCheckHubAnywayPrompt 474, ShowProjectLinkOrDisablePrompt 396-448 | 6 |
| pkg/config/state.go (whole file); settings.go `Enabled`, `LocalOnly`, `LastSyncedAt` fields and `IsHubExplicitlyDisabled`; settings_v1.go mirrors and v1 migrations for them | 6 |
| pkg/agent/manager.go `SendKeysLocal` 1019-1057 | 6 |
| pkg/agent/run.go and provision.go `!BrokerMode` branches (run.go:156, 242, 996-1055, 1285, 1314, 1452-1458, 2309, 2960, 3049; provision.go 1190-1270 and the others); `hubDisabled` | 6 |
| pkg/agent/broker_hub_env_authority.go `BrokerMode` reference; pkg/api/types.go `BrokerMode` and context helpers; pkg/runtime/interface.go reference | 6 |
| pkg/agent/list.go empty-filter cwd/global scan 66-80 (→ explicit projects); pkg/runtimebroker/heartbeat.go:579 caller | 6 |
| pkg/runtime/common.go:493-510 gcloud host-dir mount | 6 |
| **Stays:** pkg/agent Manager and everything else in pkg/agent; `provision.go StopProjectContainers` 470-510 (used by `project prune`, host maintenance); pkg/runtime (all of it apart from the mount above), including `GetRuntime(projectPath)`; pkg/config project resolution, init, embeds; pkg/hubsync link/register/client bootstrap (~1.75k LOC) and resolve.go; agentcache, templatecache, messages | — |

**What becomes dead in the shared code.** In pkg/agent, the non-broker branches and `SendKeysLocal` (phase 6). In pkg/runtime, one mount block. In pkg/config, the mode fields and state.go. Nothing in the Manager interface itself becomes dead, because the broker uses all of it. `runtime.GetRuntime(projectPath)` keeps cmd callers in attach and logs for hub mode, so it stays.

### Tests

| Group | Phase |
|---|---|
| Phase-0 table tests (new) | 0 |
| hint assertions (broker_failure_test 104/160/167/188, message_notfound_test 59/148, message_thread_2026_test:141, common_hubhint_test) | 2a |
| hub_ref_enable_test, cli_mode_test:152 and project_gate_test:33 references to enable/disable, root_test usage-error tests rewritten to `withFakeHub` (~450 LOC), templates_test fallback tests, implicit "no hub ⇒ local" tests rewritten | 3 |
| keys_test local cases (16 funcs), create_skill_resolution_test (whole file), common_resume_test (whole file), json_mode_output_test `TestDeleteStoppedLocal_JSON`, attach_cli_fixes_test local cases, delete/list/create local tests, harness_config_install_test `installLocally`, agent_log_level_test local case; remaining `noHub=true` plumbing in delete/message/attach/create/json_mode tests | 4 |
| pkg/hubsync reconcile tests (most of ~4.1k LOC); pkg/config state and hub-mode field tests; pkg/agent non-broker tests and `BrokerMode` fields | 6 |

### Scripts

hack/test_oauth.sh:51-65 and hack/merge-work.sh:48,65 use the local CLI. They are updated in phase 4: they work unchanged once auto-start exists, but their comments describe local mode. hack/workstation-smoke.sh is added in phase 1.

## Risks

| # | Risk | Mitigation |
|---|---|---|
| R1 | Auto-start spawns a daemon in surprising places (CI scripts, tests, other users' shells on shared machines). | Never inside agent containers. Opt-out setting and env var. cmd `TestMain` disables it. Dev auth binds loopback only, and per-user HOME keeps the server per-user. The first auto-start prints how to stop or disable it. |
| R2 | Credential regression for subscription-login users. | Phase 2b lands before phase 3; phase V step 6 verifies it. |
| R3 | Running local containers are not adopted. | Phase V decides; the migration message and release notes cover both outcomes. |
| R4 | Hidden test churn in cmd beyond the counted 2.2k LOC (tests that implicitly ran local paths). | Budgeted in phase 3 with a single `withFakeHub` helper. If phase 3 exceeds L, split it into 3a (contract plus test harness) and 3b (mode-switch commands and prompts). |
| R5 | macOS Apple container reachability. | Documented (D4). Auto-detect prefers podman/docker. The Apple broker PTY path is marked untested (pkg/runtimebroker/pty_handlers.go:102-110); phase V should include an Apple run if a Mac is available (open item). |
| R6 | Footprint of the always-on server on small machines. | Accepted. `scion server stop`. Idle RSS measured as an open item. |
| R7 | Users with a remote endpoint and `hub.enabled=false` (chose local deliberately) are now routed to the remote hub. | The migration message. `hub status` shows the endpoint source. Auto-link does not run against remote hubs (prompt only). |
| R8 | Users who unlinked to get back to local mode hit `errUnlinked`. | The `errUnlinked` text says "Local-only mode was removed; run `scion hub link` to manage agents for this project." |
| R9 | No end-to-end coverage of the replacement path today. | Phase-1 smoke job, extended in phase 3. |
| R10 | Port conflicts or a stale PID file break auto-start. | Auto-start surfaces the server's own errors (cmd/server_daemon.go:183-197) and does not retry or force-kill. |

### Rollback

- Phases 0, 1, 2a and 2b are additive or semantic and revert cleanly.
- **Phase 3 is the user-visible flip.** Reverting it restores local mode as long as phase 4 has not merged. That is why phase 4 should wait at least a few days after phase 3, until there is enough signal.
- After phase 4, rollback is a revert of 4 and then 3.
- Phase 6 is cleanup. Reverting it only matters if the import is needed again, so keep it a separate PR from the optional hubsync move.
- Settings stay compatible in both directions (see Migration, "Rollback for a user").

## Docs

Phase 5. It lands in the same release as phase 3. Pages are from the findings' docs inventory.

| Page | Change |
|---|---|
| getting-started/tutorial.md | Rewrite to the new first-run flow: install, `scion start` in a repo (auto-start, auto-link), `scion list`, the web UI URL, `scion server install` optional. |
| getting-started/install.md (8, 59) | Remove the "CLI-only (Local mode) setup" route; add the container-runtime prerequisites and the Apple container DNS note. |
| choosing-a-mode.md | Remove the Local tier. The spine becomes Workstation (local server) and Hosted (team hub); "how to choose" is updated. |
| onboarding.md:11 | Remove the redirect for Local-mode users. |
| overview.md:24,49; concepts.md:121-126; glossary.md:269-275 | Remove local mode; keep the worktree path explanation (still true under the broker). |
| local/ section (12 pages) | Re-frame as "Workstation" or "Agents on your machine". workspace.md:161-182 (`hub disable`/`--no-hub`) and local-governance.md:8 ("Solo Mode") are rewritten. Runtime, template, tmux and credential material is kept. agent-credentials.md documents `use_host_credentials` (2b). |
| hosted/user/coding-agent-cli.md:51-108 | Remove the `--no-hub` local actions. |
| reference/cli.md (16, 56, 257, 344, 436-450, 662) | Remove `--no-hub`, `hub enable`/`disable` and local `keys`; add `delete --cleanup-only`, `hub.auto_start`, and the new `hub unlink` semantics. |
| reference/ (new page) local-mode-removal.md | The migration page that the CLI messages link to: what changed, first-run behaviour, import, running containers (per phase V), opt-outs, rollback. Modelled on reference/grove-removal.md. |
| contributing/architecture.md "Solo Mode" (257-~293) and 14, 25, 64, 91, 613 | Rewrite: one CLI architecture, with the broker as the only executor. |
| Minor mentions (messaging-authorization, orchestrator-settings, api, times-and-timezones, workstation/git-projects, hosted/ha/runtime-broker) | Sweep. |
| AGENTS.md:48-60 "Solo/Local Architecture" | Replace with "Workstation architecture": local server, auto-start, worktrees still at `<repo>/.scion/agents/<agent>/workspace`. Keep the agent status notes, re-pointed. |
| GLOSSARY.md:366-377, README.md:94,137 | Retire "Local mode" as a canonical term (mark it historical); update the README quickstart. |
| .design/ hosted/hosted-architecture.md §7.1, hosted/sync-design.md, workspace-sync.md, project-skills.md, hosted/hosted-templates.md, scion-plugins.md, workstation-server.md, workstation-onboarding.md | Add a one-line "local-only mode removed; see <this design>" note where local mode is described as current. Archived designs are not touched. This design is filed as `.design/local-mode-removal.md` once approved. |

## Open items

1. ~~Maintainer answers~~: D1-D7 and D13 are all approved (2026-10-10). Nothing is pending from the maintainer.
2. **Setting names** `hub.auto_start` and `use_host_credentials`, and the flag name `--cleanup-only`: naming nits for review.
3. **Idle footprint** of the workstation server (RSS, CPU), to inform R6. Measure during phase V.
4. **Apple container under the broker**: include a macOS run in phase V if hardware is available; otherwise D4 stands on documentation only.
5. **The broker's own delete cleanup.** On a co-located broker, check whether the broker already removes the worktree on delete. If it does, the CLI-side `cleanupAfterHubDelete` and `--cleanup-only` could later be reduced to remote-broker cases only. That would be a follow-up, not part of this plan.
6. **sciontool's no-hub mode** (D6): a separate decision.
7. **Optional hubsync move** to `pkg/hubcli` after phase 6.
8. **Fresh-install preconditions, out of scope (Amendment 2).** These match today's local-mode baseline and are not regressions of this plan; they are listed for triage:
   - `scion init` and `init --machine` require a detected container runtime (`DetectLocalRuntime`) even when only a remote hub will run the agents.
   - Commands outside a project need `--global`; there is no automatic fallback to the global project.
   - `image_registry` must be configured. The broker fails fast without it, which is deliberate: otherwise image pulls fail with opaque 404s at dispatch.
9. **From Phase V run 1, adjacent and not in scope (for triage):**
   - The co-located workstation broker is labelled "Hosted Broker" in `hub brokers`/`list`. After removal every workstation user sees it, so a "Local broker" default name in workstation mode is worth a small follow-up.
   - `init --machine` auto-picks podman over docker when a user podman service exists. That matches the documented detect order, but it surprised the tester.
   - `create` has no `--no-auth` while `start` has one; check the hub `create` path for parity.
   - `--no-hub start` prints an empty `(ID: )`. It disappears in phase 4; no fix needed.

## Amendments

### Amendment 1 (2026-10-10): the import keeps non-running agents not running

Source: Phase V run 1. A running local agent is adopted in place. Created-only and suspended agents were **started** by the `hub link` import, because `ExecuteSync` sends create plus start for every agent.

Why it is in scope: it is a migration-correctness fix to the import that D3 already keeps for one release. It adds no new behaviour. Without it, the first command after upgrading would silently start every dormant agent in every repo, re-sending its task. That costs LLM spend and changes worktrees. A warning in the migration message was rejected for that reason: by the time the user reads it, the agents are already running.

Change:
- Phase 2a gains a separate pkg/hubsync-only commit. Non-running agents are imported with `ProvisionOnly`; running agents are unchanged.
- The migration message now says running agents are adopted and other agents are imported as stopped or created.
- Phase 3 depends on this commit and on Phase V run 2.
- Phase 6 still deletes the import together with this fix.

Size: phase 2a stays M (this adds about 40 LOC plus tests).

### Amendment 2 (2026-10-10): phase-1 acceptance restated; fresh-install preconditions

Source: running the phase-1 slice on a fresh HOME. Three findings:
- the auto-started server exits without `image_registry`;
- a repo without `.scion` is not a project, and `init` needs `init --machine` plus a container runtime;
- outside a project, commands need `--global`.

Correction: the "Replacement ergonomics" table claimed local mode does an implicit `init`. It does not. Today's local-mode first run is `init --machine` (once, needs a runtime and an image registry), then `init` (per repo), then `start`, with the registry checked at agent start. The table now states that baseline. The target state keeps it unchanged and adds only auto-start and auto-link. The goal is still met: the first-run path is no longer than it is today.

Ownership:
- **Registry precondition → phase 1.** `ensureLocalServer()` preflights the registry check in the CLI and fails before spawning. This is ensureLocalServer's own error UX, not a change to other code. The broker's fail-fast at startup stays.
- **`init` without a runtime, and the project gate's `--global` requirement → out of scope.** Both are unchanged baseline behaviour (open item 8). No phase changes them.

Phase-1 acceptance, the phase-3 acceptance and Phase V step 5 are restated with these preconditions, and the `--global` variant is added.

### Amendment 3 (2026-10-10, ACCEPTED by maintainer 2026-10-10): no auto-start

Trigger: the maintainer questioned auto-start: "not sure autostart is a key feature. or should divert to onboarding setup wizard in hub if not run before." This delta specifies that variant. **Accepted. It supersedes D2's first sentence** ("the CLI auto-starts it…") and every auto-start mention earlier in this document: Summary, Target state, Replacement ergonomics, Migration message, phases 1 and 3, and Risks R1 and R10. Where those sections conflict with this amendment, this amendment wins. D2's second sentence (no-prompt auto-link on the local hub) is unaffected.

**Behaviour in the variant**
- **No hub endpoint configured:** any hub-needing command fails with one line: `No hub configured. Start the local hub with 'scion server start' (first run opens setup), or set a remote one with 'scion config set hub.endpoint <url>'.`
- **Endpoint is the local workstation endpoint but nothing answers:** `Local scion server is not running. Start it with 'scion server start'.`
- **First run of `scion server start` opens the onboarding wizard.** Today it opens `/onboarding` only when the global settings file did not exist before start (`needsOnboarding`, cmd/server_daemon.go:228). Every local-mode user has run `scion init --machine`, so their settings file exists and that signal misses them. The variant changes the signal: once the server is ready, `server start` asks the hub's onboarding status (`GET /api/v1/system/status`, `Complete`, pkg/hub/system_handlers.go:453-545). If it is not complete, the CLI prints and opens `/onboarding`. It uses the existing browser rule: interactive, not headless, `SCION_NO_BROWSER` unset (cmd/server_daemon.go:1003).
- **A missing registry must not kill the server.** Today the server exits at startup without `image_registry` (`requireImageRegistryForBroker`, cmd/server_foreground.go:534-536). The wizard is the place to set it (its registry endpoint is in pkg/hub/system_handlers.go), so the wizard has to be reachable. In workstation mode only, a missing registry no longer exits the server:
  - hub and web start normally;
  - the broker is not started, and a warning is logged and printed;
  - `server start` prints the registry fix next to the wizard URL.

  The broker starts once the wizard sets the registry. Whether that can happen without a restart depends on the existing broker re-init path (server_foreground.go:3510, "engine without a full server restart"). Otherwise the wizard tells the user to run `scion server restart`; that was left to confirm during implementation. **Confirmed (phase 1b):** the re-init path only swaps the engine of a broker that is already running; it cannot start one that was skipped. The remedy is `scion server restart`, stated in three places:
- the CLI output and the server log;
- the wizard: after a registry save, when `status.embeddedBrokerID` is empty, it shows "Run `scion server restart` to start the broker" (small onboarding.ts change in the same PR);
- the linked-workspace step's "No embedded broker available" error gets the same hint.

If the onboarding-status call fails, or the server is not ready, the wizard trigger falls back to the legacy signal (settings file missing before start). Hosted mode keeps the fail-fast. This is the same as Amendment 2's open item, resolved the other way: the wizard, not a CLI preflight.

**Phase-1 pieces (phase-1 PR) in the variant**

| Piece | Fate |
|---|---|
| `isLocalWorkstationEndpoint` | **Survives.** It gates auto-link, the link-prompt skip, and the choice between the local and remote error text. |
| `autoStartAllowed`, the `hub.auto_start` setting and schema key, `SCION_HUB_AUTO_START` | **Removed.** Nothing auto-starts. The key was never released, so drop it from the schema with no deprecation. |
| `ensureLocalServer` (subprocess `server start`, readiness wait, settings reload) and its registry preflight | **Removed.** |
| `hub link` auto-start when there is no endpoint | **Removed.** `hub link` with no endpoint prints the one-line not-configured error. |
| Auto-start rollback (cleanup after a failed auto-start) | **Removed** with `ensureLocalServer`. |
| Auto-link on the local hub (`AutoLinkLocal`) | **Survives** unchanged (D2, second sentence). |
| Link-prompt skip for explicit `hub link` on the local endpoint | **Survives** unchanged. |
| Smoke script and CI job | **Survive, rewired.** The new sequence:<br>1. with no endpoint, `scion hub link` prints the one-line error and exits non-zero, with no daemon started;<br>2. `SCION_NO_BROWSER=1 scion server start`, and the output contains the `/onboarding` URL (onboarding not complete);<br>3. `scion hub link` links with no prompt;<br>4. `scion hub status` shows linked;<br>5. `scion list` is empty through the hub;<br>6. `scion server stop`.<br>Optionally add a no-registry case: `server start` stays up, prints the fix, and `hub status` is reachable. |

Recommended landing: shrink the current phase-1 PR by deleting the removed pieces, keeping the survivors and rewiring the smoke test, then merge it. Add **phase 1b (S, ./cmd)** for:
- the onboarding-status check on `server start`;
- the workstation-mode registry degrade.

This keeps the reviewed PR a pure subtraction and puts the new behaviour in its own review.

**Phase 3 in the variant**
- `requireHub` loses both auto-start branches:
  - no endpoint → the not-configured error;
  - a local endpoint that does not answer → the "not running" error;
  - remote unreachable → unchanged.
- Everything else in phase 3 stays: no nil contract; the hidden erroring `--no-hub` and enable/disable; no per-command reconcile; auto-link plus import on the local hub; the template fallback removed.
- The `TestMain` auto-start guard is no longer needed. `withFakeHub` stays.
- The migration text for a local user's first command after upgrading becomes: `Local-only mode has been removed: scion now runs agents through a hub. Run 'scion server start' (the first run opens setup), then rerun this command; this project links automatically and existing agents are imported. See <docs>/reference/local-mode-removal.` After that, the first command against the running local hub prints the import lines from the Migration section (Amendment 1).
- `scion init`: if a hub answers, it links. Otherwise it prints the `server start` pointer as a note, not an error.

**Phase 2b:** not affected. Host credentials depend on a co-located broker, not on how the server was started. Showing `use_host_credentials` in the wizard is a possible follow-up, but it is out of scope, because the pkg/hub admin-settings allow-list is out of scope.

**Other sections affected if accepted:**
- Replacement ergonomics: step 4 gains a once-per-machine `scion server start` (plus the wizard) before the first agent command. `server install` is still suggested.
- Risks: R1 (surprise daemons) and R10 (auto-start port conflicts) drop out.
- Phase V run 2 (c) becomes: `server start` → wizard URL shown → `hub link` without a prompt.
- Docs: the tutorial starts with `scion server start`.

**Trade-off.** It costs one explicit command per machine, which is the only lengthening against today's local-mode first run. In return:
- no CLI-spawned daemons and no subprocess or readiness code in the CLI;
- one fewer setting;
- the fresh-install preconditions that auto-start ran into (Amendment 2: registry, images, runtime check, harness setup) get the wizard, which already handles them, instead of a CLI error.

**Decision: accepted.** The wizard covers more of the real first-run friction than auto-start did, and the explicit step is a single command that the error names.

### Amendment 4 (2026-10-12): interaction with the CLI current-context design

Source: the maintainer's direction that, after the current-context work (the separate design "A current hub and project for the CLI"), no hub config exists outside its hub list, and every hub command maps to the current context. The points below align the two designs. They keep phase 3 as designed, with one internal-structure change.

- **One seam for "hub configured".** Phase 3 implements the check as **one** cmd-level function, for example `hubConfigured()`, used at the ~14 former `IsHubEnabled` call sites. Those sites do not call `Settings.HasHubEndpoint` directly. Its body in phase 3 is the D8 rule (endpoint from flag, env or settings). This is internal structure only, with no behaviour change. A later context phase swaps that one function body to "current context hub".
- **Error and stub texts unchanged for now.** Phase 3's `errHubNotConfigured`, the "not running" text, and the hidden `--no-hub` / `hub enable` / `hub disable` stub texts stay as designed (`scion server start` / `scion config set hub.endpoint <url>`). The context design's phase 2 updates them when the hub registry exists.
- **Broker project-settings readers are not phase 3.** The broker-side readers of project settings (for example `hubEndpointFromProjectSettings`, pkg/runtimebroker/hubenv.go) are not in phase 3's scope. They go to phase 6 here, or to the flat-broker work, whichever lands first.
- **Unchanged:** everything else in phase 3, including the `hub.enabled` deprecation and the `HasHubEndpoint` semantics, proceeds as designed. The context design's phase 7 replaces the settings-based hub config later. Settings stay compatible in between.

### Amendment 5 (2026-10-12; resolved by Amendment 6): new agents in a linked repo are cloned, not worktreed; D4 premise corrected

Source: a review of the docs phase against main at d1bd2992.

**Finding 1: workspace regression for new agents (design error).** "Target state" and "Migration" say a linked project keeps its layout and that "the co-located broker uses the same paths". That is true for **existing** agent dirs (adopted or imported; Phase V run 1), but **not for new agents**.
- Auto-link and `hub link` register the project with its git remote and no workspace mode.
- `populateAgentConfig` (pkg/hub/handlers_agent_create_helpers.go:299-312) then sets `AppliedConfig.GitClone` for every git-anchored, non-shared project.
- The broker therefore clones per agent inside the container (pkg/runtimebroker/start_context.go:1064ff). The CLI says so: "local worktrees are not used" (cmd/common.go:1195).
- The `worktree-per-agent` mode (start_context.go:1044-1060) is create-only, is not set by register, and builds its worktree from a host-side base **clone** of the remote, not from the user's checkout.

Compared with local mode, a new agent in a linked repo:
- needs a token for private repos;
- does not see local unpushed commits or uncommitted state;
- puts its branch in a clone rather than in the user's repo;
- gets a separate clone rather than a worktree of the user's checkout (the host directory `<repo>/.scion/agents/<agent>/workspace` still exists, but holds the clone), so `cdw` and local review of agent branches no longer work the same way.

Neither the findings nor this design caught this. It materialises with phase 3, the flip.

Options:
- **(A) Local-worktree dispatch for linked projects. Recommended.** When the dispatch target broker is a provider with a local path for the project (a linked project on that host), the hub does not set `GitClone`. The broker's pkg/agent provisioning then creates the worktree from the provider's local checkout, as local mode did; this is the same engine. It is a hub-side change in `populateAgentConfig` (or at dispatch, where the broker is known), plus broker verification. It touches pkg/hub. Precondition to verify first: that broker-mode `Provision` with `GitClone == nil` and a provider local path yields the local-mode worktree layout.
- **(B) Auto-link and `hub link` register git repos as `worktree-per-agent`.** No token is needed only if the base clone can use local credentials. Agents still do not see unpushed commits, and branches are not in the user's repo. It is a partial fix.
- **(C) Accept and document.** Clone plus `GITHUB_TOKEN`; local/workspace.md already describes this. It is a real UX regression for every former local-mode user.

Recommendation: (A), as a new phase before the phase-3 release. Phase 3 can merge if the maintainer accepts shipping (C) temporarily; otherwise phase 3's release waits for (A). Phase 5 documents actual behaviour (C) and marks the gap, until the decision.

**Finding 2: D4's premise was wrong.** *(Resolved: the macOS order is now podman, docker, then Apple container (#3440), so D4's premise holds again.)* D4 assumed macOS auto-detect prefers podman/docker. The code tries Apple `container` first (pkg/config/runtime_detect.go:34-38: container, podman, docker). Any Mac with Apple `container` installed therefore gets it by default, and with it the root DNS/pf step for container-to-hub reachability, plus the untested Apple broker PTY path (R5). D4's "accept and document" may still stand, but it was approved on an incorrect premise, so it goes back to the maintainer. Options:
- keep D4 and document the root step prominently;
- change the auto-detect order on macOS to podman, docker, container, a one-line change in pkg/config;
- have the onboarding wizard run the apple-dns helper by default.

Docs follow the code order meanwhile.

### Amendment 6 (2026-10-12, ACCEPTED by maintainer 2026-10-12): phase 3a, local worktrees for linked repos (variant L)

Source: a read-only investigation for phase 3a against main at 6dc5708. It verified option A′ from Amendment 5. **A′ as stated is unsafe and is withdrawn.** Using the user's checkout as the `ProvisionShared` base would, on first use:
- detach the user's HEAD;
- write `gc.auto` and `branch.autoSetupMerge` into their `.git/config`;
- chown the repo's parent directory;
- write a sentinel next to the repo;
- create `<repo>/worktrees/<uuid>`.

On docker, delete would also miss those worktrees (UUID vs slug keys). The "reuse the maintained ProvisionShared machinery" rationale in Amendment 5 does not hold.

**Recommendation: variant L.**
- **CLI:** auto-link, `hub link` and `runtime-broker register` label new git projects `worktree-per-agent`, but only when the hub is the local workstation hub.
- **Broker:** a pure decision function (`resolveLinkedCheckout`, pkg/runtimebroker/start_context.go, ahead of the worktree-per-agent block) routes the case to the existing pkg/agent worktree engine when all of these hold:
  - the project is labelled;
  - the provider path is linked (not hub-native and not recovered from a container);
  - the dispatch came over the co-located connection;
  - the runtime is not Kubernetes.

  It then leaves `GitClone` and `Workspace` empty, so `ProvisionAgent`'s worktree case creates `<repo>/.scion/agents/<slug>/workspace`.
- That engine is the one local mode and the adopted agents already use (Phase V run 1). The result:
  - branch in the user's repo, no clone, no token;
  - unpushed commits visible. Uncommitted changes are **not** visible, because a new worktree starts from HEAD (corrected 2026-10-12 from the phase-3a validation run). The exception is the attach case, an agent named after the checked-out branch, which uses the user's own checkout;
  - user's HEAD and config untouched;
  - delete and cleanup already proven.
- **No pkg/hub change.** The hub already sends the provider path (to the linked broker only), the label and `GitClone`.
- **Size M:** about 5 production files.

Variant M (A′ made safe: an "external base" mode in pkg/provision, a parameterised worktree dir, and fixes to every `<base>/worktrees` assumption and to delete keying) is size L, in safety-critical locking/JOIN/cleanup code. **Rejected:** its only gain is cross-process provisioning locks, which local mode never had. Git serialises `worktree add` itself.

**The investigation's four decisions: all accepted as recommended.**
1. **L vs M: L.**
2. **Reincarnate for new linked-repo agents: accept the loss now; pkg/hub follow-up later.** The loss is not an artefact of the label. pkg/agent `Reprovision` refuses any agent with neither `GitClone` nor an explicit workspace (`api.ReincarnateEligible`, pkg/agent/provision.go ~669), so any local-worktree routing loses reincarnate, labelled or not. The label at least makes the hub refuse cleanly (400) instead of failing at the broker (409). This is no regression against local mode, which never had reincarnate; it is one only against today's hub-clone default for linked repos. A follow-up (pkg/hub plus pkg/agent reprovision for local worktrees) goes on the backlog.
3. **Checkout check fails on a co-located broker for a labelled linked project: fail closed with an actionable error.** Causes: remote mismatch, path not a git repo, or git < 2.48. Do not fall through to today's worktree-per-agent path, which would make a full base clone inside `<repo>/.scion/workspace`. Do not silently fall back to clone-per-agent either: the agent would quietly lose local work. Error text per cause, e.g. "this project's checkout at <path> has origin <x> but the hub project is <y>" / "git 2.48 or newer is required for local worktrees (found <v>)". The fail-closed rule applies only when the routing preconditions hold (labelled, linked provider path, co-located). Every other case stays on today's path.
4. **Already-linked projects (immutable label, so they keep cloning): document "re-link to get local worktrees"; the label upgrade is a backlog follow-up.** If 3a lands **before** phase 3, every former local-mode user registers fresh during the phase-3 migration and gets the label. The gap then only affects users who had already linked a repo to a workstation hub by hand.

**Ordering.** 3a does not depend on phase 3, and it **should merge before phase 3**, so the migration links carry the label. Phase 3's merge therefore depends on 3a (which replaces option A as Amendment 5's fix), and phase 3 must avoid the CLI changes that 3a makes.

**Consequences for other phases:**
- **Phase 6** must keep `ProvisionAgent`'s legacy worktree case (pkg/agent/provision.go ~1587-1712) and the slug-keyed sharer/delete path. 3a makes them load-bearing for broker-mode agents, not just adopted ones. The `!BrokerMode` collapse must not remove them, and phase 6's acceptance gains a 3a regression check.
- **Phase 5 docs:** the worktree layout is `<repo>/.scion/agents/<name>/workspace`. This design's earlier text "worktrees still at `../.scion_worktrees`" is wrong and is superseded here. Linked-repo agents have no reincarnate yet. Already-linked projects re-link to get worktrees.

**To the backlog, raised separately and not in 3a:**
- the docker delete bug for hub-native worktree-per-agent (UUID vs slug keys: worktree, marker and branch left behind; the test hides it with `AgentID == AgentName`);
- the base clone inside `<repo>/.scion/workspace` on the existing worktree-per-agent path for linked paths;
- reincarnate for local worktrees;
- the label upgrade for already-linked projects;
- stale `../.scion_worktrees` lookups and docs;
- the stale comment at pkg/agent/run.go:1559-1560.

### Amendment 7 (2026-10-12, ACCEPTED by maintainer 2026-10-12, both items; owner: phase 3b): two findings from the phase-3a validation run

Source: the phase-3a validation run. The re-link check passed:
- a plain unlink plus link re-attaches to the old, unlabelled project;
- delete project, then link, re-creates it with the same ID and the label;
- new agents then get `<repo>/.scion/agents/<name>/workspace`.

Two findings need owners:

1. **A hub project ID leaks from global settings into a project (pre-existing bug; correctness).** After `hub unlink` and `hub link`, the project's own settings hold `hub.enabled/linked` but no `hub.projectId`. Link writes it only when the hub ID differs from the local `project_id`. The merged settings then inherit a `hub.projectId` from the global file, if the global project was ever linked, and agents are created in the **Global** hub project without any warning. When nothing global is set, the local ID coincides with the hub ID and masks the bug.
   - **Root fix:** for a non-global project, the hub project ID is resolved from that project's **own** settings file plus its local `project_id`, never inherited from the global file. Use the same own-file read as `ProjectLinkedSetting` (2a).
   - **Belt and braces:** `hub link` and auto-link always write `hub.projectId` to the project's own settings, even when it equals the local ID.
   - **Tests:**
     - with the global settings linked to Global, unlink and link a repo, then `GetProjectID` returns the repo's hub ID;
     - a project with no own `hub.projectId` does not pick up the global one.
   - **Owner:** phase 3b, a standalone fix merged **before phase 3**, because phase 3's migration and the documented re-link procedure both run link. Implemented in the settings loader: for a non-global project, the global file layer's hub project ID is dropped (legacy alias included; env and flags still win).
2. **`hub link` in a git clone with no `.scion`, under `$HOME`, links the Global project.** `$HOME/.scion` is a parent project, so resolution falls back to it. This also writes `hub.projectId` into the global settings, which is the trigger for finding 1.
   - **Fix:** when the working directory is inside a git repository with no `.scion` of its own, and project resolution falls back to the global (home) project, `hub link` and auto-link refuse with: "this git repository is not a scion project yet; run 'scion init' here first (or pass --global to link the global project)". Explicit `--global` keeps working.
   - This is a narrow correctness guard on the link path. It is not the general "outside a project" behaviour (Amendment 2, open item 8).
   - **Owner:** phase 3b, together with finding 1.
   - **Docs:** the re-link and first-link procedures say "run `scion init` in the repo first".

Also from the run, docs only:
- the command is `scion hub projects info <name>`, not `hub project show`;
- labels are visible only with `--format json`;
- `init` leaves an untracked `.gitignore` and `.scion/` in the checkout.
