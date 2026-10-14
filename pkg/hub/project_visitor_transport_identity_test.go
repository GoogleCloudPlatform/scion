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
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A project visitor grant never creates a pending account for a transport
// identity (ptone/scion#4240, WS2): a proxy driver principal (also given in
// mixed case with surrounding spaces) and the platform service account each
// get 400 validation_error with details.field email and the generic message,
// and no federated account, visitor binding or grant audit is written.
// Another email under the same issuer is still granted.
func TestProjectVisitors_GrantRefusesTransportIdentities(t *testing.T) {
	const (
		driver   = "web-driver@visitor-proj.iam.gserviceaccount.com"
		platform = "platform-sa@visitor-proj.iam.gserviceaccount.com"
	)
	f := newVisitorGrantFixture(t, func(srv *Server) {
		srv.proxyDrivers = newDriverPrincipalSet([]string{driver})
		srv.platformAuthSA = platform
	})
	ctx := context.Background()
	issuer := store.NormalizeFederatedIssuer(grantTestOtherIssuer)

	for name, email := range map[string]string{
		"driver principal":                        driver,
		"driver principal, mixed case and spaces": "  Web-Driver@Visitor-Proj.IAM.gserviceaccount.com ",
		"platform service account":                platform,
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestOtherIssuer, "email": email})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, proxyDriverPrincipalUserMessage, resp.Error.Message)
			assert.Equal(t, ErrCodeValidationError, resp.Error.Code)
			assert.Equal(t, "email", resp.Error.Details["field"])
			assert.NotContains(t, rec.Body.String(), "driver", "the answer does not name drivers")

			_, err := f.st.GetFederatedUserByEmail(ctx, issuer, store.NormalizeFederatedEmail(email))
			assert.True(t, errors.Is(err, store.ErrNotFound), "no federated account for %s (err %v)", email, err)
		})
	}
	assert.Empty(t, f.list(t), "no visitor binding")
	assert.Empty(t, f.audits(t, mutationTypeProjectVisitorGrant), "no grant audit")

	v := f.grantOK(t, grantTestOtherIssuer, "carol@partner.example")
	assert.Equal(t, store.UserStatusPending, v.Status)
	assert.Len(t, f.list(t), 1)
}
