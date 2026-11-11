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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGovernanceDenial_CodeMapping pins the status-to-code mapping of the
// status+reason in-transaction refusals (ptone/scion#2695).
func TestGovernanceDenial_CodeMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusForbidden, ErrCodeRoleAssignmentForbidden},
		{http.StatusNotFound, "not_found"},
		{http.StatusConflict, "conflict"},
	} {
		err := governanceDenial(tc.status, "reason: with a colon")
		var gdErr *governanceDenialError
		require.True(t, errors.As(err, &gdErr), "status %d", tc.status)
		assert.Equal(t, MembershipDecision{
			Allowed:    false,
			DenialCode: tc.code,
			Reason:     "reason: with a colon",
			HTTPStatus: tc.status,
		}, gdErr.decision)
		assert.Equal(t, "reason: with a colon", err.Error())
	}
}

// TestGovernanceDenial_SurvivesWithTxAndWrapping shows that a typed denial
// returned from a WithTx closure survives the transaction (the real store's
// WithTx) and any %w wrapping, and that errors.As recovers the exact decision.
// The end-to-end deletion path is pinned by TestDeleteProjectRefusalBody_*.
func TestGovernanceDenial_SurvivesWithTxAndWrapping(t *testing.T) {
	_, s := testServer(t)
	want := MembershipDecision{
		Allowed:    false,
		DenialCode: ErrCodeProjectDeleteForbidden,
		Reason:     "actor is not a direct project owner (re-evaluated under lock)",
		HTTPStatus: http.StatusForbidden,
	}

	txErr := s.WithTx(context.Background(), func(tx store.Store) error {
		return asGovernanceDenial(want)
	})
	require.Error(t, txErr)

	for name, err := range map[string]error{
		"tx":             txErr,
		"wrapped once":   fmt.Errorf("delete project: %w", txErr),
		"wrapped twice":  fmt.Errorf("outer: %w", fmt.Errorf("lock project for deletion: %w", txErr)),
		"errors.Join":    errors.Join(errors.New("other"), txErr),
		"wrapped + join": fmt.Errorf("outer: %w", errors.Join(txErr, errors.New("other"))),
	} {
		var gdErr *governanceDenialError
		require.True(t, errors.As(err, &gdErr), name)
		assert.Equal(t, want, gdErr.decision, name)
	}
}
