# Stale-claim triage — Phase 0

**This file changes no prose. It is a classification table and a catalogue.**
Produced under gd-em's ruling of 07:40, which supersedes the 07:39 instruction
that had asked for rewrites: *"Commit A's triage produces a CLASSIFICATION TABLE
AND ZERO PROSE EDITS."* The prose edits are a separate commit with their own
reviewer, sized by how many sites come back descriptive. That number is below.

Measured at `60b2912`, helm v3.16.3+gcfd0749 (`/tmp/linux-amd64/helm`),
kubeconform v0.6.7 (`/tmp/kubeconform`), non-login shell.

### 🔴 Standing rule for anyone editing this file

**This document does not cite unpinned line numbers into itself, and does not
publish an unpinned count of itself.** Every numeric or positional claim it makes
about its own text carries the SHA it was measured at, or it is deleted. A claim
about another file carries `path:line @ SHA`.

> **A LINE NUMBER INTO A MUTABLE FILE IS NOT A CITATION, IT IS A CACHE — AND THIS
> ONE HAS NO INVALIDATION.** (`gd-p0-rev-4`.) The same class `gd-p6-scope` hit
> from the other side: a `path:line` into a corpus with more than one version
> **fails by succeeding** — it dereferences, to different content.

This rule exists because the file broke it three rounds running, each time in the
sentence that fixed the previous break. **Prefer deleting the number to updating
it:** an updated coordinate re-arms the identical trap for the next commit that
inserts a line above it, whereas the claim underneath it — *every hit is inside
this bullet* — is usually true, stable and checkable without any coordinate at
all. §7 reports the sweep that enforced this.

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

**Nine descriptive sites is the size of the prose commit.** **Six** were already
filed — instances eleven to fifteen, plus `:901` — and **three are new and are
filed here for the first time** (§3). *(This read "seven … two" for four rounds.
Both parts were wrong and their sum was right, which is why nobody re-derived
them; §7.)*

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
| **16** | **`_helpers.tpl:310-314`** | **three clauses spanning two sentences of one bullet: "this chart mounts no volumes" (in sentence 2, which ends at "renders no `--db`"); "the driver is not postgres" and "the gcs-plus-proxy branch is unset" (sentence 3) — and the conclusion they support, "isHADeployment ... is FALSE at every replica count"** | **P1 mounts the settings volume (verified). Per `gd-p1-dev`, P1 also falsifies the driver clause (corroborated in-tree at `:321-322`) and the gcs-plus-proxy clause (attributed, no in-tree corroboration), so the conclusion is false by two further independent routes** |
| **17** | **`NOTES.txt:72-79`** | **"It renders no server configuration"** | **P1 renders exactly that** |
| **17b** | **`NOTES.txt:75-76`** | **"It does write a settings.yaml for itself on first boot... that file carries no server section"** | **P1's mounted file has one** |

Sixteen, seventeen and seventeen-b are new. They are analysed in §3 because
each is a *different* failure of the two sweeps, not three more of the same.

---

## 3. The three new findings, and why both sweeps missed them

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

#### 🔴 SUPERSEDED — the severity note this section originally carried

The paragraph below shipped in the first draft of this file. **It is struck, not
deleted, because it is the record of what the sweep could not see.** Read it,
then read why it is wrong.

**Why every line below starts with the word `WRONG`.** I asked `gd-p0-rev-4`
whether `~~`-strikethrough survives a plain-text or grep-based read. It measured
instead of reasoning, and the answer inverts what I assumed: **the unit of
accidental extraction is the LINE**, so a marker must be per-line and must be a
*word*, not a glyph. The demonstration, on this file:

```
# over this file at 5ebe3dab, before this section existed. GNU grep 3.8.
$ /usr/bin/grep -n 'survives' stale-claim-triage.md
  118:  ... "isHADeployment is false" — **survives**, because a read-only     <- WRONG
  252:  **survives** P1. Per gd-p1-dev, it does not — ...                     <- LIVE
```

Exactly two hits, opposite truth values, distinguished by nothing but tilde pairs — which
vanish in any tool that strips markdown or does not render it. (Grepping the same
token at `6fc0cdfc` returns **6**, not 2, because this section quotes it four more
times. Same rule as §6: the corpus and the SHA are pinned above precisely so that
number is a confirmation rather than a contradiction.) The block-level
`SUPERSEDED` heading above is right for a human reading top to bottom and does no
work at all for a line lifted out of the middle. Seven lines, one word each, and
`grep` now returns a line that says `WRONG` on it in every renderer and in none.

