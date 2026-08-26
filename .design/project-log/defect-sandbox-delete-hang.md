# Defect: `sandbox delete --force` always hangs and orphans a `runsc` process

**Filed by:** Scion single-node design workstream
**Date:** 2026-08-26 (**revision 3** — supersedes revisions 1 and 2)
**Component:** Cloud Run Sandboxes — `sandbox` CLI (`/usr/local/gcp/bin/sandbox`), platform-injected via `sandboxLauncher: true`
**Severity (to us):** High — teardown is our *normal* lifecycle, not an edge case
**Upstream status:** **Filed with the Cloud Run team, tracked internally** (ptone,
2026-08-26). No public issue exists, so this document is the citable record — it is
what our workaround code references, and it should travel with the code rather than
living only in a scratchpad.

> **Identifying the affected build.** Because there is no public bug to watch, the
> checkable anchor is the runtime version: **`runsc google-958767651`, spec 1.2.1**
> (binaries dated 2026-08-04). Anyone assessing whether a given Instance still has
> the defect should read that off the live host rather than infer it from a date.

> ### What changed in revision 2
>
> Revision 1 reported the hang as occurring *while an interactive `sandbox exec` was
> attached*, and flagged that we had not run the controls to know whether the exec was
> implicated. **We have now run them, and the exec is a red herring.**
>
> `sandbox delete --force` hangs on **every** sandbox we tried, including one running
> nothing but `sleep 3600`. The claim is therefore simpler and stronger than revision 1
> stated: **`--force` delete does not complete, full stop.** Please discard revision 1's
> framing — in particular §5's list of unknowns, which is now resolved rather than
> outstanding.
>
> Revision 2 also adds: version strings, the exact orphan argv, the fate of the
> orphans, and a separate defect in the **non**-`--force` path (§4).

> ### What changed in revision 3
>
> **The `sandbox` CLI has a 120 s internal timeout.** When it fires, the wrapper
> exits rc=1 and takes its `runsc` child process with it. All orphans — both
> `runsc delete` and `runsc state` — self-clean at 120 s. No zombies, no
> reparenting, no survivors. This was measured within a single run on a dedicated
> Instance (`val-persist-em2`) with `/proc/<pid>/stat` state characters and PPIDs
> recorded at eight checkpoints from t=0 through t=30 min.
>
> **Two claims from earlier revisions are retracted:**
>
> 1. Revision 2's §3 stated that orphans "had become zombies (`<defunct>`) — their
>    parent was eventually reaped." This was observed across separate test runs at
>    unrecorded times. The within-run probe showed orphans remain in state **S**
>    (sleeping) for their entire 120 s lifetime and are never observed as **Z**
>    (zombie). They disappear when the wrapper's timeout fires, not by zombie reap.
> 2. An earlier draft of revision 3 claimed `runsc state` orphans had a "worse
>    persistence profile" than `runsc delete` orphans. The within-run probe showed
>    both types behave identically: state S throughout, gone simultaneously at
>    t=2m10s. There is no differential.
>
> Revision 3 also adds: §4b documenting `sandbox exec`'s `runsc state` orphans
> (one per exec call on a mid-delete sandbox, scaling 1:1 with probe count), and
> a **negative result** — `sandbox wait` shells out to `runsc wait`, which **exits
> cleanly** when the wrapper is killed. The hang is specific to `delete` and `state`,
> not general to the CLI.

---

## 1. Summary

`sandbox delete --force` does not return. It reaches network-namespace cleanup, raises
SIGTERM, then blocks. We observed 120 s and stopped waiting; exit was by our timeout,
not by the command completing. The underlying `runsc … delete --force` subprocess is
left behind.

This is **independent of what runs inside the sandbox** and **independent of whether an
exec is attached** — see the control matrix in §3.

Separately, plain `sandbox delete` (no `--force`) **refuses** — correctly and in 209 ms
— but appears to kill the sandbox anyway while leaving its gVisor processes running.
That is a second, distinct defect (§4).

