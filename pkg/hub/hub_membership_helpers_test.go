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

package hub

import (
	"context"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ensureHubMembership adds the given user to the hub-members group.
// This is best-effort; errors are logged at debug level and ignored.
//
// Test-only convenience: production code grants hub-members through
// syncHubRoleGrants (or ensureHubMembershipTx directly).
func ensureHubMembership(ctx context.Context, s store.Store, userID string) {
	if err := ensureHubMembershipTx(ctx, s, userID); err != nil {
		slog.Debug("failed to ensure hub-members group membership", "userID", userID, "error", err)
	}
}
