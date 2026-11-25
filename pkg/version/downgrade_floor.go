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

package version

// DowngradeFloor is the oldest release this binary lets the hub install.
// The running binary refuses older targets (below_downgrade_floor), so the
// check works without older binaries knowing about it.
//
// Bump it, in the same PR, to the first release that contains any schema
// change an older binary cannot ignore: a dropped table or column (see the
// drop block in pkg/ent/entc/client.go AutoMigrate), a new NOT NULL column
// without a default, a narrowed enum, or rewritten values. Releases older
// than the floor would run on a schema they do not understand.
//
// The value must be a nightly-YYYYMMDD release tag. A nightly tag carries its
// midnight UTC date, which update.BelowFloor uses to order cross-channel
// targets (preview and stable releases) by their publish date; a non-nightly
// floor has no embedded date, so cross-channel comparisons return unknown and
// fail closed.
//
// Set it to the first nightly that will contain the migration (nightlies cut
// at ~03:00 UTC) and never later than the earliest preview or stable release
// that contains it. Setting the floor a day late causes that nightly and any
// preview or stable release published on the same day to sort below the floor
// and be refused as downgrade targets (below_downgrade_floor).
// Current floor (nightly-20261126):
//   - scheduled_exec_runs makes event_id nullable and adds fire_id plus the
//     scheduled_exec_runs_single_owner CHECK constraint (Stage 2 P8,
//     ptone/scion#8241); pre-P8 binaries fail AutoMigrate on Postgres once any
//     trigger-owned run row exists (SQLSTATE 23502 on SET NOT NULL) and on
//     SQLite table rebuild (SQLITE_CONSTRAINT_NOTNULL 1299).
//   - Retiring the "variable" secret type and introducing "on_demand"
//     (ptone/scion#9160): existing variable rows are rewritten to internal or
//     on_demand or deleted, and the secret_type enums are narrowed; older
//     binaries use a denylist delivery path that would fetch and send on_demand
//     values to brokers, reject on_demand in validators, and return the stored
//     target in runtime-read responses.
//   - agents.message_mode rewrites retired values ("lineage" -> "owner",
//     "branch" -> "direct") and adds "tree" (ptone/scion#9158); older binaries
//     reject the new enum values in Ent MessageModeValidator and deny unknown
//     modes in message authorization.
//   - Ordered actions[] per trigger (Stage 2 P9, ptone/scion#9065): loosens
//     trigger_fires uniqueness from (event_id, trigger_id) to
//     (event_id, trigger_id, action_index) and trigger_subject_claims
//     uniqueness from (trigger_id, subject) to (trigger_id, action_index, subject),
//     dropping the legacy unique indexes in AutoMigrate; older binaries
//     re-tighten those unique indexes on AutoMigrate (failing once multi-action
//     rows exist) and cannot read triggers stored in triggers.actions with empty
//     legacy action_type.
const DowngradeFloor = "nightly-20261126"
