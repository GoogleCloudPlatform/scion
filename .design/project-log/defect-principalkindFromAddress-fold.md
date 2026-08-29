# Defect: PrincipalKindFromAddress case-fold masks kind-prefix validation

**Filed by:** ca-msg-em9  
**Date:** 2026-08-28  
**Updated:** 2026-08-28 (v4 — false premise corrected, SSE impact added, :1653 sharpened)  
**Origin:** em6 finding during DMConversationKey tightening audit  
**Status:** Audit complete — live authorization bypass confirmed — no code changes made  

---

## Summary

`PrincipalKindFromAddress` (pkg/messages/dm_key.go:75-85) applies
`strings.ToLower` to the kind prefix before validating against
`validDMKinds`. This means non-canonical prefixes like `"User:"` or
`"Agent:"` are silently accepted and mapped to their canonical forms.

At call site 2 (handlers_broker_inbound.go:276), the fold is a necessary
link in a **live authorization bypass chain** on upstream/main. A broker
plugin sending `"User:alice"` as Sender bypasses the case-sensitive
`HasPrefix("user:")` gate that performs ActionAttach authorization and
SenderID resolution. The fold then repairs the prefix downstream,
allowing DM conversation resolution to proceed with an
attacker-controlled SenderID. The bypass is not latent — it is
exploitable now.

**v1 error corrected:** The original audit incorrectly concluded the
bypass was "inert in practice, gated by a SenderID nil-guard." That
guard is at messagebroker.go:835 (call site 4) — a different function on
a different call path. It does not run at call site 2. Furthermore,
SenderID is decoded from the request JSON body
(`json:"sender_id,omitempty"`) and is attacker-controlled; the nil-guard
would not have helped even if it ran on this path.

## Function under audit

```go
func PrincipalKindFromAddress(address string) (string, bool) {
    idx := strings.IndexByte(address, ':')
    if idx < 0 { return "", false }
    kind := strings.ToLower(address[:idx])   // ← the fold
    if validDMKinds[kind] { return kind, true }
    return "", false
}
```

`validDMKinds` contains only `"user"` and `"agent"`.

---

## Call-site provenance audit (upstream/main)

### Site 1 — handlers_agent_messaging.go:1578

```go
senderKind, sOK := messages.PrincipalKindFromAddress(mentionMsg.Sender)
```

**Provenance:** `mentionMsg.Sender` ← `originalMsg.Sender` ←
`structuredMsg.Sender` ← B5 auth override (lines 540-559).

B5 override constructs Sender from authenticated context:
- `"user:" + user.DisplayName()` or `"user:" + user.Email()`
- `"agent:" + agentIdent.ID()`
- fallback: `"user:unknown"`

All prefixes are string literals in lowercase.

**Classification: INTERNAL.** Server-constructed, always canonical. The
fold is identity-preserving. No non-canonical prefix can reach this site.

### Site 2 — handlers_broker_inbound.go:276

```go
senderKind, sOK := messages.PrincipalKindFromAddress(req.Message.Sender)
```

**Provenance:** `req.Message.Sender` comes from the JSON body of a
broker plugin HTTP POST. Transport is HMAC-authenticated (line 58), but
the Sender field content is controlled by the broker plugin.

**Upstream gate (lines 126-146):** A case-sensitive
`strings.HasPrefix(req.Message.Sender, "user:")` check controls:
1. User lookup via `store.GetUserByEmail`
2. `SenderID` caching on the message
3. `ActionAttach` permission check for the resolved user

If Sender is `"User:alice"` (capital U):
- `HasPrefix("user:")` → **false** — skips user lookup, SenderID cache,
  and ActionAttach permission check
- `PrincipalKindFromAddress` at line 276 → folds `"User"` to `"user"`,
  returns `("user", true)` — proceeds into DM conversation resolution

This creates a path where DM conversation resolution proceeds with
`senderKind = "user"` but without the ActionAttach authorization that
normally gates user-originated messages.

**Classification: EXTERNAL.** Broker plugin controls Sender content. The
fold repairs a non-canonical prefix that the upstream case-sensitive gate
correctly rejected, enabling an authorization bypass.

**This bypass is live, not latent.** The full chain:

