# Design: Skill URI Input Validation & Auto-Transform

**Author:** ps-arch  
**Date:** 2026-07-24  
**Branch:** `scion/project-skills-uri-transform`  
**Depends on:** PR #859 (`gh://` private-repo skill resolution, merged to main)  
**Related findings:** `findings-uri-transform.md`

---

## Problem & Goals

Users adding skills via `scion project skills add`, `scion user skills add`, or the web UI must type (or paste) a URI. The only current validation is `strings.Contains(s, "://")` in the CLI and an empty-string check in the hub — both trivially loose. The result:

1. **Pasting a GitHub URL fails silently or with a cryptic 404.** A user who copies `https://github.com/org/repo/tree/main/skills/my-skill` from their browser will get a confusing 404 at provision time rather than having the URI auto-transformed to `gh://org/repo/my-skill@main`.
2. **Unsupported schemes are stored and silently fail.** `scion://my-skill` appears in CLI docs but has no registered resolver; it fails at provision time with no useful guidance.
3. **No deduplication.** Two different URL forms for the same skill are stored as separate entries.
4. **No user feedback on what formats are accepted.**

**Goals:**
1. Accept full GitHub tree (and blob) URLs as input and transform them to the canonical `gh://` form.
2. Reject unrecognized or ambiguous inputs with specific, actionable error messages.
3. Fix the `scion://` latent bug by normalizing to `skill://` at input time.
4. Enforce one canonical stored form so deduplication works correctly.
5. Provide input-time feedback in the CLI (transform notice) and web UI (live preview).

**Success criteria:** A user pasting `https://github.com/scion-frontiers/scion-repo-contrib/tree/main/skills/harness-qa` into any add flow sees the URI transformed to `gh://scion-frontiers/scion-repo-contrib/harness-qa@main` and stored in that form — without any additional steps.

---

## Non-Goals

- Fixing the 404 on private repos (`scion-frontiers/scion-repo-contrib`). That is a credential configuration issue, not a URI format issue. See `findings-uri-transform.md §9`. The resolver correctly handles both URI forms; the 404 means the project lacks credentials for that org.
- Supporting non-GitHub hosting (GitLab, Bitbucket, etc.). Scope creep; add a GCP resolver if needed separately.
- Validating that the referenced skill actually exists at resolution time (that happens at provision time).
- Supporting `https://raw.githubusercontent.com/` direct raw content URLs.

---

## Proposed Design

### 1. `NormalizeSkillURI` in `pkg/api`

Add a `NormalizeSkillURI(input string) (string, error)` function to `pkg/api/skill_uri.go`. This is the authoritative normalization function used by the hub (enforcement) and CLI (early feedback). The web UI implements a TypeScript equivalent.

**Why `pkg/api`?** It already contains `ParseSkillURI` (for `skill://` URIs) and `SkillURIScheme`. Both `pkg/hub` and `pkg/agent` import `pkg/api`. The function does not need to import `pkg/agent` — the GitHub URL parsing is reimplemented inline (simpler than `ParseGitHubSkillURI`, which returns a full struct). The two implementations share the same URL grammar rules, documented with a cross-reference comment.

**Function contract:**

```go
// NormalizeSkillURI accepts a user-supplied skill URI in any supported input form
// and returns the canonical stored form.
//
// Supported input → output:
//
//  gh://owner/repo/skill[@ref][?token=SECRET_NAME]
//    → gh://owner/repo/skill[@ref][?token=SECRET_NAME]   (validated, returned as-is)
//
//  https://github.com/owner/repo/tree/ref/skills/skill-name[?token=SECRET_NAME]
//    → gh://owner/repo/skill-name@ref[?token=SECRET_NAME]   (shorthand)
//
//  https://github.com/owner/repo/tree/ref/other/path[?token=SECRET_NAME]
//    → https://github.com/owner/repo/tree/ref/other/path[?token=SECRET_NAME]  (kept; resolver handles it)
//
//  https://github.com/owner/repo/blob/ref/skills/skill-name/SKILL.md[?token=SECRET_NAME]
//    → gh://owner/repo/skill-name@ref[?token=SECRET_NAME]   (blob: strip filename, then apply tree rules)
//
//  scion://skill-name
//    → skill://skill-name   (alias fix; see §4 below)
//
//  skill://... or bare name
//    → validated via ParseSkillURI, returned as-is
//
// Returns a non-nil error for unsupported or ambiguous inputs, with an actionable
// message that includes a valid-format example.
func NormalizeSkillURI(input string) (string, error)
```

