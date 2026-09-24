# auth-passthrough Phase 3 (`057182769..ef152a864`): service accounts

**Developer:** ap-p3-dev
**Spec:** `impl-design.md` §4.1 (r7), §4.2(ii), §4.4, §4.5, §5 Phase 3, §6 rows S1-S7 + K1
**Prior phases:** Phase 1 (`.design/project-log/auth-passthrough-p1-dev.md`, 4 review rounds), Phase 2
(`.design/project-log/auth-passthrough-p2-dev.md`, 2 review rounds).

## Commits

1. `057182769` — Phase 3 feature: SA admission (§4.1, §4.2(ii), §4.4, §4.5) + config.
2. `ef152a864` — P2 review r3 carry-in Nit 3: extract `newRejectingCountingValidator()`.

## Design deviation raised and resolved before implementation: `allowed_projects` naming collision

Before writing any config code, I found that `TrustedIssuerConfig.AllowedProjects` (`pkg/config/federation_config.go`)
**already existed**, for an unrelated, shipped feature: hub-to-hub federation project scoping. It restricts
which federated Scion project (from a peer hub's agent-token `project_id` claim) may authenticate
(`federation_auth.go:296-298`, `case IssuerTypeHub:`), and `FederationConfig.Validate()` already errors if it's
set on any non-hub issuer type — including a Google one. The design doc's original §4.1/K1 asked for exactly the
config shape that validation forbids: `AllowedProjects` set on a Google `issuer_type: user` entry, for a
completely different purpose (GCP project ID parsed from an SA email). Implementing K1 literally would have
either broken two passing, unrelated tests (`TestFederationAuth_ProjectAllowed`/`_ProjectNotAllowed`,
`federation_config_test.go`'s "hub issuer with allowed_projects is still valid") or required a special case
exempting hub-type issuers from a rule the design didn't know needed exempting.

Flagged to `ap-em` immediately (before touching `pkg/config`), with a proposed resolution (reuse the field,
carve out hub-type issuers from the new rule). `ap-em` escalated to the issue owner, who decided **option B: a
distinct field**, `AllowedGCPProjects` / `allowed_gcp_projects` (design updated to r7). Rationale recorded in the
design doc: reusing `allowed_projects` would give one config key two meanings depending on `issuer_type`, and an
operator misreading `allowed_projects: [x]` on a Google issuer as "these callers may only reach Scion project x"
(when it actually admits every SA in GCP project x) fails open. I implemented per that decision; see below.
While waiting for the decision (~40 minutes), I worked ahead on the self-contained pieces that don't touch
`pkg/config`: §4.5 (`googleSAProject`), §4.2(ii) (the SA ID-token audience rule in the validator), and the SA
branch skeleton in `auth_external_bearer.go` with the config field read isolated behind a single accessor
(`trustAllowedProjects`) so the eventual field name/shape decision required touching exactly one line. Test
fixtures for the SA branch initially bypassed `NewFederationAuthenticator`'s validation (same-package struct
literal) for the same reason; once the field decision landed, these were rewritten to go through the real,
validated constructor (see "What changed" below).

## What changed

- **`pkg/hub/google_sa.go`** (new, §4.5) — `googleSAProject(email) (string, bool)`. Lower-cases the input; parses
  `name@PROJECT.iam.gserviceaccount.com` and `PROJECT@appspot.gserviceaccount.com`; explicitly rejects Google
  service agents (`gcp-sa-*.iam.gserviceaccount.com`), compute default SAs
  (`N-compute@developer.gserviceaccount.com` — project *number* only, not the project ID the allowlist is
  configured with), `*@system.gserviceaccount.com`, and anything without a parseable project.

- **`pkg/hub/google_credential_validator.go`** (§4.2(ii)) — `ValidateIDToken`'s audience-extraction step now
  branches on `isServiceAccount` (already computed for `IsServiceAccount`, unchanged from Phase 1). SA ID tokens:
  `aud` is checked against `allowedClientIDs` by the existing `isAllowedAudience` call (unchanged, runs earlier);
  `azp`, when present, must equal `sub` (the metadata-server/`iamcredentials.generateIdToken` shape — anything
  else is `ErrGoogleFieldDisagreement`); the resolved `Audience` is the matched allowed client ID
  (new `matchedAudience` helper). User tokens keep the pre-existing azp/aud logic unchanged. The branch runs
  after signature verification and the `email_verified` gate, so a bad-signature or unverified-email token never
  reaches it regardless of what its (unverified) email looks like.

- **`pkg/config/federation_config.go`, `settings_v1.go`, `opsettings/registry.go`** (§4.1 r7) —
  `AllowedGCPProjects []string` (`allowed_gcp_projects`) added to `TrustedIssuerConfig` and its admin-API mirror
  `V1TrustedIssuerConfig` (same field, same position, so the existing direct struct-type conversions between the
  two keep compiling) and the opsettings JSON-schema (`additionalProperties: false` would otherwise reject it).
  Validation: `allowed_gcp_projects` set on any issuer that isn't Google (`isGoogleIssuerURL`, matching both
  `accounts.google.com` forms) is an error; the **existing** `allowed_projects`-on-non-hub-issuer error message
  now appends a suggestion to use `allowed_gcp_projects` when the issuer happens to be Google, without changing
  when that error fires. The old `AllowedProjects` field, its validation rule, its tests, and
  `federation_auth.go`'s use of it are all untouched.

