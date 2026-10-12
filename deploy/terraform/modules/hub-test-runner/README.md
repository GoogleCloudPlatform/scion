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
in `impersonators`, and an alert policy on that metric. The metric matches
the account on the log entry's `service_account` resource
(`resource.labels.email_id`); the exact filter is in `main.tf`. The alert
condition watches the metric on `resource.type="global"` and auto-closes
after 30 minutes without matches.

**First live enablement.** The mock-provider tests cannot check any of this
against GCP. The first time the module is enabled on a real project, confirm
against one real `GenerateIdToken` entry that the call is logged under
`DATA_READ` (the `data_access` log) and that the metric's filter matches it,
and, after the metric's first data point, that its descriptor reports
`resource.type="global"`.

`<hub>-issuer` is only created here. Configuring the test hub to trust it is
a separate change.

## Inputs

| Variable | Meaning |
|---|---|
| `project_id` | the test hub's project |
| `hub_name` | the test hub's name, at most 23 characters so `<hub>-issuer` fits the 30-character service-account ID limit |
| `impersonators` | at least one `serviceAccount:<email>` member, with the email in lowercase; `group:`, `user:` and other member types are rejected |
| `backend_service_name` | the test hub's IAP backend service (`hub-lb`'s `backend_service.name`) |
| `alert_notification_channels` | optional notification channel IDs; empty creates no alert objects |

Outputs: `uat_email`, `uat_unique_id`, `issuer_email`, `issuer_unique_id`,
plus `iam_grants` and `alerts_enabled` for operators and tests.

`iam_grants` lists every grant as `{ account, role, member, resource }`,
read from the grant resources:

| Field | Meaning |
|---|---|
| `account` | the test-runner account the grant concerns (`uat` or `issuer`; `unknown` if it matches neither). For a token-creator grant it is the target account, the one tokens can be minted for, derived from the grant's `service_account_id`. For the IAP grant it is the account IAP admits, derived from the grant's `member`. |
| `role` | the granted role |
| `member` | who receives the role: an impersonator for token-creator grants, the `-uat` account itself for the IAP grant |
| `resource` | the grant's scope: `service_account` (the account in `account`) or `backend_service:<name>` |

`role` and `resource` are known at plan time. `account`, and the IAP grant's
`member`, depend on the accounts' computed name and email, so they are known
once the accounts exist.

## Audit-log precondition

Every IAM member resource has a `lifecycle.precondition`. It reads the
project's IAM policy (`data "google_project_iam_policy"`). GCP combines audit
configs across services, unioning their log types and their exempted
members, so the precondition checks the `iamcredentials.googleapis.com` and
`allServices` configs together and requires both of:

- `DATA_READ` is enabled in at least one of them;
- no impersonator appears in `exemptedMembers` of any `DATA_READ` log config
  in either of them.

Without that, ID tokens minted for these accounts would not all be logged.
Members are compared case-insensitively. Only literal members are compared:
an exemption through a `group:` that contains an impersonator is not
detected.

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
only, for another service, with an exempted impersonator in either config,
including when only one of the two configs exempts it, or with an exemption
that differs only in case; passes with `iamcredentials.googleapis.com` or
`allServices` `DATA_READ`, or with `DATA_READ` in one config and no
impersonator exempted in either), the alert objects and filter, the outputs
and the input validations (including a mixed-case email).

The IAP-only-on-`-uat` check is carried by the `outputs_and_grant_targets`
apply run (against the mock provider): it asserts the IAP grant's `member` is
the `-uat` email, and, through `iam_grants` (whose `account` is derived from
each grant resource), that the only IAP grant is for `uat` and each
token-creator grant targets its own account. The plan run checks the IAP
grant's role and backend service. `configurations/hub-gke`'s
`test_runner_outputs_pass_through` run makes the same `iam_grants` check
through the root.

They cannot prove that GCP accepts the grants, that `GenerateIdToken` is
logged under `DATA_READ`, or that the alert filter matches real log entries
(see "First live enablement").
