# Auth Proxy Mode — Phase 3 Implementation (Deployment Docs)

**Date:** 2026-06-05
**Branch:** scion/auth-proxy-mode
**Author:** Scion Agent (auth-proxy-phase3)

## Summary

Created the deployment guide for the IAP + Cloud Run-invoker topology,
completing Phase 3 (the final phase) of the auth-proxy-mode feature.

## Files Added/Changed

| File | Action |
|------|--------|
| `docs-site/src/content/docs/hub-admin/auth-proxy-iap.md` | **New** — Full deployment guide |
| `docs-site/astro.config.mjs` | Edit — Added sidebar entry under Hub Administration |
| `.design/project-log/auth-proxy-mode-phase3.md` | **New** — This project log |

## Documentation Coverage

The guide covers all five deliverable sections:

1. **Overview** — Three exclusive auth modes (`oauth`, `proxy`, `dev`) and when to
   pick proxy/IAP.
2. **Inbound (human IAP)** — `auth.mode=proxy` + `auth.proxy` config with full YAML
   examples; audience format for GCE/GKE backend services vs App Engine; issuer/JWKS
   overrides; `require_trusted_proxy_ip`; middleware precedence; provisioning behavior
   (lazy, allow-list-gated, auto-create); suspended user rejection; logout semantics.
3. **Outbound (agent transport auth)** — Dual-layer model (outer OIDC + inner
   X-Scion-Agent-Token); `auth.transport` config (`mode`, `oidc_audience`,
   `platform_auth_sa`); dispatch env vars (`SCION_TRANSPORT_TOKEN`,
   `SCION_TRANSPORT_AUDIENCE`, `SCION_TRANSPORT_TOKEN_EXPIRY`); refresh `tokens[]`
   array; agent-side token source selection (injected vs metadata vs disabled);
   audience selection for IAP vs Cloud Run invoker.
4. **Security notes** — Signed-only trust model; audience binding; IAP-only reachability;
   JWKS rotation; clock skew; suspended users.
5. **End-to-end GCP setup checklist** — IAP enablement, OAuth client/audience,
   transport SA creation, IAM bindings, hub config, verification steps, reference
   to cloudrun scripts.

## Config Key Verification

All config keys and env vars were verified against the shipped code:

### Settings.yaml keys (V1 snake_case format)
- `auth.mode` — V1AuthConfig.Mode
- `auth.proxy.provider` — V1ProxyConfig.Provider
- `auth.proxy.iap.audience` — V1IAPConfig.Audience
- `auth.proxy.iap.issuer` — V1IAPConfig.Issuer
- `auth.proxy.iap.jwks_url` — V1IAPConfig.JWKSURL
- `auth.proxy.require_trusted_proxy_ip` — V1ProxyConfig.RequireTrustedProxyIP
- `auth.transport.mode` — V1TransportConfig.Mode
- `auth.transport.oidc_audience` — V1TransportConfig.OIDCAudience
- `auth.transport.platform_auth_sa` — V1TransportConfig.PlatformAuthSA
- `auth.user_access_mode` — V1AuthConfig.UserAccessMode
- `auth.authorized_domains` — V1AuthConfig.AuthorizedDomains
- `hub.admin_emails` — V1ServerHubConfig.AdminEmails

### Env vars (dispatch payload)
- `SCION_TRANSPORT_TOKEN` — httpdispatcher.go
- `SCION_TRANSPORT_AUDIENCE` — httpdispatcher.go
- `SCION_TRANSPORT_TOKEN_EXPIRY` — httpdispatcher.go

### Agent-side env vars
- `SCION_HUB_OIDC_AUDIENCE` — oidc.go:EnvHubOIDCAudience
- `SCION_TRANSPORT_TOKEN` — oidc.go:EnvTransportToken
- `SCION_TRANSPORT_AUDIENCE` — oidc.go:EnvTransportAudience

## Discrepancies Between Design Doc and Shipped Code

### No discrepancies found
All config keys, env vars, and behavior documented in the guide match the
shipped implementation. Minor differences noted:

- **Dispatch schema**: The design doc proposed using the same `tokens[]` JSON
  shape for both dispatch and refresh. The shipped implementation uses individual
  env vars for dispatch (`SCION_TRANSPORT_TOKEN`, `SCION_TRANSPORT_AUDIENCE`,
  `SCION_TRANSPORT_TOKEN_EXPIRY`) and `tokens[]` JSON array for refresh. This was
  already documented in the Phase 2 project log as a pragmatic deviation matching
  existing dispatch conventions.

## Build Verification

- Docs build (`npm run build`) could not be run — requires Node.js ≥22.12.0,
  only v20.20.2 available in the sandbox.
- Frontmatter format verified manually against sibling pages (title + description).
- Sidebar entry added to `astro.config.mjs` matching the existing pattern.
