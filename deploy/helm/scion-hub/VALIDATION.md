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

### Configuration intake (`settings.yaml`)

The rendered content is fully covered by `hack/verify.sh` and `golden/` — the
file shape, the preflight keys in both auth modes, the deep merge, the absent
environment variables — and none of that needs a cluster. What needs a cluster
is everything about *delivery*: whether the mount lands, whether the hub can
read it, and what the hub does when it tries to write it.

#### 1. The hub can read the mounted settings file

The single most likely delivery failure, and the one that does not announce
itself. A Secret volume is projected `root:root` regardless of the pod's
`securityContext`, and the hub runs as a non-root uid — so the file's mode is
load-bearing. The chart projects `0444`.

    kubectl exec <hub-pod> -- ls -l /home/scion/.scion/settings.yaml
    kubectl exec <hub-pod> -- head -5 /home/scion/.scion/settings.yaml

Pass: the mode is `-r--r--r--`, and the hub's own logs show it loaded the
configuration — the hub ID it reports matches `hub.hubId`, and the database
driver it reports matches `database.driver`.

**Fail looks like a configuration error, not a permissions error.** An unreadable
settings file does not produce "permission denied" anywhere the operator will
look. The hub starts on its defaults and then fails the hosted preflight naming
a missing key — sending you to a settings file that is, in fact, correct. If you
are debugging a preflight failure that names a key you can see in the Secret,
check the mode first.

Do not fix a permissions problem here by adding `fsGroup`. It is pod-wide, so it
grants the group to every sidecar as well, and it makes the kubelet apply
recursive ownership changes to mounted volumes — which becomes a startup hazard
once the workspace share is an NFS mount.

#### 2. `$HOME/.scion` is writable and the settings file is not

    kubectl exec <hub-pod> -- touch /home/scion/.scion/probe && echo dir-writable
    kubectl exec <hub-pod> -- sh -c '>> /home/scion/.scion/settings.yaml' ; echo $?
    kubectl exec <hub-pod> -- ls -a /home/scion/.scion

Pass: the directory accepts a write and the append to `settings.yaml` fails. The
directory is the hub's whole state directory — `storage/`, `scion-token` and
anything else it needs at runtime are created in it — so a read-only directory
breaks the hub for reasons unrelated to configuration. Only the one file is
read-only.

**Expect a `cache/` tree and nothing else, and compare full paths rather than
directory names.** Both halves of that will otherwise send somebody hunting, and
the second half will send them to the wrong conclusion rather than to no
conclusion.

Neither `agents/` nor `harness-configs/` will exist directly under
`/home/scion/.scion`, and that is correct. Two mechanisms combine, and they are
independent — losing one does not restore the directories.
`cmd/server_foreground.go:1771` bootstraps templates and harness configs from
local `~/.scion` directories only in the `else` arm of an explicit
`if hostedMode`; the hosted arm uses `BootstrapBundledResources` instead, "so
every replica converges on the same DB + storage state". And
`config.InitGlobal`, at `cmd/server_foreground.go:104`, which *does* run in
hosted mode and *would* create both directories, is reached only
`if os.Stat(globalDir)` reports the directory missing — and the chart mounts an
`emptyDir` at exactly that path, so it never fires.

That second mechanism is the chart changing which startup branch the hub takes,
which is why it is written down rather than assumed.

Nothing in the hub needs either directory in this deployment, and that is an
enumeration of the source rather than an inference from one comment. For
`harness-configs/` the three non-CLI readers are `cmd/server_foreground.go:1777`
(the `else` arm above) and `pkg/hub/system_handlers.go:348` and `:538`, both
registered behind `requireWorkstation` (`pkg/hub/server.go:3591`, `:3594`),
which returns 404 whenever `Workstation` is false — and
`cmd/server_foreground.go:1496` sets `Workstation: !hostedMode`. For `agents/`
the reachable readers are `pkg/agent/provision.go:88` and
`pkg/agent/list.go:201`; both *are* reachable here, because the chart enables
the runtime broker, and both treat a missing directory as "no agents"
(`provision.go:172-176` stats each candidate and `continue`s; `list.go:207-210`
reads and `continue`s on error). No reader returns an error, warns, or fails a
startup step.

