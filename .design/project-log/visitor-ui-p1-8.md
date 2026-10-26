# Project Visitors tab and the per-issuer visitor access switch (phase 1, item 8)

**Date:** 2026-10-18
**Stacked on:** the A2A bridge visitor item (phase 1, item 7)
**Tracked in:** ptone/scion#4603

## What changed

- **Visitors tab (project settings > Members).** `project-settings.ts`
  renders Members and Visitors as two tabs when
  `projectVisitorsVisible(_capabilities)` holds: the
  `federation.project_visitors` experiment is on in the browser flag layer
  and the project capabilities carry `manage_visitors`. Otherwise the
  Members section renders exactly as before. The hub enforces both checks
  on every visitor route; the browser check only decides what is shown.
- **`scion-project-visitors-editor`** (new, `components/shared/`). It lists
  visitor rules (by domain) and visitor grants (by email) from
  `GET /visitors` and `GET /visitors/rules`, with issuer, status, granted by
  (looked up with `principalName`) and expiry. Status is Pending, Active or
  Suspended, or "Issuer disabled by hub admin" while `issuerAccepted` is
  false. Add posts to `/visitors` (email) or `/visitors/rules` (domain) with
  an optional expiry preset. Extend sends `PATCH` with the stored
  `notBefore` and the new `expiresAt`, because the hub replaces the whole
  window. A visitor that a rule added has no Extend (its window follows the
  rule). Revoke and Remove send `DELETE` after a confirm. Hub refusals are
  shown with their own text.
- **Issuer field.** `loadVisitorIssuerChoices` in
  `shared/project-visitors.ts` is the only place that decides it. A hub admin
  (`canReadHubIssuers`: admin or super-admin, with `hub.config.read`) gets a
  picker of the trusted `hub` and `user` issuers whose
  `allow_visitor_grants` is on, read from `GET /api/v1/admin/server-config`.
  Any other caller sends no admin request. They get a text field checked
  for shape (an `https://` URL with no credentials, query or fragment), with
  suggestions taken only from the project's own grants and rules that have
  `issuerAccepted`.
- **Admin > Federation.** The issuer dialog has the switch "Allow project
  owners to grant visitor access to users from this issuer"
  (`allow_visitor_grants`) for `hub` and `user` issuers, shown while the
  experiment is on. It is dropped on save when the type is
  `service_account`, which the hub refuses. The Default Role dropdown is
  gone. When the dialog saves an existing issuer, it now keeps the fields it
  does not edit (`default_role`, `allowed_gcp_projects`, `allowed_domains`,
  `accept_user_tokens`) as stored; before, it dropped them.
- **Docs.** The Web Dashboard page describes the Visitors tab and the switch.
  `auth.md` and the server-config reference mark `default_role` as
  deprecated (accepted and ignored).

## Decisions and deviations

- No hub behaviour change. Design 4.5 asks for an issuer picker for every owner.
  No route an owner can call lists the accepted issuers, so only hub
  admins get the picker; others type the issuer. A project-scoped issuer
  route would only change `loadVisitorIssuerChoices`.
- The design's "last active" column is left out: the visitor list response
  carries no last-seen field.
- Both points were decided before coding; the follow-ups are tracked
  separately.

## Verification

Nothing was built or run in this item (no npm, no go). The web checks
(typecheck, lint, unit tests, build) run in a later npm lane. Formatting
was done by hand to the repo's Prettier settings, so `npm run lint`
(`prettier/prettier`) is the first check to watch.

## Follow-up changes after review

- `federation.project_visitors` is registered with the web layer as well
  as the server layer. `GET /api/v1/experiments` carries only web-layer
  experiments, so without it the Visitors tab and the admin switch never
  rendered. Tests that set `window.__SCION_FEATURES__` directly could not
  catch this; a Go test now checks the resolved map, and the existing
  `resolvedExperiments` test that counted the map was updated.
- The status badge follows the grant or rule window: **Expired** and
  **Not yet active** come before the account status (the issuer label
  still comes first). Test fixtures use instants relative to the run so
  they do not age.
- The issuer text field allows a path but refuses non-https URLs, URLs with
  no host, a query, a fragment or userinfo, and metadata paths
  (`/.well-known/`, `/openid-configuration`, `/jwks.json`). The value is
  sent trimmed of surrounding whitespace only.
- The admin dialog keeps stored fields it does not show only while they
  apply to the saved issuer type. The hub picker is empty while federation
  is off. The row action is labelled **Change expiry**.
- Nothing was built or run in this round either; gofmt, `git diff --check`
  and a word scan ran.
