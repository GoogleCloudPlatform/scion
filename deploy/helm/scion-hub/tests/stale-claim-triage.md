# Stale-claim triage — Phase 0

**This file changes no prose. It is a classification table and a catalogue.**
Produced under gd-em's ruling of 07:40, which supersedes the 07:39 instruction
that had asked for rewrites: *"Commit A's triage produces a CLASSIFICATION TABLE
AND ZERO PROSE EDITS."* The prose edits are a separate commit with their own
reviewer, sized by how many sites come back descriptive. That number is below.

Measured at `60b2912`, helm v3.16.3+gcfd0749 (`/tmp/linux-amd64/helm`),
kubeconform v0.6.7 (`/tmp/kubeconform`), non-login shell.

---

## What is being classified, and why the boundary is drawn here

The defect class: **a claim about behaviour that is true when written and false
after a later phase lands, with nothing that fails when it turns.** Fifteen
instances have been identified in this subtree. Every one was found by a person
reading prose; none by a check.

Scope is gd-em's ruling: the union of the hedge sweep and the subject sweep,
**restricted to sites naming a subject in Phase 1's render delta** — ConfigMap,
Secret, env, envFrom, volumes, `settings.yaml`, `SCION_SERVER_BASE_URL`,
base-url, hub-id delivery. Sites whose subject belongs to a later phase are
**catalogued, not triaged**, and are listed in §4 for that phase's brief.

The reasoning is the same axis-(d) test that put `DELIVERS_BASE_URL_CHANNEL`
into Commit A and kept `.helmignore`'s `golden/` and `hack/` out: **act on the
transition that is next; catalogue the rest.** A claim about Phase 4's ingress
does not rot until Phase 4, and triaging it now means triaging it against a
render nobody has written.

### Mood taxonomy (gd-p0-rev-2's, adopted verbatim)

| mood | test | disposition |
|---|---|---|
| **descriptive** | asserts the current world; a render can falsify it | **the class.** Fix in the prose commit. |
| **quotation** | reproduces a claim, usually to refute it | leave alone. Editing it damages the warning. |
| **normative** | instructs a future phase | leave alone. No truth value to go stale. |

---

## 1. Counts

| | sites |
|---|---|
| raw candidate lines, both sweeps, P1-delta subjects | 81 |
| — of which code, not prose (list literals, `fail` bodies, field names) | 57 |
| **prose claim sites triaged** | **24** |
| **descriptive** | **9** |
| quotation | 6 |
| normative | 9 |

**Nine descriptive sites is the size of the prose commit.** Seven were already
filed as instances eleven to fifteen plus `:901`; **two are new and are filed
here for the first time** (§3).

The 81 → 24 reduction is the reason gd-em's resize was correct: 37 sites was
never 37 instances. It is also the reason the triage could not be mechanised —
distinguishing a claim from a quotation of a claim requires reading the
paragraph that contains it.

---

## 2. Descriptive — the class. All nine.

All are **true at `60b2912`** and false at `11a78701`. None reopens Phase 0.
`gd-p1-dev` owns the fixes; they land with the ConfigMap.

| # | site | claim | falsified by |
|---|---|---|---|
| 11 | `_helpers.tpl:1089` | "This chart delivers none of them yet: today the flag would simply take effect" | ConfigMap + `SCION_SERVER_BASE_URL` |
| 12 | `deployment.yaml:65-67` | "Nothing in this chart yet feeds hub.hubId into the running process... until then the hub's actual ID is still hostname-derived" | `settings.yaml` carries `hub_id` |
| 13 | `_helpers.tpl:829-835` | "fully LIVE on argv today... no second source yet" | `SCION_SERVER_BASE_URL` |
| 14 | `_helpers.tpl:637-638` | "This chart delivers no ConfigMap and no Secret" | both land |
| 15 | `_helpers.tpl:827` | "the chart renders no ConfigMap, no Secret, no env, no envFrom and no volumes" | all five land |
| — | `_helpers.tpl:899-901` | "argv value silently outranks the Secret-backed environment variable a later phase mounts" | the Secret lands |
| **16** | **`_helpers.tpl:310`** | **"this chart mounts no volumes and renders no --db"** | **P1 mounts the settings volume** |
| **17** | **`NOTES.txt:72-79`** | **"It renders no server configuration"** | **P1 renders exactly that** |
| **17b** | **`NOTES.txt:75-76`** | **"It does write a settings.yaml for itself on first boot... that file carries no server section"** | **P1's mounted file has one** |

Sixteen, seventeen and seventeen-b are new. They are analysed in §3 because
each is a *different* failure of the two sweeps, not three more of the same.

---

## 3. The two new findings, and why both sweeps missed them

### Sixteen — `_helpers.tpl:310`: **a claim that names the phase which will falsify it, and names the wrong one**

> "this chart mounts no volumes and renders no --db"

Eight lines below, the same paragraph says:

> "LATER PHASES FALSIFY THAT ON PURPOSE, WHICH IS WHY IT IS WRITTEN DOWN RATHER
> THAN LEFT AS AN ABSENCE. The Cloud SQL phase sets the postgres driver and
> turns isHADeployment true; **the Filestore phase lands the shared volumes.**"

This is the most carefully written prose in the file on exactly this hazard. The
author anticipated the transition, wrote it down, and **attributed it to the
wrong phase**: Phase 1 mounts a volume for `settings.yaml`, several phases
before Filestore. So the claim ages out at a boundary its own disclaimer does
not cover.

