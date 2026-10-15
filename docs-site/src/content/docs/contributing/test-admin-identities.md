---
title: Test Identities with Admin Roles
description: Operator runbook for issuing, inspecting and revoking test identities with hub-admin or super-admin roles on test-designated hubs.
---

This runbook is for operators of **test-designated hubs** who run admin-level automated tests. On such a hub, a super-admin can issue a short-lived **test identity with an admin role**: a synthetic user that holds the real `hub-admin` or `super-admin` role until it expires. The commands are:

- `scion hub test-identity issue --admin-role ...`
- `scion hub test-identity residue <id>`
- `scion hub test-identity revoke-all`

The routes behind them are described under "Test admin identities" in the [API reference](/scion/reference/api/). The commands are listed under `scion hub test-identity` in the [CLI reference](/scion/reference/cli/). Member and viewer test identities are covered in [Browser Sessions as Test Identities](/scion/contributing/test-identity-browser-sessions/).

All three commands are for human operators only:

- They are not available in agent mode.
- They do not run with a hub profile (`--hub-profile` or `SCION_HUB_PROFILE`).
- They use your own hub sign-in on the hub itself.

## Before you start: the hub

A hub issues test identities with admin roles only when it was started for that. There is no runtime switch.

1. **An operator writes the test infrastructure marker offline, on the hub host.** No agent ever writes it. The command opens the hub database directly and does not need a running server:

   ```bash
   scion server test-infra mark --hub-id <hub-id> \
     --i-understand-this-hub-can-mint-super-admins
   ```

   There is no hub route for the marker, and `scion server` is not available in agent mode.

2. **Set `server.hub.class: test`** in the settings file or environment.

3. **Start the hub with `--test-mode=admin:<hub-id>`.** This allows hub-admin test identities. Use `--test-mode=super-admin:<hub-id>` to also allow super-admin test identities.

The admin levels have further requirements: hub-local signing keys, no federation, not a workstation hub, and no remote brokers. They are listed under "Test mode (operators)" in the [API reference](/scion/reference/api/). If any requirement is missing, the hub refuses to start and names it.

## Issue one identity

The issuer must be a human who is a live super-admin at that moment. An access token is accepted only if it has the hub boundary, exactly the kind's scope (`test_identity:issue_admin` or `test_identity:issue_super_admin`), and an age of at most 8 hours.

```bash
scion hub test-identity issue --admin-role hub-admin \
  --purpose "admin settings journey" --out ./hub-admin.token
```

- `--admin-role` is `hub-admin` or `super-admin`. A super-admin identity needs `--test-mode=super-admin`; otherwise the command fails with "test identities with admin roles are not enabled on this hub".
- `--purpose` is required: 8 to 200 characters. It is recorded on the identity and in the audit.
- `--out` is required. The token is written **only** to this file:
  - The file is created exclusively (`O_CREATE|O_EXCL`) with mode `0600`.
  - An existing file, a symlink (even a dangling one) and a path under a symlinked directory are refused before any request is sent.
  - The token never appears on standard output, standard error or in an error message. Only the identity ID, admin role, kind, email, issuance ID, expiry, purpose and file path are printed. `--json` prints the same facts, never the token.
- `--role` does not apply.
- `--ttl` and `--lifetime` are optional:

  | Admin role | Token lifetime (`--ttl`) | Identity lifetime (`--lifetime`) |
  |---|---|---|
  | `hub-admin` | default 15m, at most 30m | default and at most 1h |
  | `super-admin` | default 10m, at most 15m | default and at most 30m |

- The token is never re-issued. If it is lost, remove the identity with `scion hub test-identity delete <id> --purge` and issue a new one.
- A plain `scion hub test-identity delete <id>` does not remove an identity with an admin role: the hub answers it as not found, and the CLI's notice points to `--purge` or `revoke-all`.

The hub also limits issuance:

- one live identity with an admin role per issuer;
- two per hub, of which at most one is a super-admin;
- per issuer, six hub-admin and two super-admin issuances per hour.

## Inspect what an identity left: residue

```bash
scion hub test-identity residue <id>
scion hub test-identity residue <id> --json
```

The report covers **one identity per call**. The identity may be live or already purged. The report lists:

- the grants stamped with its issuance: role bindings, group memberships, custom roles, invites and join tokens;
- the resources stamped with it: agents, schedules, groups and projects.

A purge (`scion hub test-identity delete <id> --purge`) removes both. The report also lists the changes the identity made to rows **without** its stamp, for example a user it suspended. These are reported and never reverted: review them by hand.

The report holds IDs and audit facts only, never a token. It is only as complete as the mutation audit.

Only a human super-admin signed in interactively (`scion hub auth login`) may read it. Access tokens, test identities and other roles get the error "this command requires a human super-admin signed in interactively".

## Emergency stop: revoke-all

`revoke-all` revokes **every** test identity with an admin role on the hub:

- Each identity is deleted. If it owns agents or is a project's last owner, it is suspended and its role bindings are removed instead.
- Its tokens stop working at once.

Only a human super-admin signed in interactively may run it. Other callers get the same named error as for `residue`.

Follow these steps:

1. **Dry run first.**

   ```bash
   scion hub test-identity revoke-all --reason "INC-123 stop test admins" --dry-run
   ```

   `--dry-run` changes nothing. It reads only the admin health summary and prints:
   - whether the admin tier and the super-admin kind are enabled;
   - how many identities with admin roles are live;
   - your reason.

   It never calls revoke-all. It does **not** check that you are a super-admin, because reading the health summary needs only `hub.health.read`. The real call does check.

2. **Revoke, with a reason.**

   ```bash
   scion hub test-identity revoke-all --reason "INC-123 stop test admins"
   ```

   `--reason` is required. It is printed with the result as **your own record only**: the hub does not receive or store the reason. The hub's audit records who revoked each identity and when. Keep the reason in your incident or runbook log. The output lists every identity and its outcome (`deleted` or `suspended`). `--json` prints the same facts.

3. **Clean up what is left.** For each suspended identity, check `scion hub test-identity residue <id>`, then run `scion hub test-identity delete <id> --purge`.

4. **Restart the hub without `--test-mode=admin` or `super-admin`.** A token issued earlier then stops working even if it was missed.