**Transform rules in detail:**

| Input form | Output | Notes |
|---|---|---|
| `gh://owner/repo/skill[@ref][?token=X]` | same (validated) | Must have exactly 3 path segments |
| `https://github.com/owner/repo/tree/ref/skills/skill-name` | `gh://owner/repo/skill-name@ref` | Skills-convention shorthand |
| `https://github.com/owner/repo/tree/ref/other/deep/path` | `https://github.com/owner/repo/tree/ref/other/deep/path` | Non-standard path; kept as full URL |
| `https://github.com/owner/repo/blob/ref/.../filename.ext` | parent dir → apply tree rule | Strip last segment if it contains `.` |
| `scion://skill-name` | error | Not supported; use `skill://`; docs bug tracked at #561 |
| `skill://...` or bare name | same (validated) | Via existing `ParseSkillURI` |
| `https://github.com/owner/repo` (bare repo) | error | No skill path |
| `https://github.com/owner/repo/tree/main` (no path) | error | No skill path after ref |
| `gcp-skill://...` | error | Not supported via add command; scheme reserved |
| Any unknown `foo://` | error | Message: "unsupported scheme; supported: gh://, skill://, or a GitHub URL" |

**`?token=SECRET_NAME` passthrough:** Present in input → preserved in output unchanged. The normalizer validates the secret name format (`[A-Z][A-Z0-9_]*`) but does not look up the value.

**`blob` URL handling:** If the last path segment contains a `.` (looks like a filename), strip it and use the parent directory as the skill path. Apply the same rule as a `tree` URL. This covers the dominant pattern (`skills/skill-name/SKILL.md`, `skills/skill-name/README.md`). If the parent directory is empty after stripping, return an error.

**`scion://` alias:** See §4.

---

### 2. Hub Enforcement (Authoritative)

**File:** `pkg/hub/handlers_skills_injection.go`

In `addProjectInjectedSkill` and `addUserInjectedSkill`, after trimming whitespace, call `api.NormalizeSkillURI`:

```pseudocode
entry.SkillURI = strings.TrimSpace(entry.SkillURI)
if entry.SkillURI == "" {
    ValidationError(w, "skillUri is required", nil)
    return
}
normalized, err := api.NormalizeSkillURI(entry.SkillURI)
if err != nil {
    ValidationError(w, err.Error(), nil)  // HTTP 422
    return
}
entry.SkillURI = normalized
// (rest of handler unchanged)
```

Also apply in the **bulk PUT** endpoints (`setProjectInjectedSkills`, `setUserInjectedSkills`) where the per-entry URI is trimmed and dedup-checked. Each entry's URI is normalized before the dedup set and before storage.

**HTTP response on invalid URI:** `422 Unprocessable Entity` with the error message from `NormalizeSkillURI`. Not 400 — the input was structurally valid JSON but semantically invalid. This is consistent with how `ValidationError` is used elsewhere in the handler.

---

### 3. CLI Normalization (Early Feedback)

**Files:** `cmd/project_skills.go`, `cmd/user_skills.go`

In `runProjectSkillsAdd` and `runUserSkillsAdd`, after the existing `isSkillURI` check:

```pseudocode
skillURI := args[0]
normalized, err := api.NormalizeSkillURI(skillURI)
if err != nil {
    return fmt.Errorf("invalid skill URI: %w", err)
}
if normalized != skillURI {
    fmt.Fprintf(cmd.ErrOrStderr(), "Note: URI transformed → %s\n", normalized)
}
skillURI = normalized
// ... send skillURI to hub API
```