**Step 1 — Gate bypass (handlers_broker_inbound.go:126-146).** The
case-sensitive `HasPrefix("user:")` gate does three things: (a) resolves
the sender via `store.GetUserByEmail`, (b) runs the ActionAttach
permission check, and (c) overwrites `req.Message.SenderID` with the
resolved user's ID. Sender `"User:alice"` (capital U) causes HasPrefix
to return false. All three operations are skipped.

**Step 2 — Attacker-controlled SenderID survives.** `SenderID` is a
JSON-decoded field (`json:"sender_id,omitempty"` in
pkg/messages/types.go:131). Because the gate at :126 was skipped, the
body-supplied `sender_id` is not overwritten with a server-resolved
value. The attacker controls it.

**Step 3 — DM ownership check passes (#1322, line 165-170).** The
ownership check compares `dmUserID` (parsed from the attacker-supplied
`thread_id`) against `senderID` (the attacker-supplied `sender_id`).
Both values are attacker-controlled. They trivially match.

**Step 4 — SenderID used under false invariant (line 241).** The
assignment `senderUserID := req.Message.SenderID` carries a comment:
*"The sender user ID was resolved and cached in req.Message.SenderID
during the upstream permission check for 'user:' senders."* This comment
asserts exactly the invariant that the case-sensitive gate fails to
guarantee. The attacker's body-supplied value is used as if it were
server-resolved.

**Step 5 — Fold enables DM resolution (line 276).** At this point,
`PrincipalKindFromAddress("User:alice")` folds `"User"` to `"user"` and
returns `("user", true)`. Without the fold, it would return `("", false)`
and the forged message would never reach `ResolveOrCreateDMConversation`.
The fold is not the root cause, but it is a necessary link in the chain.
Removing it shrinks blast radius; it does not close the bypass.

**Step 6 — DM resolution proceeds (line 279).** The call to
`ResolveOrCreateDMConversation` runs with `senderKind = "user"` and an
attacker-controlled `senderUserID`. Note that this is a direct call to
`messaging.ResolveOrCreateDMConversation`, NOT through
`resolveDMConversation` in messagebroker.go. The nil-guard at
messagebroker.go:835 (call site 4) is on a completely different call path
and does not run here.

**Root cause:** The case-sensitive `HasPrefix("user:")` gate at line 126.
The fold in `PrincipalKindFromAddress` is a contributing factor that
enables the worst outcome (DM resolution with forged identity).

**Aggravating factor — live-push via EventPublisher.** The forged message
is not merely persisted. After `CreateMessage` succeeds,
`s.events.PublishUserMessage` at line 291 publishes it to the
EventPublisher bus (`p.events`), which carries hub Event structs to
web/SSE consumers and other subscribers. Because
`storeMsg.AgentID = agent.ID` (line 258), the forged message is sinked
to `agent.<agentID>.message` — live-pushed to every subscriber on the
target agent's stream, carrying the victim's (attacker-chosen) SenderID.
The forgery is visible in real-time to all connected clients watching
that agent's conversation feed.

**Existing broker plugins:** The Slack broker plugin constructs Sender as
`"agent:" + slug` using lowercase string literals with case-sensitive
HasPrefix checks. No known first-party plugin sends non-canonical
prefixes. Third-party plugins are unconstrained.

### Site 3 — messagebroker.go:465

```go
if recipientKind, rOK := messages.PrincipalKindFromAddress(msg.Recipient); rOK {
```

**Provenance:** `msg.Recipient` arrives via eventbus subscription
(`subscribeProjectUserMessages`). Published by hub handlers:
- `handleAgentOutboundMessage` sets Recipient = `"user:" + name`
  (server-constructed, lines 150/158/164)
- `handleBrokerInbound` does not publish through this path

All publishers use lowercase string literal prefixes.

**Classification: INTERNAL.** Server-constructed, always canonical. The
fold is identity-preserving.

### Site 4 — messagebroker.go:839

```go
senderKind, ok := messages.PrincipalKindFromAddress(msg.Sender)
```

**Provenance:** Called from `resolveDMConversation`, which is invoked by
`deliverToUser` and `deliverToAgent`. Messages arrive via eventbus,
published by hub handlers where Sender is always B5-forced (`"user:" +
name/email`) or agent-constructed (`"agent:" + slug`). All lowercase
string literal prefixes.

**Classification: INTERNAL.** Server-constructed, always canonical. The
fold is identity-preserving.

---

## Answers to architect's questions

### Q1: Can a non-canonical kind prefix originate outside the trust boundary?

**Yes, at exactly one call site.** Site 2 (handlers_broker_inbound.go:276)
receives Sender from a broker plugin's JSON body. The broker plugin is
HMAC-authenticated at transport level but the Sender field content is not
canonicalized or validated before reaching `PrincipalKindFromAddress`.

Sites 1, 3, and 4 are all server-constructed with lowercase string
literal prefixes. No non-canonical prefix can reach them.

### Q2: Is the fold-induced aliasing semantically inert, or does it matter?

**The aliasing on the DM key is semantically inert in isolation.** There
is no distinct `"User"` or `"Agent"` principal kind in the system.
`"User:x"` and `"user:x"` name the same principal. The fold maps to the
only valid canonical form, and `DMConversationKey` itself also applies
`ToLower` (line 41-42), so the key derivation is doubly normalized.

**But the fold matters because it is a necessary link in a live
authorization bypass at site 2.** Without the fold,
`PrincipalKindFromAddress("User:alice")` would return `("", false)` and
the B15 dual-write block would not reach `ResolveOrCreateDMConversation`.
The fold repairs the prefix that the case-sensitive gate correctly
rejected, enabling the forged message to complete DM resolution with an
attacker-controlled SenderID.

The full chain at site 2:

- `"user:alice"` → HasPrefix passes → authorized, SenderID resolved →
  fold → DM resolution with server-verified identity
- `"User:alice"` → HasPrefix fails → **not authorized, SenderID
  attacker-supplied** → fold repairs to `"user"` → DM resolution with
  attacker-controlled identity

The aliasing on the key is inert. The fold's role in enabling the bypass
is not.

---

## Reasoning error in v1 (corrected)

The v1 audit carried a safety argument across call sites: the nil-guard
at messagebroker.go:835 (call site 4, `resolveDMConversation`) was cited
as protecting call site 2 (handlers_broker_inbound.go:276). These are
different functions on different call paths. Site 2 calls
`messaging.ResolveOrCreateDMConversation` directly at line 279 — it does
not go through the `resolveDMConversation` wrapper in messagebroker.go.

Even if the nil-guard ran on this path, it checks
`msg.SenderID == ""`. SenderID is decoded from the request JSON body
(`json:"sender_id,omitempty"` in pkg/messages/types.go:131). An attacker
exercising this bypass controls the body and would not leave SenderID
empty.

The lesson: when carrying a safety argument across call sites, the guard
must be on the path being cleared. Write the guard's file and line next
to the site it protects — the mismatch becomes immediately visible.

---

## Recommendation (no code changes in this audit)

Routing is with the user. Architect has recommended a standalone hotfix
ahead of the refactor.

1. **Root cause — fix the gate at site 2:** Make the `HasPrefix("user:")`
   check at handlers_broker_inbound.go:126 case-insensitive, or
   canonicalize the kind prefix before the gate runs. This closes the
   bypass regardless of the fold.
2. **Contributing factor — remove the fold from PrincipalKindFromAddress:**
   Make it case-sensitive to match the upstream gates. This does not close
   the bypass on its own (the gate is still the root cause) but removes a
   necessary link in the worst version of the chain, shrinking blast
   radius.
3. **Do not rely on the SenderID nil-guard at site 4** as defense for
   site 2. It is on a different call path and tests a condition the
   attacker controls.

---

## DEF-32 class audit: all 24 case-sensitive prefix tests in pkg/hub

DEF-32 is a class, not an instance. There are 24 case-sensitive prefix
tests on principal addresses in pkg/hub non-test code. This section
triages each into one of three buckets:

- **AUTHZ** — the comparison gates a permission check, identity
  resolution, or a field the security layer later trusts. A miss grants
  something.
- **ROUTING** — the comparison picks a delivery path or display form. A
  miss misroutes or mislabels, but grants nothing.
- **INERT** — operand is provably server-constructed canonical; no
  external path can supply a non-canonical prefix.

### Bucket counts

| Bucket  | Count | Sites |
|---------|-------|-------|
| AUTHZ   | 3     | handlers_broker_inbound.go:126, :127; handlers_chat_v2.go:1653 |
| ROUTING | 5     | events.go:761; handlers_agent_messaging.go:139; handlers_broker_inbound.go:297; handlers_chat_v2.go:1966, :1974 |
| INERT   | 16    | events.go:744; handlers_agent_messaging.go:911, :912, :1105, :1466; messagebroker.go:437, :518, :535, :536, :716, :748, :791; webchannel.go:103, :185, :188, :201 |
| **Total** | **24** | |

### AUTHZ sites (3)

**handlers_broker_inbound.go:126** — `HasPrefix(req.Message.Sender, "user:")`
Gate for ActionAttach permission check, user resolution, SenderID
overwrite. EXTERNAL: Sender from broker plugin JSON body.
*Miss consequence:* ActionAttach check skipped; body-supplied sender_id
survives as if server-resolved. This is the DEF-32 root cause.

**handlers_broker_inbound.go:127** — `TrimPrefix(req.Message.Sender, "user:")`
Inside the :126 if-block. Extracts email from Sender for user lookup.
Dependent on :126 — only reachable when :126 passes, so not independently
exploitable. However, any fix that makes :126 case-insensitive must also
address :127 or canonicalize before both.
*Miss consequence:* Part of the :126 gate; email extraction fails if
prefix casing mismatches.

**handlers_chat_v2.go:1653** — `HasPrefix(msg.Sender, "agent:")`
In `hasAgentReplyAfter` (:1635). Reads stored messages from DB to
determine if an agent has replied in the conversation after a given
timestamp. Called from the edit handler (:1491) and delete handler
(:1591). Gates edit/delete permission on user's own message (immutability
after agent reply).

