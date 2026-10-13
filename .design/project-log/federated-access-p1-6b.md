# Federation user credentials on the receiving hub (phase 1, item 6, part B)

**Date:** 2026-10-13
**Stacks on:** item 6 part A (issuing side)
**Tracked in:** ptone/scion#4603

## What changed

- **Setting.** Trusted issuer flag `accept_user_tokens` (default false) in
  `server.federation.trusted_issuers[]`: config struct, V1 wire format (the
  two structs are converted by direct type conversion, so the field is last
  on both), JSON schema, docs. Only valid for `issuer_type: hub` (or the
  empty default); any other issuer type with the flag fails validation.
  `allowed_domains` is now also valid on a hub issuer with the flag, where it
  constrains that hub's user access credentials (exact, case-insensitive,
  no subdomains, same as for the Google issuer). The Google issuer is never
  treated as a peer hub.
- **Kind dispatch** (`extractHubTokenIdentity`, `pkg/hub/federation_auth.go`),
  issuer_type hub only:
  - `scion_token_kind` `"agent"`, or no kind (credentials from hubs that
    predate the claim): `FederatedAgentIdentity`, unchanged, including the
    `allowed_projects` / `allowed_root_users` checks. A credential without
    the claim is always read as an agent, whatever other claims it carries.
  - `"user"`: accepted only if the experiment is on and the issuer has
    `accept_user_tokens`. It must have `sub`, `email`, `email_verified:
    true` and none of `project_id`, `agent_name`, `ancestry`, `root_user`,
    and pass `allowed_emails` and `allowed_domains`. It becomes a
    `FederatedUserIdentity` (role viewer, no scopes: the issuer's
    `default_scopes` are agent scopes), then the visitor identity through
    item 1's lookup once the (issuer, subject) pair is linked. The agent
    constraints are not applied to it.
  - Any other kind value is refused.
  - `issuer_type: user` and `service_account` issuers do not read the claim.
- **Refusal shape.** Every refusal is an `Authenticate` error, so the
  middleware answers it like any other verification failure: **401,
  "invalid federation token"**. The reason is logged at debug level only.
- **Experiment gate.** `federation.project_visitors`, checked server side on
  every request: `AuthConfig.FederationHubUserTokensEnabled` (wired to
  `Server.federationHubUserTokensEnabled`, which calls `experimentEnabled`)
  feeds `FederationAuthOptions.AcceptHubUserTokens`. While off, user-kind
  credentials from hub issuers are refused and agent credentials behave as
  before. `Authenticate(token)` without options refuses them too (fail
  closed for any caller that does not opt in).
- **Live gate.** The middleware loads the current authenticator from the
  server's atomic pointer per request, and a federation settings reload
  (`ApplySnapshot`) installs a new one built from the new config. The flag
  is read from that config inside each `AuthenticateWithOptions` call;
  nothing caches the decision.

## Tests

`pkg/hub/federation_peer_user_token_test.go`: two in-process hubs. The peer
mints real credentials with its federation-token and identity-token
handlers (and its OIDC signer for shapes those handlers never produce); the
receiving hub verifies them through the peer's published JWKS, and its
configuration changes go through `ApplySnapshot`. Covered: minted user
credential accepted and linked to a visitor; refused without the flag;
each agent claim refused; unknown kinds refused; email_verified false or
missing, email or sub missing; allowed_domains and allowed_emails; agent
constraints not applied to user credentials; minted and legacy agent
credentials unchanged (flag off, experiment off, allowed_projects still
applied); flag off and on without restart; experiment off; user issuers
unchanged. Config: validation table cases, V1 round trip, schema keys.

## Notes for later items

- Item 8: the admin Federation page does not edit `accept_user_tokens` (it
  already drops `allowed_domains` and `allowed_gcp_projects` when an issuer
  is saved from the page); add the field there.
- Item 7 (bridge): the bridge's own classification rule for credentials
  without the claim (email and no agent claims means user) differs from the
  hub's (no claim means agent). A user credential from a peer hub always
  carries the claim, so both agree on every credential part A mints.