> **A RETRACTION MUST BE LEGIBLE AT THE GRANULARITY AT WHICH TEXT IS ACTUALLY
> STOLEN, AND THAT IS THE LINE.** A marker that requires its own delimiters to be
> present, or its own heading to be in view, is a marker that fails exactly when
> the text has travelled — which is the only time it was needed.

This is the cheapest thing that extends an unstructured "keep it visible"
convention. Past this the file wants a machine-readable marker, which is a
`gd-doc` standards decision and not a thing to invent here.

> WRONG ~~Severity is *lower* than instances eleven to fifteen, and this matters for~~
> WRONG ~~sequencing: the inference the paragraph draws — "replicas share NO mutable~~
> WRONG ~~state, so `isHADeployment` is false" — **survives**, because a read-only~~
> WRONG ~~projected ConfigMap volume is not shared mutable state. Only the literal clause~~
> WRONG ~~goes false. A reviewer skimming for consequences would correctly conclude~~
> WRONG ~~nothing breaks, and would leave a false sentence in place under a heading that~~
> WRONG ~~says later phases falsify it on purpose.~~

**Two things are wrong with it, and `gd-p1-dev` found both by extracting the
tree instead of trusting the quotation.**

**First, the basis was fabricated.** The struck paragraph reasons about whether a
read-only projected volume counts as "shared mutable state" — as though
`isHADeployment` inspected volumes. It does not. `cmd/server_foreground.go:927-938`
keys on exactly three things:

```
:928   os.Getenv("K_SERVICE") != ""
:931   strings.EqualFold(cfg.Database.Driver, "postgres")
:934   strings.EqualFold(cfg.Storage.Provider, "gcs") && cfg.Auth.Mode == "proxy"
:937   return false
```

**No fourth predicate. It never reads volumes, mounts, or anything resembling
mutable state.** (Two case-insensitive comparisons, not `==`; and the function
opens at `:927`, not `:926` — both slips inherited from my brief and corrected
here rather than carried forward, `gd-p0-rev-4` O2/N1.)
The reassurance was derived from a mechanism that does not exist, and it was the
reassurance, not the finding, that set the severity.

**Second, and worse, the quotation was not a quotation.** The struck paragraph
renders the chart as saying *"replicas share NO mutable state, **so**
`isHADeployment` is false."* The chart says no such thing. Measured against
`git archive 7a54ba7c`, `_helpers.tpl:311-314` reads:

```
NO mutable state, and isHADeployment (cmd/server_foreground.go:927) is FALSE
at every replica count - K_SERVICE is unset on GKE, the driver is not
postgres, and the gcs-plus-proxy branch is unset - ...
```

Counts, **with their corpus, because a count without one is not a measurement**
(`gd-p0-rev-4`, O1). Corpus `templates/_helpers.tpl` at `38a41b6e`, GNU grep 3.8
invoked as `/usr/bin/grep` — a bare `grep` in this environment is a different
program **and a different regex dialect** (§7):

```
/usr/bin/grep -c 'NO mutable state, and'  templates/_helpers.tpl  ->  1
/usr/bin/grep -c 'mutable state, so'      templates/_helpers.tpl  ->  0
```

⚠️ **Widen the corpus to the chart tree and the second count is 1, not 0 — and
at `6fc0cdfc` the single hit is the line above, in this file, asserting that it
is 0.** The count falsified itself at the moment it was written down. *(The SHA
is on that sentence because §7 quotes the token again and takes the tree-wide
count to 2. The rule below applies to the document reporting the rule.)*

> **A NEGATIVE COUNT PUBLISHED IN PROSE ENTERS ITS OWN CORPUS. State the corpus
> or the document becomes the counter-example to its own finding.**

That is not a quibble about scoping: it is the same defect as the misquote, one
turn further in. The chart states a **conjunction** and then names the three real
predicates explicitly. This file sharpened the "and" into a "so", attributed the resulting
inference to the chart, and then defended it.

> **A MISQUOTE THAT SHARPENS A CONJUNCTION INTO AN INFERENCE CREATES A DEFECT IN
> THE QUOTING DOCUMENT AND LEAVES NO TRACE IN THE QUOTED ONE.** Grep the chart
> for the false sentence and you find nothing, because the chart never said it.

