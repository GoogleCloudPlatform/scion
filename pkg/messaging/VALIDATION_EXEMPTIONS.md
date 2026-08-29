# Validation Choke Point Exemptions

The following server-generated message emitters are exempt from
`ValidateLegacyMessage`. They construct messages entirely from
server-internal state, not from untrusted user input.

## 1. Mention Fan-Out (`sendAgentRouted` in handlers_chat_v2.go)

**Constructor:** `messages.NewMention(msg.Sender, recipient, content, msg.Recipient)`

The mention fan-out creates copies of the primary chat message for additional
mentioned agents. The primary message has already been validated through
`ValidateLegacyMessage` by the time fan-out occurs. The fan-out message is a
server-constructed derivation: same body, same metadata, different recipient.
No user input is introduced.

## 2. Notification Dispatch (`SendNotification` and related functions in notifications.go)

**Constructor:** `messages.NewNotification(sender, recipient, msg, msgType)`

Notification messages are constructed from server-internal notification state:
- Sender/recipient are agent/user slugs from the store
- Body is the notification message from the notification store
- Type is derived from notification status (state-change or input-needed)

No user-supplied input flows into these messages. They are system lifecycle
signals between the hub and agents/channels.

## 3. Scheduler Message (`runScheduledEvents` in server.go)

**Constructor:** `messages.NewSystemMessage(sender, recipient, msg, category)`

Scheduler messages deliver previously-scheduled event payloads. The payload
message content was validated when the scheduled event was created (through
the scheduling API). The sender is the literal string "scheduler" and the
category is `SystemCategoryScheduler`. No new user input is introduced at
dispatch time.

**Known property:** Validation at creation time means that if validation rules
tighten after an event is scheduled, a long-lived scheduled event fires a
payload that was never checked against the new rule. This is accepted as a
property of deferred execution — the payload was valid when the user created it.

---

**Rationale:** Routing these through `ValidateLegacyMessage` would add
latency and complexity for messages that by construction cannot contain
invalid user input. The choke point protects against untrusted external
input; server-generated internal messages with hardcoded types, senders,
and channels do not benefit from re-validation.

If any of these emitters is modified to accept user-supplied content in the
future, it MUST be routed through the validation choke point at that time.

**Enforcement:** DEF-37 tracks a marker gate that would enforce the exempt
set mechanically, so a future emitter cannot join it silently. This gate is
not yet implemented. If it should be built, that decision belongs to the
project's CI owners, not to the team changing the validation layer.

## Unreachable branches in validateMessageContent

After the removal of `ValidateMessage` (D3, Tranche D), `validateMessageContent`
is called only by `ValidateLegacyMessage`. Two of its branches have no
reachable caller and no test:

1. **MaxMsgSize byte-length check** (`len(msg.Body) > messages.MaxMsgSize`).
   Unreachable by arithmetic, independent of callers: `MaxMessageLength` is
   16000 runes, and a UTF-8 rune is at most 4 bytes, so 16000 runes is at
   most 64000 bytes -- always under `MaxMsgSize` (65536). The rune-count
   check above it fires first, always. No input can reach this branch.

2. **ReplyToID empty-string check** (`msg.ReplyToID != nil && *msg.ReplyToID == ""`).
   Unreachable only because the legacy mapping is the sole caller.
   `MapLegacyEnvelope` (envelope_compat.go) sets `ReplyToID` by taking the
   address of a non-empty `ThreadID` inside a non-empty guard, so it can
   never produce a pointer to `""`. This branch becomes reachable and
   untested the moment Tranche G adds a native validation entry point;
   whoever adds that entry point owes a test for this branch.

Neither branch is deleted -- removing defensive checks is a behaviour change
and out of scope for this cleanup.