The `isSkillURI` check (`strings.Contains(s, "://")`) can be removed or replaced with the normalization call — `NormalizeSkillURI` accepts both URIs and bare skill names.

**Behavior change:** Currently a bare skill name (`my-skill`) passes `isSkillURI` as false and is rejected. After this change, `NormalizeSkillURI` accepts it as a hub-skill bare name, normalizing to the same bare name. This is a net improvement (consistent with the API accepting bare names).

---

### 4. `scion://` Handling

`scion://` appears in CLI help text and examples but has no registered resolver — it fails silently at provision time. The correct scheme is `skill://`.

**Decision (2026-07-24, user sign-off):** `scion://` is rejected by `NormalizeSkillURI` with a clear error: `"scion:// is not a supported scheme; use skill:// for hub-bank skills"`. No alias or silent normalization.

The underlying docs bug is tracked at ptone/scion#561 and will be addressed by a separate cleanup agent (CLI help text updated to use `skill://`). The design doc update entry in the transform table changes from "normalized to skill://" to "rejected with error".

---

### 5. Web UI Normalization

**File:** `web/src/components/shared/injected-skills-panel.ts`

In `handleAddSkill`, before submitting, apply a TypeScript normalization function. This is a client-side TypeScript re-implementation of the same rules (not a shared WASM or API call, to keep the latency-free UX).

```typescript
// Returns { canonical: string, transformed: boolean } or throws Error
function normalizeSkillURI(input: string): { canonical: string; transformed: boolean }
```

Supported transforms in TypeScript:
1. `https://github.com/owner/repo/tree/ref/skills/skill-name` → `gh://owner/repo/skill-name@ref`
2. `https://github.com/owner/repo/blob/ref/.../file.ext` → strip filename → apply #1
3. `scion://skill` → `skill://skill`
4. `gh://...` → validate 3-segment form, pass through
5. `skill://...` → pass through
6. Other `://` schemes → throw with error message

If `transformed === true`, show a brief "Transformed to: `gh://...`" message below the input before the user clicks Add.

**Fallback:** If the TypeScript normalizer misses a case, the hub's Go implementation is authoritative and returns a 422 with a clear message that the UI can display.

---

### 6. Error Message Standards

All error messages from `NormalizeSkillURI` must:
1. State what was wrong with the input
2. Give a correct-format example

Example messages:
- `"unsupported GitHub URL form \"%s\": expected a /tree/ URL with a skill path after the ref; example: https://github.com/owner/repo/tree/main/skills/my-skill"`
- `"unsupported scheme \"gcp-skill\": skills using gcp-skill:// cannot be added via this command"`
- `"invalid gh:// URI: expected gh://owner/repo/skill-name[@ref][?token=SECRET_NAME]"`
- `"bare repo URL \"%s\" has no skill path; specify the skill directory, e.g. https://github.com/owner/repo/tree/main/skills/my-skill"`

---

## Alternatives Considered

### A. Hub-only, no CLI/web normalization

Only the hub normalizes. CLI and web UI send raw input; the hub returns 422 on invalid.

**Rejected:** CLI users see a round-trip error with no pre-flight feedback. Acceptable security baseline, but poor UX. We implement both (hub authoritative + clients normalize for immediate feedback) at low additional cost since the Go function is shared between hub and CLI via `pkg/api`.

### B. Normalization in `pkg/agent`

Put `NormalizeSkillURI` in `pkg/agent/github_uri.go` alongside `ParseGitHubSkillURI`, and have `pkg/hub` import `pkg/agent`.

**Rejected:** Currently `pkg/hub` does not import `pkg/agent` (only `pkg/agent/state`). Adding this import inverts the intended layering — `pkg/agent` is broker-side code. The normalization function is a user-facing API concern, not a broker concern. `pkg/api` is the correct home.

