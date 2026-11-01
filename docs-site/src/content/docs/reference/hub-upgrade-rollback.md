---
title: Hub Upgrade and Binary Rollback
description: The settings to check before upgrading a Hub, which stored data a newer Hub build rewrites one way on startup, why rolling the binary back afterwards is unsupported, and the startup warning an older build logs.
---

A Hub upgrade is one-way. When a newer Hub build starts on a database (and on
the settings files of its host) for the first time, it rewrites some stored
data to newer forms. Older builds do not write that data back. **Running an
older Hub binary after a newer build has started once (a binary rollback) is
unsupported.** To return to an older build, restore the database and settings
files from a copy taken before the newer build first started.

## Upgrade checklist: settings to check first

Some releases add a setting that a Hub needs for a given deployment. Check
these before you upgrade. The Admin -> Maintenance update operations
(`update-binary` and `rebuild-server`) print a `==> Settings check` section in
their log that lists the required settings this Hub leaves unset, and
`scion server preflight` reports the same as warnings. Both only warn: the
update goes ahead.

### `server.trusted_proxies` (Runtime Broker transport)

The Hub accepts Runtime Broker registration, joins, secret rotation, the
control channel and the conduit only over a confidential transport: TLS, a
loopback request, or a request from a TLS-terminating proxy it trusts. See
[Runtime Broker transport and trusted proxies](/scion/reference/server-config/#runtime-broker-transport-and-trusted-proxies).

1. **Find where TLS ends.** If the Hub's public URL is `https`, TLS ends at a
   proxy in front of the Hub (a load balancer, an ingress, Caddy, nginx).
2. **Pick the value of `server.trusted_proxies`:**
   - **Not set:** a proxy on the Hub's own host that connects from loopback
     (`127.0.0.1`, `::1`) is trusted. The Hub logs one warning at startup.
     This is enough for Caddy or nginx installed on the Hub's host.
   - **A list:** the addresses the proxy connects to the Hub from. **A proxy
     on another host must be listed.** **A proxy container on a bridge
     network must be listed too:** it connects from the bridge address (for
     example `172.17.0.1`), not from loopback.
   - **`[]`:** trust no proxy. Only TLS to the Hub itself, or a loopback
     request without forwarding headers, is accepted.
3. **After the upgrade, check the command path.** `scion doctor` counts
   Runtime Brokers that are "online but cannot receive commands", and
   `scion runtime-broker status` prints a `Commands:` line for each hub
   connection and, while the broker is online, for the broker as the Hub
   sees it. A Runtime Broker is
   online when its heartbeats arrive, which does not use the control channel;
   `Commands: yes` means the control channel is connected.
4. **Reading a refusal:**
   - The client gets `403` with the code `transport_not_confidential` and
     `details` `{"reason": ..., "setting": "server.trusted_proxies"}`. The
     reason is `untrusted_local_proxy` or `local_proxy_not_trusted` (a proxy
     on the Hub's host that is not trusted), `proxy_proto_not_https` (the
     proxy did not forward `https`) or `not_tls` (a plain request from another
     address, such as a bridge network proxy or a proxy on another host).
   - The Hub logs a refusal from a proxy on its own host at ERROR and reports
     the health check `broker_transport_proxy` for 15 minutes; other refusals
     are a WARN that names `server.trusted_proxies`.
   - The Runtime Broker logs an ERROR naming the reason and
     `server.trusted_proxies`, and reports its `control_channel` (and
     `conduit`) check unhealthy. The Hub's health summary then shows the
     broker as online but unable to receive commands.

## What a newer build rewrites one way

| Stored data | What startup does | Effect on an older binary |
| :--- | :--- | :--- |
| Built-in role definitions and their revision markers (`hub_settings` keys `builtin_role.revision.<role>`) | The role reconciler sets each built-in role's permission list to the build's list and records the build's role revision. It never lowers a revision: a role whose stored revision is higher than the build's is left alone. | For each role whose revision the newer build raised, the older build keeps the newer build's permission list, because it will not lower the revision, and it logs the warning described below. A role whose revision the newer build did not raise, but whose computed permission list differs between the builds, is rewritten to the older build's list at startup, with no warning. |
| Project members group marker (group annotation) | A one-time migration rewrites the old key `scion.io/system-project-members-group` to `scion.io/project-members-group`, and records completion in the `hub_settings` key `migration_legacy_project_members_group_marker_v1`. | A build from before the marker change recognises only the old key, so it does not recognise the rewritten groups as project members groups. |
| Settings files (`settings.yaml`, global and per project) | When the file is loaded, a legacy `hub.grove_id` key is rewritten in place to `hub.project_id`. | A build that reads only `hub.grove_id` no longer finds the project link. |
| `.scion` project marker files | When a marker file is read, legacy `grove-id`, `grove-name` and `grove-slug` keys are rewritten in place to `project-id`, `project-name` and `project-slug`. | A build that reads only the `grove-*` keys no longer finds them. |
| `templates` table (`config` and `image` columns) | After schema migration, startup drops the retired `templates.config` and `templates.image` columns on SQLite and PostgreSQL (`templates.agent_config` is kept). | An older build that queries or writes `templates.config` or `templates.image` fails against the migrated schema. |

Other one-time data migrations record their completion in `hub_settings` keys
whose names start with `migration_`. They are not reverted either.

## The startup warning

At startup, the role reconciler checks each built-in role's stored revision
against the running build's. When one or more stored revisions are higher
(a newer build has started on this database), it leaves those roles
unchanged and logs one warning:

```text
level=WARN msg="Stored built-in role revisions are newer than this Hub build's. A newer Hub build has started on this database; this build does not lower the roles, so it runs with the newer build's permission lists. Running an older Hub binary after a newer build has started (a binary rollback) is unsupported." roles="hub-member (stored revision 5, this build's revision 4)" count=1
```

`roles` names each affected role with its stored revision and the running
build's revision. The Hub still starts. Nothing is logged when every stored
revision is equal to or lower than the build's. If you see this warning, run
the newer build again, or restore the database and settings files from
before the newer build first started.
