---
title: Test Identities on Another Test Hub
description: Use CLI target-hub profiles to issue and use test identities on another test hub from an agent or a workstation.
---

A **target-hub profile** lets the CLI issue and use [test identities](/scion/reference/cli/)
on another test hub, called the *target* here. It is meant for test runners, usually agents on
one hub that drive journeys on a separate test hub. A profile is a named, non-secret
description of the target. All tokens are minted in memory when a command runs.

The pieces:

- **The target hub** runs with test identities on, and allows its own issuer service account
  as a workload issuer (`server.hub.test_identities.workload_issuers`). It publishes its
  issuer audience in the public `GET /api/v1/test-infra/status` (`testIssuerAudience`).
- **A human issuer on the target** creates a *workload issuer delegation*. This is the consent
  step: it lets the target's issuer service account issue member and viewer identities within
  that person's own live `test_identity.issue` authority.
- **The runner** holds a profile naming the target, its service accounts and the delegation id.
  It obtains short-lived ID tokens through its assigned service account, so it needs no key
  file or long-lived credential.

## Operator prerequisites

These are set up once per target by the operators of the two hubs. Scion documents them but
does not configure or enforce them.

- **Service accounts on the target side.** Use `<T>-issuer`, the target's issuer service
  account (allow-listed on the target by its numeric unique id). If the target is behind IAP,
  also use `<T>-uat`, the target's IAP service account, with IAP access to the target.
- **The runner's assigned service account** is the GCP service account the runner agent runs as
  (`assign` metadata mode on a docker broker). Grant it
  `roles/iam.serviceAccountOpenIdTokenCreator` on `<T>-issuer`, and on `<T>-uat` when the
  target uses IAP. Nothing else is needed: the runner calls IAM Credentials `generateIdToken`
  *as* those accounts, using its own access token from the metadata server.
- **Egress.** The runner's container must reach `iamcredentials.googleapis.com`.
- **Uniqueness and narrow assignment are the operator's responsibility.** Anyone who can run
  as the assigned service account can mint the target's issuer tokens while a delegation is
  live. Keep that service account unique to the runner, and assign it only to the agents that
  need it. Scion does not check this.
- **Attribution.** The target's audit records the delegation, the grantor and the issuer
  account's subject. It does not record which agent minted the token. That record lives in
  the IAM Credentials data access logs of the issuer account's project, which we recommend
  turning on.
- **Revocation.** Revoking the delegation on the target stops new issuance at once. Removing
  the IAM grant stops new issuer tokens; tokens already minted stay valid until they expire (at
  most an hour). Identities already issued are unaffected either way: tear the run down, or
  delete them, to end them.

## 1. Create the delegation (human, on the target)

Sign in to the target interactively. Then create a member-tier delegation pinned to the
issuer account's unique id:

```bash
scion hub auth login --hub https://t1.example.com
scion --hub https://t1.example.com hub test-identity delegate create \
  --workload-sub 123456789012345678901 --ttl 8h --member-cap 5 --label "uat window"
```

The command prints the delegation id, which is not a secret. Use `delegate list` and
`delegate revoke <id>` to manage delegations. These commands take a human sign-in only, and
are not available in agent mode.

## 2. Add the profile (runner)

```bash
scion hub profile add t1 \
  --endpoint https://t1.example.com \
  --transport iap \
  --iap-audience 1234567890-abc.apps.googleusercontent.com \
  --uat-sa t1-uat@my-project.iam.gserviceaccount.com \
  --issuer-sa t1-issuer@my-project.iam.gserviceaccount.com \
  --issuer-delegation member=<delegation id>
```

The profile is saved to `~/.scion/hub-profiles/t1.yaml` (mode `0600`) and holds only the
fields above. For a target without IAP, use `--transport none` and leave out `--iap-audience`
and `--uat-sa`.

Select the profile for a command with `--hub-profile t1`, or for a whole session with
`SCION_HUB_PROFILE=t1`. Name the run with `--run <label>` or `SCION_HUB_RUN`. `--as` and
`--run` are flags of the `hub` and `artifact` commands and apply only with a profile.

:::caution
A profile must name another hub. The CLI refuses a profile whose endpoint names the agent's own
hub (`SCION_HUB_ENDPOINT` or `SCION_HUB_URL`), and refuses every profile when either of those is
set but is not an absolute URL. The check compares parsed host names and ports: lowercase,
without a trailing dot, IP literals in canonical form, and loopback and container bridge host
names (such as `host.docker.internal`) treated as one host. It does not resolve DNS aliases, so
a profile naming another name for the agent's own hub, such as its public URL while
`SCION_HUB_ENDPOINT` holds a bridge address, is not detected. The target hub's own delegation
remains the boundary.
:::