The function's author explicitly reasoned about failure and chose
fail-closed on the error path:

    if err != nil || result == nil {
        return true // fail-closed: deny edit/delete when we can't verify
    }

But the casing miss is not an error. The query succeeds, returns rows,
and `HasPrefix(msg.Sender, "agent:")` simply does not match `"Agent:bot"`.
The loop completes normally and the function returns `false` — fail-OPEN.
An explicit fail-closed comment sits eight lines above a fail-open path
in the same function. The comment creates confidence that the function
fails closed, when it only covers the error branch.

**Casing exploit — latent fail-open, exploitability unproven.**
handleBrokerInbound persists `req.Message.Sender` verbatim. The :126
gate only inspects for `"user:"` prefix — `"Agent:bot"` (capital A)
passes through :126 unexamined, and the message is persisted with
`Sender = "Agent:bot"`. When a user later calls edit/delete,
`HasPrefix("agent:")` misses, and the function returns false.

However, adding a non-canonical message does not remove a canonical one.
`hasAgentReplyAfter` returns true as soon as it finds ANY message
matching `HasPrefix("agent:")`. Canonical agent replies are server-
constructed (handlers_agent_messaging.go:239 `"agent:" + agent.Slug`,
notifications.go:486 same pattern) — never caller-supplied. A genuine
agent reply through the normal path is canonical and matches. The casing
exploit requires a thread in which ALL agent replies were persisted via
the verbatim-Sender broker inbound path and none through the canonical
server-constructed path. No such thread has been demonstrated.

