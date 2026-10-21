# GitHub setup (outline)

> **TODO(P5):** this is an outline. P5 fills every section with exact steps and
> UI paths. The roles each step needs are already final in
> [permissions-matrix.md](permissions-matrix.md#1-github-steps).

Goal: take a repo admin (fork) or an org owner (upstream) from nothing to a
working App installation, with routing ready but switched off.

**Precondition (required, not optional):** set fork pull request workflows to
"Require approval for **all** external contributors" (not only first-time
contributors) on every target repo or its org before routing any job
([matrix G8](permissions-matrix.md#1-github-steps); why:
[architecture.md](architecture.md#deploy-preconditions)).

## 1. Decide the target and scope

- TODO(P5): fork, upstream or both; `GITHUB_APP_INSTALLATION_SCOPE` `repo` vs
  `org` (recommendation and trade-offs:
  [matrix 4.2](permissions-matrix.md#42-repo-vs-org-installation-scope-for-upstream)).
- TODO(P5): who owns the App
  ([matrix 4.1](permissions-matrix.md#41-who-should-own-the-github-app-for-upstream)).

## 2. Create the GitHub App (manifest flow)

- TODO(P5): `scripts/github-app-manifest.json` with the permission set for the
  chosen scope, webhook event `workflow_job`, and installability (only this
  account vs any account).
- TODO(P5): manifest-flow steps; record the App ID in `GITHUB_APP_ID`.
- TODO(P5): generate the private key and store it in Secret Manager straight
  from the downloaded file (`scripts/put-secret.sh`); delete the local copy.

## 3. Configure the webhook

- TODO(P5): webhook URL = `MEDIATOR_URL` + `/webhook` (after `deploy-mediator.sh`);
  generate the webhook secret locally, store it in Secret Manager, and paste it
  into the App settings; check the recent deliveries page.

## 4. Install the App

- TODO(P5): fork: install on `ptone/scion` only ("Only select repositories").
- TODO(P5): upstream, repo scope: org owner installs on `scion` only; repo-level
  runner policy allowed for `scion`.
- TODO(P5): upstream, org scope: org owner installs; runner group with *Allow
  public repositories* limited to `scion`.

## 5. Actions settings

- TODO(P5): fork PR approval = "Require approval for all external contributors".
- TODO(P5): confirm no org or enterprise policy blocks repo-level self-hosted
  runners.

## 6. Routing variable (rollout, done last)

- TODO(P5): `scripts/set-routing-variable.sh` sets `ROUTING_VARIABLE_NAME` to
  `RUNNER_LABEL`; the `runs-on` snippet from `workflows/`; the PoC workflow first.

## 7. Verify

- TODO(P5): PoC run checklist (latency, peak memory, runner auto-removal).