### C. Move `GitHubSkillRef` to `pkg/api`, share parsing

Refactor `pkg/agent/github_uri.go` to import `pkg/api` for the struct, and put `ParseGitHubSkillURI` in `pkg/api`. Eliminates code duplication between normalizer and resolver.

**Rejected for this iteration:** Too large a refactor for the scope of this feature. The normalizer produces a canonical *string*; it doesn't need a `GitHubSkillRef` struct. The duplication is ~30 lines of URL parsing logic with a comment noting they must be kept in sync. Tracked as a future cleanup.

### D. Reject all GitHub full URLs; require `gh://` input only

Don't implement normalization. Document `gh://` as the only accepted form and tell users to convert manually.

**Rejected:** The user's explicit requirement is that pasting a GitHub URL should work. The conversion is unambiguous for the common case. Requiring manual conversion is a UX failure.

### E. Extend `gh://` to support multi-segment paths (`gh://owner/repo/path/to/skill`)

Allow `gh://owner/repo/path/to/skill` as a 4+-segment path form, eliminating the implicit `skills/` prefix assumption and enabling canonical storage of all skill paths.

**Rejected for this iteration:** Breaking change risk (current 3-part shorthand prepends `skills/`; a 4-part form would need a different semantic). The common case (skills at `skills/skill-name`) is handled by the 3-part shorthand. Non-standard paths can be stored as full GitHub URLs, which the resolver already handles. Tracked as potential future extension.

---

## Migration / Rollout

- **No schema change.** URIs are stored as `text` columns; the canonical form is valid for existing DB values.
- **Existing stored URIs are not migrated.** Entries already in the DB retain their current form. If a user previously stored `https://github.com/org/repo/tree/main/skills/my-skill` as a full URL, it continues to work (resolver handles both forms). The deduplication improvement only applies to new entries added after this change.
- **`scion://` in existing DB entries.** Any existing `scion://` entries remain broken (no registered resolver). The normalizer rejects new `scion://` input with a clear error. Existing stored entries are unaffected by this feature; cleanup tracked at ptone/scion#561.
- **Backward compat for API callers.** Programmatic callers (hub API clients) that POST a valid normalized URI see no change. Callers that POST a full GitHub URL will receive the canonical `gh://` form.
- **CLI behavior change.** `isSkillURI()` currently rejects bare names (no `://`). After this change, bare names are accepted (normalized to hub-skill bare name form). This is strictly more permissive.

---

## Open Questions

### OQ1: `scion://` — silent normalize vs. reject-with-guidance?

**Resolved (2026-07-24):** Reject `scion://` with a clear error. Do not add a normalization alias. The docs/examples bug is tracked at ptone/scion#561 and will be cleaned up separately by a dedicated agent. Error message: `"scion:// is not a supported scheme; use skill:// for hub-bank skills"`.

### OQ2: `blob` URL handling — support or reject?

**Recommendation:** Support `blob` URLs with the filename-stripping heuristic. The pattern (`skills/my-skill/SKILL.md` or `skills/my-skill/README.md`) is universal for well-formed skills. The heuristic is: "strip the last path segment if it contains a `.`".

**Risk:** If a user pastes `https://github.com/org/repo/blob/main/skills/my-skill/some.dir.with.dots` (a directory with dots, unlikely but possible), the heuristic would incorrectly strip it. The error would be caught at provision time (skill directory not found).

**Alternative:** Reject `blob` URLs with: `"Paste the directory URL instead of the file URL: replace /blob/ with /tree/ and omit the filename"`. Explicit and safe, but adds friction.

**Needs user sign-off.**

---

## Implementation Phases

### Phase 1: `NormalizeSkillURI` in `pkg/api` + tests (~150 lines)

Add `NormalizeSkillURI(string) (string, error)` to `pkg/api/skill_uri.go` (or a new `pkg/api/skill_uri_normalize.go`).

