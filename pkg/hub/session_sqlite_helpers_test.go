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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realTokenContext authenticates a minted token key the way the auth
// middleware does and returns a request context carrying its identity,
// credential and auth type.
func realTokenContext(t *testing.T, srv *Server, key string) context.Context {
	t.Helper()
	scoped, err := srv.uatService.ValidateToken(context.Background(), key)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), userContextKey{}, scoped)
	ctx = contextWithIdentity(ctx, scoped)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(scoped))
	return contextWithAuthType(ctx, AuthTypeUAT)
}

// requestWithContext builds a request for a direct handler call.
func requestWithContext(ctx context.Context, method, path string, body interface{}) *http.Request {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func requireSessionOnlyRefusal(t *testing.T, rec *httptest.ResponseRecorder, want authzop.SessionOnlyReason, label string) {
	t.Helper()
	reason, credential := sessionOnlyDetailsOf(rec)
	assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
	assert.Equal(t, string(want), reason, "%s: details.reason: %s", label, rec.Body.String())
	assert.Equal(t, sessionRequiredCredential, credential, "%s: details.credential: %s", label, rec.Body.String())
}
