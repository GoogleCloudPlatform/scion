// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"strings"
	"time"
)

// The fixture is a Go-defined spec that seeds at least one representative row
// per table of the hub schema, deliberately exercising the edge cases that most
// often break a SQLite->Postgres migration:
//
//   - NULL optional fields    (nullable columns left unset)
//   - max-length strings      (multi-kilobyte text values)
//   - nested / unicode JSON   (emoji + multi-byte scripts in JSON columns)
//   - soft-deleted rows       (deleted_at populated alongside a live row)
//
// IDs are shared across tables (a single project/user/agent/broker/...) so the
// fixture is internally coherent — foreign keys point at rows that exist —
// even though the loader disables FK enforcement while inserting in spec order.

// Shared, stable identifiers referenced across multiple tables.
const (
	projectID = "11111111-1111-1111-1111-111111111111"
	userID    = "22222222-2222-2222-2222-222222222222"
	agentID   = "33333333-3333-3333-3333-333333333333"
	brokerID  = "44444444-4444-4444-4444-444444444444"
	groupID   = "55555555-5555-5555-5555-555555555555"
	policyID  = "66666666-6666-6666-6666-666666666666"
	subID     = "77777777-7777-7777-7777-777777777777"
)

// baseTime is a fixed timestamp so the generated fixture is byte-reproducible
// across runs (no time.Now()).
var baseTime = time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)

// maxLenString is a deliberately long value used to exercise large TEXT
// handling and column-length assumptions.
var maxLenString = strings.Repeat("x", 8192)

// unicodeJSON is a nested JSON document mixing emoji and multi-byte scripts.
const unicodeJSON = `{"team":"🚀 platform-éñ","nested":{"langs":["日本語","العربية","emoji 😀"],"depth":{"level":2,"ok":true}}}`

// nestedConfigJSON is a representative nested config blob.
const nestedConfigJSON = `{"harness":"claude","env":{"LOG_LEVEL":"debug","UNICODE":"naïve café 北京"},"args":["--flag","value"]}`

// TableFixture is the seed data for a single table.
type TableFixture struct {
	Table string
	Rows  []row
}

// row is a column->value map. Nil values become SQL NULL; bool becomes 0/1;
// time.Time and []byte are passed through to the driver.
type row map[string]any