Those two results are not equally durable, and the difference is worth carrying.
`agents/` is safe because of what its readers do, which survives any later phase
switching a feature on. `harness-configs/` is safe because of which features are
off, which does not. **If a later phase makes workstation mode or the onboarding
endpoints reachable**, `handleSystemInit` (`pkg/hub/system_handlers.go:446`)
becomes an HTTP endpoint that calls `config.InitMachine` and `os.RemoveAll`
inside the tree this chart mounts a read-only settings file over. Neither gate
holding it shut is visible from the chart's side.

**What you should see instead** is `cache/templates`, `cache/harness-configs`
and `cache/skills`, created at broker startup by `templatecache.New`
(`pkg/templatecache/cache.go:85`) from `pkg/runtimebroker/server.go:396`, `:413`
and `:422`. Note the collision: `cache/harness-configs` is **not**
`harness-configs`. A check that greps for the name, or runs
`find /home/scion/.scion -name harness-configs`, finds one and concludes the
bootstrap ran — the opposite of the truth.

That tree is also the positive twin for the write test above. The `touch` probe
proves the directory is writable by the shell you exec'd as; the `cache/` tree
proves it was writable by the hub's own uid at startup, which is the principal
the question is actually about.

**Record what is actually in the directory** — none of this has been observed.
Two things would be findings rather than local fixes: any hub log line about a
missing `agents/` or `harness-configs/` directory, or about templates it could
not find; and the **absence** of the `cache/` tree, which would mean the state
directory is not writable. That failure is silent — `templatecache.New` failing
is handled with `slog.Warn` and the broker continues without a template cache
(`pkg/runtimebroker/server.go:337`) — so it degrades rather than crashing, and
nothing else will tell you.

#### 3. What silently does not persist, until ptone/scion#1091

The mount stops the hub from rewriting `settings.yaml`, which is the point: those
writes are pod-local, and `syncHubSettings` re-seeds shared database state from
the pod-local file on every boot, so a replica that can write this file can
promote its own divergence into shared truth. Refusing the write prevents that.

The cost is that several operations now do nothing, and mostly say so quietly.
Confirm each, and record what the operator actually sees:

- **The GitHub App configuration `PUT` returns HTTP 200 and does not persist.**
  Verify it: configure a GitHub App through the API, get the 200, then read the
  configuration back and restart the pod. Expected: the setting is not there.
  This is the worst of the set because the success response is unqualified.
- **Integration settings writes log a warning and swallow the error.** One of
  these paths returns HTTP 500 to the caller while the server continues. Find
  which, and record the message.
- **The startup write logs a warning and continues.** Confirm the pod still
  reaches ready.

Pass: every one of these is soft. Nothing crashloops, nothing panics, nothing is
corrupted, and no two replicas end up disagreeing. **Fail** — and this is the
outcome that would reopen the mount decision — is any of them terminating the
process, or any two replicas reporting different configuration.

This is a behaviour change and in one respect it reads as a regression: with a
writable file these writes succeeded, appeared to work, and were silently lost at
the next pod replacement. They now never take effect. That is the better failure
— consistent beats intermittent, and nothing can diverge — but only if operators
are told, which is what this section and the corresponding `NOTES.txt` section
are for. Both carry the issue number so this reads as temporary, which it is.

#### 4. `helm upgrade` with a changed configuration actually takes effect

A `subPath` mount is bound when the container starts and is frozen for the
container's lifetime; the kubelet's periodic refresh of Secret volumes does not
reach through one. The chart therefore annotates the pod with a checksum of the
rendered Secret so that a configuration change rolls the pods.

Change something visible in `config.extra` — `server.log_level`, say — and run
`helm upgrade`.

Pass: the pods are replaced and the new value is in effect.

**Fail is the quiet one:** the upgrade reports success, the Secret is updated,
and the running hub keeps the old configuration indefinitely, until some
unrelated event restarts the pods and the change takes effect at a time nobody
chose. If you see that, the checksum annotation is missing or is being computed
over something that did not change.

Under `config.existingSecret` the chart deliberately renders no such annotation —
it does not own the file and cannot checksum it. Editing that Secret is expected
*not* to roll the pods; restart them yourself.

#### 5. `schema_version` and the migration rename