**DEF-34: Asymmetric Channel invariant (independent of DEF-32, confirmed).**

The function's filter pins `Channel: "web"` (line 1636):

    filter := store.MessageFilter{
        Channel:  "web",
        ThreadID: threadID,
        After:    after,
    }

The hub enforces Channel on one side of the conversation and the guard
filters on it as though both sides were enforced:

- **User → agent (handlers_agent_messaging.go:736):** Channel is
  defaulted to `"web"` when the authenticated user's client type is
  `"web"`:

      if structuredMsg.Channel == "" {
          if user := GetUserIdentityFromContext(ctx); user != nil {
              if au, ok := user.(*AuthenticatedUser); ok && au.ClientType() == "web" {
                  structuredMsg.Channel = "web"
              }
          }
      }

- **Agent → user (handlers_agent_messaging.go:188-202, 207, 247):**
  Channel goes through a conditional inference, not a default. When the
  runtime omits Channel (req.Channel == ""), reply affinity runs:

      if req.Channel == "" && recipientID != "" && s.webChatStore != nil && s.GetMessageBrokerProxy() != nil {
          if lastCh, err := s.webChatStore.GetLastChannel(ctx, recipientID, agent.ProjectID, agent.ID); err != nil {
              // Non-fatal: fall through to fan-out-to-all behavior.
          } else if lastCh != "" {
              req.Channel = lastCh
          }
      }

  `GetLastChannel` is keyed `(userID, projectID, agentID)` — it is
  **thread-agnostic** (webchannel_store.go:62). But the guard's
  MessageFilter is keyed `(channel, threadID)` — **per-thread**. Producer
  key and consumer key disagree.

  If no affinity row exists, lastCh = "" → req.Channel stays "" →
  Channel validation at :207 is skipped (gates on `req.Channel != ""`)
  → reply persisted with Channel = "". Empty Channel is documented
  intended behaviour: "leave channel empty so the message fans out to
  all spokes" (:193 comment). An intended sentinel value that the guard
  cannot see.

  **Rule 206.** When one site writes a field and another reads it as a
  precondition, the two must agree on the field's key. Producer keyed
  (user, project, agent) and consumer keyed (channel, thread) disagree
  the first time a user is active on two channels, and the disagreement
  presents as a guard that passes.

  **Rule 207.** An intended sentinel is still a blind spot. That a value
  is supported is an argument for handling it, not evidence that it is
  handled.