## 3. Issue identities into the run cache

```bash
export SCION_HUB_PROFILE=t1 SCION_HUB_RUN=r1
scion hub test-identity issue --as owner --role member --purpose "project journey"
scion hub test-identity issue --as reader --role viewer
```

Each identity is kept at `~/.scion/hub-profiles/t1/run-r1/<alias>.json`. The file is created
exclusively with mode `0600`, an existing file or a symlink is refused, and the alias must be
new in the run. The token is never printed. The file also holds `tokenExpiresAt`, so a
journey can mark a step as blocked when its token expired instead of failing for an unclear
reason. Only member and viewer identities can be issued through a profile.

## 4. Act as an identity

Pass `--as <alias>` to use a cached identity on the target:

```bash
scion hub status --as owner
scion hub projects create --name uat-throwaway --as owner
scion hub projects --as owner
scion artifact publish report.md --project uat-throwaway --as owner
scion hub projects delete uat-throwaway --as owner --yes
```

With a profile selected, the bearer comes **only** from the profile: the issuer delegation for
`hub test-identity` commands, or the `--as` identity elsewhere. A login,
`~/.scion/scion-token`, `SCION_AUTH_TOKEN` and `SCION_HUB_TOKEN` are never read. A command with
no applicable profile credential fails rather than falling back to another credential.

Only these commands run with a profile:

- `hub status`
- `hub profile`
- `hub test-identity`, except `delegate` and `web-code`
- `hub projects`: list, `info`, `create` and `delete`, with the project named explicitly
- `artifact publish`, `get`, `versions` and `delete`; `publish` needs `--project <name or id>`
  on the target

They read no local project settings. Every other command, such as the agent commands,
`whoami` and anything that touches local state, is refused while a profile is selected.

A member token used with `--as` is re-issued automatically through the delegation within 5
minutes of its expiry. `scion hub test-identity token --as owner` re-issues it at once. Once
the identity itself has expired, commands are refused with "issue a new one".

## 5. Check the gate with `hub status`

```bash
scion hub status            # the issuer delegation
scion hub status --as owner # an identity
scion hub status --as owner --json
```

The output shows:

- the profile and endpoint;
- the bearer source (`profile:issuer` or `profile:<alias>`);
- the identity's email, kind, tier and expiry;
- the transport principal and audience;
- the target's issuer audience.

All of these are decoded locally; no token is printed. The command probes the target with the
profile credential and exits non-zero in three cases:

- the request did not carry the profile's bearer, or carried another credential;
- the transport principal is not the profile's IAP service account;
- the target refused the credential.

Run it before a journey starts.

## 6. Tear down the run

```bash
scion hub test-identity teardown --run r1          # delete
scion hub test-identity teardown --run r1 --purge  # purge what the identities created too
```

Teardown deletes every identity of the run. That covers each cached alias, plus any identity
the delegation still lists whose purpose names the run. It then removes the run's cache. An
identity that is already gone counts as done. If anything is left, the command lists it and
exits non-zero; run it again.

## Agent mode

Inside an agent (`SCION_CLI_MODE=agent`), these commands are available:

- `hub status`
- `hub profile`
- `hub test-identity` `issue`, `token`, `list`, `delete` and `teardown`
- `artifact publish`, `get` and `versions` (agent commands with or without a profile)

`hub status` and the `hub test-identity` commands run only with a target-hub profile whose
endpoint is not the agent's own hub. Without a profile, or with a profile for the agent's own
hub, they are refused. With a profile selected, the `artifact` commands act on the target as the
`--as` identity, and the profile for the agent's own hub is refused for them too.
`hub test-identity delegate` (the human consent step), `hub test-identity web-code`, the other
`hub` commands and `hub projects` are not available in agent mode.

## Troubleshooting

- **"SCION_METADATA_MODE is not "assign""**: the runner has no assigned service account.
  Assign one to the agent.
- **"generateIdToken as …: IAM Credentials returned 403"**: the assigned account lacks
  `roles/iam.serviceAccountOpenIdTokenCreator` on that target account.
- **"the target hub does not accept workload issuer delegation"**: the target publishes no
  issuer audience. Either test identities are off there, or no workload issuer is configured.
- **"no profile credential applies"**: the profile has no `--issuer-delegation member=<id>`,
  and no `--as` identity was given.
