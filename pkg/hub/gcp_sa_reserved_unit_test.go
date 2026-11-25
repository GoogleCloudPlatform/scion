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
	"testing"

	"github.com/stretchr/testify/assert"
)

type stubHubEmailGenerator struct {
	email string
}

func (g *stubHubEmailGenerator) GenerateAccessToken(_ context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	return &GCPAccessToken{AccessToken: "tok", ExpiresIn: 3600, TokenType: "Bearer"}, nil
}

func (g *stubHubEmailGenerator) GenerateIDToken(_ context.Context, _, _ string) (*GCPIDToken, error) {
	return &GCPIDToken{Token: "id-tok"}, nil
}

func (g *stubHubEmailGenerator) VerifyImpersonation(_ context.Context, _ string) error {
	return nil
}

func (g *stubHubEmailGenerator) ServiceAccountEmail() string {
	return g.email
}

func TestIsValidGCPServiceAccountEmail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"standard gcp sa", "worker@my-proj.iam.gserviceaccount.com", true},
		{"short email", "agent@example.com", true},
		{"numeric unique id", "123456789012345678901", false},
		{"empty", "", false},
		{"no at sign", "worker-my-proj", false},
		{"multiple at signs", "a@b@c.com", false},
		{"missing local part", "@my-proj.iam.gserviceaccount.com", false},
		{"missing domain", "worker@", false},
		{"contains slash", "projects/-/serviceAccounts/worker@proj.iam.gserviceaccount.com", false},
		{"contains space", "worker @my-proj.iam.gserviceaccount.com", false},
		{"contains newline", "worker@my-proj.iam.gserviceaccount.com\n", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isValidGCPServiceAccountEmail(tc.in))
		})
	}
}

func TestGCPServiceAccountEmailsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"exact match", "hub@proj.iam.gserviceaccount.com", "hub@proj.iam.gserviceaccount.com", true},
		{"mixed case", "Hub@Proj.IAM.GServiceAccount.com", "hub@proj.iam.gserviceaccount.com", true},
		{"trimmed whitespace", "  hub@proj.iam.gserviceaccount.com \t\n", "hub@proj.iam.gserviceaccount.com", true},
		{"both padded and mixed case", " HUB@PROJ.iam.gserviceaccount.com ", "\thub@proj.iam.gserviceaccount.com ", true},
		{"different email", "other@proj.iam.gserviceaccount.com", "hub@proj.iam.gserviceaccount.com", false},
		{"first empty", "", "hub@proj.iam.gserviceaccount.com", false},
		{"second empty", "hub@proj.iam.gserviceaccount.com", "", false},
		{"both empty", "", "", false},
		{"whitespace only", "   ", "   ", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, gcpServiceAccountEmailsEqual(tc.a, tc.b))
		})
	}
}

func TestServer_IsHubOwnServiceAccountEmail_UnknownIdentity(t *testing.T) {
	ctx := context.Background()
	// When no GCP token generator is configured (e.g. non-GCP deployment),
	// the Hub identity is unknown; the helper returns false without erroring.
	srvNoGen := &Server{}
	assert.False(t, srvNoGen.isHubOwnServiceAccountEmail("hub@proj.iam.gserviceaccount.com"))
	assert.False(t, srvNoGen.isHubOwnServiceAccountEmail(""))
	_, _, refused, err := srvNoGen.gcpServiceAccountEmailRefusal(ctx, "hub@proj.iam.gserviceaccount.com")
	assert.NoError(t, err)
	assert.False(t, refused)

	// When a GCP token generator is configured but its email is empty
	// (e.g. running outside GCE/Cloud Run without a metadata server),
	// the Hub identity is unknown; the helper returns false without erroring.
	srvEmptyEmail := &Server{gcpTokenGenerator: &stubHubEmailGenerator{email: ""}}
	assert.False(t, srvEmptyEmail.isHubOwnServiceAccountEmail("hub@proj.iam.gserviceaccount.com"))
	assert.False(t, srvEmptyEmail.isHubOwnServiceAccountEmail(""))
	_, _, refused, err = srvEmptyEmail.gcpServiceAccountEmailRefusal(ctx, "hub@proj.iam.gserviceaccount.com")
	assert.NoError(t, err)
	assert.False(t, refused)

	// When the Hub identity is known, matching emails (including mixed-case
	// and surrounding whitespace) are refused with ErrCodeIdentityReserved.
	srvKnown := &Server{gcpTokenGenerator: &stubHubEmailGenerator{email: "hub-sa@my-proj.iam.gserviceaccount.com"}}
	assert.True(t, srvKnown.isHubOwnServiceAccountEmail("hub-sa@my-proj.iam.gserviceaccount.com"))
	assert.True(t, srvKnown.isHubOwnServiceAccountEmail("  Hub-SA@My-Proj.IAM.GServiceAccount.COM "))
	assert.False(t, srvKnown.isHubOwnServiceAccountEmail("worker@my-proj.iam.gserviceaccount.com"))

	code, msg, refused, err := srvKnown.gcpServiceAccountEmailRefusal(ctx, "HUB-SA@MY-PROJ.IAM.GSERVICEACCOUNT.COM")
	assert.NoError(t, err)
	assert.True(t, refused)
	assert.Equal(t, ErrCodeIdentityReserved, code)
	assert.NotEmpty(t, msg)
}
