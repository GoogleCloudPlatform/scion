# modules/hub-test-runner: test-runner identities for a GKE test hub

Optional. Lets an agent on one hub run user-surface tests (CLI first) against
a separate test hub deployed with `configurations/hub-gke`, without carrying
its own hub identity to the test hub. `configurations/hub-gke` calls it with
`count = var.test_runner.enabled ? 1 : 0`; it is off by default.

## What it creates

With one impersonator, exactly five objects:

| Object | Scope |
|---|---|
| service account `<hub>-uat` | no project roles |
| service account `<hub>-issuer` | no project roles, no IAP access |
| `roles/iam.serviceAccountOpenIdTokenCreator` on `<hub>-uat` | per impersonator |
| `roles/iam.serviceAccountOpenIdTokenCreator` on `<hub>-issuer` | per impersonator |
| `roles/iap.httpsResourceAccessor` for `<hub>-uat` | this hub's backend service only |

Each further impersonator adds one token-creator grant on each account. No
grant is project-wide, and all IAM is additive `*_iam_member`.

When `alert_notification_channels` is non-empty, it also creates a log-based
metric counting `GenerateIdToken` calls on either account by a principal not
in `impersonators`, and an alert policy on that metric. The exact log filter
is in `main.tf` and must be confirmed against a real log entry before relying
on it.

`<hub>-issuer` is only created here. Configuring the test hub to trust it is
a separate change.

## Inputs

| Variable | Meaning |
|---|---|
| `project_id` | the test hub's project |
| `hub_name` | the test hub's name, at most 23 characters so `<hub>-issuer` fits the 30-character service-account ID limit |
| `impersonators` | at least one `serviceAccount:<email>` member; `group:`, `user:` and other member types are rejected |
| `backend_service_name` | the test hub's IAP backend service (`hub-lb`'s `backend_service.name`) |
| `alert_notification_channels` | optional notification channel IDs; empty creates no alert objects |

Outputs: `uat_email`, `uat_unique_id`, `issuer_email`, `issuer_unique_id`,
plus `iam_grants` and `alerts_enabled` for operators and tests.

## Audit-log precondition

Every IAM member resource has a `lifecycle.precondition`. It reads the
project's IAM policy (`data "google_project_iam_policy"`) and fails unless an
audit config for `iamcredentials.googleapis.com` or `allServices` enables
`DATA_READ` without exempting any impersonator. Without it, ID tokens minted
for these accounts would not be logged.

To satisfy it, set `iamcredentials_data_access_logs = true` in
`configurations/shared-infra` and apply that first. The audit config is
project-level, so it is owned once there, not per hub. Reading the policy
needs `resourcemanager.projects.getIamPolicy`, which
`roles/resourcemanager.projectIamAdmin` (in the operator roles listed in
`../../README.md`) includes.

## Operator notes

**Operator responsibilities.** Each impersonating service account must be
dedicated to this purpose, must not be a project default account (such as the
Compute Engine default account), and must be assigned only to the testing
agent. The module does not enforce this. In particular, it does not enforce
service-account uniqueness: it cannot tell whether the account you list is
shared with other workloads or assigned to other agents. Creating,
registering and assigning the impersonating account is an operator task
outside this module.

**Attribution.** Audit logs on the test-hub side attribute each
`GenerateIdToken` call to the impersonating service account, not to an
individual agent. Attributing a call to a specific agent needs the
originating hub's own audit records, kept over the same retention window.

**The precondition only applies at provision time.** It is checked when
Terraform plans these resources. If audit logging is later turned off
outside Terraform, nothing here notices until the next `terraform plan`
fails the precondition. Treat Terraform as the source of truth for the audit
config and run plans regularly.

## Tests

```bash
terraform init -backend=false
terraform validate
terraform test
```

The tests run with a mock provider and need no credentials. They cover the
five objects and their exact roles and scopes, that `<hub>-issuer` has no IAP
grant, the precondition (fails without the audit config, with `ADMIN_READ`
only, for another service, or with an exempted impersonator; passes with
`iamcredentials.googleapis.com` or `allServices` `DATA_READ`), the alert
objects, the outputs and the input validations. They cannot prove that GCP
accepts the grants or that the alert filter matches real log entries.
