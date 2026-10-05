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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3335: `scion start --service-account <sa>` gives one response
// whether the named account is unregistered or registered but not usable by
// the caller, and its wording covers both causes.

// TestAgentStart_UnusableAndUnregisteredSA_SameStatusAndBody names a
// registered service account the caller may not use from this project, and
// an unregistered one, on the agent create (start) path. The two responses
// must match byte for byte, status included.
func TestAgentStart_UnusableAndUnregisteredSA_SameStatusAndBody(t *testing.T) {
	f := bypassAgentsSetup(t)
	// Registered and verified, but scoped to another project, so this
	// caller may not use it here.
	registered := bypassAgentsCreateSA(t, f, f.other.ID, true)
	got, err := f.store.GetGCPServiceAccount(t.Context(), registered.ID)
	require.NoError(t, err, "fixture must persist the registered account")
	require.Equal(t, registered.ID, got.ID)

	start := func(name, saID string) oracleProbe {
		return probe(createAgentAsOwner(t, f, CreateAgentRequest{
			Name: name,
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: saID,
			},
		}))
	}

	unusable := start("sa-uniform-registered", registered.ID)
	unregistered := start("sa-uniform-unregistered", uuid.New().String())

	require.Equal(t, http.StatusBadRequest, unregistered.status, "body: %s", unregistered.body)
	assert.Equal(t, unregistered.status, unusable.status, "status codes must match")
	assert.Equal(t, unregistered.body, unusable.body, "response bodies must match byte for byte")
}

// TestAgentStart_SAUnavailableMessageNamesBothCauses pins the exact text and
// that it reaches the response unchanged as a validation_error.
func TestAgentStart_SAUnavailableMessageNamesBothCauses(t *testing.T) {
	assert.Equal(t,
		"Service account not available: it is not registered in this project, or you are not authorized to use it.",
		msgSANotAvailableInProject)

	f := bypassAgentsSetup(t)
	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name: "sa-uniform-text",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: uuid.New().String(),
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeValidationError, resp.Error.Code)
	assert.Equal(t, msgSANotAvailableInProject, resp.Error.Message)
}
