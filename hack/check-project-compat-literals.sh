#!/usr/bin/env bash
# Flags legacy grove literals outside known compatibility, test, fixture, and
# example surfaces. Keep this allowlist explicit: new files with legacy names
# should either route through pkg/projectkeys or be added here with intent.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v rg >/dev/null 2>&1; then
  echo "Warning: ripgrep (rg) not found — skipping compat-literals check" >&2
  exit 0
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

# With -i, scion\.grove|grove_id|groveId|/groves are all substrings of
# grove, so the single alternative below is the same match set.
rc=0
rg -in 'grove' \
  cmd pkg extras \
  --glob '*.go' \
  --glob '!pkg/ent/**' >"$tmp" || rc=$?
# rg exits 1 for "no matches", which is normal and leaves $tmp empty; that
# case falls through to the stale-entry check below so a fully-empty match
# set still reports every allowlist entry as stale rather than passing
# silently. Only exit codes above 1 (bad pattern, unreadable path, etc.) are
# real failures.
if (( rc > 1 )); then
  echo "rg failed (exit $rc)" >&2
  exit "$rc"
fi

allowed_paths=(
  # Current compatibility and migration tests/fixtures.
  "^cmd/command_tree_grove_guard_test.go$"
  "^cmd/config_test.go$"
  "^cmd/hub_test.go$"
  "^cmd/legacy_grove_migration_test.go$"
  "^cmd/list_test.go$"
  "^cmd/message_project_json_test.go$"
  "^cmd/template_resolution_test.go$"
  "^extras/agent-viz/internal/logparser/parser_test.go$"
  "^extras/fs-watcher-tool/pkg/fswatcher/project_test.go$"
  # Asserts a scion.grove.* user topic yields no user ID from
  # extractUserIDFromTopic. The literal is the point of the negative test.
  "^extras/scion-a2a-bridge/internal/bridge/pgstore_crossprocess_test.go$"
  "^extras/scion-chat-app/internal/chatapp/notifications_test.go$"
  "^extras/scion-chat-app/internal/state/state_test.go$"
  # Asserts parseTopicComponents does not treat a scion.grove.* prefix as a
  # project topic: the project ID falls back to the whole topic string. The
  # literal is the point of the negative test.
  "^extras/scion-discord/internal/discord/broker_test.go$"
  "^extras/scion-slack/internal/slack/broker_test.go$"
  "^extras/scion-telegram/internal/telegram/broker_v2_test.go$"
  # Asserts SCION_GROVE is absent from RunConfig.Env alongside a SCION_PROJECT
  # presence check. The literal is the point of the negative assertion.
  "^pkg/agent/run_test.go$"
  "^pkg/agent/run_shared_dir_storage_test.go$"
  "^pkg/api/types_test.go$"
  "^pkg/brokerclient/agents_test.go$"
  "^pkg/config/koanf_test.go$"
  "^pkg/config/legacy_grove_migration_test.go$"
  "^pkg/config/project_discovery_test.go$"
  "^pkg/config/project_marker_test.go$"
  "^pkg/config/settings_v1_test.go$"
  "^pkg/config/shared_dirs_test.go$"
  "^pkg/config/v7_fixes_test.go$"
  "^pkg/hub/capability_marshal_test.go$"
  # Mixes concurrent legacy "grove" and canonical "project" clone requests
  # for the same destination; the literal is the point of the negative
  # assertion (every "grove" request must be rejected with 400).
  "^pkg/hub/clone_concurrency_test.go$"
  # Regression test proving grove.<projectId>.* duplicate SSE subjects are
  # never published by pkg/hub/events.go. The literal is the point of the
  # test: it subscribes to the legacy wildcard and asserts nothing is ever
  # delivered there.
  "^pkg/hub/events_test.go$"
  "^pkg/hub/fs_safety_test.go$"
  # Asserts TokenResponse no longer emits a groveId key alongside projectId.
  # The literal is the point of the negative test.
  "^pkg/hub/handlers_auth_test.go$"
  "^pkg/hub/handlers_broker_inbound_test.go$"
  # Asserts groveId is no longer a recognized notification filter alias: an
  # unrecognized query param is ignored rather than treated as projectId. The
  # literal is the point of the negative test.
  "^pkg/hub/handlers_notifications_test.go$"
  "^pkg/hub/handlers_project_test.go$"
  # Asserts a broker heartbeat body keyed by the removed "groves"/"groveId"
  # names decodes to zero projects and never updates agent state. The
  # literal is the point of the negative test.
  "^pkg/hub/handlers_runtime_brokers_test.go$"
  # Asserts the hub rejects the removed "grove" harness-config scope (and
  # other unrecognized scopes) with 400 instead of storing it and flattening
  # its storage path. The literal is the point of the negative tests.
  "^pkg/hub/harness_config_scope_validation_test.go$"
  "^pkg/hub/httpdispatcher_test.go$"
  # Asserts the project cache/sync-status responses no longer emit a groveId
  # key alongside projectId. The literal is the point of the negative tests.
  "^pkg/hub/project_cache_test.go$"
  "^pkg/hub/project_webdav_test.go$"
  # Regression test for SSE subject authorization default-deny: proves a
  # non-member is denied on the legacy grove.* subjects and other unknown
  # namespaces.
  "^pkg/hub/sse_default_deny_test.go$"
  "^pkg/hub/template_clone_scope_test.go$"
  # Asserts the hub rejects the removed "grove" template scope (and other
  # unrecognized scopes) with 400 instead of storing it as-is. The literal is
  # the point of the negative test.
  "^pkg/hub/template_scope_validation_test.go$"
  "^pkg/hub/web_test.go$"
  "^pkg/hubclient/agents_test.go$"
  "^pkg/hubclient/client_test.go$"
  "^pkg/hubclient/notifications_test.go$"
  "^pkg/hubclient/projects_test.go$"
  # Asserts CreateAgentRequest, CreateSubscriptionRequest,
  # CreateSubscriptionTemplateRequest and CreateTokenRequest no longer emit a
  # groveId key. The literal is the point of each negative test.
  "^pkg/hubclient/request_no_grove_marshal_test.go$"
  "^pkg/hubclient/runtime_brokers_test.go$"
  "^pkg/hubclient/scheduled_events_test.go$"
  "^pkg/hubclient/schedules_test.go$"
  "^pkg/hubclient/templates_test.go$"
  "^pkg/hubclient/tokens_test.go$"
  "^pkg/hubclient/types_test.go$"
  # Asserts legacy top-level and hub project-id config key names (and the
  # SCION_GROVE_ID / SCION_HUB_GROVE_ID env vars) are no longer recognized.
  # The literal is the point of each negative test.
  "^pkg/projectkeys/config_test.go$"
  # Asserts a scion.grove.* topic is rejected as malformed rather than parsed
  # as a project topic. The literal is the point of the negative test.
  "^pkg/projectkeys/topics_test.go$"
  "^pkg/runtime/cloudrun_sandbox_runtime_test.go$"
  # Asserts SCION_GROVE and SCION_GROVE_ID are absent from buildCommonRunArgs
  # output alongside a SCION_PROJECT/SCION_PROJECT_ID presence check. The
  # literal is the point of the negative assertion.
  "^pkg/runtime/common_test.go$"
  # Builds a legacy ".scion/groves/<name>" path to prove an agent's recorded
  # project-path label is still found after config.MigrateLegacyGlobalLayout
  # moves the project and leaves a per-entry symlink behind. The literal is
  # the point of the test. Same test and reason as pkg/runtime/podman_test.go
  # below.
  "^pkg/runtime/docker_test.go$"
  "^pkg/runtime/k8s_runtime_test.go$"
  # Builds a legacy ".scion/groves/<name>" path to prove an agent's recorded
  # project-path label is still found after config.MigrateLegacyGlobalLayout
  # moves the project and leaves a per-entry symlink behind. The literal is
  # the point of the test. Same test and reason as pkg/runtime/docker_test.go
  # above.
  "^pkg/runtime/podman_test.go$"
  "^pkg/runtimebroker/delete_project_scope_test.go$"
  "^pkg/runtimebroker/handlers_test.go$"
  "^pkg/runtimebroker/legacy_layout_migration_test.go$"
  "^pkg/runtimebroker/protocol_mismatch_test.go$"
  # Asserts that a groveId-only PTY attach query no longer scopes the lookup
  # (two same-slug agents in different projects become ambiguous). The
  # literal is the point of the negative test.
  "^pkg/runtimebroker/server_lookup_test.go$"
  "^pkg/runtimebroker/start_context_test.go$"
  "^pkg/runtimebroker/types_test.go$"
  "^pkg/runtimebroker/workspace_handlers_test.go$"
  "^pkg/sciontool/telemetry/aggregator_test.go$"
  "^pkg/storage/storage_test.go$"
  "^pkg/store/entadapter/agent_session_metrics_projectid_test.go$"
  # Seeds scope='grove' rows with raw SQL to prove the data migration in
  # legacy_scope_migration.go rewrites them; the literal is the point of the
  # test.
  "^pkg/store/entadapter/legacy_scope_migration_test.go$"
  "^pkg/store/models_json_test.go$"
  # Asserts ConnectMessage/StreamOpenMessage no longer emit or decode the
  # removed "groves"/"groveId" names. The literal is the point of the
  # negative tests.
  "^pkg/wsprotocol/protocol_test.go$"

  # First-party integration compatibility boundaries.
  "^extras/agent-viz/internal/logparser/parser.go$"
  "^extras/scion-chat-app/internal/state/state.go$"
  "^extras/scion-telegram/internal/telegram/broker_v2.go$"

  # Core compatibility adapters and bounded legacy protocol/storage surfaces.
  "^pkg/config/koanf.go$"
  "^pkg/config/legacy_grove_migration.go$"
  "^pkg/config/project_marker.go$"
  "^pkg/config/settings_v1.go$"
  # Accepts the legacy "grove" template-list scope query value and maps it to
  # "project" before it drives the scope switch or the store filter.
  "^pkg/hub/template_handlers.go$"
  # isHubManagedWorktreeBase distinguishes a hub-native worktree-per-agent
  # base (under ~/.scion/projects/<slug> or the legacy
  # ~/.scion/groves/<slug>) from a linked project (the user's own checkout),
  # mirroring config.ProjectsDir/legacyProjectsDirName (pkg/config/paths.go,
  # already allowlisted above) so the read-only admin-dir mount narrowing
  # applies to both current and pre-migration hub-native project layouts.
  "^pkg/runtime/common.go$"
  "^pkg/runtimebroker/handlers.go$"
  # Reserved-identity-attribute denylist: the three retired grove-named
  # telemetry keys (scion.grove, scion.grove.id, scion.grove_id) are kept
  # here so the receiver still strips them from user-supplied attributes,
  # even though it no longer treats them as valid identity sources (Q2 = (a)).
  "^pkg/sciontool/telemetry/policy.go$"
  "^pkg/sciontool/telemetry/policy_test.go$"
  "^pkg/storage/storage.go$"
  "^pkg/store/entadapter/composite.go$"
  # One-shot data migration rewriting stored scope='grove' rows to 'project';
  # removed once no hub can have a pre-migration row left to normalize.
  "^pkg/store/entadapter/legacy_scope_migration.go$"
)

