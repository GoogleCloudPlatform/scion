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
	sel := newDeliverySelector(m.d.authzService, m.agent, PurposeRestart)
	_, err = m.d.selectDeliverySecrets(ended, sel, m.agent)
	require.NoError(t, err, "the selection reaches its summary point")
	assert.Positive(t, sel.denied, "the item was denied; only the Warns are suppressed")
	assert.Empty(t, warnRecords(t, buf.Bytes(), itemMsg), "no per-item Warn after the request ended")
	assert.Empty(t, warnRecords(t, buf.Bytes(), summaryMsg), "no summary Warn after the request ended")
}

// TestDispatchDelivery_LegacyFallbackWarnQuietAfterRequestEnds: an item
// delivered through the legacy fallback under an ended request logs
// nothing and does not use the Warn's rate-limit slot, so the next live
// delivery within the window is still logged.
func TestDispatchDelivery_LegacyFallbackWarnQuietAfterRequestEnds(t *testing.T) {
	m := newLDFixture(t, "lg-ended", ldOptions{noEdge: true})
	m.env(t, "LG_ENDED_ENV", "v", store.ScopeProject, m.agent.ProjectID, false)
	buf := lgLogger(m)

	ended := endedCtx{context.Background()}
	env, _, err := m.d.selectDeliveryEnv(ended, newDeliverySelector(m.d.authzService, m.agent, PurposeRestart), m.agent)
	require.NoError(t, err)
	assert.Equal(t, "v", env["LG_ENDED_ENV"], "the ended run reached the legacy fallback and delivered the item")
	assert.Empty(t, warnRecords(t, buf.Bytes(), lgFallbackMsg), "no legacy Warn after the request ended")

	require.NoError(t, m.d.DispatchAgentRestart(context.Background(), m.agent))
	assert.Equal(t, "v", m.client.lastRestartResolvedEnv["LG_ENDED_ENV"])
	assert.Len(t, warnRecords(t, buf.Bytes(), lgFallbackMsg), 1, "the live delivery is still logged within the window")
}

// TestProvisionCredentials_RefusalWarnQuietAfterRequestEnds: the provision
// refusal summary Warn is logged for a live request and not once the
// request has ended.
func TestProvisionCredentials_RefusalWarnQuietAfterRequestEnds(t *testing.T) {
	const msg = "ProvisionCredentials not authorized for delivery to the agent; not sent"
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		wantW bool
	}{
		{"live", context.Background(), true},
		{"ended", endedCtx{context.Background()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, rec, buf := newProvisionCredsFixture(t, "pc-ended-"+tc.name, ldOptions{edge: withBoundedCeiling("agent.read")})
			m.secret(t, "GH_ACME", "project-value", store.SecretTypeEnvironment, store.ScopeProject, m.agent.ProjectID)

			assert.Nil(t, m.d.resolveProvisionCredentials(tc.ctx, m.agent, "DispatchAgentStart"))
			assert.Empty(t, fetchedNames(rec), "a refused secret is never fetched")
			warn := findProvisionCredsRecord(buf.records(t), "WARN", msg)
			if tc.wantW {
				require.NotNil(t, warn, "a live request logs the refusal; log: %s", buf.String())
			} else {
				assert.Nil(t, warn, "no refusal Warn after the request ended; log: %s", buf.String())
			}
		})
	}
}
