# Confirmed decisions and messaging-mode delegation check

Updated 2026-09-17. Records the user's 15:16 UTC follow-up in conversation
`1f0b5c6a-4352-4079-9522-0d7c88976e76`. Source inspection is against commit
`1b0a25a9774cdfa505c98debf29d7f1812acf6fc`; no product code was changed and no
runtime exploit or integration test was attempted.

## Confirmed decisions

| Question | Decision | Implementation consequence |
|---|---|---|
| Must a receiving agent also have hub mode? | No; a hub sender can reach a project-mode recipient. | Require sender `hub`, recipient `project` or `hub`, Hub enabled, and eligible destination policy. Receiving does not grant reply authority. |
| May full-role agents select hub mode? | Yes, only if they already have hub mode. | Add a shared effective-mode grant guard to creation and later mode mutation; full role by itself is insufficient. |
| Are cross-project groups part of the first release? | Start with DMs, preserving the option to add groups. | Reuse generic conversation storage, keep direct-pair invariants kind-specific, and version supported kinds. Do not bake permanent project confinement into the participant schema. |
| What does global disable block? | Both cross-project sends and agent history reads. | Recheck pending/retried delivery and all agent read surfaces; retain stored records and authorized human audit access. Already delivered harness content cannot be recalled. |

## Source check: role ceiling exists; messaging-mode ceiling does not

1. **Creation has a role ceiling.**
   `pkg/hub/handlers_agents_core.go:645` reads the creator's stored role and
   message mode. At line 681, explicit higher role requests are rejected using
   `CompareRoles`; line 688 calculates the effective role with `minRole`.
   `pkg/hub/agentrole.go:95` compares agent roles, not messaging modes.
2. **Mode resolution is inheritance, not a ceiling.**
   `pkg/hub/handlers_agents_core.go:1048` chooses explicit request, template,
   parent mode, then project default. Request/template values receive enum
   validation only. Neither path compares the resolved mode with the creator's
   messaging mode. An otherwise-authorized branch-mode creator can select
   project mode through an explicit request or template under this code.
3. **The early creation gate does not supply the missing check.**
   `pkg/hub/authorize.go:136` authorizes agent creation using action scope and
   same-project confinement. It does not receive the resolved messaging mode.
4. **Later mode mutation also lacks a mode ceiling.**
   `pkg/hub/handlers_agent_message_mode.go:127` checks the agent's full-role
   scope and same-project target, without comparing caller and requested modes.
   Line 247 applies the requested mode. `cascadeMessageMode` at line 295 walks
   descendants and line 333 assigns the requested mode to each.
5. **Full role is a separate concept.**
   `pkg/hub/agentrole.go:55` includes both creation and set-message-mode scopes
   in the full-role scope set. Having those scopes is not proof that the agent
   holds hub messaging mode. Existing role tests do not establish a messaging
   delegation invariant.

Conclusion: merely adding `hub` to the enum would expose a grant path to
otherwise-authorized non-hub agents. The implementation must add the guard;
it cannot rely on inheritance or the role ceiling to provide it.

## Required guard contract

Use the same helper from all effective-mode mutation paths:

```text
AuthorizeMessageModeGrant(authenticated actor, target, resolved mode)
  first require existing action-specific authorization
  if this is not a new hub grant: retain existing mode rules
  if human caller: retain existing action-specific human authorization
  if agent caller:
    load current caller record; lookup failure denies the grant
    require caller's stored role == full AND stored message mode == hub
    intersect with authenticated action scopes and other credential limits
    require caller's project == target project
  otherwise deny
```

For creation, any effective `hub` is a new grant, even if it came from a
template, parent inheritance, or a default. For existing agents, a transition
to hub is a grant; restart or unrelated update retaining hub is not. Applying
a cascade to an unchanged hub root still requires checks for descendants that
would newly receive hub mode. No input path may bypass effective-mode
resolution and its guard.

| Agent caller | May grant hub within its own project? |
|---|---|
| Current full role + hub mode + required action scope | Yes, subject to existing target/action authorization. |
| Current full role + any other messaging mode | No, including self-promotion. |
| Current hub mode + any lower role | No. |
| Stale token scope but current role or mode revoked | No. |
| Missing/corrupt caller record or policy-store failure | No; distinguish retryable infrastructure failure from policy denial. |

Read fresh authority at mutation and enforce consistency with concurrent
downgrades; an old preview is not a grant. Dry-run uses the same check. Cascade
execution must stop or deny remaining unauthorized grants after revocation and
report outcomes accurately; it must not silently clamp hub to project mode.

Preserve human authorization separately: permission to create an agent is not
automatically permission to change every existing agent's mode. Agent ancestry
does not let an agent use its originating human's mutation privileges. A
human-authorized seed or an already-authorized full/hub agent bootstraps hub
mode. A full/hub parent can create a lower-role hub child under the independent
role ceiling; that child can send externally but cannot delegate hub onward.

The Hub availability switch does not rewrite stored modes. Authorized full/hub
agents may stage grants while it is disabled, but cannot enable the switch or
send/read externally. This separates stored delegation authority from current
feature availability.

Scope the new rule to the hub privilege. The existing none/lineage/branch/project
modes are communication cells, not a simple total ordering. A general legacy
mode-ranking change would require a separate policy decision; the user-confirmed
hub ceiling does not silently redefine those transitions.

## DM-first without a group dead end

The first release must advertise only direct cross-project conversations and
reject foreign-room operations before effects. Keep the directional message
evaluator independent of conversation kind and retain arbitrary group
participants. A group's project remains its owner, not a database constraint
that every future participant must belong to that project.

Future group support can add explicit admission/invitations, per-recipient
receive consent, history visibility/revocation, and observer rules. A permitted
DM edge must never authorize an entire room transcript. Adding a group uses a
new conversation ID and explicit membership; it does not mutate a DM key or
retroactively share its history. See [design section 7](02-design.md#7-conversation-authorization-and-history).

## Acceptance coverage and remaining proposed details

The [delivery plan](03-delivery-plan.md) includes creation request/template/
inheritance/default paths; self/peer/child mutation; cascade and dry-run;
caller revocation/store failure; human seed; project-recipient read/denied reply;
later authorized promotion and reciprocal reply; and Hub-off sends/history.

The design still labels finer details as proposals: exact eligible built-in
membership roles and group semantics, policy mutation governance, narrow
human observation, and history access after individual mode/policy changes.
The recommended history rule permits a canonical participant to read while at
least one direction remains authorized and the Hub feature is enabled. It
allows project-mode recipient reads without an implicit reply grant. None of
these details weakens the confirmed global-disable behavior.
