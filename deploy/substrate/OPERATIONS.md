# Operating the Substrate broker

Day-2 operations for a deployed `deploy/substrate/broker.yaml` broker: warming
a template before real traffic, what a broker restart does and doesn't
recover, cleaning up actors a restarted broker can no longer identify, and
where the runtime's other known limits are tracked. See `README.md` for
install and `cluster/README.md` for the cluster-level prerequisites these
procedures assume are already in place.

## Warm the template before first use

Creating the first agent on a given template builds its golden `ActorTemplate`
snapshot, which can take on the order of a minute or more — dominated by the
golden build itself, not broker or hub overhead. The hub's control-channel
dispatch has a fixed timeout; if the golden build plus actor bootstrap
doesn't finish inside it, the hub gives up and sends a rollback delete that
races the still-in-progress create. The broker's create finishes anyway,
leaving a **running, unbootstrapped actor the hub doesn't know about**,
holding a worker indefinitely. A cold template build is the easiest way to
hit this, but it is a general dispatch-timeout gap, not specific to this
runtime.

**Timeout invariant.** The hub→broker create timeout must be at least the
substrate start budget: actor create, then bootstrap, then the wait for the
actor to reach `RUNNING`. The two sides are bounded by different settings,
and only one of them can be configured today:

- **Broker side (the start budget).** The wait for `RUNNING` is bounded by
  `defaultActorRunningTimeout` in `pkg/runtime/substrate_runtime.go`
  (5 minutes). It is a compiled-in constant, not a setting, and so is the
  post-start healthz wait. On a cold template, the golden build that runs
  before this is bounded separately by `runtimes.<name>.substrate.template_ready_timeout`,
  the only configurable substrate start bound (default 10 minutes).
- **Hub side (the create timeout).** There is **no hub setting** for this. A
  substrate broker serves every create synchronously (it does not support
  async launch, so `hub.launch_timeout` does not apply). The hub waits on a
  fixed 120-second request timeout, whether it dispatches over the control
  channel or calls the broker directly over HTTP.

The hub's fixed 120 seconds is shorter than the broker's worst-case start
budget, so the invariant only holds when the start actually finishes inside
the hub's window. Warming the template (below) keeps the golden build out of
the request path. Until the hub timeout becomes configurable, treat a create
that times out on the hub as the orphaned-actor case described above.

Warm a golden for a brand-new project by creating one throwaway agent first —
the first `CreateActorTemplate` for a project needs that project's atespace
to exist, and an ordinary `scion start` creates it as a side effect:

```sh
# Start a throwaway agent on the profile/template you're about to use for
# real. The first create against a new template builds the golden snapshot
# and can take well over a minute — expected here.
scion start --profile substrate warm-template-check "echo warm"

# Wait for it to actually reach `running` before treating the template as
# warm — don't just wait for the command to return.
scion list | grep warm-template-check

# Clean up the throwaway agent. The template and its golden snapshot are
# NOT deleted with it (there is no template GC yet) — that's the point: the
# next real create against the same template reuses the now-ready golden
# and takes the fast (sub-second broker dispatch) path.
scion delete warm-template-check
```

Re-warm before pointing real traffic at anything that changes the template's
content-addressed name — a new image digest, resource shape, sandbox class,
`worker_selector`, or `egress_trust_bundle`.

## After a broker restart

The broker keeps two things only in memory, never persisted: the per-agent
record used to report an agent's project/slug on `list`, and the control
token minted at bootstrap for exec/message. A broker restart — a pod
reschedule, a rolling update, an OOM kill, anything that starts a new
process — drops both for every actor a *previous* process created. The
actors themselves, their egress policies, and their workers are untouched on
the cluster; only this broker's memory of them is gone.

