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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// orphanBrokerCredentialSweepBudget bounds the startup sweep of broker
// credentials that have no broker row.
const orphanBrokerCredentialSweepBudget = 30 * time.Second

// sweepOrphanBrokerCredentials removes, once at hub startup, every broker
// secret and join token whose broker ID has no runtime broker row. Deleting
// a broker removes its credentials in the same transaction, but an earlier
// deletion may have left some behind; without this sweep a broker row later
// re-created with the same ID would start with that credential.
//
// It runs synchronously in New, before the server (and the startup code
// that creates the co-located and plugin broker rows) uses the store, so it
// sees the credentials as they were before this start.
//
// A failure is logged and does not stop the hub: a credential with no row
// is already refused by broker authentication, and the sweep runs again on
// the next start. Only counts are logged, never key material.
func sweepOrphanBrokerCredentials(ctx context.Context, s store.Store) (secrets, joinTokens int, err error) {
	ctx, cancel := context.WithTimeout(ctx, orphanBrokerCredentialSweepBudget)
	defer cancel()
	secrets, joinTokens, err = s.DeleteOrphanBrokerCredentials(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to sweep broker credentials that have no broker row; they stay refused and the sweep runs again on the next start",
			"error", err)
		return 0, 0, err
	}
	if secrets > 0 || joinTokens > 0 {
		slog.InfoContext(ctx, "Swept broker credentials that have no broker row",
			"secrets_removed", secrets, "join_tokens_removed", joinTokens)
	}
	return secrets, joinTokens, nil
}
