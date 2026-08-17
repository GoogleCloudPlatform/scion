# Operator validation checklist

**These checks have never been run.**

Everything in this file requires a live environment — a GKE Autopilot cluster with
Cloud SQL, Filestore and a GCS bucket — and no such environment was available to
the people who wrote this chart. The chart's static checks (`helm lint`,
`helm template | kubeconform -strict`, the render-time assertions) all pass; that
says the manifests are well formed and internally consistent, and it says nothing
at all about whether the hub they describe actually works.

> **Quote the resource count, every time — `kubeconform` alone does not tell you
> the render happened.** `helm template | kubeconform -strict` exits **0** when the
> render fails: `helm` writes its error to stderr, `kubeconform` gets empty stdin,
> and reports `Valid: 0, Invalid: 0, Errors: 0, Skipped: 0` with a success status.
> The shell reports the exit code of the last command in the pipeline, so the whole
> gate passes on a chart that did not render. Two reviewers hit this live.
>
> Every `kubeconform` result quoted anywhere in this chart's review history is
> therefore trustworthy *only* because it was quoted with its numbers — **5 valid,
> 0 invalid, 0 skipped**. The count was doing the work and nobody knew it, and a
> bare "kubeconform passed" would have read as identical while meaning nothing.
> Until the CI job asserts the count itself (`set -o pipefail` plus a resource-count
> check — Phase 6 owns it), the count is the check and reporting it is not optional.

The checks below were originally written as chart acceptance criteria. They were
moved here rather than deleted, because an acceptance criterion nobody can run is
an acceptance criterion that gets quietly ticked. They are the operator's, and
they are outstanding until an operator runs them.

If you are the first person to install this chart against real infrastructure,
you are the first person to test it. Please record the outcome.

---

## Live checks

Numbering follows the design document's whole-chart acceptance list (§18, items
14–23) so the two can be cross-referenced.

### 14. Two-step install under `auth.mode: proxy` (IAP)

Run the two-step install from `NOTES.txt` (that runbook arrives with the ingress
change; it is not in `NOTES.txt` yet): `helm install` with
`bootstrap.deferHub=true`, wait for the Ingress to get an address, read the
backend-service ID, then `helm upgrade` with `iap.audience` set.

Pass: the upgrade completes; `curl https://<host>/readyz` returns 200;
`gcloud compute backend-services get-health <name> --global` reports `HEALTHY`
for the NEG; a browser reaches the web UI through IAP and a signed-in user gets
a session.

A **404** on the health check means a prefixed readiness path slipped into the
chart — the endpoint is `/readyz` at the root and both the route table and the
authentication exemption match it by exact string. A **401** means the health
check is not hitting an auth-exempt endpoint.

### 15. Single-step install under `auth.mode: oauth`

Only possible once the hosted-mode preflight split has landed; until then the
pod fails preflight at startup whatever the chart renders.

Pass: one `helm install`, no backend-service ID, no `bootstrap.deferHub`, and a
signed-in user gets a session with no IAP in the request path.

### 16. `hub_id` is identical across replicas and stable across upgrade

    kubectl get pods -l app.kubernetes.io/name=scion-hub \
      -o jsonpath='{range .items[*]}{.metadata.annotations.scion\.io/hub-id}{"\n"}{end}'

Pass: every pod prints the same value, it equals the `hub.hubId` you supplied,
and it is unchanged after `helm upgrade`. Confirm the hub agrees with the
annotation — check the hub's own logs or admin view for the ID it is using, not
just the manifest.

Fail here is expensive and quiet: divergent hub IDs put replicas on different
GCS prefixes and different secret scopes, and it gets worse on every rollout.

### 17. The hub reads and writes the configured GCS bucket

Exercise something that stores a blob (a template or an attachment), then:

    gsutil ls -r gs://<bucket>/

Pass: objects appear under the bucket, prefixed by the hub ID. Also confirm the
negative: no equivalent objects are written to the Filestore share. Hub blob
storage and workspace storage are different subsystems and conflating them is
the most likely configuration error in this chart.

### 18. Long-lived WebSockets, unbuffered SSE, unmodified headers

Open a web terminal on an agent and leave it idle for more than 120 seconds.

Pass: the session survives. A drop at ~30s means the backend timeout is not
being applied — `BackendConfig.spec.timeoutSec` is a *stream* timeout for
WebSockets and 3600 is load-bearing, not tuning.

Also confirm server-sent event streams are not buffered (events arrive as they
are produced, not in a burst), and that `X-Scion-*` request headers arrive at
the hub byte-identical — they are inputs to an HMAC canonical string, so any
rewrite breaks authentication in a way that reads like a bad credential.

