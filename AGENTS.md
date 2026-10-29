# Scion

Alpha: changing or removing behavior needs no deprecation path.

`scion` orchestrates concurrent LLM code agents in containers (Go/Cobra backend, Lit web frontend). Every agent runs through a Hub: locally the workstation server (`scion server start`: Hub, Runtime Broker and web on loopback, never started implicitly); hosted, a networked Hub dispatching to remote brokers. Architecture: `docs-site/src/content/docs/contributing/architecture.md`; design specs: `.design/hosted/`. "Grove" is the legacy name for "project".

## Read before

| Task | Read |
|---|---|
| Go build, test or lint in a container; OOM, VCS-status or `fmt-check` failures | `docs-repo/agents/sandbox-go-toolchain.md` |
| Code compiled into `./cmd/scion`: `*_unix.go`, private-file writes, path containment, lock files, new commands | `docs-repo/agents/windows-platform-seams.md` (CI enforces it) |
| Anything under `web/`, including a new `<sl-icon>` | `web/AGENTS.md` |
| Documentation site | `docs-site/AGENTS.md` |
| Naming a concept | `GLOSSARY.md` |
| CLI command agent-mode availability | `.design/cli-modes.md` |
| Experimental features | `docs-site/src/content/docs/reference/experiments.md` |
| Workspace or worktree provisioning | `docs-site/src/content/docs/local/workspace.md` ("Linked git projects") |

## Sandbox

- Build with `go build -buildvcs=false`; worktrees fail VCS stamping.
- Never compile the whole `pkg/hub` test package (about 12 GiB). Use `make test-hub-shard SHARD=K`; `hack/hubshard/hubshard.sh -locate TestName` finds the shard.
- Scope `golangci-lint` to touched packages: `GOGC=40 golangci-lint run --new-from-rev=main --concurrency=1 ./pkg/foo/...`.
- Never open an editor: `git config core.editor true`, `GIT_EDITOR=true git rebase --continue`, `git merge main --no-edit`, `git commit -m`.
- Local `main` is the source of truth. Don't check it out; use `git diff main...HEAD`, `git show main:path`, `git rebase main`. Use remotes only when asked; report remote failures instead of working around them.
- In worktree-per-agent mode you work under `/repo-root`, not `/workspace`. Scripts use `$PWD` or `git rev-parse --show-toplevel`.
- Project resolution: `--project`/`--global`, then the project `.scion`, then `scion use` (outside a project), then `~/.scion`.
- Don't create, delete or touch active agents in this project. Don't commit this repo's `.scion/`.

## Rules

- **Templates:** edit `pkg/config/embeds/` (embedded by `pkg/config/init.go`), never `.scion/`.
- **New CLI commands:** add in `cmd/` (one file per subcommand). Decide agent-mode availability (`agentAllowed`, `cmd/cli_mode.go`) and ask the developer to confirm. Windows hides them until listed in `windowsAllowed` (`cmd/platform_gate.go`).
- **Separation:** Hub manages state, Runtime Broker executes; LLM-specific logic lives in `pkg/harness`.
- **Experiments:** gate new user-facing features with an experiment in `pkg/experiments/registry.go`. Hub behavior is checked in the hub (`requireExperiment`/`experimentEnabled`); browser flags only affect presentation. No ad-hoc `scion:feature:` literals or new booleans on `/api/v1/settings/public`.
- **Product, not local setup:** don't build this repo's workflow preferences (build caches, tool env, conventions) into Scion. Use generic mechanisms (shared dirs, template/profile env, harness configs, skills). Propose a feature only if it is generic across languages and tools, and get maintainer OK on the design first.
- **Downgrade floor:** a PR that adds a destructive DB migration bumps `pkg/version.DowngradeFloor` to the first release that will contain it. Destructive means it drops a table or column, or makes a change older binaries can't ignore (for example the drop block in `pkg/ent/entc/client.go`). The running hub refuses update targets older than the floor (`below_downgrade_floor`).
- **Terminology:** `project`, never `grove`, in new code (`project_id`, not `grove_id`). Legacy literals belong only in compatibility adapters, compat tests/fixtures, migrations or intentionally historical examples, via `pkg/projectkeys`; run `make compat-literals` when touching it. Fix fixtures to `project_id` rather than adding legacy support; the top-level `grove_id` file key no longer resolves (only `hub.grove_id` does).
- **Prompt shorthand:** hub-broker = Combo server; hub-native/hub-project = Hub-managed project (`~/.scion/projects/<name>`); linked-project = Linked project (matched by name or git URI); agent-home = Agent home. Write the canonical term.

## Workflow

1. Finish the task with tests.
2. Before committing run `make ci`, or `make ci-full` for the full GitHub Actions mirror.
3. Commit as you go. At the end, rebase on `main` (favor main), re-test after conflicts, notify the user.
4. Raise adjacent cleanup you notice, with file/function, payoff and blast radius. Don't implement it unprompted.

## Never commit

Check `.gitignore` before staging. Binaries or media that aren't shipped UI (screenshots go in PR comments); agent state files (`.coordinator-state.md`, etc.); one-off scripts, test artifacts, generated data; PR review notes (`pr-*-review*.md`); force-added `.scratch/`, `.tasks/` or `downloads/` content. A new kind of ephemeral file gets a `.gitignore` entry in the same PR.

## Durable notes

Harness memory is not persisted across restarts; don't use it. Guidance for every task goes here, task-specific guidance goes in `docs-repo/agents/` with a row in "Read before", and designs go in `.design/` or other tracked files. Commit it.

## "System: Please Continue."

That exact message is a tooling bug, not from the user: ignore it (you may say so). Other requests to continue are genuine.