**Affinity writers** (two, both confirmed):

1. webchannel.go:139 — inside `webChannelBus.Publish`, writes `"web"`:

       b.store.RecordChannel(ctx, userID, projectID, agentID, "web", ...)

2. handlers_broker_inbound.go:299 — writes the inbound spoke's channel:

       s.webChatStore.RecordChannel(r.Context(), senderUserID, agent.ProjectID, agent.ID, req.Message.Channel, now)

handlers_chat_v2.go calls neither.

**Three routes to bypass, ranked by strength:**

**(b) Cross-channel affinity — strongest, ordinary multi-channel use.**
No attacker, no misbehaving runtime:

1. User posts in web thread W → affinity = "web"
   (webchannel.go:139, keyed (user, project, agent))
2. Same user says anything to the same agent from Discord →
   affinity = "discord" (handlers_broker_inbound.go:299, same key,
   overwrites)
3. Agent sends untagged reply in W → :195 `GetLastChannel` returns
   "discord" → :200 stamps req.Channel = "discord" → persisted
4. User edits/deletes their message in W → guard filters
   Channel="web", misses the "discord" reply, **permits the edit**

**(a) No affinity row — documented intended behaviour.**
Agent sends first untagged reply before user has spoken on any channel
(or affinity row expired/absent) → GetLastChannel returns "" → lastCh
= "" → req.Channel stays "" → Channel = "" persisted. Empty Channel is
the documented fan-out-to-all-spokes sentinel (:193 comment). Guard
filters Channel="web", misses it.

**(c) Runtime sets non-web explicitly — weakest, needs cooperating
client.**
Agent runtime sets `"channel": "slack"` (or any non-"web" value) in its
response body → passes channel-registration validation at :207 if
registered → persisted with that Channel → invisible to guard.

**Fix ruling.** The guard's question is "has the agent replied in this
thread?" Channel is not part of that question. The correct fix is to
drop `Channel: "web"` from the MessageFilter in `hasAgentReplyAfter`.
Defaulting Channel on the agent path is wrong on its own terms — empty
Channel is load-bearing, it means fan-out-to-all-spokes. Forcing "web"
would break fan-out to repair a guard. Removing the Channel filter
widens the match set, so it fails closed: messages that were previously
invisible become visible, which only causes the guard to return true
(deny edit/delete) in cases it was incorrectly permitting.

**Adjacent note: pagination termination.** `hasAgentReplyAfter`'s loop
terminates on `result.NextCursor == "" || len(result.Items) < pageSize`
(line 1658). A short page that still carries a cursor ends the scan and
returns false (fail-open). Removing the Channel filter changes page
occupancy (more messages per page), so this termination condition should
be reviewed when the filter is removed. Not part of DEF-34, but worth
recording before anyone modifies this loop.

*Miss consequence (casing):* Latent fail-open on agent-reply immutability.
*Miss consequence (DEF-34, channel asymmetry):* Agent-reply immutability
bypassed through ordinary multi-channel use. Confirmed exploitable, no
malicious actor required.

### ROUTING sites (5)

**events.go:761** — `!HasPrefix(msg.ThreadID, "agent:")`
In `PublishUserMessage` (EventPublisher). Tests ThreadID to distinguish
legacy agent-slug keys from UUID topic threads. ThreadID from broker
inbound path is body-supplied. Miss causes message to be published to
the shared-space chat event subject when it should not be. No privilege
granted.
*Miss consequence:* Message appears in wrong event subject.