### 19. An agent starts, on Autopilot, without a Pending PVC

Create an agent and watch it through to `Running`.

    kubectl get pvc -n <agent-namespace> -w

Pass: the agent pod reaches `Running` and no PVC sits in `Pending`. A `Pending`
RWX PVC on Autopilot means the default StorageClass is not RWX-capable; the
agent will hang silently rather than error.

### 20. `helm upgrade` with a changed image

Pass: the upgrade completes and the hub returns to ready. Expect a short outage
at one replica — the update strategy is `Recreate` — and expect PTY sessions,
port tunnels and in-memory presence to be lost across the restart.

### 21. `helm uninstall` is non-destructive

    helm uninstall <release>

Pass: the PersistentVolume, the PersistentVolumeClaim, the Filestore instance,
the GCS bucket and the Cloud SQL instance all still exist afterwards.

### 22. Documented surprise: database-owned settings shadow chart values

Change `runtimes` or `admin_emails` in your values file and run `helm upgrade`
against a hub that has already run once with Postgres.

Pass: the *database* value wins and the chart value is inert. This is the
documented behaviour, not a bug, and it is the single most surprising
operational property of the deployment: `helm upgrade` reports success and
changes nothing.

### 23. Agent workspace storage regression check

With `workspaceStorage.backend: nfs` and the runtime plumbing issue still open,
inspect an agent pod's manifest.

    kubectl get pod <agent-pod> -o yaml | grep -A5 'volumes:'

Expected (not desired): an `EmptyDir`, not the workspace PVC. Then determine
whether edits an agent makes reach the share at all. The hub side of workspace
storage does work, so the hub reads and writes Filestore while agents write to
ephemeral disk — a split view of the same workspace, which is worse than a
feature that is simply switched off. Record what you observe; this is the
question the check exists to answer.

---

## Relocated per-phase checks

Later changes to this chart append their own unrunnable checks here, with the
same rule: a check that was moved here is a check that has **not** passed.

### Chart skeleton and core workload

One item. Every other criterion for this part of the chart is static and was
verified at authoring time.

#### A root image fails pod admission

The chart sets `runAsNonRoot: true` on the pod and on the hub container and
exposes no value that can turn it off. The static half of that is verified: the
field is a literal in the template, `hub.securityContext` rejects unknown
properties so it cannot be reintroduced as an override, and `runAsUser: 0` fails
the render. What could not be verified is the half that matters — that the
kubelet actually refuses the pod.

Point the chart at a root image on purpose, for example the published
`scion-hub` artifact, and install it.

    kubectl describe pod <hub-pod>

Pass: the pod does not start, and the event says the container has
`runAsNonRoot` and the image will run as root. **Fail** — and this is the case
worth watching for — is a pod that reaches `Running`. That means something in
the path is stripping or overriding the security context, and the wrong image is
running as root while looking healthy.

Then run the same install with a hub image built from the root `Dockerfile` with
`--target hub-gke` and confirm the pod schedules and `/readyz` returns 200.
Confirm the files it creates on the workspace share are owned by
`hub.securityContext.runAsUser` and `runAsGroup`, not by uid 0.

That positive direction is stated more precisely in the image and storage
checks below, which were relocated separately. Run them as a pair with this one.
This check establishes that the wrong image is **refused**; those establish that
the right image is **admitted and serves**. An operator who runs only one of the
two learns half of it — a cluster that refuses everything passes this check and
is completely broken.

### Image build and workspace storage

Relocated with the same reasoning, and stated here in the words of the phase
that owns them: there is no cluster in this environment; `runAsNonRoot`
admission is kubelet behaviour and Docker locally is not a pod; and NFS
ownership depends on the share, the mount options and `fsGroup`, none of which
exist outside a cluster.

- [ ] Deploy the chart and confirm the hub pod reaches Ready with
      `securityContext.runAsNonRoot: true` set -- `kubectl get pod -l
      app.kubernetes.io/name=scion-hub` shows Running, not
      `CreateContainerConfigError`. Then from inside the cluster
      `curl -s -o /dev/null -w '%{http_code}' http://<svc>:8080/readyz` returns 200.
      (Exact path `/readyz`. Not a prefixed variant, and not the endpoint that
      answers 200 unconditionally.)

- [ ] With the Filestore share mounted, make the hub write to it (start it, or
      `touch` a file under the mounted path as the hub's uid) and confirm
      `ls -n` on the share shows the new files owned by the numeric `nfs.uid` /
      `nfs.gid` configured in `values.yaml` -- not `0:0`, and not `root:root`.
