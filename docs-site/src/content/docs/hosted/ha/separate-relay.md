---
title: Separate Conduit Relays
description: Run conduit relays as their own processes with scion server --role relay, and what a relay is trusted with.
---

:::caution[Development and test hubs only]
Separate relays need the `hub.conduit` [experiment](/scion/reference/experiments/). A separate relay requires targets (agents and Runtime Brokers) on version X or later. Until then, and until the other production gates are met, run separate relays on development and test hubs only.
:::

## Overview

By default every hub process runs its own conduit relay (`server.hub.conduit.relay_mode: in_process`). With `relay_mode: external`, the hubs run no relay. Separate relay processes hold the conduit sessions instead, and each one is started with `scion server --role relay`.

A relay is **policy-free**:

- **Admission.** The relay forwards the dialer's credential headers to a hub, and the hub decides whether to admit the dialer and as whom. Only an allowlist of credential headers is forwarded, and proxy identity headers never are.
- **RPCs and refreshes.** The hub answers every RPC a user sends on a relay-held session and checks every in-band credential refresh.
- **Re-checks.** The relay schedules re-checks of its sessions and user streams: periodically, on the hub's revocation events, and whenever its event connection reconnects. The hub evaluates each one, and the relay acts on the answer: close, renew or keep.

The relay keeps each session's hub admission token and presents it unchanged. It never opens, parses or logs it.

## Trust

**Relays are trusted for availability only, not for authorization.** A relay can refuse or drop sessions and streams, but it cannot admit a principal, widen what a principal may do, or keep a stream alive past the hub's decision:

- **No secrets.** A relay holds no session secret and no grant signing key. Its only credential is the relay-peer secret (`server.hub.conduit.peer_secret`), which authenticates its calls to the hubs and to other relays.
- **Hub decisions.** Admission, RPCs, refreshes and re-checks are decided by a hub. When the hub cannot be reached, the relay admits nothing new and closes nothing on its own, and each stream still ends at the authorization deadline the hub set.
- **Database access.** The relay reaches the database only for the conduit registry, the `experiments` settings row, and the hub's dedicated `scion_ev_conduit` event channel, which carries identifiers only. Run it under a least-privilege database role. See [Relay database access](/scion/reference/server-config/#relay-settings-serverrelay).

## Hub ingress

Clients and Runtime Brokers keep dialing the hub's own `GET /api/v1/conduit`. With `server.hub.conduit.ingress: proxy` (the default), the hub passes each dial on to a relay:

- **Selection.** The hub samples two eligible relays (recently seen and not draining) and picks the one holding fewer sessions. If the session count cannot be read, it picks one of the two at random.
- **The proxied dial.** The hub forwards only the WebSocket upgrade, to the relay's internal listener, authenticated as `relay-peer`. The client's credential headers, path, query and `Host` pass unchanged, so a Runtime Broker's signature verifies as sent. The hub removes any relay-peer or ingress header the client sent, and names itself in an ingress header that the relay trusts only on that authenticated leg.
- **Session lifetime.** On a proxied dial the hub sends its own `server.hub.conduit.lifetime_cap`, and the relay uses the smaller of that value and its own `lifetime_cap`. The hub's value can only shorten a session. If the value is missing or outside `90s`-`24h`, the relay uses its own cap and logs the cause `ingress_cap_invalid`.
- **Shutdown.** On SIGTERM a hub stops proxying first, so no conduit dial reaches a relay after that. A Runtime Broker's dial gets `503` with reason `conduit_not_serving`; other dials get the hub's general shutdown `503`. The hub then asks every live relay to send `GoAway` to the sessions it proxied, keeps those sessions' connections open until they close, and closes any that remain at its hold deadline. The hold deadline is the end of the shutdown's conduit drain step, which has the same time limit as the hub's own conduit drain (12s when `server.hub.conduit.shutdown_grace` is unset), or, if earlier, 1s before the hub's HTTP drain must end (with `shutdown_grace: 10s`, 5s after SIGTERM). The drain deadline sent to the relays ends at least 1s before the hold deadline, and a drain only ever shortens a session. Failed calls are counted in `scion.hub.conduit.ingress_drain_failed` and never extend the step. A client whose session is closed reconnects through another hub.
- **Host binding.** The relay tells the hub how a dial arrived. A dial that came through a hub must name one of the hub's public hosts. A dial made directly to a relay must name that relay's public host. Any other combination is refused with `403`.
- **Retries.** The hub tries another relay only when the first refused the dial before admission: a connection failure before any answer, a relay that was not the one the leg was signed for, or a relay that is draining or not ready (`503` with reason `relay_not_admitting`). Every other answer reaches the client unchanged. When no relay is left, the dial gets `503` with reason `conduit_not_serving`.

Proxy ingress needs HMAC relay-peer authentication (`server.hub.conduit.peer_auth: hmac`), because on the proxied dial `Authorization` belongs to the client. Under any other resolved peer-auth mode (an explicit `oidc`, or the default `auto`, which resolves to `oidc` on GCP), the hub still starts, but its ingress proxy stays off. Conduit dials then get `503` with reason `conduit_not_serving` and no request reaches a relay. The hub logs a warning once at startup with the cause `ingress_requires_hmac`, and the readiness gauge `scion.hub.conduit.eligible_relays` carries the attribute `cause`. The HTTP readiness endpoint is not affected. To serve dials, set `peer_auth: hmac`. `redirect` is not supported yet; the hub refuses to start with it.

## Running a relay

A relay reads its own configuration file, never the hub's. It needs:

- `server.relay.hub_internal_url`: the hub internal API, reachable only inside the cluster or VPC.
- `server.relay.public_url`: the URL dialers reach this relay at.
- `server.hub.conduit.peer_secret`: the same secret the hubs use.
- `server.hub.conduit.internal_listen`, and `internal_advertise` where the bound address is not reachable: the relay internal API that the hubs and other relays call.
- `database.url`: a relay-scoped database role.

The relay serves conduit sessions on `server.hub.host` and `server.hub.port`. It serves no hub API, web UI or Runtime Broker there.

```bash
scion server start --role relay --config /etc/scion-relay
```

The relay refuses to start while `hub.conduit` is off. If the setting cannot be read, it retries for up to a minute and then fails. While it runs, it re-reads the experiment every 15 seconds and on every settings change. Turning the experiment off stops admission and drains the open sessions, but the process keeps running. Turning it back on resumes admission.

At startup the relay logs the configuration files it loaded and its database driver, never the connection string.

See [Relay Settings](/scion/reference/server-config/#relay-settings-serverrelay) for every setting.