- **`pkg/hub/federation_auth.go`** — `NewFederationAuthenticator` lower-cases `AllowedGCPProjects` (only) at
  load time, on both the `rawConfig` and resolved `config` copies of each issuer entry. `AllowedProjects` is
  deliberately left as-configured (case-sensitive match against a claim value elsewhere; out of scope here).

- **`pkg/hub/auth_external_bearer.go`** (§4.4) — the SA branch of `authenticateExternalBearer`:
  ```go
  policy := ResolvePolicy{}
  if id.IsServiceAccount {
      if kind == externalBearerAccessToken {
          return nil, errSAAccessTokenRejected
      }
      proj, ok := googleSAProject(id.Email)
      if !ok || !containsFold(trustAllowedProjects(trust), proj) {
          return nil, errSAProjectNotAllowed
      }
      policy.PreAuthorized = true
  }
  u, err := cfg.GoogleResolver.Resolve(ctx, id, policy)
  ```
  `errSAAccessTokenRejected` renames Phase 2's `errExternalBearerPrincipalRejected` (which rejected every SA
  outright; Phase 3 gives ID tokens an admission policy, so the sentinel name now says what it actually means).
  Both new sentinels fall through `serveExternalBearer`'s existing `default:` arm → 401 `unauthorized`
  `invalid external bearer token`, matching the design's status table without any new switch case.
  `trustAllowedProjects`/`containsFold` isolate the one read of `trust.AllowedGCPProjects`.

- **`pkg/hub/google_identity_resolver.go`** — renamed the Phase-1-era `allowed_projects` mentions (the
  `ResolvePolicy.PreAuthorized` doc comment, `provisionNewUser`'s doc comment, and the audit-log line's
  `reason` value) to `allowed_gcp_projects`, per `ap-em`'s explicit instruction. No behavioral change beyond the
  log field's string value.

- **`pkg/hub/ge_exchange_ratelimit_test.go`** — `newRejectingCountingValidator()` (P2 review r3 carry-in Nit 3),
  replacing 7 identical pasted `&countingGoogleValidator{fakeGoogleValidator{idTokenErr: ...,
  accessTokenErr: ...}}` blocks across `auth_external_bearer_test.go`. No behavior change; committed separately.

## New tests

- **`google_sa_test.go`** (S5): every §4.5 row (standard SA, appspot, `gcp-sa-*` service agent, compute default
  SA, `system.gserviceaccount.com`, non-SA emails, edge cases: bare domain, empty local part, no `@`, empty
  string) plus a case-folded variant of each SA-matching row.

- **`google_credential_validator_test.go`**: `TestProductionValidator_IDToken_ServiceAccount_AZPEqualsSub_Valid`
  (S1 at the validator level, real numeric sub/azp), `..._AZPDiffersFromSub_Rejected` (S4 at the validator
  level), `..._ServiceAccountEmail_Unverified_NeverReachesSABranch` and `..._BadSignature_NeverReachesSABranch`
  (SA-shaped `azp==sub` claims that would validate if the SA branch ran, but don't because the
  email-verified/signature gates run first), and `TestProductionValidator_IDToken_UserToken_SAShapedAzpSub_StillRejected`
  — a **non-SA** email with `azp==sub` (would pass the SA rule if mis-routed) and `aud != azp` (must fail the
  unchanged user rule): the mutation-killer for "the SA/user split uses the verified email, not claim shape."

