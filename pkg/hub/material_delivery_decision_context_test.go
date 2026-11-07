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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// decisionContextTestKey is a request-scoped value the decision context
// must keep (the standing memo and the stopped note travel the same way).
type decisionContextTestKey struct{}

// TestDeliverySelector_DecisionContextScope: only a start or restart
// decides on a context detached from the request's cancellation, with its
// own bound; every other purpose (the request-scoped availability probe
// among them) keeps the request context, so the request-end refusal
// still applies to it. Every decision context keeps the request's values.
func TestDeliverySelector_DecisionContextScope(t *testing.T) {
	const want = "request-value"
	ended := endedCtx{context.WithValue(context.Background(), decisionContextTestKey{}, want)}
	for _, tc := range []struct {
		purpose  MaterialPurpose
		detached bool
	}{
		{PurposeDispatch, true},
		{PurposeRestart, true},
		{PurposeAvailabilityProbe, false},
		{PurposeProvisionCredential, false},
		{PurposeRuntimeRead, false},
	} {
		t.Run(string(tc.purpose), func(t *testing.T) {
			s := &deliverySelector{purpose: tc.purpose}
			dctx, cancel := s.decisionContext(ended)
			defer cancel()
			assert.Equal(t, want, dctx.Value(decisionContextTestKey{}), "the decision context keeps the request's values")
			_, hasDeadline := dctx.Deadline()
			if tc.detached {
				assert.NoError(t, dctx.Err(), "the decision context is not ended with the request")
				assert.True(t, hasDeadline, "the detached decision has its own bound")
			} else {
				assert.ErrorIs(t, dctx.Err(), context.Canceled, "the request context is kept")
				assert.False(t, hasDeadline)
			}
		})
	}
}