**The invariant: no delete or stop reports success while the actor exists**,
with two exceptions: an actor already in `ACTOR_STATE_DELETING` (see "A
pre-restart actor stuck in `DELETING`" below — the `DELETING` exclusion has
nothing to do with a restart by itself, but can combine with one); and a
pre-restart actor under a second substrate profile on a different `ateapi`
endpoint, which isn't probed until that profile's first `Run` (see
"Record-less-actor probe scope" below — not applicable to a deployment with
only one substrate profile). Otherwise, concretely:

- **Delete/stop of a pre-restart agent by its slug returns HTTP 409**
  (`agent_identity_unknown`) instead of the ordinary 404/202, with a generic
  body reporting only how many unidentified actors it holds — the
  runtime-specific scope (for example, the atespace) is not in the body;
  operators read it from the broker WARN log under the `runtime_scope` key.
  This is
  intentional: a 404 is treated as an idempotent completed delete, and a 202
  as a completed stop — either would let the actor, its egress policy, and
  its worker leak with no further signal. An explicit conflict, requiring an
  operator, is the safer failure.
- **If the underlying check itself can't run**, delete/stop return an
  explicit 5xx, never the idempotent success a failed check would otherwise
  fall back to.
- **Exec** on a pre-restart agent fails permanently for that actor — the
  bootstrap endpoint is one-shot, so a new broker process has no way to
  re-mint or recover a token an old process already used.
- **Message** is accepted and reported as delivered by the hub, but never
  actually delivered — a general gap in how a dispatched message ID reaches
  the broker, not specific to this runtime.
- Any agent created **after** the restart is unaffected.

### Cleaning up a record-less actor

The 409 response body carries only the atespace and a count, never an actor
name or a control token — identify the actor from the broker's own log or
your own records first, not from the response body:

