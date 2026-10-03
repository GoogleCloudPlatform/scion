# Custom Lint Check Conventions

Conventions for scripts in `hack/check-*.sh`. Reference implementations:
`check-authz-guards.sh` (security-grade) and `check-project-compat-literals.sh`
(formatting-grade).

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Analysed, no violations. |
| 1 | Analysed, violations found (list on stderr). |
| 2 | **RESERVED.** GNU make flattens all non-zero recipe exits to 2, so this code can never be owned by a script. Read it as "ask the log." |
| 3 | COULD NOT ANALYSE: required tool missing (e.g. `rg` not installed). |
| 4 | COULD NOT ANALYSE: no candidate files matched (wrong cwd, empty checkout). |

Exit 3 is unconditional: every check, at every severity level, exits 3 when a
required tool is missing. A run that examined nothing must not look like a
clean pass (ptone/scion#1114). Use the shared `require_tool` helper in
`hack/lib/require-tool.sh` rather than open-coding the check. Severity governs
only the no-candidates case (exit 4 vs exit 0) — see Severity Levels below.

## Severity Levels

| Level | No-candidates behaviour | Rationale |
|-------|-------------------------|-----------|
| **Security-grade** | Exit 4 (build failure). | An empty scan of a security surface usually means the scan is broken, and a silently skipped security check ships a bypass. |
| **Formatting-grade** | Exit 0 (pass). | Zero matches for a formatting pattern is a legitimate clean state. |

A missing tool is **not** a severity question: it always exits 3 (see Exit
Codes above). Formatting-grade checks used to exit 0 on a missing tool; that
let a local `make ci` without `rg` report a clean pass for checks that never
ran.

Choose the level at script creation time and document it in the script header.

## Allowlists

- Anchor entries on **stable identifiers**: file path + function/symbol name.
  Never use line numbers — they shift on every edit.
- Every entry **must have a comment** explaining why the exception exists.
- Use a shell array of regex patterns, one per line, grouped by category with
  section comments. See `check-project-compat-literals.sh` for a populated
  example, and `check-authz-guards.sh` for the anchoring guidance (file +
  function, not line numbers).

## Self-Test

- **Security-grade** rules **MUST** implement `--self-test` with a fixture that
  exercises every classifier verdict (clean, violation, each edge case).
- **Formatting-grade** rules **SHOULD** implement `--self-test` where practical.

Fixtures can be inline heredocs written to a temp file (the current approach in
`check-authz-guards.sh`) or standalone files in `hack/testdata/`.

## Provenance Reporting

Print the commit SHA at the start of output so stale-checkout results are
identifiable. Append `-dirty` when the working tree has uncommitted changes
(see the `provenance()` function in `check-authz-guards.sh`):

```bash
sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" && -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi
echo "check-name: analysed ${sha}, ..." >&2
```

## Script Structure

Recommended order for new check scripts:

```
1. Header comment   — what it checks, why, exit codes, severity level
2. set -euo pipefail
3. cd to repo root  — cd "$(dirname "$0")/.."
4. Dependency check — source hack/lib/require-tool.sh; require_tool exits 3
5. Pre-filter       — find candidate files (exit 4 if none, or exit 0 for formatting)
6. Classify/scan    — run the actual analysis
7. Allowlist filter — remove known-good entries
8. Report           — print violations to stderr, print summary
9. Exit             — exit 1 if violations remain, exit 0 otherwise
```

## CI Integration

- Each check gets its **own CI workflow step** with a distinct `::error title=`
  annotation. This keeps failures individually identifiable in the GitHub UI.
- CI steps that need exit-code preservation (security-grade checks) invoke
  scripts **directly** (`./hack/check-foo.sh`), not via make (see exit code 2
  above). Formatting-grade checks may use `make` when the flattened exit code
  is acceptable.
- The `make check-custom` target exists for **local development convenience** —
  it runs all custom checks in one command but flattens exit codes to 2.
- Individual `make` targets (e.g. `make check-authz-guards`) remain available
  for running a single check locally.

## Adding a New Check

1. Write the script following the structure above.
2. Add a dedicated `make` target for the script.
3. Add the new target as a dependency of `check-custom`.
4. Add a CI workflow step in `.github/workflows/ci.yml` with its own error
   annotation.
5. Update the `.PHONY` line in the Makefile.