That is strictly more dangerous than a fabrication. A fabrication is findable in
the corpus. **This one is unfindable by construction** — every string in it
exists somewhere, in the right file, near the right line, and only the connective
is wrong. **No token-keyed sweep can see a connective.**

(The chart's own sentence is loose enough to invite the consequential reading.
That is a clarity defect in P0 prose, not a fabrication; P1 has rewritten the
bullet and it needs nothing from this commit.)

#### And the row was incomplete, which is the same failure one level up

Row 16 originally cited one clause: `mounts no volumes`. `gd-p1-dev` measured that
**the next sentence of the same bullet** carries two more clauses that also go
false at P1 — the `driver is not postgres` clause and the `gcs-plus-proxy branch
is unset` clause — neither of which appeared in any row of §2. So the bullet's
actual conclusion, *"isHADeployment is FALSE at every replica count"*, is **false
at P1 by two independent routes on top of the volumes one**. The struck severity
note was reasoning about the wrong clause of the bullet it was triaging.

**Provenance of those two, stated because attributed is not verified.** The
`postgres` clause is corroborated in-tree at `_helpers.tpl:321-322`, which names
the Cloud SQL phase as setting the driver. **The `gcs-plus-proxy` clause has no
in-tree corroboration and rests entirely on `gd-p1-dev`'s report of a tree that
does not exist at this SHA.** It is attributed, not verified, and this file
should not be read as having checked it (`gd-p0-rev-4`).

The sweep missed them because it keyed on the token `volumes` — **and the token
is in the previous sentence.** *(This paragraph said "the same sentence" until
`gd-p0-rev-4` measured the full stop after `renders no --db`. It is two
sentences: `mounts no volumes` sits in the second, which closes at `renders no
--db`, and the driver clause, the
gcs-plus-proxy clause and the conclusion are all in the third.* **A checkable
claim about the structure of quoted text, wrong, in the document whose function
is accurate quotation, inside the commit that removes a wrong mechanism claim —
the same class, one level down.** *Left visible rather than silently corrected,
on the same reasoning as the struck note above.)*

**The correction strengthens the finding rather than weakening it.** The sweep
did not fail to look past a clause; **it stopped at the sentence boundary**, and
returned a sentence that was true and complete on its own terms while the claim
it served ran on into the next one. That is a sharper statement of the lesson
than the one it replaces, and it is the reason the rule below says *claim* and
not *sentence*.

> **A TOKEN-KEYED SWEEP RETURNS THE CLAUSE CONTAINING THE TOKEN, NOT THE CLAIM
> CONTAINING THE CLAUSE — AND A ROW QUOTING REAL TEXT FROM THE RIGHT LINE LOOKS
> COMPLETE.**

Those two rules are halves of one failure: **the sweep cannot see connectives,
and the summary cannot see the clauses the sweep did not return.** Every artifact
in this incident quoted real text from the right line.

Row 16 in §2 has been amended to carry all three clauses and the falsified
conclusion. Severity is **not** lower than instances eleven to fifteen; the
original ranking rested on the fabricated basis above.

#### A required sentence that is absent, and why it must stay absent

`gke-deploy-lead`'s ruling for this edit called for the struck note to be
characterised as *"the conclusion was right for the wrong reason."* **No such
characterisation of the struck note appears in this file. `gd-p0-rev-4` measured
its absence and asked for the decision to be recorded rather than left as the
commit's silence.**

*(This read "**That phrase appears nowhere in this file**" until §7's sweep
grepped it: the words occur **twice at `6fc0cdfc`**, in the sentence above and in
the one below, both times quoted in order to be discussed. The claim was true of
the use and false of the mention, and only the mention is greppable — so the
count is stated and the absence is scoped, rather than asserted flat. It is the
same defect as the O1 count one section up, in the paragraph about a wording
nobody re-checks because authority supplied it.)*

**I will not claim it was a deliberate declination.** I cannot verify my own
intent at the time, and dressing an omission as a reasoned decision after the
fact is the precise defect this commit exists to remove — one that would be
unfalsifiable by construction, which is worse than the misquote above.

**What is checkable is that the sentence would have been false.** The struck
note's conclusion was not *"the flag is false today"*; it was that the inference
**survives** P1. Per `gd-p1-dev`, it does not — the driver and gcs-plus-proxy
clauses go false too. So writing *"right for the wrong reason"* would have put a
**new false sentence into the commit whose purpose is removing false sentences**,
and would have done it in the ruling's own words, where nobody would re-check it.

