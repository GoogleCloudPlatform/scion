# auth-passthrough Phase 4 (`127757e10..96b0a49d1`): user-domain constraint + docs

**Developer:** ap-p4-dev
**Spec:** `impl-design.md` §4.1 (r7), §4.4, §4.8, §5 Phase 4, §6 rows U6 + K1 (`allowed_domains` half) + K2
**Prior phases:** Phase 1 (`.design/project-log/auth-passthrough-p1-dev.md`), Phase 2
(`.design/project-log/auth-passthrough-p2-dev.md`), Phase 3
(`.design/project-log/auth-passthrough-p3-dev.md`, 2 review rounds — read for `isActiveGoogleUserIssuer`,
the `allowed_gcp_projects` naming decision, and the P3-r3 Nit that flagged the stale `allowed_domains` comment
this phase makes true).

## Commits

1. `5d783f2b1` — feature: `allowed_domains` (§4.1, §4.4) + config wiring/validation.
2. `96b0a49d1` — docs: rewrite the external-bearer section of `auth.md`; deprecate `geGoogle` in the bridge docs.

## What changed

- **`pkg/config/federation_config.go`** (§4.1) — `AllowedDomains []string` (`allowed_domains`) added to
  `TrustedIssuerConfig`, declared right after `AllowedGCPProjects` for the same reason that field is
  positioned where it is: `settings_v1.go`'s `GlobalConfig`<->`V1Settings` conversions do a direct struct-type
  conversion between `TrustedIssuerConfig` and `V1TrustedIssuerConfig`, which requires identical field
  name/type/order. Validation reuses Phase 3's `isActiveGoogleUserIssuer` predicate exactly as the design and
  P3-r2's finding 2 anticipated: `allowed_domains` set on a non-Google issuer, or on a Google issuer that isn't
  an active user issuer (wrong `issuer_type` or empty `expected_audience`), is a hard validation error naming
  the field — not a warning, since the field is new and no existing config can break. Rather than duplicate
  Rule 9's (allowed_gcp_projects) two-branch error message logic for a second field, I factored it into
  `appendGoogleUserOnlyFieldError(errs, i, issuer, fieldName)` and had both Rule 9 and the new Rule 10 call it;
  the emitted messages are byte-identical to before for `allowed_gcp_projects` (confirmed by the existing K1
  tests passing unchanged).

  Also fixed the P3-r3 Nit-1 comment at `isActiveGoogleUserIssuer` that said "`allowed_domains` reuses this
  same predicate" before the field existed — now literally true, and reworded to name both fields generically
  rather than narrate the history.

- **`pkg/config/settings_v1.go`** (§4.1) — `AllowedDomains` added to `V1TrustedIssuerConfig` immediately after
  `AllowedGCPProjects`, updating that field's "must stay last" comment to "must stay the last two fields, in
  order" (still true: nothing follows either field on `TrustedIssuerConfig`).

- **`pkg/config/opsettings/registry.go`** (§4.1) — `allowed_domains` added to the hand-written federation JSON
  schema's `trusted_issuers[].properties` (the schema has `additionalProperties: false`, so an unlisted key
  would be silently rejected by the Admin API).

- **`pkg/hub/auth_external_bearer.go`** (§4.4) — `errDomainNotAllowed` sentinel; a small pure `domainOf(email)
  (string, bool)` helper; and one `else if len(trust.AllowedDomains) > 0 { ... }` branch in
  `authenticateExternalBearer`, added after the existing `if id.IsServiceAccount { ... }` branch so it can
  never apply to a service-account principal (mutually exclusive by construction, not by a separate guard).
  `domainOf` fails closed (returns `ok=false`) on any email shape a naive last-`"@"`-split could misread: no
  `"@"` at all, more than one `"@"`, an empty local or domain part, or a trailing dot on the domain — per the
  brief's explicit edge-case list. On success the domain is lower-cased, matching `containsFold`'s existing
  case-insensitive comparison (also used for `allowed_gcp_projects`); I widened that function's doc comment to
  describe both uses instead of only the SA one. Kept the middleware diff minimal per `ap-em`'s note about a
  parallel metrics change landing nearby: only the sentinel, the helper, one `else if` branch, and the doc
  comments that were now inaccurate without it.