- **`auth_external_bearer_sa_test.go`** (new): S1 (project listed → 200, `neverAuthorized` resolver proves
  `PreAuthorized` bypassed the sign-in policy; plus an `azp==""` variant), S2 (project not listed / unset /
  unparseable-project, the last using an allowlist containing `""` specifically so dropping the `googleSAProject`
  `ok` check is still caught), S3 (SA access token with its project *listed* — proving the access-token check
  and the project-membership check aren't accidentally ANDed together, which an empty allowlist couldn't
  distinguish), S6 (suspended SA user, 403 `user_suspended`, provisioned via `PreAuthorized` first).

- **`auth_external_bearer_test.go`**: renamed `TestExternalBearer_ServiceAccountIDToken_Rejected` →
  `..._AZPNotSub_Unauthorized` (S4 at the middleware level — it was already exercising this by coincidence
  once the SA branch changed; the docstring was stale) and rewired it to list the SA's project, isolating S4
  from S2.

- **`ge_exchange_test.go`** (S7): `TestGEExchange_RealValidator_ServiceAccountIDToken_Rejected_ExactBytes` and
  `..._ServiceAccountAccessToken_Rejected_ExactBytes`, both through the **real** validator with a real SA claim
  shape (`azp==sub` for the ID token; a real SA email via tokeninfo/userinfo for the access token) — Phase 1's
  F1 finding noted the existing `TestGEExchange_ServiceAccount` fake (which builds `IsServiceAccount: true`
  directly) doesn't exercise the real classification/validation path this phase changed.

- **`federation_config_test.go`** (K1, both directions): `allowed_gcp_projects` on a non-Google hub issuer and
  on a non-Google user issuer both error; on the Google issuer it's valid; the *old* `allowed_projects` on the
  Google issuer still errors, and the message now names `allowed_gcp_projects`.

- **`federation_auth_test.go`**: `TestFederationAuth_AllowedGCPProjects_NormalisedToLowerCaseAtLoad` and
  `..._AllowedProjects_NotNormalisedToLowerCase` (proves only the new field is normalised).

- **`settings_v1_test.go`**, **`opsettings/opsettings_test.go`**: extended the existing federation
  conversion/round-trip tests with `AllowedGCPProjects` on both directions, and added a schema-valid-doc case
  with `allowed_gcp_projects` in the JSON body (the `additionalProperties: false` schema would otherwise reject
  the field silently were it not added to `registry.go`).