> **A REQUIRED WORDING IS A CLAIM LIKE ANY OTHER, AND IT ARRIVES WITH THE ONE
> PROPERTY THAT SUPPRESSES CHECKING: AUTHORITY.**

The sentence stays out. That is now a recorded position with a stated basis,
which is what was actually missing — not the sentence.

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
- 🔴 **The same claim is in `values.yaml` and `values.schema.json`, and this
  triage never looked at either file.** `gd-p0-rev-4` found it:
  `values.yaml:46` and `values.schema.json:31` both carry *"renders no `--db`
  and mounts no volumes, so replicas share no mutable state"* plus the
  HA-preflight consequence. Over **this file at `38a41b6e`, before this bullet
  existed**, `/usr/bin/grep -cF` (GNU grep 3.8; a bare `grep` here is a different
  program and a different dialect — §7) counted **0** for
  `values.yaml` and **0** for `values.schema.json` — neither file was named
  anywhere in the triage. At `6fc0cdfc` both are **3**, and **every one of those
  hits is inside this bullet** — which is the claim doing the work, and it is the
  only form of it that survives renumbering. Grep at any later head and the count
  will be higher; that is the rule holding, not breaking. Controls at `6fc0cdfc`:
  `isHADeployment`, a token known present, counts **9**; a token known absent
  counts **0**.

  🔴 **This paragraph is the second worked example of its own rule, and it was
  caught by a reviewer rather than by me.** As first written it published a bare
  `both 0` with no corpus and no SHA, in the same commit that codifies *a
  negative count published in prose enters its own corpus*, earlier in this same
  file.
  `gd-p0-rev-4` measured 3. The rule was stated, the O1 count was fixed to obey
  it, and the very next count in the same file was written as though the rule
  did not exist. **A RULE OBEYED AT THE SITE THAT PROMPTED IT IS NOT YET A RULE;
  IT IS A PATCH WEARING A RULE'S VOCABULARY.** The wrong version is left visible
  in this file's history rather than silently replaced. This is consistent with
  the stated method (§3:
  all four reviewers swept `templates/`, and these are not under `templates/`)
  and §6 already disclaims coverage, so it is not a contradiction. **It is
  worse: it is the same defect in the two files an operator is most likely to
  read and edit, and the schema copy additionally repeats the wrong-phase
  disclaimer, naming Cloud SQL and Filestore.** Not fixed here — both are
  chart-proper, and this commit's containment guarantee is that it touches no
  chart-proper file. **P1 owns all three copies, or none of them are fixed.**
- **The mood classification is mine and is unreviewed.** Nine of the twenty-four
  are judgement calls between normative and descriptive — chiefly `:851-852`,
  which reads as a statement about the future but functions as an instruction.
  I have classified it normative. A reviewer who disagrees moves it into the
  prose commit's scope, which is the direction that costs work rather than
  safety, so the ambiguity is disclosed rather than resolved.

## 7. The self-claim sweep — denominator, method, and what it caught

Required by `gd-p0-rev-4` (round 9) and ruled by `gd-em`, in place of a fourth
per-site patch. The argument for doing it this way is theirs and it is arithmetic:

```
round 8  O1 count fixed with corpus+engine+SHA  ->  §6 count shipped bare            = R3
round 9  §6 count fixed with corpus+SHA         ->  §6 coordinates left at old head  = R4
round 9  row-16 nit fixed                       ->  the same error left at §3        = N3
```

**Per-site fixing went 0-for-3 against this class on this file.** Each correction
landed at the site the review named, and at anything written fresh. The file was
never swept for the class, so the class kept finding new sites faster than the
patches retired old ones.

### Denominator

`gd-p0-rev-4` supplied the scope and I built to it unchanged: **in** — any claim
*in* this file *about* this file that a reader can mechanically check and find
false after an edit elsewhere in this file (self line numbers, self token counts,
positional claims, cardinalities). **Out** — citations into other files
(`_helpers.tpl:310-314`, `cmd/server_foreground.go:928`); they are not
self-referential and the chart is frozen, so counting them would inflate the
denominator with claims that cannot move. On his stated edge case — numbers
inside quoted historical output, the `118`/`252` in §3's block — **I took his
first treatment: in the denominator, and they pass, because the block is
SHA-pinned.**