```sh
# 0. Identify the actor. The 409 response body only proves at least one
#    record-less actor exists in this atespace; it does not name it, and it
#    is never a license to act on every name the broker's log turns up. The
#    broker logs the record-less actor NAMES at WARN on every such 409 (only
#    in its own log, never in the HTTP response). Because a rolling update
#    or `strategy: Recreate` can list more than one broker pod at once, find
#    the SPECIFIC pod that logged the line you care about, not an arbitrary
#    one (`kubectl logs deploy/...` picks one pod for you, which may not be
#    the one that logged this WARN):
kubectl -n "${BROKER_NAMESPACE}" logs --prefix --tail=-1 -l app=scion-substrate-broker \
  | grep -i "agent identity unknown"
#    Each matching line is prefixed "[pod/<pod-name>/scion-substrate-broker]"
#    -- note that pod name (call it POD below) and read the
#    "recordless_actors" field on that same line.
#
#    The actor THIS delete/stop is for is named "<project-slug>--<agent-slug>"
#    -- act ONLY on that one name, and only if it appears in POD's WARN line
#    and passes both checks below. The WARN line lists every record-less
#    actor in the atespace, which routinely includes OTHER pre-restart
#    agents in the same project that are still running (and, per the
#    atespace-prefix note below, possibly another project's actors sharing
#    the prefix). Those other names are NOT part of this procedure and must
#    not be deleted here: each is cleaned up through its own agent's
#    delete/stop, when that request hits this same 409.
#
#    For the one name that matches <project-slug>--<agent-slug>:
#      (i)  confirm its .metadata.createTime predates the broker CONTAINER's
#           current start in POD -- not POD's .status.startTime, which is
#           when the POD started and does NOT move when the kubelet restarts
#           a crashed or OOM-killed container in place (restartPolicy:
#           Always, the Deployment default). An in-pod container restart is
#           exactly the kind of "broker restart" this whole mechanism exists
#           to catch, so the pod's start time is the wrong boundary: use the
#           container's own state.running.startedAt instead:
kubectl ate get actor -a <atespace> <actor> -o yaml   # read .metadata.createTime
kubectl -n "${BROKER_NAMESPACE}" get pod "$POD" \
  -o jsonpath='{.status.containerStatuses[?(@.name=="scion-substrate-broker")].state.running.startedAt}'
#           An actor created after that timestamp is not a pre-restart
#           actor, whatever this 409 says -- do not act on it.
#      (ii) cross-check it against the hub's own agent list for this
#           project (its phase/name should match what you expect).
#    If more than one project could plausibly share this atespace (see the
#    atespace-prefix note below), confirm ownership with that project's
#    owner first. Only the one name verified this way -- read from POD's own
#    WARN log line, confirmed pre-restart against POD's own container start,
#    and matched against the hub's agent list -- may be acted on below;
#    never guess from the requested slug alone, and never act on the other
#    names the same WARN line lists.

# 1. Only once the actor is confirmed: delete its egress policy FIRST. This
#    order is mandatory, not a suggestion — deleting the actor before its
#    egress policy leaves the policy behind with nothing left to delete it
#    (the record-less-actor check only ever reports actors, so an orphaned
#    policy alone is invisible to this whole mechanism). There is no
#    dedicated CLI for this; call the ateapi Control service's
#    DeleteActorEgressPolicy RPC directly. Keep TLS verification ON: never
#    grpcurl -insecure/-plaintext against a real cluster endpoint.
#
#    Per deployment, fill in: <atespace> and <actor> (from step 0); the
#    CA bundle passed to -cacert, which is the CA that signs the
#    api.${ATE_SYSTEM_NAMESPACE}.svc certificate (cluster-internal, not in a
#    public trust store; with the settings this deployment uses it is the
#    ClusterTrustBundle named by substrate.cluster_trust_bundle, or the file
#    named by substrate.ca_file if you use that instead); and a host to run
#    grpcurl from that can resolve and reach api.${ATE_SYSTEM_NAMESPACE}.svc:443
#    directly (inside the cluster network) -- it is a headless,
#    cluster-internal service name, so it does not resolve from a
#    workstation (see the workstation form after the in-cluster form below).
#
#    The token is held in an exported environment variable, never written to
#    a file and never put on grpcurl's command line: with -expand-headers,
#    grpcurl itself replaces ${ATE_TOKEN} in the header with the value of
#    that environment variable, and the single quotes stop the shell from
#    expanding it first, so argv only ever contains the literal text
#    '${ATE_TOKEN}'. The export and both calls run in a subshell, so
#    ATE_TOKEN is gone as soon as it exits -- whether or not the calls
#    inside succeed -- with no separate `unset` needed.
kubectl get clustertrustbundle "${CLUSTER_TRUST_BUNDLE_NAME}" \
  -o jsonpath='{.spec.trustBundle}' > ate-ca.pem
(
  export ATE_TOKEN="$(kubectl create token scion-substrate-broker -n "${BROKER_NAMESPACE}" \
    --audience api.${ATE_SYSTEM_NAMESPACE}.svc --duration 600s)"
  grpcurl -cacert ate-ca.pem -expand-headers -H 'Authorization: Bearer ${ATE_TOKEN}' \
    -d '{"actor":{"atespace":"<atespace>","name":"<actor>"}}' \
    api.${ATE_SYSTEM_NAMESPACE}.svc:443 ateapi.Control/DeleteActorEgressPolicy

  #  Verify the policy is actually gone before touching the actor — an
  #  orphaned policy after this step is invisible to this whole mechanism, so
  #  this check matters, not just as a courtesy:
  grpcurl -cacert ate-ca.pem -expand-headers -H 'Authorization: Bearer ${ATE_TOKEN}' \
    -d '{"actor":{"atespace":"<atespace>","name":"<actor>"}}' \
    api.${ATE_SYSTEM_NAMESPACE}.svc:443 ateapi.Control/GetActorEgressPolicy
)   # ATE_TOKEN is gone as soon as the subshell exits, whatever happened inside
#    Expect a NotFound gRPC status. Anything else means the policy is still
#    there; do not proceed to step 2 until it reads NotFound.
#
#    From a workstation: the CA bundle is already fetched above by the same
#    command; port-forward instead of reaching the cluster-internal service
#    name directly, and keep TLS verification anchored to the in-cluster
#    name via -authority, which also sets the TLS server name used for
#    verification. Run the port-forward and both calls in their own
#    subshell, so ATE_TOKEN stays scoped to it just like the in-cluster form
#    above; guard the port-forward with a trap right after capturing its
#    PID, so it is stopped whether the subshell exits normally, is
#    interrupted, or is terminated. A separate `INT TERM` trap that exits is
#    required alongside the `EXIT` one: a trap only runs a handler and
#    resumes the script at the next command, so an `EXIT INT TERM` trap
#    whose handler is just the cleanup would kill the port-forward on
#    Ctrl-C and then keep going into `export ATE_TOKEN=...` and the grpcurl
#    calls below instead of stopping the block — `exit 130` (128+SIGINT) on
#    `INT TERM` makes the interrupt actually stop the block, and that exit
#    is what then fires the `EXIT` trap's cleanup. Wait for the
#    port-forward to actually be listening before the first grpcurl call:
#      (
#        kubectl -n "${ATE_SYSTEM_NAMESPACE}" port-forward svc/api 9555:443 &
#        pf=$!
#        trap 'kill "$pf" 2>/dev/null' EXIT
#        trap 'exit 130' INT TERM
#        sleep 2   # give the port-forward time to start listening
#        export ATE_TOKEN="$(kubectl create token scion-substrate-broker -n "${BROKER_NAMESPACE}" \
#          --audience api.${ATE_SYSTEM_NAMESPACE}.svc --duration 600s)"
#        grpcurl -cacert ate-ca.pem -authority api.${ATE_SYSTEM_NAMESPACE}.svc \
#          -expand-headers -H 'Authorization: Bearer ${ATE_TOKEN}' \
#          -d '{"actor":{"atespace":"<atespace>","name":"<actor>"}}' \
#          localhost:9555 ateapi.Control/DeleteActorEgressPolicy
#        # (same substitution for the GetActorEgressPolicy verification
#        # call above, run inside this same subshell)
#      )
#    Once verification reads NotFound, proceed to step 2.

# 2. Delete the actor itself, any_state so a non-RUNNING actor isn't
#    rejected:
kubectl ate delete actor -a <atespace> <actor> --any-state

# 3. Force-delete the hub's own agent record so it doesn't keep dispatching
#    to an actor that no longer exists. There is no --force on the delete
#    client -- call the hub API directly, with the access token from
#    ~/.scion/credentials.json (mode 0600), the same one scion itself uses
#    to authenticate; do not create or expect any separate "~/.scion/token"
#    file. Nothing that expands a secret may appear on the command line
#    (shell history, `ps`, `/proc/<pid>/cmdline`) or be left behind if this
#    is interrupted: the whole read/write/curl sequence runs in a subshell,
#    so its EXIT trap fires — removing the header file — as soon as curl
#    returns, not only when the surrounding interactive shell eventually
#    exits. A separate `INT TERM` trap that exits is required alongside the
#    `EXIT` one, the same reasoning as the port-forward subshell above:
#    without it, an interrupt would remove the header file and then fall
#    through to the curl call instead of stopping the block.
(
  read -rs TOKEN          # paste the hub token; not echoed, not in history
  HDR=$(mktemp); trap 'rm -f "$HDR"' EXIT; trap 'exit 130' INT TERM
  printf 'Authorization: Bearer %s\n' "$TOKEN" > "$HDR"; chmod 600 "$HDR"
  unset TOKEN
  curl --fail-with-body -X DELETE \
    "https://<hub-endpoint>/api/v1/agents/<agent-id>?force=true" -H @"$HDR"
)
```