Every new SA-branch guard was hand-verified mutation-resistant before the feature commit: swapping the
access-token/project-check order (survived — both converge on the same 401, revealing no observable ordering
dependency, so the test's docstring was corrected to describe what it actually proves instead), AND-merging the
access-token and project checks (killed by S3's listed-project variant), disabling the project-membership check
entirely (killed by S2's two tests), and dropping just the `googleSAProject` `ok` check while keeping the
membership check (killed by S2's unparseable-project test, specifically because its allowlist contains `""`).

## §6 acceptance rows → tests

| Row | Test(s) |
|---|---|
| S1: SA ID token, `azp==sub`, project listed → 200, no Hub policy; `azp==""` variant | `TestExternalBearer_ServiceAccountIDToken_ProjectListed_Authenticates`, `..._EmptyAZP_ProjectListed_Authenticates`; validator level: `TestProductionValidator_IDToken_ServiceAccount_AZPEqualsSub_Valid` |
| S2: project not listed → 401; unset → 401 | `TestExternalBearer_ServiceAccountIDToken_ProjectNotListed_Unauthorized`, `..._AllowedProjectsUnset_Unauthorized`, `..._UnparseableProject_Unauthorized` |
| S3: SA access token, listed project too → 401 | `TestExternalBearer_AccessToken_ServiceAccount_ProjectListed_StillRejected` (+ `..._AccessToken_ServiceAccount_Rejected` from Phase 2, unchanged) |
| S4: SA ID token `azp != sub` → 401 | `TestExternalBearer_ServiceAccountIDToken_AZPNotSub_Unauthorized`; validator level: `TestProductionValidator_IDToken_ServiceAccount_AZPDiffersFromSub_Rejected` |
| S5: `googleSAProject` table, every row + case-folding | `TestGoogleSAProject` |
| S6: suspended SA user → 403 `user_suspended` | `TestExternalBearer_ServiceAccountIDToken_Suspended_Forbidden` |
| S7: exchange still rejects SA ID/access tokens, real validator, exact bytes | `TestGEExchange_RealValidator_ServiceAccountIDToken_Rejected_ExactBytes`, `..._ServiceAccountAccessToken_Rejected_ExactBytes` |
| K1: `allowed_gcp_projects` on non-Google → error; valid on Google; `allowed_projects` on Google still errors, names the new field | `TestFederationConfig_Validate` subtests (4 new cases) |
| K1 (lower-casing) | `TestFederationAuth_AllowedGCPProjects_NormalisedToLowerCaseAtLoad`, `..._AllowedProjects_NotNormalisedToLowerCase` |
| User `aud != azp` unchanged; SA/user split uses verified email | `TestProductionValidator_IDToken_UserToken_SAShapedAzpSub_StillRejected`; existing `TestProductionValidator_IDToken_AudAzpDisagreement` (unaffected, still green) |
| SA classification requires verified email, not just shape | `TestProductionValidator_IDToken_ServiceAccountEmail_Unverified_NeverReachesSABranch`, `..._BadSignature_NeverReachesSABranch` |
| Config/conversion round-trip for the new field | `TestConvertV1FederationConfig_RoundTrip` (extended), `TestFederationSettingsRoundTrip` (extended), new valid-doc schema case |
| Phase 1/2 + `TestGEExchange*` stay green | full targeted suite below, all green |

## Gates

- ✅ `gofmt -l pkg/hub pkg/config` — clean.
- ✅ `go build -buildvcs=false ./pkg/hub/... ./pkg/config/...` — clean.
- ✅ `go vet -buildvcs=false ./pkg/hub/... ./pkg/config/...` — clean.
- ✅ `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/... ./pkg/config/...` —
  `0 issues.`
- ✅ Targeted subset (`TestExternalBearer|TestGoogleTrust|TestGEExchange|TestProductionValidator|TestNoPackage|
  TestNoTokenInfo|TestGoogleIdentityResolver|TestGoogleCredential|TestFederation|TestGoogleSAProject|
  TestServer_ExternalBearerRateLimiter|TestMutationClassification|TestExternalBearerRateLimiter|
  TestConvertV1FederationConfig`, `-count=1 -race`, across `pkg/hub` and `pkg/config`) — all green, no data
  races.
- ✅ Bare-issue-number greps against `upstream-main`, run at the final commit (`ef152a864`) with real GNU grep
  (`/usr/bin/grep`, per the P1/P2 note that the interactive shell `grep` is a `ugrep` wrapper): both the
  commit-message and diff greps print nothing.
- Not run this phase: the full `pkg/hub` + `authzop` + `pkg/config` suite (`-timeout 40m`) — per the brief,
  waiting on `ap-em`'s go-ahead for the full-run slot before spending it, since it touches production code in
  both packages. `pkg/config`'s narrower run above (targeted, not full) is clean; the full `pkg/config` suite is
  known to have pre-existing, unrelated failures (confirmed again this phase via `git stash`: an
  `'auto_expose_ports' expected a map or struct, got "string"` decode error, present identically with and
  without this phase's changes — same issue Phase 1's log recorded against a `ca486fa` baseline).

## Deviations / design questions

1. **`allowed_projects` naming collision** — see above; resolved by the issue owner (option B, distinct field
   `AllowedGCPProjects`/`allowed_gcp_projects`), design updated to r7. No longer open.
2. No other design ambiguities were hit this phase. The S1/S3 "prove PreAuthorized bypasses policy" and
   "prove the access-token check isn't ANDed with the project check" test framings were my own choices to make
   the acceptance rows mutation-resistant per the brief's quality bar; not raised as questions since the design
   text already fully determined the intended behavior.