**handlers_agent_messaging.go:139** — `TrimPrefix(recipient, "user:")`
In recipient resolution for handleAgentMessage. If recipient is
`"User:alice"`, TrimPrefix does not strip, email/name lookup fails,
resolution fails. No privilege granted — denial, not bypass.
*Miss consequence:* Recipient resolution failure; message not deliverable.

**handlers_broker_inbound.go:297** — `HasPrefix(req.Message.Sender, "user:")`
Gates reply-affinity recording (RecordChannel) and thread watermark
updates. Miss means reply-affinity not recorded for non-canonical sender.
Agent's next untagged reply may route to wrong channel. No privilege
granted.
*Miss consequence:* Routing quality degrades; agent reply may go to wrong
channel.

**handlers_chat_v2.go:1966** — `HasPrefix(m.Sender, "agent:") && HasPrefix(m.Recipient, "agent:")`
Inter-agent message filter (first merge loop). Reads from DB. If a non-
canonical agent Sender/Recipient was stored, message is filtered out of
inter-agent list. No privilege granted — information hidden, not exposed.
*Miss consequence:* Inter-agent messages not displayed.

**handlers_chat_v2.go:1974** — same shape as :1966
Second merge loop, same filter. Same analysis, same consequence.

### INERT sites (16)

All operands are provably server-constructed canonical. No external path
can supply a non-canonical prefix to any of these sites.

**events.go:744** — `HasPrefix(msg.Recipient, "user:")`
In PublishUserMessage (EventPublisher). Recipient is always server-
constructed: "agent:" + slug from handleBrokerInbound, or "user:" + name
from handleAgentOutboundMessage. No external path sets Recipient.

**handlers_agent_messaging.go:911** — `HasPrefix(structuredMsg.Sender, "agent:")`
**handlers_agent_messaging.go:912** — `HasPrefix(structuredMsg.Recipient, "agent:")`
Agent-to-agent observer publishing gate. Both operands B5-forced
(Sender auth-derived, Recipient from URL path agent resolution).

**handlers_agent_messaging.go:1105** — `HasPrefix(agentMsg.Sender, "agent:")`
Group message observer publishing gate. Sender B5-forced.

**handlers_agent_messaging.go:1466** — `!HasPrefix(msg.Sender, "agent:") || msg.SenderID == ""`
`publishBroadcastDeliveryFailed`. Called from handleProjectBroadcast
(HTTP handler path). Sender and SenderID both B5-forced from auth
context. SenderID NOT body-settable — B5 override at lines 548-559
overwrites body value. If prefix missed: DELIVERY_FAILED notification
not sent (silent failure, no privilege).

**messagebroker.go:437** — `HasPrefix(msg.Sender, "agent:")`
In deliverToUser, sets AgentID on stored message. msg arrives via
`p.bus` (the StructuredMessage bus). handleBrokerInbound never publishes
to `p.bus` — it publishes only to `p.events` (EventPublisher), which
carries a different type on different subjects. No path from broker
inbound reaches deliverToUser. All `p.bus` publishers B5-force fields.

**messagebroker.go:518** — `!HasPrefix(storeMsg.ThreadID, "agent:")`
In deliverToUser, thread watermark routing. msg from `p.bus`
(StructuredMessage bus); all publishers B5-force fields. No broker
inbound path.

**messagebroker.go:535** — `HasPrefix(storeMsg.Sender, "agent:")`
**messagebroker.go:536** — `TrimPrefix(storeMsg.Sender, "agent:")`
DM notification block in deliverToUser. msg from `p.bus`, B5-forced.
No broker inbound path.

**messagebroker.go:716** — `HasPrefix(msg.Sender, "agent:") && msg.SenderID == ""`
R3b diagnostic warning in fanOutToProject. Invoked at :383 inside a
`p.bus.Subscribe` handler. msg is `*messages.StructuredMessage` from the
StructuredMessage bus. handleBrokerInbound never publishes to `p.bus`
(only to `p.events`). All `p.bus` publishers B5-force fields. SenderID
auth-derived, NOT body-settable. If prefix missed: warning suppressed.
Self-skip at :726 (ID-based) works independently.
**B5/R1 self-skip note:** This is broadcast self-skip territory. The R3b
warning is cosmetic. The actual self-skip logic (`msg.SenderID != "" &&
msg.SenderID == agent.ID`) does not depend on the prefix test and is
safe regardless.