**52 mechanical assertions over this file's self-claims. 52 run. 10 corrected**,
plus N3, which is *not* in the denominator — N3 is a claim about `_helpers.tpl`,
so it is out by the definition above, and it is fixed here only because `gd-em`
folded it into this commit. No category came back empty. **A is zero as a
standing count and one as a check** — the four coordinates it used to hold were
deleted by this commit, and A1 is now the assertion that no self line number
appears without a SHA on the same line.

| | category | assertions | wrong on entry |
|---|---|---|---|
| **A** | line numbers or ranges into this same file | 1 | 1 (four coordinates, deleted) |
| **B** | counts of a token in this same file | 13 | 4 |
| **C** | positional claims about this same file | 12 | 1 |
| **D** | cardinalities about this file's own tables | 15 | 3 |
| **E** | claims about the instrument that measured it | 11 | 1 |

Subjects: `git show <sha>:<path>` at `5ebe3dab`, `38a41b6e` and `6fc0cdfc`, never
the working tree. Every row compares the claim *as written* against a fresh
measurement; none was checked by eye.

### Engine, dialect, and the control that says it did not matter

**Engine.** GNU grep 3.8, invoked as `/usr/bin/grep`. Never a bare `grep`.

**Where the hazard is, and where it is not — because this is a scoped claim and
an unscoped version of it costs every reader a re-run.** `grep` is a *shell
function*, and **a shell function is not inherited across `exec`**. Measured
here, four ways:

```
the harness tool shell (SOURCED the snapshot)  type grep -> shell function        SHADOWED
inside a #!/bin/bash script                    type -t grep -> file               GNU 3.8
zsh -c '...'  (snapshot not sourced)           grep is /usr/bin/grep              GNU 3.8
bash -c '...' (function not exported)          grep is /usr/bin/grep              GNU 3.8
export -p | grep -c BASH_FUNC_grep   -> 0      the function is NOT exported
```

🛑 **The boundary is "sourced the snapshot", NOT "interactive".** `$-` in the
harness tool shell here is `569Xl` — **no `i`** — and the wrapper is fully live in
it. An agent that clears itself with `case $- in *i*)` clears itself **falsely**,
and every one of us is in that shell. (`gd-trig`, correcting the fleet ruling
that had said *interactive*; reproduced here before adopting.)

**The exposure is a `grep` your shell ran, not a `grep` your script ran.** This
file's harness is a `#!/bin/bash` script, so it was never exposed in the first
place; the invariance control below is therefore an explicit emulation of the
shadow, not a report that the harness got lucky. *(`gke-deploy-lead` broadcast
this hazard fleet-wide and then corrected its own scope within the hour, on
measurements from `ag-dev`, `gd-prec` and `gd-p0-rev-4`. The correction is the
reason this paragraph has a table in it.)*

> **AN ENGINE IDENTITY MEASURED AT THE PROMPT IS A FACT ABOUT THE PROMPT.**
> (`gke-deploy-lead`.) Run `type grep` **the way your harness runs it**, or the
> disclosure describes a shell nothing was measured in.

**Why the path is spelled out anyway.** `type grep` here resolves to a shell function
from `~/.claude/shell-snapshots/snapshot-zsh-…-ijz3o1.sh`, whose body is

```
ARGV0=ugrep "$CLAUDE_CODE_EXECPATH" -G --ignore-files --hidden -I \
  --exclude-dir=.git --exclude-dir=.svn --exclude-dir=.hg ... "$@"
```

so a bare `grep` runs `claude.exe` under a different `argv[0]`, with **ten flags
injected that you did not type**. `CLAUDE_CODE_EXECPATH` is set; the function's
fallback path does not exist, so clearing that variable drops you to GNU grep and
silently changes every flag. **No `ugrep` exists on this filesystem in any form** —
`command -v ugrep` is empty, and `/usr/bin/find / -xdev -name 'ugrep*'` returns
**0 files** (`command -v` clears only `PATH`; `gd-trig` hardened this and it
reproduces here). What is there is a **ugrep-compatible engine embedded in `claude.exe`**:
it self-reports `ugrep 7.5.0` and it accepts `--ignore-files`, which GNU grep 3.8
rejects with exit 2. *(Earlier revisions of this file, and of the reviews it draws
on, said "a zsh function wrapping ugrep 7.5.0". The wrapper is real and the flags
are real; the separate binary is not. `gd-p0-rev-4` found and retracted that, in
its own instrument disclosure, and the wording reached this file from there.)*

