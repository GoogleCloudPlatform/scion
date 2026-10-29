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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// endedCtx reports the request as ended (Err is context.Canceled) while its
// Done channel never closes, so store reads proceed and only the code that
// consults ctx.Err() sees the cancellation.
type endedCtx struct{ context.Context }

func (endedCtx) Err() error { return context.Canceled }

// TestDispatchDelivery_WithheldWarnsQuietAfterRequestEnds: the per-item and
// summary "launch material withheld" Warns are logged for a live request
// and not once the request has ended (cancelled or expired ctx).
func TestDispatchDelivery_WithheldWarnsQuietAfterRequestEnds(t *testing.T) {
	m := newLDFixture(t, "warn-ended", ldOptions{edge: withBoundedCeiling("agent.read")})
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m.authz.logger = logger
	m.d.log = logger
	m.secret(t, "MD_ENDED_SECRET", "ended-secret-value", store.SecretTypeEnvironment, store.ScopeProject, m.scopeID(store.ScopeProject))

	const itemMsg = "launch material withheld: not authorized for delivery"
	const summaryMsg = "launch material withheld: items not authorized for delivery"

	// Live request: both Warns.
	_, err := m.d.selectDeliverySecrets(context.Background(), newDeliverySelector(m.d.authzService, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err)
	require.NotEmpty(t, warnRecords(t, buf.Bytes(), itemMsg), "a live request logs the withheld item; log: %s", buf.String())
	require.Len(t, warnRecords(t, buf.Bytes(), summaryMsg), 1)

	// Ended request: the selection runs to its end and logs neither.
	buf.Reset()
	ended := endedCtx{context.Background()}
	_, err = m.d.selectDeliverySecrets(ended, newDeliverySelector(m.d.authzService, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err, "the selection reaches its summary point")
	assert.Empty(t, warnRecords(t, buf.Bytes(), itemMsg), "no per-item Warn after the request ended")
	assert.Empty(t, warnRecords(t, buf.Bytes(), summaryMsg), "no summary Warn after the request ended")
}
