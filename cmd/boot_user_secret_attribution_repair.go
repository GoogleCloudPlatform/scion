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

package cmd

import (
	"context"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runUserSecretAttributionRepair moves created_by on user-scope secrets
// captured by an agent before ptone/scion#1557 from "agent:<uuid>" to the
// scope owner, where that is provably the owner's attribution. Without the
// repair the user secret parent ceiling withholds those rows at start,
// restart and runtime read from the owner's agents with a recorded
// delegation chain. See hub.RepairLegacyUserSecretAttribution.
//
// M-1' semantics: per-row skips are logged at WARN, counted as residuals
// and do not block the marker, except row_changed: a row that kept changing
// under the repair is not a final outcome, so the marker stays unwritten
// and the next boot retries. A store error aborts the pass and also leaves
// the marker unwritten. Only rows still attributed to an agent are
// touched, so a rerun is safe. Secret values are never read or logged.
func runUserSecretAttributionRepair(ctx context.Context, s store.Store) {
	done, err := IsMigrationComplete(ctx, s, MigrationUserSecretAttributionRepair)
	if err != nil {
		slog.ErrorContext(ctx, "User secret attribution repair: failed to check completion marker; will attempt migration",
			"error", err)
	} else if done {
		slog.DebugContext(ctx, "User secret attribution repair: already complete, skipping")
		return
	}

	res, err := hub.RepairLegacyUserSecretAttribution(ctx, s)
	for _, r := range res.Repaired {
		slog.InfoContext(ctx, "User secret attribution repair: attributed secret to its owner",
			"secret", r.Name, "secret_id", r.SecretID, "user_id", r.ScopeID,
			"original_created_by", r.OriginalCreatedBy, "basis", r.Basis)
	}
	for _, skip := range res.Skipped {
		slog.WarnContext(ctx, "User secret attribution repair: secret left attributed to an agent; the owner's agents with a recorded delegation chain do not receive it",
			"secret", skip.Name, "secret_id", skip.SecretID, "user_id", skip.ScopeID,
			"created_by", skip.CreatedBy, "reason", skip.Reason)
	}
	if err != nil {
		slog.ErrorContext(ctx, "User secret attribution repair: pass did not complete; will retry next boot",
			"repaired", len(res.Repaired), "error", err)
		return
	}
	slog.InfoContext(ctx, "User secret attribution repair: pass completed",
		"examined", res.Examined, "repaired", len(res.Repaired), "skipped", len(res.Skipped), "vanished", res.Vanished,
		"author_changed", res.AuthorChanged)
	if n := res.RowChanged(); n > 0 {
		slog.WarnContext(ctx, "User secret attribution repair: rows changed during the repair; marker not written, will retry next boot",
			"row_changed", n)
		return
	}

	if markErr := MarkMigrationComplete(ctx, s, MigrationUserSecretAttributionRepair, len(res.Skipped)); markErr != nil {
		slog.ErrorContext(ctx, "User secret attribution repair: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}
