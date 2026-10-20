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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestHostSAClaimWriteAllowed(t *testing.T) {
	a := hostSAClaim{Email: "a@p1.iam.gserviceaccount.com", ProjectID: "p1"}
	b := hostSAClaim{Email: "b@p1.iam.gserviceaccount.com", ProjectID: "p1"}
	aOtherProject := hostSAClaim{Email: a.Email, ProjectID: "p2"}
	cleared := hostSAClaim{}

	tests := []struct {
		name        string
		prior, next hostSAClaim
		verdict     hostSAClaimVerdict
		wantErr     bool
	}{
		{"unchanged, zero verdict", a, a, hostSAClaimVerdict{}, false},
		{"unchanged empty, zero verdict", cleared, cleared, hostSAClaimVerdict{}, false},
		{"cleared, zero verdict", a, cleared, hostSAClaimVerdict{}, false},
		{"cleared email with project left, zero verdict", a, hostSAClaim{ProjectID: "p1"}, hostSAClaimVerdict{}, false},
		{"changed, allowed for next", a, b, hostSAClaimVerdict{Claim: b, Allowed: true}, false},
		{"set from empty, allowed for next", cleared, a, hostSAClaimVerdict{Claim: a, Allowed: true}, false},
		{"changed, denied", a, b, hostSAClaimVerdict{Claim: b}, true},
		{"changed, zero verdict", a, b, hostSAClaimVerdict{}, true},
		{"changed, verdict for a different claim", cleared, b, hostSAClaimVerdict{Claim: a, Allowed: true}, true},
		{"project-only change, verdict for old pair", a, aOtherProject, hostSAClaimVerdict{Claim: a, Allowed: true}, true},
		{"project-only change, allowed for new pair", a, aOtherProject, hostSAClaimVerdict{Claim: aOtherProject, Allowed: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := hostSAClaimWriteAllowed(tc.prior, tc.next, tc.verdict)
			if tc.wantErr {
				assert.True(t, errors.Is(err, ErrHostSAClaimNotAuthorized), "got %v", err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestHostSAClaimOf_LowercasesEmail(t *testing.T) {
	assert.Equal(t, hostSAClaim{}, hostSAClaimOf(nil))
	got := hostSAClaimOf(&store.RuntimeBroker{
		GCPHostServiceAccountEmail: "Host@My-Project.iam.gserviceaccount.com",
		GCPHostProjectID:           "My-Project",
	})
	assert.Equal(t, hostSAClaim{Email: "host@my-project.iam.gserviceaccount.com", ProjectID: "My-Project"}, got)
}

func TestHostSATarget_MatchesPassthroughGateTarget(t *testing.T) {
	t.Run("derived project ID", func(t *testing.T) {
		got := hostSATarget(hostSAClaim{Email: "host@derived-project.iam.gserviceaccount.com"})
		assert.Equal(t, "host@derived-project.iam.gserviceaccount.com", got.Email)
		assert.Equal(t, "derived-project", got.ProjectID)
	})
	t.Run("explicit project ID", func(t *testing.T) {
		got := hostSATarget(hostSAClaim{
			Email:     "123-compute@developer.gserviceaccount.com",
			ProjectID: "explicit-project",
		})
		assert.Equal(t, "123-compute@developer.gserviceaccount.com", got.Email)
		assert.Equal(t, "explicit-project", got.ProjectID)
	})
}

func TestWriteHostSAClaimDenied_NonUserCaller(t *testing.T) {
	rec := httptest.NewRecorder()
	writeHostSAClaimDenied(rec, hostSAClaimVerdict{
		Claim:  hostSAClaim{Email: "host@p.iam.gserviceaccount.com"},
		Result: store.ActAsResult{Outcome: store.ActAsDenied, Mechanism: MechanismHostSAClaimNonUser},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(),
		"Your identity cannot be granted permission to set gcpHostServiceAccountEmail on this broker")
}