allowlist="$(printf '%s\n' "${allowed_paths[@]}" | sed 's/\$$/:/' | paste -sd '|' -)"

# Stale entry detection: every allowlisted path is expected to match the
# literal search above. An entry that matches nothing here is dead
# bookkeeping - either the legacy literal was removed from that file, or the
# entry never matched and was copied in by mistake. Fix by deleting the entry,
# not by re-adding the literal it once excused.
stale_entries=()
for path in "${allowed_paths[@]}"; do
  pattern="${path%\$}:"
  if ! grep -qE "$pattern" "$tmp"; then
    stale_entries+=("$path")
  fi
done

violations=""
if [[ -n "$allowlist" ]]; then
  violations="$(grep -Ev "$allowlist" "$tmp" || true)"
elif [[ -s "$tmp" ]]; then
  violations="$(cat "$tmp")"
fi

# Report both failure classes from one run rather than stopping at whichever
# is checked first - otherwise fixing one class only reveals the other on
# the next CI attempt.
failed=0
if [[ ${#stale_entries[@]} -gt 0 ]]; then
  echo "Stale entries in the project compatibility allowlist (match nothing):" >&2
  printf '  %s\n' "${stale_entries[@]}" >&2
  echo >&2
  echo "Remove these entries from hack/check-project-compat-literals.sh." >&2
  echo >&2
  failed=1
fi
if [[ -n "$violations" ]]; then
  echo "Legacy grove literals found outside the project compatibility allowlist:" >&2
  echo "$violations" >&2
  echo >&2
  echo "Use project vocabulary for new code, or route legacy handling through pkg/projectkeys." >&2
  failed=1
fi
if [[ "$failed" -eq 1 ]]; then
  exit 1
fi
