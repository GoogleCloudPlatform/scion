# Experiments Phase 2 — convention and documentation (ptone/scion#2217)

Branch: `scion/experiments-2`, based on `scion/experiments-1b` (ptone/scion#2436, upstream GoogleCloudPlatform/scion#2191), which is rebased onto upstream `main` after Phase 1a-i and 1a-ii merged upstream as GoogleCloudPlatform/scion#2121 and GoogleCloudPlatform/scion#2152.

## Scope

Docs and convention only. The two code files touched are `web/src/utils/feature-flags.ts` and `web/e2e/chat-palette/fixture.ts`, each a comment-only edit.

- `docs-site/src/content/docs/reference/experiments.md` (new): what an experiment is; for admins (permissions, next-page-load semantics, unknown-override retention and single-name removal, the failed-request fallback, and the malformed-settings behavior with "Reset all to defaults"); for developers launching an experiment, including the server-decides-hub-behavior enforcement principle; changing a default; retiring (graduate or abandon); the four-row precedence table and the devtools-override behavior change; and the review-date cadence.
- `AGENTS.md` (root): a short "Experimental features" note pointing new feature work at the registry and the new reference page.
- `web/AGENTS.md`: the same note as the root file, as one paragraph.
- `web/src/utils/feature-flags.ts`: corrected three doc comments that still referenced the old mechanism — `setFeatureFlag`'s own comment (a Go-template injection of `window.__SCION_FEATURES__`) and two "disabled via server injection" comments (on `DEFAULT_ON_FLAGS` and `NATIVE_CHAT_V2_FLAG`). The module header comment already described the real boot-fetch mechanism from the prior phase.
- `docs-site/src/content/docs/reference/web-config.md`: the Feature Flags section now describes the boot-time fetch and the current precedence order, distinguishes a registered experiment from an unregistered flag, and links to the new reference page.
- `web/e2e/terminal-workspace/ROLLOUT.md`: rewritten to document admin control through the Experiments tab and to drop the stale "not in `DEFAULT_ON_FLAGS`" and Go-template statements.

## Notes

- The nav path is **Admin → Server Config → Experiments**. Confirmed against `web/src/components/shared/nav.ts` and existing cross-references in `admin-users.ts`. The new docs page and both doc fixes use this path.
- `web/src/utils/feature-flags.ts`'s module header comment already described the boot fetch and the four-row precedence, and no longer claimed a Go template sets the flags — that comment was already corrected in ptone/scion#2436. The remaining stray references were in `setFeatureFlag`'s own doc comment and two "disabled via server injection" comments, fixed here.
- A pre-existing, unrelated comment in `web/e2e/chat-palette/fixture.ts` described a native-chat test fixture's flags as "exactly as main.ts would set them from the Go template." This predates this work and concerns `web.native_chat_v2` (not a registered experiment); it is corrected here (comment-only) alongside the other stale references.

## Release notes

This repository records release notes as dated weekly digests under `docs-site/src/content/docs/release-notes/`, plus a daily per-PR changelog under `changelog/`. Both are compiled after merge from PRs merged to `main` in the period they cover; neither is edited by an individual feature PR, and neither currently has an open page for this unmerged work. A proposed line is included in the PR description for whichever digest ends up covering this change.

## Verification

- `docs-site`: `npm ci`, then `npm run build` (requires Node >= 22; the sandbox's default Node was 20, so a local Node 22 toolchain and the `d2` CLI were used to run the real build rather than skip it). Build succeeds, generates `/reference/experiments/`, and the `starlight-links-validator` link check reports all internal links valid.
- `web`: `npm ci`; `npm run typecheck` clean; `npx prettier --check src/utils/feature-flags.ts e2e/chat-palette/fixture.ts` clean; `npx eslint src/utils/feature-flags.ts` clean; `e2e/chat-palette/fixture.ts` cannot be type-linted (a pre-existing, repo-wide tsconfig gap excludes e2e/test files); `npx vitest run src/utils/feature-flags.test.ts` — 35/35 passing (no test changes needed; existing precedence tests already cover the corrected comments' behavior; the count includes three guard tests added upstream in `scion/experiments-1b`, not by this PR).
- Repo-wide grep confirms no remaining doc claims that a Go template sets `window.__SCION_FEATURES__`, and `ROLLOUT.md` no longer says the flag is off by default.