Not runnable as a positive test — the point is that nothing happens. Worth
knowing while you are in here: the hub auto-migrates a settings file whose format
it cannot detect, the detector keys on `schema_version`, and the migration
replaces the file with `os.Rename`, which returns `EBUSY` against a bind mount.
The chart always renders `schema_version: "1"`, and `hack/verify.sh` enforces it
under the name `migration-rename-hazard` across every values permutation.

If you supply your own file through `config.existingSecret`, it must carry
`schema_version`. If a hub ever fails with a rename or `EBUSY` error on the
settings path, this is why.

### Cloud SQL

Four items, and **every one of them is UNRUN rather than pending.** The
distinction is the point of this section, so it is stated once here and not
softened below: *pending* describes work that is scheduled, and reads as a
queue that will drain on its own. **Nothing here is scheduled.** No cluster and
no Cloud SQL instance were reachable from the environment this phase was built
in, the access needed to reach them was not held, and no request for it was
outstanding at the time of writing. An item marked UNRUN stays UNRUN until
somebody runs it and edits this file.

#### 7.1 The connection budget's `S` has never been observed

`NOTES.txt` prints the budget as `TOTAL = R * (M + S)` and requires the operator
to supply `S` — every connection a replica holds that is not in the ent pool.
**The chart does not default `S`, does not estimate it, and no one on this phase
measured it.** The value shipped is an operator input for exactly that reason.

`NOTES.txt` also prints a **structural maximum**, `S_max = 2 * max(4, NumCPU) + 2`,
derived by reading pgx v5.9.2 (`pgxpool/pool.go:383`, `defaultMaxConns = 4`,
raised to `runtime.NumCPU()` when larger) against the hub's two pool
constructions. **That is a ceiling read out of source, not an overhead observed
in a process**, and the two must not be confused: a ceiling tells you what the
code cannot exceed, not what it typically holds. Sizing a Cloud SQL instance
from `S_max` will over-provision; sizing it from an unmeasured guess will
under-provision at the worst moment.

**To run it**, once a hub is serving against Cloud SQL under representative
load:

```sql
SELECT count(*) FROM pg_stat_activity WHERE datname = '<database.name>' AND usename = '<role>';
```

Subtract `database.maxOpenConns` and divide by `replicaCount`. Record the number
and the load it was taken under — a value with no load beside it is not
reusable.

#### 7.2 The IAM verification: UNRUN, with the refusals that stopped it

The design for this phase called for verifying, against the real project,
whether Cloud SQL IAM database authentication is usable — and made the result
of that verification the thing that would set `database.auth`'s default.

**The verification could not be performed. It is UNRUN.** Measured
`2026-08-17T10:24:24Z`, principal
`scion-my-grove@deploy-demo-test.iam.gserviceaccount.com`:

```
$ kubectl config get-contexts
CURRENT   NAME   CLUSTER   AUTHINFO   NAMESPACE
                                                    <- header row only; zero contexts

$ gcloud container clusters list
ERROR: (gcloud.container.clusters.list) ResponseError: code=403, message=Required
"container.clusters.list" permission(s) for "projects/deploy-demo-test". This command
is authenticated as scion-my-grove@deploy-demo-test.iam.gserviceaccount.com which is
the active account specified by the [core/account] property.

$ gcloud sql instances list
ERROR: (gcloud.sql.instances.list) [scion-my-grove@deploy-demo-test.iam.gserviceaccount.com]
does not have permission to access projects instance [deploy-demo-test] (or it may not
exist): The client is not authorized to make this request. This command is authenticated
as scion-my-grove@deploy-demo-test.iam.gserviceaccount.com which is the active account
specified by the [core/account] property.
```

`docker` is also absent, so there was no local Postgres fallback either.

**What the chart did instead of guessing.** `database.auth` is **required and
has no default.** It is the only value in this phase treated the way
`image.repository` and `hub.hubId` are treated: the operator must state it. A
default here would have been a parameter nobody chose, presented as one somebody
did — and the two failure directions are not symmetric. Defaulting to `iam` on a
project without IAM DB auth enabled produces an authentication failure at the
first query, after a successful install. Defaulting to `password` silently
selects the weaker credential path for operators who never read this file.
Withholding the default costs one line in every values file and is the only one
of the three that is reversible once somebody measures.

