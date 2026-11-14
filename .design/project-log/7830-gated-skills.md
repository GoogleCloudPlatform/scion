# Experiment-gated platform skill content (ptone/scion#7830, phase 1)

**Date:** 2026-11-14
**Branch:** scion/choices-skills-p1
**Delivers:** ptone/scion#7830 (phase 1) and ptone/scion#7399
**Design:** design-7830 r2, approved by ptone (message 02310a31-7ef3-4755-a50c-a917f2c5e300 in conv:243261fb-263e-4797-836d-d40c9040b72c)

## What was built

- **Gate syntax.** `experiment: <id>` in a platform skill Markdown file's
  frontmatter. On `SKILL.md` it gates the whole skill (ptone/scion#7399); on any
  other `*.md` it gates that file. Inside any `*.md`, the lines between
  `<!-- scion:experiment <id> -->` and `<!-- scion:end-experiment -->` (each marker
  on its own line) are kept only while `<id>` is on. The marker lines are always
  removed.
- **Evaluator** (`pkg/agent/skill_gate.go`). `gateMarkdown` resolves one file.
  `gatedView(fs.FS, isOn) fs.FS` is a read-only view that drops gated-off files and
  serves resolved content, with matching sizes in `Stat` and `ReadDir`. It passes
  `testing/fstest.TestFS`. `injectPlatformSkills` installs from the view through
  the unchanged `InstallTreeFS`, so atomic replace and link confinement are
  unchanged and no new OS file primitive was added.
- **State source.** `experimentOn(ctx, settings)` is used by both the
  agent-plugins gate and the skill gates. Broker mode reads only
  `HubAgentDefaults.Experiments`; local mode reads `local_experiments`.
  `skillExperimentGate` turns an unknown or retired ID off and logs a warning. No
  experiment ID is named in the provisioning code.
- **Reincarnate.** With `ForceOverwrite` set, a gated-off whole skill whose name
  the template chain does not own is removed (`home.RemoveAll`). A template-owned
  copy is kept. Gated files and blocks inside a skill need no extra handling,
  because `InstallTreeFS` replaces the skill directory as a unit.
- **Dispatch.** `experiments.ChatChoices` was added to `dispatchExperimentNames`.
  `TestDispatchExperimentNames_CoverPlatformSkillGates` scans the embedded skills
  and fails, naming the files, when a gated ID is missing from the list.
- **Content.** `scion-messaging/references/choices.md` (file-gated) and two gated
  blocks in `scion-messaging/SKILL.md`.

## Decisions

- **Gated-off `SKILL.md`:** it is absent from the view. `injectPlatformSkills`
  still reads the raw frontmatter, so it can tell "gated off" (skip, and remove on
  reincarnate) apart from "no SKILL.md".
- **Malformed gates fail closed:**
  - An unclosed block is dropped to the end of the file.
  - A nested opener drops the rest of the file.
  - A stray end marker is removed.
  - The `resources` tests make all of these unreachable in a shipped binary.
- **Frontmatter `experiment:` is kept** in the installed file. It is harmless
  metadata, and keeping it leaves a file-gated file byte-identical to its source.
- **Content neutrality:**
  - Following ptone/scion#7842, the reference says only that `default` marks a
    recommended choice, not how the UI renders it. A test forbids "preselect",
    "start selected" and "outlined".
  - Following ptone/scion#7841 option (c), the reference tells agents to resend
    every still-pending key of a set with `--update`.

## Gotchas

- `TestDocSyntax` skips any fenced line that contains `<`, `>` or `$`, so
  placeholder examples were never flag-checked.
  `TestDocSyntax_PlatformSkillChoices` adds a flag check for those lines in the
  scion-messaging skill files. It also forbids inline `--choices` JSON in
  examples.
- Agents do not see message IDs in the delivery envelope. For
  `scion decisions record --from`, the reference points to
  `scion conversation messages ... --json`.
- Local E2E: the workstation broker does not start without `image_registry`
  (`scion config set --global image_registry <x>`). A project created before the
  broker registered has no broker; create the project after it registers.
- The default template on a fresh workstation server used the antigravity
  harness, so the skills landed in `home/.gemini/antigravity-cli/skills/`.

## Follow-ups (not in this PR)

- The `scion-artifacts` skill gated on `hub.artifacts` (ptone/scion#7897) is the
  first shipped user of the whole-skill gate. It also needs `experiments.Artifacts`
  added to `dispatchExperimentNames`.
- `scion decisions` docs-site pages: ptone/scion#7898.
