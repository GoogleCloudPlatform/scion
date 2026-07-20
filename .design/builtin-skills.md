# Design: Mandatory Instruction Injection for Scion Agents

**Project:** builtin-skills  
**Author:** builtin-skills-arch  
**Date:** 2026-07-20  
**Status:** Final — user-approved design direction

---

## Problem & Goals

Three distinct problems, addressed together because they share a common code surface:

1. **No mandatory instruction mechanism.** The current design has two instruction delivery paths: (a) template `agents.md`, which is overridable by any custom template, and (b) platform skills, which are discoverable sidebar files rather than core instructions. There is no way to guarantee a piece of instruction content reaches every provisioned agent regardless of template.

2. **Status-signaling boilerplate is silently droppable.** The default template's `agents.md` carries the three `sciontool status` commands (ask_user, blocked, task_completed). When a custom template provides its own `agents.md`, the default's copy is silently replaced. The `agent-status-signals` platform skill covers the same content, but as a skill file it sits in `.claude/skills/` — it relies on the harness to load it into context, not on direct injection into instructions. An agent that doesn't consult the skill never sees the content.

3. **Dead boilerplate files.** `resources/templates/default/agents-hub.md` and `agents-git.md` are unreferenced by any code path and should be deleted.

**Success criteria:**
- Every provisioned agent receives the three sciontool status signals as the leading content of its agent instructions, regardless of template and regardless of harness.
- Dead files are removed.
- No new `api.Harness` interface methods are introduced (existing harness implementations are unchanged).
- Existing tests continue to pass; new tests cover the new mechanism.

---

## Non-Goals

- **Contrib-repo template updates.** All 12 templates in the contrib-repo already contain status-signal content in their own `agents.md`. After this change they will receive it twice: once from the mandatory preamble, once from their template. This pre-existing duplication (the platform skill also duplicated it today) is benign and out of scope for this project.
- **Hub-enabled gating.** The `inject_when: hub_enabled` condition is implemented in `shouldInjectSkill()` but unused. `scion-messaging` and `scion-cli-operations` contain hub-centric content but are always injected. Per user guidance ("pretty much all agents run on hubs now; the conditional field may go away soon"), this project does not add hub gating to any skill.
- **Importing `candidate-agents-additions.md` content.** The 10 candidate additions in the contrib-repo scratchpad are a separate decision.
- **Changes to the `api.Harness` interface.** Content composition happens at the `ProvisionAgent()` call site, not in harness implementations.

---

## Proposed Design

### 1. New Embedded Resource: `resources/mandatory_boilerplate/`

A new directory alongside `resources/platform_skills/` and `resources/templates/`:

```
resources/
├── mandatory_boilerplate/
│   └── agent-instructions-preamble.md   ← the mandatory content
├── platform_skills/
│   ├── git-sandbox/
│   ├── scion-agent-manage/
│   ├── scion-cli-operations/
│   ├── scion-messaging/
│   └── team-creation/
│       (agent-status-signals/ deleted in Phase 2)
└── templates/
    └── default/
        └── agents.md   ← emptied in Phase 2
```

**`resources/embed.go`** gains one new directive:

```go
//go:embed all:mandatory_boilerplate/*
var mandatoryBoilerplateFS embed.FS
```

**`resources/catalog.go`** gains one new accessor function:

```go
// MandatoryBoilerplateFS returns the embedded filesystem containing mandatory
// agent instruction preamble content. Files are read in lexical order and
// prepended to every agent's instructions at provision time.
func MandatoryBoilerplateFS() fs.FS {
    sub, err := fs.Sub(mandatoryBoilerplateFS, "mandatory_boilerplate")
    if err != nil {
        panic(fmt.Sprintf("resources: sub mandatory_boilerplate FS: %v", err))
    }
    return sub
}
```

### 2. New Function: `loadMandatoryPreamble()`

Added to `pkg/agent/provision.go`, near `injectPlatformSkills()`:

```go
// loadMandatoryPreamble reads all .md files from the mandatory boilerplate FS
// in lexical filename order and concatenates them separated by double newlines.
// Returns nil if the FS contains no non-empty .md files.
func loadMandatoryPreamble(boilerplateFS fs.FS) ([]byte, error) {
    entries, err := fs.ReadDir(boilerplateFS, ".")
    if err != nil {
        return nil, fmt.Errorf("read mandatory boilerplate: %w", err)
    }
    var parts [][]byte
    for _, e := range entries {
        if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
            continue
        }
        data, err := fs.ReadFile(boilerplateFS, e.Name())
        if err != nil {
            return nil, fmt.Errorf("read mandatory boilerplate %s: %w", e.Name(), err)
        }
        if len(bytes.TrimSpace(data)) > 0 {
            parts = append(parts, bytes.TrimRight(data, "\n"))
        }
    }
    if len(parts) == 0 {
        return nil, nil
    }
    return bytes.Join(parts, []byte("\n\n")), nil
}
```

**Design notes:**
- Files are read in lexical order (Go's `fs.ReadDir` guarantees this). If multiple files are added to `mandatory_boilerplate/` in the future, they are concatenated in that order. Name files with a leading sort prefix (e.g., `00-status-signals.md`, `10-foo.md`) if ordering matters.
- Files that are empty or whitespace-only are skipped, so the placeholder in Phase 1 can be an empty file without affecting behavior.
- The function returns `nil` if the FS has no non-empty .md files. The caller treats `nil` as "no preamble."

### 3. Modified `ProvisionAgent()` — Step 4 Injection

Currently Step 4 resolves template content and calls `h.InjectAgentInstructions(agentHome, content)`. The change introduces a preamble load and content composition.

**Load site** (just before the existing Step 4 block, after skill installation):

```go
// Load mandatory preamble — prepended to all agent instructions regardless of template.
mandatoryPreamble, err := loadMandatoryPreamble(resources.MandatoryBoilerplateFS())
if err != nil {
    return "", "", nil, fmt.Errorf("failed to load mandatory boilerplate: %w", err)
}
```

**Composition helper** (inline or extracted):

```go
// composeInstructions prepends mandatory preamble to template content.
// If preamble is nil, returns templateContent unchanged.
// If templateContent is nil/empty, returns preamble alone.
func composeInstructions(preamble, templateContent []byte) []byte {
    if len(preamble) == 0 {
        return templateContent
    }
    if len(bytes.TrimSpace(templateContent)) == 0 {
        return preamble
    }
    return append(append(preamble, '\n', '\n'), templateContent...)
}
```

**Call site changes** — two places in Step 4:

*Path A (template chain present)*: Currently:
```go
if err := h.InjectAgentInstructions(agentHome, content); err != nil { ... }
```
Becomes:
```go
if err := h.InjectAgentInstructions(agentHome, composeInstructions(mandatoryPreamble, content)); err != nil { ... }
```

*Path B (inline config, no template chain)*: Currently:
```go
content := []byte(finalScionCfg.AgentInstructions)
if err := h.InjectAgentInstructions(agentHome, content); err != nil { ... }
```
Becomes:
```go
content := []byte(finalScionCfg.AgentInstructions)
if err := h.InjectAgentInstructions(agentHome, composeInstructions(mandatoryPreamble, content)); err != nil { ... }
```

**Injection order in `ProvisionAgent()` after the change:**
1. Template chain resolution + config merge (unchanged)
2. Home directory copy (unchanged)
3. Template skills copy (unchanged)
4. Platform skills injection: `injectPlatformSkills()` (unchanged)
5. Skill bank resolution and install (unchanged)
6. **[NEW]** Load mandatory preamble: `loadMandatoryPreamble(resources.MandatoryBoilerplateFS())`
7. Resolve template agent instructions (unchanged)
8. **[MODIFIED]** Compose and inject: `h.InjectAgentInstructions(agentHome, composeInstructions(preamble, templateContent))`
9. System prompt injection (unchanged)

**Key property:** `h.InjectAgentInstructions()` is called exactly once (as today). No new harness interface methods. The three harness implementations (`Generic`, `ContainerScriptHarness`, `DeclarativeGenericHarness`) are unmodified.

### 4. Content Relocation Plan

#### `agent-status-signals` → mandatory boilerplate

**Action:** Remove `resources/platform_skills/agent-status-signals/` entirely. Its content becomes `resources/mandatory_boilerplate/agent-instructions-preamble.md`.

**Rationale:** Status signals are universal, non-negotiable, and needed by every agent. Delivering them as a skill (a sidebar the agent reads when triggered) is the wrong delivery layer — they must appear in the core instructions. The skill's own frontmatter says "Every agent template should reference this skill," which acknowledges the problem: it had to rely on templates to remember to do what should be automatic.

**Content of `agent-instructions-preamble.md`:** The existing `agent-status-signals/SKILL.md` content (minus the YAML frontmatter), possibly lightly revised. The three signals, the sleep anti-pattern warning, and the stall-prevention note all belong here.

#### `scion-cli-operations` → no change

Stays as an always-injected platform skill. The non-interactive mode rules and CLI constraints are near-universal but not strictly required in non-hub, non-container contexts. Always-injected is fine; the content is short and harmless.

#### `scion-messaging` → no change (for now)

Stays as an always-injected platform skill. Hub gating is explicitly deferred per user guidance. No content changes in this PR.

#### `git-sandbox` → no change

Correctly gated with `inject_when: git_workspace`. Content is appropriate. No changes.

#### `team-creation` → Phase 2 update

Remove the sentence "Every agent template should reference this skill" from the `agent-status-signals` description (it will no longer be a skill). Update the orchestrator `agents.md` template example to remove `[status reporting boilerplate]` — that boilerplate is now delivered by mandatory injection, not by templates. Also remove the checklist item that asks template authors to verify status boilerplate is included.

#### `scion-agent-manage` → no change

Situational reference content. No changes.

### 5. Default Template `agents.md`

**Action:** Empty the file (blank content). Keep it on disk. Keep the `agent_instructions: agents.md` reference in `scion-agent.yaml`.

**Rationale:**
- `ResolveContentInChain()` returns an empty `[]byte{}` for an empty file, which is non-nil. `composeInstructions(preamble, []byte{})` returns `preamble` alone — correct behavior.
- The file and config reference serve as a documented hook: operators extending the default template can see where template-specific instructions would go. An empty file is self-explanatory; absence of a file plus a config reference would cause a confusing "file not found" debug scenario.
- Reversible: future content can be added back without a config change.

### 6. Dead File Removal

- Delete `resources/templates/default/agents-hub.md` — unreferenced, orphaned from March 2026 "conditional instruction extensions" feature that was never implemented. Content is superseded by `scion-cli-operations` platform skill.
- Delete `resources/templates/default/agents-git.md` — same provenance. Content is superseded by `git-sandbox` platform skill.

No other dead boilerplate was found during the code walk. The `default/skills/.gitkeep` is correctly placed and unremarkable.

### 7. Small Cleanup Items

**`scion-messaging` verification checklist fix** (Phase 2):  
Line 91 of `resources/platform_skills/scion-messaging/SKILL.md` reads:
```
- [ ] Does the message have a clear recipient (`agent:`, `user:`, or `set[]`)?
```
Fix `set[]` → `group[]` to match the corrected body text introduced in commit `1d9b4f6`.

**Stale test fixture update** (Phase 2):  
`pkg/agent/provision_test.go:2298–2324`, test case "skill with scripts subdirectory is fully copied," uses a synthetic `fstest.MapFS` with skill name `scion` and script `start-agent.sh` — both of which no longer reflect any real platform skill. Update the fixture to use a realistic name (e.g., `scion-agent-manage`) and remove the `scripts/start-agent.sh` entry (no platform skill ships scripts after commit `1d9b4f6`). The test still exercises the copy-subdirectory mechanism; the fixture just needs to reflect current platform skill structure to avoid misleading future test authors.

---

## Alternatives Considered

### Alt 1: Add `AppendAgentInstructions()` to the `api.Harness` interface

**Approach:** Add a new method to the interface for append semantics; call it after `InjectAgentInstructions`.

**Rejected because:** Interface changes are breaking for any external harness implementors. All three existing harness implementations (`Generic`, `ContainerScriptHarness`, `DeclarativeGenericHarness`) would need new methods for no functional gain — all three implementations just write bytes to a file. Content composition at the call site in `ProvisionAgent()` is semantically identical and requires zero harness changes.

### Alt 2: Mandatory boilerplate as a special platform skill

**Approach:** Add a new `inject_when: mandatory` (or equivalent) condition to `shouldInjectSkill()` and deliver mandatory content through the existing `injectPlatformSkills()` path.

**Rejected because:** `injectPlatformSkills()` writes to the harness-specific *skills directory* (e.g., `.claude/skills/`). Skills are a sidebar mechanism — the harness decides when to load them into context. Mandatory instruction content must live in the agent instructions file (CLAUDE.md / agents.md), not in a skill directory. Conflating "mandatory instructions" with "skills" violates the conceptual model and makes the delivery unreliable for harnesses that don't eagerly load all skills.

### Alt 3: Embed mandatory content in every template's `agents.md`

**Approach:** Add the status-signal instructions to all templates' `agents.md` files as the canonical delivery mechanism.

**Rejected because:** External templates (contrib-repo and user-defined) would need to be updated — a coordination problem with no enforcement mechanism. The "silently dropped when custom template overrides `agents.md`" problem that exists today would persist for any new template that forgets to include the boilerplate. No single authoritative source.

### Alt 4: Compile mandatory content as a Go string constant in `provision.go`

**Approach:** Hardcode the preamble as a `const` string in the provisioning code.

**Rejected because:** Content would be non-reviewable as markdown, non-editable without a recompile, and invisible to anyone looking in `resources/`. The embedded-file approach costs nothing extra and keeps content in its natural home.

---

## Migration / Rollout

**Zero-downtime:** The mandatory injection step runs at `ProvisionAgent()` call time. Already-running agents are unaffected; their instruction files were written at their own provision time. New agents pick up the preamble automatically.

**Backward compatibility — duplication:**  
Custom templates that include status-signal content in their own `agents.md` will receive the signals twice: once from the mandatory preamble, once from their template. This is the same duplication that exists today (the `agent-status-signals` platform skill + template `agents.md`). Net change in duplication: zero for existing templates. LLMs handle idempotent instructions gracefully.

**Backward compatibility — agent-status-signals skill removal:**  
Templates that override the `agent-status-signals` platform skill by placing their own `skills/agent-status-signals/` directory today are suppressing the platform version. After this change, mandatory injection cannot be overridden by a template skill — the preamble is always prepended. This is intentional: mandatory means mandatory. In practice, no known template overrides `agent-status-signals`.

**Config compatibility:** No `scion-agent.yaml` changes are required for any existing template. The `agent_instructions: agents.md` field and resolution chain are untouched.

---

## Open Questions

None. All design questions were resolved with user input during the design phase:
- **Content scope of mandatory boilerplate:** `agent-status-signals` content only. Mechanism-first priority confirmed.
- **Agents.md fate in default template:** Empty file (blank). Config reference preserved.
- **Contrib-repo migration scope:** Out of scope; pre-existing duplication is acceptable.
- **Hub-enabled gating:** Deferred; the `inject_when` conditional field may be removed soon.

---

## Implementation Phases

### Phase 1: Mandatory Injection Mechanism (plumbing, no content migration)

**Goal:** Working code path with placeholder content; all existing tests green.

**Files touched:**
- `resources/embed.go` — add `//go:embed all:mandatory_boilerplate/*` and `mandatoryBoilerplateFS var`
- `resources/catalog.go` — add `MandatoryBoilerplateFS() fs.FS`
- `resources/mandatory_boilerplate/agent-instructions-preamble.md` — create with placeholder content (empty or a comment; must be a non-empty file for Go embed to include the directory)
- `pkg/agent/provision.go` — add `loadMandatoryPreamble()`, add `composeInstructions()`, modify Step 4 injection for both template-chain path and inline-config path
- `pkg/agent/provision_test.go` — new tests for the new mechanism

**New tests to write:**
- `loadMandatoryPreamble()`: empty FS returns nil; single .md file returns content; multiple .md files concatenated in lexical order; whitespace-only file skipped; non-.md files ignored
- `composeInstructions()`: preamble + content; preamble only (empty template content); content only (nil preamble); both nil
- `ProvisionAgent()` integration (table-driven, using mock harness or temp dirs): mandatory content appears in injected instructions when template has `agents.md`; mandatory content appears when template has no instructions; mandatory content appears when `inlineCfg` is used instead of template chain; mandatory content is prepended (not appended)

**Rebase checkpoint:** Rebase onto `origin/main` at phase start.

**Commit cadence:** One commit: "feat: add mandatory instruction preamble injection mechanism"

### Phase 2: Content Migration + Cleanup

**Goal:** Real content in the preamble, dead files removed, small inconsistencies fixed.

**Files touched:**
- `resources/mandatory_boilerplate/agent-instructions-preamble.md` — replace placeholder with `agent-status-signals` content (minus YAML frontmatter)
- `resources/platform_skills/agent-status-signals/` — **delete entire directory**
- `resources/templates/default/agents.md` — empty the file
- `resources/templates/default/agents-hub.md` — **delete**
- `resources/templates/default/agents-git.md` — **delete**
- `resources/platform_skills/team-creation/SKILL.md` — remove references to `agent-status-signals` skill; update orchestrator `agents.md` template example to remove `[status reporting boilerplate]` placeholder and the checklist item requiring it
- `resources/platform_skills/scion-messaging/SKILL.md` — fix checklist `set[]` → `group[]`
- `pkg/agent/provision_test.go:2298–2324` — update stale fixture: rename skill `scion` → `scion-agent-manage`, remove `scripts/start-agent.sh` entry

**Rebase checkpoint:** Rebase onto `origin/main` at phase start and again before sending a compare URL.

**Commit cadence:** Two commits preferred:
1. "feat: promote agent-status-signals to mandatory boilerplate; remove dead template files" (content migration + deletions)
2. "fix: update stale test fixture and scion-messaging checklist" (cleanup items)

These can be one commit if the developer prefers; the logical separation is what matters for review.

### Note for Developer: Design Doc Commit

Per project convention, the finalized design doc must be committed to `.design/builtin-skills.md` in the repo as part of the feature PR. The source is this file (`/scion-volumes/scratchpad/projects/builtin-skills/design.md`). Commit it in Phase 1 alongside the mechanism code.

---

## Acceptance Criteria

### Phase 1

- [ ] `resources/mandatory_boilerplate/agent-instructions-preamble.md` exists and is embedded in the binary
- [ ] `resources.MandatoryBoilerplateFS()` returns a valid `fs.FS` containing the preamble file
- [ ] `loadMandatoryPreamble()` unit tests pass for all cases (empty FS, single file, multiple files, whitespace-only, non-.md files)
- [ ] `composeInstructions()` unit tests pass for all four combinations (preamble+content, preamble-only, content-only, both-nil)
- [ ] A default-template agent provisioned in a test receives the mandatory preamble as the leading content of its instructions
- [ ] A custom-template agent provisioned in a test receives the mandatory preamble prepended to its template's `agents.md` content
- [ ] An agent provisioned via `inlineCfg` (no template chain) also receives the mandatory preamble
- [ ] `go test ./...` green with no regressions

### Phase 2

- [ ] `resources/platform_skills/agent-status-signals/` does not exist
- [ ] `resources/mandatory_boilerplate/agent-instructions-preamble.md` contains the three `sciontool status` commands (`ask_user`, `blocked`, `task_completed`) and the sleep anti-pattern warning
- [ ] `resources/templates/default/agents.md` is empty (zero bytes or blank)
- [ ] `resources/templates/default/agents-hub.md` does not exist
- [ ] `resources/templates/default/agents-git.md` does not exist
- [ ] `resources/platform_skills/scion-messaging/SKILL.md` verification checklist uses `group[]` (not `set[]`)
- [ ] `provision_test.go` stale test fixture references `scion-agent-manage` (not `scion`) and contains no `start-agent.sh` path
- [ ] `resources/platform_skills/team-creation/SKILL.md` contains no reference to `agent-status-signals` skill and does not instruct template authors to include status boilerplate
- [ ] End-to-end: an agent provisioned from the default template receives the three `sciontool status` signal instructions in its agent instructions file
- [ ] End-to-end: an agent provisioned from a contrib-repo template receives the mandatory preamble prepended to that template's `agents.md` content
- [ ] `go test ./...` green with no regressions

---

## Branch

All implementation work on: `scion/builtin-skills-mandatory-instructions`

Compare URL (after rebasing): `https://github.com/GoogleCloudPlatform/scion/compare/main...ptone:scion/builtin-skills-mandatory-instructions`