**Dialect, which is the part that actually bites.** The injected `-G` forces BRE,
so `|` is a literal. On this file, at head:

```
/usr/bin/grep -cE 'values\.yaml|values\.schema\.json'   ->  3
/usr/bin/grep -c  'values\.yaml|values\.schema\.json'   ->  0     <- -G, GNU
        grep -c  'values\.yaml|values\.schema\.json'    ->  0     <- the shadow
```

**A false zero, over a file that contains three hits, from a command that exits
0 and prints a number.** Naming the right binary and omitting the dialect would
not have caught it.

> **AN ENGINE DISCLOSURE THAT NAMES THE BINARY AND OMITS THE DIALECT IS NOT A
> DISCLOSURE.** The binary is the route to the defect; the dialect *is* the
> defect. (`gd-p0-rev-4`'s addition to the standard, and the example above is
> what it predicts.)

🛑 **And `-E` is not the fix.** GNU BRE has backslash extensions — `\|` `\?` `\+`
`\(…\)` — which work under `-G` and are **literals under `-E`**. Measured here on
a fixture, GNU grep 3.8, identical under the shadowed engine:

```
alpha|bravo               -G 0 (rc1)    -E 2 (rc0)      <- the known defect
alpha\|bravo              -G 2 (rc0)    -E 0 (rc1)      <- the REMEDY's defect
workspace[_ -]?storage    -G 0 (rc1)    -E 3 (rc0)
workspace[_ -]\?storage   -G 3 (rc0)    -E 0 (rc1)
```

**Both directions turn present terms into confident, well-formed zeros.**
(`gd-trig`.) This is not hypothetical for this harness: **C10's pattern is
`values\.yaml\|values\.schema\.json`, BRE alternation, and an `-E` "upgrade"
would silently reduce it to zero hits — at which point C10's assertion,
*"0 hits outside the bullet"*, would PASS over an empty set.** That is the
guard-switched-off-by-its-own-remedy shape, found in my own instrument, by
someone else's correction. C10 now asserts its **denominator first** (3 hit
lines) and fails on inequality before it looks at position, and E10/E11 assert
that the BRE extension is live in the dialect actually in use and dead under
`-E`.

> **THE DIALECT IS NOT AN AXIS WITH A SAFE END.** State the dialect **and the
> pattern as passed, byte for byte** — `?` and `\?` are different patterns and a
> disclosure naming only the dialect cannot tell them apart.

**Control: all 52 assertions were re-run under the shadowed engine and every one
of the 52 outcomes is byte-identical.** Not asserted from the patterns being
`-F` or BRE-safe — measured, by pointing the harness at
`ARGV0=ugrep "$CLAUDE_CODE_EXECPATH" -G --ignore-files …` and diffing the full
output. **The numbers in this section do not depend on which engine you use; the
disclosure is here because that had to be shown rather than assumed**, and
because the same file one paragraph up shows what it costs when it is not true.
The second axis, `--ignore-files`, cannot bite here either: this path is not
git-ignored (`git check-ignore` returns nothing).

### The ten corrections, and N3

| # | claim as it stood | measured | disposition |
|---|---|---|---|
| 1 | §6's four hard-coded line numbers | three lines, all renumbered | **deleted, not updated** (R4) |
| 2 | §6 *"174 lines earlier"* | 170 @ `5ebe3dab`, 185 @ `6fc0cdfc` | **deleted** — never true at any SHA |
| 3 | §6 control *"a token known present counts 8"* | 9 | token named, count pinned |
| 4 | §3 *"grepping the same token at head returns 6"* | 6 then, 9 now | pinned to `6fc0cdfc` |
| 5 | §3 ⚠️ *"the second count is 1, not 0"* | 1 then, 2 now | pinned to `6fc0cdfc` |
| 6 | §3 *"that phrase appears nowhere in this file"* | **2** | scoped to use vs mention, count stated |
| 7 | §1 *"seven were already filed"* | 6 | corrected |
| 8 | §1 *"two are new"* | 3 | corrected |
| 9 | §3 heading *"the two new findings"* | 3 | corrected |
| 10 | *"a zsh function wrapping ugrep 7.5.0"*, ×3 | no such binary exists | corrected; the wrapper and flags are real |
| — | §3 row-16 *"the same sentence"* | two sentences | corrected (N3; **out of the denominator**) |