**Do not restore a default to this key on the strength of this section being
read.** The condition for adding one is a measurement, recorded here.

**The `gcloud` commands `NOTES.txt` prints are derived from documentation and
are themselves unrun.** They were checked against Google's Cloud SQL IAM
documentation — including the detail that `gcloud sql users create` and the
login role both take the service account **with `.gserviceaccount.com`
stripped**, which was verified against the docs rather than assumed, an
assumption having been drafted and caught first. Documentation-derived is not
executed. No command in that section has been run against a real instance.

**To run it**, with a project where the 403s above do not occur:

1. Confirm the instance has IAM authentication on:
   `gcloud sql instances describe <INSTANCE> --format='value(settings.databaseFlags)'`
   and look for `cloudsql.iam_authentication=on`.
2. Run the numbered commands from `helm install`'s `NOTES.txt` output in order.
3. Install with `database.auth: iam`, then check `/readyz` (see 7.3).
4. **Then edit this section**: replace it with what happened, whichever way it
   went. A verification that failed is a result and belongs here; only an
   unrun one belongs under this heading.

> **This section moving off UNRUN is a registered trigger obligation.** See
> `hack/trigger-entry-c.sh`, which is the executable form of the paragraph
> above, and read its header before assuming CI is watching this for you. **It
> is not.** The predicate is the state of a Google Cloud project, which no check
> in this repository can observe.

#### 7.3 The manual smoke test: UNRUN

Three claims, none of them exercised, all of which the static checks are
structurally unable to reach:

1. **The hub reaches Postgres through the proxy.** The chart renders
   `server.database.url` with the host fixed at `127.0.0.1` and the port shared
   with the proxy's `--port` from one value. That they agree is asserted
   statically. That a connection is established over the loopback the proxy
   binds is not.
2. **`AutoMigrate` completes.** The native sidecar's startup probe is what is
   supposed to hold the hub container until the tunnel is up, so migration does
   not race the proxy. **The ordering is a property of the kubelet, and no
   kubelet has run this pod.**
3. **`/readyz` returns 200.** The readiness path is `/readyz`; the chart
   contains no occurrence of the deprecated liveness path Phase 0 banned. The
   banned literal is deliberately NOT written out in this file: VALIDATION.md
   is not in `.helmignore`, so it ships inside the chart, and a tree-wide sweep
   for that literal would otherwise find its own documentation and go red on
   the sentence claiming the absence. It did exactly that once - see the note
   below. That absence is asserted statically. A 200
   from a live hub is not.

**To run it:** install against a real cluster and instance, then
`kubectl exec` into the hub container and `curl -s -o /dev/null -w '%{http_code}' localhost:<port>/readyz`.
Record all three outcomes, not just the last — a 200 tells you nothing about
whether migration raced the tunnel on the way there.

#### 7.4 The proxy container's runtime identity: UNVERIFIED

Two properties of the rendered proxy container are asserted by the manifest and
have never been observed in a running pod:

- **The proxy runs as uid 1000, not 65532.** The upstream image declares
  `USER 65532`, but this pod sets `runAsUser` at pod level from
  `hub.securityContext.runAsUser`, and a pod-level `runAsUser` is inherited by
  every container — it can be overridden, not unset. So the rendered spec puts
  the proxy on the hub's uid. This is **expected** to be harmless: the proxy
  writes nothing, opens no uid-owned files, and needs a socket and the metadata
  server. It is **not verified**, and it is the sort of thing that is obvious in
  hindsight either way.
- **`readOnlyRootFilesystem: true` is survivable for the proxy.** It is set on
  the proxy container and deliberately not on the hub container, which writes
  its state directory. Whether the proxy genuinely writes nothing to its root
  filesystem — under every code path, including error paths and credential
  refresh — has been reasoned about and not observed.

**To run it:** `kubectl get pod <pod> -o jsonpath='{.spec.initContainers[?(@.name=="cloud-sql-proxy")].securityContext}'`
for what was admitted, then `kubectl exec -c cloud-sql-proxy <pod> -- id` for
what is actually running. If the proxy crash-loops with a write error, this
section is why.
