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
  that person's own live `test_identity.issue` authority. On a systemd test hub running the
  admin tier, a super-admin can also delegate admin test identities (see
  [Admin-tier delegations](#admin-tier-delegations-systemd-test-hubs)).
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
`--run` are flags of the `hub` and `artifact` commands and of the agent commands that run with a
profile (`create`, `start`, `list`, `stop`, `delete`, `look`, `logs`), and apply only with a
profile.

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
reason. `issue` issues member and viewer identities; admin test identities go through
`issue-admin` and an admin delegation (see
[Admin-tier delegations](#admin-tier-delegations-systemd-test-hubs)).

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
- `hub test-identity`, except `delegate`
- `hub projects`: list, `info`, `create` and `delete`, with the project named explicitly
- `artifact publish`, `get`, `versions` and `delete`; `publish` needs `--project <name or id>`
  on the target
- the agent commands `create`, `start`, `list`, `stop`, `delete`, `look` and `logs`, with
  `--project <name or id>` on the target (see [Agents on the target](#6-agents-on-the-target))

They read no local project settings. Every other command, such as `message`, `whoami`,
`resume` and anything that touches local state, is refused while a profile is selected.

A member token used with `--as` is re-issued automatically through the delegation within 5
minutes of its expiry. `scion hub test-identity token --as owner` re-issues it at once. Once
the identity itself has expired, commands are refused with "issue a new one".

## 5. Open a browser session as an identity

`hub test-identity web-code` mints a single-use web-login code for an identity of the run, on
the target, through the profile's issuer delegation:

```bash
ID=$(jq -r .identityId ~/.scion/hub-profiles/t1/run-r1/owner.json)
dir=$(mktemp -d); dir=$(cd "$dir" && pwd -P)
scion hub test-identity web-code "$ID" --out "$dir/code.txt"
```

The code goes **only** to the file named by `--out`, exactly as without a profile: the path must
not exist yet (an existing file, a symlink, or a path under a symlinked directory is refused
before any request is sent), the file is created with mode `0600`, and the code is never
printed. A browser redeems it at `POST /auth/test-identity/redeem` on the target (see
[Browser Sessions as Test Identities](/scion/contributing/test-identity-browser-sessions/)).
`--as` does not apply: the issuer delegation mints the code. The target mints codes only for
member and viewer identities issued through the same delegation, and only where its own test
identity web sessions are available; for any other identity it answers `404`. `hub test-identity
list` shows the ids of the delegation's identities. For an admin test identity, add
`--admin-role hub-admin` or `super-admin` (see
[Admin-tier delegations](#admin-tier-delegations-systemd-test-hubs)).

## 6. Agents on the target

With `--project <name or id>`, the agent commands act on agents of that project on the target,
as the `--as` identity:

```bash
scion create a1 "provision only" --project uat-throwaway --as owner
scion start a2 "do the task" --project uat-throwaway --as owner --no-wait
scion list --project uat-throwaway --as owner
scion look a2 --project uat-throwaway --as owner
scion logs a2 --project uat-throwaway --as owner
scion stop a2 --project uat-throwaway --as owner
scion delete a1 a2 --project uat-throwaway --as owner
```

The project is always named with `--project`; the profile names none, and the current
directory's project is never used. These commands work only through the target hub's API:

- no local project is resolved, no local settings are read, and no hub sync runs;
- no agent directory, worktree, sync state or agent name cache is written or removed;
- `--type` must name a template that exists on the target; a local template is never read or
  uploaded;
- no environment value is gathered from the local environment. A start does not ask for
  missing environment values, and if the target asks anyway the start fails: set the values as
  secrets or environment on the target hub instead. This covers implicit gathering only.
- `--config` and `--task-file` are explicit input: their content is sent to the target as
  given. An inline config can carry environment values and, through `file://` references, the
  contents of local files, so pass only what the target should receive.

They fail closed, before any agent request, where they would need local state or another
credential: `--attach`, `--workspace`, `--upload-template`, `delete --cleanup-only`,
`list --all` and the relationship filters of `list` (`--descendants`, `--ancestors`,
`--lineage`) are refused with a profile. `stop --all` and `delete --stopped` act on the named
project. `look` and `logs` read the terminal output and the agent log through the target's
relay to its broker. `message` is not available with a profile: it names the sender from the
agent's own identity on its own hub.

## 7. Check the gate with `hub status`

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

## 8. Tear down the run

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
- `hub test-identity` `issue`, `issue-admin`, `token`, `list`, `delete`, `teardown` and `web-code`
- `artifact publish`, `get` and `versions` (agent commands with or without a profile)
- `create`, `start`, `list`, `stop`, `delete`, `look` and `logs` (agent commands with or without
  a profile)

`hub status` and the `hub test-identity` commands run only with a target-hub profile whose
endpoint is not the agent's own hub. Without a profile, or with a profile for the agent's own
hub, they are refused. The `artifact` commands and the agent commands keep their usual
behaviour on the agent's own hub without a profile. With a profile selected they act on the
target as the `--as` identity, and a profile for the agent's own hub is refused for them too.
`hub test-identity delegate` (the human consent step), the other `hub` commands and
`hub projects` are not available in agent mode.

## Admin-tier delegations (systemd test hubs)

A target hub that runs the test admin tier (`--test-mode=admin`, or `super-admin`) also accepts
delegations for admin test identities. The admin tier runs only on systemd test hubs. One
delegation holds one scope:

| `--scope` | Issues | Hub level | Lifetime (`--ttl`) | Per-delegation cap (`--admin-cap`) |
|---|---|---|---|---|
| `member` (default) | member and viewer identities | `member` or above | default 8h, at most 7 days | `--member-cap` 1-5 |
| `hub-admin` | test hub-admins | `admin` or above | default 4h, at most 8h | 1-2 (default 2) |
| `super-admin` | test super-admins | `super-admin` | default 1h, at most 2h | 1 |

The lifetime is checked at create and again, from the creation time, at every issuance. The
per-delegation caps are sub-limits: the hub never holds more than four live admin test
identities, of which at most one test super-admin, across every delegation, access token and
session. Over a cap, issuance answers `429` (`admin_delegation_cap_reached`,
`admin_hub_cap_reached` or `super_admin_hub_cap_reached`).

**Allowlist.** The issuer's entry needs `max_tier: admin` in the target's settings file. A hub
started below `--test-mode=admin` with such an entry refuses to start.

```yaml
server:
  hub:
    test_identities:
      workload_issuers:
        - sub: "<T>-admin-issuer unique id"
          max_tier: admin
```

**Create (a live super-admin, interactive sign-in only).**

```bash
scion --hub https://t9.example.com hub test-identity delegate create \
  --workload-sub 123456789012345678902 --scope hub-admin --ttl 2h --admin-cap 1 --label "admin journey"
```

Only a human super-admin's interactive session creates an admin delegation. An access token,
a dev credential, an agent, a workload delegation and every test identity are refused; the test
admin identities are refused by the test-admin policy.

### Which command issues admin test identities

Two commands issue test hub-admins and test super-admins. They are for different callers and never stand in for each other:

| Caller | Command | Credential | Where the token goes |
|---|---|---|---|
| A human operator, on the hub itself | `scion hub test-identity issue --admin-role ... --out <file>` | your own sign-in as a live super-admin (no hub profile, not in agent mode) | the `--out` file only |
| A runner on another hub, for example an agent | `scion hub test-identity issue-admin --admin-role ... --as <alias> --run <label>` | the profile's admin issuer delegation (a hub profile is required; in agent mode only for a profile of another hub) | the run cache file `~/.scion/hub-profiles/<profile>/run-<label>/<alias>.json` only |

The human command uses your session, so the authority is yours at that moment. The runner command acts through a delegation that a super-admin created on the target, so the hub re-checks the delegation and its grantor at every mint. `issue --admin-role` refuses a hub profile and agent mode, and `issue-admin` refuses to run without a target-hub profile. The run cache file is created exclusively with mode `0600`, symlinks are refused, an alias that already exists is refused before any request, and the token is never printed. `teardown --run <label>` removes the file.

### Admin delegations through a profile

**Add the admin delegations to the runner's profile.** An admin delegation pins the target's
admin issuer service account, `<T>-admin-issuer` (see the prerequisites below), so the profile
names that account with `--admin-issuer-sa`. It must differ from `--issuer-sa`, and is required
with an admin delegation. A profile can hold the member delegation and both admin delegations,
each under its own tier name; there is no fallback from one tier to another.

```bash
scion hub profile add t9 \
  --endpoint https://t9.example.com \
  --transport iap \
  --iap-audience 1234567890-def.apps.googleusercontent.com \
  --uat-sa t9-uat@my-project.iam.gserviceaccount.com \
  --issuer-sa t9-issuer@my-project.iam.gserviceaccount.com \
  --admin-issuer-sa t9-admin-issuer@my-project.iam.gserviceaccount.com \
  --issuer-delegation member=<member delegation id>,hub-admin=<hub-admin delegation id>
```

**Issue.** `issue-admin` issues through the profile's admin delegation of `--admin-role`, with
an issuer ID token minted in memory as the admin issuer account, and stores the identity in the
run cache like `issue` does:

```bash
export SCION_HUB_PROFILE=t9 SCION_HUB_RUN=r1
scion hub test-identity issue-admin --admin-role hub-admin --purpose "admin journey" --as admin1
```

It needs a hub profile in every mode: there is no path through your own sign-in. In agent mode
it runs only with a profile for another hub. The token goes only to the run cache (mode
`0600`); the output carries the identity's ID, email, kind, role and expiry and the cache file, never
the token. `--purpose` is required (the hub records it on every admin identity). `--lifetime`
and `--ttl` follow the kind's limits: a test hub-admin lives at most 60 minutes (token at most
30), a test super-admin at most 30 minutes (token at most 15). A delegation issues only its own
scope's kind.

On the wire this is `POST /api/v1/test-identities/admin` with
`Authorization: Bearer <ID token of <T>-admin-issuer, aud = the target's testIssuerAudience>`,
`X-Scion-Issuer-Delegation: <delegation id>`, an optional `X-Scion-Run-Label`, and a body such as
`{"adminRole": "hub-admin", "purpose": "admin journey r1"}`.

**Token re-issue.** `scion hub test-identity token --as admin1` re-issues the cached identity's
token through the admin delegation that issued it, and commands that act `--as admin1`
re-issue it within 5 minutes of its expiry. The target re-issues only for identities the same
delegation issued (any other identity gets `404`), within the kind's token limit and never past
the identity's expiry or its kind's maximum lifetime: re-issue never extends an identity. If
the profile no longer holds that delegation, the token is used until it expires. Re-issue is
bounded by the delegation's test identity rate limit only (there is no separate admin limit).
Each re-issue adds a live token, and earlier tokens stay valid until they expire, but no token
outlives the identity's kind lifetime (at most an hour).

**Web-login code.** `scion hub test-identity web-code <id> --admin-role hub-admin --out
<file>` mints a code through the admin delegation, under the same rules as the member flow
(the code goes only to `--out`). The target mints it only when it runs with
`server.hub.test_identities.admin_web_sessions` set to its own hub id, at the kind's
`--test-mode` level, and only for an identity the same delegation issued; otherwise it
answers `404`. The browser session is capped at the identity's expiry and the kind's maximum
lifetime.

**Grantor and delegation re-checked at every mint.** Issuance, token re-issue and web-login
codes each re-check, inside their own transaction, that the delegation is live and that the
grantor is still an active human super-admin holding the scope's capability. If the grantor is
demoted or suspended, or the delegation is revoked, the next mint gets `403` and nothing is
created.

**Not through a delegation.** An admin delegation does not list, delete or purge identities, does
not run the issuer cleanup or the provenance route, and does not issue member identities (`403`). A member delegation reaches no admin identity. So
`delete --as <admin alias>` is refused by the CLI, and `teardown` reports an admin alias as
`left-to-expire` and removes its cache file: the identity ends at its expiry (at most an hour),
or earlier through `POST /api/v1/test-identities/admin/revoke-all` by a human super-admin, a
purge (`delete --purge`) by the grantor (on its own sign-in or access token) or a human
super-admin, or a restart without the admin level.

**Audit.** The identity records the delegation id. The `test_admin_issue` audit record names the
delegation, the grantor, the issuer account's subject, the token's issue time and the run
label, and the `[scion-test-admin-audit]` line carries `issuer_delegation_id` and
`workload_sub`. A token re-issue writes a `test_identity_token_issue` record with the same
delegation part and a `test_admin_token_issue` line; a web-login code writes a
`test_identity_web_code_issue` record and a `test_admin_web_code_issue` line, each with the
delegation id. No record or line holds a token or code.

### Operator prerequisites for the admin tier

Admin delegations use their own issuer service account per target, `<T>-admin-issuer`. It is
separate from `<T>-issuer` (the member tier) so that the principal able to mint admin issuer
tokens for a target is distinct from the one that mints member issuer tokens.

- **One impersonator per target.** Grant `roles/iam.serviceAccountOpenIdTokenCreator` on
  `<T>-admin-issuer` to exactly one principal dedicated to that target: for runners on the
  community Kubernetes broker, a per-target Kubernetes service account bound through Workload
  Identity (`roles/iam.workloadIdentityUser`), in its own namespace. Do not grant it to the
  community hub's runtime service account, nor to any service account shared with member-tier
  runners or with other targets. Scion does not configure or check these grants.
- **Narrow assignment.** Whoever can run a workload as that per-target principal can mint the
  target's admin issuer tokens while an admin delegation is live. Keep the namespace and its
  service account restricted to the admin runners for that target.
- **Allowlist only `<T>-admin-issuer` with `max_tier: admin`**; keep `<T>-issuer` at
  `max_tier: member`.
- **Data access logs.** Turn on IAM Credentials data access logs in the project of
  `<T>-admin-issuer`: they record which principal minted each token.

**Operator check (do this after setting up each target, and after IAM changes).** Confirm that
the community hub's runtime service account cannot mint `<T>-admin-issuer` tokens:

1. The IAM policy of `<T>-admin-issuer` (`gcloud iam service-accounts get-iam-policy`) lists no
   binding for the community hub's runtime service account, and the per-target principal is
   the only holder of `roles/iam.serviceAccountOpenIdTokenCreator`.
2. A `generateIdToken` call for `<T>-admin-issuer` made as the community hub's runtime service
   account fails with a permission error.
3. The same call made as the per-target principal succeeds.

Record the result with the target's setup notes. Scion has no test for this: it depends only on
the IAM configuration.

**Revocation, fastest first:** revoke the delegation on the target; remove the IAM grant of the
per-target principal; `POST /api/v1/test-identities/admin/revoke-all` for identities already
issued; restart the target without the admin level.

## Troubleshooting

- **"SCION_METADATA_MODE is not "assign""**: the runner has no assigned service account.
  Assign one to the agent.
- **"generateIdToken as …: IAM Credentials returned 403"**: the assigned account lacks
  `roles/iam.serviceAccountOpenIdTokenCreator` on that target account.
- **"the target hub does not accept workload issuer delegation"**: the target publishes no
  issuer audience. Either test identities are off there, or no workload issuer is configured.
- **"no profile credential applies"**: the profile has no `--issuer-delegation member=<id>`,
  and no `--as` identity was given.
- **`web-code` answers that web sessions are not available**: the target cannot mint web-login
  codes. A target in proxy auth mode (behind IAP) mints them only when it lists at least one
  driver principal (`server.auth.proxy.driver_principals`); with none configured it answers
  this for every code (`details.reason: test_identity_web_sessions_unavailable`,
  `details.cause: proxy_auth_without_driver_principals`), and test identities still work for
  API calls there. The redeem on such a target also needs the driver's proxy assertion (see
  [IAP-fronted hubs](/scion/contributing/test-identity-browser-sessions/#iap-fronted-hubs)).
  The other causes are a target without the test infrastructure marker or with test identities
  off.
