# A2A bridge: a federated user's reply is read from its own conversation

**Date:** 2026-10-18
**Tracked in:** upstream issue 4603

## What changed

- With `reply_poll.enabled` on, a federated user caller's reply is read
  from its own direct conversation with the agent
  (`GET /api/v1/chat/conversations/{dmKey}/messages`), with the caller's
  own user access credential through `federatedUserGet`
  (`visitorDMReader`). A hub refusal (401, 403, 404) ends the call with the
  uniform refusal text; other failures are retried until the deadline.
- The preflight and the access re-check before a reply is forwarded make
  the same read (`visitorDMReader.check`), so the conversation key and
  path are derived in one place. A key that cannot be derived is the
  uniform refusal, before anything is sent.
- The poller is built with exactly one read source
  (`newVisitorReplyPoller` for a federated user, `newReplyPoller` for
  everyone else).

## Tests

- The bridge test hub pages the conversation (limit, opaque cursor). Unit
  tests cover cursor forwarding, too many newer pages, an unavailable
  preflight read, an agent question, a pushed reply while polling, and a
  key that cannot be derived; each was checked to fail under a mutation of
  the line it covers.

## Notes

- A pushed reply is ignored while polling by the `pollOnlyEvent` wait
  predicate, which is the guarantee, and by the poller-live drop in
  `HandleBrokerMessage`, which is an optimisation (no append, no state
  update, no push dispatch). Each has its own unit test
  (`TestHandleBrokerMessage_DropsPushWhilePollerLive` and the
  `pollOnlyEvent` wait tests). The executor-level test
  `TestReplyPoll_VisitorIgnoresPushedReply` fails only when both are
  removed.
