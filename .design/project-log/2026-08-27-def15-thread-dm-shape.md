# DEF-15: dm:-prefixed ThreadID produces third DM shape

## Date
2026-08-27

## Discovery
During merge verification of origin/main into scion/messaging-v2, the architect
identified that a dm:-prefixed ThreadID (e.g. "dm:agent:X:user:Y") takes the
thread resolution path instead of the DM resolution path.

## Root cause
handlers_agent_messaging.go line 244 (outbound) and line 848 (inbound):
```
if req.ThreadID != "" {
    convResult = ResolveOrCreateThreadConversation(...)
} else if ... {
    convResult = ResolveOrCreateDMConversation(...)
}
```
ThreadID is PRIORITISED over pair-based DM resolution. A dm:-prefixed ThreadID
never reaches DMConversationKey. Instead, ResolveOrCreateThreadConversation
(conversation.go:158) builds:
- external_ref = "thread:<projectID>:dm:agent:X:user:Y"
- kind = "group"
- project-scoped

## Impact
- Third DM shape: DEF-8 again, in the code that was supposed to cure DEF-8
- Bypasses key-based authorization (kind != "direct")
- Project-scoped instead of global (contradicts DEF-10)
- Two affected sites: outbound handler (:244) and inbound handler (:848)

## Status
Logged as DEF-15. Skipped test committed. Architect will spec the fix.
Acceptance criterion: delete the t.Skip line and the test passes.
