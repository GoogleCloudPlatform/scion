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
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// failingUATLookupStore wraps a real store.UserAccessTokenStore and forces
// GetUserAccessTokenByHash to fail, simulating a store/database outage
// during UAT lookup rather than a client presenting a bad credential.
type failingUATLookupStore struct {
	store.UserAccessTokenStore
	err error
}

func (f *failingUATLookupStore) GetUserAccessTokenByHash(context.Context, string) (*store.UserAccessToken, error) {
	return nil, f.err
}

// TestUATRejection_LookupFailureIsNotLoggedAsInvalid proves a store/database
// failure during token lookup is never logged as reason="invalid"
// (indistinguishable from a client presenting a bad token) — an operator
// must be able to tell an outage from a wave of bad tokens.
func TestUATRejection_LookupFailureIsNotLoggedAsInvalid(t *testing.T) {
	srv, _ := testServer(t)
	capture := &capturingHandler{}
	srv.authConfig.Logger = slog.New(capture)

	srv.uatService.tokens = &failingUATLookupStore{
		UserAccessTokenStore: srv.uatService.tokens,
		err:                  errors.New("db unavailable"),
	}

	rr := doRequestWithBearer(srv, "scion_pat_whatever-looks-like-a-token")
	require.Equal(t, http.StatusUnauthorized, rr.Code)

	rec, ok := findRecord(capture.all(), "credential rejected")
	require.True(t, ok)
	attrs := recordAttrs(rec)
	require.Equal(t, "lookup_error", attrs["reason"], "a store failure must not be reported as an ordinary invalid-token rejection")
	require.Equal(t, slog.LevelError, rec.Level, "a lookup failure is an operational error, not a client-side rejection")
}
