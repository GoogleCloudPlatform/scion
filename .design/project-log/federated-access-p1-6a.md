# Federation user credentials on the issuing hub (phase 1, item 6, part A)

**Date:** 2026-10-12
**Branch:** scion/mmr-fed-p1-6a (stacked on scion/mmr-fed-p1-1)
**Tracked in:** ptone/scion#4603

## What changed

- **Route.** `POST /api/v1/auth/federation-token` with `{"audience": "<peer
  hub issuer URL>"}` returns `{"token", "expires_at"}`: a short-lived user
  credential, signed with the hub's existing OIDC key (same key manager and
  JWKS as agent identity tokens), that names the caller to one peer hub.
  It is gated by the `federation.project_visitors` experiment: 404 while
  off, and also 404 when the OIDC provider (`server.oidc.enabled`) is off.
- **Callers.** An interactive session, or a hub-boundary user access
  credential carrying the new scope `federation_token:mint`, of a stored
  user whose kind is human and whose status is active. Dev, federation,
  visitor, agent, broker, delegated, test identity and every other caller
  gets one 403 with one message. A missing audience is 400; an audience not
  in the allow-list (including an empty list) is 403.
- **Scope.** `federation_token.mint` (scope `federation_token:mint`) is a
  self permission (target class self, hub boundary only), like
  `user_skill_injection.update`: any active local user may put it on a hub
  token; no role or seed change. The handler checks it with
  `selfScopedDecision`.
- **Settings.** `server.oidc.user_token_audiences` (list, default empty,
  meaning nothing is issued) and `server.oidc.user_token_lifetime` (default
  15m, whole seconds, at most 1h; JWT times are whole seconds, so a
  fractional lifetime would make the response `expires_at` differ from
  `exp`). Layer-0 next to the issuer settings: **read at startup,
  a change needs a restart.** Invalid values fail startup.
- **Claims.** `iss`, `sub` (local user ID), `aud` (normalized, no trailing
  slash), `email`, `email_verified: true`, `name` (omitted when empty),
  `iat`, `nbf`, `exp`, `jti`, `scion_token_kind: "user"`. Never
  `project_id`, `agent_name`, `ancestry` or `root_user`.
- **Agent identity tokens** now always carry `scion_token_kind: "agent"`.
  Not gated by the experiment: the claim is additive, receiving hubs ignore
  unknown claims, and an ungated claim keeps the token shape independent of
  a toggle. The discovery document lists the new claims.
- **Audit.** Each issue writes a `federation_token_issued` mutation audit
  record (target the user; after: audience, jti, expires_at) before the
  credential is returned. If the write fails, the request answers 500 and
  nothing is issued. Catalog operation `federation.token.mint`.
- **CLI.** `scion hub auth federation-token --audience URL [--json]` prints
  the credential (hubclient `AuthService.FederationToken`, never retried).
  Human mode only: `hub.auth` is not in `agentAllowed`. The design text says
  `scion auth federation-token`; there is no top-level `auth` group, so the
  command sits beside `hub auth login/logout`.

## Notes for later items

- Part B (receiving hub): classify by `scion_token_kind`, `accept_user_tokens`
  on hub issuers. Tokens from hubs without this change carry no kind.
- Item 8: moving the two settings to Layer-1 (hot reload) needs the admin
  Federation page or a new admin field, because the web settings tests
  require every Layer-1 section key to be edited and sent by the page.
- The per-user rate limit reuses the OIDC identity token limiter, keyed
  `user:<id>`.