Unit test coverage:
- All transform cases in the table above
- `?token=SECRET_NAME` passthrough
- Error cases with message content assertions
- Round-trip: output of `NormalizeSkillURI` is a fixed point (calling it again returns the same string)

This phase has no external dependencies and can be reviewed in isolation.

### Phase 2: Hub enforcement (~40 lines)

Add `NormalizeSkillURI` calls in `pkg/hub/handlers_skills_injection.go`:
- `addProjectInjectedSkill` (line ~141 region)
- `addUserInjectedSkill` (line ~431 region)
- `setProjectInjectedSkills` bulk PUT (inner loop)
- `setUserInjectedSkills` bulk PUT (inner loop)

Return `ValidationError` (422) on normalization error.

Integration test: POST a full GitHub URL → verify stored URI is the canonical `gh://` form.

### Phase 3: CLI normalization (~30 lines)

Update `cmd/project_skills.go` and `cmd/user_skills.go`:
- Replace `isSkillURI` check with `api.NormalizeSkillURI` call
- Print transformation notice to stderr when normalized form differs from input

Update CLI help text and examples: replace `scion://` with `skill://`.

### Phase 4: Web UI TypeScript normalization (~70 lines TS)

Add `normalizeSkillURI` in `injected-skills-panel.ts` (or a shared utility module).
Show transformation preview below the input field when a GitHub URL is entered.
Handle 422 responses from hub by displaying the error message inline.

---

## Acceptance Criteria

The QA tester should verify:

**Transform coverage:**

1. **Full GitHub tree URL → `gh://`** — `scion project skills add https://github.com/org/repo/tree/main/skills/my-skill` succeeds. Stored URI is `gh://org/repo/my-skill@main` (not the full URL). No error.

2. **Full GitHub tree URL with non-standard path** — `https://github.com/org/repo/tree/main/contrib/my-skill` is accepted and stored as the full URL (resolver handles it). No error.

3. **GitHub blob URL → `gh://`** — Pasting `https://github.com/org/repo/blob/main/skills/my-skill/SKILL.md` is transformed to `gh://org/repo/my-skill@main`. No error.

4. **`gh://` shorthand passthrough** — `gh://org/repo/my-skill@main` is accepted unchanged. `gh://org/repo/my-skill` (no ref) is accepted unchanged.

5. **`skill://` passthrough** — `skill://my-skill` is accepted unchanged.

6. **Bare name** — `my-skill` is accepted as a bare hub-skill name.

7. **`scion://` rejected** — `scion://my-skill` produces a 422 error with a message pointing to `skill://`. No entry is stored.

**Validation (error cases):**

8. **Bare repo URL** — `https://github.com/org/repo` produces a 422 error with message referencing the expected `/tree/ref/path` format.

9. **Unsupported scheme** — `gcp-skill://something` produces a 422 with a message explaining the scheme is not accepted via this command.

10. **Unknown scheme** — `ftp://anything` produces a 422 with the actionable error message.

11. **Invalid `gh://` format** — `gh://owner-only` (too few segments) produces a 422.

**Canonical storage (deduplication):**

12. **Dedup** — Adding `https://github.com/org/repo/tree/main/skills/my-skill` when `gh://org/repo/my-skill@main` already exists produces a 409 Conflict (they normalize to the same URI).

**Web UI:**

13. **Live transform preview** — Pasting a full GitHub URL into the web UI skill add input shows the transformed `gh://` form before the user clicks Add.

14. **Error display** — Pasting an unsupported URL displays the error message inline (not a console error or silent failure).

**CLI:**

15. **Transformation notice** — `scion project skills add https://github.com/org/repo/tree/main/skills/my-skill` prints `Note: URI transformed → gh://org/repo/my-skill@main` to stderr before confirming success.

**Backward compatibility:**

16. **Existing stored full-URL entries** — Skills stored as full GitHub URLs before this change continue to resolve at provision time (resolver handles both forms).

17. **No regression on `skill://` and bare names** — Existing skill injection entries using `skill://` or bare names are unaffected.