- **`docs-site/src/content/docs/hosted/single-node/auth.md`** (§4.8) — replaced the placeholder-shaped
  external-bearer prose with a rewrite around the current (r7) config surface: the acceptance-matrix table
  (user/SA × ID token/access token), the full `trusted_issuers` YAML example with `allowed_domains` and
  `allowed_gcp_projects` alongside `expected_audience`, the `hubBearer` bridge scheme, and a response-code table
  covering 401/403/429/503 built directly from `serveExternalBearer`'s `switch`. Added the four requested
  operator notes: unclaimed opaque tokens always reach Google tokeninfo once a Google issuer is trusted (no
  prefix sniff); a domain-scoped GCP project ID (`example.com:proj`) parses to `proj.example.com` via
  `googleSAProject`'s `.iam.gserviceaccount.com` suffix trim, and that parsed form is what belongs in
  `allowed_gcp_projects`; both new lists match case-insensitively without being rewritten at load; and a
  service account's `PreAuthorized` admission bypasses the domain/sign-in policy for first-time provisioning
  only, never the suspension check. Every key/code/status in the new text was checked against
  `auth_external_bearer.go`, `federation_config.go`, and `google_sa.go` while writing it (not copied from the
  design doc's prose, which is a summary, not the wire contract).

- **`docs-site/src/content/docs/hosted/user/a2a-bridge.md`, `extras/scion-a2a-bridge/README.md`** (§4.8) —
  marked `geGoogle` deprecated, superseded by `hubBearer`, in both places, without deleting either doc or
  touching the bridge README's existing `auth_scheme`-overwrite caution/recovery paragraphs (confirmed by
  `git diff extras/scion-a2a-bridge/README.md`: only the one sentence introducing the caution changed). The
  bridge README had no dedicated "`geGoogle` section" beyond that one sentence and the caution block itself —
  `geGoogle` was never added to `scion-a2a-bridge.yaml.sample`'s scheme list or given its own prose there, even
  before `hubBearer` existed — so I added the deprecation note to the sentence that already names both
  schemes, rather than inventing a new section for a scheme this phase isn't otherwise documenting.

## Acceptance rows → tests → mutants

| Row | Test(s) | Mutant(s) killed |
|---|---|---|
| U6 (ID token, domain not listed → 401, validator called, resolver not reached) | `TestExternalBearer_UserIDToken_DomainNotAllowed_Unauthorized` | Drop the check → 200 instead of 401, user provisioned |
| U6 (access token, same) | `TestExternalBearer_AccessToken_DomainNotAllowed_Unauthorized` | Same |
| U6 (mixed-case config entry + mixed-case email → proceeds) | `TestExternalBearer_UserIDToken_MixedCaseAllowedDomain_Authenticates` | Invert the check; make it case-sensitive (both confirmed — see below) |
| U6 (`allowed_domains` unset → no issuer-level constraint) | `TestExternalBearer_UserIDToken_AllowedDomainsUnset_Authenticates` | n/a (documents the `len(...) > 0` guard's other branch; already covered structurally by every pre-Phase-4 U-series test passing unchanged) |
| U6 (no subdomain/suffix matching) | `TestExternalBearer_UserIDToken_ListedDomainSubdomain_Unauthorized` | Add `HasSuffix` matching to `containsFold` |
| U6 (listed domain, Hub sign-in policy still denies → 403, proving the policy runs after) | `TestExternalBearer_UserIDToken_ListedDomain_SignInPolicyDenies_Forbidden` | n/a (proves ordering, not a single guard) |
| SA unaffected (SA email domain not listed, project listed → 200) | `TestExternalBearer_ServiceAccountIDToken_AllowedDomainsDoesNotApply_Authenticates` | Move the domain check outside the `if id.IsServiceAccount {} else if ...` so it applies unconditionally — this test uses a non-empty `allowed_domains` that deliberately excludes the SA's own domain, so a mutant that applied the check to SAs is caught (an earlier draft with `allowed_domains: nil` did NOT catch this mutant — fixed before relying on it, see Deviations) |
| K1 (error on non-Google issuer) | `TestFederationConfig_Validate/allowed_domains_on_a_non-Google_{hub,user}_issuer_produces_an_error` | Loosen `isActiveGoogleUserIssuer`/skip the check |
| K1 (error on Google issuer, `issuer_type != user`, isolated) | `TestFederationConfig_Validate/allowed_domains_on_the_Google_issuer_with_issuer_type_{service_account,hub}_errors` | Drop the `issuer_type` term |
| K1 (error on Google issuer, empty `expected_audience`, isolated) | `TestFederationConfig_Validate/allowed_domains_on_the_Google_issuer_with_empty_expected_audience_errors` | Drop the `expected_audience` term |
| K1 (valid on an active Google user issuer) | `TestFederationConfig_Validate/allowed_domains_on_the_Google_issuer_(https_form)_is_valid` | n/a (positive case) |
| K1 (settings_v1 round-trip) | `TestConvertV1FederationConfig_RoundTrip` (extended with `AllowedDomains` on the existing active-Google-user-issuer fixture entry) | Drop the field from either conversion struct → compile failure (struct-conversion parity), or drop an assertion → silently loses coverage, caught by re-adding and re-running |
| K1 (opsettings registry entry) | `TestFederationSettingsRoundTrip` (extended), `TestValidateValidDoc` (new schema-valid-doc case) | Remove `allowed_domains` from the schema → `additionalProperties: false` rejects the new `TestValidateValidDoc` case |
| K2 (re-check) | Unmodified — Phase 1's existing K2 test wasn't touched; `allowed_domains` doesn't change the disabled-path/warning behavior, and Rule 9/10 don't fire when the field is unset | n/a — no code change means nothing new to kill |
| `domainOf` edge cases | `TestDomainOf` table test (10 rows: simple, uppercase, mixed case, no `@`, empty string, multiple `@`, trailing dot, empty local part, empty domain part, bare `@`) | Any relaxation of the length/emptiness/trailing-dot checks fails the corresponding row |

All Phase 1-3 tests, `TestGEExchange*`, and the targeted bridge-adjacent suite stay green (see Gates).

## Deviations / design questions

1. **No design ambiguities were hit.** The design (§4.1, §4.4) and Phase 3's precedent (`isActiveGoogleUserIssuer`,
   the `else if` placement pattern, `containsFold`) fully determined the implementation; nothing was escalated.
