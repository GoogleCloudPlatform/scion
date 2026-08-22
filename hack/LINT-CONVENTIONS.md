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

Both 3 and 4 must fail the build — a run that examined nothing must not look
like a clean pass.

## Severity Levels

| Level | Tool-missing behaviour | Rationale |
|-------|------------------------|-----------|
| **Security-grade** | Exit 3 (build failure). | A silently skipped security check ships a bypass. |
| **Formatting-grade** | Exit 0 (silent skip). | A silently skipped format check costs a reformat later. |

Choose the level at script creation time and document it in the script header.

## Allowlists

- Anchor entries on **stable identifiers**: file path + function/symbol name.
  Never use line numbers — they shift on every edit.
- Every entry **must have a comment** explaining why the exception exists.
- Use the format from `check-authz-guards.sh`: shell array of regex patterns,
  one per line, grouped by category with section comments.

## Self-Test

- **Security-grade** rules **MUST** implement `--self-test` with a fixture that
  exercises every classifier verdict (clean, violation, each edge case).
- **Formatting-grade** rules **SHOULD** implement `--self-test` where practical.

Keep fixtures in `hack/testdata/` alongside the script.

## Provenance Reporting

Print the git tree SHA at the start of output so stale-checkout results are
identifiable:

```bash
echo "tree: $(git rev-parse HEAD:)" >&2
```

## Script Structure

Recommended order for new check scripts:

```
1. Header comment  — what it checks, why, exit codes, severity level
2. set -euo pipefail
3. Dependency check — exit 3 (security) or exit 0 (formatting) if tool missing
4. Pre-filter       — find candidate files (exit 4 if none, or exit 0 for formatting)
5. Classify/scan    — run the actual analysis
6. Allowlist filter — remove known-good entries
7. Report           — print violations to stderr, print summary
8. Exit             — exit 1 if violations remain, exit 0 otherwise
```

## CI Integration

- Each check gets its **own CI workflow step** with a distinct `::error title=`
  annotation. This keeps failures individually identifiable in the GitHub UI.
- CI steps invoke scripts **directly** (`./hack/check-foo.sh`), not via make,
  to preserve the script's exit code (see exit code 2 above).
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
