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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Which purge dispatch results settle a record (ptone/scion#4744). A broker
// that has no files for the agent (its soft delete removed them, or they
// were never there) answers 404, which DispatchAgentDelete returns as nil:
// settled, with nothing removed.
func TestClassifyRetainedPurgeResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		want   retainedPurgeOutcome
		reason string
	}{
		{name: "removed, or nothing there (404)", err: nil, want: retainedPurgeRemove},
		{name: "another run holds the name", err: &DeleteRunMismatchError{RequestedRunID: "a", CurrentRunID: "b"}, want: retainedPurgeRemove, reason: "another run"},
		{name: "refused", err: &brokerStatusError{StatusCode: http.StatusConflict,
			Body: `{"error":{"code":"` + api.BrokerErrCodeRetainedFilesRefused + `","details":{"reason":"a start of the agent is in flight"}}}`},
			want: retainedPurgeRemove, reason: "a start of the agent is in flight"},
		{name: "invalid request", err: &brokerStatusError{StatusCode: http.StatusBadRequest, Body: `{"error":{"code":"validation_error"}}`}, want: retainedPurgeRemove, reason: "invalid"},
		{name: "other conflict", err: &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"stale_dispatch"}}`}, want: retainedPurgeKeep},
		{name: "server error", err: &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: `{}`}, want: retainedPurgeKeep},
		{name: "unreachable", err: fmt.Errorf("failed to send request: %w", errors.New("connection refused")), want: retainedPurgeKeep},
		{name: "timeout", err: context.DeadlineExceeded, want: retainedPurgeKeep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyRetainedPurgeResult(tc.err)
			assert.Equal(t, tc.want, got)
			if tc.reason != "" {
				assert.Contains(t, reason, tc.reason)
			}
		})
	}
}

// The purge dispatch is a delete with purgeRetained=true, scoped to the
// agent's project; other deletes never carry it.
func TestDeleteAgentQuery_PurgeRetained(t *testing.T) {
	q := deleteAgentQuery(context.Background(), "proj-1", DeleteAgentOptions{DeleteFiles: true, PurgeRetained: true})
	assert.Contains(t, q, "&"+api.PurgeRetainedQueryParam+"=true")
	assert.Contains(t, q, "projectId=proj-1")
	assert.NotContains(t, q, "softDelete")

	plain := deleteAgentQuery(context.Background(), "proj-1", DeleteAgentOptions{DeleteFiles: true})
	assert.False(t, strings.Contains(plain, api.PurgeRetainedQueryParam))
}

// A purge dispatch deferred to another hub node keeps its purgeRetained mark.
func TestDeleteDispatchArgs_PurgeRetainedRoundTrip(t *testing.T) {
	raw, err := MarshalDispatchArgs(&DeleteDispatchArgs{DeleteFiles: true, PurgeRetained: true})
	require.NoError(t, err)
	got, err := UnmarshalDeleteArgs(raw)
	require.NoError(t, err)
	assert.True(t, got.PurgeRetained)

	ctx := withPurgeRetained(context.Background())
	assert.True(t, purgeRetainedFrom(ctx))
	assert.False(t, purgeRetainedFrom(context.Background()))
}