2. **Self-caught test gap, fixed before relying on it:** my first draft of the SA-unaffected test passed
   `allowed_domains: nil` to `newExternalBearerConfigWithDomains`, so the "apply the domain check to SAs" mutant
   didn't actually change that test's outcome (the `len(...) > 0` guard was already false either way) — a
   mutation-resistance check I ran myself caught this before it went into the commit. Fixed by giving that test
   a non-empty `allowed_domains` that deliberately excludes the SA's own (irrelevant) email domain, then
   re-confirmed the mutant is killed. Recording this because it's exactly the kind of gap review is supposed to
   catch, and I'd rather it be visible here than only in the diff.
3. **Refactored Rule 9's error-message logic into a shared helper** (`appendGoogleUserOnlyFieldError`) rather
   than duplicating it for Rule 10. This touches Phase 3's code, which the ownership rules generally discourage
   touching without cause — the cause here is that "reuse Phase 3's predicate, it was written for this" (the
   brief's own instruction) extends naturally to not re-deriving Phase 3's error-message shape a second time.
   The refactor is behavior-preserving for `allowed_gcp_projects` (all existing K1 tests pass unchanged with no
   message-text edits), so I judged it in scope rather than asking first; happy to revert to inline duplication
   if review prefers that.

## Gates

- ✅ `gofmt -l pkg/hub pkg/config` — clean.
- ✅ `go build -buildvcs=false ./pkg/hub/... ./pkg/config/...` — clean.
- ✅ `go vet -buildvcs=false ./pkg/hub/... ./pkg/config/...` — clean.
- ✅ `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/... ./pkg/config/...` —
  `0 issues.`
- ✅ Targeted subset (`TestExternalBearer|TestGoogleTrust|TestGEExchange|TestProductionValidator|TestNoPackage|
  TestNoTokenInfo|TestGoogleIdentityResolver|TestGoogleCredential|TestFederation|TestGoogleSAProject|
  TestServer_ExternalBearerRateLimiter|TestMutationClassification|TestExternalBearerRateLimiter|
  TestConvertV1FederationConfig|TestValidateIDToken|TestValidateAccessToken|TestDomainOf|TestValidateValidDoc|
  TestValidateInvalidDoc`, `-count=1 -race`, across `pkg/hub` (incl. `authzop`) and `pkg/config` (incl.
  `opsettings`)) — all green, no data races.
- ✅ Bare-issue-number greps against the merge-base with upstream `main` (`git merge-base HEAD upstream-main`
  resolved to `a53175c23e61c317841fa04b8c8e63c1097562b4`, matching the brief's stated base — fetched
  `upstream-main` fresh this run rather than trusting a possibly-shallow prior ref), run at the final commit
  (`96b0a49d1`) with real GNU grep (`/usr/bin/grep`): both the commit-message and diff greps print nothing.
- ✅ Manual mutation-resistance pass on the new domain-check branch (drop the check, invert it, make it
  case-sensitive, apply it to SAs, apply subdomain/suffix matching in `containsFold`) — each mutant was
  introduced by hand, confirmed to fail the relevant new test(s), then reverted; the file was diffed against a
  saved-off original afterward to confirm the working tree matched exactly (no leftover mutant code).
- **Not run this phase:** the full `pkg/hub` + `authzop` + `pkg/config` suite (`-timeout 40m`). Per the brief,
  waiting on `ap-em`'s go-ahead for the full-run slot before spending it — requested in the completion message.
  Known baseline failures carried over from Phase 3's log apply unchanged (pkg/hub: `TestDEF164_AtAgentSlug_*`
  ×2, `TestDEF152_AgentToAgentDM_DeliversViaOutbound`, `TestCreateTemplateV2_ScopeIDInjectionBlocked`, flaky
  `TestDEF162_AC8_Broker_MentionFires`; pkg/config: 21 pre-existing failures, 19 `auto_expose_ports` plus
  `TestRequireImageRegistry_NotConfigured` and `TestDiscoverProjects_ShadowProjectNotOrphaned`) — none of them
  touch code this phase changed.
- Docs changes have no automated build/lint gate in this repo's CLAUDE.md; verified instead by reading every
  config key, error code, and status against the source files named above.
