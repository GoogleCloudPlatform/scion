---
title: Separate Conduit Relays
description: Run conduit relays as their own processes with scion server --role relay, and what a relay is trusted with.
---

:::caution[Development and test hubs only]
Separate relays need the `hub.conduit` [experiment](/scion/reference/experiments/). Until their production gates are met, run them on development and test hubs only.
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