**messagebroker.go:748** — `HasPrefix(msg.Sender, "agent:") && msg.SenderID == ""`
R3b warning in fanOutGlobal. Same analysis as :716. msg from `p.bus`,
B5-forced. SenderID auth-derived, NOT body-settable.

**messagebroker.go:791** — `!HasPrefix(msg.Sender, "agent:") || msg.SenderID == ""`
`publishDeliveryFailed` in MessageBrokerProxy. Called from deliverToAgent
on dispatch failure. msg is `*messages.StructuredMessage` from `p.bus`
agent topic subscriptions. handleBrokerInbound never publishes to
`p.bus` — it publishes only to `p.events` (EventPublisher, different
type, different subjects). All `p.bus` publishers (PublishMessage from
HTTP handlers, fanOutToProject/Global iteration) B5-force fields.
SenderID auth-derived, NOT body-settable. If prefix missed:
DELIVERY_FAILED not sent (silent failure, no privilege).

**webchannel.go:103** — `!HasPrefix(msg.ThreadID, "agent:")`
Thread watermark routing in web channel spoke. The web spoke is
registered as an observer on the FanOutEventBus and receives
`*messages.StructuredMessage` via `p.bus`. handleBrokerInbound never
publishes to `p.bus`. All `p.bus` publishers B5-force fields.

**webchannel.go:185** — `HasPrefix(msg.Sender, "agent:")`
**webchannel.go:188** — `TrimPrefix(msg.Sender, "agent:")`
In identityFromTopic, TopicKindUser case. Sets agentID from agent sender.
msg from `p.bus`, server-constructed on all publishing paths.

**webchannel.go:201** — `HasPrefix(msg.Sender, "user:")`
In identityFromTopic, TopicKindAgent case. Sets userID from user sender.
msg from `p.bus`, server-constructed on all publishing paths.

---

## Why the INERT count is high

16 of 24 sites (67%) are INERT because of a two-bus architecture:

- `MessageBrokerProxy.bus` (`eventbus.EventBus`) carries
  `*messages.StructuredMessage` — the broker-delivery pipeline.
  fanOutToProject, deliverToUser, deliverToAgent, and all 16 INERT sites
  hang off `p.bus` subscribers.

- `MessageBrokerProxy.events` (`EventPublisher`) carries hub Event
  structs (e.g., `UserMessageEvent`) — in-process fan-out to web/SSE
  consumers and other subscribers.

The B5 auth-derivation override (handlers_agent_messaging.go:540-559)
canonicalizes Sender and SenderID at the HTTP handler entry point, and
these canonical values propagate through `p.bus` to all downstream
consumers.

handleBrokerInbound is the only HTTP handler that does NOT B5-force
Sender. It contains zero `p.bus` references and exactly one publish:
`s.events.PublishUserMessage` at line 291. It publishes to `p.events`
(EventPublisher), never to `p.bus` (StructuredMessage bus). The two
buses carry different types on different subjects. No `p.bus` subscriber
receives messages from handleBrokerInbound.

This is why the forged message reaches EventPublisher subscribers (web
clients see the forgery live-pushed to `agent.<agentID>.message`) but
does not reach the 16 INERT sites that consume `*messages.StructuredMessage`
off `p.bus`.

The 3 AUTHZ + 5 ROUTING sites that are not INERT all either:
1. Directly process broker inbound request fields (:126, :127, :297), or
2. Read from the DB where broker inbound may have stored non-canonical
   values (:1653, :1966, :1974), or
3. Receive ThreadID from a path that includes broker inbound (:761), or
4. Process user-supplied recipient strings (:139).

## Fix shape implications

If DEF-32 were a single instance (1 AUTHZ site), patching
handlers_broker_inbound.go:126 case-insensitively would suffice.

With 3 AUTHZ sites (2 at the gate + 1 second-order via DB), plus 5
ROUTING sites that degrade on non-canonical input, the correct fix moves
upstream: **canonicalize Sender (and Recipient if present) once at
broker inbound ingress** before any downstream comparison can be fooled
by casing. This closes all 8 non-INERT sites with one normalization
point, and prevents future case-sensitive comparisons from becoming new
instances of the class.