`--fail-with-body` makes a 401/403/404 exit non-zero (with the body still
shown) instead of silently exiting 0, so a rejected request cannot be
mistaken for a completed step.

`force=true` does not skip the broker: the hub still dispatches the delete
to the broker exactly as a normal delete would — it only changes what
happens when that dispatch fails. Without `force`, a broker error
(including this same 409) fails the hub request and leaves the hub record
alone; with `force`, the hub logs the broker error and deletes its own
agent record anyway, regardless of what the broker did or didn't manage to
clean up. Run it only after steps 0–2 succeed: run it early and the actor,
egress policy and worker are orphaned with no further signal (the broker
error that would have surfaced this is now just a log line); run it while
other record-less actors remain in this atespace and the broker still
returns 409, so the project's on-broker files for this agent may also be
left behind.

### Fail-closed consequences worth knowing about, not fixed here

- **(a)** The atespace name derived from a project ID truncates that ID to
  its first 12 (sanitized) characters. This collision is not just
  theoretical: project IDs are client-supplied at project creation, so
  anyone able to create a project who knows another project's ID prefix can
  deliberately create one that maps to the same atespace. What that allows
  is a count disclosure (how many record-less actors project X's atespace
  holds) and the ability to force 409s on project X's genuinely-absent-slug
  deletes/stops — never a wrong-project action: the probe only ever turns a
  would-be success into an error, it never selects a delete target across
  atespace boundaries.
