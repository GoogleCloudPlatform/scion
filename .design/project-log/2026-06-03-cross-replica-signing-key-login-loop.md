# Fix: cross-replica login loop (`session_expired`) after cookie-store fix

**Date:** 2026-06-03
**Branch:** postgres/wave-b-integration
**Symptom:** After OAuth login the dashboard flashes, then the browser is
redirected to `/login?error=session_expired&returnTo=/`, repeatedly.

## Background

Commit `0515e2a8` replaced the per-replica gorilla `FilesystemStore` with an
encrypted+signed `CookieStore` whose keys derive from the shared
`SESSION_SECRET`, so the whole web session (OAuth state + Hub JWTs) rides in the
client cookie and any replica can read it. That fixed the OAuth `state_mismatch`
and made the *session container* replica-portable.

## Root cause (one layer deeper)

The cookie is portable, but the **Hub JWT inside it is signed with a per-replica
key**. Signing keys are resolved by `ensureSigningKey()` scoped to
`(scope=hub, scope_id=hubID)`, and `hubID = sha256(hostname)[:12]`
(`DefaultHubID`). The integration deployment runs **two replicas of one logical
hub** behind a single LB (`multi.demo.scion-ai.dev`), sharing one Postgres DB
and one `SESSION_SECRET`, but with different hostnames:

| Replica | hub_id | user_signing_key fp |
|---|---|---|
| scion-integration  | `ca39430276ee` | `9a35ae24cfeedba0` |
| scion-integration2 | `9662ebe99da4` | `97d3f30a36554d7a` |

So each replica minted/validated user JWTs with a *different* HS256 key. When a
post-login request landed on the replica that did **not** mint the token,
`ValidateUserToken` failed (`go-jose: error in cryptographic primitive`),
refresh failed too (the refresh token is signed with the same foreign key), and
`sessionToBearerMiddleware` declared the session "irrecoverably invalid",
**deleted the cookie** (`MaxAge=-1`) and returned `session_expired`. The cookie
deletion is what turns it into a loop. Logs show the same user alternating
between "User authenticated" and "Hub token irrecoverably invalid, clearing
session" depending on which replica served the request.

## Fix

Extend the `0515e2a8` philosophy from the cookie to the keys inside it: derive
the agent and user JWT signing keys deterministically from the shared
`SESSION_SECRET`.

- `ServerConfig.SharedSigningSecret` (new field).
- `ensureSigningKey()`: when `SharedSigningSecret != ""`, return
  `deriveSharedSigningKey(secret, keyName)` (domain-separated by key name),
  bypassing per-host secret-backend storage. Empty secret → unchanged per-hub
  behavior (no regression for single-node/local dev).
- `cmd/server_foreground.go`: new `resolveSessionSecret()` helper feeds the same
  value into both the web cookie store and `hubCfg.SharedSigningSecret`.

Now every replica with the same `SESSION_SECRET` agrees on the signing keys,
regardless of hostname/hubID — no operator coordination (matching HubID) needed.

## Tests

`pkg/hub/signing_key_shared_test.go`:
- derivation is deterministic, 32 bytes, domain-separated, secret-sensitive;
- two servers with **different hubID, same secret** derive identical keys and a
  token minted on one validates on the other; a different secret cannot;
- an explicit pre-configured key still wins over derivation.

## Deploy note

Rolling out the new binary changes the signing keys (they now derive from
`SESSION_SECRET` instead of the stored per-host keys), so existing web sessions
and CLI tokens are invalidated **once** — users log in again, CLI/agents
re-auth. Both replicas already share `SESSION_SECRET`, so no config change is
required. (Faster stopgap without a rebuild: pin the same
`SCION_SERVER_HUB_HUBID` on both VMs to an existing hub ID so they share the
already-stored keys.)
