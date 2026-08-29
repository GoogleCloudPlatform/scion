# Scion Messaging — Semantic Contract Refactor

**Status:** approved; implementation in progress
**Date:** 2026-08-23 (last revised 2026-08-29)
**Author:** architect agent (`ca-msg-arch`)
**Base commit:** `a7ac9c489`
**Companion:** `messaging-conversation-model-findings.md` (grounded defect inventory — read first)
**Discussion:** [#1264 — Messaging is moving to a conversation model](https://github.com/GoogleCloudPlatform/scion/discussions/1264)
**Decision on file:** Option A (Hub-owned Conversation entity covering external threads),
confirmed by @ptone 2026-08-23.

> **Note on drift from Discussion #1264.** That discussion was posted 2026-08-24, when this
> document was 1056 lines; it is now 2100. Most of the growth is resolved open questions (Q1, Q2)
> and new sections for DEF-6, DEF-7, DEF-9, DEF-11, DEF-12, DEF-13, DEF-15 and DEF-16.
>
> **Two decisions announced in #1264 have since been reversed, and #1264 has not been edited.**
> Where the two disagree, this document is current:
>
> 1. **Cross-project addressing is gone.** #1264 presents `#<space>/<thread>` as "a thread in
>    another space", and an earlier draft added `@<project>/<agent>` as a disambiguation escape
>    hatch. Both were removed. References never cross a project boundary: the grammar cannot
>    express such an address, and a `conv:<id>` from another project is rejected by a permission
>    check at send time rather than resolved. Working in another project is a context switch
>    (`scion --project other message …`), not an address. This is a security boundary, not a
>    convenience change.
> 2. **`scion schedule message` is not a separate verb.** #1264 maps `--in` / `--at` onto it.
>    That split was dropped from both the phase plan and the flag mapping.
>
> §2.6.3 (`Conversation` and `webchat_topic` as parallel constructs) remains **OPEN** and is the
> one load-bearing question this document does not settle.

---

## 1. Problem & Goals

### Problem

Scion has three overlapping addressing schemes for "where does this message go", added at
different times and never reconciled (`messaging-conversation-model-findings.md` §0). The only cross-field consistency
rule in the entire system is `thread_id requires channel`. Nothing checks that the schemes
agree, so they can contradict each other and the system routes on whichever the executing
code path happens to read.

The visible consequences: messages that route nowhere and report success; replies that
broadcast to every linked channel in a project; a channel name that cannot be addressed at
all; a message type enum that mixes four unrelated concerns; and a `scion message` command
with 13 flags and 34 pairwise exclusion rules.

### Goals

| # | Goal | Success criterion |
|---|---|---|
| G1 | One primary address | A message names exactly one venue. No message can carry two addresses that disagree. |
| G2 | No silent misrouting | Every send either reaches an identified venue or fails with a specific error. The "success, but delivered nowhere" outcome is unreachable. |
| G3 | A contract an agent can state in one sentence | An agent can decide "is this for me, and what is wanted" from structured fields, without parsing prose. |
| G4 | Common model across surfaces | Native chat and every integration share one conversation abstraction, so native features degrade gracefully rather than being special-cased. |
| G5 | Collapse the flag matrix | `scion message` carries orthogonal concerns only; exclusion rules drop from 34 to a handful. |
| G6 | Machine-readable lifecycle | `status` and `visibility` reach the agent as fields, not prose. |

### Non-Goals

- **Changing delivery semantics.** No queuing, fast-fail on a stopped agent, and the
  `persisted → delivered \| failed` state model are settled and unchanged (`messaging-conversation-model-findings.md` §11.1).
- **Centralising mention rendering.** Platform-specific `@`-syntax and identity mapping stay
  inside each broker plugin (`messaging-conversation-model-findings.md` §11.3). This design does not create a shared
  hub-side mention renderer.
- **Federation.** No hub-to-hub addressability (`messaging-conversation-model-findings.md` §11.7). Conversations are
  local to one Hub deployment.
- **Fixing every listed bug.** `messaging-conversation-model-findings.md` §12 lists eight pre-existing bugs. This design
  fixes the ones that are structural (reply affinity, channel-ID mismatch, silent
  misrouting, mention fan-out) and explicitly does not address the rest (scrollback
  pagination, attachment content transfer, notification redelivery). Those remain separate
  work items and must not regress.
- **A new transport.** The event bus, broker plugin gRPC interface, and runtime broker hop
  are unchanged.

---

## 2. Proposed Design

### 2.1 The core abstraction: Conversation

A **Conversation** is the unit of addressing. It is a place where messages accumulate and
participants read them. Every message is posted to exactly one Conversation.

A Conversation is *the same kind of thing* whether it is a native chat topic, a native DM,
a Discord thread, a Slack thread, a Telegram forum topic, or a Google Chat thread. That
uniformity is the whole point of Option A: it is the common model that native features
degrade *from*.

```
Space (= Project)
  └── Conversation (kind: group)          e.g. #general, a Discord thread
  └── Conversation (kind: group)
Conversation (kind: direct)               DMs are global, not space-scoped
```

### 2.2 Data model

```go
// Conversation is the addressable venue for all messages.
type Conversation struct {
    ID        string   // hub-owned UUID, stable, never a platform ID
    ProjectID *string  // nil for direct conversations (they are global)
    Kind      ConversationKind  // direct | group
    Surface   Surface           // native | discord | slack | telegram | gchat | teams

    // ExternalRef is the platform's opaque identifier for this venue.
    // Empty for native conversations. Unique per (Surface, ExternalRef).
    ExternalRef string
    // ParentRef is the platform container the link is registered on —
    // Discord parent channel / forum, Slack channel, Telegram chat.
    // Honours the settled rule that links live on the container, not the thread.
    ParentRef string

    DisplayName    string
    DefaultAgentID *string   // exactly one place this now lives

    DriftState     DriftState // active | orphaned | unresolvable
    LastActivityAt time.Time
    CreatedAt      time.Time
    ArchivedAt     *time.Time
    DeletedAt      *time.Time
}

// ConversationParticipant records a Scion principal that can send or receive here.
// NOTE: this is NOT the platform's membership list. A Discord channel may have 500
// humans in it; only those with a linked Scion identity, plus the agents bound to
// the conversation, are participants. This distinction is load-bearing — see §2.7.
type ConversationParticipant struct {
    ConversationID string
    PrincipalKind  PrincipalKind // user | agent
    PrincipalID    string
    Role           Role          // member | observer
    JoinedAt       time.Time
    LeftAt         *time.Time
}
```

Uniqueness: `UNIQUE (surface, external_ref) WHERE external_ref <> '' AND deleted_at IS NULL`.
This index is what makes broker-edge resolution idempotent and is the single most important
constraint in the design.

### 2.3 The message envelope

`channel` and `thread_id` are **removed** from the message. They become derived, read-only
properties of the conversation. This kills the "two addresses that disagree" class outright
(G1) — it is not that we validate agreement, it is that there is only one address.

```go
type Message struct {
    ID             string
    ConversationID string   // REQUIRED. The one address.
    ReplyToID      *string  // in-conversation threading / quoting

    From       PrincipalRef  // user:<id> | agent:<id> | system:<component>
    Kind       MessageKind   // text | event

    // Exactly one of the following is populated, per Kind.
    Intent *TextIntent  // inform | request | question      (Kind == text)
    Event  *EventBody   // typed lifecycle notice           (Kind == event)

    Body        string
    Attachments []AttachmentRef
    Visibility  Visibility    // normal | verbose | full
    CreatedAt   time.Time
}

// Addressee records who, within the conversation, this message demands attention from.
// One message, N addressees — replacing today's N messages with N types.
type Addressee struct {
    MessageID     string
    PrincipalKind PrincipalKind
    PrincipalID   string
    Via           AddressedVia // explicit | body-mention | default-agent | direct
    DeliveryState DeliveryState // pending | delivered | failed
    FailureReason *string
}
```

**Addressing is two fields: the conversation, and the addressee set.**

`Recipient`, `RecipientID`, `Recipients`, `Broadcasted`, `Channel`, `ThreadID` all
disappear from the envelope. `Plain`, `Raw`, `ObserverOnly` move out of the message and
into delivery options (§2.8).

#### 2.3.1 Why `ReplyToID` does not subsume `ConversationID`

A message ID is more specific than a conversation ID, so it is reasonable to ask whether
`reply_to` alone could address a message. It cannot. The two fields answer different
questions:

| Field | Question | Required |
|---|---|---|
| `ConversationID` | Where does this go? | Always |
| `ReplyToID` | Which message am I responding to? | Never |

Four reasons the container ID has to exist independently:

1. **Cold start.** The first message in a conversation has no parent. More importantly, a
   conversation can exist with zero messages — a native DM pane is opened, or a Discord
   thread is created and nobody has spoken. Native chat must be able to render and address
   an empty conversation, and `POST /conversations/:id/messages` must have an `:id`.
2. **Mutable container state has nowhere else to live.** Participant set, default agent,
   read state, `ArchivedAt`, `DriftState`, and the `(Surface, ExternalRef)` mapping to the
   real platform thread all change over the life of a conversation. Messages are immutable
   records. Attaching mutable state to an immutable row is a category error, and it is
   unclear which message would own it.
3. **Derivation cost and fragility.** Recovering the container means walking `reply_to` to
   a root: every routing decision becomes a graph traversal instead of a column lookup, and
   the chain breaks when a middle message is deleted or redacted.
4. **Most messages are not replies.** In an active thread the common case is "the next thing
   said in the room." If `reply_to` were the address, senders would have to nominate an
   arbitrary parent — and on Discord and Slack `reply_to` maps to the native quoted-reply
   affordance, so a synthetic parent renders as a false quote to every human present.

**Invariant: `reply_to` is never a routing input.** On the wire, `conversation` is always
populated and always authoritative. `reply_to` must reference a message whose
`conversation_id` equals this message's — which is an O(1) column comparison precisely
because the container ID is stored on both rows. Remove it and the constraint becomes both
a chain walk and unvalidatable, since there is nothing to compare against.

**Ergonomic consequence (adopted).** Because a message belongs to exactly one conversation,
`reply_to → conversation` is a total function. The CLI therefore accepts:

```
scion message --reply-to <msg-id> "..."      # conversation positional omitted
```

resolving the conversation in one lookup and placing the *resolved* conversation on the
wire. This does not reintroduce the two-address failure mode: the CLI requires exactly one
of (conversation positional, `--reply-to`), there is still no implicit fallback, and
supplying both with a mismatch is an error rather than a silent preference.

### 2.4 Addressee resolution — how "no recipient named" works

This is the mechanism that delivers the requested "send with no recipient explicitly named".

When a message arrives with an empty addressee set, the Hub resolves it **from the
conversation's own structure**, in this order:

1. **`Kind == direct`** → the other participant. Recorded in
   `conversation_participants`, so it is unambiguous and auditable.
2. **`DefaultAgentID` is set** → that agent, `Via: default-agent`.
3. **Otherwise** → the message is persisted and visible to all participants, and
   **no agent is woken**. This is the ordinary "humans talking in a room" case, which the
   current system cannot represent at all.

Case 3 is a real outcome, not a failure. It is reported distinctly by the API so a caller
can tell "posted, nobody woken" from "posted, agent dispatched".

#### Reconciling with the settled "recipient must be explicit" rule

`messaging-conversation-model-findings.md` §11.6 records a settled decision that the outbound agent→user recipient must
always be explicit, because the old `CreatedBy` fallback was an implicit guess.

This design **satisfies that decision more strongly than the current code does**, and does
not re-open it. The distinction:

- The rejected behaviour inferred a recipient from *agent metadata* (who created the agent)
  — an implicit guess about an unrelated fact.
- This design derives the addressee from *the conversation's recorded participant set* — an
  explicit, stored, queryable fact about the venue the sender named.

The recipient is still explicit. It is declared once, when the conversation is established,
instead of being retyped on every message. That is the ergonomic win, and it is why
omitting an addressee is now safe where omitting `--thread-id` is currently catastrophic.

#### 2.4.1 Global direct conversations: resolution and failure modes

Direct conversations are **global** — one per pair of principals, `ProjectID` nil (Q2,
resolved). This keeps the familiar "one DM per person" model, but it means a DM has no
ambient project, and anything project-scoped inside it must resolve without one. Today that
resolution is a guess that returns `""` and silently disables mention resolution
(`messaging-conversation-model-findings.md` §9). The rule below replaces the guess.

**Candidate scope.** A project-scoped reference inside a direct conversation resolves against
the **intersection of the projects the two participants both belong to**. Not the sender's
projects; the shared set. An agent belongs to exactly one project, so any DM with an agent has
a candidate set of size ≤ 1 and is trivially unambiguous.

**Resolution outcomes**, exhaustively:

| Shared projects | Slug matches | Outcome |
|---|---|---|
| exactly 1 | 1 | Resolve. |
| N > 1 | exactly 1 across all N | Resolve. Multiple projects do not imply ambiguity. |
| N > 1 | > 1 | **Fail: `ambiguous`.** Error lists every qualified candidate. |
| N ≥ 1 | 0 | **Fail: `not-found`.** |
| 0 | — | **Fail: `no-shared-project`.** |

**Never resolve to a first match.** Ambiguity resolves to nothing. This is the same rule as
conversation references (§2.6): no fallback destination, ever.

**Disambiguation escape hatch.** A qualified mention `@<project>/<agent>` resolves directly.
Every `ambiguous` error prints the exact qualified forms that would have worked, so the error
is a fix, not a dead end.

This form *names* a project but does not address across one: both candidate projects are
already shared by both participants, and the DM itself belongs to no project. It is
disambiguation within the shared set, not a cross-project reference. (It no longer mirrors a
`#<space>/<thread>` reference form — that form is removed, §2.6.1.)

**A single message may not address agents in more than one project.** Without this, mentions
would be a way to build a cross-project venue by the back door: Alice mentions `@builder`
(project X) and `@deployer` (project Y) in one DM message, and two agents from isolated
projects are now addressees of a shared conversation, able to see each other's replies. The
shared-project intersection permits this and must be constrained explicitly. If the resolved
addressee set would span projects, the send **fails** and names the conflicting projects. Human
participants are unaffected; the rule applies to agent addressees.

**Information disclosure.** `not-found` must not distinguish "no such agent" from "an agent
with that slug exists in a project you do not share." Both produce the same message: *not
found in any project you share with `<peer>`*. Otherwise the error becomes a probe for the
existence of private projects.

**Does a failed mention fail the send?** No — and this is safe specifically because
`Kind == direct` always resolves the peer as an addressee (§2.4 case 1). A mention in a DM is
always *additive*, so an unresolved one can never mean nobody receives the message. The rule:

- The message is delivered to the peer.
- The response carries an `unresolved[]` array: `{ text, reason, candidates[] }` where
  `reason ∈ { ambiguous, not-found, no-shared-project, not-a-participant }`.
- The CLI prints each unresolved mention to stderr and **exits non-zero** (distinct code for
  *delivered with unresolved references*, separate from *send failed*). An agent that checks
  its exit status learns the fan-out did not happen. This is the difference from today, where
  the same situation exits 0 and reports nothing.

**Conversation references inside a DM.** The bare `#<thread>` form means "in the current
project's space" and therefore has no meaning in a direct conversation, which has no ambient
project. It fails naming the cause and the fix — select a project first
(`scion --project <slug> message #thread …`) — rather than resolving against an arbitrary one.
There is no qualified `#<space>/<thread>` form to fall back on; addressing a conversation in
another project is not something the system does (§2.6.1).

**`--to` inside a DM** may only name one of the two participants. Naming anyone else fails as
`not-a-participant`; a DM is not a back door to addressing a third party.

#### 2.4.2 Reconciling the two direct-conversation shapes (DEF-8, DEF-10)

*Added 2026-08-27, after the S5 survey. This section describes work that does not exist yet.*

**The defect.** Two code paths create direct conversations, using two different identity keys,
and neither can see the other's rows.

| | dual-write (`ResolveOrCreateDMConversation`) | resolver (`createDirectConversation`) |
|---|---|---|
| key | `external_ref = dm:{sorted(idA,idB)}` | the participant set |
| `ProjectID` | nil (global) — correct per Q2 | sender's project for `@<agent>`, nil for `@<email>` — **DEF-10** |
| participants | **none written** | both written |
| lookup | `UpsertConversationByExternalRef`, one indexed read | `findDirectConversation`: list the sender's conversations, then list participants of each |
| created by | every legacy send, as a stamp | `scion message @<agent>` / `@<email>` |

The lookups are asymmetric in exactly the way that prevents convergence. `findDirectConversation`
reaches rows through `conversation_participants`, so it can never see a dual-write row (which has
none). `UpsertConversationByExternalRef` keys on `external_ref`, so it can never see a resolver
row (whose ref is `""`, and which the partial unique index excludes). **One principal pair, two
conversation IDs, indefinitely.** This is what the divergence board will report once traffic runs
through both paths, and it is the reason the read switch cannot be turned on yet.

**Neither path is individually wrong.** Each is internally consistent and idempotent; there is no
row-growth bug. The defect is that they were specified in different sections and never required
to agree on what identifies a DM.

**Decision: converge on `external_ref` as the identity key.** The resolver computes
`DirectMessageExternalRef(senderID, targetID)`, creates through
`UpsertConversationByExternalRef`, and then ensures participants. `findDirectConversation` is
deleted in favour of a single indexed read. All direct conversations become global —
`ProjectID` nil — which closes DEF-10. `resolveAgentDM` keeps using the ambient project to
resolve the *agent slug*; it stops writing that project onto the conversation. Those are
different uses of the same value and conflating them is what produced DEF-10.

**Why this key and not the other.**

| Alternative | Rejected because |
|---|---|
| Converge on the participant set: dual-write calls the resolver | Dual-write is on the hot send path and the participant lookup is N+1. Worse, uniqueness would become application-enforced only — the partial unique index excludes `external_ref = ''`, so two concurrent first-sends race into two rows. It trades a DB guarantee for a code convention. |
| Keep both, add an alias table mapping the IDs | Institutionalises the split and taxes every future reader with a join. This is the "add a layer over the confusion" move; the confusion is cheap to remove now and expensive later. |
| Stop dual-write from creating DM rows; let the resolver own them | Dual-write exists so that legacy messages carry a `conversation_id` for the read switch to read. Removing it now leaves the switch with nothing. This is what phase 13 does eventually, not what unblocks the switch. |

**Migration.** Ordered, because steps 2 and 3 are not commutative.

1. **Code first, behind no flag.** Resolver computes and sets `external_ref`; sets `ProjectID`
   nil for all direct conversations; ensures participants after upsert. From this point no new
   divergent rows are created.
1c. **Key-based DM authorisation.** `requireParticipant` branches on kind. For `kind = 'direct'`
   it parses `external_ref` and requires the caller's **kind *and* ID** to match a named
   principal — strictly tighter than shipped `isDMParticipant`, which checks ID only. Parse
   failure denies. No fallback, no repair. `kind = 'group'` and unknown kinds keep the existing
   table-based check. **This step is what makes `DMConversationKey` security-critical**, and
   therefore what promotes the golden vectors from a consistency check to a security control.
2. **Rebuild the listing index** — backfill participants onto existing `dm:` rows. Per §2.4.2.1
   this is a *listing* repair, not an access-control operation: a wrong or missing row hides a
   DM from someone's list, it does not grant or deny. Kind is read from the key and then
   *verified* against the claimed table (verification, not discovery). Old-format kind-free rows
   are unparseable and are left participant-less and recorded. All-or-nothing per row: a
   half-written DM would list asymmetrically. The migration reports the unparseable count —
   **silence is not zero.**
3. **Merge resolver rows, then re-key old-format rows.** For each `kind = direct AND
   external_ref = ''` row, derive the ref from its two participants; if a row with that ref
   exists, re-stamp its messages, copy any missing participant, soft-delete the duplicate;
   otherwise set the ref in place. Then re-key surviving kind-free `dm:X:Y` rows to the
   kind-encoded format by looking both IDs up. Ambiguous rows are left as-is — they stay
   inaccessible under 1c, which is fail-closed and the correct outcome for a row we cannot
   describe.
4. **Guard.** A migration is not done because it ran. Assert zero rows matching
   `kind = 'direct' AND external_ref = '' AND deleted_at IS NULL`; assert every `dm:` row has
   exactly two participants; assert every `dm:` row *carrying* participants has a key
   `ParseDMKey` accepts. Permanent tests, not one-off queries — **and each carries a floor**, per
   rule 14, since all three are vacuously true on an empty table.

**CORRECTED 2026-08-27 13:40Z. The paragraphs that stood here specced a security migration for a
hazard that shipped code had already designed out.** They are replaced rather than amended,
because the conclusion was not merely incomplete — it was pointed the wrong way. The original
text argued that step 2's participant backfill was security-critical, since `requireParticipant`
would trust whatever `principal_kind` it wrote, and a wrong kind would be an access grant to the
wrong principal. Both premises were wrong. What follows is the corrected model, established by
reading `pkg/hub/handlers_chat_v2.go` on `origin/main` and confirmed by the native-chat architect
as deliberate design (their §4.1/§4.2), not accident.

**2.4.2.1 The key is the authority. Participant tables are a listing index.**

For a 1:1 direct conversation, the participant set *is* the identity — it is not a fact stored
alongside the identity, it is the same fact. The shipped key encodes it:

```go
// pkg/hub/handlers_chat_v2.go:388
var dmKeyRegexp = regexp.MustCompile(`^dm:(user|agent):[0-9a-f-]{36}:(user|agent):[0-9a-f-]{36}$`)

// pkg/hub/handlers_chat_v2.go:2932 — authorization. Parses the key. Reads no table.
func isDMParticipant(key, userID string) bool {
    parts := strings.Split(key, ":")
    if len(parts) < 5 { return false }
    return parts[2] == userID || parts[4] == userID
}
```

Two consequences follow, and they invert the original section.

**The principal-kind hazard does not exist.** The key is kind-encoded (`dm:agent:X:user:Y`), so
no backfill ever has to *infer* whether an ID is a user or an agent — the key already says. The
entire ambiguity analysis was solving a problem created by our own duplicate key format
(`dm:{sorted(idA,idB)}`, `pkg/messaging/divergence.go:124`), which dropped information the
shipped format carries. Adopting the shipped format deletes the hazard rather than mitigating it.

**Step 2 is therefore not a security migration.** Since authorisation derives from the key, a
missing or wrong participant row cannot grant or deny access. It means a DM fails to appear in
someone's conversation list. That is a correctness bug worth fixing and **not** a security one.
Under-granting-is-recoverable still governs — ambiguous rows are left participant-less and
recorded, never guessed — but it now protects a listing, not an ACL.

**The general rule, from the native-chat architect, which decides cases beyond DMs:**

> Chat-owned tables are authoritative only for facts no external authority encodes — names,
> defaults, watermarks, pins. **Authorisation always derives from the key, or from project
> membership.** If one of our tables is about to become the authority for an access decision,
> that is the smell.

**2.4.2.2 The invariant that makes this safe, and the boundary where it stops.**

Key-as-authority holds **only while the participant set is static and fully named by the
identifier.** The moment membership is dynamic, the key and the ACL disagree and the pattern
flips from hazard-deleting to hazard. Therefore:

> **INVARIANT D-1. A direct conversation's participant set is immutable for its lifetime.
> "Add a person" is not a mutation — it is a promotion that creates a different conversation
> under a different authority.**

This is why native chat deferred group DMs to threads, where authority becomes project
membership — again an *existing* authority, not a chat-owned table.

**Enforcement (`AddParticipant`, store layer, unbypassable):**

> For `kind = 'direct'`, accept a principal **only if `ParseDMKey(external_ref)` names that exact
> `(kind, id)` pair member**. Otherwise reject. An unparseable or empty `external_ref` rejects.

Note what this is *not*: it is not "reject once two participants exist." A count-based guard is a
proxy for the invariant and it leaks — soft-remove B (active count falls to 1), then add C, and
the count test passes while the membership silently diverges from the key. The key-derived guard
has no such gap, needs no special case to permit re-add after soft-remove, and cannot be changed
in meaning by a future refactor of how many participants the creation path writes.

**2.4.2.3 Promotion transfers continuity; it does not break it.**

Correcting my own stated reason, from `PromoteDM` (`webchannel_store.go:1805`): promotion is one
transaction that inserts the new topic, re-keys the history wholesale
(`UPDATE messages SET thread_id = <topicID> WHERE thread_id = <dmKey>`), migrates read state, and
deletes the DM registry rows. **Identity and ACL change together, atomically, and the history
moves to the new authority.** So the accurate statement is not "continuity is lost" but
"continuity is transferred."

One consequence is worth designing around rather than discovering. Because keys are
deterministic, **the same pair's DM key is reborn empty if they message again later.** Promotion
drains a DM; it does not fork it. This is also the graceful failure shape for the TOCTOU window:
a reply racing the promotion with the old key lands in the reborn-empty DM — degraded and
visible, not lost.

**Why nothing is exploitable today.** Every `dm:` row currently has zero participants, so
`conv:<id>` against one denies everyone. The system is failing closed. That remains the reason
the ungated HTTP resolve endpoint is not a live problem — but note it is now *belt and braces*
rather than the load-bearing fact it was, since key-based auth denies independently.

**Acceptance criteria for this work.**

- **AC-DEF8-1** Two sends to the same agent, one through the legacy path and one through
  `scion message @<agent>`, resolve to **one** conversation ID. The test asserts the row count,
  not the return value of either call.
- **AC-DEF8-2** Every direct conversation has `ProjectID` nil. A permanent test asserts this over
  rows created by both paths.
- **AC-DEF8-3** The divergence board shows non-zero matches and zero mismatches for DM traffic
  across both paths — the condition §"Divergence Monitoring" already requires before the read
  switch is enabled.
- **AC-DEF8-4** Post-migration guards from step 4 exist as permanent tests, each with a floor.
- **AC-DEF8-5** A backfill row whose principal kind cannot be determined produces **no
  participant** and an audit record. Mutation-verified: make the resolver ambiguous and confirm
  the named test fails.
- **AC-DEF8-6** Golden vectors pin `DMConversationKey` output for mixed-kind pairs, same-kind
  pairs, and ordering normalisation, and pin rejection of malformed input. A conformance test
  asserts every generated key matches shipped `dmKeyRegexp`
  (`handlers_chat_v2.go:388`). **These are a security control, not a consistency check** — once
  1c lands, a change to the derivation is a change to the ACL. Mutation-verified.
- **AC-1C-1** Key-based DM auth: caller named in the key passes; caller whose ID appears but
  whose **kind** differs is denied; malformed key denied; old-format kind-free key denied; a
  `kind = 'group'` conversation still takes the table path unchanged.
- **AC-IMMUTABLE-1** `AddParticipant` enforces invariant D-1 at the store layer: for
  `kind = 'direct'` it accepts only a principal named by `ParseDMKey(external_ref)`; empty or
  unparseable refs reject.
- **AC-IMMUTABLE-2** The discriminating test: soft-remove participant B, then attempt to add C;
  assert rejection **and** assert the active participant set is exactly `{A}`. Per rule 13 this
  observes the effect, not the call — and it is the case a count-based guard silently permits.
- **AC-IMMUTABLE-3** Adding a third principal to a two-participant DM rejects; participant count
  is still 2 afterwards.
- **AC-INGRESS-1** A message may not be **written** with a `direct` conversation key that does not
  name the authenticated sender. Same rule as D-1, different verb: D-1 governs who may join a
  conversation, AC-INGRESS-1 governs who may write into one. Test: an authenticated agent supplying
  a **well-formed** DM key naming two other principals is rejected, and — per rule 13 — assert the
  message row was not created, not merely that the response was 4xx. Floor per rule 14: assert at
  least one message row exists for a key that *does* name the sender, so the count query is proven
  to be looking at the right table.

  **Why this is stated separately from AC-1C-1.** PR #1319 (merged to main 2026-08-27) added
  `validDMKey` at all three ingress points, rejecting malformed keys with 400 before dispatch or
  persistence. That is a real improvement and it closes the gap §5p item 2 reported. It is
  *format* validation, and it sits exactly where an authorization check would sit, on the same
  input, returning the same status shape. The read path is membership-checked
  (`handlers_chat_v2.go:2848`: `validDMKey` → `isDMParticipant`); the write path is not, and
  ingress writes the very column the read path filters on (`webchannel_store.go:1173`:
  `WHERE channel='web' AND thread_id=?`). Once step 1c makes key-parsing authoritative, an
  unguarded write side is an ACL hole by construction. **A check that answers a neighbouring
  question in the right location is harder to notice missing than no check at all.**

**What this does not resolve.** `#<thread>` still cannot resolve (DEF-7 — nothing writes
`DisplayName`), the addressee table is still never written (DEF-9), and there is still no
conversation-driven delivery: `conversation_id` remains a stamp on the message row, not a routing
key. Reconciling the DM shapes makes the read switch safe to evaluate. It does not make
`conv:<id>` or `#<thread>` usable from the CLI, which is DEF-5 and depends on all three.

### 2.5 The type taxonomy

Today one enum of eight values mixes intent, lifecycle signal, provenance, and delivery
artifact (`messaging-conversation-model-findings.md` §4). It splits into orthogonal fields:

| Field | Values | Replaces |
|---|---|---|
| `kind` | `text`, `event` | — |
| `intent` (text only) | `inform`, `request`, `question` | `instruction`, `chat`, `assistant-reply`, addressed half of `input-needed` |
| `event.type` (event only) | `agent.state-changed`, `agent.input-needed`, `delivery.failed`, `schedule.fired`, `port.exposed` | `state-change`, `system`, broadcast half of `input-needed` |
| `from` | `user:*`, `agent:*`, `system:*` | provenance previously encoded in `assistant-reply` / `system` |
| `visibility` | `normal`, `verbose`, `full` | unchanged |

**`mention` and `group-set` cease to exist as types.** They were delivery artifacts, not
kinds of message. A message to three participants is now one row with three addressees, not
three rows with a synthetic type. This removes the structural cause of the two open mention
routing bugs (`messaging-conversation-model-findings.md` §12).

`EventBody` carries `status` as a first-class field, so the notification lifecycle value
(`COMPLETED`, `WAITING_FOR_INPUT`, `DELIVERY_FAILED`) is machine-readable rather than
embedded in English prose (G6).

#### A clarification this taxonomy buys for free

The current skill needs a paragraph of prose to explain that only a parent/creator should
answer `input-needed`, because peers answering causes wasted tokens and scope violations.
In the new model that rule is structural and needs no prose:

- An addressed question is `intent: question` with the answerer in `to`. You answer if you
  are in `to`.
- `ask_user` is `event: agent.input-needed` delivered to *subscribers*. Subscribers are not
  addressees. Nobody is being asked to answer.

The distinction the prose was trying to teach is now visible in the envelope.

### 2.6 Conversation references — the addressing syntax

Agents and humans need a writable form, not raw UUIDs. The grammar is closed and validated:

| Form | Resolves to |
|---|---|
| `conv:<id>` | canonical; this is what inbound envelopes carry |
| `@<agent-slug>` | the direct conversation with that agent in the current project |
| `@<email>` | the direct conversation with that user |
| `#<thread-name>` | a group conversation in the current project's space |

Unresolvable references are an error at send time with the candidate list, never a
fallback. There is no "if I cannot resolve this, broadcast" path anywhere in the design.

The critical ergonomic property: **the inbound envelope carries `conversation.id`, and
replying means echoing it back.** The agent never constructs a thread ID, so it cannot omit
one. This is the direct fix for the reported problem.

#### 2.6.1 Project isolation — references never cross a project boundary

**Messages cannot be sent between projects.** This is an existing, settled property of Scion,
and the addressing grammar must not be able to express a violation of it.

An earlier draft of this section included a `#<space>/<thread>` form for addressing a
conversation in another space. **That form is removed.** It described a capability the system
does not have and must not acquire.

The rule, applied to every form in the grammar:

| Form | Cross-project behaviour |
|---|---|
| `conv:<id>` | **Rejected** if the conversation's `ProjectID` is not the sender's current project. |
| `@<agent-slug>` | Resolves only within the current project. An agent in another project is not addressable. |
| `@<email>` | Permitted — direct conversations are global and belong to no project (§2.4.1). |
| `#<thread-name>` | Resolves only within the current project's space. |

`conv:<id>` deserves emphasis: it is a bare UUID, so unlike the `@` and `#` forms it carries no
visible project context and cannot be rejected on syntax alone. **Project scope must therefore
be enforced as an authorisation check at send time, not as a resolution failure.** A
conversation ID that leaks between projects — pasted from a log, echoed from a stale envelope,
recorded in a scratchpad — must be rejected on the strength of the *sender's* project, not
merely fail to be found. This is the single most important isolation check in the design,
because `conv:<id>` is the form agents use most.

The error must say *why*: a conversation that exists but belongs to another project reports a
boundary violation, not `not-found`. These are different conditions and conflating them makes
the failure impossible to diagnose. Subject to the disclosure rule below.

**Working in another project is a context switch, not an address.** The CLI already has a
global `--project` flag. Operating in another project means selecting it, not addressing across
a boundary:

```bash
scion --project other-project message #general "..."
```

This is not cross-project messaging: the sender's ambient project changes, and the message is
sent from within that project to a conversation inside it. The distinction matters because it
keeps the isolation invariant intact while still making every conversation reachable by someone
with the rights to it.

**Disclosure.** As in §2.4.1, a conversation in a project the sender does not belong to must
report the same error as one that does not exist. The boundary-violation error is only for
projects the sender *can* see; otherwise the error discloses the existence of private projects.

**Broadcast is unaffected and is not a counter-example.** `scion broadcast --all` fans out
across projects today. That remains true and is not a contradiction: a broadcast is not a
conversation and is deliberately outside this model (§2.9). It is a one-way fan-out to agents,
not a message sent *to a venue in another project*, and it cannot be replied to across the
boundary — a reply is an ordinary message and obeys the rule above.

### 2.7 Broker edge resolution

Brokers resolve platform identifiers to a conversation ID **at the edge** and never send
raw platform IDs inward:

```
inbound:   platform event
             → broker: ResolveConversation(surface, external_ref, parent_ref)
             → hub: upsert on UNIQUE(surface, external_ref) → conversation_id
             → POST /api/v1/messages { conversation_id, from, to[], kind, ... }

outbound:  hub publishes to conversation_id
             → spoke selected by conversation.surface (not by a message field)
             → broker maps conversation_id → external_ref → platform API call
```

Two consequences worth naming:

- **Spoke selection now reads `conversation.surface`**, a validated enum, not a free string
  on the message. The `Name` vs `ChannelID` mismatch that makes `--channel gchat`
  unaddressable (`messaging-conversation-model-findings.md` §3.1) cannot recur, because there is only one key.
- **Participant sets are Scion-scoped, not platform-scoped.** A Discord channel with 500
  humans yields a conversation whose participants are the linked Scion identities plus the
  bound agents. The design does not attempt to mirror platform membership, and the API
  documents this explicitly so nobody builds a presence feature on a partial list.

#### 2.7.1 What creates a conversation row (Q3, resolved)

Surface containers are created **eagerly at link time** (Q3). That decision applies to
containers that an explicit act brings into existence — it is *not* a general policy of
pre-materialising every conversation that could exist.

**The governing invariant: a conversation row is never created by enumeration.** Every row
traces to a specific act — a person linked a channel, a person opened or posted to a
conversation, or a platform delivered an event. Nothing is created by walking the cross
product of principals.

| Conversation | Created by | Cardinality bounded by |
|---|---|---|
| Surface channel (`#dev` on Discord) | the link operation | explicit link acts |
| Surface thread under a linked parent | first inbound platform event (§2.7) | actual platform activity |
| Native topic | a person creating it | explicit acts |
| Direct (DM) | first send, or a person explicitly opening the DM | explicit acts |
| *(any principal pair, speculatively)* | **never** | — |

**Why DMs are not eager.** Three reasons, in increasing order of force:

1. Cardinality is combinatorial — `users × agents + C(users,2) + C(agents,2)` — where linked
   channels are merely numerous.
2. There is no external artifact to mirror. A linked Discord channel is a real object that
   already exists; an unused (alice, bob) pair is not a thing, it is a *capability*.
3. **Agents in Scion are ephemeral.** They are spawned and destroyed continuously. Enumerating
   DM rows per agent would write to the conversation table on every spawn and leave a dead row
   for every agent that ever existed. The table would stop describing conversations and start
   describing history of the roster.

**This does not make un-materialised DMs unaddressable.** The `@` reference grammar (§2.6) is
**resolve-or-create**: `scion message @builder "..."` creates the conversation on first send,
atomically, idempotent on the normalised participant pair. Addressability comes from the
grammar; the table only records conversations that have actually been engaged.

**Roster and conversation list are different queries with different sources.**

| Question | Source | Command |
|---|---|---|
| Who can I talk to? | agent registry / project membership | `scion agents list`, project members |
| What am I talking in? | `conversations` | `scion conversations list` |

Conflating them is exactly what would reintroduce the O(N²) problem. In native chat this is
the familiar split: the "new message" picker reads the roster, the sidebar reads conversations,
and picking someone from the picker materialises a conversation on send. No new command is
needed for the roster; it already exists.

**No crawling.** Linking a Discord channel does not enumerate its existing threads. Threads
materialise when the platform delivers an event for them. Backfilling history is out of scope
(§1 Non-Goals) and would violate the no-enumeration invariant.

**Relation to §2.3.1.** That section argues the schema *must permit* conversations with zero
messages — needed for a linked-but-silent channel and for an opened-but-unused DM pane. It does
not argue that empty conversations should be manufactured. Permitting an empty conversation and
enumerating all possible ones are independent; this design does the first and refuses the
second. An empty DM the user opened and abandoned is dismissible via `ArchivedAt` without
deleting anything, since there is nothing to delete.

### 2.8 Delivery options move off the message

`Plain`, `Raw`, `ObserverOnly`, `Urgent` are transport mechanics, not properties of a
message. They move into a per-delivery options struct that is not persisted on the message
row and not part of the conversation history. `Raw` in particular is not messaging at all —
it is keystroke injection, and it becomes a separate verb (§2.9).

### 2.9 CLI surface

The current command conflates addressing, scheduling, delivery mechanics, subscription
management, attachments and broadcast, producing 34 exclusion rules. Splitting by concern:

| Verb | Concern | Removes |
|---|---|---|
| `scion message <conv> <text>` | post to a conversation | — |
| `scion broadcast <text>` | project / global fan-out (not a conversation) | ~12 rules |
| `scion schedule create …` | deferred send (exists, but see the correction below) | ~8 rules |
| `scion keys <agent> <literal>` | raw keystroke injection, local/terminal only | ~5 rules |
| `scion notifications subscribe` | subscription management (already exists) | ~2 rules |

`scion message` retains six flags: `--to`, `--attach`, `--reply-to`, `--interrupt`,
`--wake`, `--visibility`. The exclusion matrix collapses to three rules (G5).

Splitting scheduling out is *intended* to fix by construction the bug where scheduled
messages silently drop `--channel`, `--thread-id`, `--attach` and `--cc` and are re-authored as
`sender=scheduler` (`messaging-conversation-model-findings.md` §8): the scheduled event should store a complete envelope
including its conversation, and fire it with the original sender preserved.

> **Correction 2026-08-27 — this section named a command that does not exist, and I-1 is its
> consequence.** The earlier text read "`scion schedule message …` (already exists)". There is
> no `message` subcommand under `schedule`; the tree is `list | get | cancel | create |
> create-recurring | pause | resume | delete | history` (`cmd/schedule.go:766-774`). S4
> implemented the `--in`/`--at` deprecation warnings *faithfully against this paragraph* and
> so shipped a warning directing users at a command that has never existed. I found that in
> S5 and charged it to my verification of AC-15a; the deeper cause is here. **A design
> document that asserts a capability "already exists" is a claim about the code, and nothing
> was checking it.**
>
> What actually exists is `scion schedule create --agent <name> --message "…" --in 30m`. It
> **takes an agent, not a conversation** (`cmd/schedule.go:783-786`), so the envelope-preserving
> property this paragraph claims is unimplemented work rather than an existing capability. That
> gap is **DEF-6**.



Full proposed help text: **Appendix A**.

### 2.10 Validation

`Validate()` moves to a single choke point invoked on **every** inbound path — the CLI, the
Hub HTTP handlers, and broker-inbound alike. Today it is called on none of the hub paths
(`messaging-conversation-model-findings.md` §6), which is why Teams can emit a combination the validator forbids.

The validator's job shrinks considerably, because most of what it used to check is now
impossible to express: there is no channel string to regex, no thread ID to cross-check, no
recipient prefix grammar to parse. What remains is size limits, addressee membership
(every addressee must be a conversation participant), and conversation liveness.

### 2.11 Drift — the honest cost of Option A

The Hub now tracks entity lifecycle on platforms it does not control. Threads get renamed,
archived, and deleted without telling us. Designing for this from day one rather than later:

- Conversations carry `DriftState`: `active` | `orphaned` | `unresolvable`.
- **Reconciliation is lazy, never polled.** Polling every Discord thread across every
  deployment is untenable. State transitions happen on two triggers only: a send that fails
  with a platform "not found", and an inbound message that references the conversation
  (which resurrects an `orphaned` one).
- **Sends to an `orphaned` conversation fail fast with a specific error.** They do not fall
  back to the parent channel, and they do not broadcast. Fallback-on-failure is the exact
  behaviour that produces today's project-wide Discord broadcast bug.
- Orphaned conversations retain their history. They are archived, not deleted.

---

## 2.6.2 What `#<thread>` names (DEF-7, resolved by the native-chat architect)

*Added 2026-08-27 after consulting `nc-arch`. Supersedes the implied claim in §2.6 that
`#<thread>` resolves against `Conversation.DisplayName`.*

**The defect.** `resolveThread` matches `Conversation.DisplayName` (`resolve.go:429`) and nothing
in production ever writes that field. `UpsertConversationByExternalRef` also does an unconditional
`SetDisplayName` on its update branch (`conversation_store.go:400`), so a name set out of band is
wiped by the next upsert. The form is CLI-gated, so no user reaches it, but the design claimed it
worked.

**I framed this wrongly when I raised it.** I offered two options — `#general` names a native chat
room, or it names a broker thread. **Both assumed the naming lived in my entity.** It does not, and
the real answer is neither option as stated. Recorded because a question with two wrong options is
worse than no question: it invites a decision that forecloses the actual one.

**The answer.** `#general` names a **native chat thread**, and native chat already owns naming:

| | |
|---|---|
| Storage | `webchat_topic` (raw SQL, dual SQLite/PG — deliberately **not** Ent, per the `webchat_*` convention) |
| Name | **Required** for group threads, user-facing, **unique per project, case-insensitive** |
| Create | `POST /api/v1/chat/spaces/{projectId}/threads {name, defaultAgent?}` |
| Rename | `PATCH /api/v1/chat/threads/{topicId}` |
| Who | The system writes `#general` at project-create (plus lazy bootstrap for pre-existing projects; `is_general` rows cannot be renamed or deleted); thereafter any project member |
| DMs | **Deliberately nameless.** Identity is the canonical pair key; display name is derived from the peer at render time. |

**Consequences for this design.**

1. **Build no naming path, and invest nothing further in `Conversation.DisplayName`.** A second
   create/rename surface would be a competing source of truth for the same user-visible string.
2. **`#<name>` resolution must stay project-scoped. Never global.** Every project has a `#general`,
   so the name is maximally ambiguous without a scope. §2.6.1's ambient-project resolution is
   correct as written — independently confirmed rather than merely unchallenged.
3. **Fix the unconditional `SetDisplayName` overwrite regardless of anything else.** A write path
   that silently wipes an out-of-band name is a landmine for whoever ends up owning naming.
   Assigned to S6; agreed with `nc-arch`; needed under either outcome below.
4. **The final resolution target depends on an open decision.** See §2.6.3.

## 2.6.3 OPEN: `Conversation` and `webchat_topic` are parallel constructs

**Escalated 2026-08-27 13:06Z. Not an architect's call on either side.**

Two entities model the same concept, in two stores, both under active construction. My envelope
carries `conversation.id`; theirs has agents echoing `thread_id`.

| Option | Shape | Cost |
|---|---|---|
| **(i) Minimal** | `#<thread>` resolution reads `webchat_topic.name`, keyed by topic UUID. `Conversation.DisplayName` stays vestigial. | Cheap now. The two entities coexist **permanently**. |
| **(ii) Structural** | `webchat_topic` rows become — or 1:1-link to, e.g. via `external_ref` — `Conversation` rows; `thread_id` conventions become `conv:` references. | Right end state if messaging-v2 is *the* conversation model. But native chat's design is approved and implementation is in flight. |

**DECIDED 2026-08-27 13:28Z (user): (ii).** Native chat is **fully shipped and done** — no pending
wave. My original recommendation hedged on "sequence after wave 2 lands"; wave 2 had already
landed, and I had repeated another architect's stale description of their own system instead of
reading the code (rule 15). `webchat_*` is now a **stable target**, which is the safer thing to
spec a migration against.

**But the target is narrower than "unify" implies.** Native chat solved the native case, and
solved it well. messaging-v2's value is the surfaces it never covered — Discord, Slack, Teams,
Telegram — plus agent-side CLI addressing and one reference grammar across all of them. So:
**`Conversation` owns conversation identity across surfaces; `webchat_*` remains the native
projection and keeps its own read-state, prefs and presence.** A promotion of the identity layer,
not a migration of a working system into an unproven one.

**And do not implement `native` as an external integration.** The `external_ref`/`DriftState`
pattern exists *because we do not own the other schema* — we cannot add a column to Discord, and
their threads are renamed and deleted without telling us. We own `webchat_topic`; it cannot drift.
Copying that pattern here would be cargo-culting a workaround for a constraint we do not have. The
correct direction is the reverse pointer — `webchat_topic` carries a `conversation_id` — not
`Conversation` holding an opaque handle to our own table. **Surface-uniform identity;
surface-specific capability.**

**Superseded recommendation, kept for the record:**

Option (i) institutionalises across two stores and two projects **exactly the defect S6 is
currently being paid to fix inside one** — two constructs for the same DM that cannot see each
other. That is DEF-8, scaled up and made permanent, and it would arrive with a cross-store join
tax on every future reader. But unifying *now* destabilises approved, in-flight work for no urgent
gain. Declaring the direction without scheduling the migration buys the only thing that is
time-critical: **neither project builds more divergence starting today.**

**Already banked from this alignment, independent of the decision** — the shared DM key
(§2.4.2 as amended): one exported `DMConversationKey` / `ParseDMKey` in `pkg/messages`, consumed by
both projects. Not two implementations that agree by convention, which is how DEF-8 happened.

---

## 2.13 Making addressees real (DEF-9)

*Added 2026-08-27 14:42Z. Prior art grepped per rule 16 — findings below are cited, not recalled.*

**State of the code, verified on `origin/scion/messaging-v2`:**

| Element | Reality |
|---|---|
| `AddAddressee` | Defined at `entadapter/conversation_store.go:663`, declared at `store.go:1610`. **Zero callers.** The table is never written. |
| `DefaultAgentID` | **Written** at `handlers_agent_messaging.go:666`, `handlers_broker_inbound.go:217`, `backfill.go:298`. **Read nowhere.** Dead data. |
| `MessageAddressee.Via` | Already enumerates `explicit \| body-mention \| default-agent \| direct` (`models.go:1812`) — the vocabulary §2.4 needs already exists. |
| `pkg/messaging/delivery.go` | Contains one function, `FormatNewDelivery`. **There is no routing engine.** `conversation_id` is a stamp on a row. |

**No new semantics are required.** §2.4 already fixes the resolution order and it stands
unchanged: `kind=direct` → the other participant; else `DefaultAgentID` set → that agent, with
`Via: default-agent`; else → persisted, visible to all participants, **no agent woken**. §2.4
already states that case 3 is a real outcome and must be reported distinctly. DEF-9 is that
paragraph made executable. **This is implementation of a settled design, not a reopening of it —
and it needs no product decision, which I checked before escalating one.**

**2.13.1 The hazard, and it is today's lesson in a new place.**

Case 3 writes **zero addressee rows**. So does a bug that skips resolution entirely. So does a
crash between inserting the message and inserting its addressees. **Three very different events,
one observable.** That is precisely DEF-11 — an empty value standing for both "we looked and the
answer is none" and "we never looked" — and it will be materially worse here, because the
consequence is a message that silently woke nobody rather than a miscounted metric.

**Decision: record the decision, not merely its output.** The message row carries an
always-populated resolution outcome:

```
addressee_resolution ENUM('direct','default-agent','explicit','body-mention','none') NOT NULL
```

- `none` means *resolution ran and correctly selected nobody* — §2.4 case 3, a success.
- A row that reaches persistence without the field set is a **bug**, and is now visibly one.

The field is cheap, it is on the row that already exists, and it converts an ambiguous absence
into an explicit statement. **Do not infer "nobody was addressed" from an empty join.**

**2.13.2 Atomicity.** Addressee rows are written **in the same transaction as the message**. A
message that exists without its addressees is unrecoverable after the fact: nothing downstream
can distinguish it from case 3, and by then the sender is gone. If the store's current write path
cannot express this, say so and stop — **do not implement it best-effort and log on failure.**
That is the pattern that produced DEF-9 in the first place.

**2.13.3 `DeliveryState` is per-addressee and must be moved, not copied.** `pending → delivered |
failed` belongs to the addressee row. The message-level `DispatchState` remains for the legacy
path. Two writers on one concept is how the DM key ended up with two formats (§2.4.2); do not
repeat it. Where both exist during transition, the message-level field is derived and the
addressee rows are authoritative.

**2.13.4 Reading `DefaultAgentID`.** It is written in three places already, so the write side
needs no work — only the read at resolution time, per §2.4 step 2. **Confirm the three writers
agree** before relying on it; they were written independently and nothing has ever read them, so
nothing has ever forced them to be consistent. **Unread data has never been tested by use.**

**Acceptance criteria.**

- **AC-DEF9-1** A send into a `kind=direct` conversation with no explicit addressee writes
  exactly one addressee row, `Via: direct`, naming the other participant. Assert the row.
- **AC-DEF9-2** A send into a conversation with `DefaultAgentID` set and no explicit addressee
  writes one row with `Via: default-agent`, and **that agent is woken**. Assert the dispatch, not
  the row alone — the row is the record, the wake is the behaviour.
- **AC-DEF9-3** A send into a conversation with no default and no explicit addressee writes
  **zero addressee rows**, sets `addressee_resolution = 'none'`, wakes nobody, and the API
  reports "posted, nobody woken" distinctly from "posted, agent dispatched".
- **AC-DEF9-4** `addressee_resolution` is non-null on every persisted message. Permanent test
  **with a floor** — vacuous on an empty table.
- **AC-DEF9-5** Killing the transaction between message insert and addressee insert leaves
  **neither**. Mutation-verified.
- **AC-DEF9-6** Mutation: make resolution return an empty addressee set unconditionally. AC-DEF9-1
  and AC-DEF9-2 must both fail **by name**. If only one fails, the other is decorative.

## 2.12 Repairing the divergence comparison (DEF-11)

*Added 2026-08-27 13:46Z. Dispatchable now — see the note on the conflict that does not exist.*

**The defect.** When the CLI has already resolved a conversation, the Hub correctly skips
re-resolution but then builds its result object by hand and populates only one field
(`handlers_agent_messaging.go:828-832`):

```go
if structuredMsg.ConversationID != "" {
    storeMsg.ConversationID = structuredMsg.ConversationID
    convResult = &messaging.ConversationResult{
        ConversationID: structuredMsg.ConversationID,   // ExternalRef left empty
    }
}
```

The divergence comparison then tests that empty `ExternalRef` for a `dm:` prefix, finds none, and
records `routing-type-mismatch` (`divergence.go:176`). **The two models agree; the comparison is
being handed a blank.** Every `scion message @<agent>` send therefore reports as a mismatch.

**Why this is the gate, not a cosmetic bug.** The documented precondition for enabling the read
switch is non-zero matches and **zero** mismatches. DEF-11 makes zero unreachable, so the switch
could only be enabled by overriding its own safety criterion — which converts a gate into a
formality. Nothing downstream of the read switch can proceed honestly until this is fixed.

**Decision: populate `ExternalRef` by loading the conversation.** The handler has the ID; it
reads the row and copies the ref onto the hand-built result.

**Alternative rejected — treat an empty `ExternalRef` as "not compared" rather than a mismatch.**
Superficially attractive: it is a one-line change and the board goes green. That is precisely
what makes it wrong. Nearly all new-model traffic arrives CLI-resolved, so this would silence the
comparison on the majority of sends while reporting clean. **This is rule 14 applied to a system
rather than a test: a check whose input can silently become empty is not a check, and one that
reports success on an empty input is worse than no check at all.** The blank is the symptom; the
fix is to stop producing it.

**Cost.** One indexed read by primary key on a path that already performs a write. Acceptable. If
it later proves not to be, the correct optimisation is for the CLI to send the ref it already
computed — not to weaken the comparison.

**Note on sequencing — I held this work for a conflict that does not exist.** DEF-11 was deferred
to a later section on the belief that its fix would collide with S6 in
`handlers_agent_messaging.go`. Checked 13:45Z: `git diff --stat messaging-v2..ca-msg-em6 --
pkg/hub/` is **empty**. S6 touches no file in `pkg/hub`. The premise was never verified — the
same failure as rule 15, applied to a scheduling decision rather than a design claim. Rule 15 is
hereby read to cover **any** premise that gates action, not only capability claims in prose.

**Acceptance criteria.**

- **AC-DEF11-1** A send carrying a pre-resolved `ConversationID` produces a `ConversationResult`
  whose `ExternalRef` equals the stored conversation's `external_ref`. Asserted on the value, not
  on the fact that a loader was called.
- **AC-DEF11-2** Two sends to the same agent — one legacy, one CLI-resolved — produce a
  divergence **match**, not a mismatch. This is the observable that matters.
- **AC-DEF11-3** A pre-resolved ID naming a conversation that does not exist, or one whose ref is
  empty, is recorded as a **fallback** with a distinct reason string — never silently as a match.
  Mutation-verified: make the loader return an empty ref and confirm the named test fails.
- **AC-DEF11-4** The mismatch counter has a **floor** in test: a run with known-divergent traffic
  must report non-zero mismatches, so that a comparison which has stopped comparing cannot pass
  as clean.

## 2.11 Running the backfill (DEF-12)

*Added 2026-08-27, after the integration-hub deploy. Describes work that does not exist yet.*

**The defect.** `pkg/messaging/backfill.go` is complete — batching, resume, dry-run, progress
reporting — and **nothing calls it.** `git grep 'Backfill' -- '*.go'` at `ebf8cc27`, excluding the
file itself and `_test.go`, returns zero hits. No CLI subcommand, no admin endpoint, no startup
hook. Every message predating this branch therefore has an empty `conversation_id` permanently.

This is a different failure from DEF-7..DEF-11. Those are seams between sections. This is a
finished component with no doorway, which is harder to notice precisely because the code reviews
clean and its unit tests pass.

**Decision: a `sciontool` subcommand. Not a startup hook.**

```
sciontool messaging backfill [--dry-run] [--batch-size N] [--resume-from <id>] [--max N]
```

**Why not a startup hook**, which is the tempting option because it needs no operator action: it
converts a deploy into an unbounded write over the whole message table, on a hub whose message
count nobody has checked, with no operator watching and no way to stop it short of killing the
process mid-batch. A migration that cannot be paused should not be attached to the one event that
must always succeed. Rollout is also the moment you least want a long-running write competing with
live traffic.

| Alternative | Rejected because |
|---|---|
| Run on server startup | Above. Also makes the backfill's runtime a component of boot time, so a slow backfill reads as a failed deploy. |
| Admin HTTP endpoint | Needs progress streaming, cancellation, and timeout handling over HTTP to be usable on a large table — that is a job queue, and we do not have one. A CLI process the operator owns gets all of this from the terminal for free. |
| Leave it unwired; let the read switch tolerate null `conversation_id` | This is the current state by accident, and it is defensible **only** while the switch is off. Once on, historical messages read as unrouted, which is indistinguishable at the UI from data loss. |

**Requirements.**

1. `--dry-run` is the default posture in documentation: report what would change, write nothing.
2. Idempotent. Running twice must not double-stamp or produce a second conversation. The existing
   service claims this; the command must have a test that proves it by running the backfill twice
   and asserting the row count and every `conversation_id` are unchanged after the second pass.
3. Resumable, and the resume token must be logged on every batch — an operator who loses the
   terminal must be able to continue without starting over.
4. **Ordering constraint: this runs *after* DEF-8 lands, never before.** The backfill creates DM
   conversations; doing that while two divergent creation paths exist would mass-produce exactly
   the duplicate rows §2.4.2 exists to eliminate. **This is a hard dependency, not a preference.**

**Acceptance criteria.**

- **AC-DEF12-1** The command exists, appears in `sciontool --help`, and `--dry-run` writes nothing —
  asserted by snapshotting the message and conversation tables before and after and comparing.
- **AC-DEF12-2** A real run stamps `conversation_id` on messages that lacked one. The test asserts
  the count of newly-stamped rows is non-zero (rule 14 — a backfill test that finds nothing to
  backfill and passes is the exact failure mode we keep hitting).
- **AC-DEF12-3** Running twice changes nothing on the second pass.
- **AC-DEF12-4** A permanent test asserts the command is reachable from the root command tree, so
  this defect — a working component with no caller — cannot recur silently.

---

## 2.14 The CLI section — scheduled sends (DEF-6) and the undocumented grammar (DEF-13)

Paired because both are CLI-surface work with no overlap into `pkg/hub` or `pkg/ent`, so this
section can run alongside store-layer work without contention. That co-scheduling claim is a
**measurement, not a property** — whoever takes this re-runs the scope check at merge.

### 2.14.0 Prior art (rule 16)

Grepped on `origin/scion/messaging-v2` for this design's own nouns before speccing:

| Fact | Location |
|---|---|
| `ScheduledEvent.Payload` is a free-form JSON string, "handler-specific" | `pkg/store/models.go:1835` |
| `ScheduledEvent.CreatedBy` exists and is populated | `pkg/store/models.go:1838` |
| `MessageEventPayload` = AgentID, AgentName, Message, Interrupt, Plain | `pkg/hub/server.go:2761-2767` |
| Payload auto-constructed from convenience fields; requires `agentId` **or** `agentName` | `pkg/hub/handlers_scheduled_events.go:183-201`, `handlers_schedules.go:200` |
| Fire-time handler re-authors the sender: `NewSystemMessage("scheduler", …)`, `SenderID = "SCHEDULER"` | `pkg/hub/server.go:2830-2832` |
| **`dispatch_agent` already resolves `evt.CreatedBy` at fire time and authorizes as that principal, failing closed if the creator is gone or lacks scope** | `pkg/hub/server.go:2855-2875` |
| `scion schedule create` exposes `--agent`, `--message`, `--in`, `--at`, `--interrupt` — no conversation flag | `cmd/schedule.go:782-786` |

**Two things this falsifies, both mine.**

1. My DEF-6 ledger row says "there is nowhere on a scheduled event to put a conversation." False.
   `Payload` is an opaque JSON blob and `MessageEventPayload` is an ordinary struct; adding a field
   is additive and needs **no schema migration**. I asserted a storage constraint without reading
   the storage. Rule 15 applies to my own ledger, and a ledger row is exactly the kind of claim that
   gets inherited without re-checking.
2. I scoped DEF-6 as novel design work. It is mostly **not**. The `dispatch_agent` path already
   implements fire-time creator resolution with fail-closed authorization. The message path simply
   does not use it. **The design here is to extend an existing mechanism, not to invent one** — and
   had I not grepped, I would have specced a parallel one.

### 2.14.1 DEF-6 — a conversation reference on a scheduled message

**Add one field.** `MessageEventPayload.ConversationRef string \`json:"conversationRef,omitempty"\``.
`omitempty` plus Go zero-values means every existing pending event unmarshals unchanged with an
empty ref — the compatibility story is "old rows keep working", not a backfill.

**Resolve at fire time, never at create time.** A conversation can be archived, renamed, promoted
(§2.4.2.3) or deleted between scheduling and firing, and `--at` accepts arbitrarily distant times.
Create-time resolution stores an answer that silently rots; fire-time resolution can fail loudly.
Create time validates *grammar only* — that the reference parses — so typos are caught while the
user is still present.

**Authorize the creator at fire time, not the scheduler.** This is the load-bearing rule.

> A scheduled send is a **deferred act by its creator**, not an act by the scheduler. If the
> scheduler's own identity authorizes the write, then "schedule a message into a conversation I am
> not a member of" is DEF-14 with a delay — and worse, because a fired event has no interactive
> caller to attribute it to.

Follow `authorizeScheduledAgentCreate` (`server.go:2855`): resolve `evt.CreatedBy`, fail closed if
the creator no longer exists, is in a different project, or is not a participant in the target
conversation. Membership is checked **at fire time** against the conversation as it then exists,
because membership can be revoked after scheduling. Ties directly to AC-INGRESS-1: same rule, third
verb — D-1 governs joining, AC-INGRESS-1 governs writing, this governs writing *later*.

**Preserve the original sender.** `SenderID = "SCHEDULER"` is the re-authoring bug from
`messaging-conversation-model-findings.md` §8. The fired message should carry the creator as sender and record the scheduler as
the *delivery mechanism*, not the author. Deliberately left as an open question below rather than
specced — see 2.14.3.

### 2.14.2 DEF-13 — document the grammar that shipped

`cmd/message.go:98-114`. The `Long` text lists only `<agent-name>`, `agent:<name>`, `user:<name>`,
`group[...]`, and all three examples are legacy. Add `@<agent>`, `@<email>`, `conv:<uuid>` and
`#<thread>`, **including the two that currently error by design**, each annotated with its status,
so a user meets the limitation in the help text rather than in a failed command. The deprecation
warnings at `:86-91` already point at `@<agent-name>`; after this change the form they name is
defined in the same output.

**My spec gap, not a section's.** I wrote ACs requiring the warnings to fire and the reference forms
to work, and none requiring the help text to describe them. Both managers built what I asked. The
generalisable rule, now standing: **a user-facing grammar is not shipped until `--help` describes
it; an AC that covers behaviour but not discoverability covers half the feature.**

### 2.14.3 Open question — sender attribution on a fired message

Preserving the creator as sender is clearly right for attribution and clearly interacts with things
I have not traced: the `SystemCategoryScheduler` classification, whatever UI filters on
`sender = "scheduler"`, and the harness-side rendering of system messages. I am not speccing a
change to a field whose readers I have not enumerated. **Whoever takes this section enumerates the
readers of `SenderID == "SCHEDULER"` and `SystemCategoryScheduler` first and reports back before
changing either.** If the answer is that changing it breaks rendering, then the fix is an added
`OnBehalfOf` field rather than a changed `SenderID`, and that is a smaller change.

### 2.14.4 Alternatives considered

- **A new `conversation_id` column on `scheduled_events`.** Rejected: `Payload` is already the
  handler-specific extension point, a column adds a migration for zero benefit, and a *reference*
  is the right thing to store rather than a resolved ID — which is the create-time-resolution
  mistake wearing a schema.
- **Resolve at create time and store the conversation ID.** Rejected above: stores an answer that
  rots silently. Its one real advantage — the error surfaces while the user is watching — is
  recovered by validating grammar at create time.
- **Authorize as the scheduler.** Rejected: it is DEF-14 with a delay and no interactive caller.
  Recorded because it is the path of least resistance and the one a developer lands by accident,
  since the scheduler identity is already in hand at fire time.
- **Leave DEF-13 to the docs site.** Rejected: the deprecation warning names a form the help does
  not define, so the user who follows our own advice is the one who gets stuck. Docs are additive
  to this, not a substitute.

### 2.14.5 Acceptance criteria

- **AC-DEF6-1** `scion schedule create` accepts a conversation reference; grammar is validated at
  create time and a malformed reference fails immediately with a non-zero exit.
- **AC-DEF6-2** Firing resolves the reference **at fire time**. Test: schedule against a
  conversation, mutate it (archive/delete) before firing, assert the event fails loudly with the
  reason recorded on `ScheduledEvent.Error` — not silently dropped. Per rule 13 assert the stored
  error, not the return value.
- **AC-DEF6-3** **Creator authorization at fire time.** A scheduled message whose creator is not a
  participant in the target conversation does not deliver. Assert no message row is created, not
  merely that the handler errored. Floor per rule 14: the equivalent event from a legitimate
  creator *does* produce a row, so the count query is proven to observe the right table.
- **AC-DEF6-4** Creator deleted between schedule and fire → fails closed, error recorded.
- **AC-DEF6-5** A pending event created **before** this change fires unchanged. Test against a
  payload JSON literal lacking `conversationRef`, not against a struct built by current code — the
  point is wire compatibility with rows already in the database.
- **AC-DEF6-6** `--agent` continues to work unchanged and is not deprecated by this section.
- **AC-DEF13-1** `scion message --help` documents `@<agent>`, `@<email>`, `conv:<uuid>` and
  `#<thread>`, each with its current status, and at least one example uses the `@` form.
- **AC-DEF13-2** A test asserts the `Long` text mentions every reference form the parser accepts,
  enumerated **from the parser** rather than from a hand-written list, so a future grammar addition
  fails this test instead of silently shipping undocumented. This is the AC I should have written
  the first time.

### 2.14.6 Phases

1. DEF-13 help text + AC-DEF13-2's parser-derived test. Self-contained; land first so the smaller
   change is not queued behind the larger one.
2. Enumerate readers of `SenderID == "SCHEDULER"` / `SystemCategoryScheduler`; report before coding.
3. `ConversationRef` on the payload, create-time grammar validation, `--conversation` flag.
4. Fire-time resolution + creator authorization + fail-closed error recording.
5. Sender attribution, only if phase 2 shows it is safe; otherwise `OnBehalfOf`.

---

## 2.15 One key derivation, not five (DEF-15, DEF-16)

### 2.15.0 Prior art in this repository (rule 16)

Grepped `2724ed10` for the section's core nouns — `dm:` prefix discrimination and
`thread:%s:%s` key construction.

| Site | What it does | Discriminates `dm:`? |
|---|---|---|
| `pkg/hub/events.go:760,768` | web-channel event routing | **Yes** |
| `pkg/hub/messagebroker.go:512,534` | delivery `switch` on `storeMsg.ThreadID` | **Yes** |
| `pkg/hub/webchannel.go:90` | web channel dispatch | **Yes** |
| `pkg/hub/handlers_agent_messaging.go:350` | chat notifier `NotifyDMReceived` | **Yes** |
| `pkg/hub/handlers_chat_v2.go` (11 sites) | read gates, participants, search | **Yes** |
| `pkg/messages/dm_key.go:91` | canonical DM key parse | **Yes** |
| `pkg/messaging/dm_migration.go:135` | DM row migration | **Yes** |
| `pkg/messaging/conversation.go:158` | `ResolveOrCreateThreadConversation` — **write** | **No** |
| `pkg/messaging/conversation.go:198` | `ResolveThreadConversationForRead` — **read** | **No** |
| `pkg/messaging/backfill.go:195` | `groupForMessage` — **bulk write** | **No** |
| `pkg/hub/handlers_agent_messaging.go:245` | outbound dual-write call site | **No** |
| `pkg/hub/handlers_agent_messaging.go:848` | agent-message dual-write call site | **No** |

**This table is the design.** Nineteen sites in the delivery, notification and read paths
treat a `dm:`-prefixed `ThreadID` as a first-class DM signal. Five sites in the conversation
layer do not. **The convention is not in question; it is load-bearing in production today and
predates this project.** The conversation layer is the outlier, and it is the only layer that
mints rows.

**DEF-15 is five sites, not the one found in review.** Two matter more than the three that
were visible:

- **`conversation.go:198` (read).** After a write-side fix, a read-side that still derives
  `thread:<proj>:dm:…` looks up a key nothing writes. **A partial fix here does not leave a
  cosmetic inconsistency; it silently empties the read switch's DM lookups.**
- **`backfill.go:195` (bulk).** The backfill is a *volume* producer of the malformed shape:
  every historical DM-keyed message becomes a `kind='group'` project-scoped row. Its own
  comment reasons carefully about global DMs — "DMs are global per design 2.4.1; the
  external_ref must match what dual-write produces" — in the `else` branch, having never
  considered that a DM can arrive down the `if msg.ThreadID != ""` branch. **The author
  reasoned correctly about the case they were looking at.**

> **SEQUENCING BLOCKER, and it is mine.** §2.11 (DEF-12) is listed as unblocked and ready to
> dispatch. **It must not be dispatched before this section lands.** Running the backfill today
> converts a latent defect into a populated table. This is a hazard I created in my own plan by
> ordering the work before finding DEF-15.

### 2.15.1 The decision: route, do not refuse

The question logged with DEF-15 was whether a `dm:`-prefixed `ThreadID` should route to DM
resolution or be refused at ingress. **The prior-art table answers it: refuse is not available.**
Nineteen consumers depend on the form, PR #1319 format-validates it, and PR #1322 now
authorizes it. Refusing at ingress would break delivery and notification to close a defect that
lives in row creation.

**The fix is not a guard at each of the five sites.** That is the shape §5u and §5x already
recorded twice this project: adding a check to each unguarded path leaves the system better
defended and less likely to be defended further, and it leaves the two `conversation.go`
functions mis-deriving for any future caller. The defect is not five missing checks. **It is
that there are five key-derivation functions and they disagree.**

Collapse them into one:

```go
// package messaging — the ONLY place a conversation external_ref is derived.
//
// Returns the canonical external_ref, the conversation kind, and the project
// scope (nil for direct conversations, which are global per §2.4.1).
//
// Parse failure returns an error. Callers MUST NOT create a row on error and
// MUST NOT fall back to a constructed key: any guess on any input to the key
// derivation is a guess on the ACL.
func DeriveConversationKey(in KeyInputs) (extRef string, kind string, projectID *string, err error)

type KeyInputs struct {
    ThreadID      string // may be a dm: key, a native thread id, or empty
    ProjectID     string
    SenderKind    string
    SenderID      string
    RecipientKind string
    RecipientID   string
}
```

Derivation, in order:

1. `ThreadID` has prefix `dm:` → **must** satisfy `validDMKey`. On pass, return the key
   **verbatim**, `kind = "direct"`, `projectID = nil`. On fail, **error** — do not fall through
   to the thread branch. Falling through is precisely how DEF-15 produces its row.
2. `ThreadID` non-empty, no `dm:` prefix → `thread:<projectID>:<threadID>`, `kind = "group"`,
   project-scoped. Empty `projectID` is an **error**, not a warning-and-nil as today.
3. `ThreadID` empty → derive from the principal pair via the existing
   `messages.DMConversationKey`, `kind = "direct"`, `projectID = nil`.

Note (1) returns the supplied key **unmodified**. The `dm:` key is already canonical and
already the ACL; re-deriving it from parsed halves would introduce a second chance to differ
from the string the read path gates on.

**Canonicality check — settled 2026-08-27 17:00Z.** "Has a `dm:` prefix and parses" is not
canonical. `ParseDMKey` (`pkg/messages/dm_key.go:88`) enforces neither token order nor UUID
formatting, while `DMConversationKey` (`:39`) sorts tokens lexicographically and re-renders each
UUID through `uuid.Parse` to canonical lowercase hyphenated form. And `uuid.Parse` accepts 32
unhyphenated hex digits, `{braced}` and `urn:uuid:` forms — so
`dm:agent:<32-hex-no-hyphens>:user:<uuid>` is all-lowercase, parses cleanly, and is a **different
string** from the canonical key for the same pair. Two external_refs, one DM, two rows: DEF-8,
reintroduced by the function built to prevent it.

Therefore case 1 **round-trips**: parse the halves, re-derive with `DMConversationKey`, compare
to the input, and **error if they differ**. This does not contradict "return verbatim" — that
rule governs the *return value*, not what may be compared against. What it forbids is returning
the re-derived key: **never normalise here.** Silently rewriting a key makes the stored identity
differ from the string a caller may already have authorised against, which is §2.15.4(c)'s
read-gate normalisation moved to the write side. Differ means error.

Rejected alternative: checking canonical *attributes* (e.g. all-lowercase). **Canonicality is
defined by the function that produces keys, not by a list of properties canonical keys happen to
have.** An attribute list drifts from its subject as the producer changes; a round-trip cannot.

**`DirectMessageExternalRef` is the sixth derivation and must not survive this section.**
`pkg/messaging/divergence.go:132` builds the kind-free `dm:{sorted(idA,idB)}` form and **never
returns an error** — its doc states that an empty ID "produces a ref that makes the divergence
visible", the exact inverse of this section's fail-closed rule. Its only production caller is
`backfill.go:201`, which phase 4 repoints; after that it is production-dead. Delete it, or
unexport it and confine it to the divergence tests that need the legacy shape. Shipping "one key
derivation" alongside an exported, callable, non-failing alternative in the same package is
shipping two.

All five sites become callers. `ResolveOrCreateThreadConversation` and
`ResolveThreadConversationForRead` keep their names and signatures for their existing callers
but delegate; `backfill.go:195` and both handler call sites lose their inline construction.

> **Golden test vectors are a security control here, not a consistency check.** Once
> `handlers_chat_v2.go` authorizes by parsing this key, `DeriveConversationKey` is the ACL
> derivation. A vector table pinning input → `(extRef, kind, projectID, err)` for every branch,
> including every error branch, is required. **Every error case gets a vector**, because the
> failure mode this section exists to fix was an error case falling through to a success path.

### 2.15.2 Ordering: validate before you persist (DEF-16)

`handleAgentOutboundMessage` dual-writes at `:245`/`:247` and validates at `:288`.
`handleAgentMessage` validates at `:615` and dual-writes at `:848`/`:851`. **The same two
operations, opposite orders, no comment anywhere saying which is intended** (standing rule 19).

Observed consequence: a request rejected `400 thread_id requires channel to be set` still leaves
a conversation row, with no message attached and no participants. Before PR #1322 that extended
to *unauthorized* keys — the refusal was real and its side effect outlived it.

**Decision: validate first, in both handlers.** `handleAgentMessage` is already correct; move
the outbound dual-write to after `ValidateLegacyMessage`.

The argument for the current outbound order is that resolving early lets a conversation be
recorded for a message that later fails a soft check. **That is not worth an orphan.** A
conversation row is an assertion that a venue exists; the system should not assert it on behalf
of a request it then refuses. Today the orphans are inert — no read path returns them — but they
stop being inert the moment anything treats a conversation row as evidence a conversation
happened: `external_ref` uniqueness, participant listings, or the DEF-12 backfill.

**This is a behaviour change to a Phase-5 non-fatal path and must be called out in review**, not
folded in silently: a message that today gets a `conversation_id` despite failing validation will
no longer get one. Nothing reads that association, which is why it is safe — but "nothing reads
it" is a claim the implementer cites, not one they inherit from this paragraph.

### 2.15.3 Migration: the rows already in the database

Any conversation whose `external_ref` matches `thread:%:dm:%` is a DEF-15 artifact. Reconcile
through the existing `dm_migration.go` machinery rather than a new sweep.

For each such row, the embedded suffix after `thread:<projectID>:` is the intended key.

- Suffix satisfies `validDMKey` → the row's true identity is that key, `kind = 'direct'`,
  `project_id = NULL`. If a correct row already exists for that key, **merge into it**; the
  correct row wins.
- Suffix does **not** satisfy `validDMKey` → **leave the row exactly as it is** and report it.
  Do not repair, do not guess, do not delete. Under-granting is recoverable; over-granting is
  not.
- Row carries participants (it will, being `kind='group'`) that disagree with the key → **the
  key wins.** Participant tables are a derived listing index, never the access authority. A
  disagreement is reported, and the index is rebuilt from the key.

All-or-nothing per row. A half-migrated DM lists asymmetrically, which is worse than an
unmigrated one because it looks finished.

### 2.15.4 Alternatives considered

**(a) Guard at each of the five call sites.** Rejected. It is the failure mode this project has
now recorded three times (§5o, §5u, §5x): a per-site check leaves the derivation functions
themselves wrong, so the sixth caller reintroduces the defect and does so quietly. It also
leaves five copies of a security-critical decision.

**(b) Refuse `dm:`-prefixed `ThreadID` at ingress; require the principal pair.** Rejected on the
prior-art table — nineteen consumers, two merged PRs, and the native chat surface all depend on
the form. This alternative was the leading candidate when DEF-15 was logged, and the grep is
what killed it. Recording that here because the ledger row still poses it as an open choice.

**(c) Normalise the malformed key at the read gate — teach `isDMParticipant` to unwrap
`thread:<proj>:dm:…`.** Rejected, and it is the most dangerous of the three because it works.
It would make the injected rows readable by the right people and the symptom would disappear.
It also makes the read gate responsible for undoing a write-side mistake, which means the ACL
now depends on a normalisation step that the write path can change out from under it. **Fix the
key at the point of derivation or the key is not the ACL.**

**(d) Leave DEF-16 ordering as it is and clean orphans periodically.** Rejected. A reaper cannot
distinguish an orphan from a legitimately empty new conversation without a race.

### 2.15.5 Acceptance criteria

- **AC-DEF15-1** — `DeriveConversationKey` is the only function in `pkg/` that constructs a
  `thread:` or `dm:` external_ref. **Verified by a test that reads the package source, not by a
  grep** — a grep runs when someone remembers, and this is an architectural constraint that has
  to survive people who never read this document. Prior art for source-reading tests:
  `cmd/doc_syntax_test.go`. The check must cover `DirectMessageExternalRef` as well as
  `fmt.Sprintf("thread:`, or it certifies "one derivation" while a second sits in the same
  package.
- **AC-DEF15-1b** — case 1 rejects every non-canonical spelling of a valid pair: reversed token
  order, uppercase UUID, unhyphenated 32-hex UUID, braced UUID, `urn:uuid:` UUID. Each is a
  golden vector, and each expects an **error**, not a normalised key.
- **AC-DEF15-2** — A golden vector table covers all three branches and **every** error branch;
  each vector pins `(extRef, kind, projectID, err)`.
- **AC-DEF15-3** — An outbound message with `thread_id = dm:agent:X:user:Y` produces exactly one
  conversation row: `kind='direct'`, `external_ref` equal to the supplied key **verbatim**,
  `project_id IS NULL`. This is S6's restored `attachments_agent_test.go` assertion with the
  `t.Skip` removed — **deleting that Skip line is the acceptance criterion**, not writing a new
  test.
- **AC-DEF15-4** — A `dm:`-prefixed `ThreadID` failing `validDMKey` creates **no** conversation
  row of any kind. Verified by asserting a row count, not by asserting an error string.
- **AC-DEF15-5** — Read and write derive identically: for the same inputs,
  `ResolveThreadConversationForRead` finds the row `ResolveOrCreateThreadConversation` wrote.
  A test that writes then reads, not two tests that each assert a literal.
- **AC-DEF15-6** — Backfill over a fixture containing DM-keyed `ThreadID` messages produces
  `kind='direct'` global rows, and produces **zero** rows matching `thread:%:dm:%`.
- **AC-DEF16-1** — A request rejected by `ValidateLegacyMessage` at either handler leaves **no**
  conversation row. Verified by row count before and after.
- **AC-DEF16-2** — Both handlers validate before they persist. Verified by mutation: move the
  validation call back below the dual-write and confirm AC-DEF16-1's test fails **by name**.
- **AC-MIGRATE-1** — Migration over a fixture containing one repairable and one unrepairable
  `thread:%:dm:%` row repairs the first, leaves the second byte-identical, and reports both.

### 2.15.6 Implementation phases

1. `DeriveConversationKey` + golden vectors. No call sites changed. Lands green and alone.
2. Repoint `conversation.go:158` and `:198` at it. AC-DEF15-1, -2, -5.
3. Repoint the two handler call sites; unskip S6's test. AC-DEF15-3, -4.
4. Repoint `backfill.go:195`. AC-DEF15-6. **Gates §2.11.**
5. DEF-16 ordering + its mutation check. AC-DEF16-1, -2.
6. Migration sweep. AC-MIGRATE-1.

Phases 1–2 touch no handler and can run concurrently with unrelated sections. Phases 3–5 all
touch `handlers_agent_messaging.go` and must be one manager's serial work.

---

## 3. Alternatives Considered

### 3.1 Native-only Conversation; external stays an opaque tuple (Option B)

Make Conversation a first-class entity for native chat only, and keep external venues as
`(channel, thread_id)` — but as one typed, required, validated value rather than two
loosely-coupled optional fields.

**Rejected.** Much smaller blast radius and it does fix the silent-misrouting class, but it
leaves the root cause intact: two addressing systems, merely tidier. Reply affinity still
cannot remember an external thread without a bolt-on column, and there is no common model
for native features to degrade *from* — which was G4, the stated reason for doing this at
all. Presented to the owner as the explicit alternative at the decision point; Option A was
chosen with its costs understood.

### 3.2 Require and validate `channel` + `thread_id`; add no new entity

The cheapest option. Make both fields mandatory together, validate the pair against a
per-surface registry, and add a `last_thread_id` column to reply affinity.

**Rejected.** It fixes the reported symptom for roughly a tenth of the cost, and if the
goal were only "stop losing messages" it would be the right answer. But it cannot deliver
G3 or G4: `thread_id` remains a seven-way untyped union, the default agent still lives in
three places, participants remain unknowable so "who gets woken" stays unanswerable, and
the type enum still conflates delivery artifacts with message kinds. It buys a year.

### 3.3 Adopt an existing protocol as the internal model (Matrix, A2A)

Matrix's room/event model is a close conceptual fit — rooms are conversations, membership
is explicit, and bridging to external platforms is its core competence.

**Rejected.** The parts of Matrix that make it attractive are inseparable from federation:
state resolution, event DAGs, and eventual consistency across homeservers. Scion's settled
architecture has no hub-to-hub addressability (`messaging-conversation-model-findings.md` §11.7), so we would import
substantial complexity to model a distributed problem we have decided not to have. The
bridging patterns are worth borrowing at the design level — this proposal's broker-edge
resolution is essentially a Matrix application-service bridge — without adopting the
protocol.

### 3.4 Keep `recipient` primary; add `conversation` as metadata

Introduce Conversation as an additional, optional field for native chat features while
leaving recipient as the address.

**Rejected.** This is how the system reached its current state: `channel` and `thread_id`
were themselves added as optional qualifiers alongside a primary recipient. Adding a third
optional qualifier to the same envelope reproduces the failure mode at a larger scale.

### 3.5 No Conversation entity: the root message ID *is* the conversation ID

The email model. Every message carries `in_reply_to`; conversation identity is the ID of the
root message, and a thread is the transitive closure of the reply graph (RFC 5322
`References`). This is a real, long-lived design, and it is the strongest form of "if we have
message IDs, why do we need conversation IDs?"

It would genuinely remove an entity, and reply chains would be exactly faithful to how Discord
and Slack render quoted replies.

**Rejected**, for the reasons in §2.3.1 — chiefly:

- **Conversations must be able to exist before they contain messages.** Native chat opens a DM
  pane, and a broker registers a Discord thread, before anyone speaks. Under this model those
  states are unrepresentable, which is fatal for the native surface we are trying to
  differentiate.
- **Deleting the root destroys the identity of the whole thread.** Email tolerates this because
  identity is client-side and best-effort; we have foreign keys, read state, and participant
  rows hanging off it.
- **Container state has no owner.** Participants, default agent, and the `(Surface, ExternalRef)`
  mapping are mutable and thread-wide; the root message is immutable and has no claim to them.

Email also demonstrates the cost directly: threading is famously unreliable across clients
precisely because it is *derived* rather than *stored*, and every client re-derives it slightly
differently. That is the failure mode this proposal exists to eliminate.

The useful part of the idea is kept: `--reply-to` may be given *instead of* naming a
conversation, and the CLI resolves the container from it (§2.3.1).

---

## 4. Migration / Rollout

Six phases. Phases 0–2 are invisible to users; the contract does not change until Phase 3.

| Phase | Change | Reversible? |
|---|---|---|
| **0** | Add `conversations`, `conversation_participants`, `message_addressees` tables. No reads, no writes from live paths. | Yes — drop tables |
| **1** | Backfill (§4.1). Dual-write: every send resolves-or-creates a conversation and stamps `conversation_id` alongside existing `channel`/`thread_id`. Reads unchanged. | Yes — stop writing |
| **2** | Reads switch to `conversation_id`. Old fields still populated for compatibility, now derived from the conversation. Divergence between old and new routing is logged as an error and alerted on. | Yes — flip reads back |
| **3** | New envelope and new CLI surface. Old flags accepted with deprecation warnings that state the replacement. Old `type` values accepted and mapped. | Partially |
| **4** | Broker adapters resolve external refs at the edge. Platform IDs stop crossing the hub boundary. | Partially |
| **5** | Remove `channel`, `thread_id`, `recipient*`, and the old `type` enum. | **No** |

Phase 2 is the load-bearing gate. It should run with divergence logging in production for a
meaningful period before Phase 3 begins, because it is the last point at which the old and
new models can be compared against live traffic.

### 4.1 Backfill rules

| Existing state | Becomes |
|---|---|
| `channel=web`, `thread_id`=topic UUID | native group conversation; direct lookup in `webchat_topic` |
| `channel=web`, `thread_id=dm:…` | direct conversation; participants parsed from the key |
| `channel=web`, `thread_id=agent:<slug>` (wave-1) | direct conversation; slug resolved to agent UUID |
| `channel=<external>`, `thread_id` set | external conversation; `external_ref` = the value, `surface` = the channel; participants seeded from observed senders/recipients |
| `channel` set, `thread_id` empty | **surface-level conversation** — the channel itself as a venue. This case must be representable; a Discord channel with no threads is a legitimate conversation. |
| both empty | no conversation. Best-effort mapping to a direct conversation per (project, principal-pair), flagged `backfill_inferred`. These messages are already broken today; the flag makes that visible instead of inventing history. |

Two backfill hazards, both already present in the data:

- The wave-1 thread backfill can emit email-based DM keys that fail the UUID regex
  (`messaging-conversation-model-findings.md` §9). These will not parse into participants and must be routed to the
  `backfill_inferred` path rather than dropped.
- `WebChatTopic.DefaultAgent` is a slug-or-UUID union. Backfill must resolve both forms and
  normalise to a UUID; unresolvable values become `NULL` with a logged warning, not an
  arbitrary guess.

### 4.2 Compatibility during Phase 3

- Old CLI flags map mechanically: `--channel X --thread-id Y` → resolve `(X, Y)` to a
  conversation. If it resolves, proceed with a deprecation warning naming the `conv:` form.
  If it does not, **fail** — do not fall back.
- `--cc` maps to `--to`.
- Old `type` values map: `instruction`→`request`, `chat`/`assistant-reply`→`inform`,
  `input-needed`→`question` when addressed / `event:agent.input-needed` when broadcast,
  `state-change`→`event:agent.state-changed`, `system`→the matching event type by
  `system_category`, `mention`/`group-set`→dropped (the addressee set carries it).
- The `---BEGIN SCION MESSAGE---` delimiters are unchanged. Only the JSON body changes
  shape. Agents parsing by delimiter keep working; agents reading specific fields need the
  updated skill (Appendix B).

---

## 5. Open Questions

### Q1 — Mention fan-out: refinement of a settled decision — **RESOLVED 2026-08-24**

> **Resolution (ptone@google.com):** approved. One message row, N addressee rows. The
> `mention` type is removed from the taxonomy; `metadata.mention_source` is removed. Any
> "your mentions" view queries `message_addressees WHERE via = 'body-mention'` rather than
> counting message rows. Recorded in AC-9.

`messaging-conversation-model-findings.md` §11.2 records that body mentions produce **separate messages**, that brokers
send **separate inbound POSTs per mention**, and that bundling and hub-side parsing were
both explicitly rejected.

This design keeps one message with N addressees. The proposed reconciliation:

- Brokers still send separate POSTs. Unchanged.
- Brokers still do all mention parsing. The Hub still parses nothing. Unchanged.
- Each POST carries the platform's `external_message_id`, identical across the N posts.
- The Hub deduplicates on `(surface, external_message_id)` into **one** message row with
  **N** addressee rows.

I believe this honours both the letter and the intent of the original decision — the
rejection was of *brokers bundling* and *the hub parsing mention syntax*, neither of which
this does. But it is a change to the storage model that decision implied, so it needs an
explicit yes rather than my assumption. **If the answer is no, the design still works;**
mentions revert to N message rows and we lose the deduplicated conversation view.

### Q2 — Are direct conversations global or project-scoped? — **RESOLVED 2026-08-24**

> **Resolution (ptone@google.com):** keep direct conversations **global**, with explicitly
> specified failure modes wherever ambiguity cannot be resolved. `Conversation.ProjectID`
> stays nullable. Full resolution rules, the exhaustive outcome table, the
> `@<project>/<agent>` disambiguation form, the information-disclosure rule, and the
> exit-code behaviour are specified in **§2.4.1**. Verified by AC-21–AC-25.
>
> The governing principle: a DM's missing project is now an **explicit** condition with named
> outcomes, not a derived `""` that silently disables features.

Today native DMs are explicitly global and not project-scoped, which forces
`resolveProjectFromDMKey` to look up an agent to find a project, and returns `""` for
user↔user DMs — silently killing `@agent` mention resolution there (`messaging-conversation-model-findings.md` §9).

Keeping DMs global preserves current behaviour and the "one DM per pair" mental model.
Scoping them per-project fixes the mention hole and makes attachment storage
non-arbitrary — DMs currently inherit a project from whatever space was last viewed — but
changes user-visible behaviour and means the same two people have N DM threads.

I lean global-with-an-explicit-project-field-on-the-message, which fixes the mention hole
without splitting conversations. Wanted your call before I commit to it.

### Q3 — Should surface-level conversations be auto-created? — **RESOLVED 2026-08-24**

> **Resolution (ptone@google.com):** **eager** for surface containers, created at link time
> and immediately present in `scion conversations list`.
>
> Scoped in follow-up: eager applies to containers brought into existence by an explicit act.
> It does **not** imply materialising a DM per principal pair. The governing invariant —
> *no conversation row is ever created by enumeration* — the per-kind creation table, the
> resolve-or-create `@` grammar that keeps un-materialised DMs addressable, and the
> roster-vs-conversation-list separation are specified in **§2.7.1**. Verified by
> AC-26–AC-29.

When a Discord channel is linked with no threads in use, is a conversation created eagerly
at link time, or lazily on first message? Eager is more predictable and makes the channel
immediately addressable as `conv:`; lazy avoids rows for links that are never used. Low
stakes and reversible; noting it so it gets decided rather than defaulted.

---

## 6. Implementation Phases

Commit-sized, ordered, each independently reviewable.

| # | Phase | Scope |
|---|---|---|
| 1 | Schema | `conversations`, `conversation_participants`, `message_addressees` ent schemas + migrations, both dialects. No behaviour. |
| 2 | Store layer | `ConversationStore` interface + ent adapter. CRUD, upsert-on-`(surface, external_ref)`, participant management. Unit tests. |
| 3 | Resolution | `ResolveConversation` service: the `conv:`/`@`/`#` grammar, upsert semantics, `DriftState` transitions. Pure logic, heavily tested. |
| 4 | Backfill | Migration job implementing §4.1, including the two hazards. Idempotent, resumable, dry-run mode. |
| 5 | Dual-write | Send paths resolve-or-create and stamp `conversation_id`. Reads unchanged. Divergence logging. |
| 6 | Envelope | New `Message` type, `Addressee`, the split taxonomy, addressee resolution (§2.4). Old envelope still accepted and mapped. |
| 7 | Validation choke point | Single `Validate()` invoked on **every** inbound path — the CLI, the Hub HTTP handlers **including native chat**, and broker-inbound. The list is illustrative, **not exhaustive**; see AC-8 and AC-8c. Fixes the Teams violation. |
| 8 | Read switch | Reads move to `conversation_id`. Old fields derived. Lands behind a **default-off runtime flag, flippable without redeploy** (D3). ~~Gate: soak with divergence logging before proceeding.~~ **Superseded:** there is no production soak before the beta exercise, so the gate is the exercise itself, with divergence counters — including a **fallback counter** distinguishing "no disagreement" from "the new path never ran" — readable live on `GET /api/v1/admin/messaging/divergence`. |
| 9 | Delivery formatter | New agent-facing JSON (Appendix B). `status` and `visibility` delivered. Metadata allowlist removed — the fields it was smuggling are now first-class. |
| 10 | CLI split | **`scion message <conversation> <text>` — the conversation becomes a required positional argument, parsed by `ParseReference` and resolved by `Resolve` (§2.5).** Plus `scion broadcast`, `scion keys`; `scion message` reduced to six flags; deprecation mapping. **A deprecation warning may not name a replacement syntax the command cannot yet parse** — see AC-15a. |
| 11 | Broker edge | Per-plugin `ResolveConversation` at inbound; spoke selection by `conversation.surface`. One commit per plugin. |
| 12 | Docs | Skill (Appendix B), `docs-site` messaging page, GLOSSARY entries for Conversation / Surface / Addressee / Participant. Fixes the three doc-drift items in `messaging-conversation-model-findings.md` §10. **Documentation describes the build as it ships, not the design's end state** — anything behind a default-off flag is documented as off, and any syntax not yet parseable is not presented as available. See AC-15a; the same rule that governs a deprecation warning governs a docs page. |
| 13 | Removal | Drop `channel`, `thread_id`, `recipient*`, old type enum. **Not reversible.** **Preconditions:** the beta exercise has passed, and every replacement named in a deprecation warning has shipped and been exercised (AC-15a). Removing a field whose replacement was never reachable strands the callers the warning redirected. |

---

## 7. Acceptance Criteria

### Correctness

- **AC-1** A message cannot be sent without a resolvable conversation. Every rejection names
  the unresolved reference and lists candidates. No code path falls back to broadcast on a
  resolution failure.
- **AC-2** Sending to a `direct` conversation with no `--to` delivers to the other
  participant, and the API response names that participant. (The "no recipient" case works.)
- **AC-3** Sending to a `group` conversation with no `--to` and no default agent persists
  the message, wakes no agent, and returns a response distinguishable from a dispatch.
- **AC-4** Every message reaching an agent, on every surface, is attached to a conversation,
  updates that conversation's activity, and updates unread state. The current "persisted but
  attached to nothing" outcome is unreachable — verified by a test that asserts no message
  row can exist with a null conversation.
- **AC-5** A reply to a Discord thread returns to *that thread*. Regression test for the
  project-wide broadcast bug: with two linked channels, an untagged reply reaches exactly
  one.
- **AC-6** Every registered surface is addressable by its own name. Specific regression:
  `gchat` resolves and routes, closing the `Name`/`ChannelID` mismatch.
- **AC-7** A send to an `orphaned` conversation fails with a specific error and does not
  reach the parent container.
- **AC-8** `Validate()` is invoked on **every** inbound path, not a fixed count of them.
  Enumerated at time of writing: the CLI, each Hub HTTP handler that accepts a message
  (**including native chat**), and broker-inbound. Specific regression: the Teams
  `channel:"" + thread_id:set` combination is rejected at the boundary.
  **Verification is by mutation, not by inspection:** make the choke point fail
  unconditionally and confirm that every inbound path's tests fail. A path whose tests
  still pass is a bypass.
  > Reworded 2026-08-27. The original said "all three inbound paths", which S3 built to
  > literally and satisfied while leaving native chat unvalidated. The count was never the
  > requirement, and naming one invited the reading that the list was closed. §2.10 always
  > said "every". Do not reintroduce a number here.
- **AC-8c** Server-generated emitters that deliver an envelope to an agent — mention
  fan-out, notifications, scheduler messages — either pass through the same choke point or
  are listed as explicit exemptions with a stated reason. An unvalidated emitter that is
  merely undocumented is a defect; the current system's condition is what that accumulates
  into.
- **AC-8a** `reply_to` never affects routing. A message whose `reply_to` names a message in a
  *different* conversation is rejected. Constructing an envelope where `conversation` is
  absent but `reply_to` is present is rejected at the API boundary — resolution happens in
  the CLI, and the wire format always carries the resolved conversation.
- **AC-8b** `scion message --reply-to <id> "text"` with no positional conversation resolves
  and delivers to the containing conversation. Supplying both a positional conversation and a
  `--reply-to` that belongs elsewhere is a CLI error naming both.

### Agent surface (G3)

- **AC-9** An agent can determine "is this addressed to me" from a single structured field
  without parsing prose or inspecting metadata. A body mention produces **one** message row
  with an additional addressee row (`via = 'body-mention'`), not a second message; the
  mentioned agent and the primary addressee receive the same `message.id` and the same `to`
  list. (Q1, resolved.)
- **AC-10** `status` on lifecycle events is a structured field. No agent needs to regex the
  body to learn an agent completed.
- **AC-11** Replying to any inbound message requires echoing one field from the envelope. No
  agent constructs a thread ID.
- **AC-12** The skill's inbound-type section fits on one screen and enumerates a closed set
  (Appendix B).

### Migration

- **AC-13** Backfill is idempotent and resumable; a dry run reports counts per §4.1 rule.
- **AC-14** Through Phase 2, old and new routing agree on 100% of live traffic, or every
  divergence is logged with both decisions.
- **AC-15** Every deprecated flag emits a warning naming its replacement and either succeeds
  identically or fails — never silently changes behaviour.
- **AC-15a** A deprecation warning may only name a replacement that **works in the same
  build**. Verification: for every replacement named in a warning string, a test executes that
  replacement and asserts it reaches the intended destination.
  > Added 2026-08-27 after S4 round 1. `--channel` and `--thread-id` were made to warn "use
  > conversation references instead: `conv:<id>`, `@<agent>`, `#<thread>`" in a build where
  > `scion message` could not parse any of the three. Following the advice did not error:
  > `@builder` became the user `user:@builder` and the message went nowhere, reporting
  > success. That is findings §1.2a — the defect this design exists to remove — newly caused
  > by the migration guidance. A warning that names an unusable replacement is worse than no
  > warning, because it converts a working invocation into a silently misrouted one.
  >
  > **Amended 2026-08-27 after S5 round 1. "Every" means every — enumerate the warnings, do
  > not verify the instance that prompted the criterion.** I accepted S4 having checked only
  > the conversation-reference replacements, because those were what F-1 was about. Three
  > other warnings on the same accepted branch named replacements that do not exist:
  > `--cc → --to` (no such flag), and `--in`/`--at` → `scion schedule message` (no such
  > subcommand). **Verification is mechanical, not a reading:** a permanent test resolves
  > every replacement named in a warning string against the real command tree, and
  > `cobra.Command.Find` alone is insufficient — it returns the deepest match and leaves the
  > remainder as args, so `schedule message` resolves happily to `schedule`. The test must
  > assert the resolved command consumed the intended path.
  >
  > **Warning strings are documentation the binary emits at runtime.** Whatever check covers
  > the docs must cover them too, or the one surface a user cannot avoid reading is the one
  > surface nothing verifies.

  > **Amended 2026-08-27 after S5 round 2. The verifier must assert it verified something.**
  > The round-2 test resolved every single-quoted `'scion …'` reference correctly and was
  > load-bearing against the exact I-1 defect — and still passed with the warning emitter
  > replaced by an empty body, and still passed the whole `cmd` suite with a *new* deprecated
  > flag naming `scion agent poke`, a command that does not exist, because it was back-tick
  > quoted. Extraction that keys on a delimiter fails open. **Two requirements:** extraction
  > is delimiter-agnostic, and the test asserts a floor on the number of replacements it
  > found (>= 7 today, of ten warnings). The same floor requirement applies to any check that
  > iterates over discovered input — the docs parse-check passed green with all four of its
  > source files renamed away. A check whose input can silently become empty is not a check.

### Non-regression

- **AC-16** Fast-fail delivery is unchanged: 409 on a stopped agent, no queuing.
- **AC-17** Observer spokes still see agent↔agent traffic without re-dispatch.
- **AC-18** Visibility filtering still works across all three levels, now end to end.
- **AC-19** Project-scoped isolation holds: no conversation query crosses a project boundary
  except for global direct conversations, which carry no project data.
- **AC-20** The five `messaging-conversation-model-findings.md` §12 bugs that are out of scope are verified still-present
  or still-fixed as applicable, and none is newly broken.

### Direct conversations and ambiguity (Q2, §2.4.1)

- **AC-21** Each row of the §2.4.1 outcome table has a test. Specifically: two shared
  projects with a slug present in only one **resolves**; the same slug present in both
  **fails as `ambiguous`** and the error text contains both `@<project>/<agent>` forms.
- **AC-22** A user↔user DM resolves `@agent` mentions correctly. This is the direct
  regression for `resolveProjectFromDMKey` returning `""` — the current silent no-op must be
  unreachable, asserted by a test that fails if resolution ever returns empty without an
  accompanying `unresolved[]` entry.
- **AC-23** No mention ever resolves to a "first match" under ambiguity. Property test: for
  any candidate set of size > 1, the outcome is a failure, never a delivery.
- **AC-24** An unresolved mention in a DM still delivers to the peer, returns a populated
  `unresolved[]` with the correct `reason`, and exits non-zero with a code distinct from
  send-failure. A test asserts the exit codes differ.
- **AC-25** `not-found` is indistinguishable between "no such agent" and "agent exists in an
  unshared project" — asserted by comparing both error strings for equality. Bare `#<thread>`
  inside a DM fails naming the `--project` fix. `--to` naming a third party in a DM fails as
  `not-a-participant`.

### Conversation creation (Q3, §2.7.1)

- **AC-26** Linking a surface channel creates exactly one conversation, immediately listable
  and addressable as `conv:<id>` before any message exists. Linking does not enumerate
  existing platform threads.
- **AC-27** **No-enumeration invariant.** Spawning N agents into a project with M users
  creates zero conversation rows. Asserted directly: row count before and after is unchanged.
  This is the regression guard for the O(N²) failure mode.
- **AC-28** `scion message @<agent>` to a never-used pair succeeds, creating the DM on send.
  Two concurrent first-sends to the same pair produce exactly one conversation — enforced by
  the unique index on the normalised participant pair, and tested under concurrency.
- **AC-29** `scion conversations list` and the roster are separate queries. A project member
  with no conversations lists none while remaining fully addressable via `@`. Deleting an
  agent does not orphan or delete a conversation that has messages.

### Project isolation (§2.6.1)

- **AC-30** **`conv:<id>` is authorisation-checked, not merely resolved.** Sending to a valid
  conversation ID belonging to another project is rejected on the strength of the sender's
  project. Tested with a real ID from project B used by a sender in project A — the failure
  must not depend on the ID being unknown.
- **AC-31** The grammar cannot express a cross-project address. `#<space>/<thread>` is not
  accepted by the parser. `@<agent>` resolves only within the current project.
- **AC-32** A conversation that exists in a project the sender belongs to but is not currently
  scoped into reports a **boundary violation**; one in a project the sender cannot see reports
  **not-found**. Asserted by string comparison in both directions.
- **AC-33** No single message has agent addressees in more than one project. Property test over
  mention resolution in global DMs: a resolved set spanning projects is a failure, never a
  partial delivery.
- **AC-34** `scion --project <slug> message #thread` succeeds — context switching is the
  supported path and remains available.

---

## Appendix A — Proposed `scion message` usage

```
Post a message to a conversation.

Usage:
  scion message <conversation> <text> [flags]
  scion message --reply-to <msg-id> <text> [flags]

A conversation is the place a message goes: a native thread, a DM, a Discord
thread, a Slack thread. Every message goes to exactly one. Inbound messages
tell you which conversation they came from — reply by naming the same one.

Give exactly one destination: a conversation, or a message to reply to. A
message belongs to exactly one conversation, so --reply-to identifies the
destination on its own.

CONVERSATION REFERENCES
  conv:<id>              A conversation by ID. This is what inbound messages
                         carry; echoing it back is the normal way to reply.
  @<agent>               Your direct conversation with that agent.
  @<email>               Your direct conversation with that user.
  #<thread>              A thread in the current project's space.

  If a reference does not resolve, the send fails and lists candidates.
  Nothing is ever sent to a fallback destination.

  References never cross a project. To work in another project, select it —
  this is a context switch, not an address:

    scion --project other-project message #general "..."

  A conv:<id> from another project is rejected, not silently ignored.

ADDRESSING WITHIN A CONVERSATION
  Use --to when you need specific participants to act. Omit it and Scion
  resolves the addressee from the conversation itself:

    direct conversation      -> the other participant
    has a default agent      -> that agent
    otherwise                -> posted for everyone present; no agent is woken

  The third case is normal. It is how you say something in a room without
  handing anyone a task.

IN A DIRECT CONVERSATION
  A DM is global — one per pair, belonging to no project. So a bare @agent in
  the body resolves against the projects you and the other participant both
  belong to. If that is ambiguous, the send reports it and exits non-zero
  rather than picking one; write @<project>/<agent> to be exact. A bare
  #<thread> has no meaning here, since a DM has no project — select one
  with --project. A single message cannot address agents in two projects.

FLAGS
  --to <principal>       Address a participant directly (repeatable).
                         Must already be a participant.
  --reply-to <msg-id>    Reply to a specific message. Renders as a quoted reply
                         on surfaces that support it. May be used instead of
                         naming a conversation. If you name both, the message
                         must belong to that conversation.
  --attach <path>        Attach a file (repeatable). Paths under /workspace
                         or /scion-volumes.
  --visibility <level>   normal (default) | verbose | full
  --interrupt            Interrupt the addressee's harness. Use sparingly.
  --wake                 Resume a suspended addressee before delivering.

EXAMPLES
  # Reply where you were spoken to — the common case.
  scion message conv:7f3a91c2 "Done. Two tests were failing; both fixed."

  # Ask one participant in a busy thread to act.
  scion message conv:7f3a91c2 --to @reviewer "Can you take the auth diff?"

  # Direct message an agent. No thread to remember.
  scion message @builder "Rebase onto main when you get a chance."

  # Say something in a room without tasking anyone.
  scion message #general "Heads up: staging is down for ~10 minutes."

  # Answer one specific message. The conversation is implied by the message.
  scion message --reply-to msg:4c81de07 "Yes — that path is already covered."

SEE ALSO
  scion broadcast              Send to all agents. Not a conversation.
  scion schedule create        Send later.
  scion keys                   Send literal keystrokes to a harness.
  scion notifications          Subscribe to agent lifecycle events.
```

### What changed and why

| Was | Now | Reason |
|---|---|---|
| `--channel` + `--thread-id`, both optional | the conversation, required and positional | Cannot be forgotten. Omitting it is a parse error, not a silent misroute. |
| `--cc` | `--to` | CC implied a copy. These are addressees. |
| `--broadcast` / `--all` | `scion broadcast` | Not conversations. Removes ~12 exclusion rules. |
| `--in` / `--at` | `scion schedule create --in/--at` | Removes ~8 rules. The dropped-envelope fix needs conversation addressing on scheduled events — DEF-6, not yet built. |
| `--raw` | `scion keys` | Keystroke injection, not messaging. Also honest that it is local-only. |
| `--notify` | `scion notifications subscribe` | Subscription management. |
| `--plain` | (removed) | A rendering hint that leaked into the envelope. |
| 13 flags, 34 exclusion rules | 6 flags, 3 rules | — |

---

## Appendix B — Proposed skill section: inbound messages

> Replaces the "Message types" section of
> `resources/platform_skills/scion-messaging/SKILL.md`.

````markdown
## Messages you receive

Messages arrive wrapped in `---BEGIN SCION MESSAGE---` / `---END SCION MESSAGE---`.

Two questions answer everything: **is it for me**, and **what is wanted**.

### Is it for me?

Look at `to`. If you are listed, the message is addressed to you and you are
expected to act. If you are not listed, you are seeing it because you are in
the conversation — read it, do not act on it.

That is the whole rule. There is no message type you have to recognise to
know whether you are being asked to do something.

### What is wanted?

For `kind: text`, read `intent`:

| intent    | Means                        | You should                          |
|-----------|------------------------------|-------------------------------------|
| `request` | Do something                 | Do it, then reply in the same conversation |
| `question`| Answer something             | Answer. Only if you are in `to`.    |
| `inform`  | For your awareness           | Nothing. Do not reply out of politeness. |

For `kind: event`, read `event.type`. Events are generated by Scion, not by a
person or an agent. They never require a reply.

| event.type            | Means                                  |
|-----------------------|----------------------------------------|
| `agent.state-changed` | An agent you subscribe to changed state. `event.status` holds the value. |
| `agent.input-needed`  | An agent is waiting for input. You are a subscriber, not the addressee — only answer if you are also in `to`. |
| `delivery.failed`     | A message you sent could not be delivered. `event.reason` says why. |
| `schedule.fired`      | A scheduled message you created has fired. |
| `port.exposed`        | A port in your container was exposed. `event.url` holds the address. |

### Replying

The envelope carries `conversation.id`. Reply by naming it:

```bash
scion message conv:7f3a91c2 "..."
```

You never construct a thread or channel ID. If you echo back the conversation
you were addressed in, your reply lands where the sender is looking.

To answer one specific message in a busy conversation, name the message
instead — the conversation is implied, and on surfaces that support quoted
replies it will render as a reply to that message:

```bash
scion message --reply-to msg:4c81de07 "..."
```

To reply somewhere else entirely, name that other conversation — but be
deliberate, because it will not be where the sender is watching.

### Example — a request

```json
{
  "timestamp": "2026-08-23T21:06:22Z",
  "conversation": {
    "id": "conv:7f3a91c2",
    "kind": "group",
    "surface": "discord",
    "name": "#scion-dev / messaging-refactor",
    "participants": ["user:ptone@google.com", "agent:reviewer", "agent:ca-msg-arch"]
  },
  "from": "user:ptone@google.com",
  "to": ["agent:ca-msg-arch"],
  "kind": "text",
  "intent": "request",
  "msg": "Draft the design doc for the messaging refactor.",
  "visibility": "normal"
}
```

You are in `to`, `intent` is `request`. Do the work, reply to `conv:7f3a91c2`.

### Example — an event

```json
{
  "timestamp": "2026-08-23T21:31:04Z",
  "conversation": {
    "id": "conv:1a2b8e40",
    "kind": "direct",
    "surface": "native",
    "name": "you and agent:builder"
  },
  "from": "system:notifications",
  "to": ["agent:ca-msg-arch"],
  "kind": "event",
  "event": { "type": "agent.state-changed", "subject": "agent:builder", "status": "COMPLETED" },
  "msg": "Agent builder completed its task.",
  "visibility": "normal"
}
```

`event.status` is `COMPLETED`. Read the field; do not parse the sentence.

### Example — not for you

```json
{
  "conversation": { "id": "conv:7f3a91c2", "kind": "group", "surface": "discord", "name": "#scion-dev / messaging-refactor" },
  "from": "user:ptone@google.com",
  "to": ["agent:reviewer"],
  "kind": "text",
  "intent": "request",
  "msg": "Can you review the auth diff?",
  "visibility": "normal"
}
```

You are not in `to`. Someone else was asked. Do not start reviewing the auth
diff, and do not reply.
````

### Contract lines this replaces

| Old prose the skill needed | Now |
|---|---|
| "`mention` means FYI/CC — no action unless the text directs it" | Not in `to` → not for you. No type to learn. |
| "`group-set` — act like `instruction`" | Type gone. One message, several addressees. |
| A paragraph on why only the parent should answer `input-needed` | Addressed question → you are in `to`. Broadcast → you are a subscriber. Visible in the envelope. |
| "check `metadata.system_category`" | `event.type` is a first-class field. |
| Nothing — `status` was not delivered | `event.status` is delivered. |
| "2000 character limit" (wrong; it is 16000 for agents) | One documented limit per addressee kind, generated from the constants. |