A third defect: `sandbox exec` on a mid-delete sandbox orphans a `runsc state`
process per call (§4b). All orphans self-clean at 120 s via the CLI's internal
timeout (§4b).

## 2. Environment

| | |
|---|---|
| Platform | Cloud Run **Instances** (beta), deployed via Cloud Run v2 REST with `containers[0].sandboxLauncher: true` |
| Region / project | `us-east4` / `ptone-experiments` |
| Container image | `docker.io/library/python:3.11`, `tmux` installed at startup |
| `sandbox` binary | `/usr/local/gcp/bin/sandbox`, 55 MB, **dated Aug 4 2026**. Self-describes as "Serverless sandboxing CLI"; **no `--version` flag** |
| `runsc` | `/usr/local/gcp/bin/runsc`, 128 MB, dated Aug 4 2026. **version `google-958767651`, spec 1.2.1** |
| Sandbox invocation | `sandbox run <id> --detach --rootfs / --write --allow-egress -- <cmd>` |

## 3. The control matrix — this is the core of the report

| Case | Inside the sandbox | `--force` | Result | Time | Orphan |
|---|---|---|---|---|---|
| **T9** | tmux + interactive exec attached | yes | **HANG** | >90 s | `runsc delete` |
| **C1** | tmux, **no exec attached** | yes | **HANG** | **120 027 ms** (our timeout) | `runsc delete` |
| **C2** | **bare `sleep 3600`** | yes | **HANG** | **120 028 ms** (our timeout) | `runsc delete` |
| **C3** | tmux | **no** | **REFUSES**, then kills anyway | 209 ms | gofer + sandbox procs (§4) |

C1 removes the exec. C2 removes everything — no tmux, no shell, a single `sleep`. Both
hang identically, to within a millisecond of each other, which is our timeout firing
rather than any property of the workload.

### Output, identical across T9 / C1 / C2

```
Found network annotations for session <id>, cleaning up netns
E0000 ... Raising signal 15 with default behavior
    <blocks indefinitely>
```

and in the T9 case, eventually:

```
destroying container: stopping container: ... waiting sandbox stop: sandbox is still running
```

### The orphan

Identical argv in every `--force` case:

```
/usr/local/gcp/bin/runsc --platform=xemu --platform_device_path=/dev/xemu \
  --root=/tmp/runsc-root --ignore-cgroups --TESTONLY-unsafe-nonroot \
  --overlay2=root:memory --network=none delete --force <sandbox-id>
```

Two observations we'd flag to whoever picks this up:

1. **`--network=none` on the delete** — this is the delete operation's own network
   mode, not the sandbox's. The sandboxes were created `--allow-egress`, which maps to
   `--network=host`. We don't know whether that mismatch is related to the hang, but it
   is the one thing in the argv that looks surprising given the preceding log line is
   about netns cleanup.
2. **`--TESTONLY-unsafe-nonroot`** is present in the production argv. Presumably
   deliberate; noting it because the name invites a double-take.

### The good news, and it matters for our workaround

- **Orphans do not persist indefinitely.** The `sandbox` CLI has a 120 s internal
  timeout; on expiry it exits rc=1 and takes its `runsc` child with it. See §4b for the
  measured timeline. *(Revision 2 reported that orphans "had become zombies (`<defunct>`)
  — their parent was eventually reaped." That was observed across separate test runs at
  unrecorded times and was not reproducible in a within-run probe; see the revision 3
  change note above.)*
- **The delete is effective despite not returning.** The sandboxes really are gone:
  `sandbox exec` on them reports "not running" / "no control socket found".

So the `--force` hang appears to be **a reporting/termination failure rather than a
state leak**. That is a much better failure mode than we assumed in revision 1, and it
is why our workaround (§5) is viable.

## 4. Second, distinct defect: plain `delete` refuses but kills

```
$ sandbox delete c3-test          # no --force
Found network annotations for session c3-test, cleaning up netns
cannot delete container that is not stopped without --force flag
Error: cmd.Wait(delete) failed: exit status 128
                                                    # 209 ms, exit 1
```

