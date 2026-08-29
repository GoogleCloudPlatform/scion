# Scion Messaging — Problem Inventory

**Status:** complete (input to `messaging-conversation-model.md`)
**Date:** 2026-08-23
**Author:** architect agent (`ca-msg-arch`)
**Base commit:** `7d171a50`

This document is the grounded defect inventory for the messaging refactor. Every claim
carries a file reference. It is deliberately descriptive, not prescriptive — the target
design lives in `messaging-conversation-model.md`.

---

## 0. The one-sentence diagnosis

Scion has **three overlapping addressing schemes** for "where does this message go",
added at different times and never reconciled. Almost every reported symptom is a
second-order effect of that.

| # | Scheme | Shape | Introduced by |
|---|---|---|---|
| 1 | `recipient` string with a prefix grammar | `agent:x`, `user:x`, `thread:<uuid>`, `group[a,b]`, legacy `set[a,b]` | original agent↔agent messaging |
| 2 | `channel` + `thread_id` envelope fields | two independent optional strings | `.design/chat-channel-routing.md` (2026-05-31, #113) |
| 3 | native chat conversation keys | `dm:agent:<uuid>:user:<uuid>`, topic UUIDs | native chat wave 1/2 |

The **only** cross-field consistency rule in the entire system is
`thread_id requires channel` (`pkg/messages/types.go:194-196`, mirrored at
`cmd/message.go:115-117`). Nothing checks that scheme 1 and scheme 2 agree. They can
contradict each other, and the system routes on whichever the executing code path
happens to read.

---

## 1. Routing defaults

### 1.1 There is no default venue — there are three unrelated fallbacks

| Recipient kind | Behaviour when `channel` is empty |
|---|---|
| `agent:<slug>` | Direct runtime-broker dispatch, no spoke tagged. **Except**: `channel` is auto-set to `"web"` iff the caller is an authenticated *web* user (`pkg/hub/handlers_agent_messaging.go:667-676`). CLI and API callers are deliberately left untagged. |
| `user:<id>` | Reply-affinity lookup (`GetLastChannel`), else **fan out to every registered spoke** (`pkg/eventbus/fanout.go:130-146`) |
| native chat | Inverse direction — the thread key *implies* the recipient (`pkg/hub/handlers_chat_v2.go:911-949`) |

### 1.2 Omitting `--thread-id` silently degrades — the reported bug

Trace for a `user:` recipient with no thread:

1. `deliverToUser` persists the `store.Message` with `Channel`/`ThreadID` as-is
   (`pkg/hub/messagebroker.go:432-517`).
2. `TouchDMActivity` / `TouchTopicActivity` are gated on `ThreadID != ""` (`:471-488`).
3. `NotifyDMReceived` is gated on `ThreadID` having the `dm:` prefix (`:494-505`).

Net effect: the message lands in the flat inbox and the SSE stream, is attached to **no
conversation**, marks **nothing unread**, and notifies **nobody**. No warning is emitted;
the send reports success.

### 1.2a Lived example — the architect lost a week of reports to this defect

Recorded 2026-08-27, during implementation of this design.

Coordinating this refactor, I sent the user section-boundary reports for S1 and S2 without
`--channel` and `--thread-id`. None were delivered. I did not discover it from an error,
from a delivery failure, or from a missing acknowledgement — the user eventually told me
they only see messages carrying both flags.

Every element of §1.2 is present:

- The flags are optional, so omitting them is not a parse error.
- The sends reported success.
- Nothing warned, at send time or after.
- The failure was invisible from the sender's side and stayed invisible across multiple
  exchanges, because the user *was* replying — to their own prompts, not to my reports.
  I read those replies as evidence the channel worked.

That last point is the part worth keeping. The defect does not merely fail silently; it can
produce a conversation that *looks* two-sided while one direction is dropped. An operator
actively watching for trouble will not see it.

This is also the case for making the venue a required positional argument rather than a
validated optional flag (design §2.2). A warning on omission would not have helped here —
warnings go to stderr, and stderr was exactly the channel nobody was reading.

### 1.3 Reply affinity records `channel` but not `thread_id`

`webchat_conversation_context` has a `last_channel` column and no `last_thread_id`
(`pkg/hub/webchannel_store.go:359-366`). An untagged agent reply is therefore routed to
`discord` with no thread; the Discord broker falls through its resolution ladder to
priority 3 and **broadcasts to every linked channel in the project**
(`extras/scion-discord/internal/discord/broker.go:552-594`).

This is the highest-value single schema gap found, and it is not cleanly expressible in
the current two-field model.

---

## 2. `thread_id` is an untyped union

Seven distinct encodings share one column:

| Channel | Encoding | Reference |
|---|---|---|
| `web` | topic UUID, or `dm:<kind>:<uuid>:<kind>:<uuid>` | `handlers_chat_v2.go:387` |
| `web` (legacy) | `agent:<slug>` — wave-1 residue | `webchannel.go:103` |
| `discord` | raw channel/thread snowflake | `broker.go:555-560` |
| `slack` | `"<channelID>:<threadTS>"`, or bare channelID | `broker.go:394-403` |
| `teams` | activity `ReplyToID`, else conversation ID | `activities.go:85-98` |
| `telegram` | decimal int64 forum topic id | `broker_v2.go:752-753` |
| `gchat` | `spaces/X/threads/Y` | `chatapp/adapter.go:594` |

Discrimination is by **prefix sniffing at 20+ call sites**
(`handlers_chat_v2.go:733,1562,1744,1907,1988,2463,2645,2820,2931,3028`;
`webchannel.go:90,103`; `messagebroker.go:473,495`; `events.go:755-828`;
`handlers_agent_messaging.go:306`). The field that *is* the namespace discriminator —
`channel` — is ignored at every one of them.

Consequence: a Discord snowflake arriving via broker-inbound is neither `dm:`- nor
`agent:`-prefixed, so `messagebroker.go:479-486` runs `TouchTopicActivity` against
`webchat_topic` with it. A silent zero-row UPDATE.

---

## 3. `channel` is an untyped, triple-overloaded word

No `ChannelKind` enum exists anywhere in the repo. Three unrelated namespaces share the
term:

| Concept | Type | Values | Defined at |
|---|---|---|---|
| Message transport channel | bare `string` | `web`, `discord`, `slack`, `teams`, `telegram`, `gchat`, plugin-defined | **nowhere** — open set, runtime-registered |
| Notification channel (alert sink) | `NotificationChannel` iface | `discord`, `teams`, `email`, `slack`, `webhook` | `pkg/hub/channels.go:127-143` |
| Native-chat room | does not exist under that name — it is a *Space* (project) + *Topic* | — | `pkg/hub/webchannel_store.go:241` |

Validation is lexical only: `MaxChannelLength = 64`, `^[a-zA-Z0-9-]+$`
(`pkg/messages/types.go:45-49`). Semantic validation is inconsistent — see §6.

### 3.1 `--channel gchat` cannot work

`validateChannel` and the server-side equivalent match on `BusChannel.Name`
(`cmd/message.go:898`, `handlers_agent_messaging.go:198`), but `FanOutEventBus.Publish`
routes on `BusChannel.ChannelID` falling back to `Name` (`pkg/eventbus/fanout.go:87-91`).

The chat-app plugin registers `Name: "scion-chat-app"`, `ChannelID: "gchat"`
(`extras/scion-chat-app/internal/chatapp/broker.go:87,200`). Therefore:

- `--channel gchat` → fails validation ("not registered")
- `--channel scion-chat-app` → passes validation, then fails to route

`BusChannels()` returns both fields (`fanout.go:206-227`); the validators simply read the
wrong one.

Related: Telegram declares no `ChannelID` at all, unlike every sibling plugin
(`extras/scion-telegram/internal/telegram/broker_v2.go:1553-1558`).

---

## 4. The type enum mixes four unrelated concerns

Eight values, one enum (`pkg/messages/types.go:52-80`):

| Value | Actually expresses |
|---|---|
| `instruction` | intent |
| `chat` | intent |
| `input-needed` | lifecycle signal |
| `state-change` | lifecycle signal |
| `assistant-reply` | provenance |
| `system` | provenance |
| `mention` | **delivery artifact** |
| `group-set` | **delivery artifact** |

`mention` and `group-set` are not kinds of message — they are fan-out *copies* of another
message. This is why one logical "message to three participants" becomes N stored rows
carrying different types, and it is the structural reason mention routing has produced two
separate open bugs (`projects/ca-inv-mention-bug/research.md`,
`projects/ca-mgr-mention/research.md`).

`ValidateType`'s error string omits `chat` from the enumerated list
(`pkg/messages/types.go:152`) — the enum has already drifted from its own error message.

---

## 5. The envelope is lossy at the agent boundary

`StructuredMessage` carries 20 fields (`pkg/messages/types.go:122-147`). The agent
receives an 11-field projection (`deliveryMessage`, `pkg/messages/format.go:44-56`) plus a
5-key metadata allowlist (`format.go:34-40`).

Dropped before the agent sees it: `version`, `recipient`, `recipient_id`, `sender_id`,
`plain`, `raw`, `observer_only`, **`status`**, **`visibility`**.

`status` is the only machine-readable value on notification-class messages
(`COMPLETED`, `WAITING_FOR_INPUT`, `DELIVERY_FAILED`). Agents must currently parse it out
of English prose in `msg`.

The allowlist comment itself documents cruft: `"channel"` and `"thread_id"` are listed
"for completeness — callers may set them as metadata instead of (or in addition to) the
top-level fields" (`format.go:31-33`). Two ways to say the same thing, both honoured.

---

## 6. Validation is enforced in the wrong places

`StructuredMessage.Validate()` is **never called on the hub inbound paths** —
neither `handlers_agent_messaging.go` nor `handlers_broker_inbound.go` invokes it. Length
caps, metadata caps, and the `thread_id requires channel` rule are enforced CLI-side only.

Observable consequence: the Teams adapter emits `Channel: ""` with a non-empty `ThreadID`
(`extras/scion-teams/internal/teams/activities.go:77-98`), which `Validate()` explicitly
forbids. It works only because nothing validates.

Other validation gaps:

- `sendGroupMessageViaHub` forwards `--channel`/`--thread-id` without calling
  `validateChannel` (`cmd/message.go:644,723-725`).
- Broker-inbound copies `req.Message.Channel` straight to storage with no check
  (`handlers_broker_inbound.go:242`).
- Local (non-Hub) mode ignores `--channel`, `--thread-id`, `--plain`, `--cc` and
  `@mentions` entirely (`cmd/message.go:317-395`).

---

## 7. Duplicated and divergent implementations

| Duplicated thing | Copies | Divergence |
|---|---|---|
| Mention resolution | `cmd/message.go:932-1007` vs `pkg/messages/mentions.go:105-157` | CLI warns to stderr; shared returns per-slug `MentionResult` |
| Channel validation | `cmd/message.go:888-912` vs `handlers_agent_messaging.go:188-215` | group[] path skips both |
| Leading-`!` → urgent | `messagebroker.go:545-557` vs `handlers_broker_inbound.go:170-177` | one copies the struct, one **mutates in place**; not applied on `POST /agents/{id}/message` at all |
| `MessageRequest` type | `handlers_agent_messaging.go:456` vs `runtimebroker/types.go:479` | different field sets |
| Conversation authorization | `authorizeConversationAccess` (`handlers_chat_v2.go:1985`) vs inlined copies at `:731-763` and `:1561-1590` | the doc comment at `:1980-1984` names the divergence risk that already exists |
| DM key regex | `handlers_chat_v2.go:387` (Go) vs `web/src/components/pages/chat.ts:1038` (TS) | two copies of a wire contract |
| Mention normalization | `mentions.go` (slug), `handlers_chat_v2.go:2916-2927` (displayName/email/local-part), `chat.ts:2258-2265` (TS) | agents match by slug, humans by three other keys, no collision detection |
| "Default agent for this conversation" | `WebChatTopic.DefaultAgent`, Discord/Slack `ChannelLink.DefaultAgent`, chat-app `GetThreadDefault` | the first is itself a slug-or-UUID union resolved by try-slug-then-try-uuid (`handlers_chat_v2.go:933-939`) |

---

## 8. Behavioural inconsistencies across send paths

| Symptom | Detail |
|---|---|
| Scheduled messages lose envelope state | `--channel`, `--thread-id`, `--attach`, `--cc` silently dropped (`cmd/message.go:801-838`); the fired message is re-authored as `sender=scheduler, type=system` (`pkg/hub/server.go:2787-2798`) |
| Type default differs by endpoint | CLI sends `instruction` (`cmd/message.go:627`); hub defaults an omitted type to `input-needed` (`handlers_agent_messaging.go:73-75`) |
| `--raw` is a no-op in Hub mode | Only local mode calls `MessageRaw`; Hub mode just sets the flag |
| `--all` is client-side fan-out | Explicit TODO for a global endpoint (`cmd/message.go:483-484`); no skipped-target breakdown, unlike `--broadcast` |
| `user:` recipients are agent-only | `SCION_AGENT_NAME` gate (`cmd/message.go:610-613`) — a human operator on the CLI cannot message another user |
| User cap ≠ agent cap | 2000 runes user-directed vs 16000 agent-directed; the skill documents 2000 for both |
| Debounce concatenation | The 2s `MessageBuffer` joins pending messages with `\n\n` (`pkg/agent/msgbuffer.go:120`), so an agent can receive multiple envelopes in one paste |

### 8.1 The flag matrix

`scion message` carries 13 flags and **34 pairwise exclusion rules**
(`cmd/message.go:80-229`). The count is itself the diagnosis: the command has accreted
orthogonal concerns (addressing, scheduling, delivery mechanics, subscription management,
attachment staging) onto one verb.

---

## 9. Native chat model gaps

- **No membership table.** Space membership is derived from `ActionRead` on the project;
  thread membership does not exist — every project reader sees every thread
  (`handlers_chat_v2.go:111-120, 2280, 2314`).
- **DM canonicalisation is client-enforced only**, and the two participant kinds use
  *different* rules: user↔user is lexicographically sorted, agent↔user is positional
  (`web/src/components/pages/chat.ts:2273-2282`). The server regex accepts
  `dm:user:<u>:agent:<a>`, but `parseAgentDMKey` only inspects `parts[1]`
  (`handlers_chat_v2.go:2789-2795`), so a reversed key silently loses agent routing.
- **`validDMKey` requires lowercase-hex 36-char UUIDs** (`handlers_chat_v2.go:387`). Any
  non-UUID identity cannot DM. The wave-1 backfill can emit email-based keys
  (`webchannel_store.go:1272-1274`) that permanently 400 that conversation.
- **`@agent` mentions are silently dropped in user↔user DMs** because
  `resolveProjectFromDMKey` returns `""` and the mention step is gated on a non-empty
  project ID (`handlers_chat_v2.go:866, 2799-2808`).
- **Dead rows by construction**: `registerDMParticipants` writes a `webchat_dm` row keyed
  on the agent UUID that nothing ever reads (`handlers_chat_v2.go:2881-2887`).
- **No FK or CHECK on `conversation_key`** in `webchat_read_state` / `webchat_dm`; the
  two-row DM invariant is enforced in application code in three places.
- **Wave-1 residue is load-bearing**: `agent:<slug>` thread ids, `webchat_thread.agent_id`
  holding slug-or-UUID, dual read-state tables, `DEPRECATED(wave-1)` endpoints still
  routed (`pkg/hub/server.go:3628-3629`).

---

## 10. The vocabulary was never defined

`GLOSSARY.md` defines *Message Broker*, *Message Group*, *Native Web Chat*,
*Notification*. It has **no entry** for **Channel**, **Thread**, **DM**, **Mention**, or
**Message** — the five terms the contract actually turns on. The glossary even lists
"thread" under *avoid* (as a synonym for sub-agent), which collides with its messaging
sense.

Doc drift found:

- `resources/platform_skills/scion-messaging/SKILL.md:87` documents a 2000-char limit for
  all messages; code says 16000 for agent-directed.
- The same skill does not mention `--cc`, which has shipped.
- `docs-site/.../hosted/user/messaging.md:176-182` documents a header/body delivery format
  (`sender:` / `type:` / `---`) that the code does not produce — the code emits indented
  JSON.

---

## 11. Settled decisions — constraints, not open questions

Sourced from `.design/` and the scratchpad project archive. The design must work *within*
these unless explicitly re-opened with the owner.

1. **No queuing.** Agent not running → 409, fast fail. No persistent message queue.
   State model: `persisted → delivered | failed`.
   (`projects/message-improvements/design.md`, 2026-06-11, "all policy decisions finalized")
2. **Start-mention vs body-mention** is settled with 10 acceptance criteria. Start mentions
   are primary recipients and are stripped from the body; body mentions produce separate
   messages. The Hub stays type-agnostic for routing; brokers send separate inbound POSTs
   per mention. Bundling and hub-side parsing were both explicitly rejected.
   (`projects/chat-admin/arch-mention-message.md`)
3. **Mention rendering is broker-side.** A shared hub-side mention package was proposed and
   rejected — formats are platform-specific and identity maps are broker-local.
   (`projects/chat-admin/arch-mentions.md`)
4. **`thread_id` requires `channel`**; an unmatched channel is an error, not a silent drop;
   InProcess always receives. (`.design/chat-channel-routing.md`)
5. **Channel links live on the parent channel / forum container**, never on the thread.
   (`projects/chat-admin/arch-scion-thread-cmd.md`)
6. **Outbound agent→user recipient must be explicit.** The `CreatedBy` fallback was
   deliberately removed for messages (it still governs `ask_user`).
   *Note: this is in tension with the stated goal of "send with no recipient named"; see
   `messaging-conversation-model.md` for how the Conversation model resolves it structurally.*
7. **No hub-to-hub HTTP.** Postgres `LISTEN`/`NOTIFY` is the only inter-node transport.
   (`.design/broker-dispatch.md`)
8. **Attachments are shared-volume staging** with a hard error if scratchpad is missing;
   silent drop is the bug being fixed, not the design.
   (`projects/attachment-routing/design.md`)

---

## 12. Pre-existing open bugs the refactor must fix or explicitly not regress

| Bug | Source |
|---|---|
| `/api/v1/message-channels` may be unregistered in some builds → every `--channel` use 404s | `projects/telegram-channel-debug/findings.md` |
| `--attach` transfers path strings only; no file content moves; silently wrong in `clone-per-agent` and `worktree-per-agent` | `projects/attachment-routing/investigation-findings.md` |
| Native chat scrollback broken — client sends `?cursor=`, server reads `?before=`; history capped at newest 50 | `projects/native-chat/feature-gap-analysis.md` |
| Mute and thread-pinning exist in the DB and are honoured on read, but nothing can set them | same |
| Agent-targeted notifications with no broker at dispatch time are left undelivered with no redelivery | `projects/notification-sweep/design.md`, issue #495 (reopened) |
| Body-position `@agent` mentions wrongly become `group-set` primary recipients; the body-mentioned agent receives nothing | `projects/ca-inv-mention-bug/research.md` |
| `dispatch_failure_reason` is persisted but never mapped back through `store.Message`, so it is invisible to API consumers | `projects/message-improvements/review-findings-p4-recheck.md` |
| Slack can emit a `slack:<username>` sender prefix — a fourth identity namespace that bypasses the `user:` permission check | `extras/scion-slack/internal/slack/events.go:375-377` vs `handlers_broker_inbound.go:119` |

---

## 13. What works and must be preserved

Not everything here is broken. The refactor should keep:

- **Fast-fail delivery semantics** — no queue, 409 on a stopped agent. This is a good,
  deliberate decision and users rely on it.
- **The single delivery formatter.** `FormatForDelivery` is the sole agent-facing renderer
  (`pkg/messages/format.go`); one shape for all types is a strength, not a defect.
- **Broker plugin isolation.** Platform-specific rendering, identity mapping and link
  storage living inside each plugin is correct and should not be centralised.
- **Visibility three-state filtering** (`normal` / `verbose` / `full`) — a genuinely useful
  differentiator; its only flaw is that it is dropped at the agent boundary.
- **Observer spokes.** `ObserverOnly` republication lets plugins see agent↔agent traffic
  without re-dispatching it. Keep.
- **Project-scoped isolation.** All queries and deliveries scoped by `ProjectID`.
- **Reply affinity as a concept** — the implementation is incomplete (no thread), but
  "reply where the human last spoke" is the right default behaviour.