- **(b)** Once at least one record-less actor exists in a project's
  atespace, *every* delete or stop of an absent slug in that project returns
  409 instead of the usual idempotent 404/202, until every record-less actor
  in that atespace has been cleaned up (above).
- **(c)** A narrow window between `CreateActor` succeeding and this process
  recording it (spanning the running-wait, health check, and bootstrap —
  tens of seconds) makes a new actor look record-less to any concurrent
  delete/stop of an unrelated absent slug in the same project, or of the
  in-flight agent itself: both get a spurious 409 rather than a wrong
  action, since the probe never selects a target. Not hardened further; see
  Known limits below.
- **(d)** See "A pre-restart actor stuck in `DELETING`" below — this is the
  other of the invariant's two stated exceptions.

All are accepted trade-offs of failing closed rather than risking a false
success.

### Record-less-actor probe scope

The record-less-actor probe only reaches the substrate profiles this broker
process has already resolved a manager for. Immediately after a restart,
that is just the default substrate profile — a second substrate profile
pointed at a *different* `ateapi` endpoint is not probed until this process
resolves it at least once (its first `Run`), so a pre-restart actor under
that second profile's atespace gets the ordinary idempotent 404/202 until
then. A deployment with only one substrate profile doesn't hit this case.

### A pre-restart actor stuck in `DELETING`

An actor already mid-delete when the broker restarts (for example, one this
same broker had started deleting, whose deletion never finished before a
crash) is **excluded** from the record-less-actor count above — its egress
policy is already gone, so counting it would turn an ordinary
stop-then-delete race into a false 409. The consequence: a delete of its
slug returns the ordinary idempotent 404 and the hub drops its record, while
the actor itself may still be sitting on the cluster, stuck in `DELETING`.
It can't be found by attempting a delete/stop — a `DELETING` actor never
produces the 409 above. Locate and clear it instead:

```sh
# 1. List the atespace's actors and note every one in ACTOR_STATE_DELETING
#    (there is no --state filter):
kubectl ate get actors -a <atespace>

# 2. For each candidate, compare its creation time against every currently
#    running broker pod's CONTAINER start (not pod start -- see above). Only
#    a candidate that predates every one of those timestamps is stuck; a
#    DELETING actor created after any of them may just be that pod's own,
#    ordinary, in-flight delete.

# 3. Only for a confirmed candidate: delete its egress policy exactly as in
#    "Cleaning up a record-less actor" step 1 above (tolerate NotFound if
#    it's already gone), then:
kubectl ate delete actor -a <atespace> <NAME> --any-state
```

### Exec error redaction after a restart

The broker redacts the secret values it placed in an agent's bootstrap
environment out of exec error text, using a per-agent list of those values
that it caches in memory when it starts the agent. That cache is not
persisted. After a broker restart, or for any actor this broker process
never bootstrapped itself, the cache has no entry for the agent, so an exec
failure's error text (including the truncated control-server stderr it
embeds) is written to the **broker's own log** without that redaction.