The refusal is correct and fast. But afterwards:

- `sandbox exec c3-test` → **"sandbox c3-test is not running"**
- and yet `runsc-gofer` and `runsc-sandbox` for `c3-test` are **still running** — one
  of them at 19.3 % CPU. These are the original sandbox runtime processes, not delete
  orphans.

So a command that reported failure has left the sandbox dead to the CLI and alive to
the host. **A caller who correctly handles the error and retries will be operating on a
sandbox the CLI has already disowned.** We think this is worse than the hang, because
the hang at least announces itself.

## 4b. The `sandbox` CLI has a 120 s internal timeout; orphans self-clean

**Headline finding:** the `sandbox` CLI wrapper exits rc=1 after **120 s** and takes
its `runsc` child process with it. Both `runsc delete` orphans (§3) and `runsc state`
orphans (see below) behave identically: state **S** (sleeping) throughout their
lifetime, gone simultaneously when the wrapper's timeout fires. No zombies, no
reparenting to PID 1, no late reappearances through t=30 min.

### Measured lifecycle — within-run, with PPID tracking

*(Source: `probe_persistence_v4.py`, dedicated Instance `val-persist-em2`,
2026-08-26 03:48–04:19 UTC. `runsc google-958767651`, spec 1.2.1. All data
is from a single run — no cross-run comparisons.)*

One sandbox created, `delete --force` issued, then 3 `sandbox exec` calls issued on
the mid-delete sandbox (creating 3 `runsc state` orphans). `/proc/<pid>/stat` field 3
(state) and field 4 (PPID) recorded at each checkpoint:

| time | `runsc delete` (PID 111) | `runsc state` ×3 (PIDs 124, 137, 150) | wrappers |
|---|---|---|---|
| t = 0 (8 s) | **S**, ppid=103 | all **S**, ppid=116/129/142 | all alive |
| t = 1 min | **S**, ppid=103 | all **S**, ppid=116/129/142 | all alive |
| t = 2 min | **S**, ppid=103 | all **S**, ppid=116/129/142 | all alive |
| t = 2 min 0.5 s | — | — | **all exit rc=1** (120 s timeout) |
| t = 2 min 10 s | **gone** | **all gone** | dead |
| t = 3 min | gone | gone | dead |
| t = 5 min | gone | gone | dead |
| t = 10 min | gone | gone | dead |
| t = 30 min | gone | gone | dead |

The `sandbox` CLI wrappers are reparented to PID 1 (their Python parent does not wait
on them). The `runsc` children maintain their wrapper as parent throughout. When the
wrapper's 120 s timeout fires, all children exit — no orphan survives the wrapper.

This is still a defect. A two-minute per-operation wedge — four sleeping processes per
`delete --force`, more if anything calls `sandbox exec` during that window — is real
resource consumption at fleet scale. The self-cleaning bounds the damage; it does not
make the hang acceptable.

### `runsc state` orphans — exec probes on mid-delete sandboxes

`sandbox exec` internally shells out to `runsc state` to check sandbox liveness.
When exec is called on a sandbox whose delete is in-flight, the `runsc state`
subprocess hangs — one orphan per exec call, scaling linearly:

*(Source: `delete_timeout_validation_v4.py`, Instance `val-delete-2`,
2026-08-26 02:42–02:45 UTC)*

| `sandbox exec` probes issued during delete | `runsc state` orphans found |
|---|---|
| 0 (no polling) | **0** |
| 4 | **4** |
| 7 | **7** |

Without exec polling, only the `runsc delete` orphan (§3) appears. These `runsc state`
orphans share the exact same lifecycle as the `runsc delete` orphan — state S for 120 s,
then gone when the wrapper exits.

### Negative result: `runsc wait` does NOT hang

`sandbox wait` shells out to `runsc wait` (not `runsc state`). When the `sandbox wait`
wrapper is killed — simulating context cancellation, which is the production path on
every delete — the `runsc wait` child **exits cleanly**, gone from `/proc` within 2 s.