// Spec returns the ordered fixture set for every hub table. Parent rows
// (projects, users, ...) are listed before the rows that reference them.
func Spec() []TableFixture {
	return []TableFixture{
		// ---- Core identities referenced elsewhere ----
		{Table: "projects", Rows: []row{
			{ // full row with nested/unicode JSON labels
				"id": projectID, "name": "Platform", "slug": "platform",
				"git_remote": "https://github.com/example/platform.git",
				"labels":     unicodeJSON, "annotations": `{"note":"primary"}`,
				"created_at": baseTime, "updated_at": baseTime,
				"owner_id": userID, "visibility": "private",
			},
			{ // minimal row: nullable optionals (git_remote, labels, owner...) left NULL
				"id": "11111111-1111-1111-1111-1111111111aa", "name": "Minimal Project",
				"slug": "minimal-project",
			},
		}},
		{Table: "users", Rows: []row{
			{
				"id": userID, "email": "alice@example.com", "display_name": "Alice",
				"role": "admin", "status": "active",
				"preferences": `{"theme":"dark"}`, "created_at": baseTime,
			},
			{ // max-length display_name edge case + NULL avatar_url
				"id":    "22222222-2222-2222-2222-2222222222aa",
				"email": "long@example.com", "display_name": maxLenString,
			},
		}},
		{Table: "runtime_brokers", Rows: []row{
			{
				"id": brokerID, "name": "broker-1", "slug": "broker-1", "type": "docker",
				"status": "online", "created_at": baseTime, "updated_at": baseTime,
				"capabilities": `{"webPty":true,"sync":true,"attach":false}`,
			},
		}},
		{Table: "agents", Rows: []row{
			{ // live agent with nested/unicode JSON
				"id": agentID, "agent_id": agentID, "name": "worker", "template": "claude",
				"project_id": projectID, "labels": unicodeJSON,
				"applied_config": nestedConfigJSON,
				"created_at":     baseTime, "updated_at": baseTime,
				"phase": "running", "visibility": "private", "state_version": 1,
			},
			{ // soft-deleted agent (deleted_at populated)
				"id": "33333333-3333-3333-3333-3333333333aa", "agent_id": "33333333-3333-3333-3333-3333333333aa",
				"name": "deleted-worker", "template": "claude", "project_id": projectID,
				"created_at": baseTime, "updated_at": baseTime, "deleted_at": baseTime,
				"phase": "stopped", "visibility": "private", "state_version": 2,
			},
		}},

		// ---- Permissions ----
		{Table: "groups", Rows: []row{
			{
				"id": groupID, "name": "Engineering", "slug": "engineering",
				"description": "Eng team", "labels": unicodeJSON,
				"created_at": baseTime, "updated_at": baseTime,
				"group_type": "explicit",
			},
		}},
		{Table: "group_members", Rows: []row{
			{"group_id": groupID, "member_type": "user", "member_id": userID, "role": "owner", "added_at": baseTime},
			{"group_id": groupID, "member_type": "agent", "member_id": agentID, "role": "member", "added_at": baseTime},
		}},
		{Table: "policies", Rows: []row{
			{
				"id": policyID, "name": "Allow Read", "description": "read agents",
				"scope_type": "hub", "resource_type": "agent",
				"actions": `["read","list"]`, "effect": "allow", "priority": 10,
				"conditions": unicodeJSON,
				"created_at": baseTime, "updated_at": baseTime,
			},
		}},
		{Table: "policy_bindings", Rows: []row{
			{"policy_id": policyID, "principal_type": "user", "principal_id": userID},
			{"policy_id": policyID, "principal_type": "group", "principal_id": groupID},
		}},

		// ---- Config / scoped values ----
		{Table: "env_vars", Rows: []row{
			{
				"id": "e0000000-0000-0000-0000-000000000001", "key": "LOG_LEVEL", "value": "debug",
				"scope": "project", "scope_id": projectID, "created_at": baseTime, "updated_at": baseTime,
			},
		}},
		{Table: "secrets", Rows: []row{
			{ // long encrypted_value exercises large TEXT
				"id": "5ec00000-0000-0000-0000-000000000001", "key": "API_KEY",
				"encrypted_value": maxLenString, "scope": "project", "scope_id": projectID,
				"secret_type": "environment", "created_at": baseTime, "updated_at": baseTime,
			},
		}},
		{Table: "templates", Rows: []row{
			{
				"id": "7e000000-0000-0000-0000-000000000001", "name": "claude", "slug": "claude",
				"harness": "claude", "image": "scion/claude:latest", "config": nestedConfigJSON,
				"scope": "global", "status": "active", "visibility": "public",
				"created_at": baseTime, "updated_at": baseTime,
			},
		}},
		{Table: "harness_configs", Rows: []row{
			{
				"id": "4a000000-0000-0000-0000-000000000001", "name": "claude-web", "slug": "claude-web",
				"harness": "claude", "config": nestedConfigJSON, "scope": "global",
				"status": "active", "visibility": "public", "created_at": baseTime, "updated_at": baseTime,
			},
		}},

		// ---- Brokers / project wiring ----
		{Table: "project_contributors", Rows: []row{
			{
				"project_id": projectID, "broker_id": brokerID, "broker_name": "broker-1",
				"mode": "connected", "status": "online", "last_seen": baseTime,
			},
		}},
		{Table: "project_sync_state", Rows: []row{
			{
				"project_id": projectID, "broker_id": brokerID, "last_sync_time": baseTime,
				"last_commit_sha": "deadbeefcafe", "file_count": 42, "total_bytes": 123456,
			},
		}},
		{Table: "broker_secrets", Rows: []row{
			{ // BLOB column
				"broker_id": brokerID, "secret_key": []byte{0x01, 0x02, 0x03, 0x04, 0xfe, 0xff},
				"algorithm": "hmac-sha256", "created_at": baseTime, "status": "active",
			},
		}},
		{Table: "broker_join_tokens", Rows: []row{
			{
				"broker_id": brokerID, "token_hash": "abc123hash", "expires_at": baseTime.Add(time.Hour),
				"created_at": baseTime, "created_by": userID,
			},
		}},

		// ---- Notifications / messaging ----
		{Table: "notification_subscriptions", Rows: []row{
			{
				"id": subID, "scope": "agent", "agent_id": agentID, "subscriber_type": "user",
				"subscriber_id": userID, "project_id": projectID,
				"trigger_activities": `["COMPLETED","WAITING_FOR_INPUT"]`,
				"created_at":         baseTime, "created_by": userID,
			},
		}},
		{Table: "notifications", Rows: []row{
			{
				"id": "0t000000-0000-0000-0000-000000000001", "subscription_id": subID,
				"agent_id": agentID, "project_id": projectID, "subscriber_type": "user",
				"subscriber_id": userID, "status": "COMPLETED", "message": "agent completed 🎉",
				"created_at": baseTime,
			},
		}},
		{Table: "subscription_templates", Rows: []row{
			{
				"id": "57000000-0000-0000-0000-000000000001", "name": "All Events",
				"scope": "project", "trigger_activities": `["COMPLETED","ERROR"]`,
				"project_id": projectID, "created_by": userID,
			},
		}},
		{Table: "messages", Rows: []row{
			{
				"id": "11500000-0000-0000-0000-000000000001", "project_id": projectID,
				"sender": "user:alice", "sender_id": userID, "recipient": "agent:worker",
				"recipient_id": agentID, "msg": "do the thing — café ☕", "type": "instruction",
				"agent_id": agentID, "created_at": baseTime,
			},
		}},

		// ---- Schedules ----
		{Table: "schedules", Rows: []row{
			{
				"id": "5c000000-0000-0000-0000-000000000001", "project_id": projectID,
				"name": "nightly", "cron_expr": "0 0 * * *", "event_type": "dispatch_agent",
				"payload": nestedConfigJSON, "status": "active", "next_run_at": baseTime.Add(24 * time.Hour),
				"created_at": baseTime, "updated_at": baseTime,
			},
		}},
		{Table: "scheduled_events", Rows: []row{
			{
				"id": "5e000000-0000-0000-0000-000000000001", "project_id": projectID,
				"event_type": "dispatch_agent", "fire_at": baseTime.Add(time.Hour),
				"payload": `{"task":"run"}`, "status": "pending", "created_at": baseTime,
			},
		}},

		// ---- Access control: allow list / invites / tokens ----
		{Table: "allow_list", Rows: []row{
			{
				"id": "a1000000-0000-0000-0000-000000000001", "email": "invited@example.com",
				"note": "early access", "added_by": userID, "created": baseTime,
			},
		}},
		{Table: "invite_codes", Rows: []row{
			{
				"id": "1c000000-0000-0000-0000-000000000001", "code_hash": "hash_of_code",
				"code_prefix": "scion_in", "max_uses": 5, "use_count": 1,
				"expires_at": baseTime.Add(48 * time.Hour), "created_by": userID, "created": baseTime,
			},
		}},
		{Table: "user_access_tokens", Rows: []row{
			{
				"id": "0a000000-0000-0000-0000-000000000001", "user_id": userID, "name": "ci-token",
				"prefix": "scion_pat_ab", "key_hash": "tokenhash", "project_id": projectID,
				"scopes": `["agent:read","agent:list"]`, "created_at": baseTime,
			},
		}},
		{Table: "api_keys", Rows: []row{
			{ // NULL expires_at / last_used optionals
				"id": "a9000000-0000-0000-0000-000000000001", "user_id": userID, "name": "legacy-key",
				"prefix": "scion_ak", "key_hash": "apikeyhash", "scopes": `["read"]`, "created_at": baseTime,
			},
		}},

		// ---- GCP / GitHub identity ----
		{Table: "gcp_service_accounts", Rows: []row{
			{
				"id": "9c000000-0000-0000-0000-000000000001", "scope": "project", "scope_id": projectID,
				"email": "agent-worker@example.iam.gserviceaccount.com", "project_id": "gcp-proj-123",
				"display_name": "Worker SA", "default_scopes": `["https://www.googleapis.com/auth/cloud-platform"]`,
				"created_by": userID, "created_at": baseTime, "managed": true,
			},
		}},
		{Table: "github_installations", Rows: []row{
			{
				"installation_id": int64(987654), "account_login": "example-org",
				"account_type": "Organization", "app_id": int64(112233),
				"repositories": `["example/platform","example/infra"]`, "status": "active",
				"created_at": baseTime, "updated_at": baseTime,
			},
		}},

		// ---- Maintenance ----
		{Table: "maintenance_operations", Rows: []row{
			{
				"id": "0d000000-0000-0000-0000-000000000001", "key": "purge_deleted_agents",
				"title": "Purge Deleted Agents", "description": "remove soft-deleted agents",
				"category": "cleanup", "status": "pending", "created_at": baseTime,
				"metadata": `{"batchSize":100}`,
			},
		}},
		{Table: "maintenance_operation_runs", Rows: []row{
			{
				"id": "07000000-0000-0000-0000-000000000001", "operation_key": "purge_deleted_agents",
				"status": "completed", "started_at": baseTime, "completed_at": baseTime.Add(time.Minute),
				"started_by": userID, "result": `{"purged":3}`, "log": "done",
			},
		}},
	}
}
