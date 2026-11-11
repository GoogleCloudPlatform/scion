# Choices v5 delta, PR A2: answer a choice set in one submit (ptone/scion#7672)

Base: upstream main at `60fc1bda` (PR A merged), then upstream main
merged in twice for the open pull request (`9ce34fdd`, `9e87b089`).
Design: native-chat choices v5 delta (additive to v4), sections 4.1 to
6, hub only. PR B (web cards) builds on it.

## What was built

- **Storage.** Decision answers gain `batch_id` (immutable, indexed) and
  `matched_default` (immutable, default false); no destructive migration,
  no downgrade floor change. `RecordAnswerSet` records a whole set in one
  transaction under one batch id: it locks the prompt's rows (ordered,
  `FOR UPDATE` on Postgres), replays an identical submit by the same user
  under its idempotency key first, rechecks every row against the
  actionable set (reasons `replaced`, `decided`, `not_recorded_answerer`,
  `missing`, `not_actionable`) and writes nothing on any mismatch.
  `RecordAnswer` sets `matched_default` too.
- **Set endpoint.** `POST /api/v1/choice-prompts/{messageId}/answers`
  behind `web.chat_choices`, in the route metadata, the route
  authorization manifest and the operation catalog
  (`chat.choiceprompt.answer`). Session or dev credential only, as for the
  per-decision route. Shape errors are 400; a changed actionable set is
  409 `answer_set_changed` with the current views and a reason per row;
  the same idempotency key with other answers is 422; the chat send rate
  limit applies to every submit.
- **Per-decision endpoint.** A row of a prompt with two or more decisions
  is 409 `batch_required`.
- **One response per submit.** The set's choice-response lists every
  answer in position order. The agent receives one line per decision
  plus the hub-set `choice_batch` and `choice_answers` metadata (reserved
  keys: client values are stripped first, the hub sets them after).
  `matched_default` marks a line "recommended".
- **Views, SSE and history.** Prompt views carry an answer mode (`single`
  or `set`); chat history and SSE carry the set response
  (`messageChoiceAnswerModes`, `messageChoiceResponseSets`,
  `choiceResponseSet`), and `choice.updated` is published once per row.
- **Fallback.** The markdown fallback of a set reads "N decisions needed.
  Answer all of them together." and ends with the "answer all" link and a
  one-line-per-decision reply hint.
- **Asker lookup.** Both answer endpoints read the asking agent through
  one helper, `choiceAsker`, before anything is recorded: a stopped or
  deleted asker (not found) is the gone case (answers recorded, response
  row stored failed, nothing delivered, as design v4 specifies); any other
  lookup error is an error response. The per-decision endpoint from PR A
  had the same pattern and gets the same handling.

## Decisions and their reasons

- The replay check runs before the reason check, so an identical replay
  after a success answers 200 with `replayed`, not 409 `decided`.
- The set response lists the views of every decision of the prompt,
  replaced rows included, so a card can refresh from one answer.
- Within a set the fallback keeps v4's decision and choice lines; the
  question is the agent's markdown and is not wrapped in bold.
- A user who may not message the asking agent (could not @-mention it in
  the conversation) cannot answer its prompt either: 403
  `message_denied`, nothing recorded. In a shared topic that includes a
  project member who neither started the agent nor owns the project. The
  web card shows such choices disabled, with the reason.

## Tests

- Store: set recording, replay scope (per user), every mismatch reason,
  nothing written on mismatch, `matched_default`, and a Postgres race
  test where exactly one of two concurrent submits wins.
- Hub: shape, idempotency, conflict reasons, outcomes, the messaging
  guard, publishes and delivery, concurrent submits behind a start
  barrier, a gone asker on the set endpoint, and an injected asker
  lookup fault on both endpoints (through the store fault switch, never
  a store reassignment).
- Fallback rendering for sets up to ten decisions, keys with `-` and `_`.

## Check results

- Every slice: `go vet`, scoped lint, `gofmt`, the touched packages'
  tests, and the hub test shard holding the choices tests.
- Pull request CI then failed on the publish-site registries
  (`TestPersistedRowEffectEnumeration`, `TestMemberFanoutFollowsPublish`):
  `publishChoiceResponseSet` was listed in neither. The local runs of
  that hub test shard had used a test name filter for the choices tests.
  After the fix, the shard ran in full.
- Both merges of main were conflict-free; the generated catalog report
  changed on both sides the second time, and its generator confirmed the
  combined file.
- An end-to-end run against a locally started hub (127.0.0.1 only,
  test login, a broker in the test driver's process with no container)
  passed
  criteria 18 to 24: answer modes; `batch_required`; the shape, credential
  and reader refusals; replay and 422; missing, success, decided and
  replaced; one response with three answers in order, its SSE frames, and
  the agent delivery with one line per decision and both metadata keys;
  `matched_default` and the "recommended" mark; the race with one winner
  and the smaller set after an update. The foreign-interpreted reason has
  no live route before phase 2 and is test-covered.

## Process notes

- PR A's note held and was missed again: a guard test that enumerates
  publish sites must list a new publish site. Run the whole hub shard
  that holds a changed file, not a filtered test set.
- Source-scanning registry tests in `pkg/hub`: 147 test functions in 77
  test files parse Go source (directly or through a test helper). A new
  call site of a tracked kind can fail any of them. The files:

  access_constraint_delete_audit action_map_reads_guard
  admin_user_invite_batch_policy agent_delegation_arms
  agent_delegation_branches agent_delegation_list_routes
  agent_pending_config_effect_http agent_run_writers agent_upsert_scan
  area_scan_coverage artifacts_grant_notify artifacts_sponsor_review
  attachments_artifacts_reach attachments_p4 attachments_refs
  auth_external_bearer authz_delivery_credential authz_provenance_root
  authzop_catalog_method_inventory collected_read_guard
  conduit_broker_rpc conduit_ingress_broker_nonce conduit_open_renewal
  conduit_placement_memo conduit_proxy conduit_stream_recheck
  conduit_stream_renewal create_message_enumeration
  credential_decoration_authz_guard delegation_edge_writers
  deletion_redact dispatch_metadata_seal_sites
  dispatch_state_enumeration encryption_ring events_postgres_hooks
  federation_principal_check_census handlers_access_constraints
  handlers_broker_inbound_attachments
  handlers_gcp_identity_mint_authority
  harness_config_resource_literal_guard identity_attestation_guard
  identity_classification keyring_legacy_compare local_backend_clone
  message_artifacts_coverage message_artifacts_recipient
  msg_containment_callsite multipart_form_pin
  permission_registry_callsites permission_registry project_leave_class
  project_membership_applyroleplantx_ast project_owner_grant
  project_settings_registry publish_site_enumeration
  resource_type_registry_guard rs1_extended rs2_list_authz
  rs3_project_delete rs4_credential run_intent_guard sa_parent_ceiling
  scheduled_authority secret_parent_ceiling session_generation_relay
  skill_resource_literal_guard storage_layout_view
  template_resource_literal_guard test_admin_a3_census
  test_admin_census test_admin_gate test_admin_policy_census
  test_identity_offline_admin_delegation
  test_identity_offline_delegation test_identity_web_driver test_mode
  uat_intrinsic

- An end-to-end test driver that observes the agent side must key
  deliveries by the agent reference the broker receives (its slug), not
  the agent id.

## Left open

- PR B (web set card).
- Phase 2 (interpreted answers, multi-pair record).