The list holds only values the broker itself placed in the bootstrap
environment. Credentials the agent obtains afterwards, such as the refreshed
hub transport credential it keeps in its own token file, are never known to
the broker and are not on it; the broker-log guidance below applies to them
too.

The exposure is narrow:

- **Client responses are unaffected.** A failed exec returns an opaque
  error to the caller. The detailed error text only ever reaches the broker
  log.
- **The exec child carries no agent secrets.** Its environment is built
  from the exec user's passwd entry (plus a fixed PATH and the CA-bundle
  path variables), not from the agent's bootstrap environment. A secret can therefore only appear if the command's own
  output prints it, for example a command that reads a secret file and
  fails.

Treat broker logs as sensitive anyway, as you already should: restrict who
can read them, and don't paste exec failure lines from them into shared
channels without checking them first. Agents created after the restart are
redacted normally.

## Removing or changing the substrate runtime configuration

**Delete every substrate agent before removing (or changing the type of)
the substrate entry in operator settings.** The broker resolves its
default runtime once at process startup, and (since the operator-only
trust-boundary fix in this doc's own "Known limits" below) now refuses to
start at all if that resolution fails — so if the broker's active profile
still points at a substrate runtime definition that settings no longer
defines, or a settings edit introduces a validation error anywhere in that
definition, the broker will not come back up on its next restart. Removing
the substrate entry entirely while agents still exist, or silently
repointing the active profile at a different runtime type, both leave any
agent substrate actually still created unreachable: there is no longer any
registered runtime able to Stop, Exec, or Delete it, even though the actor
itself is still running at the backend. Delete every agent on the
substrate runtime first (ordinary `scion delete`/broker-API delete, same as
any other agent), confirm none remain, and only then remove or repoint the
substrate configuration.

A **construct-time** failure of the default substrate runtime behaves the
opposite way from a config-validation failure: a Kubernetes-client build
error, or a `substrate.Dial` failure (trust-bundle/CA load or API dial — for
example an API-server blip at boot), is treated as potentially transient, so
the broker starts **degraded** rather than refusing to start. That degraded
state is **not** self-healing: the default runtime is resolved once at
startup and is not rebuilt until the broker process restarts, and `/healthz`
still returns healthy (the degraded runtime is logged as the `error` runtime
but counts as an available runtime in the health check), so no liveness probe
restarts the broker on its own. **Alert on the broker log line reporting the
default runtime as `error` and restart the broker to rebuild the runtime.**
Named (non-default) profiles are unaffected — they are resolved per request,
so a profile whose construction fails retries on the next request rather than
staying degraded until a restart.

## Known limits

The runtime's behavior in areas outside this broker's control — logging,
suspend semantics, egress validation, platform quirks — is out of scope for
this operations doc. The full list of verified defects, limitations, and
feature requests filed against the upstream runtime is tracked in the
project's feedback record, grouped into defects, limitations, friction
points, and requests; the ones most likely to affect day-2 operation of
this broker are:

- Stuck `DELETING` actors that hold a worker indefinitely until force-cleaned,
  and force-clean itself leaking the actor's host-side directory — see "A
  pre-restart actor stuck in `DELETING`" above.
- Slow first creates against a cold template, and a template's golden build
  competing with real actors for pool workers — see "Warm the template
  before first use" above.
- gVisor snapshots only restoring on a matching CPU platform — see
  `cluster/README.md`, "gVisor node pool".
- NetworkPolicy enforcement requiring an enforcing CNI with no built-in
  check, and router ingress having no authorization of its own — see
  `cluster/README.md`, "Enabling NetworkPolicy enforcement", and
  `README.md`, "Known limitations".
- `worker_selector` matching the wrong label silently reporting "no free
  workers" instead of a selector-mismatch error — see `cluster/README.md`,
  "`worker_selector`".
- An actor's egress allowlist admits only the operator-configured hub
  endpoint, never an agent- or template-supplied `SCION_HUB_ENDPOINT`
  override — an override still changes which endpoint the agent itself
  calls, but not which host the actor may reach.
- An actor's own git-clone remote and telemetry endpoint are added to its
  egress allowlist only when `egress_allow` also covers them (exact match,
  or a wildcard one label above) — passing the public-hostname grammar is
  not enough on its own, since a tenant-registered domain or a service like
  nip.io/sslip.io is a well-formed public hostname that can still resolve
  anywhere the tenant chooses. List the actual git host and telemetry
  collector in `egress_allow` if agents need to reach them (see
  `settings.example.yaml`).
- **The runtime no longer grants a blanket `*.googleapis.com` allowance.**
  It adds only `oauth2.googleapis.com`, the Anthropic API host, and — when
  the agent's own config indicates Vertex AI auth — the Vertex endpoint for
  that agent's configured region. One consequence: GCP's own default Cloud
  Trace auto-export (`cloudtrace.googleapis.com`), which the old wildcard
  happened to cover, now fails closed like any other uncovered host unless
  an operator explicitly lists it (or covers it with a wildcard) in
  `egress_allow`. Any other Google API an agent might have reached under
  the old wildcard — GCS, Compute, BigQuery, and so on — needs the same
  explicit `egress_allow` entry now.
- **`stop` is the same as `delete` for a substrate actor.** Nothing in this
  runtime suspends to a durable snapshot yet, so a stop destroys the actor
  and its working directory exactly like a delete would — there is no
  "stopped but resumable" state to come back to. See
  `pkg/runtime/substrate_runtime.go`'s `Stop` doc comment.
- **The broker's own CA/trust-bundle is read once, at dial time.** Rotating
  the CA or trust bundle a broker uses to reach ateapi/the router requires
  restarting the broker process — there is no live-reload path.
- **The broker container runs as root.** `Dockerfile.hub` has no `USER`
  directive, so `broker.yaml`'s pod spec deliberately does not set
  `runAsNonRoot`/`runAsUser` (setting them against a root-only image would
  just `CrashLoopBackOff` the container). It does set
  `allowPrivilegeEscalation: false`, drops all Linux capabilities, and sets
  `seccompProfile: RuntimeDefault` — the hardening that doesn't depend on
  the image's own uid. Move to a non-root image and set `runAsNonRoot`/
  `runAsUser` together in one change, not separately.
- **The broker image is not required to be digest-pinned** the way agent
  images are (see `README.md`, "Pin the agent image by digest"); only the
  agent image is enforced. Recommend pinning `BROKER_IMAGE` by digest
  (`@sha256:...`) anyway when cutting a broker release, for the same
  supply-chain reasons.
- The broker→router hop is plaintext HTTP, and bootstrap credentials cross
  it; NetworkPolicy and the single-use, first-caller-wins bootstrap guard
  mitigate this, not transport encryption. The guard is NOT a nonce check —
  the actor does not verify the bootstrap nonce the broker generates; it
  simply accepts whichever caller claims bootstrap first and refuses every
  later attempt, and the broker treats a conflict (meaning some other
  caller claimed first) as a hijack and deletes the actor. See `README.md`
  for the fix options.
- **`/exec` does not run a login shell.** The actor's control server runs
  each exec command as the unprivileged `scion` user by switching uid and
  gid directly, in that user's home directory, and refuses a user that
  resolves to root. Supplementary groups are not applied (the child has
  only the user's primary group), `PATH` is the fixed root-trusted search
  path rather than anything from the user's profile, and no profile or rc
  files are sourced, so commands that rely on login-shell setup must set it
  up themselves. A child that calls `setsid` escapes the process-group kill
  on timeout; it stays unprivileged and is reaped at container teardown.

## TODO (not automated yet)

- The record-less-actor and stuck-`DELETING` cleanup above are manual runbook
  steps. Nothing in this folder scripts them.
- No golden-template garbage collection: a warmed-but-unused template's
  snapshot is never automatically reclaimed.
