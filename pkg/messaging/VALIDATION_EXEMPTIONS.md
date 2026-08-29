# Validation Choke Point Exemptions

The following server-generated message emitters are exempt from
`ValidateLegacyMessage` because they construct messages entirely from
server-internal state, not from untrusted user input.

## 1. Mention Fan-Out (handlers_chat_v2.go, sendAgentRouted)

**Constructor:** `messages.NewMention(msg.Sender, recipient, content, msg.Recipient)`

The mention fan-out creates copies of the primary chat message for additional
mentioned agents. The primary message has already been validated through
`ValidateLegacyMessage` by the time fan-out occurs. The fan-out message is a
server-constructed derivation: same body, same metadata, different recipient.
No user input is introduced.

## 2. Notification Dispatch (notifications.go)

**Constructor:** `messages.NewNotification(sender, recipient, msg, msgType)`
**Call sites:** Lines ~376, ~431, ~449

Notification messages are constructed from server-internal notification state:
- Sender/recipient are agent/user slugs from the store
- Body is the notification message from the notification store
- Type is derived from notification status (state-change or input-needed)

No user-supplied input flows into these messages. They are system lifecycle
signals between the hub and agents/channels.

## 3. Scheduler Message (server.go)

**Constructor:** `messages.NewSystemMessage(sender, recipient, msg, category)`
**Call site:** Line ~2830

Scheduler messages deliver previously-scheduled event payloads. The payload
message content was validated when the scheduled event was created (through
the scheduling API). The sender is the literal string "scheduler" and the
category is `SystemCategoryScheduler`. No new user input is introduced at
dispatch time.

---

**Rationale:** Routing these through `ValidateLegacyMessage` would add
latency and complexity for messages that by construction cannot contain
invalid user input. The choke point protects against untrusted external
input; server-generated internal messages with hardcoded types, senders,
and channels do not benefit from re-validation.

If any of these emitters is modified to accept user-supplied content in the
future, it MUST be routed through the validation choke point at that time.