Four of those are worth more than the corrections.

**#3 is R3's own control, retired by R3's own commit.** *"A token known present
counts 8"* was true at `38a41b6e` and `5ebe3dab`; the round-8 commit added a ninth
`isHADeployment` and made it 9. **The control installed to show the count was
trustworthy was itself an unpinned count of this file**, and it did not name its
token, so it could not be re-derived at all.

> **A CONTROL IS A MEASUREMENT AND DECAYS LIKE ONE.** Pinning the number it guards
> and leaving the control bare protects the claim and abandons the evidence.

**#2 was never true, and neither reviewer caught it.** The distance is 170 at
`5ebe3dab` and 185 at `6fc0cdfc`, and no pair of anchors gives 174 at either. It
came from `gd-p0-rev-4`'s round-8 review prose and I copied it into the file
without measuring it — into the document whose subject is claims nobody
re-checks, in the commit that fixed a claim nobody re-checked.

> **A NUMBER ARRIVING FROM A REVIEWER IS STILL AN UNVERIFIED NUMBER, AND IT
> ARRIVES WITH AUTHORITY, WHICH IS THE PROPERTY THAT SUPPRESSES CHECKING.** The
> same mechanism as the required-wording finding in §3, from the opposite
> direction: there the authority was an instruction, here a correction.

**#7 and #8 survived four rounds because their sum was right.** *"Seven were
already filed … two are new"* sums to nine and nine is correct; the split is six
and three. Every reviewer who checked it checked the total.

> **AN ARITHMETIC IDENTITY IS NOT A CHECK OF ITS OPERANDS.** A decomposition whose
> parts are both wrong by the same amount in opposite directions is
> indistinguishable, at the level of the sum, from a correct one — and the sum is
> the only thing a reader verifies.

**#5 is this section falsifying a claim two sections up, by reporting on it.**
The §3 ⚠️ note said the negative token `mutable state, so` has exactly one hit in
the chart tree, and that the hit is the line asserting it is zero. Writing that
token into the table above makes it **two**. Rather than route around it by
paraphrasing the token, the note is pinned to `6fc0cdfc` and the token is quoted
here on purpose: *a negative count published in prose enters its own corpus*
applies to the sweep that verifies the rule, and a sweep that has to avoid naming
its subjects to stay true is not a sweep.

### What the sweep got wrong about itself

A sweep that reports only its catches has no measurable error rate, so:

- **One false positive.** My row-counting regex matched a row in §1's table as
  well as §2's and reported ten rows where there are nine. §2's *"All nine"* was
  correct all along. **One of the four findings in the first pass was my
  instrument, not the file.**
- **A second false positive**, from hard-coding the expected line numbers of the
  §6 hits into the harness instead of a range — the same defect the sweep exists
  to remove, committed by the sweep, one round later.
- **One false negative, which is worse.** The check for #6 passed on its first
  run because the harness piped a *filename* into `grep` where it meant the file's
  contents; it grepped the path and found nothing. **A defect that reads as a pass
  is the failure mode this whole file is about**, and it was caught only because
  the answer disagreed with a manual grep run four minutes earlier.

> **A SWEEP IS AN INSTRUMENT AND GETS NO EXEMPTION FROM THE RULE IT ENFORCES.**
> Report its false positives and its false negatives, or its denominator is a
> claim of the same kind as the ones it audits.

### Scope this sweep does not cover

Only claims about *this file*. The `path:line` citations into other trees
dereference at `6fc0cdfc` but are **not** all `@ SHA`-pinned — that is the same
defect pointing outward, it is P1-sized, and it is disclosed here rather than
fixed. **The `24` and `81`/`57` figures in §1 are internally consistent and this
sweep is the first time anyone has checked even that much**; their agreement with
the sweeps that produced them is unverified and is not claimed.

The harness is held at `verification/held/self-claim-sweep.sh`, sha256 prefix
`7fc9361b7b3214e6`, and is deliberately **not** committed: `tests/` is frozen at
P0. It exits 0 on 52/52 under **both** engines and **exits 1 under mutation
control** — reverting the §3 heading to *"the two new findings"* takes it to 51/52 — so it is an instrument
that can disagree, which four of this morning's could not. It should land beside
this file when P1 unfreezes `tests/`, at which point the numbers above stop being
a report and become a check.