That is a distinct sub-mood and it defeats both detectors by construction. A
hedge sweep sees a hedge and a reader confirms the hedge is handled. A subject
sweep sees `volumes` and a reader finds the subject already discussed. **The
paragraph looks triaged because it *is* triaged — against the wrong boundary.**

Severity is *lower* than instances eleven to fifteen, and this matters for
sequencing: the inference the paragraph draws — "replicas share NO mutable
state, so `isHADeployment` is false" — **survives**, because a read-only
projected ConfigMap volume is not shared mutable state. Only the literal clause
goes false. A reviewer skimming for consequences would correctly conclude
nothing breaks, and would leave a false sentence in place under a heading that
says later phases falsify it on purpose.

> **A CLAIM CAN BE PROTECTED BY A DISCLAIMER THAT NAMES THE WRONG PHASE, AND
> THAT IS WORSE THAN AN UNPROTECTED CLAIM, BECAUSE THE DISCLAIMER IS WHAT STOPS
> THE NEXT READER LOOKING.**

### Seventeen — `NOTES.txt:72-79`: **the only prose in the chart the operator actually sees**

> "WHAT THIS RELEASE DOES NOT YET DO ... It renders no server configuration, so
> the hub falls back to its own defaults - SQLite and local workspace storage."

Every instance filed so far lives in `_helpers.tpl` or `deployment.yaml` —
files an operator never reads. `NOTES.txt` is **printed on every `helm install`
and every `helm upgrade`**. When this goes stale, the chart tells the operator
at the console that it renders no server configuration while mounting one.

All four reviewers, myself included, swept `templates/` and reported findings
only from `_helpers.tpl`. The sweeps' file globs did include `NOTES.txt`; the
attention did not. I cannot attribute that to the instruments — **this one is
not an instrument gap, it is that we were all reading the file we had been
arguing about.**

`17b` is inside the same paragraph and is a *second, independent* claim: the
parenthetical about the hub writing its own `settings.yaml` with no server
section. It needs its own edit; fixing the sentence above it does not touch it.

---

## 4. Catalogue — later-phase subjects, NOT triaged

Recorded so the next phase does not rediscover them. Each is true at `60b2912`.

| site | subject | phase that falsifies it |
|---|---|---|
| `_helpers.tpl:322` | shared volumes, `isHADeployment` | Filestore |
| `_helpers.tpl:320-321` | postgres driver | Cloud SQL |
| `NOTES.txt:78-79` | Cloud SQL, GCS, Filestore, session secret, Ingress, IAP | 2, 3, 4, 5 |
| `NOTES.txt:81-82` | "images published from this repository today run as root" | the `hub-gke` image |
| `_helpers.tpl:851-852` | "destined for the settings file" / `SCION_SERVER_BASE_URL` | P1, but **normative** — see §5 |
| `_helpers.tpl:854-868` | base-url precedence order | P1 changes which branch is reachable |

`_helpers.tpl:854-868` is the one to watch: it is a *precedence table* read from
the hub's source, and P1 does not falsify any row of it. It changes **which row
applies**. A claim that stays true while becoming irrelevant is not covered by
any instrument discussed so far, and I do not have a proposal for it.

---

## 5. Quotation and normative — counted, left alone

**Quotation (6).** `_helpers.tpl:726-745` supplies five of them: an enumerated
list of readings the header labels *"ASSERTED CONFIDENTLY AND WRONGLY, ALL IN
ONE DAY"*, including `:730` *"the chart mounts nothing, so no settings file
exists"*. These are correct **because** they are quotations of incorrect claims.
gd-em has ruled `726-745` off limits to any edit pass. The sixth is `:826`,
which quotes a previous version of its own paragraph in order to retract it.

**Normative (9).** `:823` (a heading addressed to later phases), `:864`,
`:871-872` (*"a later phase may still choose argv... move the entry, do not add
a second emitter"*), `:851-852`, and five shorter directives. These instruct a
future maintainer. No render can falsify an instruction; attaching a state
number to one would be attaching a tripwire to a policy.

---

## 6. What this triage does not establish

- **It is not a coverage claim.** 24 sites is what two sweeps plus one reading
  found. gd-p0-rev-3 has withdrawn the implication that any sweep's site count
  bounds the class: *"the class is larger than the detector that found it and I
  cannot bound it."* Finding sixteen and seventeen after four rounds of review
  is direct evidence for that.
- **The seed list for Commit B's detector is §2's nine sites**, per gd-em: the
  seed list is the output of the triage, not four examples chosen in advance.
  Four seeds against thirty-seven sites pins four.
- **Sixteen is a counter-example to seeding by site.** A detector seeded with
  `:310` would find `:310`. It would not find the next paragraph that handles
  its transition and misnames the phase, because what is wrong there is a
  *relation between two sentences*, and neither sentence is individually
  suspicious.
- **The mood classification is mine and is unreviewed.** Nine of the twenty-four
  are judgement calls between normative and descriptive — chiefly `:851-852`,
  which reads as a statement about the future but functions as an instruction.
  I have classified it normative. A reviewer who disagrees moves it into the
  prose commit's scope, which is the direction that costs work rather than
  safety, so the ambiguity is disclosed rather than resolved.