*(Source: `delete_timeout_validation_v5.py`, Instance `val-delete-2`,
2026-08-26 02:50–02:55 UTC)*

| Condition | `runsc` orphans after delete |
|---|---|
| Delete without watcher | 1 (`runsc delete`) |
| Delete with watcher killed before delete | 1 (`runsc delete`) |
| Delete with watcher killed simultaneously | 1 (`runsc delete`) |

Watcher cancellation adds zero extra orphans. **The hang is specific to certain `runsc`
subcommands (`delete`, `state`), not general to the CLI.** `runsc wait` exits on signal;
`runsc delete` and `runsc state` do not. This is useful diagnostic information for
whoever picks up the upstream defect.

## 5. Why this matters to us, and what we're doing about it

We are designing a single-node Scion tier where a Cloud Run Instance hosts the control
plane and each agent runs in its own sandbox. The tier is **pure-ephemeral: redeploy is
the normal lifecycle**, and a redeploy tears down *every* sandbox at once.

- A ≥120 s non-returning call per sandbox is not a teardown path at fleet scale.
- We cannot distinguish "slow" from "wedged" from the CLI's behaviour, so we cannot
  report accurate agent lifecycle state during shutdown.
- The non-`--force` path can't be used as a polite first attempt, because of §4.

**Our workaround**, now that we know deletion is effective: issue `--force`, bound it
with a timeout, treat the timeout as success, and reap the orphan rather than waiting
on it. We'd rather not carry this, and we're picking the timeout value blind.

**Update (revision 3):** The 120 s CLI timeout (§4b) means our 10 s workaround timeout
abandons a wrapper that self-terminates 110 s later and reaps its own children. Our
orphan reaper (`reapOrphanedRunsc`) is now understood to be **belt-and-braces, not
load-bearing** — the orphans it targets have a 120 s natural TTL. We retain it because
a TTL measured on one build is not a contract, and `DefaultDeleteTimeout` of 10 s still
provides a 10× margin over the observed 1 s effectiveness time under 5-way fan-out.

## 6. What would help us most

1. Is this known, and is `--force` expected to be bounded? By what?
2. §4 — is "refuse but kill" intended? If not, it's the more dangerous of the two.
3. Is treating a `--force` timeout as success actually safe, or are there cases where
   the sandbox survives the hang?
4. Is there a supported way to delete many sandboxes at once? That's our real access
   pattern and doing it serially at 120 s each is untenable.
5. Is the `runsc state` hang (which `sandbox exec` triggers internally, §4b) the same
   root cause as the `runsc delete --force` hang, or a separate defect? Both self-clean
   at 120 s via the wrapper's timeout, but the hang itself is the issue.
6. The `sandbox` CLI appears to have a 120 s internal timeout (§4b) that is
   undocumented. Is this intentional? Is it a contract we can rely on, or an
   implementation detail that may change?

## 7. Provenance and limits

Produced during Tier B of an empirical test plan run against a real Cloud Run Sandbox
on a real Cloud Run Instance — deliberately not `unshare`, not local Docker, and not a
self-installed `runsc`, all rejected as substitutes after an earlier
substitute-mechanism result in this project produced a confident wrong answer.

**Limits worth stating:** single image (`python:3.11`), single day, single `runsc` build
(`google-958767651`). The core matrix (T9, C1–C3) used `val-delete-2`; the within-run
persistence probe used a dedicated Instance (`val-persist-em2`) to avoid measurement
interference. The 120 s CLI timeout is empirical — we have no documentation confirming
it is intentional, and it may vary across builds. Concurrent 5-way fan-out was tested
for effectiveness (all sandboxes unreachable at t≤1 s) and timeout independence (all
timed out in parallel, not serially).

Raw output and full Tier B results: `ac0-results.md` (tests **T9**, **C1–C3**).
Persistence probe results: `persistence-probe-results.md`. Design consequences tracked
as phase **P4a** in `cloudrun-instances-sandboxes.md`.
