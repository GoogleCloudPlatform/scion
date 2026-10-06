---
title: Deploy via Helm (GKE)
description: Install the Scion Hub on Google Kubernetes Engine with the scion-hub Helm chart in deploy/helm/scion-hub.
---

The `scion-hub` Helm chart in
[`deploy/helm/scion-hub`](https://github.com/GoogleCloudPlatform/scion/tree/main/deploy/helm/scion-hub)
deploys the Scion Hub to Google Kubernetes Engine (GKE). Each hub pod runs the
Hub API, the web UI, and an in-process Runtime Broker that creates agent pods in
the cluster.

:::caution[Not yet validated on a live cluster]
The chart's static checks pass (`helm lint`, `helm template` with
`kubeconform -strict`, and the render-time assertions described below). The live
checks in
[`deploy/helm/scion-hub/VALIDATION.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/deploy/helm/scion-hub/VALIDATION.md)
have not been run yet. If you install the chart against real infrastructure, run
those checks and record the results.
:::

## What the chart renders, and what it does not

The chart renders:

- A `Deployment` running the hub container (and, when Cloud SQL is enabled, the
  Cloud SQL Auth Proxy as a sidecar).
- A `ClusterIP` `Service` on port 80 (`service.port`) targeting the hub's web port.
- A `ServiceAccount`, optionally annotated for Workload Identity.
- A namespaced `Role` and `RoleBinding` for managing agent pods, or a
  `ClusterRole` and `ClusterRoleBinding` when `runtime.listAllNamespaces` is `true`.
- A `Secret` holding the hub's `settings.yaml`, mounted read-only (unless
  `config.existingSecret` is set), and a `ConfigMap` with the hub's environment.
- A `Secret` holding the session secret, only when you set `auth.sessionSecret`.

The chart does **not** create an Ingress, Gateway, load balancer, IAP
configuration, Cloud SQL instance, GCS bucket, Secret Manager permissions, or
any IAM binding. Provision those separately, for example with
[Terraform](/scion/hosted/ha/terraform/) or the manual steps in
[Deploy on GCP](/scion/hosted/ha/setup-gcp/).

The chart defaults to `replicaCount: 1`. With more than one replica, the web
terminal, exec, log tailing, and port forwarding fail for roughly (N-1)/N of
requests: the agent control channel, presence, and port tunnels are held in the
memory of the pod that owns them, and load-balancer session affinity does not fix
that.

---

## 1. Prerequisites

1. A GKE cluster (Standard or Autopilot) with `linux/amd64` nodes available.
2. `kubectl`, and `helm` v3, with access to the cluster.
3. A clone of the Scion repository. The chart is installed from the local
   `deploy/helm/scion-hub` directory; no Helm chart repository is published.
4. A hub image built from the `hub-gke` target (next section), in a registry the
   cluster can pull from.
5. For an HA hosted deployment: a Cloud SQL for PostgreSQL instance, a GCS
   bucket, and a Google service account bound to the chart's Kubernetes
   ServiceAccount through Workload Identity.

---

## 2. Build the hub image

The chart always sets `runAsNonRoot: true`, and no value overrides it. The uid
and gid default to 1000 (`hub.securityContext.runAsUser` and
`hub.securityContext.runAsGroup` in `values.yaml`), which is what the `hub-gke`
image expects. A value of 0 for either is refused. The chart needs the non-root
`hub-gke` stage of the repository's root `Dockerfile`, which also embeds the web
UI.

:::danger[Do not use the published scion-hub image]
The published `scion-hub` image (built from `image-build/hub/Dockerfile`) runs
as root and is built without the web UI. Under this chart it fails pod admission
because of `runAsNonRoot`.
:::

`image-build/cloudbuild-hub-gke.yaml` builds the `hub-gke` stage for
`linux/amd64` only and pushes a single tag, the git short SHA, as
`<registry>/scion-hub-gke:<short-sha>`. No moving tag such as `latest` is pushed.
From the repository root:

```bash
gcloud builds submit \
  --config=image-build/cloudbuild-hub-gke.yaml \
  --ignore-file=image-build/gcloudignore-hub-gke \
  --substitutions=_REGISTRY=<registry>,_SHORT_SHA=$(git rev-parse --short HEAD) \
  .
```

`--ignore-file=image-build/gcloudignore-hub-gke` is required. The reasons are in
[`image-build/README.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/image-build/README.md#gke-hub-image-cloudbuild-hub-gkeyaml).

Pin the image by digest with `image.digest`. `image.tag` is accepted, but a tag
can be repointed and a digest cannot. The two are mutually exclusive. If neither
is set, the tag defaults to the chart's `appVersion` (`0.1.0`), which is a
placeholder that no build pushes. Because the image is amd64 only, set
`hub.nodeSelector` to `{kubernetes.io/arch: amd64}` on clusters that also have
arm64 nodes.

---

## 3. Required values

The chart has no defaults for these values. Leaving one out, or setting it
wrongly, makes `helm template`/`helm install` fail with a message that names the
value.

| Value | Requirement |
| :--- | :--- |
| `hub.hubId` | The hub's stable identity. Blob-storage prefixes and secret scopes are keyed by it, so keep it the same on every upgrade. The chart never generates one. |
| `hub.baseUrl` | The external URL of the hub, for example `https://hub.example.com`. It must start with `https://`: the session cookie's `Secure` attribute is derived from that prefix. |
| `auth.existingSecret` or `auth.sessionSecret` | Exactly one. The session secret is both the cookie encryption key and the hub's JWT signing key, so every replica must see the same value. `auth.existingSecret` names a Secret you manage; the key defaults to `SCION_SERVER_SESSION_SECRET` and can be changed with `auth.existingSecretKey`. |
| `image.repository` | Your `scion-hub-gke` repository (see above). |
| An agent image registry | `agents.imageRegistry`, for example `us-docker.pkg.dev/my-project/scion`. The in-process Runtime Broker does not start without one. The chart also accepts `profiles.default.image_registry` set through `config.extra` (the chart fixes `active_profile` to `default`, and `config.extra` cannot override it), or `SCION_IMAGE_REGISTRY` / `SCION_MAINTENANCE_IMAGE_REGISTRY` set through `hub.extraEnv`. Under `config.existingSecret` this value is not checked, and your settings file must set it. |
| `auth.proxy.iap.audience` | Required while `auth.mode` is `proxy`, which is the default. See [Authentication](#4-authentication). |

Create the session secret before installing:

```bash
kubectl create namespace scion-system
kubectl create secret generic scion-hub-session \
  --namespace scion-system \
  --from-literal=SCION_SERVER_SESSION_SECRET="$(openssl rand -base64 48)"
```

### Example: single pod on SQLite

This is the smallest set of values that renders. It keeps the default
`database.driver: sqlite` and `storage.provider: local`, so the hub database and
blobs are stored in an `emptyDir` in the pod and **are lost whenever the pod is
replaced**. Use it to try the chart out, not to run a deployment.

With the default `auth.mode: proxy` and a placeholder audience, the hub refuses
every request until IAP is in front of it. To try the chart out without IAP, use
`auth.mode: oauth` with a web OAuth client instead (see
[Authentication](#4-authentication)), and remove `auth.proxy`.

```yaml
# values-minimal.yaml. Replace every <...> placeholder.
image:
  repository: us-docker.pkg.dev/<project>/<repo>/scion-hub-gke
  digest: sha256:<digest-from-your-build>

hub:
  hubId: my-hub
  baseUrl: https://hub.example.com
  nodeSelector:
    kubernetes.io/arch: amd64

agents:
  imageRegistry: us-docker.pkg.dev/<project>/<repo>

auth:
  existingSecret: scion-hub-session
  proxy:
    iap:
      # Placeholder audience for a first install; see "IAP audience" below.
      audience: /projects/000000000000/global/backendServices/0
```

---

## 4. Authentication

`auth.mode` selects how users sign in:

- `proxy` (default): Google IAP in front of the hub authenticates the user, and
  the hub verifies the signed IAP header. `auth.proxy.provider` accepts only
  `iap`.
- `oauth`: the hub runs its own OAuth login. Set at least one complete web
  client under `auth.oauth.web.google` or `auth.oauth.web.github` (`clientId`
  and `clientSecret`). `auth.proxy.iap.audience` must not be set in this mode.

See [Proxy Auth (Google IAP)](/scion/hosted/ha/auth-proxy-iap/) for the IAP side
of the setup.

### IAP audience

`auth.proxy.iap.audience` is the IAP resource path. It is not an OAuth client ID.
The chart accepts the same two forms the hub accepts:

| Front end | Audience |
| :--- | :--- |
| GKE load balancer | `/projects/<project-number>/global/backendServices/<backend-service-id>` |
| Cloud Run | `/projects/<project-number>/locations/<region>/services/<service-name>` |

On GKE, the backend-service ID exists only after your Ingress or Gateway has
reconciled. For the first install, use a placeholder with an all-zero
backend-service ID or project number, such as
`/projects/000000000000/global/backendServices/0`. The hub starts, and on an HA
shape (below) it logs at startup:

```
WARNING: IAP audience "<value>" looks like a bootstrap placeholder. IAP token validation will FAIL on real requests until a valid backend-service audience is configured. ...
```

Every request is refused until the real ID is set. Once the load balancer
exists, set the real audience and run `helm upgrade`. The change alters the
`checksum/settings` pod annotation, so the pods restart with the new value.

### Transport authentication

`auth.transport` controls how agents get through IAP to reach the hub. It is
rendered as `server.auth.transport` in `settings.yaml`.

| Value | Meaning |
| :--- | :--- |
| `auth.transport.mode` | `""` (not rendered), `none`, `iap`, or `cloudrun_invoker`. |
| `auth.transport.oidcAudience` | The audience of the OIDC tokens the hub mints for agents. For `iap` this is the IAP OAuth **client ID**, a different value from `auth.proxy.iap.audience`. Required when the mode is `iap`. |
| `auth.transport.platformAuthSa` | The email of the service account the hub impersonates to mint those tokens. Required when the mode is `iap` or `cloudrun_invoker`. |

Setting `oidcAudience` or `platformAuthSa` while the mode is `""` or `none` is
refused, because the values would have no effect.

---

## 5. HA hosted values

The hub treats a deployment as HA when `database.driver` is `postgres`, or when
`storage.provider` is `gcs` and `auth.mode` is `proxy`. On an HA deployment the
hub runs its hosted HA preflight before serving anything. The chart checks the
same conditions at render time, so a configuration the preflight would refuse
also fails `helm template`. Under `auth.mode: proxy`, an HA deployment needs
`auth.transport.mode: iap` together with `oidcAudience` and `platformAuthSa`.

### Database: Cloud SQL through the proxy sidecar

The chart reaches Postgres only through the Cloud SQL Auth Proxy, so
`database.driver: postgres` requires `cloudsql.enabled: true` and
`cloudsql.instanceConnectionName` (`project:region:instance`). The hub connects
to the proxy on `127.0.0.1`. The chart builds the connection URL from the values
below. There is no value for setting it directly.

- `database.auth` is required, and there is no default:
  - `iam`: the proxy authenticates as the pod's Google service account, and the
    URL contains no password. `database.user` defaults to
    `serviceAccount.gcpServiceAccount` with the `.gserviceaccount.com` suffix
    removed. That is the form in which Cloud SQL registers IAM users.
  - `password`: requires `database.user` and `database.password`. The password is
    written only into the settings Secret.
- `database.name` is required.
- `storage.provider: gcs` with a `storage.bucket` is required under Postgres.
- The proxy runs as a native sidecar (`cloudsql.nativeSidecar: true`), which
  needs Kubernetes 1.29 or later. On older clusters, set it to `false`. The proxy
  then starts unordered alongside the hub, so the hub can crash-loop for the
  first minute of each rollout until the proxy is ready. This recovers on its own.
- `cloudsql.privateIp: true` connects over the instance's private IP.

The chart grants no IAM. The Google service account named in
`serviceAccount.gcpServiceAccount` needs the Cloud SQL, GCS, and (with `gcpsm`)
Secret Manager roles your deployment uses. Granting them is the operator's job.

### Secrets backend

`secrets.backend` sets where the hub stores user and agent secrets:

- `local` (default): in the hub database.
- `gcpsm`: in Google Cloud Secret Manager. Requires `secrets.gcpsm.projectId`.
  `secrets.gcpsm.replicationLocations` optionally switches to user-managed
  regional replication. The hub authenticates with Application Default
  Credentials, which on GKE is the Workload Identity service account.

### Admins

`hub.adminEmails` lists email addresses that are given the admin role when they
sign in.

### Example: HA hosted values

```yaml
# values-ha.yaml. Replace every <...> placeholder.
image:
  repository: us-docker.pkg.dev/<project>/<repo>/scion-hub-gke
  digest: sha256:<digest-from-your-build>

hub:
  hubId: my-hub
  name: My Scion Hub
  baseUrl: https://hub.example.com
  adminEmails:
    - alice@example.com
  nodeSelector:
    kubernetes.io/arch: amd64

serviceAccount:
  gcpServiceAccount: scion-hub@<project>.iam.gserviceaccount.com

agents:
  imageRegistry: us-docker.pkg.dev/<project>/<repo>

database:
  driver: postgres
  auth: iam
  name: scion

cloudsql:
  enabled: true
  instanceConnectionName: <project>:<region>:<instance>

storage:
  provider: gcs
  bucket: <bucket-name>

secrets:
  backend: gcpsm
  gcpsm:
    projectId: <project>

auth:
  mode: proxy
  existingSecret: scion-hub-session
  proxy:
    provider: iap
    iap:
      audience: /projects/<project-number>/global/backendServices/<backend-service-id>
  transport:
    mode: iap
    oidcAudience: <iap-oauth-client-id>.apps.googleusercontent.com
    platformAuthSa: scion-hub@<project>.iam.gserviceaccount.com
```

To use password authentication instead, override the `database` block:

```yaml
# values-db-password.yaml. Layer over values-ha.yaml.
database:
  auth: password
  user: scion
  password: <database-password>
```

### Example: refused render

This example is **intentionally refused**. It is a standalone values file, not
an overlay: a copy of `values-ha.yaml` with `auth.transport` removed. Pass it on
its own (`-f values-ha-no-transport.yaml`, without `-f values-ha.yaml`); layered
over `values-ha.yaml`, the `transport` block from that file still applies and
the render succeeds. Only the `auth` block is shown below, because it is the
only part that differs. The render stops before any manifest is produced:

```yaml
# values-ha-no-transport.yaml. Expected to FAIL. A standalone file, not an overlay:
# copy every other block from values-ha.yaml unchanged. Only the auth block differs.
auth:
  mode: proxy
  existingSecret: scion-hub-session
  proxy:
    provider: iap
    iap:
      audience: /projects/<project-number>/global/backendServices/<backend-service-id>
```

`helm template` exits non-zero with an error that begins:

```
Error: execution error at (scion-hub/templates/secret-settings.yaml:...): This release cannot start the deployment these values describe. database.driver is postgres (...) ... Under auth.mode proxy that preflight refuses this settings.yaml: server.auth.transport.mode is "", and must be iap - set auth.transport.mode: iap; ...
```

---

## 6. Exposing the hub

The chart's `Service` is always `ClusterIP`. The chart targets container-native
load balancing, so an Ingress, Gateway, or a Service you manage outside the chart
sends traffic to the hub pods. `service.annotations` is passed through to the
chart's Service, for example for a NEG annotation.

:::note[Selector-label contract]
A Service owned outside the chart (for example, a Terraform-managed NEG Service)
must select hub pods by exactly these two labels:

```
app.kubernetes.io/name: scion-hub        # or nameOverride, if set
app.kubernetes.io/instance: <release-name>
```

These are the chart's selector labels, and the chart's tests pin them. Labels in
`hub.podLabels` cannot override them.
:::

The readiness and startup probes target `/readyz` on the hub's web port
(`hub.webPort`, default 8080). The path cannot be changed. The liveness probe is
off by default. When `probes.liveness.enabled` is `true`, it is a TCP check on
the web port. The startup budget (`periodSeconds` × `failureThreshold`) must be
at least 300 seconds to allow for first-boot migrations.

---

## 7. Install and upgrade

From the repository root:

```bash
# Render without touching the cluster. This runs the schema and all render-time checks.
helm template scion-hub deploy/helm/scion-hub \
  --namespace scion-system \
  -f values-ha.yaml

# Install or upgrade.
helm upgrade --install scion-hub deploy/helm/scion-hub \
  --namespace scion-system \
  -f values-ha.yaml
```

Some changes reach the pods only after a restart, because `helm upgrade` updates
the Secret but the running containers keep the old value:

- the session secret (`auth.sessionSecret` or the Secret named by
  `auth.existingSecret`),
- `database.password`,
- an OAuth client secret (`auth.oauth.web.google.clientSecret` or
  `auth.oauth.web.github.clientSecret`).

After changing one of these, run:

```bash
kubectl rollout restart deploy/scion-hub --namespace scion-system
```

Rotating the session secret signs every user out. Other changes to
`settings.yaml` alter the `checksum/settings` pod annotation and restart the pods
on their own.

---

## 8. Render-time checks

`values.schema.json` rejects unknown keys and wrong types. The templates also
refuse to render, with a message naming the value, in cases including:

- a missing or non-`https://` `hub.baseUrl`, a missing `hub.hubId`,
  `image.repository`, or agent image registry;
- no session secret, or both `auth.sessionSecret` and `auth.existingSecret`;
- both `image.tag` and `image.digest`;
- an HA deployment whose settings the hub's preflight would refuse (see above);
- `database.driver: postgres` without `cloudsql.enabled`, or without
  `database.auth`;
- credential-like literals in `hub.args`, `hub.extraEnv` values, pod
  annotations, pod labels, or scheduling values. Deliver credentials through
  `valueFrom.secretKeyRef` instead;
- `hub.extraEnv` entries that set `SCION_SERVER_DATABASE_*`,
  `SCION_SERVER_OIDC_*`, or `SCION_SERVER_SECRETS_*`, or variables the chart
  already sets. Use `config.extra` to reach unmodelled `settings.yaml` keys;
- `config.extra` content that overwrites a key the chart renders, disables
  hosted mode, or sets `server.hub.public_url`;
- `config.existingSecret` combined with values that feed the rendered
  `settings.yaml`.

When the chart truncates a cluster-scoped RBAC name to 63 characters, it appends
a digest of the full release name and namespace, so two releases cannot end up
with the same `ClusterRole` name.

`config.existingSecret` lets you supply the whole `settings.yaml` yourself. In
that case the chart does not check the file's contents.

---

## 9. Verification

The chart has no `helm test` hooks. After installing, run the live checks in
[`deploy/helm/scion-hub/VALIDATION.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/deploy/helm/scion-hub/VALIDATION.md).
They cover the two-step IAP install, readiness through the load balancer, Cloud
SQL connectivity, and rollout behaviour.

---

## 10. Troubleshooting

Hub logs:

```bash
kubectl logs -n scion-system -l app.kubernetes.io/name=scion-hub -c hub
```

Cloud SQL Auth Proxy logs, when enabled:

```bash
kubectl logs -n scion-system -l app.kubernetes.io/name=scion-hub -c cloud-sql-proxy
```

- **Pod rejected with a `runAsNonRoot` error:** `image.repository` points at a
  root-running image. Use the `hub-gke` image.
- **Pod stays unready:** `/readyz` returns 503 with a JSON `reason`, such as
  `database not available`. Check it from inside the pod network, for example
  with `kubectl port-forward deploy/scion-hub 8080:8080 -n scion-system` and
  `curl localhost:8080/readyz`. With Cloud SQL, also check the proxy logs. The
  readiness probe does not restart the pod.
- **Every request returns 401 after install:** the IAP audience is still a
  placeholder, or does not match the backend service. Look for the placeholder
  warning in the hub log (logged only on an HA shape).
- **Users are signed out intermittently:** replicas disagree on the session
  secret. Check that every pod was restarted after the secret changed.
