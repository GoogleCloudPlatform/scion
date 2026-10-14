# Principal check on federation credentials (ptone/scion#5080, within ptone/scion#4603 item 6B)

## What
- `AuthenticateWithOptions` (pkg/hub/federation_auth.go), the only verifier of federation
  credentials, now runs `federationPrincipalRefusal` last. It refuses, with the usual 401, a
  user or service account identity whose email is in a reserved test identity domain, has the
  federated account address shape, or is this hub's platform service account
  (`FederationAuthOptions.PlatformAuthSA`, set by `UnifiedAuthMiddleware` from
  `AuthConfig.PlatformAuthSA`). Agent identities carry no email and are unchanged.
- `extractHubUserClaims` also refuses an email in the `gserviceaccount.com` domain or any
  subdomain (`isServiceAccountDomainEmail`: lower-cased, trailing dot ignored, exact domain or
  subdomain suffix, as for the reserved test identity domains) on a user access credential from
  a hub issuer. Issuer_type user and service_account issuers are unchanged by that rule. It is
  two commits (the rule and its domain match) so it can be dropped on its own; the shared
  `isGoogleServiceAccount` used by the Google credential validator is not changed.
- `TestFederationPrincipalCheckCensus` (pkg/hub/federation_principal_check_census_test.go, no
  build tag, AST only) classifies every pkg/hub function that touches federation credential
  acceptance and checks its class. An unclassified or stale site fails.

## Findings
- A receiving hub sees only the peer's claims, so it can match emails against its own
  configuration only. A peer hub's platform service account or test driver list is not known
  here; the issuing hub already mints user access credentials only for active human users.
- Test driver principals (`server.auth.proxy.driver_principals`) are not on main yet
  (GoogleCloudPlatform/scion#3535). When they land, pass that list in `FederationAuthOptions`
  and refuse its emails in `federationPrincipalRefusal`, preferably through the shared
  predicate that change introduces.
- On the peer hub user access path the service account email rule refuses the platform service
  account before the principal check runs, so request-level tests of the platform service account
  rule use an issuer_type user credential, where other service account emails are accepted.
